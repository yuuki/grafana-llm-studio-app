package metricsql

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	vmql "github.com/VictoriaMetrics/metricsql"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var (
	// Re-exported sentinel errors from pkg/query.
	ErrMetricNotInCatalog = query.ErrMetricNotInCatalog
	ErrScopeFilterMissing = query.ErrScopeFilterMissing
	ErrPolicyViolation    = query.ErrPolicyViolation

	// pureStepUnitPattern matches integer step-relative durations like 1i..10i only.
	pureStepUnitPattern = regexp.MustCompile(`^([1-9][0-9]*)i$`)

	// allowedFuncs is an explicit allowlist of vetted safe MetricsQL rollup and transform functions.
	// Structurally excludes:
	//   - "" (anonymous union `(a, b)`), union
	//   - aggr_over_time (meta-evaluator accepting arbitrary function names in strings)
	//   - label_match / label_mismatch (regex in string arg bypassing label matcher validation)
	//   - all label_* mutators (label_replace, label_join, label_set, label_del, label_keep,
	//     label_copy, label_move, label_map, label_transform, label_lowercase, label_uppercase,
	//     label_value, label_graphite_group, alias, drop_common_labels)
	//   - series-multiplying / cardinality-exploding functions (count_values_over_time,
	//     quantiles_over_time, histogram_over_time, histogram_quantiles, rollup_candlestick,
	//     rollup / rollup_rate / rollup_increase / rollup_delta / rollup_deriv / rollup_scrape_interval)
	//   - built-in recursive macros (ru, ttf)
	allowedFuncs = map[string]bool{
		// Rollup functions (single series per input series)
		"rate":                true,
		"irate":               true,
		"increase":            true,
		"increase_pure":       true,
		"rate_prometheus":     true,
		"increase_prometheus": true,
		"rate_over_sum":       true,
		"delta":               true,
		"idelta":              true,
		"deriv":               true,
		"ideriv":              true,
		"deriv_fast":          true,
		"changes":             true,
		"resets":              true,
		"avg_over_time":       true,
		"min_over_time":       true,
		"max_over_time":       true,
		"sum_over_time":       true,
		"count_over_time":     true,
		"last_over_time":      true,
		"first_over_time":     true,
		"present_over_time":   true,
		"absent_over_time":    true,
		"quantile_over_time":  true,
		"median_over_time":    true,
		"mad_over_time":       true,
		"mode_over_time":      true,
		"geomean_over_time":   true,
		"stddev_over_time":    true,
		"stdvar_over_time":    true,
		"zscore_over_time":    true,
		"share_le_over_time":  true,
		"share_gt_over_time":  true,
		"share_eq_over_time":  true,
		"count_le_over_time":  true,
		"count_gt_over_time":  true,
		"count_eq_over_time":  true,
		"count_ne_over_time":  true,
		"predict_linear":      true,
		"holt_winters":        true,
		"default_rollup":      true,
		"integrate":           true,
		"lag":                 true,
		"lifetime":            true,
		"scrape_interval":     true,
		"tfirst_over_time":    true,
		"tlast_over_time":     true,
		"timestamp":           true,

		// Transform functions (safe single/histogram transforms without label mutation)
		"abs":                true,
		"ceil":               true,
		"floor":              true,
		"round":              true,
		"sqrt":               true,
		"ln":                 true,
		"log2":               true,
		"log10":              true,
		"exp":                true,
		"sgn":                true,
		"clamp":              true,
		"clamp_min":          true,
		"clamp_max":          true,
		"smooth_exponential": true,
		"histogram_quantile": true,
		"histogram_share":    true,
		"histogram_avg":      true,
		"histogram_stddev":   true,
		"histogram_stdvar":   true,
		"buckets_limit":      true,
		"prometheus_buckets": true,
		"absent":             true,
		"vector":             true,
		"scalar":             true,
		"sort":               true,
		"sort_desc":          true,
		"keep_last_value":    true,
		"keep_next_value":    true,
		"interpolate":        true,
		"remove_resets":      true,
		"running_avg":        true,
		"running_max":        true,
		"running_min":        true,
		"running_sum":        true,
		"range_avg":          true,
		"range_max":          true,
		"range_min":          true,
		"range_sum":          true,
		"range_median":       true,
		"range_quantile":     true,
	}

	// allowedAggrFuncs is an explicit allowlist of vetted safe MetricsQL aggregation functions.
	// Structurally excludes count_values, quantiles, and histogram (cardinality/series generators).
	allowedAggrFuncs = map[string]bool{
		"sum":            true,
		"avg":            true,
		"min":            true,
		"max":            true,
		"count":          true,
		"stddev":         true,
		"stdvar":         true,
		"topk":           true,
		"bottomk":        true,
		"quantile":       true,
		"median":         true,
		"geomean":        true,
		"mad":            true,
		"mode":           true,
		"distinct":       true,
		"any":            true,
		"limitk":         true,
		"topk_avg":       true,
		"topk_max":       true,
		"topk_min":       true,
		"topk_last":      true,
		"topk_median":    true,
		"bottomk_avg":    true,
		"bottomk_max":    true,
		"bottomk_min":    true,
		"bottomk_last":   true,
		"bottomk_median": true,
		"share":          true,
		"outliersk":      true,
		"outliers_mad":   true,
	}

	// counterOnlyFuncs must never be applied to gauge metrics present in the scoped catalog.
	counterOnlyFuncs = map[string]bool{
		"rate":                true,
		"irate":               true,
		"increase":            true,
		"resets":              true,
		"increase_pure":       true,
		"rate_prometheus":     true,
		"increase_prometheus": true,
		"rate_over_sum":       true,
		"remove_resets":       true,
	}
)

