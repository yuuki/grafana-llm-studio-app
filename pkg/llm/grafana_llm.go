package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/grafana-llm-app/llmclient"
	"github.com/sashabaranov/go-openai"
	"github.com/yuuki/grafana-llm-studio-app/pkg/prompt"
	"github.com/yuuki/grafana-llm-studio-app/schemas"
)

// GrafanaLLMConfig configures the official grafana-llm-app llmclient adapter.
// All inference traffic MUST go through Grafana's plugin resource API (/api/plugins/grafana-llm-app/resources/llm/v1).
type GrafanaLLMConfig struct {
	GrafanaURL          string
	ServiceAccountToken string
	ModelTier           llmclient.Model // "base" or "large"
	MaxOutputTokens     int
	ReasoningEffort     string // optional: "", "low", "medium", "high"
	RequestTimeout      time.Duration
	MaxTransportRetries int // strictly transport-level retries (max 1), never nested with semantic repair
	HTTPClient          *http.Client
}

// rawSchemaMarshaler satisfies json.Marshaler for go-openai's ChatCompletionResponseFormatJSONSchema.
type rawSchemaMarshaler struct {
	raw json.RawMessage
}

func (r rawSchemaMarshaler) MarshalJSON() ([]byte, error) {
	return r.raw, nil
}

// GrafanaLLMClient routes structured-output completions exclusively through grafana-llm-app.
type GrafanaLLMClient struct {
	provider llmclient.LLMProvider
	cfg      GrafanaLLMConfig
}

// NewGrafanaLLMClient creates a client backed by github.com/grafana/grafana-llm-app/llmclient.
func NewGrafanaLLMClient(cfg GrafanaLLMConfig) (*GrafanaLLMClient, error) {
	if strings.TrimSpace(cfg.GrafanaURL) == "" {
		return nil, errors.New("grafanaURL is required for grafana-llm-app mode")
	}
	if strings.TrimSpace(cfg.ServiceAccountToken) == "" {
		return nil, errors.New("service account token is required for grafana-llm-app mode")
	}
	if cfg.ModelTier != llmclient.ModelBase && cfg.ModelTier != llmclient.ModelLarge {
		cfg.ModelTier = llmclient.ModelBase
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 2048
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 45 * time.Second
	}
	if cfg.MaxTransportRetries < 0 {
		cfg.MaxTransportRetries = 0
	}
	if cfg.MaxTransportRetries > 1 {
		cfg.MaxTransportRetries = 1
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: cfg.RequestTimeout,
			// Never follow HTTP 3xx redirects so Bearer SA tokens cannot be redirected to unintended hosts.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	provider := llmclient.NewLLMProviderWithClient(cfg.GrafanaURL, cfg.ServiceAccountToken, httpClient)
	return &GrafanaLLMClient{provider: provider, cfg: cfg}, nil
}

// Enabled checks whether grafana-llm-app is enabled and configured in Grafana.
func (c *GrafanaLLMClient) Enabled(ctx context.Context) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, minDuration(c.cfg.RequestTimeout, 15*time.Second))
	defer cancel()
	return c.provider.Enabled(checkCtx)
}

func (c *GrafanaLLMClient) GenerateIR(ctx context.Context, req GenerateRequest) ([]byte, CompletionMeta, error) {
	if len(req.Catalog) == 0 {
		return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, ErrInsufficientMetrics
	}
	msgs, err := prompt.BuildGenerateMessages(req.UserIntent, req.Scope, req.Catalog, req.MaxPanels)
	if err != nil {
		return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, err
	}
	return c.callStructured(ctx, msgs, "dashboard_ir", schemas.DashboardIRSchemaJSON)
}

func (c *GrafanaLLMClient) GeneratePatch(ctx context.Context, req PatchRequest) ([]byte, CompletionMeta, error) {
	if len(req.Catalog) == 0 {
		return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, ErrInsufficientMetrics
	}
	msgs, err := prompt.BuildPatchMessages(req.CurrentIR, req.TargetPanelID, req.EditIntent, req.Scope, req.Catalog)
	if err != nil {
		return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, err
	}
	return c.callStructured(ctx, msgs, "dashboard_patch", schemas.DashboardPatchSchemaJSON)
}

func (c *GrafanaLLMClient) RepairPanels(ctx context.Context, req RepairRequest) ([]byte, CompletionMeta, error) {
	msgs, err := prompt.BuildRepairMessages(req.CurrentIR, req.Targets, req.Scope, req.Catalog)
	if err != nil {
		return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, err
	}
	return c.callStructured(ctx, msgs, "dashboard_patch", schemas.DashboardPatchSchemaJSON)
}

