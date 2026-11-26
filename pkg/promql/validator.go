package promql

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var (
	// Re-exported sentinel errors from pkg/query for backward compatibility.
	ErrMetricNotInCatalog = query.ErrMetricNotInCatalog
	ErrScopeFilterMissing = query.ErrScopeFilterMissing
	ErrPolicyViolation    = query.ErrPolicyViolation

	forbiddenFunctions = map[string]bool{
		"label_replace": true,
		"label_join":    true,
	}

	// promqlParser accepts only stable PromQL: every experimental option of the
	// Prometheus parser (functions, duration expressions, extended range
	// selectors, fill modifiers) stays off, as the parser's former globals were.
	promqlParser = parser.NewParser(parser.Options{})
)

// Policy configures deterministic PromQL static inspection bounds.
// Note: Static inspection alone cannot predict exact series cardinality or query load;
// upstream Prometheus / VictoriaMetrics limits remain the hard enforcement boundary.
type Policy struct {
	MaxRangeDuration    time.Duration
	MaxSubqueryRange    time.Duration
	MinSubqueryStep     time.Duration
	MaxSubqueryDepth    int
	MaxSelectorsPerExpr int
	MaxOffsetDuration   time.Duration
	MaxRegexLength      int
	MaxRegexRepeat      int
	AllowManyToOneJoins bool
	RequireCatalogMatch bool
}

// DefaultPolicy returns conservative defaults.
func DefaultPolicy() Policy {
	return Policy{
		MaxRangeDuration:    1 * time.Hour,
		MaxSubqueryRange:    1 * time.Hour,
		MinSubqueryStep:     15 * time.Second,
		MaxSubqueryDepth:    1,
		MaxSelectorsPerExpr: 4,
		MaxOffsetDuration:   24 * time.Hour,
		MaxRegexLength:      128,
		MaxRegexRepeat:      20,
		AllowManyToOneJoins: false,
		RequireCatalogMatch: true,
	}
}

// ValidationResult is an alias of query.ValidationResult for backward compatibility.
type ValidationResult = query.ValidationResult

// Adapter implements query.Adapter for standard PromQL.
type Adapter struct {
	policy Policy
}

// NewAdapter constructs a PromQL query.Adapter backed by the given static safety policy.
func NewAdapter(policy Policy) *Adapter {
	return &Adapter{policy: policy}
}

func (a *Adapter) Kind() query.Kind {
	return query.KindPromQL
}

func (a *Adapter) DisplayName() string {
	return query.KindPromQL.DisplayName()
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
		"Use standard PromQL only (set optional query.kind to \"promql\" or omit it).",
		"Keep PromQL safe: explicit range selectors <= 1h, no catch-all regex like \".*\", no group_left/group_right joins, no negative offset, no @ modifier, no label_replace/label_join/count_values.",
		"Only reference template variables inside double-quoted label matchers ($name or ${name:regex}), never in single-quoted or backtick strings.",
	}
}

// ValidateScopeFilter validates a user-supplied ScopeFilter to prevent PromQL or prompt injection.
func ValidateScopeFilter(scope *ir.ScopeFilter) error {
	return query.ValidateScopeFilter(scope)
}

// ValidateExpr parses expr using Prometheus's official PromQL parser and enforces
// syntax, static safety policy, AST regex analysis, catalog grounding, type consistency,
// and scope preservation (without dashboard template variables).
func ValidateExpr(
	expr string,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	policy Policy,
) (ValidationResult, error) {
	return ValidateExprWithVariables(expr, catalog, scope, nil, policy)
}

