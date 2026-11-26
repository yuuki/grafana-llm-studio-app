package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var (
	_ JobStore           = (*PostgresStore)(nil)
	_ CancelSubscription = (*PostgresStore)(nil)
)

const (
	DefaultPostgresSchema      = "public"
	DefaultPostgresTablePrefix = "llm_studio_"
	DefaultPostgresSSLMode     = PostgresSSLModePrefer

	PostgresSSLModeDisable    = "disable"
	PostgresSSLModePrefer     = "prefer"
	PostgresSSLModeRequire    = "require"
	PostgresSSLModeVerifyCA   = "verify-ca"
	PostgresSSLModeVerifyFull = "verify-full"

	postgresDefaultConnectTimeout = 5 * time.Second
	postgresDefaultMaxConns       = int32(10)
	postgresCleanupBatchSize      = 500
	postgresRollbackTimeout       = time.Second
	postgresCloseConnTimeout      = time.Second
	postgresListenKeepalive       = 30 * time.Second
	postgresListenPingTimeout     = 5 * time.Second
)

var (
	// Restrict schema & tablePrefix to lowercase PostgreSQL identifiers matching unquoted
	// folded identifier conventions so quoted pgx.Identifier references never create duplicate
	// case-sensitive schemas (e.g. "PUBLIC" vs public).
	validPostgresIdentRe       = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	validPostgresTablePrefixRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,31}$`)
	allowedPostgresSSLModes    = map[string]bool{
		PostgresSSLModeDisable:    true,
		PostgresSSLModePrefer:     true,
		PostgresSSLModeRequire:    true,
		PostgresSSLModeVerifyCA:   true,
		PostgresSSLModeVerifyFull: true,
	}
)

// PostgresConfig configures the optional persistent PostgreSQL JobStore for HA Grafana deployments.
// Operators already running Grafana against PostgreSQL can share the same PostgreSQL cluster
// (using a dedicated database, schema, or table prefix) without deploying a separate Redis cluster.
type PostgresConfig struct {
	Addr           string
	Database       string
	Username       string
	Password       string
	Schema         string
	TablePrefix    string
	SSLMode        string
	ConnectTimeout time.Duration
	MaxConns       int32
	// Pool optionally supplies a pre-configured *pgxpool.Pool (e.g. for custom TLS or testing).
	// When set, Addr/Database/Username/Password/SSLMode/ConnectTimeout/MaxConns are ignored and
	// Close() does not close the externally owned pool.
	Pool *pgxpool.Pool
}

// ValidatePostgresAddr validates that addr is a bare host:port (or [ipv6]:port) with port in 1..65535,
// disallowing URL schemes, embedded credentials, whitespace, control characters, bracketed non-IPv6 hosts,
// and malformed hosts.
func ValidatePostgresAddr(addr string) error {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return errors.New("postgresAddr is required when jobStoreBackend=postgres")
	}
	if strings.Contains(trimmed, "://") || strings.Contains(trimmed, "@") {
		return errors.New("postgresAddr must be host:port without URL scheme or embedded credentials")
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c <= 0x20 || c == 0x7f || c == '/' || c == '?' || c == '#' {
			return fmt.Errorf("invalid postgresAddr %q: contains forbidden character", trimmed)
		}
	}
	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		return fmt.Errorf("invalid postgresAddr %q (expected host:port): %w", trimmed, err)
	}
	if host == "" {
		return fmt.Errorf("invalid postgresAddr %q: host is empty", trimmed)
	}
	if strings.HasPrefix(trimmed, "[") {
		ip := net.ParseIP(host)
		if ip == nil || !strings.Contains(host, ":") {
			return fmt.Errorf("invalid postgresAddr %q: bracketed host must be a valid IPv6 literal", trimmed)
		}
	} else if net.ParseIP(host) == nil {
		if !validRedisHostnameRe.MatchString(host) || strings.Contains(host, "..") {
			return fmt.Errorf("invalid postgresAddr %q: invalid hostname %q", trimmed, host)
		}
	}
	if !validRedisPortRe.MatchString(portStr) {
		return fmt.Errorf("invalid postgresAddr %q: port must be a decimal integer between 1 and 65535", trimmed)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid postgresAddr %q: port must be between 1 and 65535", trimmed)
	}
	return nil
}

func containsForbiddenTextRune(s string, disallowSpace bool) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\ufeff' {
			return true
		}
		if disallowSpace && (r == ' ' || r == '\t' || r == '\n' || r == '\r') {
			return true
		}
	}
	return false
}

// ValidatePostgresDatabase validates the PostgreSQL database name (required, 1..63 UTF-8 bytes,
// no whitespace, control characters, or BOM).
func ValidatePostgresDatabase(db string) error {
	trimmed := strings.TrimSpace(db)
	if trimmed == "" {
		return errors.New("postgresDatabase is required when jobStoreBackend=postgres")
	}
	if len(trimmed) > 63 {
		return errors.New("postgresDatabase exceeds maximum length of 63 UTF-8 bytes")
	}
	if containsForbiddenTextRune(trimmed, true) {
		return fmt.Errorf("invalid postgresDatabase %q: contains whitespace, control character, or BOM", trimmed)
	}
	return nil
}

// ValidatePostgresUsername validates the PostgreSQL username (required, 1..128 UTF-8 bytes,
// no control characters or BOM).
func ValidatePostgresUsername(user string) error {
	trimmed := strings.TrimSpace(user)
	if trimmed == "" {
		return errors.New("postgresUsername is required when jobStoreBackend=postgres")
	}
	if len(trimmed) > 128 {
		return errors.New("postgresUsername exceeds maximum length of 128 UTF-8 bytes")
	}
	if containsForbiddenTextRune(trimmed, false) {
		return errors.New("invalid postgresUsername: contains control character or BOM")
	}
	return nil
}

// ValidatePostgresSchema validates the optional PostgreSQL schema identifier, disallowing
// reserved PostgreSQL system schemas (pg_*, information_schema) and mixed-case identifiers.
func ValidatePostgresSchema(schema string) error {
	trimmed := strings.TrimSpace(schema)
	if trimmed == "" {
		return nil
	}
	if !validPostgresIdentRe.MatchString(trimmed) {
		return fmt.Errorf("invalid postgresSchema %q (allowed: 1..63 lowercase chars matching [a-z_][a-z0-9_]*)", trimmed)
	}
	if strings.HasPrefix(trimmed, "pg_") || trimmed == "information_schema" {
		return fmt.Errorf("invalid postgresSchema %q: reserved PostgreSQL system schema is not allowed", trimmed)
	}
	return nil
}

// ValidatePostgresTablePrefix validates the optional PostgreSQL table name prefix.
func ValidatePostgresTablePrefix(prefix string) error {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return nil
	}
	if !validPostgresTablePrefixRe.MatchString(trimmed) {
		return fmt.Errorf("invalid postgresTablePrefix %q (allowed: 1..32 lowercase chars matching [a-z_][a-z0-9_]*)", trimmed)
	}
	return nil
}

// ValidatePostgresSSLMode validates the optional PostgreSQL sslmode setting.
func ValidatePostgresSSLMode(mode string) error {
	trimmed := strings.TrimSpace(strings.ToLower(mode))
	if trimmed == "" {
		return nil
	}
	if !allowedPostgresSSLModes[trimmed] {
		return fmt.Errorf("unsupported postgresSslMode %q (allowed: disable, prefer, require, verify-ca, verify-full)", trimmed)
	}
	return nil
}

// PostgresStore is a transactional, org-isolated, owner-isolated, TTL-backed JobStore for HA Grafana.
// Per-org advisory transaction locks serialize concurrency slots and bounded job eviction cleanly
// across replicas without cross-org contention, single-row FOR UPDATE transactions enforce fencing
// tokens and optimistic revisions, and PostgreSQL LISTEN/NOTIFY broadcasts Cancel/Undo immediately.
type PostgresStore struct {
	pool          *pgxpool.Pool
	ownsPool      bool
	listenConnCfg *pgx.ConnConfig
	schema        string
	tablePrefix   string
	jobsTable     string
	slotsTable    string
	cancelChannel string

	schemaReady atomic.Bool
	schemaSem   chan struct{}

	closeMu     sync.Mutex
	closed      bool
	closeCtx    context.Context
	closeCancel context.CancelFunc
	subWG       sync.WaitGroup
	closeOnce   sync.Once
}

// NewPostgresStore validates cfg and creates a PostgreSQL-backed JobStore.
// Connections are established lazily on first operation or Ping, and the schema/tables
// are created idempotently on first access so transient DB outages at startup recover cleanly.
func NewPostgresStore(cfg PostgresConfig) (*PostgresStore, error) {
	if err := ValidatePostgresSchema(cfg.Schema); err != nil {
		return nil, err
	}
	if err := ValidatePostgresTablePrefix(cfg.TablePrefix); err != nil {
		return nil, err
	}

	schema := strings.TrimSpace(cfg.Schema)
	if schema == "" {
		schema = DefaultPostgresSchema
	}
	tablePrefix := strings.TrimSpace(cfg.TablePrefix)
	if tablePrefix == "" {
		tablePrefix = DefaultPostgresTablePrefix
	}

	jobsTable := pgx.Identifier{schema, tablePrefix + "jobs"}.Sanitize()
	slotsTable := pgx.Identifier{schema, tablePrefix + "slots"}.Sanitize()
	cancelChannel := postgresCancelChannelName(schema, tablePrefix)
	closeCtx, closeCancel := context.WithCancel(context.Background())

	if cfg.Pool != nil {
		return &PostgresStore{
			pool:          cfg.Pool,
			ownsPool:      false,
			schema:        schema,
			tablePrefix:   tablePrefix,
			jobsTable:     jobsTable,
			slotsTable:    slotsTable,
			cancelChannel: cancelChannel,
			schemaSem:     make(chan struct{}, 1),
			closeCtx:      closeCtx,
			closeCancel:   closeCancel,
		}, nil
	}

	poolCfg, err := buildPostgresPoolConfig(cfg)
	if err != nil {
		closeCancel()
		return nil, err
	}
	listenCfg := poolCfg.ConnConfig.Copy()
	listenCfg.RuntimeParams = map[string]string{
		"application_name": "yuuki-llmstudio-app-listen",
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		closeCancel()
		return nil, wrapPostgresErr("NewPostgresStore", err)
	}

	return &PostgresStore{
		pool:          pool,
		ownsPool:      true,
		listenConnCfg: listenCfg,
		schema:        schema,
		tablePrefix:   tablePrefix,
		jobsTable:     jobsTable,
		slotsTable:    slotsTable,
		cancelChannel: cancelChannel,
		schemaSem:     make(chan struct{}, 1),
		closeCtx:      closeCtx,
		closeCancel:   closeCancel,
	}, nil
}

// buildPostgresPoolConfig builds a *pgxpool.Config programmatically from validated fields so
// ambient PG* environment variables and DSN string interpolation can never inject parameters.
func buildPostgresPoolConfig(cfg PostgresConfig) (*pgxpool.Config, error) {
	if err := ValidatePostgresAddr(cfg.Addr); err != nil {
		return nil, err
	}
	if err := ValidatePostgresDatabase(cfg.Database); err != nil {
		return nil, err
	}
	if err := ValidatePostgresUsername(cfg.Username); err != nil {
		return nil, err
	}
	if err := ValidatePostgresSSLMode(cfg.SSLMode); err != nil {
		return nil, err
	}

	host, portStr, err := net.SplitHostPort(strings.TrimSpace(cfg.Addr))
	if err != nil {
		return nil, fmt.Errorf("invalid postgresAddr: %w", err)
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil || portNum < 1 || portNum > 65535 {
		return nil, fmt.Errorf("invalid postgresAddr port: %q", portStr)
	}
	sslMode := strings.TrimSpace(strings.ToLower(cfg.SSLMode))
	if sslMode == "" {
		sslMode = DefaultPostgresSSLMode
	}

	// Select sslmode from a closed switch of constant literals so no caller-controlled string
	// is ever interpolated into the DSN passed to pgxpool.ParseConfig.
	var sslModeParam string
	switch sslMode {
	case PostgresSSLModeDisable:
		sslModeParam = "disable"
	case PostgresSSLModePrefer:
		sslModeParam = "prefer"
	case PostgresSSLModeRequire:
		sslModeParam = "require"
	case PostgresSSLModeVerifyCA:
		sslModeParam = "verify-ca"
	case PostgresSSLModeVerifyFull:
		sslModeParam = "verify-full"
	default:
		return nil, fmt.Errorf("unsupported postgresSslMode %q", sslMode)
	}

	// Pass explicit neutral values for every env-backed key parsed by pgconn.parseEnvSettings()
	// so ambient PGHOST/PGPORT/PGSSLMODE/PGSSLROOTCERT/PGSSLCERTMODE/PGTARGETSESSIONATTRS/etc.
	// can never fail startup or hijack plugin connections.
	neutralBaseDSN := "host=127.0.0.1 port=5432 user=x password=x dbname=x " +
		"sslmode=" + sslModeParam + " sslcert='' sslkey='' sslrootcert='' sslcertmode=disable " +
		"sslpassword='' sslsni=1 sslnegotiation=postgres connect_timeout=5 " +
		"target_session_attrs=any passfile=/dev/null"
	poolCfg, err := pgxpool.ParseConfig(neutralBaseDSN)
	if err != nil {
		return nil, err
	}

	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = postgresDefaultConnectTimeout
	}
	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = postgresDefaultMaxConns
	}
	if maxConns < 2 {
		maxConns = 2
	}

	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = 0
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	cc := poolCfg.ConnConfig
	cc.Host = host
	cc.Port = uint16(portNum)
	cc.Database = strings.TrimSpace(cfg.Database)
	cc.User = strings.TrimSpace(cfg.Username)
	cc.Password = cfg.Password
	cc.ConnectTimeout = connectTimeout
	cc.DialFunc = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 5 * time.Minute}).DialContext
	cc.ValidateConnect = nil
	cc.SSLNegotiation = ""
	cc.KerberosSrvName = ""
	cc.KerberosSpn = ""
	cc.RuntimeParams = map[string]string{
		"application_name": "yuuki-llmstudio-app",
		"lock_timeout":     "5000",
	}

	// Send TLS SNI when connecting by DNS hostname (required by managed Postgres / SNI proxies
	// such as Neon, RDS Proxy, Azure Flexible Server, and HAProxy).
	sniHost := ""
	if net.ParseIP(host) == nil {
		sniHost = host
	}

	if cc.TLSConfig != nil {
		cc.TLSConfig.MinVersion = tls.VersionTLS12
		if sslMode == PostgresSSLModeVerifyFull {
			cc.TLSConfig.ServerName = host
		} else {
			cc.TLSConfig.ServerName = sniHost
		}
		if sslMode == PostgresSSLModeVerifyCA {
			// Upgrade pgconn's VerifyPeerCertificate callback to VerifyConnection so resumed TLS
			// sessions also re-verify the server certificate chain against the system root CAs.
			cc.TLSConfig.VerifyPeerCertificate = nil
			cc.TLSConfig.VerifyConnection = func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("server presented no TLS certificates")
				}
				opts := x509.VerifyOptions{
					Intermediates: x509.NewCertPool(),
				}
				for _, cert := range cs.PeerCertificates[1:] {
					opts.Intermediates.AddCert(cert)
				}
				_, verifyErr := cs.PeerCertificates[0].Verify(opts)
				return verifyErr
			}
		}
	}
	for _, fb := range cc.Fallbacks {
		fb.Host = host
		fb.Port = uint16(portNum)
		if fb.TLSConfig != nil {
			fb.TLSConfig.MinVersion = tls.VersionTLS12
		}
	}

	return poolCfg, nil
}

func postgresCancelChannelName(schema, tablePrefix string) string {
	sum := sha256.Sum256([]byte("cancel\x00" + schema + "\x00" + tablePrefix))
	return "llmstudio_c_" + hex.EncodeToString(sum[:8])
}

func (s *PostgresStore) advisoryLockKey(scope string) int64 {
	sum := sha256.Sum256([]byte("lock\x00" + s.schema + "\x00" + s.tablePrefix + "\x00" + scope))
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // G115: intentional SHA-256 bit-pattern reinterpretation as signed 64-bit PostgreSQL advisory lock key
}

func (s *PostgresStore) orgLockKey(orgID int64) int64 {
	return s.advisoryLockKey("org:" + strconv.FormatInt(orgID, 10))
}

func (s *PostgresStore) schemaLockKey() int64 {
	return s.advisoryLockKey("schema")
}

// rollbackTx rolls back tx using a short detached context so an already-cancelled caller ctx
// still sends ROLLBACK cleanly instead of forcing pgx to tear down the TCP connection.
func rollbackTx(ctx context.Context, tx pgx.Tx) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postgresRollbackTimeout)
	defer cancel()
	_ = tx.Rollback(rbCtx)
}

func closePgConnBounded(conn *pgx.Conn) {
	if conn == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), postgresCloseConnTimeout)
	defer cancel()
	_ = conn.Close(closeCtx)
}

// ensureSchema idempotently initializes the plugin tables and indexes inside a serialized
// transaction so concurrent multi-replica startups never race on CREATE TABLE IF NOT EXISTS.
// If both tables already exist (e.g. pre-created by a DBA for a least-privilege DML-only role),
// ensureSchema marks itself ready without executing any DDL statements.
func (s *PostgresStore) ensureSchema(ctx context.Context) error {
	if s.schemaReady.Load() {
		return nil
	}

	select {
	case s.schemaSem <- struct{}{}:
		defer func() { <-s.schemaSem }()
	case <-ctx.Done():
		return wrapPostgresErr("ensureSchema", ctx.Err())
	}

	if s.schemaReady.Load() {
		return nil
	}

	// Fast existence check: allows least-privilege DML-only DB roles to operate without
	// CREATE privilege on the schema or table ownership once tables are pre-created.
	var bothExist bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL`,
		s.jobsTable, s.slotsTable).Scan(&bothExist); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	if bothExist {
		s.schemaReady.Store(true)
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.schemaLockKey()); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}

	// Re-check inside lock in case another replica just created the tables.
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL`,
		s.jobsTable, s.slotsTable).Scan(&bothExist); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	if bothExist {
		s.schemaReady.Store(true)
		return nil
	}

	if s.schema != DefaultPostgresSchema {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, s.schema).Scan(&exists); err != nil {
			return wrapPostgresErr("ensureSchema", err)
		}
		if !exists {
			if _, err := tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{s.schema}.Sanitize()); err != nil {
				return wrapPostgresErr("ensureSchema", err)
			}
		}
	}

	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			job_id TEXT PRIMARY KEY,
			org_id BIGINT NOT NULL,
			user_login TEXT NOT NULL,
			owner_key TEXT NOT NULL,
			datasource_uid TEXT NOT NULL,
			query_kind TEXT NOT NULL DEFAULT '',
			scope_json TEXT NOT NULL DEFAULT '',
			import_source_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			current_ir_json TEXT NOT NULL DEFAULT '',
			current_rev INTEGER NOT NULL DEFAULT 0,
			history_json TEXT NOT NULL DEFAULT '[]',
			history_len INTEGER NOT NULL DEFAULT 0,
			last_diff_json TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			warnings_json TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			repair_attempts_used INTEGER NOT NULL DEFAULT 0,
			max_repair_attempts INTEGER NOT NULL DEFAULT 0,
			llm_meta_json TEXT NOT NULL DEFAULT '',
			known_gauges_json TEXT NOT NULL DEFAULT '[]',
			created_at_ms BIGINT NOT NULL,
			updated_at_ms BIGINT NOT NULL,
			expires_at_ms BIGINT NOT NULL,
			active_op_token BIGINT NOT NULL DEFAULT 0,
			active_op_until_ms BIGINT NOT NULL DEFAULT 0
		)`, s.jobsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (org_id, updated_at_ms, job_id)`,
			pgx.Identifier{s.tablePrefix + "jobs_org_updated_idx"}.Sanitize(), s.jobsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (expires_at_ms)`,
			pgx.Identifier{s.tablePrefix + "jobs_expires_idx"}.Sanitize(), s.jobsTable),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			org_id BIGINT NOT NULL,
			slot_id TEXT NOT NULL,
			owner_key TEXT NOT NULL,
			expires_at_ms BIGINT NOT NULL,
			PRIMARY KEY (org_id, slot_id)
		)`, s.slotsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (org_id, expires_at_ms)`,
			pgx.Identifier{s.tablePrefix + "slots_org_exp_idx"}.Sanitize(), s.slotsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (expires_at_ms)`,
			pgx.Identifier{s.tablePrefix + "slots_exp_idx"}.Sanitize(), s.slotsTable),
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return wrapPostgresErr("ensureSchema", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	s.schemaReady.Store(true)
	return nil
}

type pgQueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func resolvePostgresNowMs(ctx context.Context, q pgQueryRower, now time.Time) (int64, error) {
	if !now.IsZero() {
		return normalizeTimestamp(now).UnixMilli(), nil
	}
	var ms int64
	if err := q.QueryRow(ctx, `SELECT FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT`).Scan(&ms); err != nil {
		return 0, err
	}
	return ms, nil
}

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

		cur := lc
		defer func() {
			if cur != nil {
				cur.closeAndRelease()
			}
		}()

		backoff := 200 * time.Millisecond
		const maxBackoff = 2 * time.Second
		for {
			if cur == nil {
				timer := time.NewTimer(jitterDuration(backoff))
				select {
				case <-subCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				acqCtx, acqCancel := context.WithTimeout(subCtx, postgresDefaultConnectTimeout)
				nextLC, acqErr := s.openListenConn(acqCtx, listenSQL)
				acqCancel()
				if acqErr != nil {
					if subCtx.Err() != nil {
						return
					}
					continue
				}
				cur = nextLC
				backoff = 200 * time.Millisecond
			}

			waitCtx, waitCancel := context.WithTimeout(subCtx, postgresListenKeepalive)
			notif, waitErr := cur.conn().WaitForNotification(waitCtx)
			waitCancel()
			if waitErr != nil {
				if subCtx.Err() != nil {
					return
				}
				if errors.Is(waitErr, context.DeadlineExceeded) {
					// Periodic keepalive ping detects half-open TCP drops behind cloud NAT/LBs.
					pingCtx, pingCancel := context.WithTimeout(subCtx, postgresListenPingTimeout)
					pingErr := cur.conn().Ping(pingCtx)
					pingCancel()
					if pingErr == nil {
						continue
					}
				}
				cur.closeAndRelease()
				cur = nil
				continue
			}
			if notif == nil {
				continue
			}
			sep := strings.LastIndexByte(notif.Payload, ':')
			if sep <= 0 || sep >= len(notif.Payload)-1 {
				continue
			}
			jobID := notif.Payload[:sep]
			token, parseErr := strconv.ParseUint(notif.Payload[sep+1:], 10, 64)
			if parseErr != nil || token == 0 || ValidateJobID(jobID) != nil {
				continue
			}
			onCancel(jobID, token)
		}
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

func (s *PostgresStore) CreateGenerating(ctx context.Context, params CreateGeneratingParams) (OpLease, error) {
	if err := validateOwner(params.Owner); err != nil {
		return OpLease{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return OpLease{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return OpLease{}, err
	}

	limits := params.Limits.normalized()
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	scopeJSON, err := marshalOptionalJSON(params.Scope)
	if err != nil {
		return OpLease{}, fmt.Errorf("marshal scope: %w", err)
	}
	gauges := make(map[string]bool)
	mergeGaugeMapBounded(gauges, params.KnownGauges)
	gaugesJSON, err := marshalGaugeMap(gauges)
	if err != nil {
		return OpLease{}, fmt.Errorf("marshal knownGauges: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.orgLockKey(params.Owner.OrgID)); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	if err := s.evictExpiredOrgTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	if err := s.reapExpiredOrgSlotsTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	var exists bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE job_id = $1)`, s.jobsTable), params.JobID).Scan(&exists); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	if exists {
		return OpLease{}, fmt.Errorf("job id %q already exists", params.JobID)
	}

	oKey := ownerKey(params.Owner)
	canAcquire, err := s.canAcquireSlotTx(ctx, tx, params.Owner.OrgID, oKey, nowMs, limits)
	if err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	if !canAcquire {
		return OpLease{}, ErrConcurrencyLimited
	}

	if err := s.ensureCapacityTx(ctx, tx, params.Owner, oKey, nowMs, limits); err != nil {
		return OpLease{}, err
	}

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)
	expiresAtMs := nowMs + jobTTLMs
	leaseUntilMs := nowMs + leaseTTLMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (
		job_id, org_id, user_login, owner_key, datasource_uid, query_kind,
		scope_json, status, max_repair_attempts, known_gauges_json,
		created_at_ms, updated_at_ms, expires_at_ms, active_op_token, active_op_until_ms
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,1,$13)`, s.jobsTable),
		params.JobID,
		params.Owner.OrgID,
		params.Owner.UserLogin,
		oKey,
		params.DatasourceUID,
		string(params.QueryKind),
		scopeJSON,
		string(StatusGenerating),
		params.MaxRepairAttempts,
		gaugesJSON,
		nowMs,
		expiresAtMs,
		leaseUntilMs,
	)
	if err != nil {
		if isPgUniqueViolation(err) {
			return OpLease{}, fmt.Errorf("job id %q already exists", params.JobID)
		}
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	if err := s.upsertSlotTx(ctx, tx, params.Owner.OrgID, slotID, oKey, leaseUntilMs); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	return OpLease{OpToken: 1, SlotID: slotID}, nil
}

func (s *PostgresStore) CreateImported(ctx context.Context, params CreateImportedParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	limits := params.Limits.normalized()
	scopeJSON, err := marshalOptionalJSON(params.Scope)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal scope: %w", err)
	}
	importSrcJSON, err := marshalOptionalJSON(params.ImportSource)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal importSource: %w", err)
	}
	docBytes, err := json.Marshal(params.Draft)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal draft: %w", err)
	}
	warnsJSON, err := marshalOptionalStringSlice(params.Warnings)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal warnings: %w", err)
	}
	gauges := make(map[string]bool)
	mergeGaugeMapBounded(gauges, params.KnownGauges)
	gaugesJSON, err := marshalGaugeMap(gauges)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal knownGauges: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.orgLockKey(params.Owner.OrgID)); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	if err := s.evictExpiredOrgTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	var exists bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE job_id = $1)`, s.jobsTable), params.JobID).Scan(&exists); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}
	if exists {
		return JobSnapshot{}, fmt.Errorf("job id %q already exists", params.JobID)
	}

	oKey := ownerKey(params.Owner)
	if err := s.ensureImportCapacityTx(ctx, tx, params.Owner, oKey, nowMs, limits); err != nil {
		return JobSnapshot{}, err
	}

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	expiresAtMs := nowMs + jobTTLMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (
		job_id, org_id, user_login, owner_key, datasource_uid, query_kind,
		scope_json, import_source_json, status, current_ir_json, current_rev,
		summary, warnings_json, max_repair_attempts, known_gauges_json,
		created_at_ms, updated_at_ms, expires_at_ms, active_op_token, active_op_until_ms
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16,$17,0,0)`, s.jobsTable),
		params.JobID,
		params.Owner.OrgID,
		params.Owner.UserLogin,
		oKey,
		params.DatasourceUID,
		string(params.QueryKind),
		scopeJSON,
		importSrcJSON,
		string(StatusAwaitingPreview),
		string(docBytes),
		params.Draft.Revision,
		params.Summary,
		warnsJSON,
		params.MaxRepairAttempts,
		gaugesJSON,
		nowMs,
		expiresAtMs,
	)
	if err != nil {
		if isPgUniqueViolation(err) {
			return JobSnapshot{}, fmt.Errorf("job id %q already exists", params.JobID)
		}
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	docCopy := params.Draft.Clone()
	var warns []string
	if len(params.Warnings) > 0 {
		warns = make([]string, len(params.Warnings))
		copy(warns, params.Warnings)
	}
	qKind := params.QueryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:             params.JobID,
		Status:            StatusAwaitingPreview,
		DatasourceUID:     params.DatasourceUID,
		QueryKind:         string(qKind),
		Scope:             cloneScope(params.Scope),
		ImportSource:      cloneImportSource(params.ImportSource),
		CurrentIR:         &docCopy,
		Summary:           params.Summary,
		Warnings:          warns,
		MaxRepairAttempts: params.MaxRepairAttempts,
		CanUndo:           false,
		UpdatedAt:         time.UnixMilli(nowMs).UTC(),
	}, nil
}

type pgJobRow struct {
	id                 string
	orgID              int64
	userLogin          string
	datasourceUID      string
	queryKind          string
	scopeJSON          string
	importSourceJSON   string
	status             string
	currentIRJSON      string
	currentRev         int
	historyJSON        string
	historyLen         int
	lastDiffJSON       string
	summary            string
	warningsJSON       string
	errorMessage       string
	repairAttemptsUsed int
	maxRepairAttempts  int
	llmMetaJSON        string
	knownGaugesJSON    string
	updatedAtMs        int64
	expiresAtMs        int64
	activeOpToken      uint64
	activeOpUntilMs    int64
}

func (r *pgJobRow) isActive(nowMs int64) bool {
	return r.activeOpUntilMs > 0 && nowMs < r.activeOpUntilMs
}

func (r *pgJobRow) isExpired(nowMs int64) bool {
	if r.isActive(nowMs) {
		return false
	}
	return r.expiresAtMs > 0 && nowMs > r.expiresAtMs
}

func nextOpToken(cur uint64) (uint64, error) {
	if cur >= math.MaxInt64 {
		return 0, errors.New("operation token sequence exhausted")
	}
	return cur + 1, nil
}

func opTokenToInt64(token uint64) int64 {
	if token > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(token)
}

func (r *pgJobRow) toSnapshot(opName string) (JobSnapshot, error) {
	var scope *ir.ScopeFilter
	if r.scopeJSON != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(r.scopeJSON), &sf); err != nil {
			return JobSnapshot{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal scope: %w", err))
		}
		scope = &sf
	}
	var importSrc *ir.ImportSource
	if r.importSourceJSON != "" {
		var is ir.ImportSource
		if err := json.Unmarshal([]byte(r.importSourceJSON), &is); err != nil {
			return JobSnapshot{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal importSource: %w", err))
		}
		importSrc = &is
	}
	var currentIR *ir.DashboardIR
	if r.currentIRJSON != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(r.currentIRJSON), &doc); err != nil {
			return JobSnapshot{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal currentIR: %w", err))
		}
		if r.currentRev > 0 {
			doc.Revision = r.currentRev
		}
		currentIR = &doc
	}
	var diffs []ir.DiffEntry
	if r.lastDiffJSON != "" {
		if err := json.Unmarshal([]byte(r.lastDiffJSON), &diffs); err != nil {
			return JobSnapshot{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal lastDiff: %w", err))
		}
	}
	var warns []string
	if r.warningsJSON != "" {
		if err := json.Unmarshal([]byte(r.warningsJSON), &warns); err != nil {
			return JobSnapshot{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal warnings: %w", err))
		}
	}
	var meta *llm.CompletionMeta
	if r.llmMetaJSON != "" {
		var m llm.CompletionMeta
		if err := json.Unmarshal([]byte(r.llmMetaJSON), &m); err != nil {
			return JobSnapshot{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal llmMeta: %w", err))
		}
		meta = &m
	}
	qKind := query.Kind(r.queryKind)
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:              r.id,
		Status:             JobStatus(r.status),
		DatasourceUID:      r.datasourceUID,
		QueryKind:          string(qKind),
		Scope:              scope,
		ImportSource:       importSrc,
		CurrentIR:          currentIR,
		LastDiff:           diffs,
		Summary:            r.summary,
		Warnings:           warns,
		ErrorMessage:       r.errorMessage,
		RepairAttemptsUsed: r.repairAttemptsUsed,
		MaxRepairAttempts:  r.maxRepairAttempts,
		CanUndo:            r.historyLen > 0,
		LLM:                meta,
		UpdatedAt:          time.UnixMilli(r.updatedAtMs).UTC(),
	}, nil
}

func (s *PostgresStore) selectJobRowWithClock(ctx context.Context, q pgQueryRower, jobID string, includeHistory bool, forUpdate bool, explicitNow time.Time) (*pgJobRow, int64, error) {
	histExpr := "''"
	if includeHistory {
		histExpr = "history_json"
	}
	lockClause := ""
	if forUpdate {
		lockClause = " FOR UPDATE"
	}
	clockExpr := "FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT"
	if !explicitNow.IsZero() {
		clockExpr = strconv.FormatInt(normalizeTimestamp(explicitNow).UnixMilli(), 10) + "::BIGINT"
	}
	querySQL := fmt.Sprintf(`SELECT
		job_id, org_id, user_login, datasource_uid, query_kind,
		scope_json, import_source_json, status, current_ir_json, current_rev,
		%s, history_len, last_diff_json, summary, warnings_json,
		error_message, repair_attempts_used, max_repair_attempts, llm_meta_json,
		known_gauges_json, updated_at_ms, expires_at_ms, active_op_token, active_op_until_ms,
		%s
	FROM %s WHERE job_id = $1%s`, histExpr, clockExpr, s.jobsTable, lockClause)

	var row pgJobRow
	var opTokenSigned int64
	var nowMs int64
	err := q.QueryRow(ctx, querySQL, jobID).Scan(
		&row.id, &row.orgID, &row.userLogin, &row.datasourceUID, &row.queryKind,
		&row.scopeJSON, &row.importSourceJSON, &row.status, &row.currentIRJSON, &row.currentRev,
		&row.historyJSON, &row.historyLen, &row.lastDiffJSON, &row.summary, &row.warningsJSON,
		&row.errorMessage, &row.repairAttemptsUsed, &row.maxRepairAttempts, &row.llmMetaJSON,
		&row.knownGaugesJSON, &row.updatedAtMs, &row.expiresAtMs, &opTokenSigned, &row.activeOpUntilMs,
		&nowMs,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, ErrJobNotFound
		}
		return nil, 0, err
	}
	if opTokenSigned > 0 {
		row.activeOpToken = uint64(opTokenSigned)
	}
	return &row, nowMs, nil
}

func (s *PostgresStore) verifyOwnedRowTx(ctx context.Context, tx pgx.Tx, owner Owner, jobID string, nowMs int64, includeHistory bool) (*pgJobRow, error) {
	row, _, err := s.selectJobRowWithClock(ctx, tx, jobID, includeHistory, true, time.UnixMilli(nowMs).UTC())
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if row.isExpired(nowMs) {
		if row.orgID == owner.OrgID && row.userLogin == owner.UserLogin {
			_, _ = tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE job_id = $1 AND org_id = $2 AND user_login = $3`, s.jobsTable),
				jobID, owner.OrgID, owner.UserLogin)
			_ = tx.Commit(ctx)
		}
		return nil, ErrJobNotFound
	}
	if row.orgID != owner.OrgID || row.userLogin != owner.UserLogin {
		return nil, ErrForbidden
	}
	return row, nil
}

