package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/metricsql"
	"github.com/yuuki/grafana-llm-studio-app/pkg/promql"
	"github.com/yuuki/grafana-llm-studio-app/pkg/prompt"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

// Config controls a live integration benchmark run through grafana-llm-app.
type Config struct {
	ProviderLabel  string
	Preset         string
	LLMConfig      llm.GrafanaLLMConfig
	Iterations     int
	ScenarioFilter []string
	MaxPanels      int
	PrometheusURL  string
	HTTPClient     *http.Client
}

// CaseResult captures detailed diagnostics for a single scenario execution.
type CaseResult struct {
	ScenarioID               string             `json:"scenarioId"`
	Kind                     ScenarioKind       `json:"kind"`
	Iteration                int                `json:"iteration"`
	Passed                   bool               `json:"passed"`
	FailureStage             string             `json:"failureStage,omitempty"`
	FailureDetail            string             `json:"failureDetail,omitempty"`
	Warnings                 []string           `json:"warnings,omitempty"`
	LatencyMs                int64              `json:"latencyMs"`
	Meta                     llm.CompletionMeta `json:"meta"`
	JSONValid                bool               `json:"jsonValid"`
	IRPromQLValid            bool               `json:"irPromqlValid"`
	AdherencePassed          bool               `json:"adherencePassed"`
	PanelCount               int                `json:"panelCount"`
	PrometheusChecked        bool               `json:"prometheusChecked"`
	PrometheusExecPassed     bool               `json:"prometheusExecPassed"`
	PrometheusNonEmptyPanels int                `json:"prometheusNonEmptyPanels"`
	GeneratedExprs           []string           `json:"generatedExprs,omitempty"`
}

// LatencyStats summarizes latency distribution in milliseconds.
type LatencyStats struct {
	MinMs  int64   `json:"minMs"`
	MeanMs float64 `json:"meanMs"`
	P50Ms  int64   `json:"p50Ms"`
	P95Ms  int64   `json:"p95Ms"`
	MaxMs  int64   `json:"maxMs"`
}

// TokenStats summarizes prompt and completion token usage.
type TokenStats struct {
	TotalPromptTokens     int     `json:"totalPromptTokens"`
	TotalCompletionTokens int     `json:"totalCompletionTokens"`
	TotalTokens           int     `json:"totalTokens"`
	MeanPromptTokens      float64 `json:"meanPromptTokens"`
	MeanCompletionTokens  float64 `json:"meanCompletionTokens"`
}

// KindSummary aggregates pass rates for one operation kind (generate / patch / repair).
type KindSummary struct {
	Kind              ScenarioKind `json:"kind"`
	Total             int          `json:"total"`
	Passed            int          `json:"passed"`
	PassRate          float64      `json:"passRate"`
	JSONPassRate      float64      `json:"jsonPassRate"`
	IRPromQLPassRate  float64      `json:"irPromqlPassRate"`
	AdherencePassRate float64      `json:"adherencePassRate"`
	Latency           LatencyStats `json:"latency"`
}

// Summary aggregates metrics across all executed benchmark cases.
type Summary struct {
	TotalRuns             int                          `json:"totalRuns"`
	PassedRuns            int                          `json:"passedRuns"`
	OverallPassRate       float64                      `json:"overallPassRate"`
	JSONPassRate          float64                      `json:"jsonPassRate"`
	IRPromQLPassRate      float64                      `json:"irPromqlPassRate"`
	AdherencePassRate     float64                      `json:"adherencePassRate"`
	PrometheusCheckedRuns int                          `json:"prometheusCheckedRuns"`
	PrometheusPassRate    float64                      `json:"prometheusPassRate,omitempty"`
	Latency               LatencyStats                 `json:"latency"`
	Tokens                TokenStats                   `json:"tokens"`
	FinishReasonCounts    map[string]int               `json:"finishReasonCounts"`
	FailureStageCounts    map[string]int               `json:"failureStageCounts,omitempty"`
	ByKind                map[ScenarioKind]KindSummary `json:"byKind"`
}

