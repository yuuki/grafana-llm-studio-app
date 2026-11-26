package jobs_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

func TestPostgresStore_StrictConfigAndIdentifierValidation(t *testing.T) {
	validAddrs := []string{
		"localhost:5432",
		"postgres.internal:5432",
		"127.0.0.1:5432",
		"[::1]:5432",
		"[2001:db8::1]:5433",
		"[::ffff:127.0.0.1]:5432",
	}
	for _, addr := range validAddrs {
		if err := jobs.ValidatePostgresAddr(addr); err != nil {
			t.Fatalf("expected valid postgresAddr %q to pass, got %v", addr, err)
		}
	}
	invalidAddrs := []string{
		"",
		"postgres:0",
		"postgres:65536",
		"postgres:+5432",
		"postgres:05432",
		"postgres : 5432",
		"postgres host:5432",
		"postgres\r\n:5432",
		"postgres/db:5432",
		"::1:5432",
		"[]:5432",
		"[::1:5432",
		"::1]:5432",
		"[127.0.0.1]:5432",
		"[postgres]:5432",
		"[:::]:5432",
		"postgres://localhost:5432/grafana",
		"user:pass@localhost:5432",
	}
	for _, addr := range invalidAddrs {
		if err := jobs.ValidatePostgresAddr(addr); err == nil {
			t.Fatalf("expected invalid postgresAddr %q to be rejected", addr)
		}
	}

	// Database validation (UTF-8 byte cap, whitespace, control, BOM).
	for _, badDB := range []string{
		"",
		"   ",
		"my db",
		"db\nname",
		"\ufeffgrafana",
		strings.Repeat("d", 64),
		strings.Repeat("あ", 22), // 66 UTF-8 bytes > 63
	} {
		if err := jobs.ValidatePostgresDatabase(badDB); err == nil {
			t.Fatalf("expected invalid postgresDatabase %q to fail", badDB)
		}
	}
	if err := jobs.ValidatePostgresDatabase("grafana-prod_1"); err != nil {
		t.Fatalf("expected valid postgresDatabase to pass, got %v", err)
	}

	// Username validation (UTF-8 byte cap, control, BOM).
	for _, badUser := range []string{
		"",
		"   ",
		"user\x00name",
		"user\nname",
		"\ufeffgrafana",
		strings.Repeat("u", 129),
		strings.Repeat("あ", 43), // 129 UTF-8 bytes > 128
	} {
		if err := jobs.ValidatePostgresUsername(badUser); err == nil {
			t.Fatalf("expected invalid postgresUsername %q to fail", badUser)
		}
	}
	if err := jobs.ValidatePostgresUsername("grafana@my-pg-server"); err != nil {
		t.Fatalf("expected Azure/managed Postgres username with @ to pass, got %v", err)
	}

	// Schema and tablePrefix SQL identifier validation (lowercase only + reserved schema rejection).
	for _, badSchema := range []string{
		"123schema",
		"bad-schema",
		"public; DROP TABLE x;",
		"bad schema",
		"PUBLIC",
		"MySchema",
		"pg_catalog",
		"pg_temp",
		"pg_toast",
		"pg_custom",
		"information_schema",
		strings.Repeat("s", 64),
	} {
		if err := jobs.ValidatePostgresSchema(badSchema); err == nil {
			t.Fatalf("expected invalid postgresSchema %q to fail", badSchema)
		}
	}
	for _, badPrefix := range []string{"1prefix", "bad-prefix", "llm;drop", "LLM_", strings.Repeat("p", 33)} {
		if err := jobs.ValidatePostgresTablePrefix(badPrefix); err == nil {
			t.Fatalf("expected invalid postgresTablePrefix %q to fail", badPrefix)
		}
	}

	// SSLMode validation.
	for _, mode := range []string{"", "disable", "prefer", "require", "verify-ca", "verify-full", "VERIFY-FULL"} {
		if err := jobs.ValidatePostgresSSLMode(mode); err != nil {
			t.Fatalf("expected valid postgresSslMode %q to pass, got %v", mode, err)
		}
	}
	for _, badMode := range []string{"allow", "on", "true", "invalid"} {
		if err := jobs.ValidatePostgresSSLMode(badMode); err == nil {
			t.Fatalf("expected invalid postgresSslMode %q to fail", badMode)
		}
	}

	// NewPostgresStore constructs lazy pools for all 5 SSL modes without dialing eagerly.
	for _, mode := range []string{"disable", "prefer", "require", "verify-ca", "verify-full"} {
		s, err := jobs.NewPostgresStore(jobs.PostgresConfig{
			Addr:        "127.0.0.1:5432",
			Database:    "grafana",
			Username:    "grafana",
			Password:    "super-secret-pg-pass",
			Schema:      "llm_studio",
			TablePrefix: "studio_",
			SSLMode:     mode,
		})
		if err != nil {
			t.Fatalf("NewPostgresStore sslMode=%s failed: %v", mode, err)
		}
		if _, err := s.SubscribeCancels(context.Background(), nil); err == nil {
			t.Fatal("expected nil onCancel callback to be rejected")
		}
		_ = s.Close()
	}

	// Unreachable PostgreSQL MUST return ErrStoreUnavailable on Ping and never leak password.
	badStore, err := jobs.NewPostgresStore(jobs.PostgresConfig{
		Addr:           "127.0.0.1:1",
		Database:       "grafana",
		Username:       "grafana",
		Password:       "super-secret-pg-pass-never-leak",
		SSLMode:        jobs.PostgresSSLModeDisable,
		ConnectTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPostgresStore unreachable: %v", err)
	}
	defer func() { _ = badStore.Close() }()

	pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pingErr := badStore.Ping(pingCtx)
	if !errors.Is(pingErr, jobs.ErrStoreUnavailable) {
		t.Fatalf("expected ErrStoreUnavailable on unreachable Postgres Ping, got %v", pingErr)
	}
	if strings.Contains(pingErr.Error(), "super-secret-pg-pass-never-leak") {
		t.Fatalf("password leaked in Postgres Ping error: %v", pingErr)
	}
}

func TestPostgresStore_AmbientPGEnvVarsDoNotHijackOrBreakStore(t *testing.T) {
	// Poison every ambient libpq env var that pgconn.parseEnvSettings() reads.
	t.Setenv("PGHOST", "evil.example.invalid")
	t.Setenv("PGPORT", "not-a-port")
	t.Setenv("PGDATABASE", "wrong_db")
	t.Setenv("PGUSER", "wrong_user")
	t.Setenv("PGPASSWORD", "wrong_pass")
	t.Setenv("PGSSLMODE", "verify-full")
	t.Setenv("PGSSLROOTCERT", "/nonexistent/ca.pem")
	t.Setenv("PGSSLCERT", "/nonexistent/client.crt")
	t.Setenv("PGSSLKEY", "/nonexistent/client.key")
	t.Setenv("PGCONNECT_TIMEOUT", "invalid-timeout")
	t.Setenv("PGTARGETSESSIONATTRS", "read-only")
	t.Setenv("PGSSLNEGOTIATION", "direct")

	s, err := jobs.NewPostgresStore(jobs.PostgresConfig{
		Addr:     "127.0.0.1:5432",
		Database: "grafana",
		Username: "grafana",
		Password: "pw",
		SSLMode:  jobs.PostgresSSLModeDisable,
	})
	if err != nil {
		t.Fatalf("NewPostgresStore failed under hostile PG* env vars: %v", err)
	}
	_ = s.Close()

	// If live Postgres is available, verify writes actually succeed despite PGTARGETSESSIONATTRS=read-only.
	if cfg, ok := testPostgresConfigFromEnv(t); ok {
		defer dropTestPostgresSchema(cfg)
		live, err := jobs.NewPostgresStore(cfg)
		if err != nil {
			t.Fatalf("live NewPostgresStore under hostile PG* env vars failed: %v", err)
		}
		defer func() { _ = live.Close() }()
		if err := live.Ping(context.Background()); err != nil {
			t.Fatalf("live Ping under hostile PG* env vars failed: %v", err)
		}
	}
}

func TestPostgresStore_CrossReplicaCancelListenNotifyUnwindsSlotImmediately(t *testing.T) {
	cfg, ok := testPostgresConfigFromEnv(t)
	if !ok {
		t.Skip("set LLM_STUDIO_TEST_POSTGRES_ADDR to run live PostgreSQL integration tests")
	}
	defer dropTestPostgresSchema(cfg)

	store1, err := jobs.NewPostgresStore(cfg)
	if err != nil {
		t.Fatalf("NewPostgresStore 1: %v", err)
	}
	defer func() { _ = store1.Close() }()

	store2, err := jobs.NewPostgresStore(cfg)
	if err != nil {
		t.Fatalf("NewPostgresStore 2: %v", err)
	}
	defer func() { _ = store2.Close() }()

	patchStarted := make(chan struct{})
	cancelledInLLM := make(chan struct{})
	stub1 := &stubLLM{
		onGenerate: (&llm.MockClient{}).GenerateIR,
		onPatch: func(ctx context.Context, _ llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
			close(patchStarted)
			<-ctx.Done()
			close(cancelledInLLM)
			return nil, llm.CompletionMeta{ProviderMode: "stub"}, ctx.Err()
		},
	}

	mgr1 := jobs.NewManager(stub1, jobs.Config{
		AllowedDSUIDs:            []string{"prom-main"},
		MaxConcurrentJobs:        1,
		MaxConcurrentJobsPerUser: 1,
		JobDeadline:              60 * time.Second,
		Store:                    store1,
	})
	defer mgr1.Close()
	mgr2 := jobs.NewManager(&llm.MockClient{}, jobs.Config{
		AllowedDSUIDs:            []string{"prom-main"},
		MaxConcurrentJobs:        1,
		MaxConcurrentJobsPerUser: 1,
		JobDeadline:              60 * time.Second,
		Store:                    store2,
	})
	defer mgr2.Close()

	alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
	s1, err := mgr1.Generate(context.Background(), alice, jobs.GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		UserIntent:    "Show request rate and error rate",
		Catalog:       testCatalog(),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	patchDone := make(chan error, 1)
	go func() {
		_, pErr := mgr1.Patch(context.Background(), alice, s1.JobID, jobs.PatchInput{
			BaseRevision:  1,
			TargetPanelID: "panel-error-rate",
			EditIntent:    "Slow edit",
			Catalog:       testCatalog(),
		})
		patchDone <- pErr
	}()
	<-patchStarted

	// Cancel arrives at Replica 2 -> PostgreSQL LISTEN/NOTIFY MUST immediately cancel Replica 1's LLM context and free the slot!
	if _, err := mgr2.Cancel(alice, s1.JobID); err != nil {
		t.Fatalf("Replica 2 Cancel failed: %v", err)
	}

	select {
	case <-cancelledInLLM:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for PostgreSQL LISTEN/NOTIFY to cancel Replica 1 LLM context")
	}
	if pErr := <-patchDone; !errors.Is(pErr, jobs.ErrJobCancelled) {
		t.Fatalf("expected Replica 1 Patch to return ErrJobCancelled, got %v", pErr)
	}

	// Slot is immediately free on Replica 2 (no 60-75s lockout!).
	if _, err := mgr2.Generate(context.Background(), alice, jobs.GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		UserIntent:    "Fresh generation after cross-replica cancel",
		Catalog:       testCatalog(),
	}); err != nil {
		t.Fatalf("expected immediate Generate on Replica 2 after cross-replica cancel, got %v", err)
	}
}

func TestPostgresStore_ConcurrentSchemaInitAndWallClockTTL(t *testing.T) {
	cfg, ok := testPostgresConfigFromEnv(t)
	if !ok {
		t.Skip("set LLM_STUDIO_TEST_POSTGRES_ADDR to run live PostgreSQL integration tests")
	}
	defer dropTestPostgresSchema(cfg)

	// Launch 4 concurrent replicas initializing the exact same fresh custom schema simultaneously.
	const replicas = 4
	stores := make([]*jobs.PostgresStore, replicas)
	for i := 0; i < replicas; i++ {
		s, err := jobs.NewPostgresStore(cfg)
		if err != nil {
			t.Fatalf("NewPostgresStore %d: %v", i, err)
		}
		stores[i] = s
		defer func() { _ = s.Close() }()
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errCh := make(chan error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		s := stores[i]
		go func() {
			defer wg.Done()
			<-start
			errCh <- s.Ping(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Ping / schema auto-migration failed: %v", err)
		}
	}

	// Verify real PostgreSQL clock_timestamp() TTL expiration + CleanupExpired.
	mgr := jobs.NewManager(&llm.MockClient{}, jobs.Config{
		AllowedDSUIDs: []string{"prom-main"},
		JobTTL:        200 * time.Millisecond,
		Store:         stores[0],
	})
	defer mgr.Close()

	alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
	snap, err := mgr.Generate(context.Background(), alice, jobs.GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		UserIntent:    "Show request rate",
		Catalog:       testCatalog(),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if err := stores[1].CleanupExpired(context.Background(), time.Time{}); err != nil {
		t.Fatalf("CleanupExpired: %v", err)
	}
	if _, err := mgr.Get(alice, snap.JobID); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound after PostgreSQL TTL expiration, got %v", err)
	}
}

func TestPostgresStore_MultiOrgSameSlotIDAndDeadlockFreeCleanupStress_Race(t *testing.T) {
	cfg, ok := testPostgresConfigFromEnv(t)
	if !ok {
		t.Skip("set LLM_STUDIO_TEST_POSTGRES_ADDR to run live PostgreSQL integration tests")
	}
	defer dropTestPostgresSchema(cfg)

	store, err := jobs.NewPostgresStore(cfg)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	t0 := time.Date(2026, 11, 24, 16, 0, 0, 0, time.UTC)
	limits := jobs.StoreLimits{
		MaxJobs:                  20,
		MaxJobsPerUser:           10,
		JobTTL:                   30 * time.Second,
		MaxConcurrentJobs:        8,
		MaxConcurrentJobsPerUser: 4,
		OpLeaseTTL:               10 * time.Second,
	}

	// Multi-org identical SlotID MUST NOT collide across orgs.
	org1Owner := jobs.Owner{OrgID: 101, UserLogin: "alice"}
	org2Owner := jobs.Owner{OrgID: 202, UserLogin: "alice"}
	if _, err := store.CreateGenerating(context.Background(), jobs.CreateGeneratingParams{
		JobID: "job-org1-shared-slot", Owner: org1Owner, DatasourceUID: "prom-main",
		SlotID: "identical-slot-id", Now: t0, Limits: limits,
	}); err != nil {
		t.Fatalf("org1 CreateGenerating: %v", err)
	}
	if _, err := store.CreateGenerating(context.Background(), jobs.CreateGeneratingParams{
		JobID: "job-org2-shared-slot", Owner: org2Owner, DatasourceUID: "prom-main",
		SlotID: "identical-slot-id", Now: t0, Limits: limits,
	}); err != nil {
		t.Fatalf("org2 CreateGenerating with same SlotID failed (cross-org slot collision): %v", err)
	}

	// Seed several expired jobs & slots, then hammer CleanupExpired + ReleaseSlot + CreateGenerating
	// concurrently to verify zero PostgreSQL 40P01 deadlocks.
	for i := 0; i < 6; i++ {
		u := jobs.Owner{OrgID: 1, UserLogin: fmt.Sprintf("user-%d", i)}
		_, err := store.CreateGenerating(context.Background(), jobs.CreateGeneratingParams{
			JobID: fmt.Sprintf("exp-job-%d", i), Owner: u, DatasourceUID: "prom-main",
			SlotID: fmt.Sprintf("exp-slot-%d", i), Now: t0, Limits: limits,
		})
		if err != nil {
			t.Fatalf("seed CreateGenerating %d: %v", i, err)
		}
	}

	later := t0.Add(2 * time.Minute)
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	start := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 5; j++ {
				if e := store.CleanupExpired(context.Background(), later); e != nil {
					errCh <- fmt.Errorf("CleanupExpired: %w", e)
				}
			}
		}()
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			<-start
			u := jobs.Owner{OrgID: 1, UserLogin: fmt.Sprintf("user-%d", idx)}
			if e := store.ReleaseSlot(context.Background(), u, fmt.Sprintf("exp-slot-%d", idx), later); e != nil {
				errCh <- fmt.Errorf("ReleaseSlot %d: %w", idx, e)
			}
			lease, e := store.CreateGenerating(context.Background(), jobs.CreateGeneratingParams{
				JobID: fmt.Sprintf("fresh-job-%d", idx), Owner: u, DatasourceUID: "prom-main",
				SlotID: fmt.Sprintf("fresh-slot-%d", idx), Now: later, Limits: limits,
			})
			if e != nil {
				errCh <- fmt.Errorf("CreateGenerating %d: %w", idx, e)
				return
			}
			if e := store.ReleaseSlot(context.Background(), u, lease.SlotID, later); e != nil {
				errCh <- fmt.Errorf("ReleaseSlot fresh %d: %w", idx, e)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("deadlock/concurrency stress failure: %v", e)
	}
}

func TestPostgresStore_LeastPrivilegeDMLOnlyRoleAndExternalPoolOwnership(t *testing.T) {
	cfg, ok := testPostgresConfigFromEnv(t)
	if !ok {
		t.Skip("set LLM_STUDIO_TEST_POSTGRES_ADDR to run live PostgreSQL integration tests")
	}
	defer dropTestPostgresSchema(cfg)

	// Step 1: Bootstrap schema & tables using the admin test user.
	adminStore, err := jobs.NewPostgresStore(cfg)
	if err != nil {
		t.Fatalf("admin NewPostgresStore: %v", err)
	}
	if err := adminStore.Ping(context.Background()); err != nil {
		_ = adminStore.Close()
		t.Fatalf("admin Ping bootstrap: %v", err)
	}
	_ = adminStore.Close()

	// Step 2: Create a least-privilege DML-only role (NO CREATE on schema, NO table ownership).
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	roleName := "llmstudio_dml_" + hex.EncodeToString(rnd[:])
	rolePass := "dml-pass-@#:test"

	adminPool, err := pgxpool.New(context.Background(), buildTestPostgresDSN(cfg))
	if err != nil {
		t.Fatalf("admin pgxpool.New: %v", err)
	}
	defer func() {
		_, _ = adminPool.Exec(context.Background(), "REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA "+pgx.Identifier{cfg.Schema}.Sanitize()+" FROM "+pgx.Identifier{roleName}.Sanitize())
		_, _ = adminPool.Exec(context.Background(), "REVOKE ALL PRIVILEGES ON SCHEMA "+pgx.Identifier{cfg.Schema}.Sanitize()+" FROM "+pgx.Identifier{roleName}.Sanitize())
		_, _ = adminPool.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{roleName}.Sanitize())
		adminPool.Close()
	}()

	roleIdent := pgx.Identifier{roleName}.Sanitize()
	schemaIdent := pgx.Identifier{cfg.Schema}.Sanitize()
	for _, stmt := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", roleIdent, rolePass),
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", schemaIdent, roleIdent),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s", schemaIdent, roleIdent),
	} {
		if _, err := adminPool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("setup DML role (%s): %v", stmt, err)
		}
	}

	dmlCfg := cfg
	dmlCfg.Username = roleName
	dmlCfg.Password = rolePass
	dmlStore, err := jobs.NewPostgresStore(dmlCfg)
	if err != nil {
		t.Fatalf("DML NewPostgresStore: %v", err)
	}
	defer func() { _ = dmlStore.Close() }()

	if err := dmlStore.Ping(context.Background()); err != nil {
		t.Fatalf("DML-only role Ping failed (ensureSchema must skip DDL when tables exist): %v", err)
	}
	mgr := jobs.NewManager(&llm.MockClient{}, jobs.Config{
		AllowedDSUIDs: []string{"prom-main"},
		Store:         dmlStore,
	})
	defer mgr.Close()

	alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
	snap, err := mgr.Generate(context.Background(), alice, jobs.GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		UserIntent:    "Show request rate",
		Catalog:       testCatalog(),
	})
	if err != nil {
		t.Fatalf("DML-only Generate failed: %v", err)
	}
	if _, err := mgr.Get(alice, snap.JobID); err != nil {
		t.Fatalf("DML-only Get failed: %v", err)
	}

	// Step 3: Verify externally supplied Pool is NOT closed by PostgresStore.Close().
	extStore, err := jobs.NewPostgresStore(jobs.PostgresConfig{
		Pool:        adminPool,
		Schema:      cfg.Schema,
		TablePrefix: cfg.TablePrefix,
	})
	if err != nil {
		t.Fatalf("external pool NewPostgresStore: %v", err)
	}
	stopSub, err := extStore.SubscribeCancels(context.Background(), func(string, uint64) {})
	if err != nil {
		t.Fatalf("external pool SubscribeCancels: %v", err)
	}
	// Closing extStore MUST stop its listener without closing adminPool.
	if err := extStore.Close(); err != nil {
		t.Fatalf("extStore.Close: %v", err)
	}
	stopSub()
	if err := adminPool.Ping(context.Background()); err != nil {
		t.Fatalf("external pool was closed by PostgresStore.Close(): %v", err)
	}
}
