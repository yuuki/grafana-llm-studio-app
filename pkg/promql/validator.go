package promql

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
)

var (
	// ErrMetricNotInCatalog indicates a PromQL expression referenced a metric outside the scoped catalog.
	ErrMetricNotInCatalog = errors.New("metric not found in scoped catalog")
	// ErrScopeFilterMissing indicates a PromQL selector omitted the required scope label filter.
	ErrScopeFilterMissing = errors.New("required scope label filter missing")
	// ErrPolicyViolation indicates a PromQL query violated static cost or safety policy.
	ErrPolicyViolation = errors.New("PromQL policy violation")
)

// Policy configures deterministic PromQL static inspection bounds.
// Note: Static inspection alone cannot predict exact series cardinality or query load;
// upstream Prometheus / VictoriaMetrics limits remain the hard enforcement boundary.
type Policy struct {
	MaxRangeDuration    time.Duration
	MaxSubqueryRange    time.Duration
	MinSubqueryStep     time.Duration
	MaxOffsetDuration   time.Duration
	MaxRegexLength      int
	AllowManyToOneJoins bool
	RequireCatalogMatch bool
}

// DefaultPolicy returns conservative MVP defaults.
func DefaultPolicy() Policy {
	return Policy{
		MaxRangeDuration:    1 * time.Hour,
		MaxSubqueryRange:    1 * time.Hour,
		MinSubqueryStep:     15 * time.Second,
		MaxOffsetDuration:   24 * time.Hour,
		MaxRegexLength:      128,
		AllowManyToOneJoins: false,
		RequireCatalogMatch: true,
	}
}

// ValidationResult holds deterministic findings for one PromQL expression.
type ValidationResult struct {
	ReferencedMetrics []string
	Warnings          []string
}

