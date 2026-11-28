package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

func sampleAnalyzeHTTPBody() []byte {
	payload := ir.AnalyzeStatusPayload{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		Window:        "1h",
		BaselineMode:  "preceding",
		Language:      "en",
		CollectionStatus: ir.ReportCollectionStatus{
			AlertsChecked:       true,
			RulesChecked:        true,
			TotalRulesEvaluated: 8,
			ActiveAlertsTotal:   1,
		},
		Evidence: ir.ReportEvidenceBundle{
			CollectedAt: "2026-11-27T13:10:00Z",
			Alerts: []ir.AlertEvidenceItem{
				{
					ID:           "ev-alert-1",
					AlertName:    "HighErrorRate",
					State:        "active",
					Severity:     "critical",
					StartsAt:     "2026-11-27T13:05:00Z",
					Summary:      "5xx rate > 5%",
					MatchesScope: true,
				},
			},
			Rules:   []ir.RuleEvidenceItem{},
			Metrics: []ir.MetricEvidenceItem{},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

type reportSSESender struct {
	resps []*backend.CallResourceResponse
}

func (s *reportSSESender) Send(resp *backend.CallResourceResponse) error {
	s.resps = append(s.resps, resp)
	return nil
}

func (s *reportSSESender) combinedBody() string {
	var b strings.Builder
	for _, r := range s.resps {
		b.Write(r.Body)
	}
	return b.String()
}

func callReportResource(t *testing.T, app *App, pCtx backend.PluginContext, method, path string, body []byte) *backend.CallResourceResponse {
	t.Helper()
	sender := &reportSSESender{}
	err := app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: pCtx,
		Path:          path,
		Method:        method,
		Body:          body,
	}, sender)
	if err != nil {
		t.Fatalf("CallResource %s %s failed: %v", method, path, err)
	}
	if len(sender.resps) == 0 {
		t.Fatalf("CallResource %s %s sent no responses", method, path)
	}
	return sender.resps[0]
}

func TestApp_AnalyzeStatus_HTTP_SSE_And_ErrorMapping(t *testing.T) {
	settings := Settings{
		JSONSettings: JSONSettings{
			AllowedDatasourceUIDs: []string{"prom-main"},
			LLMMode:               LLMModeMock,
			AllowMockInDev:        true,
			MaxPanels:             6,
			MaxConcurrentJobs:     4,
			JobDeadlineSeconds:    30,
			EnableSSEProgress:     true,
		},
		EffectiveMaxRepairs: 2,
	}
	app := newAppWithClient(settings, &llm.MockClient{}, nil)
	defer app.Dispose()

	aliceCtx := backend.PluginContext{
		OrgID: 1,
		User:  &backend.User{Login: "alice", Role: "Viewer"},
	}
	bobCtx := backend.PluginContext{
		OrgID: 1,
		User:  &backend.User{Login: "bob", Role: "Viewer"},
	}

	// 1. Standard JSON POST /jobs/analyze
	resp := callReportResource(t, app, aliceCtx, http.MethodPost, "jobs/analyze", sampleAnalyzeHTTPBody())
	if resp.Status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d body=%s", resp.Status, string(resp.Body))
	}
	var snap jobs.JobSnapshot
	if err := json.Unmarshal(resp.Body, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.JobKind != jobs.JobKindReport || snap.Status != jobs.StatusReady || snap.StatusReport == nil {
		t.Fatalf("unexpected report snapshot: %#v", snap)
	}

	// 2. Owner GET succeeds; cross-user GET returns 403
	getResp := callReportResource(t, app, aliceCtx, http.MethodGet, "jobs/"+snap.JobID, nil)
	if getResp.Status != http.StatusOK {
		t.Fatalf("expected 200 on owner GET, got %d", getResp.Status)
	}
	forbiddenResp := callReportResource(t, app, bobCtx, http.MethodGet, "jobs/"+snap.JobID, nil)
	if forbiddenResp.Status != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-user GET, got %d", forbiddenResp.Status)
	}

	// 3. Patch, Repair, Undo on report job return 409 Conflict
	patchResp := callReportResource(t, app, aliceCtx, http.MethodPost, "jobs/"+snap.JobID+"/patch",
		[]byte(`{"baseRevision":1,"editIntent":"add panel","catalog":[{"name":"http_requests_total","type":"counter","labels":["service"]}]}`))
	if patchResp.Status != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on patch of report job, got %d body=%s", patchResp.Status, string(patchResp.Body))
	}
	undoResp := callReportResource(t, app, aliceCtx, http.MethodPost, "jobs/"+snap.JobID+"/undo", nil)
	if undoResp.Status != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on undo of report job, got %d body=%s", undoResp.Status, string(undoResp.Body))
	}

	// 4. Late cancel on completed report preserves status=ready
	cancelResp := callReportResource(t, app, aliceCtx, http.MethodPost, "jobs/"+snap.JobID+"/cancel", nil)
	if cancelResp.Status != http.StatusOK {
		t.Fatalf("expected 200 on cancel, got %d", cancelResp.Status)
	}
	var afterCancel jobs.JobSnapshot
	_ = json.Unmarshal(cancelResp.Body, &afterCancel)
	if afterCancel.Status != jobs.StatusReady {
		t.Fatalf("expected status=ready preserved after late cancel, got %q", afterCancel.Status)
	}

	// 5. Unknown field rejected with 400 (DisallowUnknownFields)
	badResp := callReportResource(t, app, aliceCtx, http.MethodPost, "jobs/analyze",
		[]byte(`{"orgId":999,"window":"1h","baselineMode":"preceding","language":"en"}`))
	if badResp.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown orgId field, got %d", badResp.Status)
	}

	// 6. Insufficient evidence returns 422
	emptyBody := []byte(`{"window":"1h","baselineMode":"preceding","language":"en","collectionStatus":{"alertsChecked":false,"rulesChecked":false},"evidence":{"collectedAt":"2026-11-27T13:10:00Z","alerts":[],"rules":[],"metrics":[]}}`)
	emptyResp := callReportResource(t, app, aliceCtx, http.MethodPost, "jobs/analyze", emptyBody)
	if emptyResp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for insufficient evidence, got %d body=%s", emptyResp.Status, string(emptyResp.Body))
	}

	// 7. SSE streaming emits analyzing -> validating_report -> ready -> result
	sender := &reportSSESender{}
	err := app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: aliceCtx,
		Path:          "jobs/analyze",
		Method:        http.MethodPost,
		Headers:       map[string][]string{"Accept": {"text/event-stream, application/json"}},
		Body:          sampleAnalyzeHTTPBody(),
	}, sender)
	if err != nil {
		t.Fatalf("SSE CallResource failed: %v", err)
	}
	frames := sender.combinedBody()
	if !strings.Contains(frames, `"operation":"analyze"`) ||
		!strings.Contains(frames, `"stage":"analyzing"`) ||
		!strings.Contains(frames, `"stage":"validating_report"`) ||
		!strings.Contains(frames, "event: result") {
		t.Fatalf("unexpected SSE frames: %s", frames)
	}

	// 8. Migration error hides DDL details from Viewer while exposing hint to Admin
	migErr := fmt.Errorf("%w: run ALTER TABLE public.llm_studio_jobs ADD COLUMN IF NOT EXISTS job_kind TEXT", jobs.ErrReportStorageMigrationRequired)
	viewerCtx := backend.WithPluginContext(context.Background(), aliceCtx)
	vStatus, vPayload := buildJobErrorPayloadForContext(viewerCtx, migErr, jobs.JobSnapshot{})
	if vStatus != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for migration required, got %d", vStatus)
	}
	if strings.Contains(vPayload["error"].(string), "ALTER TABLE") {
		t.Fatalf("expected DDL hint hidden from Viewer, got %q", vPayload["error"])
	}

	adminCtx := backend.WithPluginContext(context.Background(), backend.PluginContext{
		OrgID: 1,
		User:  &backend.User{Login: "admin", Role: "Admin"},
	})
	aStatus, aPayload := buildJobErrorPayloadForContext(adminCtx, migErr, jobs.JobSnapshot{})
	if aStatus != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for Admin migration error, got %d", aStatus)
	}
	if !strings.Contains(aPayload["error"].(string), "ALTER TABLE") {
		t.Fatalf("expected DDL hint visible to Admin, got %q", aPayload["error"])
	}
}
