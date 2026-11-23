package bench_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-llm-app/llmclient"
	"github.com/sashabaranov/go-openai"
	"github.com/yuuki/grafana-llm-studio-app/pkg/bench"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

func TestPresetsCoverAllTargetProviders(t *testing.T) {
	want := []string{
		"openai-gpt41-mini",
		"openai-gpt41",
		"openai-o3",
		"openrouter",
		"deepseek",
		"gemini",
		"vllm",
		"vllm-reasoning",
	}
	for _, name := range want {
		cfg := bench.Config{}
		if err := bench.ApplyPreset(name, &cfg); err != nil {
			t.Fatalf("ApplyPreset(%q) failed: %v", name, err)
		}
		if cfg.Preset != name {
			t.Fatalf("preset=%q, want %q", cfg.Preset, name)
		}
		if name == "deepseek" {
			if cfg.LLMConfig.StructuredOutputMode != llm.StructuredOutputJSONObject {
				t.Fatalf("deepseek should default to json_object, got %q", cfg.LLMConfig.StructuredOutputMode)
			}
			if cfg.LLMConfig.TokenLimitField != llm.TokenLimitMaxTokens {
				t.Fatalf("deepseek should default to max_tokens, got %q", cfg.LLMConfig.TokenLimitField)
			}
		}
		if name == "openai-o3" || name == "vllm-reasoning" {
			if cfg.LLMConfig.ReasoningEffort != "low" {
				t.Fatalf("%s should set reasoning_effort=low, got %q", name, cfg.LLMConfig.ReasoningEffort)
			}
		}
	}

	if err := bench.ApplyPreset("does-not-exist", &bench.Config{}); err == nil {
		t.Fatal("expected error for unknown preset")
	}
}