func (s *PostgresStore) Get(ctx context.Context, owner Owner, jobID string, now time.Time) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	row, nowMs, err := s.selectJobRowWithClock(ctx, s.pool, jobID, false, false, now)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return JobSnapshot{}, ErrJobNotFound
		}
		return JobSnapshot{}, wrapPostgresErr("Get", err)
	}
	if row.isExpired(nowMs) {
		if row.orgID == owner.OrgID && row.userLogin == owner.UserLogin {
			_, _ = s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE job_id = $1 AND org_id = $2 AND user_login = $3 AND active_op_until_ms <= $4 AND expires_at_ms > 0 AND expires_at_ms < $4`, s.jobsTable),
				jobID, owner.OrgID, owner.UserLogin, nowMs)
		}
		return JobSnapshot{}, ErrJobNotFound
	}
	if row.orgID != owner.OrgID || row.userLogin != owner.UserLogin {
		return JobSnapshot{}, ErrForbidden
	}
	return row.toSnapshot("Get")
}

func (s *PostgresStore) GetRepairCandidate(ctx context.Context, owner Owner, jobID string, now time.Time) (RepairCandidate, error) {
	if err := validateOwner(owner); err != nil {
		return RepairCandidate{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return RepairCandidate{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return RepairCandidate{}, err
	}

	row, nowMs, err := s.selectJobRowWithClock(ctx, s.pool, jobID, false, false, now)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return RepairCandidate{}, ErrJobNotFound
		}
		return RepairCandidate{}, wrapPostgresErr("GetRepairCandidate", err)
	}
	if row.isExpired(nowMs) {
		if row.orgID == owner.OrgID && row.userLogin == owner.UserLogin {
			_, _ = s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE job_id = $1 AND org_id = $2 AND user_login = $3 AND active_op_until_ms <= $4 AND expires_at_ms > 0 AND expires_at_ms < $4`, s.jobsTable),
				jobID, owner.OrgID, owner.UserLogin, nowMs)
		}
		return RepairCandidate{}, ErrJobNotFound
	}
	if row.orgID != owner.OrgID || row.userLogin != owner.UserLogin {
		return RepairCandidate{}, ErrForbidden
	}

	var docPtr *ir.DashboardIR
	if row.currentIRJSON != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(row.currentIRJSON), &doc); err != nil {
			return RepairCandidate{}, wrapPostgresErr("GetRepairCandidate", fmt.Errorf("unmarshal currentIR: %w", err))
		}
		if row.currentRev > 0 {
			doc.Revision = row.currentRev
		}
		docPtr = &doc
	}
	qKind := query.Kind(row.queryKind)
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return RepairCandidate{
		CurrentIR:          docPtr,
		RepairAttemptsUsed: row.repairAttemptsUsed,
		MaxRepairAttempts:  row.maxRepairAttempts,
		HasActiveOp:        row.isActive(nowMs),
		QueryKind:          qKind,
	}, nil
}

