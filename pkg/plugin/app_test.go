package plugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/yuuki/grafana-llm-studio-app/pkg/plugin"
)

type captureSender struct {
	resp *backend.CallResourceResponse
}

func (c *captureSender) Send(resp *backend.CallResourceResponse) error {
	c.resp = resp
	return nil
}

func TestLoadSettings_SecurityAndDefaults(t *testing.T) {
	// llmMode=mock without allowMockInDev MUST fail.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowedDatasourceUids":["prom-1"]}`),
	}); err == nil {
		t.Fatal("expected llmMode=mock without allowMockInDev=true to fail")
	}

	// C-1: Arbitrary non-loopback grafanaUrl MUST be rejected (SSRF protection).
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","grafanaUrl":"http://attacker.invalid:8080","allowedDatasourceUids":["prom-1"]}`),
	}, "https://grafana.internal"); err == nil {
		t.Fatal("expected untrusted external grafanaUrl to be rejected")
	}

	// Trusted AppURL pins grafanaUrl strictly to that base URL — arbitrary loopback ports and mismatched paths MUST be rejected.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","grafanaUrl":"http://127.0.0.1:9999","allowedDatasourceUids":["prom-1"]}`),
	}, "https://grafana.internal/subpath"); err == nil {
		t.Fatal("expected arbitrary loopback grafanaUrl to be rejected when trusted AppURL is configured")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","grafanaUrl":"https://grafana.internal","allowedDatasourceUids":["prom-1"]}`),
	}, "https://grafana.internal/subpath"); err == nil {
		t.Fatal("expected mismatched base path on trusted AppURL origin to be rejected")
	}

	// Exact matching trusted AppURL base URL (including default port / trailing slash normalization) is allowed.
	s, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","grafanaUrl":"https://grafana.internal:443/subpath/","maxRepairAttempts":0,"allowedDatasourceUids":["prom-1"]}`),
	}, "https://grafana.internal/subpath")
	if err != nil {
		t.Fatalf("expected matching trusted AppURL base URL to pass, got %v", err)
	}
	// Bound Grafana token succeeds for matching URL and is rejected if grafanaUrl changes (even loopback-to-loopback without trusted AppURL).
	boundToken, err := plugin.EncodeBoundGrafanaToken("http://localhost:3000", "glsa_bound_secret")
	if err != nil {
		t.Fatalf("EncodeBoundGrafanaToken: %v", err)
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"grafana-llm-app","grafanaUrl":"http://localhost:3000/","allowedDatasourceUids":["prom-1"]}`),
		DecryptedSecureJSONData: map[string]string{"llmServiceAccountToken": boundToken},
	}); err != nil {
		t.Fatalf("expected bound grafana token to pass for matching URL, got %v", err)
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"grafana-llm-app","grafanaUrl":"http://127.0.0.1:9999","allowedDatasourceUids":["prom-1"]}`),
		DecryptedSecureJSONData: map[string]string{"llmServiceAccountToken": boundToken},
	}); err == nil {
		t.Fatal("expected bound grafana token to be rejected when grafanaUrl changed to a different loopback port")
	}
	defaultBoundToken, err := plugin.EncodeBoundGrafanaToken("", "glsa_default_bound")
	if err != nil {
		t.Fatalf("EncodeBoundGrafanaToken empty url: %v", err)
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"grafana-llm-app","allowedDatasourceUids":["prom-1"]}`),
		DecryptedSecureJSONData: map[string]string{"llmServiceAccountToken": defaultBoundToken},
	}, "https://grafana.internal/subpath"); err != nil {
		t.Fatalf("expected default-bound token to pass with empty grafanaUrl and trusted AppURL, got %v", err)
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData:                []byte(`{"llmMode":"grafana-llm-app","grafanaUrl":"http://127.0.0.1:9999","allowedDatasourceUids":["prom-1"]}`),
		DecryptedSecureJSONData: map[string]string{"llmServiceAccountToken": defaultBoundToken},
	}); err == nil {
		t.Fatal("expected default-bound token to be rejected when grafanaUrl changed to custom loopback port")
	}
	// Default reasoningEffort is "" (compatible with standard OpenAI/OpenAI-compat models),
	// structuredOutputMode defaults to "json_schema", tokenLimitField defaults to "max_completion_tokens",
	// and explicit maxRepairAttempts=0 is honored.
	if s.EffectiveReasoningEffort != "" {
		t.Fatalf("expected default reasoningEffort=\"\", got %q", s.EffectiveReasoningEffort)
	}
	if s.StructuredOutputMode != "json_schema" {
		t.Fatalf("expected default structuredOutputMode=json_schema, got %q", s.StructuredOutputMode)
	}
	if s.TokenLimitField != "max_completion_tokens" {
		t.Fatalf("expected default tokenLimitField=max_completion_tokens, got %q", s.TokenLimitField)
	}
	if s.EffectiveMaxRepairs != 0 {
		t.Fatalf("expected explicit maxRepairAttempts=0 to be preserved, got %d", s.EffectiveMaxRepairs)
	}

	// Explicit json_object + max_tokens + reasoningEffort=low are accepted.
	s2, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","structuredOutputMode":"json_object","tokenLimitField":"max_tokens","reasoningEffort":"low","allowedDatasourceUids":["prom-1"]}`),
	})
	if err != nil {
		t.Fatalf("expected valid OpenAI-compatible overrides to pass, got %v", err)
	}
	if s2.StructuredOutputMode != "json_object" || s2.TokenLimitField != "max_tokens" || s2.EffectiveReasoningEffort != "low" {
		t.Fatalf("unexpected parsed overrides: %+v", s2)
	}

	// Invalid structuredOutputMode or tokenLimitField MUST be rejected.
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","structuredOutputMode":"invalid","allowedDatasourceUids":["prom-1"]}`),
	}); err == nil {
		t.Fatal("expected invalid structuredOutputMode to fail")
	}
	if _, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","tokenLimitField":"bad_field","allowedDatasourceUids":["prom-1"]}`),
	}); err == nil {
		t.Fatal("expected invalid tokenLimitField to fail")
	}

	// Production mode without token initializes with error, NEVER falls back to mock.
	inst, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","allowedDatasourceUids":["prom-1"]}`),
	})
	if err != nil {
		t.Fatalf("NewApp returned unexpected error: %v", err)
	}
	app := inst.(*plugin.App)
	defer app.Dispose()
	health, _ := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if health.Status != backend.HealthStatusError {
		t.Fatalf("expected HealthStatusError when grafana-llm-app token missing, got %v (%s)", health.Status, health.Message)
	}
}

