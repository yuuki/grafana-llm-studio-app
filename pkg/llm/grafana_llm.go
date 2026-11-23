package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ReasoningEffort     string // optional: "low", "medium", "high"
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
		httpClient = &http.Client{Timeout: cfg.RequestTimeout}
	}
	provider := llmclient.NewLLMProviderWithClient(cfg.GrafanaURL, cfg.ServiceAccountToken, httpClient)
	return &GrafanaLLMClient{provider: provider, cfg: cfg}, nil
}

// Enabled checks whether grafana-llm-app is enabled and configured in Grafana.
func (c *GrafanaLLMClient) Enabled(ctx context.Context) (bool, error) {
	return c.provider.Enabled(ctx)
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
	req := llmclient.ChatCompletionRequest{
		Model: c.cfg.ModelTier,
		ChatCompletionRequest: openai.ChatCompletionRequest{
			Messages:            messages,
			MaxCompletionTokens: c.cfg.MaxOutputTokens,
			Temperature:         0.1,
			ReasoningEffort:     c.cfg.ReasoningEffort,
			ResponseFormat: &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
				JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
					Name:   schemaName,
					Schema: rawSchemaMarshaler{raw: json.RawMessage(schemaBytes)},
					Strict: true,
				},
			},
		},
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
		resp, lastErr = c.provider.ChatCompletions(ctx, req)
		if lastErr == nil {
			break
		}
		if !isRetryableTransportError(ctx, lastErr) || attempt+1 >= attempts {
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

	if choice.FinishReason == openai.FinishReasonLength {
		return nil, meta, ErrOutputTruncated
	}
	content := strings.TrimSpace(choice.Message.Content)
	if content == "" {
		return nil, meta, errors.New("LLM returned empty content")
	}
	return []byte(content), meta, nil
}

// isRetryableTransportError strictly limits transport retries to transient network errors
// or gateway 502/503/504 responses. It never retries context cancellation, timeouts,
// auth errors (401/403), bad requests (400/422), or semantic/schema errors.
func isRetryableTransportError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.HTTPStatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return !netErr.Timeout()
	}
	return false
}