func (s *PostgresStore) BeginPatch(ctx context.Context, params BeginPatchParams) (BeginOpResult, error) {
	if err := validateOwner(params.Owner); err != nil {
		return BeginOpResult{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return BeginOpResult{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return BeginOpResult{}, err
	}

	limits := params.Limits.normalized()
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.orgLockKey(params.Owner.OrgID)); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}

	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return BeginOpResult{}, err
		}
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	if row.currentIRJSON == "" {
		return BeginOpResult{}, errors.New("job has no active IR draft to patch")
	}
	var currentIR ir.DashboardIR
	if err := json.Unmarshal([]byte(row.currentIRJSON), &currentIR); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", fmt.Errorf("unmarshal currentIR: %w", err))
	}
	if row.currentRev > 0 {
		currentIR.Revision = row.currentRev
	}
	if params.BaseRevision != currentIR.Revision {
		return BeginOpResult{}, formatRevisionConflict("", params.BaseRevision, currentIR.Revision)
	}
	if params.TargetPanelID != "" && (strings.ContainsAny(params.TargetPanelID, "\r\n") || !panelExists(currentIR.Panels, params.TargetPanelID)) {
		return BeginOpResult{}, fmt.Errorf("%w: %q", ir.ErrPanelNotFound, params.TargetPanelID)
	}
	if row.isActive(nowMs) {
		return BeginOpResult{}, ErrOperationInProgress
	}

	// Parse scope and knownGauges BEFORE committing any lease, slot, or state change.
	var scope *ir.ScopeFilter
	if row.scopeJSON != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(row.scopeJSON), &sf); err != nil {
			return BeginOpResult{}, wrapPostgresErr("BeginPatch", fmt.Errorf("unmarshal scope: %w", err))
		}
		scope = &sf
	}
	knownGauges, err := unmarshalGaugeMap(row.knownGaugesJSON)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", fmt.Errorf("unmarshal knownGauges: %w", err))
	}
	mergeGaugeMapBounded(knownGauges, params.CatalogGauges)
	gaugesJSON, err := marshalGaugeMap(knownGauges)
	if err != nil {
		return BeginOpResult{}, fmt.Errorf("marshal knownGauges: %w", err)
	}

	if err := s.reapExpiredOrgSlotsTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	oKey := ownerKey(params.Owner)
	canAcquire, err := s.canAcquireSlotTx(ctx, tx, params.Owner.OrgID, oKey, nowMs, limits)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	if !canAcquire {
		return BeginOpResult{}, ErrConcurrencyLimited
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)
	leaseUntilMs := nowMs + leaseTTLMs
	expiresAtMs := nowMs + jobTTLMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = $2, status = $3,
		    known_gauges_json = $4, updated_at_ms = $5, expires_at_ms = $6
		WHERE job_id = $7`, s.jobsTable),
		opTokenToInt64(newToken), leaseUntilMs, string(StatusGenerating), gaugesJSON, nowMs, expiresAtMs, params.JobID)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	if err := s.upsertSlotTx(ctx, tx, params.Owner.OrgID, slotID, oKey, leaseUntilMs); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginPatch", err)
	}

	qKind := query.Kind(row.queryKind)
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return BeginOpResult{
		OpToken:     newToken,
		SlotID:      slotID,
		CurrentIR:   currentIR,
		Scope:       scope,
		QueryKind:   qKind,
		KnownGauges: knownGauges,
	}, nil
}

func (s *PostgresStore) BeginRepair(ctx context.Context, params BeginRepairParams) (BeginOpResult, error) {
	if err := validateOwner(params.Owner); err != nil {
		return BeginOpResult{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return BeginOpResult{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return BeginOpResult{}, err
	}

	limits := params.Limits.normalized()
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.orgLockKey(params.Owner.OrgID)); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}

	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return BeginOpResult{}, err
		}
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	if row.currentIRJSON == "" {
		return BeginOpResult{}, errors.New("job has no active IR draft to repair")
	}
	var currentIR ir.DashboardIR
	if err := json.Unmarshal([]byte(row.currentIRJSON), &currentIR); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", fmt.Errorf("unmarshal currentIR: %w", err))
	}
	if row.currentRev > 0 {
		currentIR.Revision = row.currentRev
	}
	if params.BaseRevision != currentIR.Revision {
		return BeginOpResult{}, formatRevisionConflict("repair", params.BaseRevision, currentIR.Revision)
	}
	if row.isActive(nowMs) {
		return BeginOpResult{}, ErrOperationInProgress
	}
	if row.repairAttemptsUsed >= row.maxRepairAttempts {
		return BeginOpResult{}, fmt.Errorf("%w: used %d of %d repair attempts", ErrBudgetExhausted, row.repairAttemptsUsed, row.maxRepairAttempts)
	}

	// Parse scope and knownGauges BEFORE committing any budget decrement, lease, or slot.
	var scope *ir.ScopeFilter
	if row.scopeJSON != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(row.scopeJSON), &sf); err != nil {
			return BeginOpResult{}, wrapPostgresErr("BeginRepair", fmt.Errorf("unmarshal scope: %w", err))
		}
		scope = &sf
	}
	knownGauges, err := unmarshalGaugeMap(row.knownGaugesJSON)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", fmt.Errorf("unmarshal knownGauges: %w", err))
	}
	mergeGaugeMapBounded(knownGauges, params.CatalogGauges)
	gaugesJSON, err := marshalGaugeMap(knownGauges)
	if err != nil {
		return BeginOpResult{}, fmt.Errorf("marshal knownGauges: %w", err)
	}

	if err := s.reapExpiredOrgSlotsTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	oKey := ownerKey(params.Owner)
	canAcquire, err := s.canAcquireSlotTx(ctx, tx, params.Owner.OrgID, oKey, nowMs, limits)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	if !canAcquire {
		return BeginOpResult{}, ErrConcurrencyLimited
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)
	leaseUntilMs := nowMs + leaseTTLMs
	expiresAtMs := nowMs + jobTTLMs

	// Atomically consume repair budget BEFORE calling the LLM.
	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET repair_attempts_used = repair_attempts_used + 1,
		    active_op_token = $1, active_op_until_ms = $2, status = $3,
		    known_gauges_json = $4, updated_at_ms = $5, expires_at_ms = $6
		WHERE job_id = $7`, s.jobsTable),
		opTokenToInt64(newToken), leaseUntilMs, string(StatusRepairing), gaugesJSON, nowMs, expiresAtMs, params.JobID)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	if err := s.upsertSlotTx(ctx, tx, params.Owner.OrgID, slotID, oKey, leaseUntilMs); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BeginOpResult{}, wrapPostgresErr("BeginRepair", err)
	}

	qKind := query.Kind(row.queryKind)
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return BeginOpResult{
		OpToken:     newToken,
		SlotID:      slotID,
		CurrentIR:   currentIR,
		Scope:       scope,
		QueryKind:   qKind,
		KnownGauges: knownGauges,
	}, nil
}

