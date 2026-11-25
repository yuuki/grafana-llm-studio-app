package plugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
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

	// Destination-bound redisPassword succeeds for matching endpoint+TLS and rejects redirected addr or TLS downgrade.
	boundPass, err := plugin.EncodeBoundRedisPassword("redis.internal:6379", true, "secret-redis-pw")
	if err != nil {
		t.Fatalf("EncodeBoundRedisPassword: %v", err)
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"REDIS.INTERNAL:6379","redisTls":true}`),
		DecryptedSecureJSONData: map[string]string{"redisPassword": boundPass},
	}); err != nil {
		t.Fatalf("expected bound redisPassword to match canonical endpoint+TLS, got %v", err)
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"attacker.invalid:6379","redisTls":true}`),
		DecryptedSecureJSONData: map[string]string{"redisPassword": boundPass},
	}); err == nil {
		t.Fatal("expected bound redisPassword to be rejected when redisAddr changed to another server")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"redis.internal:6379","redisTls":false}`),
		DecryptedSecureJSONData: map[string]string{"redisPassword": boundPass},
	}); err == nil {
		t.Fatal("expected bound redisPassword to be rejected when redisTls was downgraded")
	}
	// Switching back to memory ignores bound redisPassword without error.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"memory"}`),
		DecryptedSecureJSONData: map[string]string{"redisPassword": boundPass},
	}); err != nil {
		t.Fatalf("expected memory mode to ignore bound redisPassword, got %v", err)
	}

	// Operator-pinned Redis address rejects mismatched jsonData.redisAddr and fills empty redisAddr.
	sPinned, err := plugin.LoadSettingsWithOptions(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis"}`),
	}, plugin.LoadOptions{PinnedRedisAddr: "redis.pinned:6379"})
	if err != nil || sPinned.RedisAddr != "redis.pinned:6379" {
		t.Fatalf("expected operator-pinned redisAddr fallback, got err=%v addr=%q", err, sPinned.RedisAddr)
	}
	if _, err := plugin.LoadSettingsWithOptions(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"jobStoreBackend":"redis","redisAddr":"attacker.invalid:6379"}`),
	}, plugin.LoadOptions{PinnedRedisAddr: "redis.pinned:6379"}); err == nil {
		t.Fatal("expected mismatched redisAddr to be rejected against operator-pinned LLMSTUDIO_REDIS_ADDR")
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

func TestApp_BoundRedisPasswordEndToEnd(t *testing.T) {
	mr1 := miniredis.RunT(t)
	mr1.RequireAuth("bound-live-secret")
	mr2 := miniredis.RunT(t)

	boundPass, err := plugin.EncodeBoundRedisPassword(mr1.Addr(), false, "bound-live-secret")
	if err != nil {
		t.Fatalf("EncodeBoundRedisPassword: %v", err)
	}

	// Matching redisAddr unwraps the password and authenticates cleanly to mr1.
	inst, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{
			"llmMode": "mock",
			"allowMockInDev": true,
			"allowedDatasourceUids": ["prom-1"],
			"jobStoreBackend": "redis",
			"redisAddr": "` + mr1.Addr() + `",
			"redisTls": false
		}`),
		DecryptedSecureJSONData: map[string]string{
			"redisPassword": boundPass,
		},
	})
	if err != nil {
		t.Fatalf("NewApp with matching bound redisPassword: %v", err)
	}
	app := inst.(*plugin.App)
	defer app.Dispose()

	health, err := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if err != nil || health.Status != backend.HealthStatusOk {
		t.Fatalf("expected CheckHealth OK with bound redisPassword, got err=%v health=%+v", err, health)
	}

	// Redirecting redisAddr to mr2 while keeping mr1's bound redisPassword MUST fail in NewApp before dialing mr2.
	if _, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{
			"llmMode": "mock",
			"allowMockInDev": true,
			"allowedDatasourceUids": ["prom-1"],
			"jobStoreBackend": "redis",
			"redisAddr": "` + mr2.Addr() + `",
			"redisTls": false
		}`),
		DecryptedSecureJSONData: map[string]string{
			"redisPassword": boundPass,
		},
	}); err == nil {
		t.Fatal("expected NewApp to reject redirected redisAddr with bound redisPassword from mr1")
	}
}

