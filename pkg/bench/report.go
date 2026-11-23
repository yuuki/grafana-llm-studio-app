package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// WriteJSON encodes the benchmark report as indented JSON.
func WriteJSON(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// RenderMarkdown formats the benchmark report as GitHub Flavored Markdown suitable
// for issue comments, docs/openai-compatible-apis.md, and docs/compatibility.md.
func RenderMarkdown(report Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# LLM Dashboard Studio Live Benchmark Report: `%s`\n\n", report.ProviderLabel)
	fmt.Fprintf(&b, "- **Timestamp:** %s\n", report.Timestamp.Format(time.RFC3339))
	if report.Preset != "" {
		fmt.Fprintf(&b, "- **Preset:** `%s`\n", report.Preset)
	}
	fmt.Fprintf(&b, "- **Grafana URL:** `%s` (health check: `%v`, %dms)\n", report.GrafanaURL, report.HealthCheckOK, report.HealthCheckMs)
	fmt.Fprintf(&b, "- **Model Tier / Resolved Model:** `%s` / `%s`\n", report.ModelTier, fallbackStr(report.ResolvedModel, "(unresolved)"))
	fmt.Fprintf(&b, "- **Protocol Options:** `structuredOutputMode=%s`, `tokenLimitField=%s`, `reasoningEffort=%q`, `maxOutputTokens=%d`\n",
		report.StructuredOutputMode, report.TokenLimitField, report.ReasoningEffort, report.MaxOutputTokens)
	if report.PrometheusURL != "" {
		fmt.Fprintf(&b, "- **Live Prometheus Verification:** `%s`\n", report.PrometheusURL)
	}
	fmt.Fprintf(&b, "- **Iterations:** %d\n\n", report.Iterations)

	s := report.Summary
	b.WriteString("## Summary\n\n")
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|---|---|\n")
	fmt.Fprintf(&b, "| **Overall Pass Rate** | **%d / %d (%.1f%%)** |\n", s.PassedRuns, s.TotalRuns, s.OverallPassRate)
	fmt.Fprintf(&b, "| Strict JSON Decode Pass Rate | %.1f%% |\n", s.JSONPassRate)
	fmt.Fprintf(&b, "| IR + PromQL Validation Pass Rate | %.1f%% |\n", s.IRPromQLPassRate)
	fmt.Fprintf(&b, "| Prompt Adherence Pass Rate | %.1f%% |\n", s.AdherencePassRate)
	if s.PrometheusCheckedRuns > 0 {
		fmt.Fprintf(&b, "| Live Prometheus Execution Pass Rate | %.1f%% (%d runs) |\n", s.PrometheusPassRate, s.PrometheusCheckedRuns)
	}
	fmt.Fprintf(&b, "| Latency (min / p50 / mean / p95 / max) | %d ms / %d ms / %.1f ms / %d ms / %d ms |\n",
		s.Latency.MinMs, s.Latency.P50Ms, s.Latency.MeanMs, s.Latency.P95Ms, s.Latency.MaxMs)
	fmt.Fprintf(&b, "| Tokens (total prompt / completion / total) | %d / %d / %d (mean %.1f / %.1f) |\n",
		s.Tokens.TotalPromptTokens, s.Tokens.TotalCompletionTokens, s.Tokens.TotalTokens,
		s.Tokens.MeanPromptTokens, s.Tokens.MeanCompletionTokens)
	fmt.Fprintf(&b, "| `finish_reason` Distribution | %s |\n", formatCountMap(s.FinishReasonCounts))
	if len(s.FailureStageCounts) > 0 {
		fmt.Fprintf(&b, "| Failure Stages | %s |\n", formatCountMap(s.FailureStageCounts))
	}
	b.WriteString("\n")

	b.WriteString("## Breakdown by Operation\n\n")
	b.WriteString("| Operation | Passed | Pass Rate | JSON Decode | IR/PromQL | Adherence | p50 Latency | p95 Latency |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, kind := range []ScenarioKind{KindGenerate, KindPatch, KindRepair} {
		ks, ok := s.ByKind[kind]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "| `%s` | %d/%d | %.1f%% | %.1f%% | %.1f%% | %.1f%% | %d ms | %d ms |\n",
			kind, ks.Passed, ks.Total, ks.PassRate, ks.JSONPassRate, ks.IRPromQLPassRate, ks.AdherencePassRate,
			ks.Latency.P50Ms, ks.Latency.P95Ms)
	}
	b.WriteString("\n")

	b.WriteString("## Per-Scenario Results\n\n")
	b.WriteString("| Scenario | Kind | Iter | Result | Latency | Tokens (in/out) | `finish_reason` | PromQL Exec | Notes |\n")
	b.WriteString("|---|---|---:|---|---:|---:|---|---|---|\n")
	for _, r := range report.Results {
		status := "PASS"
		notes := ""
		if !r.Passed {
			status = "FAIL"
			notes = fmt.Sprintf("[%s] %s", r.FailureStage, sanitizeMarkdownCell(r.FailureDetail))
		} else if len(r.Warnings) > 0 {
			notes = fmt.Sprintf("warnings: %d", len(r.Warnings))
		}
		promCell := "-"
		if r.PrometheusChecked {
			if r.PrometheusExecPassed {
				promCell = fmt.Sprintf("OK (%d/%d non-empty)", r.PrometheusNonEmptyPanels, r.PanelCount)
			} else {
				promCell = "FAIL"
			}
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %d | %s | %d ms | %d / %d | `%s` | %s | %s |\n",
			r.ScenarioID,
			r.Kind,
			r.Iteration,
			status,
			r.LatencyMs,
			r.Meta.PromptTokens,
			r.Meta.CompletionTokens,
			fallbackStr(r.Meta.FinishReason, "(none)"),
			promCell,
			notes,
		)
	}
	return b.String()
}

func formatCountMap(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("`%s`: %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func fallbackStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func sanitizeMarkdownCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}