// Report is the complete, secret-free benchmark output artifact.
type Report struct {
	Timestamp            time.Time    `json:"timestamp"`
	ProviderLabel        string       `json:"providerLabel"`
	Preset               string       `json:"preset,omitempty"`
	GrafanaURL           string       `json:"grafanaUrl"`
	ModelTier            string       `json:"modelTier"`
	ResolvedModel        string       `json:"resolvedModel,omitempty"`
	StructuredOutputMode string       `json:"structuredOutputMode"`
	TokenLimitField      string       `json:"tokenLimitField"`
	ReasoningEffort      string       `json:"reasoningEffort"`
	MaxOutputTokens      int          `json:"maxOutputTokens"`
	Iterations           int          `json:"iterations"`
	HealthCheckOK        bool         `json:"healthCheckOk"`
	HealthCheckMs        int64        `json:"healthCheckMs"`
	HealthCheckError     string       `json:"healthCheckError,omitempty"`
	PrometheusURL        string       `json:"prometheusUrl,omitempty"`
	Summary              Summary      `json:"summary"`
	Results              []CaseResult `json:"results"`
}

// Runner executes benchmark scenarios against an llm.Client (typically GrafanaLLMClient).
type Runner struct {
	cfg        Config
	client     llm.Client
	registry   *query.Registry
	scenarios  []Scenario
	httpClient *http.Client
	nowFunc    func() time.Time
}

// NewRunner creates a benchmark runner backed by llm.NewGrafanaLLMClient.
func NewRunner(cfg Config) (*Runner, error) {
	client, err := llm.NewGrafanaLLMClient(cfg.LLMConfig)
	if err != nil {
		return nil, err
	}
	return NewRunnerWithClient(cfg, client, DefaultScenarios())
}

// NewRunnerWithClient creates a benchmark runner with an injected llm.Client and scenario list.
func NewRunnerWithClient(cfg Config, client llm.Client, scenarios []Scenario) (*Runner, error) {
	if client == nil {
		return nil, errors.New("llm client is required")
	}
	if cfg.Iterations <= 0 {
		cfg.Iterations = 1
	}
	if cfg.MaxPanels <= 0 || cfg.MaxPanels > ir.MaxPanelsHardCap {
		cfg.MaxPanels = ir.DefaultMaxPanels
	}
	if cfg.LLMConfig.StructuredOutputMode == "" {
		cfg.LLMConfig.StructuredOutputMode = llm.StructuredOutputJSONSchema
	}
	if cfg.LLMConfig.TokenLimitField == "" {
		cfg.LLMConfig.TokenLimitField = llm.TokenLimitMaxCompletionTokens
	}
	if cfg.LLMConfig.MaxOutputTokens <= 0 {
		cfg.LLMConfig.MaxOutputTokens = 2048
	}
	if strings.TrimSpace(cfg.ProviderLabel) == "" {
		if cfg.Preset != "" {
			cfg.ProviderLabel = cfg.Preset
		} else {
			cfg.ProviderLabel = "grafana-llm-app"
		}
	}

	filtered, err := filterScenarios(scenarios, cfg.ScenarioFilter)
	if err != nil {
		return nil, err
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}

	return &Runner{
		cfg:    cfg,
		client: client,
		registry: query.NewRegistry(
			promql.NewAdapter(promql.DefaultPolicy()),
			metricsql.NewAdapter(metricsql.DefaultPolicy()),
		),
		scenarios:  filtered,
		httpClient: hc,
		nowFunc:    time.Now,
	}, nil
}