func TestApp_AuthContextHealthAndStrictRouting(t *testing.T) {
	inst, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"]}`),
		DecryptedSecureJSONData: map[string]string{
			"llmServiceAccountToken": "glsa_super_secret_should_never_leak",
		},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*plugin.App)
	defer app.Dispose()

	// Unauthenticated /config/public and /health MUST return 401 (M-1).
	for _, p := range []string{"config/public", "health"} {
		sender := &captureSender{}
		_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
			Path:   p,
			Method: http.MethodGet,
		}, sender)
		if sender.resp == nil || sender.resp.Status != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated /%s, got %+v", p, sender.resp)
		}
	}

	authPluginCtx := backend.PluginContext{
		OrgID: 1,
		User:  &backend.User{Login: "alice", Role: "Editor"},
	}

	// Authenticated /config/public returns public config WITHOUT secret token.
	sender2 := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: authPluginCtx,
		Path:          "config/public",
		Method:        http.MethodGet,
	}, sender2)
	if sender2.resp == nil || sender2.resp.Status != http.StatusOK {
		t.Fatalf("expected 200 for authenticated caller, got %+v", sender2.resp)
	}
	if strings.Contains(string(sender2.resp.Body), "glsa_super_secret") {
		t.Fatal("secret llmServiceAccountToken leaked in /config/public")
	}

	// Trailing JSON tokens rejected (L-1).
	senderTrail := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: authPluginCtx,
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Body:          []byte(`{"datasourceUid":"prom-1","userIntent":"hello","catalog":[]}{"extra":1}`),
	}, senderTrail)
	if senderTrail.resp == nil || senderTrail.resp.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 on trailing JSON tokens, got %+v", senderTrail.resp)
	}

	// Extra path segments rejected (L-1).
	senderExtraPath := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: authPluginCtx,
		Path:          "jobs/job-123/cancel/extra",
		Method:        http.MethodPost,
	}, senderExtraPath)
	if senderExtraPath.resp == nil || senderExtraPath.resp.Status != http.StatusNotFound {
		t.Fatalf("expected 404 on extra job path segments, got %+v", senderExtraPath.resp)
	}

	var cfg plugin.PublicConfig
	if err := json.Unmarshal(sender2.resp.Body, &cfg); err != nil {
		t.Fatalf("unmarshal public config: %v", err)
	}
	if !cfg.MockBanner || len(cfg.AllowedDatasourceUIDs) != 1 {
		t.Fatalf("unexpected public config: %+v", cfg)
	}

	// POST /jobs/import requires authentication and accepts a valid constrained v1.1 draft.
	senderUnauthImport := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		Path:   "jobs/import",
		Method: http.MethodPost,
		Body:   []byte(`{}`),
	}, senderUnauthImport)
	if senderUnauthImport.resp == nil || senderUnauthImport.resp.Status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /jobs/import, got %+v", senderUnauthImport.resp)
	}

	senderImport := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: authPluginCtx,
		Path:          "jobs/import",
		Method:        http.MethodPost,
		Body: []byte(`{
			"datasourceUid": "prom-1",
			"importSource": {"uid": "existing-uid", "resourceVersion": "9"},
			"draft": {
				"schemaVersion": "1.1",
				"revision": 1,
				"title": "Imported Overview",
				"timeRange": {"from": "now-15m", "to": "now"},
				"panels": [{
					"id": "panel-up",
					"type": "gauge",
					"title": "Availability",
					"datasourceRef": "selected_prometheus",
					"query": {"expr": "avg(up{service=\"checkout-api\"})"},
					"unit": "percentunit",
					"layout": {"row": 0, "col": 0, "width": "full"}
				}]
			}
		}`),
	}, senderImport)
	if senderImport.resp == nil || senderImport.resp.Status != http.StatusOK {
		t.Fatalf("expected 200 for valid /jobs/import, got %+v (body=%s)", senderImport.resp, string(senderImport.resp.Body))
	}
	if !strings.Contains(string(senderImport.resp.Body), `"resourceVersion":"9"`) {
		t.Fatalf("expected importSource.resourceVersion in /jobs/import response, got %s", string(senderImport.resp.Body))
	}

	// H-2: /jobs/import must strictly decode draft (reject unknown fields & missing required fields like layout.col / query+queries).
	senderStrictImport := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: authPluginCtx,
		Path:          "jobs/import",
		Method:        http.MethodPost,
		Body: []byte(`{
			"datasourceUid": "prom-1",
			"draft": {
				"schemaVersion": "1.1",
				"revision": 1,
				"title": "Bad Import",
				"timeRange": {"from": "now-15m", "to": "now"},
				"panels": [{
					"id": "panel-up",
					"type": "gauge",
					"title": "Availability",
					"datasourceRef": "selected_prometheus",
					"query": {"expr": "up"},
					"queries": [],
					"unit": "short",
					"layout": {"row": 0, "col": 0, "width": "full"}
				}]
			}
		}`),
	}, senderStrictImport)
	if senderStrictImport.resp == nil || senderStrictImport.resp.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 on /jobs/import with query + empty queries: [], got %+v", senderStrictImport.resp)
	}
}