func (c *GrafanaLLMClient) callStructured(
	ctx context.Context,
	messages []openai.ChatCompletionMessage,
	schemaName string,
	schemaBytes []byte,
) ([]byte, CompletionMeta, error) {
	chatReq := openai.ChatCompletionRequest{
		Messages:            messages,
		MaxCompletionTokens: c.cfg.MaxOutputTokens,
		ReasoningEffort:     c.cfg.ReasoningEffort,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   schemaName,
				Schema: rawSchemaMarshaler{raw: json.RawMessage(schemaBytes)},
				// Strict is intentionally false because schemas include optional properties and
				// grafana-llm-app's go-openai/jsonschema.Definition cannot represent nullable unions;
				// vLLM still enforces structural JSON Schema + additionalProperties:false + string enums,
				// and Go Backend enforces all remaining bounds deterministically.
				Strict: false,
			},
		},
	}
	// Omit Temperature when ReasoningEffort is explicitly specified to avoid conflicts on reasoning models.
	if c.cfg.ReasoningEffort == "" {
		chatReq.Temperature = 0.1
	}

	req := llmclient.ChatCompletionRequest{
		Model:                 c.cfg.ModelTier,
		ChatCompletionRequest: chatReq,
	}

	var (
		resp    openai.ChatCompletionResponse
		lastErr error
	)
	attempts := 1 + c.cfg.MaxTransportRetries
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, ctx.Err()
		}
		attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		resp, lastErr = c.provider.ChatCompletions(attemptCtx, req)
		attemptTimedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()

		if lastErr == nil {
			break
		}
		if attemptTimedOut || !isRetryableTransportError(ctx, lastErr) || attempt+1 >= attempts {
			return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, fmt.Errorf("grafana-llm-app chat completion failed: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	if len(resp.Choices) == 0 {
		return nil, CompletionMeta{ProviderMode: "grafana-llm-app"}, errors.New("LLM returned zero choices")
	}

	choice := resp.Choices[0]
	meta := CompletionMeta{
		FinishReason:     string(choice.FinishReason),
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		Model:            resp.Model,
		ProviderMode:     "grafana-llm-app",
	}

	if strings.TrimSpace(choice.Message.Refusal) != "" {
		return nil, meta, fmt.Errorf("%w: %s", ErrOutputRefused, choice.Message.Refusal)
	}
	if choice.FinishReason == openai.FinishReasonLength {
		return nil, meta, ErrOutputTruncated
	}
	if choice.FinishReason == openai.FinishReasonContentFilter {
		return nil, meta, ErrOutputRefused
	}
	if choice.FinishReason != "" && choice.FinishReason != openai.FinishReasonStop {
		return nil, meta, fmt.Errorf("unexpected LLM finish_reason %q", choice.FinishReason)
	}

	content := stripJSONCodeFence(strings.TrimSpace(choice.Message.Content))
	if content == "" {
		return nil, meta, errors.New("LLM returned empty content")
	}
	return []byte(content), meta, nil
}

// isRetryableTransportError strictly limits transport retries to transient network errors
// or 502/503/504 responses (including grafana-llm-app v1.0.8 HTTP 500 wrappers containing
// explicit upstream 502/503/504 or transient connection reset errors).
// Never retries context cancellation, timeouts, 4xx errors, or semantic errors.
func isRetryableTransportError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Check *openai.RequestError BEFORE *openai.APIError: when grafana-llm-app returns
	// HTTP 500 {"error":"<string>"}, go-openai wraps an empty *openai.APIError (HTTPStatusCode=0)
	// inside RequestError.Err, so checking APIError first would unwrap and miss RequestError.Body.
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		if isTransientGatewayStatus(reqErr.HTTPStatusCode) {
			return true
		}
		if reqErr.HTTPStatusCode == http.StatusInternalServerError {
			return isWrappedTransient500(string(reqErr.Body) + " " + reqErr.Error())
		}
		return false
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		if isTransientGatewayStatus(apiErr.HTTPStatusCode) {
			return true
		}
		if apiErr.HTTPStatusCode == http.StatusInternalServerError {
			return isWrappedTransient500(apiErr.Message + " " + err.Error())
		}
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return !netErr.Timeout()
	}
	return false
}

func isWrappedTransient500(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "status code: 502") ||
		strings.Contains(lower, "status code: 503") ||
		strings.Contains(lower, "status code: 504") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "unexpected eof")
}

func isTransientGatewayStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

func stripJSONCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	firstNL := strings.IndexByte(s, '\n')
	if firstNL < 0 {
		return s
	}
	rest := strings.TrimSpace(s[firstNL+1:])
	if strings.HasSuffix(rest, "```") {
		return strings.TrimSpace(strings.TrimSuffix(rest, "```"))
	}
	return s
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
