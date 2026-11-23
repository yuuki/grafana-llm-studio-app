package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// TestGrafanaLLMApp_To_VLLM_Contract verifies the realistic contract:
// Custom Backend (GrafanaLLMClient) -> official llmclient -> Grafana plugin resource API
// (mirroring real grafana-llm-app v1.0.8 error wrapping as HTTP 500 {"error":"..."}) -> local vLLM.
func TestGrafanaLLMApp_To_VLLM_Contract(t *testing.T) {
	var capturedVLLMReq map[string]any
	var vllmCalls atomic.Int32
	var forceStatus atomic.Int32
	var forceFinishReason atomic.Value
	var forceRefusal atomic.Value
	var wrapWithFence atomic.Bool
	forceFinishReason.Store("stop")
	forceRefusal.Store("")

	// 1. Mock local vLLM OpenAI-compatible server.
	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vllmCalls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if code := forceStatus.Load(); code != 0 {
			http.Error(w, fmt.Sprintf(`{"error":{"message":"simulated upstream failure (status %d)"}}`, code), int(code))
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
		content := string(contentBytes)
		if wrapWithFence.Load() {
			content = "```json\n" + content + "\n```"
		}
		resp := openai.ChatCompletionResponse{
			ID:    "chatcmpl-vllm-test",
			Model: "zai-org/GLM-5.3-Flash",
			Choices: []openai.ChatCompletionChoice{
				{
					Index: 0,
					Message: openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: content,
						Refusal: forceRefusal.Load().(string),
					},
					FinishReason: openai.FinishReason(forceFinishReason.Load().(string)),
				},
			},
			Usage: openai.Usage{PromptTokens: 210, CompletionTokens: 95, TotalTokens: 305},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer vllmSrv.Close()

	// 2. Mock Grafana server hosting grafana-llm-app v1.0.8 resource proxy.
	// Faithfully mirrors real grafana-llm-app handleChatCompletions/handleError:
	// unmarshals into openai.ChatCompletionRequest and wraps ALL upstream errors into
	// HTTP 500 {"error":"error, status code: <N>, ..."}!
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
			incoming.ChatCompletionRequest.Model = "zai-org/GLM-5.3-Flash"

			fwdBytes, _ := json.Marshal(incoming.ChatCompletionRequest)
			fwdReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, vllmSrv.URL+"/v1/chat/completions", io.NopCloser(jsonReader(fwdBytes)))
			fwdReq.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(fwdReq)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				// Exact grafana-llm-app v1.0.8 handleError behavior: returns HTTP 500 with string "error".
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": fmt.Sprintf("error, status code: %d, message: upstream error", resp.StatusCode),
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
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

	t.Run("structured output JSON schema (strict=false) and parameters reach vLLM intact", func(t *testing.T) {
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

		if capturedVLLMReq["model"] != "zai-org/GLM-5.3-Flash" {
			t.Fatalf("expected mapped model name, got %v", capturedVLLMReq["model"])
		}
		if capturedVLLMReq["max_completion_tokens"] != float64(1536) {
			t.Fatalf("expected max_completion_tokens=1536, got %v", capturedVLLMReq["max_completion_tokens"])
		}
		if capturedVLLMReq["reasoning_effort"] != "low" {
			t.Fatalf("expected reasoning_effort=low, got %v", capturedVLLMReq["reasoning_effort"])
		}
		// When reasoning_effort is set, temperature should be omitted to avoid model conflicts.
		if _, hasTemp := capturedVLLMReq["temperature"]; hasTemp {
			t.Fatalf("expected temperature to be omitted when reasoning_effort is set, got %v", capturedVLLMReq["temperature"])
		}
		rf, ok := capturedVLLMReq["response_format"].(map[string]any)
		if !ok || rf["type"] != "json_schema" {
			t.Fatalf("response_format.type=json_schema did not reach vLLM: %#v", capturedVLLMReq["response_format"])
		}
		js, ok := rf["json_schema"].(map[string]any)
		if !ok || js["name"] != "dashboard_ir" || js["strict"] == true {
			t.Fatalf("response_format.json_schema contract broken (strict should be false): %#v", rf)
		}
		schemaObj := js["schema"].(map[string]any)
		props := schemaObj["properties"].(map[string]any)
		panelsDef := props["panels"].(map[string]any)
		itemProps := panelsDef["items"].(map[string]any)["properties"].(map[string]any)
		dsEnum := itemProps["datasourceRef"].(map[string]any)["enum"].([]any)
		if len(dsEnum) != 1 || dsEnum[0] != "selected_prometheus" {
			t.Fatalf("datasourceRef enum constraint lost during proxy round-trip: %#v", dsEnum)
		}
	})

	t.Run("strips optional markdown json code fence defensively", func(t *testing.T) {
		wrapWithFence.Store(true)
		defer wrapWithFence.Store(false)
		raw, _, err := client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if err != nil {
			t.Fatalf("GenerateIR with code fence failed: %v", err)
		}
		if _, err := ir.DecodeStrictIR(raw); err != nil {
			t.Fatalf("fenced JSON failed strict decode: %v", err)
		}
	})

	t.Run("finish_reason=length detected as ErrOutputTruncated and Refusal as ErrOutputRefused", func(t *testing.T) {
		forceFinishReason.Store("length")
		_, meta, err := client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		forceFinishReason.Store("stop")
		if !errors.Is(err, llm.ErrOutputTruncated) || meta.FinishReason != "length" {
			t.Fatalf("expected ErrOutputTruncated, got %v (meta=%+v)", err, meta)
		}

		forceRefusal.Store("cannot comply")
		_, _, err = client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		forceRefusal.Store("")
		if !errors.Is(err, llm.ErrOutputRefused) {
			t.Fatalf("expected ErrOutputRefused, got %v", err)
		}
	})

	t.Run("grafana-llm-app HTTP 500 wrapping 503 retries once; wrapped 400 does not retry (H-1)", func(t *testing.T) {
		vllmCalls.Store(0)
		forceStatus.Store(http.StatusServiceUnavailable)
		_, _, err := client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if err == nil {
			t.Fatal("expected wrapped 503 error")
		}
		if got := vllmCalls.Load(); got != 2 {
			t.Fatalf("expected exactly 2 attempts (1 + 1 transport retry) on wrapped 503, got %d", got)
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
			t.Fatal("expected wrapped 400 error")
		}
		if got := vllmCalls.Load(); got != 1 {
			t.Fatalf("expected 1 attempt (no retry) on wrapped 400, got %d", got)
		}
		forceStatus.Store(0)
	})

	t.Run("wrapped HTTP 429 rate limit is NEVER retried (prevents quota burn)", func(t *testing.T) {
		vllmCalls.Store(0)
		forceStatus.Store(http.StatusTooManyRequests)
		_, _, err := client.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		forceStatus.Store(0)
		if err == nil {
			t.Fatal("expected wrapped 429 error")
		}
		if got := vllmCalls.Load(); got != 1 {
			t.Fatalf("expected 1 attempt (no retry) on wrapped 429, got %d", got)
		}
	})

	t.Run("internet OpenAI-compatible mode: omits reasoning_effort when empty, supports json_object and max_tokens", func(t *testing.T) {
		compatClient, err := llm.NewGrafanaLLMClient(llm.GrafanaLLMConfig{
			GrafanaURL:           grafanaSrv.URL,
			ServiceAccountToken:  expectedSAToken,
			ModelTier:            llmclient.ModelBase,
			StructuredOutputMode: llm.StructuredOutputJSONObject,
			TokenLimitField:      llm.TokenLimitMaxTokens,
			MaxOutputTokens:      2048,
			ReasoningEffort:      "",
			RequestTimeout:       5 * time.Second,
			MaxTransportRetries:  1,
		})
		if err != nil {
			t.Fatalf("NewGrafanaLLMClient compat: %v", err)
		}

		capturedVLLMReq = nil
		rawIR, _, err := compatClient.GenerateIR(context.Background(), llm.GenerateRequest{
			UserIntent: "Show request rate",
			Scope:      scope,
			Catalog:    catalog,
			MaxPanels:  6,
		})
		if err != nil {
			t.Fatalf("compat GenerateIR: %v", err)
		}
		if len(rawIR) == 0 {
			t.Fatal("expected non-empty IR bytes")
		}

		if _, exists := capturedVLLMReq["reasoning_effort"]; exists {
			t.Fatalf("expected reasoning_effort to be omitted when empty, got %v", capturedVLLMReq["reasoning_effort"])
		}
		if temp, ok := capturedVLLMReq["temperature"].(float64); !ok || temp <= 0 {
			t.Fatalf("expected temperature to be sent when reasoning_effort is empty, got %v", capturedVLLMReq["temperature"])
		}
		if mt, ok := capturedVLLMReq["max_tokens"].(float64); !ok || int(mt) != 2048 {
			t.Fatalf("expected max_tokens=2048, got %v", capturedVLLMReq["max_tokens"])
		}
		if _, exists := capturedVLLMReq["max_completion_tokens"]; exists {
			t.Fatalf("expected max_completion_tokens to be omitted when tokenLimitField=max_tokens, got %v", capturedVLLMReq["max_completion_tokens"])
		}
		rf, _ := capturedVLLMReq["response_format"].(map[string]any)
		if rf["type"] != "json_object" {
			t.Fatalf("expected response_format.type=json_object, got %v", rf)
		}
		msgs, _ := capturedVLLMReq["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatal("expected messages in request")
		}
		sysMsg, _ := msgs[0].(map[string]any)
		sysContent, _ := sysMsg["content"].(string)
		if sysContent == "" || !json.Valid([]byte(rawIR)) {
			t.Fatal("expected valid JSON output and non-empty system prompt")
		}
		if !containsSubstring(sysContent, "JSON Schema") || !containsSubstring(sysContent, "selected_prometheus") {
			t.Fatalf("expected system prompt in json_object mode to embed JSON Schema, got len=%d", len(sysContent))
		}
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

func containsSubstring(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
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
