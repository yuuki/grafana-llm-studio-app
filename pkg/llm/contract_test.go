package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-llm-app/llmclient"
	"github.com/sashabaranov/go-openai"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

// TestGrafanaLLMApp_To_VLLM_Contract verifies the full contract:
// Custom Backend (GrafanaLLMClient) -> official llmclient -> Grafana plugin resource API
// (grafana-llm-app proxy) -> local vLLM OpenAI-compatible endpoint.
func TestGrafanaLLMApp_To_VLLM_Contract(t *testing.T) {
	var capturedVLLMReq map[string]any
	var vllmCalls atomic.Int32
	var forceStatus atomic.Int32
	var forceFinishReason atomic.Value
	forceFinishReason.Store("stop")

	// 1. Mock local vLLM OpenAI-compatible server.
	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vllmCalls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if code := forceStatus.Load(); code != 0 {
			http.Error(w, `{"error":{"message":"simulated upstream failure"}}`, int(code))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		capturedVLLMReq = body

		validIR := ir.DashboardIR{
			SchemaVersion: ir.SchemaVersion,
			Revision:      1,
			Title:         "checkout-api Overview",
			TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
			Panels: []ir.PanelIR{
				{
					ID:            "panel-req",
					Type:          "timeseries",
					Title:         "Request Rate",
					DatasourceRef: ir.SelectedDatasourceRef,
					Query:         ir.QueryIR{Expr: `sum(rate(http_requests_total{service="checkout-api"}[5m]))`},
					Unit:          "reqps",
					Layout:        ir.LayoutHint{Row: 0, Col: 0, Width: "full"},
				},
			},
		}
		contentBytes, _ := json.Marshal(validIR)
		resp := openai.ChatCompletionResponse{
			ID:    "chatcmpl-vllm-test",
			Model: "zai-org/GLM-5.3-Flash",
			Choices: []openai.ChatCompletionChoice{
				{
					Index:        0,
					Message:      openai.ChatCompletionMessage{Role: "assistant", Content: string(contentBytes)},
					FinishReason: openai.FinishReason(forceFinishReason.Load().(string)),
				},
			},
			Usage: openai.Usage{PromptTokens: 210, CompletionTokens: 95, TotalTokens: 305},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer vllmSrv.Close()

	// 2. Mock Grafana server hosting grafana-llm-app resource proxy (mirrors real grafana-llm-app v1.0.8 behavior:
	//    unmarshals into openai.ChatCompletionRequest, maps "base"/"large" to vLLM served model name, forwards to vLLM).
	const expectedSAToken = "glsa_test_llm_only_token"
	grafanaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectedSAToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/plugins/grafana-llm-app/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"llmProvider":{"configured":true,"ok":true}}}`))
			return
		case "/api/plugins/grafana-llm-app/resources/llm/v1/chat/completions":
			// Mirror grafana-llm-app: decode into openai.ChatCompletionRequest (which drops unknown non-OpenAI fields).
			var incoming struct {
				openai.ChatCompletionRequest
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
				http.Error(w, `{"error":"bad request: `+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			if incoming.Model != "base" && incoming.Model != "large" {
				http.Error(w, `{"error":"unrecognized model"}`, http.StatusBadRequest)
				return
			}
			// Model mapping managed by grafana-llm-app.
			incoming.ChatCompletionRequest.Model = "zai-org/GLM-5.3-Flash"

			fwdBytes, _ := json.Marshal(incoming.ChatCompletionRequest)
			fwdReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, vllmSrv.URL+"/v1/chat/completions", io.NopCloser(jsonReader(fwdBytes)))
			fwdReq.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(fwdReq)
			if err != nil {
				http.Error(w, `{"error":"upstream unreachable"}`, http.StatusBadGateway)
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

	client, err := llm.NewGrafanaLLMClient(llm.GrafanaLLMConfig{
		GrafanaURL:          grafanaSrv.URL,
		ServiceAccountToken: expectedSAToken,
		ModelTier:           llmclient.ModelBase,
		MaxOutputTokens:     1536,
		ReasoningEffort:     "low",
		RequestTimeout:      5 * time.Second,
		MaxTransportRetries: 1,
	})
	if err != nil {
		t.Fatalf("NewGrafanaLLMClient: %v", err)
	}

	catalog := []ir.MetricCatalogItem{
		{Name: "http_requests_total", Type: "counter", Help: "HTTP requests"},
	}
	scope := &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"}

	t.Run("structured output JSON schema and parameters reach vLLM intact", func(t *testing.T) {
		ok, err := client.Enabled(context.Background())
		if err != nil || !ok {
			t.Fatalf("Enabled() = (%v, %v), want (true, nil)", ok, err)
		}

		raw, meta, err := client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if err != nil {
			t.Fatalf("GenerateIR failed: %v", err)
		}
		if meta.FinishReason != "stop" || meta.ProviderMode != "grafana-llm-app" {
			t.Fatalf("unexpected meta: %+v", meta)
		}
		if _, err := ir.DecodeStrictIR(raw); err != nil {
			t.Fatalf("returned IR failed strict decode: %v", err)
		}

		// Verify contract fields received by vLLM.
		if capturedVLLMReq["model"] != "zai-org/GLM-5.3-Flash" {
			t.Fatalf("expected mapped model name, got %v", capturedVLLMReq["model"])
		}
		if capturedVLLMReq["max_completion_tokens"] != float64(1536) {
			t.Fatalf("expected max_completion_tokens=1536, got %v", capturedVLLMReq["max_completion_tokens"])
		}
		if capturedVLLMReq["reasoning_effort"] != "low" {
			t.Fatalf("expected reasoning_effort=low, got %v", capturedVLLMReq["reasoning_effort"])
		}
		rf, ok := capturedVLLMReq["response_format"].(map[string]any)
		if !ok || rf["type"] != "json_schema" {
			t.Fatalf("response_format.type=json_schema did not reach vLLM: %#v", capturedVLLMReq["response_format"])
		}
		js, ok := rf["json_schema"].(map[string]any)
		if !ok || js["name"] != "dashboard_ir" || js["strict"] != true {
			t.Fatalf("response_format.json_schema contract broken: %#v", rf)
		}
		schemaObj, ok := js["schema"].(map[string]any)
		if !ok || schemaObj["type"] != "object" || schemaObj["additionalProperties"] != false {
			t.Fatalf("DashboardIR JSON Schema structure did not survive proxy round-trip: %#v", js["schema"])
		}
		props, ok := schemaObj["properties"].(map[string]any)
		if !ok || props["panels"] == nil || props["schemaVersion"] == nil {
			t.Fatalf("DashboardIR properties missing after grafana-llm-app proxy round-trip: %#v", schemaObj)
		}
		// Verify datasourceRef single-value string enum reaches vLLM intact.
		panelsDef := props["panels"].(map[string]any)
		itemProps := panelsDef["items"].(map[string]any)["properties"].(map[string]any)
		dsEnum := itemProps["datasourceRef"].(map[string]any)["enum"].([]any)
		if len(dsEnum) != 1 || dsEnum[0] != "selected_prometheus" {
			t.Fatalf("datasourceRef enum constraint lost during proxy round-trip: %#v", dsEnum)
		}
	})

	t.Run("finish_reason=length detected as ErrOutputTruncated", func(t *testing.T) {
		forceFinishReason.Store("length")
		defer forceFinishReason.Store("stop")

		_, meta, err := client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if !errors.Is(err, llm.ErrOutputTruncated) {
			t.Fatalf("expected ErrOutputTruncated, got %v", err)
		}
		if meta.FinishReason != "length" {
			t.Fatalf("expected finishReason=length, got %q", meta.FinishReason)
		}
	})

	t.Run("transient 503 retries at most once, 400 does not retry", func(t *testing.T) {
		vllmCalls.Store(0)
		forceStatus.Store(http.StatusServiceUnavailable)
		_, _, err := client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if err == nil {
			t.Fatal("expected 503 error")
		}
		if got := vllmCalls.Load(); got != 2 {
			t.Fatalf("expected exactly 2 attempts (1 + 1 transport retry) on 503, got %d", got)
		}

		vllmCalls.Store(0)
		forceStatus.Store(http.StatusBadRequest)
		_, _, err = client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if err == nil {
			t.Fatal("expected 400 error")
		}
		if got := vllmCalls.Load(); got != 1 {
			t.Fatalf("expected 1 attempt (no retry) on 400, got %d", got)
		}
		forceStatus.Store(0)
	})

	t.Run("context cancellation propagates immediately without retry", func(t *testing.T) {
		vllmCalls.Store(0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := client.GenerateIR(ctx, llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if got := vllmCalls.Load(); got != 0 {
			t.Fatalf("expected 0 upstream calls for pre-cancelled context, got %d", got)
		}
	})
}

func jsonReader(b []byte) io.Reader {
	return &sliceReader{b: b}
}

type sliceReader struct {
	b []byte
	i int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.i >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.i:])
	s.i += n
	return n, nil
}
