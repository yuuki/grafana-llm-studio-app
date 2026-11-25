package plugin_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/plugin"
)

func TestLoadSettings_JobStoreValidation(t *testing.T) {
	// Default jobStoreBackend is memory with 30m TTL.
	s, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"]}`),
	})
	if err != nil {
		t.Fatalf("LoadSettings default failed: %v", err)
	}
	if s.JobStoreBackend != plugin.JobStoreBackendMemory || s.JobTTLMinutes != 30 {
		t.Fatalf("unexpected defaults: backend=%q ttlMinutes=%d", s.JobStoreBackend, s.JobTTLMinutes)
	}

	// Unsupported backend rejected.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"sqlite"}`),
	}); err == nil {
		t.Fatal("expected unsupported jobStoreBackend to fail")
	}

	// Redis backend without redisAddr rejected.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis"}`),
	}); err == nil {
		t.Fatal("expected jobStoreBackend=redis without redisAddr to fail")
	}

	// Redis backend with URL/credentials in redisAddr rejected.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"redis://:secret@redis:6379"}`),
	}); err == nil {
		t.Fatal("expected credentials/URL scheme in redisAddr to be rejected")
	}

	// Redis backend with out-of-range DB or invalid key prefix rejected.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"redis:6379","redisDb":16}`),
	}); err == nil {
		t.Fatal("expected redisDb=16 to fail")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"redis:6379","redisKeyPrefix":"bad prefix!"}`),
	}); err == nil {
		t.Fatal("expected invalid redisKeyPrefix to fail")
	}

	// Valid Redis config accepted and normalized.
	sRedis, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"REDIS","redisAddr":"redis.internal:6379","redisDb":2,"redisTls":true,"jobTtlMinutes":60}`),
	})
	if err != nil {
		t.Fatalf("expected valid redis settings to pass, got %v", err)
	}
	if sRedis.JobStoreBackend != plugin.JobStoreBackendRedis || sRedis.RedisKeyPrefix != jobs.DefaultRedisKeyPrefix || sRedis.JobTTLMinutes != 60 || !sRedis.RedisTLS {
		t.Fatalf("unexpected parsed redis settings: %+v", sRedis)
	}

	// Postgres backend validation: required fields & strict format checks.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"postgres"}`),
	}); err == nil {
		t.Fatal("expected jobStoreBackend=postgres without postgresAddr to fail")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"postgres","postgresAddr":"postgres://user:pass@pg:5432/grafana","postgresDatabase":"grafana","postgresUsername":"grafana"}`),
	}); err == nil {
		t.Fatal("expected credentials/URL scheme in postgresAddr to be rejected")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"postgres","postgresAddr":"pg:5432","postgresUsername":"grafana"}`),
	}); err == nil {
		t.Fatal("expected jobStoreBackend=postgres without postgresDatabase to fail")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"postgres","postgresAddr":"pg:5432","postgresDatabase":"grafana"}`),
	}); err == nil {
		t.Fatal("expected jobStoreBackend=postgres without postgresUsername to fail")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"postgres","postgresAddr":"pg:5432","postgresDatabase":"grafana","postgresUsername":"grafana","postgresSchema":"bad;schema"}`),
	}); err == nil {
		t.Fatal("expected invalid postgresSchema to fail")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"postgres","postgresAddr":"pg:5432","postgresDatabase":"grafana","postgresUsername":"grafana","postgresTablePrefix":"bad-prefix"}`),
	}); err == nil {
		t.Fatal("expected invalid postgresTablePrefix to fail")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"postgres","postgresAddr":"pg:5432","postgresDatabase":"grafana","postgresUsername":"grafana","postgresSslMode":"allow"}`),
	}); err == nil {
		t.Fatal("expected unsupported postgresSslMode to fail")
	}

	// Valid Postgres config accepted and normalized with defaults.
	sPg, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"POSTGRES","postgresAddr":"postgres.internal:5432","postgresDatabase":"grafana","postgresUsername":"grafana","jobTtlMinutes":45}`),
	})
	if err != nil {
		t.Fatalf("expected valid postgres settings to pass, got %v", err)
	}
	if sPg.JobStoreBackend != plugin.JobStoreBackendPostgres ||
		sPg.PostgresSchema != jobs.DefaultPostgresSchema ||
		sPg.PostgresTablePrefix != jobs.DefaultPostgresTablePrefix ||
		sPg.PostgresSSLMode != jobs.DefaultPostgresSSLMode ||
		sPg.JobTTLMinutes != 45 {
		t.Fatalf("unexpected parsed postgres settings: %+v", sPg)
	}

	// Stale hidden postgres fields MUST be ignored when jobStoreBackend=memory.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"memory","postgresSchema":"bad;schema","postgresSslMode":"invalid"}`),
	}); err != nil {
		t.Fatalf("expected stale postgres fields to be ignored when jobStoreBackend=memory, got %v", err)
	}
}