// Run executes the health check and all selected scenarios across configured iterations.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	report := Report{
		Timestamp:            r.nowFunc().UTC(),
		ProviderLabel:        r.cfg.ProviderLabel,
		Preset:               r.cfg.Preset,
		GrafanaURL:           sanitizeURL(r.cfg.LLMConfig.GrafanaURL),
		ModelTier:            string(r.cfg.LLMConfig.ModelTier),
		StructuredOutputMode: r.cfg.LLMConfig.StructuredOutputMode,
		TokenLimitField:      r.cfg.LLMConfig.TokenLimitField,
		ReasoningEffort:      r.cfg.LLMConfig.ReasoningEffort,
		MaxOutputTokens:      r.cfg.LLMConfig.MaxOutputTokens,
		Iterations:           r.cfg.Iterations,
		PrometheusURL:        sanitizeURL(r.cfg.PrometheusURL),
	}
	if report.ModelTier == "" {
		report.ModelTier = "base"
	}

	healthStart := time.Now()
	enabled, err := r.client.Enabled(ctx)
	report.HealthCheckMs = time.Since(healthStart).Milliseconds()
	if err != nil {
		report.HealthCheckOK = false
		report.HealthCheckError = err.Error()
		return report, fmt.Errorf("grafana-llm-app health check failed: %w", err)
	}
	if !enabled {
		report.HealthCheckOK = false
		report.HealthCheckError = "grafana-llm-app is not enabled or no LLM provider is configured"
		return report, llm.ErrLLMUnavailable
	}
	report.HealthCheckOK = true

	results := make([]CaseResult, 0, len(r.scenarios)*r.cfg.Iterations)
	for iter := 1; iter <= r.cfg.Iterations; iter++ {
		for _, sc := range r.scenarios {
			if ctx.Err() != nil {
				report.Results = results
				report.Summary = computeSummary(results)
				return report, ctx.Err()
			}
			res := r.runScenario(ctx, sc, iter)
			if report.ResolvedModel == "" && res.Meta.Model != "" {
				report.ResolvedModel = res.Meta.Model
			}
			results = append(results, res)
		}
	}

	report.Results = results
	report.Summary = computeSummary(results)
	return report, nil
}

