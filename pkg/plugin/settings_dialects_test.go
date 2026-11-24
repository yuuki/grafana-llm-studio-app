package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

func TestLoadSettings_DatasourceDialectsValidationAndPublicFiltering(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"allowedDatasourceUids": []string{"prom-main", "vm-prom"},
		"datasourceDialects": map[string]string{
			"vm-prom":   "MetricsQL",
			"stale-uid": "metricsql",
		},
		"llmMode":        "mock",
		"allowMockInDev": true,
	})
	s, err := LoadSettings(backend.AppInstanceSettings{JSONData: raw})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.DatasourceDialects["vm-prom"] != "metricsql" {
		t.Fatalf("expected normalized metricsql dialect, got %+v", s.DatasourceDialects)
	}
	pub := s.Public()
	if pub.DatasourceDialects["vm-prom"] != "metricsql" {
		t.Fatalf("expected vm-prom dialect in PublicConfig, got %+v", pub.DatasourceDialects)
	}
	if _, leaked := pub.DatasourceDialects["stale-uid"]; leaked {
		t.Fatalf("expected stale UID outside allowlist to be filtered from PublicConfig")
	}

	// Invalid dialect value must fail LoadSettings.
	badRaw, _ := json.Marshal(map[string]any{
		"allowedDatasourceUids": []string{"vm-prom"},
		"datasourceDialects":    map[string]string{"vm-prom": "logql"},
		"llmMode":               "mock",
		"allowMockInDev":        true,
	})
	if _, err := LoadSettings(backend.AppInstanceSettings{JSONData: badRaw}); err == nil {
		t.Fatalf("expected unsupported dialect to be rejected by LoadSettings")
	}
}

func TestHandleImport_AcceptsQueryKindWithStrictJSON(t *testing.T) {
	settings := Settings{
		JSONSettings: JSONSettings{
			AllowedDatasourceUIDs: []string{"vm-ds"},
			LLMMode:               LLMModeMock,
			AllowMockInDev:        true,
			MaxPanels:             6,
			JobDeadlineSeconds:    30,
			MaxConcurrentJobs:     2,
		},
		EffectiveMaxRepairs: 2,
	}
	app := newAppWithClient(settings, &llm.MockClient{}, nil)
	defer app.Dispose()

	body, _ := json.Marshal(map[string]any{
		"datasourceUid": "vm-ds",
		"queryKind":     "metricsql",
		"scope":         map[string]string{"labelName": "service", "labelValue": "checkout"},
		"draft": map[string]any{
			"schemaVersion": "1.1",
			"revision":      1,
			"title":         "VM Dashboard",
			"timeRange":     map[string]string{"from": "now-15m", "to": "now"},
			"panels": []map[string]any{
				{
					"id":            "panel-vm",
					"type":          "timeseries",
					"title":         "VM Rate",
					"datasourceRef": ir.SelectedDatasourceRef,
					"query": map[string]any{
						"kind": "metricsql",
						"expr": `sum(increase_pure(http_requests_total{service="checkout"}[5m])) default 0`,
					},
					"unit":   "reqps",
					"layout": map[string]any{"row": 0, "col": 0, "width": "full"},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/jobs/import", bytes.NewReader(body))
	ctx := backend.WithPluginContext(context.Background(), backend.PluginContext{
		OrgID: 1,
		User:  &backend.User{Login: "tester", Role: "Editor"},
	})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	app.handleImport(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 on MetricsQL /jobs/import, got %d: %s", rr.Code, rr.Body.String())
	}
	var snap jobs.JobSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if snap.QueryKind != "metricsql" {
		t.Fatalf("expected JobSnapshot.QueryKind=metricsql, got %q", snap.QueryKind)
	}
}