func TestApp_MultiReplicaRedisJobStoreHTTPAndSecretIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("super-secret-redis-pass-999")
	redisAddr := mr.Addr()

	appSettings := backend.AppInstanceSettings{
		JSONData: []byte(`{
			"llmMode": "mock",
			"allowMockInDev": true,
			"allowedDatasourceUids": ["prom-1"],
			"jobStoreBackend": "redis",
			"redisAddr": "` + redisAddr + `",
			"redisKeyPrefix": "ha-plugin-test",
			"jobTtlMinutes": 45
		}`),
		DecryptedSecureJSONData: map[string]string{
			"redisPassword":          "super-secret-redis-pass-999",
			"llmServiceAccountToken": "glsa_secret_token",
		},
	}

	inst1, err := plugin.NewApp(context.Background(), appSettings)
	if err != nil {
		t.Fatalf("NewApp replica 1: %v", err)
	}
	app1 := inst1.(*plugin.App)

	inst2, err := plugin.NewApp(context.Background(), appSettings)
	if err != nil {
		t.Fatalf("NewApp replica 2: %v", err)
	}
	app2 := inst2.(*plugin.App)
	defer app2.Dispose()

	// CheckHealth verifies live Redis connectivity and reports jobStore=redis without leaking secrets.
	health, err := app1.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if err != nil || health.Status != backend.HealthStatusOk {
		t.Fatalf("expected CheckHealth OK with live Redis, got err=%v health=%+v", err, health)
	}
	if !strings.Contains(health.Message, "jobStore=redis") {
		t.Fatalf("expected jobStore=redis in health message, got %q", health.Message)
	}
	if strings.Contains(health.Message, "super-secret-redis-pass-999") {
		t.Fatal("redisPassword leaked in CheckHealth message")
	}

	aliceCtx := backend.PluginContext{OrgID: 1, User: &backend.User{Login: "alice", Role: "Admin"}}
	bobCtx := backend.PluginContext{OrgID: 1, User: &backend.User{Login: "bob", Role: "Editor"}}

	// /config/public and /health MUST NEVER leak redisPassword or redisAddr.
	for _, p := range []string{"config/public", "health"} {
		sender := &captureSender{}
		_ = app1.CallResource(context.Background(), &backend.CallResourceRequest{
			PluginContext: aliceCtx,
			Path:          p,
			Method:        http.MethodGet,
		}, sender)
		if sender.resp == nil || sender.resp.Status != http.StatusOK {
			t.Fatalf("expected 200 for /%s, got %+v", p, sender.resp)
		}
		body := string(sender.resp.Body)
		if strings.Contains(body, "super-secret-redis-pass-999") {
			t.Fatalf("redisPassword leaked in /%s response", p)
		}
		if p == "config/public" && strings.Contains(body, redisAddr) {
			t.Fatalf("redisAddr leaked in /config/public response: %s", body)
		}
	}

	// Replica 1 generates a job over HTTP CallResource.
	genSender := &captureSender{}
	_ = app1.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Body: []byte(`{
			"datasourceUid": "prom-1",
			"scope": {"labelName": "service", "labelValue": "checkout-api"},
			"userIntent": "Show request rate and error rate",
			"catalog": [
				{"name": "http_requests_total", "type": "counter", "labels": ["service", "status"]}
			]
		}`),
	}, genSender)
	if genSender.resp == nil || genSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 1 /jobs/generate failed: %+v", genSender.resp)
	}
	var genSnap jobs.JobSnapshot
	if err := json.Unmarshal(genSender.resp.Body, &genSnap); err != nil {
		t.Fatalf("unmarshal generate response: %v", err)
	}

	// Dispose Replica 1 (simulating pod restart/rollout).
	app1.Dispose()

	// Replica 2 serves GET, Patch, and Undo for Alice's job seamlessly.
	getSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/" + genSnap.JobID,
		Method:        http.MethodGet,
	}, getSender)
	if getSender.resp == nil || getSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 2 GET job failed after replica 1 dispose: %+v", getSender.resp)
	}

	// Bob attempting GET on Replica 2 MUST get 403 Forbidden.
	bobSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: bobCtx,
		Path:          "jobs/" + genSnap.JobID,
		Method:        http.MethodGet,
	}, bobSender)
	if bobSender.resp == nil || bobSender.resp.Status != http.StatusForbidden {
		t.Fatalf("expected 403 for bob on replica 2, got %+v", bobSender.resp)
	}

	// Patch on Replica 2 (rev 1 -> rev 2).
	patchSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/" + genSnap.JobID + "/patch",
		Method:        http.MethodPost,
		Body: []byte(`{
			"baseRevision": 1,
			"targetPanelId": "panel-error-rate",
			"editIntent": "Switch error rate to timeseries",
			"catalog": [
				{"name": "http_requests_total", "type": "counter", "labels": ["service", "status"]}
			]
		}`),
	}, patchSender)
	if patchSender.resp == nil || patchSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 2 patch failed: %+v (body=%s)", patchSender.resp, string(patchSender.resp.Body))
	}

	// Undo on Replica 2 (rev 2 -> rev 3).
	undoSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/" + genSnap.JobID + "/undo",
		Method:        http.MethodPost,
	}, undoSender)
	if undoSender.resp == nil || undoSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 2 undo failed: %+v", undoSender.resp)
	}

	// Stop Redis -> CheckHealth MUST report ERROR, non-admin /health MUST return 503 with generic message,
	// and /jobs/{id} MUST return 503 without leaking Redis addr/password.
	mr.Close()
	healthDown, _ := app2.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if healthDown.Status != backend.HealthStatusError {
		t.Fatalf("expected HealthStatusError when Redis is unreachable, got %+v", healthDown)
	}
	nonAdminHealth := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: bobCtx,
		Path:          "health",
		Method:        http.MethodGet,
	}, nonAdminHealth)
	if nonAdminHealth.resp == nil || nonAdminHealth.resp.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected non-admin /health to return 503 when Redis is down, got %+v", nonAdminHealth.resp)
	}
	if strings.Contains(string(nonAdminHealth.resp.Body), redisAddr) || strings.Contains(string(nonAdminHealth.resp.Body), "super-secret-redis-pass-999") {
		t.Fatalf("internal Redis details leaked in non-admin /health body: %s", string(nonAdminHealth.resp.Body))
	}

	downSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/" + genSnap.JobID,
		Method:        http.MethodGet,
	}, downSender)
	if downSender.resp == nil || downSender.resp.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when Redis is unreachable, got %+v", downSender.resp)
	}
	if strings.Contains(string(downSender.resp.Body), redisAddr) || strings.Contains(string(downSender.resp.Body), "super-secret-redis-pass-999") {
		t.Fatalf("internal Redis address or password leaked in 503 body: %s", string(downSender.resp.Body))
	}
}