// Policy configures deterministic MetricsQL static inspection bounds.
// Upstream VictoriaMetrics flags (-search.maxQueryDuration, -search.maxConcurrentRequests,
// -search.maxUniqueTimeseries, -search.maxSeries) remain the final DoS boundary.
type Policy struct {
	MaxRangeDuration    time.Duration
	MaxSubqueryRange    time.Duration
	MinSubqueryStep     time.Duration
	MaxSubqueryDepth    int
	MaxStepMultiplier   int // max allowed integer multiplier for step-relative `i` windows (e.g. 10 -> 1i..10i)
	MaxSelectorsPerExpr int
	MaxOffsetDuration   time.Duration
	MaxRegexLength      int
	MaxRegexRepeat      int
	MaxAggrLimit        int
	MaxASTNodes         int
	MaxASTDepth         int
	AllowManyToOneJoins bool
	RequireCatalogMatch bool
}

// DefaultPolicy returns conservative defaults matching pkg/promql plus MetricsQL-specific guards.
func DefaultPolicy() Policy {
	return Policy{
		MaxRangeDuration:    1 * time.Hour,
		MaxSubqueryRange:    1 * time.Hour,
		MinSubqueryStep:     15 * time.Second,
		MaxSubqueryDepth:    1,
		MaxStepMultiplier:   10,
		MaxSelectorsPerExpr: 4,
		MaxOffsetDuration:   24 * time.Hour,
		MaxRegexLength:      128,
		MaxRegexRepeat:      20,
		MaxAggrLimit:        100,
		MaxASTNodes:         256,
		MaxASTDepth:         32,
		AllowManyToOneJoins: false,
		RequireCatalogMatch: true,
	}
}

// Adapter implements query.Adapter for VictoriaMetrics MetricsQL.
type Adapter struct {
	policy Policy
}

// NewAdapter constructs a MetricsQL query.Adapter backed by the given static safety policy.
func NewAdapter(policy Policy) *Adapter {
	return &Adapter{policy: policy}
}

func (a *Adapter) Kind() query.Kind {
	return query.KindMetricsQL
}

