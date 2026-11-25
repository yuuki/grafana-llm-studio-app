package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

type streamCaptureSender struct {
	mu      sync.Mutex
	chunks  []*backend.CallResourceResponse
	onChunk func(idx int, resp *backend.CallResourceResponse)
}

func (s *streamCaptureSender) Send(resp *backend.CallResourceResponse) error {
	cp := &backend.CallResourceResponse{
		Status:  resp.Status,
		Headers: resp.Headers,
		Body:    append([]byte(nil), resp.Body...),
	}
	s.mu.Lock()
	idx := len(s.chunks)
	s.chunks = append(s.chunks, cp)
	cb := s.onChunk
	s.mu.Unlock()
	if cb != nil {
		cb(idx, cp)
	}
	return nil
}

func (s *streamCaptureSender) snapshot() []*backend.CallResourceResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*backend.CallResourceResponse, len(s.chunks))
	copy(out, s.chunks)
	return out
}

func (s *streamCaptureSender) combinedBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sb strings.Builder
	for _, c := range s.chunks {
		sb.Write(c.Body)
	}
	return sb.String()
}

type parsedSSEFrame struct {
	Event string
	Data  string
}

func parseSSEFrames(raw string) []parsedSSEFrame {
	blocks := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n")
	var out []parsedSSEFrame
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				if data != "" {
					data += "\n"
				}
				data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if ev != "" || data != "" {
			out = append(out, parsedSSEFrame{Event: ev, Data: data})
		}
	}
	return out
}

type gatedLLMClient struct {
	delegate     llm.Client
	entered      chan struct{}
	enteredOnce  sync.Once
	release      chan struct{}
	canceled     chan struct{}
	canceledOnce sync.Once
	failErr      error
	patchFailErr error
}

func (g *gatedLLMClient) Enabled(ctx context.Context) (bool, error) {
	return true, nil
}

func (g *gatedLLMClient) GenerateIR(ctx context.Context, req llm.GenerateRequest) ([]byte, llm.CompletionMeta, error) {
	if g.entered != nil {
		g.enteredOnce.Do(func() { close(g.entered) })
	}
	select {
	case <-ctx.Done():
		if g.canceled != nil {
			g.canceledOnce.Do(func() { close(g.canceled) })
		}
		return nil, llm.CompletionMeta{ProviderMode: "mock"}, ctx.Err()
	case <-g.release:
	}
	if g.failErr != nil {
		return nil, llm.CompletionMeta{ProviderMode: "mock"}, g.failErr
	}
	return g.delegate.GenerateIR(ctx, req)
}

func (g *gatedLLMClient) GeneratePatch(ctx context.Context, req llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
	if g.patchFailErr != nil {
		return nil, llm.CompletionMeta{ProviderMode: "mock"}, g.patchFailErr
	}
	return g.delegate.GeneratePatch(ctx, req)
}

func (g *gatedLLMClient) RepairPanels(ctx context.Context, req llm.RepairRequest) ([]byte, llm.CompletionMeta, error) {
	return g.delegate.RepairPanels(ctx, req)
}

func testAuthCtx() backend.PluginContext {
	return backend.PluginContext{
		OrgID: 1,
		User:  &backend.User{Login: "alice", Role: "Editor"},
	}
}

func sampleGenerateBody() []byte {
	return []byte(`{
		"datasourceUid": "prom-1",
		"scope": {"labelName": "service", "labelValue": "checkout-api"},
		"userIntent": "Show request rate, 5xx error rate, and p95 latency",
		"catalog": [
			{"name": "http_requests_total", "type": "counter", "labels": ["service", "status"]},
			{"name": "http_request_duration_seconds_bucket", "type": "histogram", "labels": ["service", "le"]}
		]
	}`)
}