func (s *PostgresStore) SetStatus(ctx context.Context, owner Owner, jobID string, opToken uint64, status JobStatus, now time.Time, jobTTL time.Duration) error {
	if err := validateOwner(owner); err != nil {
		return err
	}
	if err := ValidateJobID(jobID); err != nil {
		return err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wrapPostgresErr("SetStatus", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return wrapPostgresErr("SetStatus", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return err
		}
		return wrapPostgresErr("SetStatus", err)
	}
	if opToken > 0 && row.activeOpToken == opToken && row.isActive(nowMs) {
		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET status = $1, updated_at_ms = $2, expires_at_ms = $3 WHERE job_id = $4`, s.jobsTable),
			string(status), nowMs, nowMs+ttlMs, jobID)
		if err != nil {
			return wrapPostgresErr("SetStatus", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return wrapPostgresErr("SetStatus", err)
		}
	}
	return nil
}

func (s *PostgresStore) FinishJob(ctx context.Context, params FinishJobParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	var docJSON string
	var docRev int
	hasDoc := false
	if params.Doc != nil {
		b, err := json.Marshal(params.Doc)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal doc: %w", err)
		}
		docJSON = string(b)
		docRev = params.Doc.Revision
		hasDoc = true
	}
	diffsJSON, err := marshalOptionalDiffs(params.Diffs)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal diffs: %w", err)
	}
	warnsJSON, err := marshalOptionalStringSlice(params.Warnings)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal warnings: %w", err)
	}
	var metaJSON string
	hasMeta := false
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		metaJSON = string(b)
		hasMeta = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	if params.OpToken == 0 || row.activeOpToken != params.OpToken || !row.isActive(nowMs) {
		snap, snapErr := row.toSnapshot("FinishJob")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ErrJobCancelled
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.status = string(params.Status)
	if hasDoc {
		row.currentIRJSON = docJSON
		row.currentRev = docRev
	}
	row.lastDiffJSON = diffsJSON
	row.summary = params.Summary
	row.warningsJSON = warnsJSON
	row.errorMessage = params.ErrorMessage
	if hasMeta {
		row.llmMetaJSON = metaJSON
	}
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    current_ir_json = $3, current_rev = $4, last_diff_json = $5,
		    summary = $6, warnings_json = $7, error_message = $8,
		    llm_meta_json = $9, updated_at_ms = $10, expires_at_ms = $11
		WHERE job_id = $12`, s.jobsTable),
		opTokenToInt64(newToken), row.status,
		row.currentIRJSON, row.currentRev, row.lastDiffJSON,
		row.summary, row.warningsJSON, row.errorMessage,
		row.llmMetaJSON, row.updatedAtMs, row.expiresAtMs, params.JobID)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	return row.toSnapshot("FinishJob")
}