func (a *Adapter) DisplayName() string {
	return query.KindMetricsQL.DisplayName()
}

func (a *Adapter) ValidateScopeFilter(scope *ir.ScopeFilter) error {
	return query.ValidateScopeFilter(scope)
}

func (a *Adapter) ValidateExprWithVariables(
	expr string,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	variables []ir.VariableIR,
	opts query.ValidateOptions,
) (query.ValidationResult, error) {
	p := a.policy
	p.RequireCatalogMatch = opts.RequireCatalogMatch
	return ValidateExprWithVariables(expr, catalog, scope, variables, p)
}

func (a *Adapter) InterpolateVariables(expr string, variables []ir.VariableIR) string {
	return query.InterpolateVariables(expr, variables)
}

func (a *Adapter) PromptRules() []string {
	return []string{
		"Use MetricsQL (VictoriaMetrics) syntax and set query.kind to \"metricsql\".",
		"Prefer explicit bounded range selectors like [5m] (<= 1h) for readability; non-subquery rollups may also use implicit lookback or 1i..10i, while subqueries MUST specify explicit range <= 1h and step >= 15s.",
		"Safe MetricsQL helpers available include share_le_over_time, histogram_share, increase_pure, median_over_time, keep_last_value, smooth_exponential, and conditional operators default / if / ifnot (without group_left/group_right).",
		"Hard restrictions: NEVER use WITH (...) templates, ru(), ttf(), union / tuple (a, b), aggr_over_time, label_* mutating/matching functions, alias, count_values*, quantiles*, histogram_over_time, group_left/group_right, negative offset, @ modifier, or raw $__interval tokens.",
		"If using `or` inside label matchers, EVERY OR-branch MUST independently specify an explicit catalog metric name and retain the requiredScope label matcher.",
		"Only reference template variables inside double-quoted label matchers ($name or ${name:regex}), never in single-quoted or backtick strings.",
	}
}

// ValidateExpr validates a MetricsQL expression without dashboard template variables.
func ValidateExpr(
	expr string,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	policy Policy,
) (query.ValidationResult, error) {
	return ValidateExprWithVariables(expr, catalog, scope, nil, policy)
}

// ValidateExprWithVariables pre-scans and parses expr with VictoriaMetrics's MetricsQL parser,
// then walks the AST recursively to enforce all security, scope, catalog, and bounded-cost rules.
func ValidateExprWithVariables(
	expr string,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	variables []ir.VariableIR,
	policy Policy,
) (query.ValidationResult, error) {
	if err := query.ValidateScopeFilter(scope); err != nil {
		return query.ValidationResult{}, err
	}
	normalizedScope := query.NormalizeScope(scope)

	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return query.ValidationResult{}, fmt.Errorf("%w: empty MetricsQL expression", ErrPolicyViolation)
	}
	if !utf8.ValidString(trimmed) || utf8.RuneCountInString(trimmed) > ir.MaxExprLen {
		return query.ValidationResult{}, fmt.Errorf("%w: expression exceeds %d runes or invalid UTF-8", ErrPolicyViolation, ir.MaxExprLen)
	}

	// Pre-parse lexical scan blocks WITH(...) / ru() / ttf() OOM bombs, single-quote/backtick
	// variable injection, non-matcher variable smuggling, and raw $__interval magic tokens.
	if err := query.PrescanExpr(trimmed, query.KindMetricsQL); err != nil {
		return query.ValidationResult{}, err
	}

	ast, err := vmql.Parse(trimmed)
	if err != nil {
		return query.ValidationResult{}, fmt.Errorf("MetricsQL syntax error: %w", err)
	}
	if vmql.IsLikelyInvalid(ast) {
		return query.ValidationResult{}, fmt.Errorf("%w: MetricsQL expression is structurally invalid (implicit rollup over non-selector expression)", ErrPolicyViolation)
	}

	state := &walkState{
		policy:          policy,
		normalizedScope: normalizedScope,
		catalogByName:   query.IndexCatalogFirstWins(catalog),
		varByName:       query.IndexVariablesFirstWins(variables),
		seenMetrics:     make(map[string]bool),
		seenVars:        make(map[string]bool),
	}

	if err := state.walkExpr(ast, 0, 1); err != nil {
		return query.ValidationResult{}, err
	}

	if len(state.res.ReferencedMetrics) == 0 {
		return query.ValidationResult{}, fmt.Errorf("%w: expression must reference at least one metric", ErrPolicyViolation)
	}
	return state.res, nil
}