func TestWantsEventStream_AcceptParsing(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"", false},
		{"application/json", false},
		{"*/*", false},
		{"text/event-stream", true},
		{"text/event-stream, application/json", true},
		{"application/json, text/event-stream;q=0.9", true},
		{"TEXT/EVENT-STREAM; charset=utf-8", true},
		{"text/event-stream;q=0, application/json", false},
		{"text/event-stream;q=0.0", false},
		{"text/event-stream;q=NaN", false},
		{"text/event-stream;q=Inf", false},
		{"text/event-stream;q=-0.5", false},
		{"text/event-stream;q=1.5", false},
		{"text/event-stream;q=invalid", false},
		{`application/json;ext="foo,text/event-stream"`, false},
		{`application/json;ext="foo,bar", text/event-stream;q=0.8`, true},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodPost, "/jobs/generate", nil)
		if tc.accept != "" {
			req.Header.Set("Accept", tc.accept)
		}
		if got := wantsEventStream(req); got != tc.want {
			t.Errorf("Accept=%q: got %v, want %v", tc.accept, got, tc.want)
		}
	}
}

func TestLoadSettings_SSEAndMockDelayBounds(t *testing.T) {
	// Default: enableSseProgress=false, mockDelayMs=0.
	sDefault, err := LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"]}`),
	})
	if err != nil {
		t.Fatalf("LoadSettings default: %v", err)
	}
	if sDefault.EnableSSEProgress {
		t.Fatal("expected EnableSSEProgress default false")
	}
	if sDefault.Public().EnableSSEProgress {
		t.Fatal("expected PublicConfig.EnableSSEProgress default false")
	}
	if sDefault.MockDelayMs != 0 {
		t.Fatalf("expected MockDelayMs=0, got %d", sDefault.MockDelayMs)
	}

	// MockDelayMs is clamped to 10000 in mock mode and zeroed in grafana-llm-app mode.
	sMock, err := LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"enableSseProgress":true,"mockDelayMs":99999}`),
	})
	if err != nil {
		t.Fatalf("LoadSettings mock: %v", err)
	}
	if !sMock.EnableSSEProgress || !sMock.Public().EnableSSEProgress {
		t.Fatal("expected EnableSSEProgress=true")
	}
	if sMock.MockDelayMs != 10000 {
		t.Fatalf("expected MockDelayMs clamped to 10000, got %d", sMock.MockDelayMs)
	}

	sProd, err := LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"grafana-llm-app","allowedDatasourceUids":["prom-1"],"enableSseProgress":true,"mockDelayMs":5000}`),
	})
	if err != nil {
		t.Fatalf("LoadSettings prod: %v", err)
	}
	if sProd.MockDelayMs != 0 {
		t.Fatalf("expected MockDelayMs forced to 0 outside mock mode, got %d", sProd.MockDelayMs)
	}
}