// ValidateExprWithVariables parses expr using Prometheus's official PromQL parser and enforces
// syntax, static safety policy, AST regex analysis, scoped template variable safety,
// catalog grounding, type consistency, and scope preservation.
func ValidateExprWithVariables(
	expr string,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	variables []ir.VariableIR,
	policy Policy,
) (ValidationResult, error) {
	if err := query.ValidateScopeFilter(scope); err != nil {
		return ValidationResult{}, err
	}
	normalizedScope := query.NormalizeScope(scope)

	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return ValidationResult{}, fmt.Errorf("%w: empty PromQL expression", ErrPolicyViolation)
	}
	if !utf8.ValidString(trimmed) || utf8.RuneCountInString(trimmed) > ir.MaxExprLen {
		return ValidationResult{}, fmt.Errorf("%w: expression exceeds %d runes or invalid UTF-8", ErrPolicyViolation, ir.MaxExprLen)
	}

	if err := query.PrescanExpr(trimmed, query.KindPromQL); err != nil {
		return ValidationResult{}, err
	}

	ast, err := promqlParser.ParseExpr(trimmed)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("PromQL syntax error: %w", err)
	}

	catalogByName := query.IndexCatalogFirstWins(catalog)
	varByName := query.IndexVariablesFirstWins(variables)

	var (
		res           ValidationResult
		seen          = make(map[string]bool)
		seenVars      = make(map[string]bool)
		selectorCount = 0
		firstErr      error
		setErr        = func(e error) {
			if firstErr == nil {
				firstErr = e
			}
		}
	)

	parser.Inspect(ast, func(node parser.Node, path []parser.Node) error {
		if firstErr != nil || node == nil {
			return nil
		}

		switch n := node.(type) {
		case *parser.StringLiteral:
			if query.ContainsVarSyntax.MatchString(n.Val) {
				setErr(fmt.Errorf("%w: template variable tokens are forbidden in PromQL string literals (%q)", ErrPolicyViolation, n.Val))
				return nil
			}

		case *parser.AggregateExpr:
			if n.Op == parser.COUNT_VALUES {
				setErr(fmt.Errorf("%w: count_values aggregation is disabled to prevent cardinality explosion", ErrPolicyViolation))
				return nil
			}

		case *parser.Call:
			if n.Func != nil {
				fn := n.Func.Name
				if forbiddenFunctions[fn] {
					setErr(fmt.Errorf("%w: function %s() is disallowed", ErrPolicyViolation, fn))
					return nil
				}
				if fn == "rate" || fn == "irate" || fn == "increase" || fn == "resets" {
					if len(n.Args) > 0 {
						for _, mName := range collectMetricNamesInNode(n.Args[0]) {
							if item, exists := catalogByName[mName]; exists && strings.EqualFold(item.Type, "gauge") {
								setErr(fmt.Errorf("%w: %s() cannot be applied to gauge metric %q", ErrPolicyViolation, fn, mName))
								return nil
							}
						}
					}
				}
			}

		case *parser.MatrixSelector:
			if n.Range <= 0 || (policy.MaxRangeDuration > 0 && n.Range > policy.MaxRangeDuration) {
				setErr(fmt.Errorf("%w: range selector [%s] out of allowed bounds (0, %s]", ErrPolicyViolation, n.Range, policy.MaxRangeDuration))
				return nil
			}

		case *parser.SubqueryExpr:
			subDepth := 1
			for _, ancestor := range path {
				if _, ok := ancestor.(*parser.SubqueryExpr); ok {
					subDepth++
				}
			}
			maxDepth := policy.MaxSubqueryDepth
			if maxDepth <= 0 {
				maxDepth = 1
			}
			if subDepth > maxDepth {
				setErr(fmt.Errorf("%w: nested subquery depth %d exceeds max %d", ErrPolicyViolation, subDepth, maxDepth))
				return nil
			}
			if n.Range <= 0 || (policy.MaxSubqueryRange > 0 && n.Range > policy.MaxSubqueryRange) {
				setErr(fmt.Errorf("%w: subquery range %s out of allowed bounds (0, %s]", ErrPolicyViolation, n.Range, policy.MaxSubqueryRange))
				return nil
			}
			// Require explicit step >= MinSubqueryStep (omitted step == 0 is rejected).
			if policy.MinSubqueryStep > 0 && n.Step < policy.MinSubqueryStep {
				setErr(fmt.Errorf("%w: subquery step must be explicitly >= %s (got %s)", ErrPolicyViolation, policy.MinSubqueryStep, n.Step))
				return nil
			}
			if n.OriginalOffset < 0 || (policy.MaxOffsetDuration > 0 && n.OriginalOffset > policy.MaxOffsetDuration) {
				setErr(fmt.Errorf("%w: subquery offset %s out of allowed bounds [0, %s]", ErrPolicyViolation, n.OriginalOffset, policy.MaxOffsetDuration))
				return nil
			}
			if n.Timestamp != nil || n.StartOrEnd != 0 {
				setErr(fmt.Errorf("%w: @ modifier is not permitted in studio queries", ErrPolicyViolation))
				return nil
			}

		case *parser.BinaryExpr:
			// Allow set operators (and/or/unless), which PromQL parses with CardManyToMany,
			// while still blocking arithmetic/comparison many-to-one / one-to-many / many-to-many joins.
			if !policy.AllowManyToOneJoins && !n.Op.IsSetOperator() && n.VectorMatching != nil {
				if n.VectorMatching.Card == parser.CardManyToOne || n.VectorMatching.Card == parser.CardOneToMany || n.VectorMatching.Card == parser.CardManyToMany {
					setErr(fmt.Errorf("%w: vector join (%s) is disabled to avoid cardinality explosion", ErrPolicyViolation, n.VectorMatching.Card))
					return nil
				}
			}

		case *parser.VectorSelector:
			selectorCount++
			if policy.MaxSelectorsPerExpr > 0 && selectorCount > policy.MaxSelectorsPerExpr {
				setErr(fmt.Errorf("%w: expression references %d selectors (max %d)", ErrPolicyViolation, selectorCount, policy.MaxSelectorsPerExpr))
				return nil
			}
			if n.OriginalOffset < 0 || (policy.MaxOffsetDuration > 0 && n.OriginalOffset > policy.MaxOffsetDuration) {
				setErr(fmt.Errorf("%w: offset %s out of allowed bounds [0, %s]", ErrPolicyViolation, n.OriginalOffset, policy.MaxOffsetDuration))
				return nil
			}
			if n.Timestamp != nil || n.StartOrEnd != 0 {
				setErr(fmt.Errorf("%w: @ modifier is not permitted in studio queries", ErrPolicyViolation))
				return nil
			}

			metricName := resolveMetricName(n)
			if metricName == "" {
				setErr(fmt.Errorf("%w: metric selector must specify an explicit metric name", ErrPolicyViolation))
				return nil
			}
			if query.ContainsVarSyntax.MatchString(metricName) {
				setErr(fmt.Errorf("%w: template variables cannot be used as metric names (%q)", ErrPolicyViolation, metricName))
				return nil
			}
			if !seen[metricName] {
				seen[metricName] = true
				res.ReferencedMetrics = append(res.ReferencedMetrics, metricName)
			}

			var catalogItem ir.MetricCatalogItem
			inCatalog := false
			if policy.RequireCatalogMatch {
				item, ok := catalogByName[metricName]
				if !ok {
					setErr(fmt.Errorf("%w: %q", ErrMetricNotInCatalog, metricName))
					return nil
				}
				catalogItem = item
				inCatalog = true
			}

			scopeMatched := normalizedScope == nil
			allowedLabelSet := make(map[string]bool, len(catalogItem.Labels))
			for _, lbl := range catalogItem.Labels {
				allowedLabelSet[lbl] = true
			}

			for _, m := range n.LabelMatchers {
				if m.Name == model.MetricNameLabel {
					if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp || m.Type == labels.MatchNotEqual {
						setErr(fmt.Errorf("%w: non-equality matching on __name__ is disallowed", ErrPolicyViolation))
						return nil
					}
					if query.ContainsVarSyntax.MatchString(m.Value) {
						setErr(fmt.Errorf("%w: template variables are forbidden in __name__ matcher", ErrPolicyViolation))
						return nil
					}
					continue
				}
				qm := toQueryMatcher(m)
				if qm.Type == query.MatchNotEqual && qm.Value == "" {
					setErr(fmt.Errorf("%w: catch-all negative matcher %s!=\"\" is disallowed", ErrPolicyViolation, qm.Name))
					return nil
				}

				if query.ContainsVarSyntax.MatchString(qm.Value) {
					v, err := query.ValidateVariableMatcher(qm, varByName, policy.MaxRegexLength, policy.MaxRegexRepeat)
					if err != nil {
						setErr(err)
						return nil
					}
					if !seenVars[v.Name] {
						seenVars[v.Name] = true
						res.ReferencedVariables = append(res.ReferencedVariables, v.Name)
					}
				} else {
					if qm.Type == query.MatchRegexp || qm.Type == query.MatchNotRegexp {
						if err := query.ValidateRegexMatcher(qm.Name, qm.Type, qm.Value, policy.MaxRegexLength, policy.MaxRegexRepeat); err != nil {
							setErr(err)
							return nil
						}
					}
					if !scopeMatched && qm.Name == normalizedScope.LabelName && qm.Type == query.MatchEqual && qm.Value == normalizedScope.LabelValue {
						scopeMatched = true
					}
				}

				if inCatalog && len(allowedLabelSet) > 0 && !allowedLabelSet[qm.Name] {
					res.Warnings = append(res.Warnings, fmt.Sprintf("label %q not listed in sampled labels for %q", qm.Name, metricName))
				}
			}

			if !scopeMatched {
				setErr(fmt.Errorf("%w: selector for %q must retain literal %s=%q", ErrScopeFilterMissing, metricName, normalizedScope.LabelName, normalizedScope.LabelValue))
				return nil
			}
		}

		return nil
	})

	if firstErr != nil {
		return ValidationResult{}, firstErr
	}

	if len(res.ReferencedMetrics) == 0 {
		return ValidationResult{}, fmt.Errorf("%w: expression must reference at least one metric", ErrPolicyViolation)
	}
	return res, nil
}