// ValidateDashboardIR runs IR validation plus MetricsQL validation across all panels and variables.
func ValidateDashboardIR(
	doc ir.DashboardIR,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	irOpts ir.ValidateOptions,
	policy Policy,
) ([]string, error) {
	return query.ValidateDashboardIR(
		NewAdapter(policy),
		doc,
		catalog,
		scope,
		irOpts,
		query.ValidateOptions{RequireCatalogMatch: policy.RequireCatalogMatch},
	)
}

type walkState struct {
	policy          Policy
	normalizedScope *ir.ScopeFilter
	catalogByName   map[string]ir.MetricCatalogItem
	varByName       map[string]ir.VariableIR
	seenMetrics     map[string]bool
	seenVars        map[string]bool
	selectorCount   int
	nodeCount       int
	res             query.ValidationResult
}

func (s *walkState) walkExpr(e vmql.Expr, subqueryDepth int, astDepth int) error {
	if e == nil {
		return nil
	}
	s.nodeCount++
	maxNodes := s.policy.MaxASTNodes
	if maxNodes <= 0 {
		maxNodes = 256
	}
	if s.nodeCount > maxNodes {
		return fmt.Errorf("%w: AST node count exceeds max %d", ErrPolicyViolation, maxNodes)
	}
	maxDepth := s.policy.MaxASTDepth
	if maxDepth <= 0 {
		maxDepth = 32
	}
	if astDepth > maxDepth {
		return fmt.Errorf("%w: AST depth %d exceeds max %d", ErrPolicyViolation, astDepth, maxDepth)
	}

	switch n := e.(type) {
	case *vmql.NumberExpr:
		return nil

	case *vmql.StringExpr:
		if query.ContainsVarSyntax.MatchString(n.S) {
			return fmt.Errorf("%w: template variable tokens are forbidden in MetricsQL string literals (%q)", ErrPolicyViolation, n.S)
		}
		return nil

	case *vmql.MetricExpr:
		return s.validateMetricExpr(n)

	case *vmql.RollupExpr:
		return s.validateRollupExpr(n, subqueryDepth, astDepth)

	case *vmql.FuncExpr:
		return s.validateFuncExpr(n, subqueryDepth, astDepth)

	case *vmql.AggrFuncExpr:
		return s.validateAggrFuncExpr(n, subqueryDepth, astDepth)

	case *vmql.BinaryOpExpr:
		return s.validateBinaryOpExpr(n, subqueryDepth, astDepth)

	default:
		return fmt.Errorf("%w: unsupported MetricsQL AST node type %T", ErrPolicyViolation, e)
	}
}