func TestApp_SSEDisabledIgnoresAcceptHeader(t *testing.T) {
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"enableSseProgress":false}`),
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)
	defer app.Dispose()

	sender := &streamCaptureSender{}
	err = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Headers:       map[string][]string{"Accept": {"text/event-stream, application/json"}},
		Body:          sampleGenerateBody(),
	}, sender)
	if err != nil {
		t.Fatalf("CallResource: %v", err)
	}
	chunks := sender.snapshot()
	if len(chunks) != 1 {
		t.Fatalf("expected single JSON response chunk when SSE disabled, got %d", len(chunks))
	}
	if ct := strings.Join(chunks[0].Headers["Content-Type"], ","); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected application/json Content-Type, got %q", ct)
	}
	var snap jobs.JobSnapshot
	if err := json.Unmarshal(chunks[0].Body, &snap); err != nil {
		t.Fatalf("unmarshal JobSnapshot: %v", err)
	}
	if snap.Status != jobs.StatusAwaitingPreview || snap.CurrentIR == nil {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestApp_SSEIncrementalFlushHeartbeatAndStages(t *testing.T) {
	settings, err := LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"enableSseProgress":true}`),
	})
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}

	gate := &gatedLLMClient{
		delegate: &llm.MockClient{},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	app := newAppWithClient(settings, gate, nil)
	app.sseHeartbeatInterval = 25 * time.Millisecond
	defer app.Dispose()

	firstStageFlushed := make(chan struct{})
	heartbeatFlushed := make(chan struct{})
	var firstOnce, hbOnce sync.Once

	sender := &streamCaptureSender{
		onChunk: func(_ int, resp *backend.CallResourceResponse) {
			body := string(resp.Body)
			if strings.Contains(body, `"stage":"generating"`) {
				firstOnce.Do(func() { close(firstStageFlushed) })
			}
			if strings.Contains(body, ": keepalive") {
				hbOnce.Do(func() { close(heartbeatFlushed) })
			}
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- app.CallResource(context.Background(), &backend.CallResourceRequest{
			PluginContext: testAuthCtx(),
			Path:          "jobs/generate",
			Method:        http.MethodPost,
			Headers:       map[string][]string{"Accept": {"text/event-stream, application/json"}},
			Body:          sampleGenerateBody(),
		}, sender)
	}()

	// Stage "generating" MUST flush immediately BEFORE the LLM completes.
	select {
	case <-firstStageFlushed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for incremental flush of generating stage event")
	}

	// Keepalive comment MUST flush while waiting on LLM.
	select {
	case <-heartbeatFlushed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE keepalive heartbeat chunk")
	}

	// Unblock LLM and wait for completion.
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatalf("CallResource: %v", err)
	}

	chunks := sender.snapshot()
	if len(chunks) < 4 {
		t.Fatalf("expected >=4 incrementally flushed chunks (generating, keepalive, validating_ir, awaiting_preview/result), got %d", len(chunks))
	}
	if chunks[0].Status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", chunks[0].Status)
	}
	if ct := strings.Join(chunks[0].Headers["Content-Type"], ","); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream Content-Type, got %q", ct)
	}
	if xab := strings.Join(chunks[0].Headers["X-Accel-Buffering"], ","); xab != "no" {
		t.Fatalf("expected X-Accel-Buffering=no, got %q", xab)
	}
	if cc := strings.Join(chunks[0].Headers["Cache-Control"], ","); !strings.Contains(cc, "no-cache") || !strings.Contains(cc, "no-transform") {
		t.Fatalf("expected Cache-Control no-cache, no-transform, got %q", cc)
	}

	frames := parseSSEFrames(sender.combinedBody())
	var stages []jobs.JobStatus
	var resultSnap *jobs.JobSnapshot
	for _, f := range frames {
		switch f.Event {
		case "stage":
			// Stage events MUST NEVER carry raw LLM tokens or unvalidated IR panels.
			if strings.Contains(f.Data, `"panels"`) || strings.Contains(f.Data, `"currentIR"`) {
				t.Fatalf("stage event leaked IR/token data: %s", f.Data)
			}
			var ev jobs.StageEvent
			if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
				t.Fatalf("unmarshal stage event: %v", err)
			}
			if ev.Operation != jobs.OperationGenerate || ev.JobID == "" {
				t.Fatalf("unexpected stage event metadata: %+v", ev)
			}
			stages = append(stages, ev.Stage)
		case "result":
			var snap jobs.JobSnapshot
			if err := json.Unmarshal([]byte(f.Data), &snap); err != nil {
				t.Fatalf("unmarshal result event: %v", err)
			}
			resultSnap = &snap
		default:
			t.Fatalf("unexpected SSE event type %q: %s", f.Event, f.Data)
		}
	}

	wantStages := []jobs.JobStatus{jobs.StatusGenerating, jobs.StatusValidatingIR, jobs.StatusAwaitingPreview}
	if len(stages) != len(wantStages) {
		t.Fatalf("stages=%v, want %v", stages, wantStages)
	}
	for i := range wantStages {
		if stages[i] != wantStages[i] {
			t.Fatalf("stage[%d]=%q, want %q (all=%v)", i, stages[i], wantStages[i], stages)
		}
	}
	if resultSnap == nil || resultSnap.CurrentIR == nil || resultSnap.CurrentIR.Revision != 1 {
		t.Fatalf("expected validated rev 1 result snapshot, got %+v", resultSnap)
	}

	// Verify Patch SSE stage progression as well.
	patchSender := &streamCaptureSender{}
	err = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/" + resultSnap.JobID + "/patch",
		Method:        http.MethodPost,
		Headers:       map[string][]string{"Accept": {"text/event-stream"}},
		Body: []byte(`{
			"baseRevision": 1,
			"editIntent": "Change error rate panel to timeseries",
			"catalog": [
				{"name": "http_requests_total", "type": "counter", "labels": ["service", "status"]},
				{"name": "http_request_duration_seconds_bucket", "type": "histogram", "labels": ["service", "le"]}
			]
		}`),
	}, patchSender)
	if err != nil {
		t.Fatalf("patch CallResource: %v", err)
	}
	patchFrames := parseSSEFrames(patchSender.combinedBody())
	var patchStages []jobs.JobStatus
	var patchSnap *jobs.JobSnapshot
	for _, f := range patchFrames {
		if f.Event == "stage" {
			var ev jobs.StageEvent
			_ = json.Unmarshal([]byte(f.Data), &ev)
			if ev.Operation != jobs.OperationPatch {
				t.Fatalf("expected operation=patch, got %q", ev.Operation)
			}
			patchStages = append(patchStages, ev.Stage)
		} else if f.Event == "result" {
			var s jobs.JobSnapshot
			_ = json.Unmarshal([]byte(f.Data), &s)
			patchSnap = &s
		}
	}
	if len(patchStages) != 3 || patchStages[0] != jobs.StatusGenerating || patchStages[1] != jobs.StatusValidatingIR || patchStages[2] != jobs.StatusAwaitingPreview {
		t.Fatalf("unexpected patch stages: %v", patchStages)
	}
	if patchSnap == nil || patchSnap.CurrentIR == nil || patchSnap.CurrentIR.Revision != 2 {
		t.Fatalf("expected patch rev 2, got %+v", patchSnap)
	}
}