// ValidateDashboardIR runs IR validation plus PromQL validation across all panels,
// multi-query targets, and variable metric references.
func ValidateDashboardIR(
	doc ir.DashboardIR,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	irOpts ir.ValidateOptions,
	promPolicy Policy,
) ([]string, error) {
	return query.ValidateDashboardIR(
		NewAdapter(promPolicy),
		doc,
		catalog,
		scope,
		irOpts,
		query.ValidateOptions{RequireCatalogMatch: promPolicy.RequireCatalogMatch},
	)
}

// InterpolateVariables performs single-pass, injection-safe substitution of declared template variable
// placeholders ($var, ${var}, ${var:regex}, ${var:pipe}) with their default values for raw execution.
func InterpolateVariables(expr string, variables []ir.VariableIR) string {
	return query.InterpolateVariables(expr, variables)
}

func toQueryMatcher(m *labels.Matcher) query.Matcher {
	var mt query.MatchType
	switch m.Type {
	case labels.MatchEqual:
		mt = query.MatchEqual
	case labels.MatchNotEqual:
		mt = query.MatchNotEqual
	case labels.MatchRegexp:
		mt = query.MatchRegexp
	case labels.MatchNotRegexp:
		mt = query.MatchNotRegexp
	}
	return query.Matcher{
		Name:  m.Name,
		Value: m.Value,
		Type:  mt,
	}
}

func collectMetricNamesInNode(node parser.Node) []string {
	var names []string
	parser.Inspect(node, func(n parser.Node, _ []parser.Node) error {
		if vs, ok := n.(*parser.VectorSelector); ok {
			if m := resolveMetricName(vs); m != "" {
				names = append(names, m)
			}
		}
		return nil
	})
	return names
}

func resolveMetricName(vs *parser.VectorSelector) string {
	if vs.Name != "" {
		return vs.Name
	}
	for _, m := range vs.LabelMatchers {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual {
			return m.Value
		}
	}
	return ""
}