func TestApp_AuthBeforeServiceUnavailableAndJobIDValidation(t *testing.T) {
	// Missing SA token in grafana-llm-app mode -> a.jobManager == nil.
	inst, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","allowedDatasourceUids":["prom-1"]}`),
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*plugin.App)
	defer app.Dispose()

	// Unauthenticated callers MUST receive 401 (not 503) even when jobManager == nil.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "jobs/generate"},
		{http.MethodPost, "jobs/import"},
		{http.MethodGet, "jobs/job-123"},
	} {
		sender := &captureSender{}
		_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
			Path:   tc.path,
			Method: tc.method,
			Body:   []byte(`{}`),
		}, sender)
		if sender.resp == nil || sender.resp.Status != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated %s /%s when jobManager==nil, got %+v", tc.method, tc.path, sender.resp)
		}
	}

	// Authenticated caller with invalid jobID syntax MUST get 404 before hitting JobStore.
	instMock, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"]}`),
	})
	if err != nil {
		t.Fatalf("NewApp mock: %v", err)
	}
	appMock := instMock.(*plugin.App)
	defer appMock.Dispose()

	authCtx := backend.PluginContext{OrgID: 1, User: &backend.User{Login: "alice", Role: "Editor"}}
	badIDSender := &captureSender{}
	_ = appMock.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: authCtx,
		Path:          "jobs/bad:id*",
		Method:        http.MethodGet,
	}, badIDSender)
	if badIDSender.resp == nil || badIDSender.resp.Status != http.StatusNotFound {
		t.Fatalf("expected 404 for invalid jobID path, got %+v", badIDSender.resp)
	}

	// IPv6 [::1]:6379 accepted; >128 char redisUsername rejected only for redis backend.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"[::1]:6379"}`),
	}); err != nil {
		t.Fatalf("expected IPv6 [::1]:6379 redisAddr to pass, got %v", err)
	}
	longUser := strings.Repeat("a", 129)
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"redis:6379","redisUsername":"` + longUser + `"}`),
	}); err == nil {
		t.Fatal("expected >128 char redisUsername to fail when jobStoreBackend=redis")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"memory","redisUsername":"` + longUser + `"}`),
	}); err != nil {
		t.Fatalf("expected stale redisUsername to be ignored when jobStoreBackend=memory, got %v", err)
	}
}