func TestApp_SSEClientDisconnectCancelsLLMAndReleasesSlot(t *testing.T) {
	settings, err := LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"enableSseProgress":true}`),
	})
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}

	gate := &gatedLLMClient{
		delegate: &llm.MockClient{},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	app := newAppWithClient(settings, gate, nil)
	defer app.Dispose()

	var observedJobID string
	var jobIDMu sync.Mutex
	gotJobID := make(chan struct{})
	var gotOnce sync.Once

	sender := &streamCaptureSender{
		onChunk: func(_ int, resp *backend.CallResourceResponse) {
			for _, f := range parseSSEFrames(string(resp.Body)) {
				if f.Event == "stage" {
					var ev jobs.StageEvent
					if json.Unmarshal([]byte(f.Data), &ev) == nil && ev.JobID != "" {
						jobIDMu.Lock()
						observedJobID = ev.JobID
						jobIDMu.Unlock()
						gotOnce.Do(func() { close(gotJobID) })
					}
				}
			}
		},
	}

	reqCtx, cancelReq := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.CallResource(reqCtx, &backend.CallResourceRequest{
			PluginContext: testAuthCtx(),
			Path:          "jobs/generate",
			Method:        http.MethodPost,
			Headers:       map[string][]string{"Accept": {"text/event-stream"}},
			Body:          sampleGenerateBody(),
		}, sender)
	}()

	select {
	case <-gotJobID:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for generating stage with jobId")
	}

	// Simulate client disconnect by cancelling CallResource context.
	cancelReq()

	select {
	case <-gate.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected LLM context to be cancelled on client disconnect")
	}
	if err := <-done; err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	jobIDMu.Lock()
	jobID := observedJobID
	jobIDMu.Unlock()

	// Verify job transitioned cleanly to cancelled via GET /jobs/{id}.
	getSender := &streamCaptureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/" + jobID,
		Method:        http.MethodGet,
	}, getSender)
	var snap jobs.JobSnapshot
	if err := json.Unmarshal([]byte(getSender.combinedBody()), &snap); err != nil {
		t.Fatalf("unmarshal job after disconnect: %v", err)
	}
	if snap.Status != jobs.StatusCancelled {
		t.Fatalf("expected status=cancelled after client disconnect, got %q", snap.Status)
	}

	// M-4: Verify per-user concurrency slot (limit=1) was genuinely released on the same App/Store instance.
	close(gate.release)
	followUpSender := &streamCaptureSender{}
	_ = app.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Body:          sampleGenerateBody(),
	}, followUpSender)
	followUpChunks := followUpSender.snapshot()
	if len(followUpChunks) != 1 || followUpChunks[0].Status != http.StatusOK {
		t.Fatalf("expected follow-up Generate after disconnect to succeed (slot released), got %+v", followUpChunks)
	}
}

func TestApp_SSEPreFlightAndMidFlightErrors(t *testing.T) {
	settings, err := LoadSettings(backend.AppInstanceSettings{
		JSONData: []byte(`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"enableSseProgress":true}`),
	})
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}

	// 1. Pre-flight error (denied datasource) MUST return HTTP 403 JSON, NOT 200 text/event-stream.
	appMock := newAppWithClient(settings, &llm.MockClient{}, nil)
	defer appMock.Dispose()

	deniedSender := &streamCaptureSender{}
	_ = appMock.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Headers:       map[string][]string{"Accept": {"text/event-stream, application/json"}},
		Body:          []byte(`{"datasourceUid":"prom-forbidden","userIntent":"test","catalog":[{"name":"up"}]}`),
	}, deniedSender)
	deniedChunks := deniedSender.snapshot()
	if len(deniedChunks) != 1 || deniedChunks[0].Status != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 pre-flight error, got %+v", deniedChunks)
	}
	if ct := strings.Join(deniedChunks[0].Headers["Content-Type"], ","); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected JSON Content-Type for pre-flight error, got %q", ct)
	}

	// 2. Repair SSE stages (repairing -> validating_ir -> awaiting_preview).
	mockWithSyntaxErr := &llm.MockClient{InjectSyntaxErrorOnFirstGenerate: true}
	appRepair := newAppWithClient(settings, mockWithSyntaxErr, nil)
	defer appRepair.Dispose()

	// Create imported job with a syntactically valid IR, then test Repair SSE using matching panel hash.
	importSender := &streamCaptureSender{}
	_ = appRepair.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/import",
		Method:        http.MethodPost,
		Body: []byte(`{
			"datasourceUid": "prom-1",
			"scope": {"labelName": "service", "labelValue": "checkout-api"},
			"draft": {
				"schemaVersion": "1.1",
				"revision": 1,
				"title": "Repair Test",
				"timeRange": {"from": "now-15m", "to": "now"},
				"panels": [{
					"id": "panel-req",
					"type": "timeseries",
					"title": "Req",
					"datasourceRef": "selected_prometheus",
					"query": {"expr": "rate(http_requests_total{service=\"checkout-api\"}[5m])"},
					"unit": "reqps",
					"layout": {"row": 0, "col": 0, "width": "full"}
				}]
			}
		}`),
	}, importSender)
	var imported jobs.JobSnapshot
	if err := json.Unmarshal([]byte(importSender.combinedBody()), &imported); err != nil {
		t.Fatalf("unmarshal import: %v", err)
	}
	qHash := ir.PanelQueryHash(imported.CurrentIR.Panels[0])
	repairBody, _ := json.Marshal(map[string]any{
		"baseRevision": 1,
		"results": []map[string]any{
			{"panelId": "panel-req", "queryHash": qHash, "category": "syntax_error", "errorText": "parse error"},
		},
		"catalog": []map[string]any{
			{"name": "http_requests_total", "type": "counter", "labels": []string{"service", "status"}},
		},
	})
	repairSender := &streamCaptureSender{}
	_ = appRepair.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/" + imported.JobID + "/repair",
		Method:        http.MethodPost,
		Headers:       map[string][]string{"Accept": {"text/event-stream"}},
		Body:          repairBody,
	}, repairSender)
	var repairStages []jobs.JobStatus
	for _, f := range parseSSEFrames(repairSender.combinedBody()) {
		if f.Event == "stage" {
			var ev jobs.StageEvent
			_ = json.Unmarshal([]byte(f.Data), &ev)
			if ev.Operation != jobs.OperationRepair {
				t.Fatalf("expected operation=repair, got %q", ev.Operation)
			}
			repairStages = append(repairStages, ev.Stage)
		}
	}
	if len(repairStages) != 3 || repairStages[0] != jobs.StatusRepairing || repairStages[1] != jobs.StatusValidatingIR || repairStages[2] != jobs.StatusAwaitingPreview {
		t.Fatalf("unexpected repair stages: %v", repairStages)
	}

	// 3. Mid-flight LLM error during Patch emits stage(generating) -> stage(failed) -> event: error with status & job.
	failGate := &gatedLLMClient{
		delegate: &llm.MockClient{},
		release:  make(chan struct{}),
		failErr:  errors.New("simulated upstream failure"),
	}
	close(failGate.release)
	appFail := newAppWithClient(settings, failGate, nil)
	defer appFail.Dispose()

	failSender := &streamCaptureSender{}
	_ = appFail.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Headers:       map[string][]string{"Accept": {"text/event-stream"}},
		Body:          sampleGenerateBody(),
	}, failSender)
	failFrames := parseSSEFrames(failSender.combinedBody())
	// Generate records LLM errors as a StatusFailed JobSnapshot (err == nil), emitted as stage(failed) + result.
	var sawFailedStage, sawResult bool
	for _, f := range failFrames {
		if f.Event == "stage" && strings.Contains(f.Data, `"stage":"failed"`) {
			sawFailedStage = true
		}
		if f.Event == "result" && strings.Contains(f.Data, `"status":"failed"`) {
			sawResult = true
		}
	}
	if !sawFailedStage || !sawResult {
		t.Fatalf("expected failed stage and result snapshot, got frames=%+v", failFrames)
	}

	// 4. Mid-flight Patch error (ErrOutputTruncated) emits stage(generating) -> stage(failed) -> event: error (status=502, job).
	patchFailGate := &gatedLLMClient{
		delegate:     &llm.MockClient{},
		release:      make(chan struct{}),
		patchFailErr: llm.ErrOutputTruncated,
	}
	close(patchFailGate.release)
	appPatchFail := newAppWithClient(settings, patchFailGate, nil)
	defer appPatchFail.Dispose()

	genSender := &streamCaptureSender{}
	_ = appPatchFail.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/generate",
		Method:        http.MethodPost,
		Body:          sampleGenerateBody(),
	}, genSender)
	var baseJob jobs.JobSnapshot
	if err := json.Unmarshal([]byte(genSender.combinedBody()), &baseJob); err != nil {
		t.Fatalf("unmarshal base job: %v", err)
	}

	patchErrSender := &streamCaptureSender{}
	_ = appPatchFail.CallResource(context.Background(), &backend.CallResourceRequest{
		PluginContext: testAuthCtx(),
		Path:          "jobs/" + baseJob.JobID + "/patch",
		Method:        http.MethodPost,
		Headers:       map[string][]string{"Accept": {"text/event-stream"}},
		Body: []byte(`{
			"baseRevision": 1,
			"editIntent": "Change error rate panel",
			"catalog": [{"name": "http_requests_total", "type": "counter", "labels": ["service", "status"]}]
		}`),
	}, patchErrSender)
	patchErrFrames := parseSSEFrames(patchErrSender.combinedBody())
	var sawPatchErrorEvent bool
	for _, f := range patchErrFrames {
		if f.Event == "error" {
			var payload struct {
				Status int               `json:"status"`
				Error  string            `json:"error"`
				Job    *jobs.JobSnapshot `json:"job"`
			}
			if err := json.Unmarshal([]byte(f.Data), &payload); err != nil {
				t.Fatalf("unmarshal SSE error frame: %v", err)
			}
			if payload.Status != http.StatusBadGateway || payload.Job == nil || payload.Job.JobID != baseJob.JobID {
				t.Fatalf("unexpected SSE error payload: %+v", payload)
			}
			sawPatchErrorEvent = true
		}
	}
	if !sawPatchErrorEvent {
		t.Fatalf("expected SSE event: error frame on mid-flight patch failure, got %+v", patchErrFrames)
	}
}
