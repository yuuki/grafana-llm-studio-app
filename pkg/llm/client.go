package llm

import (
	"context"
	"errors"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/prompt"
)

var (
	// ErrOutputTruncated indicates finish_reason == "length" and output JSON may be incomplete.
	ErrOutputTruncated = errors.New("LLM output truncated (finish_reason=length)")
	// ErrOutputRefused indicates the LLM refused the request or triggered a content filter.
	ErrOutputRefused = errors.New("LLM refused generation or triggered content filter")
	// ErrInsufficientMetrics indicates no relevant metrics exist in the scoped catalog.
	ErrInsufficientMetrics = errors.New("insufficient matching metrics in scoped catalog")
	// ErrLLMUnavailable indicates grafana-llm-app is disabled or unconfigured.
	ErrLLMUnavailable = errors.New("grafana-llm-app is not enabled or configured")
)

// CompletionMeta captures observability metadata from an LLM call.
type CompletionMeta struct {
	FinishReason     string `json:"finishReason"`
	PromptTokens     int    `json:"promptTokens,omitempty"`
	CompletionTokens int    `json:"completionTokens,omitempty"`
	Model            string `json:"model,omitempty"`
	ProviderMode     string `json:"providerMode"`
}

// GenerateRequest carries inputs for initial DashboardIR generation.
type GenerateRequest struct {
	UserIntent string
	Scope      *ir.ScopeFilter
	Catalog    []ir.MetricCatalogItem
	MaxPanels  int
}

// PatchRequest carries inputs for partial DashboardPatch generation.
type PatchRequest struct {
	CurrentIR     ir.DashboardIR
	TargetPanelID string
	EditIntent    string
	Scope         *ir.ScopeFilter
	Catalog       []ir.MetricCatalogItem
}

// RepairRequest carries inputs for syntax_error repair patch generation.
type RepairRequest struct {
	CurrentIR ir.DashboardIR
	Targets   []prompt.RepairTarget
	Scope     *ir.ScopeFilter
	Catalog   []ir.MetricCatalogItem
}

// Client abstracts LLM inference behind either grafana-llm-app (production) or explicit dev mock.
type Client interface {
	Enabled(ctx context.Context) (bool, error)
	GenerateIR(ctx context.Context, req GenerateRequest) ([]byte, CompletionMeta, error)
	GeneratePatch(ctx context.Context, req PatchRequest) ([]byte, CompletionMeta, error)
	RepairPanels(ctx context.Context, req RepairRequest) ([]byte, CompletionMeta, error)
}