func TestRunnerFullSuiteThroughGrafanaLLMAppAndPrometheus(t *testing.T) {
	var (
		mu              sync.Mutex
		capturedReqs    []map[string]any
		promQueryCount  int
		expectedSAToken = "glsa_bench_test_token"
	)

	// 1. Mock OpenAI-compatible upstream answering all 9 default scenarios deterministically.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		if err := json.Unmarshal(raw, &reqBody); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		capturedReqs = append(capturedReqs, reqBody)
		mu.Unlock()

		msgs, _ := reqBody["messages"].([]any)
		userContent := ""
		if len(msgs) >= 2 {
			if m, ok := msgs[1].(map[string]any); ok {
				userContent, _ = m["content"].(string)
			}
		}
		var payload map[string]any
		_ = json.Unmarshal([]byte(userContent), &payload)
		task, _ := payload["task"].(string)

		var contentBytes []byte
		switch task {
		case "generate_dashboard_ir":
			doc := ir.DashboardIR{
				SchemaVersion: ir.SchemaVersion,
				Revision:      1,
				Title:         "checkout-api Observability Overview",
				TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
				Panels: []ir.PanelIR{
					{
						ID:            "panel-request-rate",
						Type:          "timeseries",
						Title:         "Request Rate",
						DatasourceRef: ir.SelectedDatasourceRef,
						Query:         ir.QueryIR{Expr: `sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))`, LegendFormat: "{{status}}"},
						Unit:          "reqps",
						Layout:        ir.LayoutHint{Row: 0, Col: 0, Width: "half"},
					},
					{
						ID:            "panel-error-rate",
						Type:          "stat",
						Title:         "5xx Error Ratio",
						DatasourceRef: ir.SelectedDatasourceRef,
						Query:         ir.QueryIR{Expr: `sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m])) / clamp_min(sum(rate(http_requests_total{service="checkout-api"}[5m])), 1e-9)`},
						Unit:          "percentunit",
						Layout:        ir.LayoutHint{Row: 0, Col: 1, Width: "half"},
					},
					{
						ID:            "panel-latency-p95",
						Type:          "timeseries",
						Title:         "p95 Request Latency",
						DatasourceRef: ir.SelectedDatasourceRef,
						Query:         ir.QueryIR{Expr: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout-api"}[5m])))`},
						Unit:          "s",
						Layout:        ir.LayoutHint{Row: 1, Col: 0, Width: "half"},
					},
					{
						ID:            "panel-memory-bytes",
						Type:          "stat",
						Title:         "Resident Memory",
						DatasourceRef: ir.SelectedDatasourceRef,
						Query:         ir.QueryIR{Expr: `avg(process_resident_memory_bytes{service="checkout-api"})`},
						Unit:          "bytes",
						Layout:        ir.LayoutHint{Row: 1, Col: 1, Width: "half"},
					},
				},
			}
			contentBytes, _ = json.Marshal(doc)

		case "generate_dashboard_patch":
			editReq, _ := payload["editRequest"].(string)
			targetID, _ := payload["targetPanelId"].(string)
			var patch ir.DashboardPatch
			switch {
			case targetID == "panel-latency-p95":
				legend := "p99"
				patch = ir.DashboardPatch{
					SchemaVersion: ir.SchemaVersion,
					BaseRevision:  1,
					Summary:       "Updated latency panel from p95 to p99",
					Operations: []ir.PatchOperation{
						{
							Op:      "update_panel",
							PanelID: "panel-latency-p95",
							Panel: &ir.PanelUpdate{
								Title: "p99 Request Latency",
								Query: &ir.QueryUpdate{
									Expr:         `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout-api"}[5m])))`,
									LegendFormat: &legend,
								},
							},
						},
					},
				}
			case strings.Contains(strings.ToLower(editReq), "add a new stat panel"):
				patch = ir.DashboardPatch{
					SchemaVersion: ir.SchemaVersion,
					BaseRevision:  1,
					Summary:       "Added resident memory stat panel",
					Operations: []ir.PatchOperation{
						{
							Op:      "add_panel",
							PanelID: "panel-memory-bytes",
							Panel: &ir.PanelUpdate{
								ID:            "panel-memory-bytes",
								Type:          "stat",
								Title:         "Resident Memory",
								DatasourceRef: ir.SelectedDatasourceRef,
								Query:         &ir.QueryUpdate{Expr: `avg(process_resident_memory_bytes{service="checkout-api"})`},
								Unit:          "bytes",
								Layout:        &ir.LayoutHint{Row: 1, Col: 0, Width: "full"},
							},
						},
					},
				}
			default:
				patch = ir.DashboardPatch{
					SchemaVersion: ir.SchemaVersion,
					BaseRevision:  1,
					Summary:       "Updated timeRange to now-1h and expanded error matcher to 4xx/5xx",
					Operations: []ir.PatchOperation{
						{
							Op:        "update_meta",
							TimeRange: &ir.TimeRange{From: "now-1h", To: "now"},
						},
						{
							Op:      "update_panel",
							PanelID: "panel-error-rate",
							Panel: &ir.PanelUpdate{
								Query: &ir.QueryUpdate{
									Expr: `sum(rate(http_requests_total{service="checkout-api",status=~"4..|5.."}[5m])) / clamp_min(sum(rate(http_requests_total{service="checkout-api"}[5m])), 1e-9)`,
								},
							},
						},
					},
				}
			}
			contentBytes, _ = json.Marshal(patch)

		case "repair_syntax_errors":
			failing, _ := payload["failingPanels"].([]any)
			first, _ := failing[0].(map[string]any)
			panelID, _ := first["panelId"].(string)
			repairedExpr := `sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))`
			if panelID == "panel-error-ratio" {
				repairedExpr = `sum(rate(http_requests_total{service="checkout-api",status="500"}[5m])) / clamp_min(sum(rate(http_requests_total{service="checkout-api"}[5m])), 1e-9)`
			}
			patch := ir.DashboardPatch{
				SchemaVersion: ir.SchemaVersion,
				BaseRevision:  1,
				Summary:       "Repaired PromQL syntax error",
				Operations: []ir.PatchOperation{
					{
						Op:      "update_panel",
						PanelID: panelID,
						Panel: &ir.PanelUpdate{
							Query: &ir.QueryUpdate{Expr: repairedExpr},
						},
					},
				},
			}
			contentBytes, _ = json.Marshal(patch)
		}

		resp := openai.ChatCompletionResponse{
			ID:    "chatcmpl-bench-test",
			Model: "gpt-4.1-mini-2025-04-14",
			Choices: []openai.ChatCompletionChoice{
				{
					Index:        0,
					Message:      openai.ChatCompletionMessage{Role: "assistant", Content: string(contentBytes)},
					FinishReason: openai.FinishReasonStop,
				},
			},
			Usage: openai.Usage{PromptTokens: 420, CompletionTokens: 180, TotalTokens: 600},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstreamSrv.Close()

	// 2. Mock Grafana server hosting grafana-llm-app resource proxy (mirroring go-openai unmarshal).
	grafanaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectedSAToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/plugins/grafana-llm-app/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"llmProvider":{"configured":true,"ok":true}}}`))
		case "/api/plugins/grafana-llm-app/resources/llm/v1/chat/completions":
			var incoming struct {
				openai.ChatCompletionRequest
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			incoming.ChatCompletionRequest.Model = "gpt-4.1-mini-2025-04-14"
			fwdBytes, _ := json.Marshal(incoming.ChatCompletionRequest)
			fwdReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamSrv.URL+"/v1/chat/completions", bytes.NewReader(fwdBytes))
			fwdReq.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(fwdReq)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer grafanaSrv.Close()

	// 3. Mock Prometheus /api/v1/query_range endpoint.
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		mu.Lock()
		promQueryCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"service":"checkout-api"},"values":[[1700000000,"42.1"]]}]}}`))
	}))
	defer promSrv.Close()

	cfg := bench.Config{
		ProviderLabel: "openai/gpt-4.1-mini-mock",
		LLMConfig: llm.GrafanaLLMConfig{
			GrafanaURL:          grafanaSrv.URL,
			ServiceAccountToken: expectedSAToken,
			RequestTimeout:      5 * time.Second,
			MaxTransportRetries: 1,
		},
		Iterations:    1,
		PrometheusURL: promSrv.URL,
	}
	if err := bench.ApplyPreset("openai-gpt41-mini", &cfg); err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}

	runner, err := bench.NewRunner(cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Runner.Run failed: %v", err)
	}

	if !report.HealthCheckOK {
		t.Fatalf("expected HealthCheckOK=true")
	}
	if report.ResolvedModel != "gpt-4.1-mini-2025-04-14" {
		t.Fatalf("expected ResolvedModel=gpt-4.1-mini-2025-04-14, got %q", report.ResolvedModel)
	}
	if report.Summary.TotalRuns != len(bench.DefaultScenarios()) {
		t.Fatalf("expected %d runs, got %d", len(bench.DefaultScenarios()), report.Summary.TotalRuns)
	}
	if report.Summary.PassedRuns != report.Summary.TotalRuns {
		for _, r := range report.Results {
			if !r.Passed {
				t.Errorf("scenario %s failed at %s: %s", r.ScenarioID, r.FailureStage, r.FailureDetail)
			}
		}
		t.Fatalf("expected 100%% pass rate, got %d/%d", report.Summary.PassedRuns, report.Summary.TotalRuns)
	}
	if report.Summary.PrometheusPassRate != 100.0 || promQueryCount == 0 {
		t.Fatalf("expected 100%% Prometheus pass rate, got %.1f%% (queries=%d)", report.Summary.PrometheusPassRate, promQueryCount)
	}
	if report.Summary.Tokens.TotalPromptTokens != 420*report.Summary.TotalRuns {
		t.Fatalf("unexpected total prompt tokens: %d", report.Summary.Tokens.TotalPromptTokens)
	}
	if report.Summary.FinishReasonCounts["stop"] != report.Summary.TotalRuns {
		t.Fatalf("unexpected finish_reason counts: %+v", report.Summary.FinishReasonCounts)
	}
	if len(capturedReqs) != report.Summary.TotalRuns {
		t.Fatalf("expected %d captured upstream requests, got %d", report.Summary.TotalRuns, len(capturedReqs))
	}

	md := bench.RenderMarkdown(report)
	if !strings.Contains(md, "openai/gpt-4.1-mini-mock") || !strings.Contains(md, "100.0%") {
		t.Fatalf("rendered markdown missing expected summary:\n%s", md)
	}
	if strings.Contains(md, expectedSAToken) {
		t.Fatal("rendered markdown must never leak service account token")
	}
	var jsonBuf bytes.Buffer
	if err := bench.WriteJSON(&jsonBuf, report); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	if strings.Contains(jsonBuf.String(), expectedSAToken) {
		t.Fatal("JSON report must never leak service account token")
	}
}