func (s *walkState) validateMetricExpr(me *vmql.MetricExpr) error {
	if len(me.LabelFilterss) == 0 {
		return fmt.Errorf("%w: metric selector must specify an explicit metric name", ErrPolicyViolation)
	}

	for branchIdx, lfs := range me.LabelFilterss {
		if len(lfs) == 0 {
			return fmt.Errorf("%w: metric selector branch %d is empty", ErrPolicyViolation, branchIdx)
		}
		s.selectorCount++
		if s.policy.MaxSelectorsPerExpr > 0 && s.selectorCount > s.policy.MaxSelectorsPerExpr {
			return fmt.Errorf("%w: expression references %d selectors (max %d)", ErrPolicyViolation, s.selectorCount, s.policy.MaxSelectorsPerExpr)
		}

		// Reset per-branch state so EVERY OR-branch must independently satisfy metric + scope rules.
		branchMetricName := ""
		metricNameCount := 0

		for _, lf := range lfs {
			if lf.Label == "__name__" {
				if lf.IsNegative || lf.IsRegexp {
					return fmt.Errorf("%w: non-equality matching on __name__ is disallowed", ErrPolicyViolation)
				}
				if query.ContainsVarSyntax.MatchString(lf.Value) {
					return fmt.Errorf("%w: template variables are forbidden in __name__ matcher", ErrPolicyViolation)
				}
				metricNameCount++
				if metricNameCount > 1 {
					return fmt.Errorf("%w: duplicate __name__ matcher in selector is disallowed", ErrPolicyViolation)
				}
				branchMetricName = lf.Value
				continue
			}
			if !query.ValidPromLabelName.MatchString(lf.Label) {
				return fmt.Errorf("%w: invalid label name %q in selector", ErrPolicyViolation, lf.Label)
			}
		}

		if branchMetricName == "" {
			return fmt.Errorf("%w: metric selector must specify an explicit metric name", ErrPolicyViolation)
		}
		if query.ContainsVarSyntax.MatchString(branchMetricName) {
			return fmt.Errorf("%w: template variables cannot be used as metric names (%q)", ErrPolicyViolation, branchMetricName)
		}
		if !s.seenMetrics[branchMetricName] {
			s.seenMetrics[branchMetricName] = true
			s.res.ReferencedMetrics = append(s.res.ReferencedMetrics, branchMetricName)
		}

		var catalogItem ir.MetricCatalogItem
		inCatalog := false
		if s.policy.RequireCatalogMatch {
			item, ok := s.catalogByName[branchMetricName]
			if !ok {
				return fmt.Errorf("%w: %q", ErrMetricNotInCatalog, branchMetricName)
			}
			catalogItem = item
			inCatalog = true
		}

		branchScopeMatched := s.normalizedScope == nil
		allowedLabelSet := make(map[string]bool, len(catalogItem.Labels))
		for _, lbl := range catalogItem.Labels {
			allowedLabelSet[lbl] = true
		}

		for _, lf := range lfs {
			if lf.Label == "__name__" {
				continue
			}
			qm := toQueryMatcher(lf)
			if qm.Type == query.MatchNotEqual && qm.Value == "" {
				return fmt.Errorf("%w: catch-all negative matcher %s!=\"\" is disallowed", ErrPolicyViolation, qm.Name)
			}

			if query.ContainsVarSyntax.MatchString(qm.Value) {
				v, err := query.ValidateVariableMatcher(qm, s.varByName, s.policy.MaxRegexLength, s.policy.MaxRegexRepeat)
				if err != nil {
					return err
				}
				if !s.seenVars[v.Name] {
					s.seenVars[v.Name] = true
					s.res.ReferencedVariables = append(s.res.ReferencedVariables, v.Name)
				}
				if !branchScopeMatched && query.MatcherSatisfiesScopeWithVariable(qm, v, s.normalizedScope) {
					branchScopeMatched = true
				}
			} else {
				if qm.Type == query.MatchRegexp || qm.Type == query.MatchNotRegexp {
					if err := query.ValidateRegexMatcher(qm.Name, qm.Type, qm.Value, s.policy.MaxRegexLength, s.policy.MaxRegexRepeat); err != nil {
						return err
					}
				}
				if !branchScopeMatched && qm.Name == s.normalizedScope.LabelName && qm.Type == query.MatchEqual && qm.Value == s.normalizedScope.LabelValue {
					branchScopeMatched = true
				}
			}

			if inCatalog && len(allowedLabelSet) > 0 && !allowedLabelSet[qm.Name] {
				s.res.Warnings = append(s.res.Warnings, fmt.Sprintf("label %q not listed in sampled labels for %q", qm.Name, branchMetricName))
			}
		}

		if !branchScopeMatched {
			return fmt.Errorf("%w: selector for %q must retain %s=%q (or an anchored single-value $%s variable)", ErrScopeFilterMissing, branchMetricName, s.normalizedScope.LabelName, s.normalizedScope.LabelValue, s.normalizedScope.LabelName)
		}
	}

	return nil
}