func (s *PostgresStore) CommitRevision(ctx context.Context, params CommitRevisionParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	maxHist := params.MaxUndoHistory
	if maxHist <= 0 {
		maxHist = defaultMaxUndoHistory
	}

	nextDocBytes, err := json.Marshal(params.NextIR)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal nextIR: %w", err)
	}
	diffsJSON, err := marshalOptionalDiffs(params.Diffs)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal diffs: %w", err)
	}
	warnsJSON, err := marshalOptionalStringSlice(params.Warnings)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal warnings: %w", err)
	}
	var metaJSON string
	hasMeta := false
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		metaJSON = string(b)
		hasMeta = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, true)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	if params.OpToken == 0 || row.activeOpToken != params.OpToken || !row.isActive(nowMs) {
		snap, snapErr := row.toSnapshot("CommitRevision")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ErrJobCancelled
	}
	if row.currentIRJSON == "" || row.currentRev != params.ExpectedRev {
		newToken, tokErr := nextOpToken(row.activeOpToken)
		if tokErr != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", tokErr)
		}
		_, updErr := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET active_op_token = $1, active_op_until_ms = 0 WHERE job_id = $2`, s.jobsTable),
			opTokenToInt64(newToken), params.JobID)
		if updErr != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", updErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", commitErr)
		}
		snap, snapErr := row.toSnapshot("CommitRevision")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ir.ErrRevisionConflict
	}

	var prevIR ir.DashboardIR
	if err := json.Unmarshal([]byte(row.currentIRJSON), &prevIR); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", fmt.Errorf("unmarshal currentIR: %w", err))
	}
	if row.currentRev > 0 {
		prevIR.Revision = row.currentRev
	}

	var history []ir.DashboardIR
	if row.historyJSON != "" && row.historyJSON != "[]" {
		if err := json.Unmarshal([]byte(row.historyJSON), &history); err != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", fmt.Errorf("unmarshal history: %w", err))
		}
	}
	history = append(history, prevIR)
	if len(history) > maxHist {
		history = history[len(history)-maxHist:]
	}
	histBytes, err := json.Marshal(history)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal history: %w", err)
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.currentIRJSON = string(nextDocBytes)
	row.currentRev = params.NextIR.Revision
	row.historyJSON = string(histBytes)
	row.historyLen = len(history)
	row.lastDiffJSON = diffsJSON
	row.summary = params.Summary
	row.warningsJSON = warnsJSON
	row.errorMessage = ""
	row.status = string(StatusAwaitingPreview)
	if hasMeta {
		row.llmMetaJSON = metaJSON
	}
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    current_ir_json = $3, current_rev = $4, history_json = $5, history_len = $6,
		    last_diff_json = $7, summary = $8, warnings_json = $9, error_message = '',
		    llm_meta_json = $10, updated_at_ms = $11, expires_at_ms = $12
		WHERE job_id = $13`, s.jobsTable),
		opTokenToInt64(newToken), row.status,
		row.currentIRJSON, row.currentRev, row.historyJSON, row.historyLen,
		row.lastDiffJSON, row.summary, row.warningsJSON,
		row.llmMetaJSON, row.updatedAtMs, row.expiresAtMs, params.JobID)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	return row.toSnapshot("CommitRevision")
}

