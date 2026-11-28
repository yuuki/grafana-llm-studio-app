package jobs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	return validateHostPortAddr("postgresAddr", "postgres", addr)
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
