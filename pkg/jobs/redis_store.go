package jobs

import (
	"context"
	"crypto/tls"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	_ JobStore           = (*RedisStore)(nil)
	_ CancelSubscription = (*RedisStore)(nil)
)

const (
	DefaultRedisKeyPrefix = "llmstudio:jobs"
	redisIndexTTLBufferMs = int64(60_000)
	redisDefaultTimeout   = 5 * time.Second
)

var validRedisKeyPrefixRe = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,64}$`)

// RedisConfig configures the optional persistent Redis JobStore for HA Grafana deployments.
type RedisConfig struct {
	Addr         string
	Username     string
	Password     string
	DB           int
	KeyPrefix    string
	TLS          bool
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// Client optionally supplies a pre-configured redis.UniversalClient (e.g. for custom TLS,
	// Sentinel, Cluster, or testing). When set, Addr/Username/Password/DB/TLS are ignored and
	// Close() does not close the externally owned client.
	Client redis.UniversalClient
}

// ValidateRedisAddr validates that addr is a bare host:port (or [ipv6]:port) with port in 1..65535,
// disallowing URL schemes, embedded credentials, whitespace, control characters, and malformed hosts.
func ValidateRedisAddr(addr string) error {
	return validateHostPortAddr("redisAddr", "redis", addr)
}

// ValidateRedisKeyPrefix validates the Redis key prefix charset and length.
func ValidateRedisKeyPrefix(prefix string) error {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return nil
	}
	if !validRedisKeyPrefixRe.MatchString(trimmed) {
		return fmt.Errorf("invalid redisKeyPrefix %q (allowed: 1..64 chars of [a-zA-Z0-9:_-])", trimmed)
	}
	return nil
}

// RedisStore is an atomic, org-isolated, owner-isolated, TTL-backed JobStore for HA Grafana.
// All Lua scripts access ONLY keys explicitly declared in KEYS[] under a hash-tagged prefix
// ({<keyPrefix>}), use Redis TIME as the authoritative clock when Now is zero, disable
// non-idempotent auto-retries (MaxRetries=-1), and broadcast Cancel/Undo fencing events via Pub/Sub.
type RedisStore struct {
	client     redis.UniversalClient
	ownsClient bool
	prefix     string // hash-tagged prefix, e.g. "{llmstudio:jobs}"
}

// NewRedisStore creates a Redis-backed JobStore from cfg.
func NewRedisStore(cfg RedisConfig) (*RedisStore, error) {
	if err := ValidateRedisKeyPrefix(cfg.KeyPrefix); err != nil {
		return nil, err
	}
	keyPrefix := strings.TrimSpace(cfg.KeyPrefix)
	if keyPrefix == "" {
		keyPrefix = DefaultRedisKeyPrefix
	}
	tagPrefix := "{" + keyPrefix + "}"

	if cfg.Client != nil {
		return &RedisStore{
			client:     cfg.Client,
			ownsClient: false,
			prefix:     tagPrefix,
		}, nil
	}

	if err := ValidateRedisAddr(cfg.Addr); err != nil {
		return nil, err
	}
	if cfg.DB < 0 || cfg.DB > 15 {
		return nil, fmt.Errorf("redisDb must be between 0 and 15, got %d", cfg.DB)
	}

	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = redisDefaultTimeout
	}
	readTimeout := cfg.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = redisDefaultTimeout
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = redisDefaultTimeout
	}

	opts := &redis.Options{
		Addr:         strings.TrimSpace(cfg.Addr),
		Username:     strings.TrimSpace(cfg.Username),
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  dialTimeout,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		// Disable automatic retries on non-idempotent Lua mutations (BeginRepair, Undo, CommitRevision, etc.)
		// so a lost reply after server-side execution never double-executes state transitions.
		MaxRetries: -1,
	}
	if cfg.TLS {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	client := redis.NewClient(opts)
	return &RedisStore{
		client:     client,
		ownsClient: true,
		prefix:     tagPrefix,
	}, nil
}

func (s *RedisStore) jobKey(jobID string) string {
	return s.prefix + ":job:" + jobID
}

func (s *RedisStore) orgIdxKey(orgID int64) string {
	return s.prefix + ":idx:org:" + strconv.FormatInt(orgID, 10)
}

func (s *RedisStore) orgMetaKey(orgID int64) string {
	return s.prefix + ":meta:org:" + strconv.FormatInt(orgID, 10)
}

func (s *RedisStore) userIdxKey(owner Owner) string {
	return s.prefix + ":idx:user:" + ownerKey(owner)
}

func (s *RedisStore) userIdxKeyByOwnerKey(oKey string) string {
	return s.prefix + ":idx:user:" + oKey
}

func (s *RedisStore) orgSlotsKey(orgID int64) string {
	return s.prefix + ":slots:org:" + strconv.FormatInt(orgID, 10)
}

func (s *RedisStore) userSlotsKey(owner Owner) string {
	return s.prefix + ":slots:user:" + ownerKey(owner)
}

func (s *RedisStore) orgsSetKey() string {
	return s.prefix + ":orgs"
}

func (s *RedisStore) cancelChannel() string {
	return s.prefix + ":cancel"
}

func redisNowArgMs(now time.Time) string {
	if now.IsZero() {
		return "0"
	}
	return strconv.FormatInt(now.UTC().UnixMilli(), 10)
}

// SubscribeCancels subscribes to Redis Pub/Sub fencing notifications emitted by Cancel and Undo
// so remote replicas cancel in-flight LLM contexts and unwind concurrency slots immediately.
func (s *RedisStore) SubscribeCancels(ctx context.Context, onCancel func(jobID string, fencedToken uint64)) (func(), error) {
	pubsub := s.client.Subscribe(ctx, s.cancelChannel())
	// Wait for SUBSCRIBE confirmation before returning so immediate Cancel calls are never missed.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, wrapRedisErr("SubscribeCancels", err)
	}

	ch := pubsub.Channel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for msg := range ch {
			if msg == nil {
				continue
			}
			if jobID, token, ok := parseCancelNotification(msg.Payload); ok {
				onCancel(jobID, token)
			}
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = pubsub.Close()
			wg.Wait()
		})
	}
	return stop, nil
}

func (s *RedisStore) Ping(ctx context.Context) error {
	if err := s.client.Ping(ctx).Err(); err != nil {
		return wrapRedisErr("Ping", err)
	}
	return nil
}

func (s *RedisStore) Close() error {
	if s.ownsClient && s.client != nil {
		return s.client.Close()
	}
	return nil
}
