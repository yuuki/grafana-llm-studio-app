package bench

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grafana/grafana-llm-app/llmclient"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

// ProviderPreset defines recommended GrafanaLLMConfig protocol parameters for a target LLM provider.
// All presets still route strictly through grafana-llm-app (never connecting directly to providers).
type ProviderPreset struct {
	Name                 string
	Description          string
	ModelTier            llmclient.Model
	StructuredOutputMode string
	TokenLimitField      string
	ReasoningEffort      string
	MaxOutputTokens      int
	RequestTimeout       time.Duration
}

var builtinPresets = map[string]ProviderPreset{
	"openai-gpt41-mini": {
		Name:                 "openai-gpt41-mini",
		Description:          "OpenAI GPT-4.1 mini (json_schema, max_completion_tokens, no reasoning_effort)",
		ModelTier:            llmclient.ModelBase,
		StructuredOutputMode: llm.StructuredOutputJSONSchema,
		TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
		ReasoningEffort:      "",
		MaxOutputTokens:      2048,
		RequestTimeout:       45 * time.Second,
	},
	"openai-gpt41": {
		Name:                 "openai-gpt41",
		Description:          "OpenAI GPT-4.1 (large tier, json_schema, max_completion_tokens)",
		ModelTier:            llmclient.ModelLarge,
		StructuredOutputMode: llm.StructuredOutputJSONSchema,
		TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
		ReasoningEffort:      "",
		MaxOutputTokens:      2048,
		RequestTimeout:       60 * time.Second,
	},
	"openai-o3": {
		Name:                 "openai-o3",
		Description:          "OpenAI o3 / GPT-5 reasoning (large tier, json_schema, max_completion_tokens, reasoning_effort=low)",
		ModelTier:            llmclient.ModelLarge,
		StructuredOutputMode: llm.StructuredOutputJSONSchema,
		TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
		ReasoningEffort:      "low",
		MaxOutputTokens:      4096,
		RequestTimeout:       90 * time.Second,
	},
	"openrouter": {
		Name:                 "openrouter",
		Description:          "OpenRouter OpenAI-compatible gateway (json_schema, max_completion_tokens)",
		ModelTier:            llmclient.ModelBase,
		StructuredOutputMode: llm.StructuredOutputJSONSchema,
		TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
		ReasoningEffort:      "",
		MaxOutputTokens:      2048,
		RequestTimeout:       60 * time.Second,
	},
	"deepseek": {
		Name:                 "deepseek",
		Description:          "DeepSeek API (json_object with embedded schema, legacy max_tokens)",
		ModelTier:            llmclient.ModelBase,
		StructuredOutputMode: llm.StructuredOutputJSONObject,
		TokenLimitField:      llm.TokenLimitMaxTokens,
		ReasoningEffort:      "",
		MaxOutputTokens:      2048,
		RequestTimeout:       60 * time.Second,
	},
	"gemini": {
		Name:                 "gemini",
		Description:          "Google Gemini OpenAI-compatible endpoint (json_schema, max_completion_tokens)",
		ModelTier:            llmclient.ModelBase,
		StructuredOutputMode: llm.StructuredOutputJSONSchema,
		TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
		ReasoningEffort:      "",
		MaxOutputTokens:      2048,
		RequestTimeout:       60 * time.Second,
	},
	"vllm": {
		Name:                 "vllm",
		Description:          "Self-hosted local vLLM >= v0.12.0 (XGrammar/Guidance json_schema, max_completion_tokens)",
		ModelTier:            llmclient.ModelBase,
		StructuredOutputMode: llm.StructuredOutputJSONSchema,
		TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
		ReasoningEffort:      "",
		MaxOutputTokens:      2048,
		RequestTimeout:       60 * time.Second,
	},
	"vllm-reasoning": {
		Name:                 "vllm-reasoning",
		Description:          "Self-hosted local vLLM >= v0.12.0 with reasoning model (json_schema, reasoning_effort=low)",
		ModelTier:            llmclient.ModelBase,
		StructuredOutputMode: llm.StructuredOutputJSONSchema,
		TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
		ReasoningEffort:      "low",
		MaxOutputTokens:      4096,
		RequestTimeout:       90 * time.Second,
	},
}

// Presets returns all built-in provider presets sorted by name.
func Presets() []ProviderPreset {
	names := make([]string, 0, len(builtinPresets))
	for name := range builtinPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ProviderPreset, 0, len(names))
	for _, name := range names {
		out = append(out, builtinPresets[name])
	}
	return out
}

// GetPreset looks up a built-in preset by name (case-insensitive).
func GetPreset(name string) (ProviderPreset, bool) {
	p, ok := builtinPresets[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// ApplyPreset populates cfg.LLMConfig (and default ProviderLabel) from a named preset.
func ApplyPreset(name string, cfg *Config) error {
	p, ok := GetPreset(name)
	if !ok {
		valid := make([]string, 0, len(builtinPresets))
		for k := range builtinPresets {
			valid = append(valid, k)
		}
		sort.Strings(valid)
		return fmt.Errorf("unknown preset %q (available: %s)", name, strings.Join(valid, ", "))
	}
	cfg.Preset = p.Name
	if strings.TrimSpace(cfg.ProviderLabel) == "" {
		cfg.ProviderLabel = p.Name
	}
	cfg.LLMConfig.ModelTier = p.ModelTier
	cfg.LLMConfig.StructuredOutputMode = p.StructuredOutputMode
	cfg.LLMConfig.TokenLimitField = p.TokenLimitField
	cfg.LLMConfig.ReasoningEffort = p.ReasoningEffort
	cfg.LLMConfig.MaxOutputTokens = p.MaxOutputTokens
	cfg.LLMConfig.RequestTimeout = p.RequestTimeout
	return nil
}