func (s *PostgresStore) RecordEditFailure(ctx context.Context, params RecordEditFailureParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	var metaJSON string
	hasMeta := false
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		metaJSON = string(b)
		hasMeta = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	if params.OpToken == 0 || row.activeOpToken != params.OpToken || !row.isActive(nowMs) {
		snap, snapErr := row.toSnapshot("RecordEditFailure")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ErrJobCancelled
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.status = string(StatusFailed)
	row.errorMessage = params.ErrorMessage
	if hasMeta {
		row.llmMetaJSON = metaJSON
	}
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    error_message = $3, llm_meta_json = $4, updated_at_ms = $5, expires_at_ms = $6
		WHERE job_id = $7`, s.jobsTable),
		opTokenToInt64(newToken), row.status, row.errorMessage, row.llmMetaJSON, row.updatedAtMs, row.expiresAtMs, params.JobID)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	return row.toSnapshot("RecordEditFailure")
}

func (s *PostgresStore) RevertOnCancel(ctx context.Context, owner Owner, jobID string, opToken uint64, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
	}
	if opToken > 0 && row.activeOpToken == opToken && row.isActive(nowMs) {
		newToken, tokErr := nextOpToken(row.activeOpToken)
		if tokErr != nil {
			return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", tokErr)
		}
		row.activeOpToken = newToken
		row.activeOpUntilMs = 0
		if row.currentIRJSON != "" {
			row.status = string(StatusAwaitingPreview)
		} else {
			row.status = string(StatusCancelled)
		}
		row.summary = "処理がキャンセルされたためドラフトは変更されていません。"
		row.updatedAtMs = nowMs
		row.expiresAtMs = nowMs + ttlMs

		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
			SET active_op_token = $1, active_op_until_ms = 0, status = $2,
			    summary = $3, updated_at_ms = $4, expires_at_ms = $5
			WHERE job_id = $6`, s.jobsTable),
			opTokenToInt64(newToken), row.status, row.summary, row.updatedAtMs, row.expiresAtMs, jobID)
		if err != nil {
			return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
		}
	}
	snap, snapErr := row.toSnapshot("RevertOnCancel")
	if snapErr != nil {
		return JobSnapshot{}, snapErr
	}
	return snap, ErrJobCancelled
}