// ValidateExpr parses expr using Prometheus's official PromQL parser and enforces
// syntax, static safety policy, catalog grounding, type consistency, and scope preservation.
func ValidateExpr(
	expr string,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	policy Policy,
) (ValidationResult, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return ValidationResult{}, fmt.Errorf("%w: empty PromQL expression", ErrPolicyViolation)
	}
	if len(trimmed) > ir.MaxExprLen {
		return ValidationResult{}, fmt.Errorf("%w: expression exceeds %d chars", ErrPolicyViolation, ir.MaxExprLen)
	}

	ast, err := parser.ParseExpr(trimmed)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("PromQL syntax error: %w", err)
	}

	catalogByName := make(map[string]ir.MetricCatalogItem, len(catalog))
	for _, item := range catalog {
		catalogByName[item.Name] = item
	}

	var (
		res      ValidationResult
		seen     = make(map[string]bool)
		firstErr error
		setErr   = func(e error) {
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
		case *parser.MatrixSelector:
			if policy.MaxRangeDuration > 0 && n.Range > policy.MaxRangeDuration {
				setErr(fmt.Errorf("%w: range selector [%s] exceeds max %s", ErrPolicyViolation, n.Range, policy.MaxRangeDuration))
				return nil
			}
			if len(path) > 0 {
				if call, ok := path[len(path)-1].(*parser.Call); ok && call.Func != nil {
					fn := call.Func.Name
					if fn == "rate" || fn == "irate" || fn == "increase" {
						if vs, ok := n.VectorSelector.(*parser.VectorSelector); ok {
							mName := resolveMetricName(vs)
							if item, exists := catalogByName[mName]; exists && strings.EqualFold(item.Type, "gauge") {
								setErr(fmt.Errorf("%w: %s() cannot be applied to gauge metric %q", ErrPolicyViolation, fn, mName))
								return nil
							}
						}
					}
				}
			}

		case *parser.SubqueryExpr:
			if policy.MaxSubqueryRange > 0 && n.Range > policy.MaxSubqueryRange {
				setErr(fmt.Errorf("%w: subquery range %s exceeds max %s", ErrPolicyViolation, n.Range, policy.MaxSubqueryRange))
				return nil
			}
			if n.Step > 0 && policy.MinSubqueryStep > 0 && n.Step < policy.MinSubqueryStep {
				setErr(fmt.Errorf("%w: subquery step %s is finer than minimum %s", ErrPolicyViolation, n.Step, policy.MinSubqueryStep))
				return nil
			}
			if policy.MaxOffsetDuration > 0 && n.OriginalOffset > policy.MaxOffsetDuration {
				setErr(fmt.Errorf("%w: subquery offset %s exceeds max %s", ErrPolicyViolation, n.OriginalOffset, policy.MaxOffsetDuration))
				return nil
			}
			if n.Timestamp != nil || n.StartOrEnd != 0 {
				setErr(fmt.Errorf("%w: @ modifier is not permitted in MVP queries", ErrPolicyViolation))
				return nil
			}

		case *parser.BinaryExpr:
			if !policy.AllowManyToOneJoins && n.VectorMatching != nil {
				if n.VectorMatching.Card == parser.CardManyToOne || n.VectorMatching.Card == parser.CardOneToMany || n.VectorMatching.Card == parser.CardManyToMany {
					setErr(fmt.Errorf("%w: vector join (%s) is disabled in MVP to avoid cardinality explosion", ErrPolicyViolation, n.VectorMatching.Card))
					return nil
				}
			}

		case *parser.VectorSelector:
			if policy.MaxOffsetDuration > 0 && n.OriginalOffset > policy.MaxOffsetDuration {
				setErr(fmt.Errorf("%w: offset %s exceeds max %s", ErrPolicyViolation, n.OriginalOffset, policy.MaxOffsetDuration))
				return nil
			}
			if n.Timestamp != nil || n.StartOrEnd != 0 {
				setErr(fmt.Errorf("%w: @ modifier is not permitted in MVP queries", ErrPolicyViolation))
				return nil
			}

			metricName := resolveMetricName(n)
			if metricName == "" {
				setErr(fmt.Errorf("%w: metric selector must specify an explicit metric name", ErrPolicyViolation))
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

			scopeMatched := scope == nil || strings.TrimSpace(scope.LabelName) == "" || strings.TrimSpace(scope.LabelValue) == ""
			allowedLabelSet := make(map[string]bool, len(catalogItem.Labels))
			for _, lbl := range catalogItem.Labels {
				allowedLabelSet[lbl] = true
			}

			for _, m := range n.LabelMatchers {
				if m.Name == labels.MetricName {
					if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp {
						setErr(fmt.Errorf("%w: regex matching on __name__ is disallowed in MVP", ErrPolicyViolation))
						return nil
					}
					continue
				}
				if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp {
					if err := validateRegexMatcher(m.Name, m.Value, policy.MaxRegexLength); err != nil {
						setErr(err)
						return nil
					}
				}
				if !scopeMatched && m.Name == scope.LabelName && m.Type == labels.MatchEqual && m.Value == scope.LabelValue {
					scopeMatched = true
				}
				if inCatalog && len(allowedLabelSet) > 0 && !allowedLabelSet[m.Name] {
					res.Warnings = append(res.Warnings, fmt.Sprintf("label %q not listed in sampled labels for %q", m.Name, metricName))
				}
			}

			if !scopeMatched {
				setErr(fmt.Errorf("%w: selector for %q must retain %s=%q", ErrScopeFilterMissing, metricName, scope.LabelName, scope.LabelValue))
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

// ValidateDashboardIR runs IR validation plus PromQL validation across all panels.
func ValidateDashboardIR(
	doc ir.DashboardIR,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	irOpts ir.ValidateOptions,
	promPolicy Policy,
) ([]string, error) {
	if err := ir.ValidateIR(doc, irOpts); err != nil {
		return nil, err
	}
	var warnings []string
	for _, p := range doc.Panels {
		res, err := ValidateExpr(p.Query.Expr, catalog, scope, promPolicy)
		if err != nil {
			return nil, fmt.Errorf("panel %q: %w", p.ID, err)
		}
		for _, w := range res.Warnings {
			warnings = append(warnings, fmt.Sprintf("panel %s: %s", p.ID, w))
		}
		if strings.Contains(p.Query.Expr, "_seconds") && (p.Unit == "bytes" || p.Unit == "percentunit" || p.Unit == "reqps") {
			warnings = append(warnings, fmt.Sprintf("panel %s: metric appears to measure seconds but unit is %q", p.ID, p.Unit))
		}
	}
	return warnings, nil
}

func resolveMetricName(vs *parser.VectorSelector) string {
	if vs.Name != "" {
		return vs.Name
	}
	for _, m := range vs.LabelMatchers {
		if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
			return m.Value
		}
	}
	return ""
}

func validateRegexMatcher(labelName, pattern string, maxLen int) error {
	if maxLen > 0 && len(pattern) > maxLen {
		return fmt.Errorf("%w: regex on label %q exceeds %d chars", ErrPolicyViolation, labelName, maxLen)
	}
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" || trimmed == ".*" || trimmed == ".+" || trimmed == "^.*$" || trimmed == "^.+$" {
		return fmt.Errorf("%w: unbounded catch-all regex %q on label %q is forbidden", ErrPolicyViolation, pattern, labelName)
	}
	if strings.Contains(trimmed, ".*.*") || strings.Contains(trimmed, ".+.+") || strings.Contains(trimmed, "(.+)+") || strings.Contains(trimmed, "(.*)+") {
		return fmt.Errorf("%w: potentially catastrophic regex %q on label %q is forbidden", ErrPolicyViolation, pattern, labelName)
	}
	return nil
}