func (s *walkState) validateRollupExpr(re *vmql.RollupExpr, subqueryDepth int, astDepth int) error {
	if re.At != nil {
		return fmt.Errorf("%w: @ modifier is not permitted in studio queries", ErrPolicyViolation)
	}
	if re.Offset != nil {
		if err := validateFixedDuration(re.Offset, 0, s.policy.MaxOffsetDuration, true, "offset"); err != nil {
			return err
		}
	}

	if re.ForSubquery() {
		nextSubDepth := subqueryDepth + 1
		maxDepth := s.policy.MaxSubqueryDepth
		if maxDepth <= 0 {
			maxDepth = 1
		}
		if nextSubDepth > maxDepth {
			return fmt.Errorf("%w: nested subquery depth %d exceeds max %d", ErrPolicyViolation, nextSubDepth, maxDepth)
		}
		if re.Window == nil {
			return fmt.Errorf("%w: subquery range must be explicitly specified and <= %s", ErrPolicyViolation, s.policy.MaxSubqueryRange)
		}
		if err := validateFixedDuration(re.Window, 1*time.Millisecond, s.policy.MaxSubqueryRange, false, "subquery range"); err != nil {
			return err
		}
		if re.Step == nil {
			return fmt.Errorf("%w: subquery step must be explicitly >= %s", ErrPolicyViolation, s.policy.MinSubqueryStep)
		}
		if err := validateFixedDuration(re.Step, s.policy.MinSubqueryStep, s.policy.MaxSubqueryRange, false, "subquery step"); err != nil {
			return err
		}
		return s.walkExpr(re.Expr, nextSubDepth, astDepth+1)
	}

	// Non-subquery RollupExpr must wrap a plain MetricExpr directly (block sum(m)[5m] / sum(m) offset 5m).
	if _, ok := re.Expr.(*vmql.MetricExpr); !ok {
		return fmt.Errorf("%w: range selector or offset without subquery step may only apply to a metric selector", ErrPolicyViolation)
	}

	if re.Window != nil {
		if err := validateRollupWindow(re.Window, s.policy.MaxRangeDuration, s.policy.MaxStepMultiplier); err != nil {
			return err
		}
	}

	return s.walkExpr(re.Expr, subqueryDepth, astDepth+1)
}

func (s *walkState) validateFuncExpr(fe *vmql.FuncExpr, subqueryDepth int, astDepth int) error {
	fn := strings.ToLower(fe.Name)
	if fn == "" || !vmql.IsSupportedFunction(fn) || !allowedFuncs[fn] {
		label := fe.Name
		if label == "" {
			label = "(anonymous union)"
		}
		return fmt.Errorf("%w: function %s() is disallowed in studio MetricsQL queries", ErrPolicyViolation, label)
	}

	// For rollup functions, require the rollup target argument to be a direct MetricExpr or RollupExpr
	// so implicit subquery conversions (e.g. timestamp(sum(m))) cannot sneak past subquery bounds.
	if vmql.IsRollupFunc(fn) {
		rollupIdx := vmql.GetRollupArgIdx(fe)
		if rollupIdx >= 0 && rollupIdx < len(fe.Args) {
			arg := fe.Args[rollupIdx]
			switch arg.(type) {
			case *vmql.MetricExpr, *vmql.RollupExpr:
				// Valid: explicit/implicit metric rollup or explicitly bounded subquery RollupExpr.
			default:
				return fmt.Errorf("%w: rollup function %s() requires a metric selector or bounded subquery argument", ErrPolicyViolation, fn)
			}
		}
	}

	if counterOnlyFuncs[fn] && len(fe.Args) > 0 {
		targetArg := fe.Args[0]
		if vmql.IsRollupFunc(fn) {
			if idx := vmql.GetRollupArgIdx(fe); idx >= 0 && idx < len(fe.Args) {
				targetArg = fe.Args[idx]
			}
		}
		for _, mName := range collectMetricNamesInExpr(targetArg) {
			if item, exists := s.catalogByName[mName]; exists && strings.EqualFold(item.Type, "gauge") {
				return fmt.Errorf("%w: %s() cannot be applied to gauge metric %q", ErrPolicyViolation, fn, mName)
			}
		}
	}

	for _, arg := range fe.Args {
		if err := s.walkExpr(arg, subqueryDepth, astDepth+1); err != nil {
			return err
		}
	}
	return nil
}

