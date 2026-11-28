package jobs

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgListenConn struct {
	dedicated *pgx.Conn
	pooled    *pgxpool.Conn
}

func (c *pgListenConn) conn() *pgx.Conn {
	if c == nil {
		return nil
	}
	if c.dedicated != nil {
		return c.dedicated
	}
	if c.pooled != nil {
		return c.pooled.Conn()
	}
	return nil
}

func (c *pgListenConn) closeAndRelease() {
	if c == nil {
		return
	}
	if c.dedicated != nil {
		closePgConnBounded(c.dedicated)
		c.dedicated = nil
		return
	}
	if c.pooled != nil {
		// Always close the underlying connection before releasing so pgxpool destroys it
		// instead of returning a connection with active LISTEN state to the idle pool.
		closePgConnBounded(c.pooled.Conn())
		c.pooled.Release()
		c.pooled = nil
	}
}

func (s *PostgresStore) openListenConn(ctx context.Context, listenSQL string) (*pgListenConn, error) {
	if s.ownsPool && s.listenConnCfg != nil {
		conn, err := pgx.ConnectConfig(ctx, s.listenConnCfg)
		if err != nil {
			return nil, err
		}
		if _, err := conn.Exec(ctx, listenSQL); err != nil {
			closePgConnBounded(conn)
			return nil, err
		}
		return &pgListenConn{dedicated: conn}, nil
	}

	pConn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := pConn.Exec(ctx, listenSQL); err != nil {
		closePgConnBounded(pConn.Conn())
		pConn.Release()
		return nil, err
	}
	return &pgListenConn{pooled: pConn}, nil
}

func jitterDuration(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	// ±25% jitter (0.75x .. 1.25x) to avoid thundering-herd reconnects on DB restart.
	frac := float64(binary.BigEndian.Uint16(b[:])) / 65535.0
	return time.Duration(float64(base) * (0.75 + 0.5*frac))
}

// SubscribeCancels listens on the store's PostgreSQL NOTIFY channel so remote replicas cancel
// in-flight LLM contexts and unwind concurrency slots immediately upon Cancel or Undo.
// When the store owns its pool, LISTEN uses a dedicated pgx.Conn so pool capacity is never pinned.
func (s *PostgresStore) SubscribeCancels(ctx context.Context, onCancel func(jobID string, fencedToken uint64)) (func(), error) {
	if onCancel == nil {
		return nil, errors.New("onCancel callback must not be nil")
	}
	if s.closeCtx.Err() != nil {
		return nil, wrapPostgresErr("SubscribeCancels", errors.New("store closed"))
	}

	// Tie setup to both caller ctx and store closeCtx so Close() never waits on a hung dial.
	setupCtx, setupCancel := context.WithCancel(ctx)
	stopWatch := context.AfterFunc(s.closeCtx, setupCancel)
	listenSQL := "LISTEN " + pgx.Identifier{s.cancelChannel}.Sanitize()
	lc, err := s.openListenConn(setupCtx, listenSQL)
	stopWatch()
	setupCancel()
	if err != nil {
		return nil, wrapPostgresErr("SubscribeCancels", err)
	}

	subCtx, subCancel := context.WithCancel(s.closeCtx)
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		subCancel()
		lc.closeAndRelease()
		return nil, wrapPostgresErr("SubscribeCancels", errors.New("store closed"))
	}
	s.subWG.Add(1)
	s.closeMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.subWG.Done()
		s.runCancelListenLoop(subCtx, lc, listenSQL, onCancel)
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			subCancel()
			wg.Wait()
		})
	}
	return stop, nil
}

func (s *PostgresStore) runCancelListenLoop(
	subCtx context.Context,
	initial *pgListenConn,
	listenSQL string,
	onCancel func(jobID string, opToken uint64),
) {
	cur := initial
	defer func() {
		if cur != nil {
			cur.closeAndRelease()
		}
	}()

	backoff := 200 * time.Millisecond
	for {
		if cur == nil {
			var ok bool
			cur, backoff, ok = s.reconnectListenConn(subCtx, listenSQL, backoff)
			if !ok {
				return
			}
			if cur == nil {
				continue
			}
		}

		keepConn, done := waitListenNotification(subCtx, cur, onCancel)
		if done {
			return
		}
		if !keepConn {
			cur.closeAndRelease()
			cur = nil
		}
	}
}

func (s *PostgresStore) reconnectListenConn(subCtx context.Context, listenSQL string, backoff time.Duration) (*pgListenConn, time.Duration, bool) {
	const maxBackoff = 2 * time.Second
	timer := time.NewTimer(jitterDuration(backoff))
	select {
	case <-subCtx.Done():
		timer.Stop()
		return nil, backoff, false
	case <-timer.C:
	}

	nextBackoff := min(backoff*2, maxBackoff)
	acqCtx, acqCancel := context.WithTimeout(subCtx, postgresDefaultConnectTimeout)
	nextLC, acqErr := s.openListenConn(acqCtx, listenSQL)
	acqCancel()
	if acqErr != nil {
		if subCtx.Err() != nil {
			return nil, nextBackoff, false
		}
		return nil, nextBackoff, true
	}
	return nextLC, 200 * time.Millisecond, true
}

func waitListenNotification(
	subCtx context.Context,
	cur *pgListenConn,
	onCancel func(jobID string, opToken uint64),
) (keepConn bool, done bool) {
	waitCtx, waitCancel := context.WithTimeout(subCtx, postgresListenKeepalive)
	notif, waitErr := cur.conn().WaitForNotification(waitCtx)
	waitCancel()
	if waitErr != nil {
		if subCtx.Err() != nil {
			return false, true
		}
		if errors.Is(waitErr, context.DeadlineExceeded) {
			// Periodic keepalive ping detects half-open TCP drops behind cloud NAT/LBs.
			pingCtx, pingCancel := context.WithTimeout(subCtx, postgresListenPingTimeout)
			pingErr := cur.conn().Ping(pingCtx)
			pingCancel()
			if pingErr == nil {
				return true, false
			}
		}
		return false, false
	}
	if notif != nil {
		if jobID, token, ok := parseCancelNotification(notif.Payload); ok {
			onCancel(jobID, token)
		}
	}
	return true, false
}
