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

func TestLoadSettings_NoSilentMockFallback(t *testing.T) {
	// llmMode=mock without allowMockInDev MUST fail.
	_, err := plugin.LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowedDatasourceUids":["prom-1"]}`),
	})
	if err == nil {
		t.Fatal("expected llmMode=mock without allowMockInDev=true to fail")
	}

	// Default production mode (grafana-llm-app) without token initializes with error, NEVER falls back to mock.
	inst, err := plugin.NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","allowedDatasourceUids":["prom-1"]}`),
	})
	if err != nil {
		t.Fatalf("NewApp returned unexpected error: %v", err)
	}
	app := inst.(*plugin.App)
	health, _ := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if health.Status != backend.HealthStatusError {
		t.Fatalf("expected HealthStatusError when grafana-llm-app token missing, got %v (%s)", health.Status, health.Message)
	}
}

func TestApp_AuthContextAndNoSecretLeak(t *testing.T) {
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

	// Unauthenticated request (no PluginContext.User) must get 401.
	sender := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		Path:   "config/public",
		Method: http.MethodGet,
	}, sender)
	if sender.resp == nil || sender.resp.Status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated caller, got %+v", sender.resp)
	}

	// Authenticated request returns public config WITHOUT secret token.
	authPluginCtx := backend.PluginContext{
		OrgID: 1,
		User:  &backend.User{Login: "alice", Role: "Editor"},
	}
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

	// Client-supplied spoofed orgId field in JSON body is rejected by DisallowUnknownFields.
	sender3 := &captureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: authPluginCtx,
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Body:          []byte(`{"orgId":999,"datasourceUid":"prom-1","userIntent":"hello","catalog":[]}`),
	}, sender3)
	if sender3.resp == nil || sender3.resp.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 when client injects spoofed orgId field, got %+v", sender3.resp)
	}

	var cfg plugin.PublicConfig
	if err := json.Unmarshal(sender2.resp.Body, &cfg); err != nil {
		t.Fatalf("unmarshal public config: %v", err)
	}
	if !cfg.MockBanner || len(cfg.AllowedDatasourceUIDs) != 1 {
		t.Fatalf("unexpected public config: %+v", cfg)
	}
}