func TestApp_PostgresUnreachableSecretIsolation(t *testing.T) {
	const pgSecret = "super-secret-pg-pass-999"
	const pgAddr = "127.0.0.1:1"

	inst, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{
			"llmMode": "mock",
			"allowMockInDev": true,
			"allowedDatasourceUids": ["prom-1"],
			"jobStoreBackend": "postgres",
			"postgresAddr": "` + pgAddr + `",
			"postgresDatabase": "grafana",
			"postgresUsername": "grafana",
			"postgresSslMode": "disable"
		}`),
		DecryptedSecureJSONData: map[string]string{
			"postgresPassword":       pgSecret,
			"llmServiceAccountToken": "glsa_secret_token",
		},
	})
	if err != nil {
		t.Fatalf("NewApp postgres unreachable: %v", err)
	}
	app := inst.(*plugin.App)
	defer app.Dispose()

	aliceCtx := backend.PluginContext{OrgID: 1, User: &backend.User{Login: "alice", Role: "Admin"}}
	bobCtx := backend.PluginContext{OrgID: 1, User: &backend.User{Login: "bob", Role: "Editor"}}

	// /config/public MUST NEVER leak postgresPassword or postgresAddr.
	pubSender := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "config/public",
		Method:        http.MethodGet,
	}, pubSender)
	if pubSender.resp == nil || pubSender.resp.Status != http.StatusOK {
		t.Fatalf("expected 200 for /config/public, got %+v", pubSender.resp)
	}
	pubBody := string(pubSender.resp.Body)
	if strings.Contains(pubBody, pgSecret) || strings.Contains(pubBody, pgAddr) {
		t.Fatalf("postgres secret or addr leaked in /config/public: %s", pubBody)
	}

	// CheckHealth MUST return HealthStatusError without leaking postgresPassword.
	healthCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	health, err := app.CheckHealth(healthCtx, &backend.CheckHealthRequest{})
	if err != nil || health.Status != backend.HealthStatusError {
		t.Fatalf("expected HealthStatusError for unreachable Postgres, got err=%v health=%+v", err, health)
	}
	if strings.Contains(health.Message, pgSecret) {
		t.Fatalf("postgresPassword leaked in CheckHealth: %s", health.Message)
	}

	// Non-admin /health MUST return 503 without leaking internal address or password.
	nonAdminHealth := &captureSender{}
	_ = app.CallResource(healthCtx, &backend.CallResourceRequest{
		PluginContext: bobCtx,
		Path:          "health",
		Method:        http.MethodGet,
	}, nonAdminHealth)
	if nonAdminHealth.resp == nil || nonAdminHealth.resp.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on non-admin /health when Postgres is down, got %+v", nonAdminHealth.resp)
	}
	if strings.Contains(string(nonAdminHealth.resp.Body), pgAddr) || strings.Contains(string(nonAdminHealth.resp.Body), pgSecret) {
		t.Fatalf("internal Postgres details leaked in non-admin /health: %s", string(nonAdminHealth.resp.Body))
	}

	// /jobs/{id} MUST return 503 without leaking internal address or password.
	downSender := &captureSender{}
	_ = app.CallResource(healthCtx, &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/job-123",
		Method:        http.MethodGet,
	}, downSender)
	if downSender.resp == nil || downSender.resp.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when Postgres is unreachable, got %+v", downSender.resp)
	}
	if strings.Contains(string(downSender.resp.Body), pgAddr) || strings.Contains(string(downSender.resp.Body), pgSecret) {
		t.Fatalf("internal Postgres details leaked in 503 body: %s", string(downSender.resp.Body))
	}
}

func TestApp_MultiReplicaPostgresJobStoreHTTPAndSecretIsolation(t *testing.T) {
	pgAddr := strings.TrimSpace(os.Getenv("LLM_STUDIO_TEST_POSTGRES_ADDR"))
	if pgAddr == "" {
		if strings.TrimSpace(os.Getenv("LLM_STUDIO_REQUIRE_POSTGRES_TESTS")) == "1" {
			t.Fatal("LLM_STUDIO_REQUIRE_POSTGRES_TESTS=1 is set but LLM_STUDIO_TEST_POSTGRES_ADDR is empty")
		}
		t.Skip("set LLM_STUDIO_TEST_POSTGRES_ADDR to run live PostgreSQL plugin HTTP tests")
	}
	pgDB := strings.TrimSpace(os.Getenv("LLM_STUDIO_TEST_POSTGRES_DB"))
	if pgDB == "" {
		pgDB = "grafana"
	}
	pgUser := strings.TrimSpace(os.Getenv("LLM_STUDIO_TEST_POSTGRES_USER"))
	if pgUser == "" {
		pgUser = "grafana"
	}
	pgPass := os.Getenv("LLM_STUDIO_TEST_POSTGRES_PASSWORD")
	pgSSLMode := strings.TrimSpace(os.Getenv("LLM_STUDIO_TEST_POSTGRES_SSLMODE"))
	if pgSSLMode == "" {
		pgSSLMode = jobs.PostgresSSLModeDisable
	}

	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	schema := "llmstudio_http_" + hex.EncodeToString(rnd[:])
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		u := &url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(pgUser, pgPass),
			Host:   pgAddr,
			Path:   "/" + pgDB,
		}
		q := u.Query()
		q.Set("sslmode", pgSSLMode)
		u.RawQuery = q.Encode()
		if pool, err := pgxpool.New(ctx, u.String()); err == nil {
			defer pool.Close()
			_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	}()

	jsonData, err := json.Marshal(map[string]any{
		"llmMode":               "mock",
		"allowMockInDev":        true,
		"allowedDatasourceUids": []string{"prom-1"},
		"jobStoreBackend":       "postgres",
		"postgresAddr":          pgAddr,
		"postgresDatabase":      pgDB,
		"postgresUsername":      pgUser,
		"postgresSchema":        schema,
		"postgresTablePrefix":   "llm_studio_",
		"postgresSslMode":       pgSSLMode,
		"jobTtlMinutes":         45,
	})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}

	appSettings := backend.AppInstanceSettings{
		JSONData: jsonData,
		DecryptedSecureJSONData: map[string]string{
			"postgresPassword":       pgPass,
			"llmServiceAccountToken": "glsa_secret_token",
		},
	}

	inst1, err := plugin.NewApp(context.Background(), appSettings)
	if err != nil {
		t.Fatalf("NewApp replica 1: %v", err)
	}
	app1 := inst1.(*plugin.App)

	inst2, err := plugin.NewApp(context.Background(), appSettings)
	if err != nil {
		t.Fatalf("NewApp replica 2: %v", err)
	}
	app2 := inst2.(*plugin.App)
	defer app2.Dispose()

	// CheckHealth verifies live PostgreSQL connectivity and reports jobStore=postgres without leaking secrets.
	health, err := app1.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if err != nil || health.Status != backend.HealthStatusOk {
		t.Fatalf("expected CheckHealth OK with live Postgres, got err=%v health=%+v", err, health)
	}
	if !strings.Contains(health.Message, "jobStore=postgres") {
		t.Fatalf("expected jobStore=postgres in health message, got %q", health.Message)
	}
	if pgPass != "" && strings.Contains(health.Message, pgPass) {
		t.Fatal("postgresPassword leaked in CheckHealth message")
	}

	aliceCtx := backend.PluginContext{OrgID: 1, User: &backend.User{Login: "alice", Role: "Admin"}}
	bobCtx := backend.PluginContext{OrgID: 1, User: &backend.User{Login: "bob", Role: "Editor"}}

	// /config/public and /health MUST NEVER leak postgresPassword or postgresAddr.
	for _, p := range []string{"config/public", "health"} {
		sender := &captureSender{}
		_ = app1.CallResource(context.Background(), &backend.CallResourceRequest{
			PluginContext: aliceCtx,
			Path:          p,
			Method:        http.MethodGet,
		}, sender)
		if sender.resp == nil || sender.resp.Status != http.StatusOK {
			t.Fatalf("expected 200 for /%s, got %+v", p, sender.resp)
		}
		body := string(sender.resp.Body)
		if pgPass != "" && strings.Contains(body, pgPass) {
			t.Fatalf("postgresPassword leaked in /%s response", p)
		}
		if p == "config/public" && strings.Contains(body, pgAddr) {
			t.Fatalf("postgresAddr leaked in /config/public response: %s", body)
		}
	}

	// Replica 1 generates a job over HTTP CallResource.
	genSender := &captureSender{}
	_ = app1.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Body: []byte(`{
			"datasourceUid": "prom-1",
			"scope": {"labelName": "service", "labelValue": "checkout-api"},
			"userIntent": "Show request rate and error rate",
			"catalog": [
				{"name": "http_requests_total", "type": "counter", "labels": ["service", "status"]}
			]
		}`),
	}, genSender)
	if genSender.resp == nil || genSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 1 /jobs/generate failed: %+v", genSender.resp)
	}
	var genSnap jobs.JobSnapshot
	if err := json.Unmarshal(genSender.resp.Body, &genSnap); err != nil {
		t.Fatalf("unmarshal generate response: %v", err)
	}

	// Dispose Replica 1 (simulating pod restart/rollout).
	app1.Dispose()

	// Replica 2 serves GET, Patch, and Undo for Alice's job seamlessly.
	getSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/" + genSnap.JobID,
		Method:        http.MethodGet,
	}, getSender)
	if getSender.resp == nil || getSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 2 GET job failed after replica 1 dispose: %+v", getSender.resp)
	}

	// Bob attempting GET on Replica 2 MUST get 403 Forbidden.
	bobSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: bobCtx,
		Path:          "jobs/" + genSnap.JobID,
		Method:        http.MethodGet,
	}, bobSender)
	if bobSender.resp == nil || bobSender.resp.Status != http.StatusForbidden {
		t.Fatalf("expected 403 for bob on replica 2, got %+v", bobSender.resp)
	}

	// Patch on Replica 2 (rev 1 -> rev 2).
	patchSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/" + genSnap.JobID + "/patch",
		Method:        http.MethodPost,
		Body: []byte(`{
			"baseRevision": 1,
			"targetPanelId": "panel-error-rate",
			"editIntent": "Switch error rate to timeseries",
			"catalog": [
				{"name": "http_requests_total", "type": "counter", "labels": ["service", "status"]}
			]
		}`),
	}, patchSender)
	if patchSender.resp == nil || patchSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 2 patch failed: %+v (body=%s)", patchSender.resp, string(patchSender.resp.Body))
	}

	// Undo on Replica 2 (rev 2 -> rev 3).
	undoSender := &captureSender{}
	_ = app2.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/" + genSnap.JobID + "/undo",
		Method:        http.MethodPost,
	}, undoSender)
	if undoSender.resp == nil || undoSender.resp.Status != http.StatusOK {
		t.Fatalf("replica 2 undo failed: %+v", undoSender.resp)
	}
}