func (s *walkState) validateAggrFuncExpr(ae *vmql.AggrFuncExpr, subqueryDepth int, astDepth int) error {
	fn := strings.ToLower(ae.Name)
	if fn == "" || !vmql.IsAggrFunc(fn) || !allowedAggrFuncs[fn] {
		return fmt.Errorf("%w: aggregation %s() is disallowed in studio MetricsQL queries", ErrPolicyViolation, ae.Name)
	}
	maxLimit := s.policy.MaxAggrLimit
	if maxLimit <= 0 {
		maxLimit = 100
	}
	if ae.Limit < 0 || ae.Limit > maxLimit {
		return fmt.Errorf("%w: aggregation limit %d out of allowed bounds [0, %d]", ErrPolicyViolation, ae.Limit, maxLimit)
	}
	for _, arg := range ae.Args {
		if err := s.walkExpr(arg, subqueryDepth, astDepth+1); err != nil {
			return err
		}
	}
	return nil
}

func (s *walkState) validateBinaryOpExpr(be *vmql.BinaryOpExpr, subqueryDepth int, astDepth int) error {
	// Unconditionally forbid group_left/group_right regardless of operator (including if/ifnot/default)
	// when AllowManyToOneJoins is false, closing cardinality explosion and group_left(*) label-copy vectors.
	if !s.policy.AllowManyToOneJoins && strings.TrimSpace(be.JoinModifier.Op) != "" {
		return fmt.Errorf("%w: vector join (%s) is disabled to avoid cardinality explosion", ErrPolicyViolation, be.JoinModifier.Op)
	}
	if be.JoinModifierPrefix != nil {
		if err := s.walkExpr(be.JoinModifierPrefix, subqueryDepth, astDepth+1); err != nil {
			return err
		}
	}
	if err := s.walkExpr(be.Left, subqueryDepth, astDepth+1); err != nil {
		return err
	}
	return s.walkExpr(be.Right, subqueryDepth, astDepth+1)
}

// validateFixedDuration rejects step-relative `i` units, compound subtraction (`2h-5m`),
// negative durations, and values outside [minDur, maxDur].
func validateFixedDuration(de *vmql.DurationExpr, minDur, maxDur time.Duration, allowZero bool, label string) error {
	raw := strings.TrimSpace(string(de.AppendString(nil)))
	if raw == "" {
		return fmt.Errorf("%w: empty %s", ErrPolicyViolation, label)
	}
	if strings.Contains(raw, "-") || strings.Contains(raw, "$") {
		return fmt.Errorf("%w: %s %q cannot contain negative or compound subtraction terms", ErrPolicyViolation, label, raw)
	}
	d0 := de.Duration(0)
	d1 := de.Duration(1000)
	if d0 != d1 {
		return fmt.Errorf("%w: step-relative duration %q is not permitted for %s", ErrPolicyViolation, raw, label)
	}
	if d0 < 0 || (!allowZero && d0 == 0) {
		return fmt.Errorf("%w: %s %q must be positive", ErrPolicyViolation, label, raw)
	}
	dur := time.Duration(d0) * time.Millisecond
	if minDur > 0 && dur < minDur {
		return fmt.Errorf("%w: %s %q must be >= %s", ErrPolicyViolation, label, raw, minDur)
	}
	if maxDur > 0 && dur > maxDur {
		return fmt.Errorf("%w: %s %q exceeds max %s", ErrPolicyViolation, label, raw, maxDur)
	}
	return nil
}