func (r *Runner) runScenario(ctx context.Context, sc Scenario, iteration int) CaseResult {
	res := CaseResult{
		ScenarioID: sc.ID,
		Kind:       sc.Kind,
		Iteration:  iteration,
	}

	maxPanels := sc.MaxPanels
	if maxPanels <= 0 || maxPanels > r.cfg.MaxPanels {
		maxPanels = r.cfg.MaxPanels
	}
	safeScope := prompt.SanitizeScope(sc.Scope)
	boundedCatalog := prompt.BoundCatalog(sc.Catalog)
	qKind, kindErr := query.ParseKind(sc.QueryKind)
	if kindErr != nil {
		res.FailureStage = "setup"
		res.FailureDetail = kindErr.Error()
		return res
	}
	adapter, adapterErr := r.registry.Get(qKind)
	if adapterErr != nil {
		res.FailureStage = "setup"
		res.FailureDetail = adapterErr.Error()
		return res
	}

	start := time.Now()
	var (
		raw  []byte
		meta llm.CompletionMeta
		err  error
	)

	switch sc.Kind {
	case KindGenerate:
		raw, meta, err = r.client.GenerateIR(ctx, llm.GenerateRequest{
			UserIntent:   sc.UserIntent,
			Scope:        safeScope,
			Catalog:      boundedCatalog,
			MaxPanels:    maxPanels,
			QueryKind:    string(qKind),
			DialectRules: adapter.PromptRules(),
		})
	case KindPatch:
		if sc.BaseIR == nil {
			res.FailureStage = "setup"
			res.FailureDetail = "patch scenario missing BaseIR"
			return res
		}
		raw, meta, err = r.client.GeneratePatch(ctx, llm.PatchRequest{
			CurrentIR:     sc.BaseIR.Clone(),
			TargetPanelID: sc.TargetPanelID,
			EditIntent:    sc.EditIntent,
			Scope:         safeScope,
			Catalog:       boundedCatalog,
			QueryKind:     string(qKind),
			DialectRules:  adapter.PromptRules(),
		})
	case KindRepair:
		if sc.BaseIR == nil || len(sc.RepairTargets) == 0 {
			res.FailureStage = "setup"
			res.FailureDetail = "repair scenario missing BaseIR or RepairTargets"
			return res
		}
		raw, meta, err = r.client.RepairPanels(ctx, llm.RepairRequest{
			CurrentIR:    sc.BaseIR.Clone(),
			Targets:      sc.RepairTargets,
			Scope:        safeScope,
			Catalog:      boundedCatalog,
			QueryKind:    string(qKind),
			DialectRules: adapter.PromptRules(),
		})
	default:
		res.FailureStage = "setup"
		res.FailureDetail = fmt.Sprintf("unsupported scenario kind %q", sc.Kind)
		return res
	}

	res.LatencyMs = time.Since(start).Milliseconds()
	res.Meta = meta
	if err != nil {
		if errors.Is(err, llm.ErrOutputTruncated) || errors.Is(err, llm.ErrOutputRefused) {
			res.FailureStage = "finish_reason"
		} else {
			res.FailureStage = "llm_call"
		}
		res.FailureDetail = err.Error()
		return res
	}

	var (
		finalDoc ir.DashboardIR
		patchPtr *ir.DashboardPatch
		diffs    []ir.DiffEntry
	)

	switch sc.Kind {
	case KindGenerate:
		doc, decErr := ir.DecodeStrictIR(raw)
		if decErr != nil {
			res.FailureStage = "json_decode"
			res.FailureDetail = decErr.Error()
			return res
		}
		res.JSONValid = true
		doc.Revision = 1
		finalDoc = doc

	case KindPatch:
		patch, decErr := ir.DecodeStrictPatch(raw)
		if decErr != nil {
			res.FailureStage = "json_decode"
			res.FailureDetail = decErr.Error()
			return res
		}
		res.JSONValid = true
		patchPtr = &patch

		if targetID := strings.TrimSpace(sc.TargetPanelID); targetID != "" {
			for _, op := range patch.Operations {
				if op.Op != "update_panel" || op.PanelID != targetID {
					res.FailureStage = "patch_guard"
					res.FailureDetail = fmt.Sprintf("targeted patch (%s) violated by op %s:%s", targetID, op.Op, op.PanelID)
					return res
				}
			}
		}

		nextIR, appliedDiffs, applyErr := ir.ApplyPatch(sc.BaseIR.Clone(), patch, ir.ValidateOptions{MaxPanels: maxPanels})
		if applyErr != nil {
			res.FailureStage = "ir_apply"
			res.FailureDetail = applyErr.Error()
			return res
		}
		finalDoc = nextIR
		diffs = appliedDiffs

	case KindRepair:
		patch, decErr := ir.DecodeStrictPatch(raw)
		if decErr != nil {
			res.FailureStage = "json_decode"
			res.FailureDetail = decErr.Error()
			return res
		}
		res.JSONValid = true
		patchPtr = &patch

		allowedTargetIDs := make(map[string]bool, len(sc.RepairTargets))
		for _, t := range sc.RepairTargets {
			allowedTargetIDs[t.PanelID] = true
		}
		for _, op := range patch.Operations {
			if op.Op != "update_panel" || !allowedTargetIDs[op.PanelID] || op.Panel == nil || (op.Panel.Query == nil && op.Panel.Queries == nil) {
				res.FailureStage = "patch_guard"
				res.FailureDetail = fmt.Sprintf("repair patch contained disallowed op/panel (%s:%s)", op.Op, op.PanelID)
				return res
			}
			p := op.Panel
			if p.DatasourceRef != "" && p.DatasourceRef != ir.SelectedDatasourceRef {
				res.FailureStage = "patch_guard"
				res.FailureDetail = fmt.Sprintf("repair patch attempted to alter datasourceRef to %q", p.DatasourceRef)
				return res
			}
			if p.Type != "" || p.Title != "" || p.Description != nil || p.Unit != "" || p.Thresholds != nil || p.Layout != nil {
				res.FailureStage = "patch_guard"
				res.FailureDetail = "repair patch must modify query only (non-query panel fields were set)"
				return res
			}
		}

		nextIR, appliedDiffs, applyErr := ir.ApplyPatch(sc.BaseIR.Clone(), patch, ir.ValidateOptions{MaxPanels: maxPanels})
		if applyErr != nil {
			res.FailureStage = "ir_apply"
			res.FailureDetail = applyErr.Error()
			return res
		}
		finalDoc = nextIR
		diffs = appliedDiffs
	}

	if qKind != query.KindPromQL {
		finalDoc.StampQueryKind(string(qKind))
	}
	res.PanelCount = len(finalDoc.Panels)
	res.GeneratedExprs = make([]string, 0, len(finalDoc.Panels))
	for _, p := range finalDoc.Panels {
		qs := p.EffectiveQueries()
		for qi, q := range qs {
			if len(qs) == 1 {
				res.GeneratedExprs = append(res.GeneratedExprs, fmt.Sprintf("%s: %s", p.ID, q.Expr))
			} else {
				res.GeneratedExprs = append(res.GeneratedExprs, fmt.Sprintf("%s[%d]: %s", p.ID, qi, q.Expr))
			}
		}
	}

	warnings, valErr := query.ValidateDashboardIR(
		adapter,
		finalDoc,
		boundedCatalog,
		safeScope,
		ir.ValidateOptions{MaxPanels: maxPanels},
		query.ValidateOptions{RequireCatalogMatch: true},
	)
	if valErr != nil {
		res.FailureStage = "ir_promql_validate"
		res.FailureDetail = valErr.Error()
		return res
	}
	res.IRPromQLValid = true
	res.Warnings = warnings

	if sc.CheckAdherence != nil {
		if adhErr := sc.CheckAdherence(finalDoc, patchPtr, diffs); adhErr != nil {
			res.FailureStage = "adherence"
			res.FailureDetail = adhErr.Error()
			return res
		}
	}
	res.AdherencePassed = true

	if strings.TrimSpace(r.cfg.PrometheusURL) != "" {
		res.PrometheusChecked = true
		nonEmpty, execErr := r.validateAgainstPrometheus(ctx, finalDoc, adapter)
		res.PrometheusNonEmptyPanels = nonEmpty
		if execErr != nil {
			res.FailureStage = "prometheus_exec"
			res.FailureDetail = execErr.Error()
			return res
		}
		res.PrometheusExecPassed = true
	}

	res.Passed = true
	return res
}

type promRangeResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values []any `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (r *Runner) validateAgainstPrometheus(ctx context.Context, doc ir.DashboardIR, adapter query.Adapter) (int, error) {
	base := strings.TrimRight(strings.TrimSpace(r.cfg.PrometheusURL), "/")
	now := r.nowFunc().UTC()
	start := now.Add(-5 * time.Minute)
	nonEmpty := 0

	for _, p := range doc.Panels {
		panelHasSeries := false
		for _, qry := range p.EffectiveQueries() {
			expr := adapter.InterpolateVariables(qry.Expr, doc.Variables)
			q := url.Values{}
			q.Set("query", expr)
			q.Set("start", strconv.FormatInt(start.Unix(), 10))
			q.Set("end", strconv.FormatInt(now.Unix(), 10))
			q.Set("step", "30s")

			reqURL := base + "/api/v1/query_range?" + q.Encode()
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			if err != nil {
				return nonEmpty, fmt.Errorf("panel %s: build prometheus request: %w", p.ID, err)
			}
			resp, err := r.httpClient.Do(httpReq)
			if err != nil {
				return nonEmpty, fmt.Errorf("panel %s: prometheus query failed: %w", p.ID, err)
			}
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				return nonEmpty, fmt.Errorf("panel %s: read prometheus response: %w", p.ID, readErr)
			}

			var pr promRangeResponse
			if err := json.Unmarshal(body, &pr); err != nil {
				return nonEmpty, fmt.Errorf("panel %s: decode prometheus HTTP %d response: %w", p.ID, resp.StatusCode, err)
			}
			if resp.StatusCode != http.StatusOK || pr.Status != "success" {
				return nonEmpty, fmt.Errorf("panel %s: prometheus %s (%d): %s", p.ID, pr.ErrorType, resp.StatusCode, pr.Error)
			}
			for _, s := range pr.Data.Result {
				if len(s.Values) > 0 {
					panelHasSeries = true
					break
				}
			}
		}
		if panelHasSeries {
			nonEmpty++
		}
	}
	return nonEmpty, nil
}

func filterScenarios(all []Scenario, filter []string) ([]Scenario, error) {
	if len(filter) == 0 {
		out := make([]Scenario, len(all))
		copy(out, all)
		return out, nil
	}
	want := make(map[string]bool, len(filter))
	for _, f := range filter {
		trimmed := strings.ToLower(strings.TrimSpace(f))
		if trimmed != "" {
			want[trimmed] = true
		}
	}
	if len(want) == 0 {
		out := make([]Scenario, len(all))
		copy(out, all)
		return out, nil
	}

	var matched []Scenario
	for _, sc := range all {
		if want[strings.ToLower(sc.ID)] || want[strings.ToLower(string(sc.Kind))] {
			matched = append(matched, sc)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("scenario filter %v matched zero scenarios", filter)
	}
	return matched, nil
}

func computeSummary(results []CaseResult) Summary {
	s := Summary{
		TotalRuns:          len(results),
		FinishReasonCounts: make(map[string]int),
		FailureStageCounts: make(map[string]int),
		ByKind:             make(map[ScenarioKind]KindSummary),
	}
	if len(results) == 0 {
		return s
	}

	var (
		latencies       = make([]int64, 0, len(results))
		jsonOK          = 0
		irPromQLOK      = 0
		adherenceOK     = 0
		promChecked     = 0
		promOK          = 0
		totalPrompt     = 0
		totalCompletion = 0
		byKindResults   = make(map[ScenarioKind][]CaseResult)
	)

	for _, r := range results {
		latencies = append(latencies, r.LatencyMs)
		if r.Passed {
			s.PassedRuns++
		}
		if r.JSONValid {
			jsonOK++
		}
		if r.IRPromQLValid {
			irPromQLOK++
		}
		if r.AdherencePassed {
			adherenceOK++
		}
		if r.PrometheusChecked {
			promChecked++
			if r.PrometheusExecPassed {
				promOK++
			}
		}
		totalPrompt += r.Meta.PromptTokens
		totalCompletion += r.Meta.CompletionTokens

		fr := strings.TrimSpace(r.Meta.FinishReason)
		if fr == "" {
			if r.FailureStage == "llm_call" {
				fr = "(error)"
			} else {
				fr = "(empty)"
			}
		}
		s.FinishReasonCounts[fr]++
		if r.FailureStage != "" {
			s.FailureStageCounts[r.FailureStage]++
		}
		byKindResults[r.Kind] = append(byKindResults[r.Kind], r)
	}

	n := float64(len(results))
	s.OverallPassRate = pct(s.PassedRuns, len(results))
	s.JSONPassRate = pct(jsonOK, len(results))
	s.IRPromQLPassRate = pct(irPromQLOK, len(results))
	s.AdherencePassRate = pct(adherenceOK, len(results))
	s.PrometheusCheckedRuns = promChecked
	if promChecked > 0 {
		s.PrometheusPassRate = pct(promOK, promChecked)
	}
	s.Latency = calcLatencyStats(latencies)
	s.Tokens = TokenStats{
		TotalPromptTokens:     totalPrompt,
		TotalCompletionTokens: totalCompletion,
		TotalTokens:           totalPrompt + totalCompletion,
		MeanPromptTokens:      round1(float64(totalPrompt) / n),
		MeanCompletionTokens:  round1(float64(totalCompletion) / n),
	}

	for kind, list := range byKindResults {
		ks := KindSummary{
			Kind:  kind,
			Total: len(list),
		}
		kLat := make([]int64, 0, len(list))
		kJSON, kIR, kAdh := 0, 0, 0
		for _, r := range list {
			kLat = append(kLat, r.LatencyMs)
			if r.Passed {
				ks.Passed++
			}
			if r.JSONValid {
				kJSON++
			}
			if r.IRPromQLValid {
				kIR++
			}
			if r.AdherencePassed {
				kAdh++
			}
		}
		ks.PassRate = pct(ks.Passed, len(list))
		ks.JSONPassRate = pct(kJSON, len(list))
		ks.IRPromQLPassRate = pct(kIR, len(list))
		ks.AdherencePassRate = pct(kAdh, len(list))
		ks.Latency = calcLatencyStats(kLat)
		s.ByKind[kind] = ks
	}
	if len(s.FailureStageCounts) == 0 {
		s.FailureStageCounts = nil
	}
	return s
}

func calcLatencyStats(vals []int64) LatencyStats {
	if len(vals) == 0 {
		return LatencyStats{}
	}
	sorted := make([]int64, len(vals))
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return LatencyStats{
		MinMs:  sorted[0],
		MeanMs: round1(float64(sum) / float64(len(sorted))),
		P50Ms:  percentile(sorted, 50),
		P95Ms:  percentile(sorted, 95),
		MaxMs:  sorted[len(sorted)-1],
	}
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil((p/100.0)*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func pct(num, den int) float64 {
	if den <= 0 {
		return 0
	}
	return round1((float64(num) / float64(den)) * 100.0)
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func sanitizeURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	u.User = nil
	return u.String()
}