type stubLLMClient struct {
	enabled bool
	genFn   func(context.Context, llm.GenerateRequest) ([]byte, llm.CompletionMeta, error)
	patchFn func(context.Context, llm.PatchRequest) ([]byte, llm.CompletionMeta, error)
	repFn   func(context.Context, llm.RepairRequest) ([]byte, llm.CompletionMeta, error)
}

func (s *stubLLMClient) Enabled(_ context.Context) (bool, error) { return s.enabled, nil }
func (s *stubLLMClient) GenerateIR(ctx context.Context, r llm.GenerateRequest) ([]byte, llm.CompletionMeta, error) {
	return s.genFn(ctx, r)
}
func (s *stubLLMClient) GeneratePatch(ctx context.Context, r llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
	return s.patchFn(ctx, r)
}
func (s *stubLLMClient) RepairPanels(ctx context.Context, r llm.RepairRequest) ([]byte, llm.CompletionMeta, error) {
	return s.repFn(ctx, r)
}

func TestRunnerDetectsTruncationAndQueryOnlyGuardViolation(t *testing.T) {
	stub := &stubLLMClient{
		enabled: true,
		genFn: func(_ context.Context, _ llm.GenerateRequest) ([]byte, llm.CompletionMeta, error) {
			return nil, llm.CompletionMeta{
				FinishReason:     "length",
				PromptTokens:     500,
				CompletionTokens: 2048,
				Model:            "o3-mini",
				ProviderMode:     "grafana-llm-app",
			}, llm.ErrOutputTruncated
		},
		patchFn: func(_ context.Context, _ llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
			return nil, llm.CompletionMeta{}, nil
		},
		repFn: func(_ context.Context, _ llm.RepairRequest) ([]byte, llm.CompletionMeta, error) {
			// Disallowed: attempts to change panel Title during syntax repair.
			badPatch := ir.DashboardPatch{
				SchemaVersion: ir.SchemaVersion,
				BaseRevision:  1,
				Summary:       "Fixed syntax and renamed title",
				Operations: []ir.PatchOperation{
					{
						Op:      "update_panel",
						PanelID: "panel-request-rate",
						Panel: &ir.PanelUpdate{
							Title: "Renamed Title Not Allowed",
							Query: &ir.QueryUpdate{Expr: `sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))`},
						},
					},
				},
			}
			raw, _ := json.Marshal(badPatch)
			return raw, llm.CompletionMeta{FinishReason: "stop", PromptTokens: 300, CompletionTokens: 80, Model: "o3-mini"}, nil
		},
	}

	cfg := bench.Config{
		ProviderLabel:  "test-failure-modes",
		ScenarioFilter: []string{"gen-red-en", "repair-syntax-paren"},
		LLMConfig: llm.GrafanaLLMConfig{
			GrafanaURL:          "http://127.0.0.1:3000",
			ServiceAccountToken: "glsa_unused",
			ModelTier:           llmclient.ModelBase,
		},
	}
	runner, err := bench.NewRunnerWithClient(cfg, stub, bench.DefaultScenarios())
	if err != nil {
		t.Fatalf("NewRunnerWithClient: %v", err)
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if report.Summary.PassedRuns != 0 || report.Summary.TotalRuns != 2 {
		t.Fatalf("expected 0/2 passed, got %d/%d", report.Summary.PassedRuns, report.Summary.TotalRuns)
	}
	if report.Results[0].FailureStage != "finish_reason" {
		t.Fatalf("expected first failure stage finish_reason, got %q", report.Results[0].FailureStage)
	}
	if report.Summary.FinishReasonCounts["length"] != 1 {
		t.Fatalf("expected finish_reason length count=1, got %+v", report.Summary.FinishReasonCounts)
	}
	if report.Results[1].FailureStage != "patch_guard" {
		t.Fatalf("expected second failure stage patch_guard, got %q (%s)", report.Results[1].FailureStage, report.Results[1].FailureDetail)
	}
}
