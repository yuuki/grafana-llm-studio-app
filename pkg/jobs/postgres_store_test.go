package jobs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

func TestPostgresStore_StrictConfigAndIdentifierValidation(t *testing.T) {
	validAddrs := []string{"localhost:5432", "postgres.internal:5432", "127.0.0.1:5432", "[::1]:5432", "[2001:db8::1]:5433"}
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
		"postgres://localhost:5432/grafana",
		"user:pass@localhost:5432",
	}
	for _, addr := range invalidAddrs {
		if err := jobs.ValidatePostgresAddr(addr); err == nil {
			t.Fatalf("expected invalid postgresAddr %q to be rejected", addr)
		}
	}

	// Database validation.
	for _, badDB := range []string{"", "   ", "my db", "db\nname", strings.Repeat("d", 64)} {
		if err := jobs.ValidatePostgresDatabase(badDB); err == nil {
			t.Fatalf("expected invalid postgresDatabase %q to fail", badDB)
		}
	}
	if err := jobs.ValidatePostgresDatabase("grafana-prod_1"); err != nil {
		t.Fatalf("expected valid postgresDatabase to pass, got %v", err)
	}

	// Username validation.
	for _, badUser := range []string{"", "   ", "user\x00name", "user\nname", strings.Repeat("u", 129)} {
		if err := jobs.ValidatePostgresUsername(badUser); err == nil {
			t.Fatalf("expected invalid postgresUsername %q to fail", badUser)
		}
	}
	if err := jobs.ValidatePostgresUsername("grafana@my-pg-server"); err != nil {
		t.Fatalf("expected Azure/managed Postgres username with @ to pass, got %v", err)
	}

	// Schema and tablePrefix SQL identifier validation.
	for _, badSchema := range []string{"123schema", "bad-schema", "public; DROP TABLE x;", "bad schema", strings.Repeat("s", 64)} {
		if err := jobs.ValidatePostgresSchema(badSchema); err == nil {
			t.Fatalf("expected invalid postgresSchema %q to fail", badSchema)
		}
	}
	for _, badPrefix := range []string{"1prefix", "bad-prefix", "llm;drop", strings.Repeat("p", 33)} {
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