func (s *PostgresStore) Undo(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return MutationResult{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, true)
	if err != nil {
		if isDomainJobErr(err) {
			return MutationResult{}, err
		}
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	if row.historyLen == 0 || row.currentIRJSON == "" {
		return MutationResult{}, ErrNoUndoHistory
	}
	if row.currentRev >= ir.MaxRevision {
		return MutationResult{}, fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	}

	var history []ir.DashboardIR
	if row.historyJSON != "" {
		if err := json.Unmarshal([]byte(row.historyJSON), &history); err != nil {
			return MutationResult{}, wrapPostgresErr("Undo", fmt.Errorf("unmarshal history: %w", err))
		}
	}
	if len(history) == 0 {
		return MutationResult{}, ErrNoUndoHistory
	}

	prev := history[len(history)-1]
	history = history[:len(history)-1]
	restored := prev.Clone()
	restored.Revision = row.currentRev + 1

	restoredBytes, err := json.Marshal(restored)
	if err != nil {
		return MutationResult{}, fmt.Errorf("marshal restored IR: %w", err)
	}
	histBytes, err := json.Marshal(history)
	if err != nil {
		return MutationResult{}, fmt.Errorf("marshal history: %w", err)
	}
	diffs := []ir.DiffEntry{{
		Op:     "undo",
		Field:  "revision",
		Before: fmt.Sprintf("rev %d", restored.Revision-1),
		After:  fmt.Sprintf("restored from rev %d as rev %d", prev.Revision, restored.Revision),
	}}
	diffsJSON, err := marshalOptionalDiffs(diffs)
	if err != nil {
		return MutationResult{}, fmt.Errorf("marshal undo diff: %w", err)
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.currentIRJSON = string(restoredBytes)
	row.currentRev = restored.Revision
	row.historyJSON = string(histBytes)
	row.historyLen = len(history)
	row.lastDiffJSON = diffsJSON
	row.summary = "直前のドラフト状態を取り消して復元しました。"
	row.status = string(StatusAwaitingPreview)
	row.errorMessage = ""
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    current_ir_json = $3, current_rev = $4, history_json = $5, history_len = $6,
		    last_diff_json = $7, summary = $8, error_message = '',
		    updated_at_ms = $9, expires_at_ms = $10
		WHERE job_id = $11`, s.jobsTable),
		opTokenToInt64(newToken), row.status,
		row.currentIRJSON, row.currentRev, row.historyJSON, row.historyLen,
		row.lastDiffJSON, row.summary,
		row.updatedAtMs, row.expiresAtMs, jobID)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, s.cancelChannel, jobID+":"+strconv.FormatUint(newToken, 10)); err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	snap, err := row.toSnapshot("Undo")
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Snapshot: snap, OpToken: newToken}, nil
}

func (s *PostgresStore) Cancel(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return MutationResult{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return MutationResult{}, err
		}
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	if row.currentIRJSON == "" {
		row.status = string(StatusCancelled)
	} else {
		row.status = string(StatusAwaitingPreview)
	}
	row.summary = "実行中の処理をキャンセルしました。"
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    summary = $3, updated_at_ms = $4, expires_at_ms = $5
		WHERE job_id = $6`, s.jobsTable),
		opTokenToInt64(newToken), row.status, row.summary, row.updatedAtMs, row.expiresAtMs, jobID)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, s.cancelChannel, jobID+":"+strconv.FormatUint(newToken, 10)); err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	snap, err := row.toSnapshot("Cancel")
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Snapshot: snap, OpToken: newToken}, nil
}

// ReleaseSlot deletes only the caller's exact slot row (single-row PK lookup) so concurrent
// slot releases can never deadlock (40P01) against org reaping or background CleanupExpired.
func (s *PostgresStore) ReleaseSlot(ctx context.Context, owner Owner, slotID string, _ time.Time) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND slot_id = $2 AND owner_key = $3`, s.slotsTable),
		owner.OrgID, slotID, ownerKey(owner))
	if err != nil {
		return wrapPostgresErr("ReleaseSlot", err)
	}
	return nil
}

// CleanupExpired removes expired inactive jobs and expired slots in bounded SKIP LOCKED batches
// so background cleanup never deadlocks concurrent org transactions or rolls back on large backlogs.
func (s *PostgresStore) CleanupExpired(ctx context.Context, now time.Time) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	nowMs, err := resolvePostgresNowMs(ctx, s.pool, now)
	if err != nil {
		return wrapPostgresErr("CleanupExpired", err)
	}

	deleteJobsSQL := fmt.Sprintf(`DELETE FROM %s WHERE job_id IN (
		SELECT job_id FROM %s
		WHERE active_op_until_ms <= $1 AND expires_at_ms > 0 AND expires_at_ms < $1
		ORDER BY job_id
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	)`, s.jobsTable, s.jobsTable)
	for ctx.Err() == nil {
		tag, err := s.pool.Exec(ctx, deleteJobsSQL, nowMs, postgresCleanupBatchSize)
		if err != nil {
			return wrapPostgresErr("CleanupExpired", err)
		}
		if tag.RowsAffected() < postgresCleanupBatchSize {
			break
		}
	}

	deleteSlotsSQL := fmt.Sprintf(`DELETE FROM %s WHERE (org_id, slot_id) IN (
		SELECT org_id, slot_id FROM %s
		WHERE expires_at_ms <= $1
		ORDER BY org_id, slot_id
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	)`, s.slotsTable, s.slotsTable)
	for ctx.Err() == nil {
		tag, err := s.pool.Exec(ctx, deleteSlotsSQL, nowMs, postgresCleanupBatchSize)
		if err != nil {
			return wrapPostgresErr("CleanupExpired", err)
		}
		if tag.RowsAffected() < postgresCleanupBatchSize {
			break
		}
	}
	return nil
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return wrapPostgresErr("Ping", err)
	}
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	return nil
}

func (s *PostgresStore) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closed = true
		s.closeCancel()
		s.closeMu.Unlock()
		s.subWG.Wait()
		if s.ownsPool && s.pool != nil {
			s.pool.Close()
		}
	})
	return nil
}

// --- Transactional capacity & slot helpers ---

func (s *PostgresStore) evictExpiredOrgTx(ctx context.Context, tx pgx.Tx, orgID int64, nowMs int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND active_op_until_ms <= $2 AND expires_at_ms > 0 AND expires_at_ms < $2`, s.jobsTable),
		orgID, nowMs)
	return err
}