// validateRollupWindow allows either pure bounded step-relative integer durations (1i..maxStepMultiplier*i)
// or fixed positive durations in (0, maxDur]. Rejects mixed forms like 1h5i, 0i, or negative/compound forms.
func validateRollupWindow(de *vmql.DurationExpr, maxDur time.Duration, maxStepMultiplier int) error {
	raw := strings.TrimSpace(string(de.AppendString(nil)))
	if raw == "" {
		return fmt.Errorf("%w: empty range selector", ErrPolicyViolation)
	}
	if strings.Contains(raw, "-") || strings.Contains(raw, "$") {
		return fmt.Errorf("%w: range selector [%s] cannot contain negative or compound terms", ErrPolicyViolation, raw)
	}
	if maxStepMultiplier <= 0 {
		maxStepMultiplier = 10
	}
	if m := pureStepUnitPattern.FindStringSubmatch(raw); m != nil {
		mult, err := strconv.Atoi(m[1])
		if err != nil || mult < 1 || mult > maxStepMultiplier {
			return fmt.Errorf("%w: step-relative range [%s] out of allowed bounds [1i, %di]", ErrPolicyViolation, raw, maxStepMultiplier)
		}
		return nil
	}
	// Any other duration must be strictly fixed (zero step-relative component).
	d0 := de.Duration(0)
	d1 := de.Duration(1000)
	if d0 != d1 {
		return fmt.Errorf("%w: mixed or non-integer step-relative range [%s] is disallowed (use 1i..%di or fixed duration)", ErrPolicyViolation, raw, maxStepMultiplier)
	}
	if d0 <= 0 {
		return fmt.Errorf("%w: range selector [%s] must be positive", ErrPolicyViolation, raw)
	}
	dur := time.Duration(d0) * time.Millisecond
	if maxDur > 0 && dur > maxDur {
		return fmt.Errorf("%w: range selector [%s] out of allowed bounds (0, %s]", ErrPolicyViolation, raw, maxDur)
	}
	return nil
}

func toQueryMatcher(lf vmql.LabelFilter) query.Matcher {
	var mt query.MatchType
	switch {
	case lf.IsRegexp && lf.IsNegative:
		mt = query.MatchNotRegexp
	case lf.IsRegexp && !lf.IsNegative:
		mt = query.MatchRegexp
	case !lf.IsRegexp && lf.IsNegative:
		mt = query.MatchNotEqual
	default:
		mt = query.MatchEqual
	}
	return query.Matcher{
		Name:  lf.Label,
		Value: lf.Value,
		Type:  mt,
	}
}

func collectMetricNamesInExpr(e vmql.Expr) []string {
	var names []string
	var walk func(node vmql.Expr)
	walk = func(node vmql.Expr) {
		if node == nil {
			return
		}
		switch n := node.(type) {
		case *vmql.MetricExpr:
			for _, lfs := range n.LabelFilterss {
				for _, lf := range lfs {
					if lf.Label == "__name__" && !lf.IsNegative && !lf.IsRegexp && lf.Value != "" {
						names = append(names, lf.Value)
					}
				}
			}
		case *vmql.RollupExpr:
			walk(n.Expr)
		case *vmql.FuncExpr:
			for _, a := range n.Args {
				walk(a)
			}
		case *vmql.AggrFuncExpr:
			for _, a := range n.Args {
				walk(a)
			}
		case *vmql.BinaryOpExpr:
			walk(n.Left)
			walk(n.Right)
		}
	}
	walk(e)
	return names
}