func (s *PostgresStore) reapExpiredOrgSlotsTx(ctx context.Context, tx pgx.Tx, orgID int64, nowMs int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND expires_at_ms <= $2`, s.slotsTable),
		orgID, nowMs)
	return err
}

func (s *PostgresStore) canAcquireSlotTx(ctx context.Context, tx pgx.Tx, orgID int64, oKey string, nowMs int64, limits StoreLimits) (bool, error) {
	var orgSlots int
	var userSlots int
	err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT
		COUNT(*) FILTER (WHERE expires_at_ms > $2),
		COUNT(*) FILTER (WHERE owner_key = $3 AND expires_at_ms > $2)
	FROM %s WHERE org_id = $1`, s.slotsTable), orgID, nowMs, oKey).Scan(&orgSlots, &userSlots)
	if err != nil {
		return false, err
	}
	if orgSlots >= limits.MaxConcurrentJobs || userSlots >= limits.MaxConcurrentJobsPerUser {
		return false, nil
	}
	return true, nil
}

func (s *PostgresStore) upsertSlotTx(ctx context.Context, tx pgx.Tx, orgID int64, slotID string, oKey string, leaseUntilMs int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (org_id, slot_id, owner_key, expires_at_ms)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (org_id, slot_id) DO UPDATE
		SET owner_key = EXCLUDED.owner_key, expires_at_ms = EXCLUDED.expires_at_ms`, s.slotsTable),
		orgID, slotID, oKey, leaseUntilMs)
	return err
}

func (s *PostgresStore) ensureCapacityTx(ctx context.Context, tx pgx.Tx, caller Owner, callerKey string, nowMs int64, limits StoreLimits) error {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT job_id, owner_key, active_op_until_ms FROM %s WHERE org_id = $1 ORDER BY updated_at_ms ASC, job_id ASC`, s.jobsTable), caller.OrgID)
	if err != nil {
		return wrapPostgresErr("ensureCapacity", err)
	}
	defer rows.Close()

	var callerInactive []string
	var otherOrgInactive []string
	userCount := 0
	orgCount := 0

	for rows.Next() {
		var jID, oKey string
		var activeUntil int64
		if scanErr := rows.Scan(&jID, &oKey, &activeUntil); scanErr != nil {
			return wrapPostgresErr("ensureCapacity", scanErr)
		}
		orgCount++
		isActive := activeUntil > 0 && nowMs < activeUntil
		if oKey == callerKey {
			userCount++
			if !isActive {
				callerInactive = append(callerInactive, jID)
			}
		} else if !isActive {
			otherOrgInactive = append(otherOrgInactive, jID)
		}
	}
	if err := rows.Err(); err != nil {
		return wrapPostgresErr("ensureCapacity", err)
	}

	needUserEvict := 0
	if userCount >= limits.MaxJobsPerUser {
		needUserEvict = userCount - limits.MaxJobsPerUser + 1
	}
	if needUserEvict > len(callerInactive) {
		return ErrConcurrencyLimited
	}

	var victims []string
	victims = append(victims, callerInactive[:needUserEvict]...)
	remainingCaller := callerInactive[needUserEvict:]
	remainingOrgCount := orgCount - needUserEvict

	needOrgEvict := 0
	if remainingOrgCount >= limits.MaxJobs {
		needOrgEvict = remainingOrgCount - limits.MaxJobs + 1
	}
	if needOrgEvict > len(remainingCaller)+len(otherOrgInactive) {
		return ErrConcurrencyLimited
	}

	for i := 0; i < len(remainingCaller) && needOrgEvict > 0; i++ {
		victims = append(victims, remainingCaller[i])
		needOrgEvict--
	}
	for i := 0; i < len(otherOrgInactive) && needOrgEvict > 0; i++ {
		victims = append(victims, otherOrgInactive[i])
		needOrgEvict--
	}

	if len(victims) > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND job_id = ANY($2) AND active_op_until_ms <= $3`, s.jobsTable),
			caller.OrgID, victims, nowMs); err != nil {
			return wrapPostgresErr("ensureCapacity", err)
		}
	}
	return nil
}

// ensureImportCapacityTx transactionally enforces per-user and per-org caps for Import jobs
// without ever evicting another user's non-expired job or any active job.
func (s *PostgresStore) ensureImportCapacityTx(ctx context.Context, tx pgx.Tx, caller Owner, callerKey string, nowMs int64, limits StoreLimits) error {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT job_id, owner_key, active_op_until_ms FROM %s WHERE org_id = $1 ORDER BY updated_at_ms ASC, job_id ASC`, s.jobsTable), caller.OrgID)
	if err != nil {
		return wrapPostgresErr("ensureImportCapacity", err)
	}
	defer rows.Close()

	var callerInactive []string
	userCount := 0
	orgCount := 0

	for rows.Next() {
		var jID, oKey string
		var activeUntil int64
		if scanErr := rows.Scan(&jID, &oKey, &activeUntil); scanErr != nil {
			return wrapPostgresErr("ensureImportCapacity", scanErr)
		}
		orgCount++
		if oKey == callerKey {
			userCount++
			if activeUntil <= 0 || nowMs >= activeUntil {
				callerInactive = append(callerInactive, jID)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return wrapPostgresErr("ensureImportCapacity", err)
	}

	needUserEvict := 0
	if userCount >= limits.MaxJobsPerUser {
		needUserEvict = userCount - limits.MaxJobsPerUser + 1
	}
	needOrgEvict := 0
	if orgCount >= limits.MaxJobs {
		needOrgEvict = orgCount - limits.MaxJobs + 1
	}
	needTotal := needUserEvict
	if needOrgEvict > needTotal {
		needTotal = needOrgEvict
	}
	if needTotal > len(callerInactive) {
		return ErrConcurrencyLimited
	}
	if needTotal > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND job_id = ANY($2) AND active_op_until_ms <= $3`, s.jobsTable),
			caller.OrgID, callerInactive[:needTotal], nowMs); err != nil {
			return wrapPostgresErr("ensureImportCapacity", err)
		}
	}
	return nil
}

// --- Serialization & error helpers ---

func marshalGaugeMap(gauges map[string]bool) (string, error) {
	if len(gauges) == 0 {
		return "[]", nil
	}
	list := make([]string, 0, len(gauges))
	for k, v := range gauges {
		if v {
			list = append(list, k)
		}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalGaugeMap(raw string) (map[string]bool, error) {
	out := make(map[string]bool)
	if raw == "" || raw == "[]" {
		return out, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	mergeGaugeMapBounded(out, list)
	return out, nil
}

func isPgUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isDomainJobErr(err error) bool {
	return errors.Is(err, ErrJobNotFound) ||
		errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrOperationInProgress) ||
		errors.Is(err, ErrConcurrencyLimited) ||
		errors.Is(err, ErrBudgetExhausted) ||
		errors.Is(err, ErrNoUndoHistory) ||
		errors.Is(err, ErrJobCancelled) ||
		errors.Is(err, ir.ErrRevisionConflict) ||
		errors.Is(err, ir.ErrPanelNotFound)
}

func wrapPostgresErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrStoreUnavailable) {
		return err
	}
	return fmt.Errorf("%w: postgres %s: %v", ErrStoreUnavailable, op, err)
}
