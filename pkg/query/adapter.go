package query

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
)

// Kind identifies the query language dialect handled by an Adapter.
type Kind string

const (
	KindPromQL    Kind = "promql"
	KindMetricsQL Kind = "metricsql"
)

var (
	// ErrMetricNotInCatalog indicates a query expression referenced a metric outside the scoped catalog.
	ErrMetricNotInCatalog = errors.New("metric not found in scoped catalog")
	// ErrScopeFilterMissing indicates a query selector omitted the required scope label filter.
	ErrScopeFilterMissing = errors.New("required scope label filter missing")
	// ErrPolicyViolation indicates a query violated static cost or safety policy.
	ErrPolicyViolation = errors.New("query policy violation")
	// ErrUnsupportedKind indicates a requested query dialect has no registered adapter.
	ErrUnsupportedKind = errors.New("unsupported query kind")
)

// IsValidKind returns true when k is a known query dialect.
func IsValidKind(k Kind) bool {
	return k == KindPromQL || k == KindMetricsQL
}

// ParseKind normalizes and validates a raw query kind string.
// An empty input defaults to KindPromQL for backward compatibility.
func ParseKind(raw string) (Kind, error) {
	norm := Kind(strings.ToLower(strings.TrimSpace(raw)))
	if norm == "" {
		return KindPromQL, nil
	}
	if !IsValidKind(norm) {
		return "", fmt.Errorf("%w: %q (allowed: promql, metricsql)", ErrUnsupportedKind, raw)
	}
	return norm, nil
}

// DisplayName returns a human-readable dialect name for error messages and prompts.
func (k Kind) DisplayName() string {
	switch k {
	case KindMetricsQL:
		return "MetricsQL"
	default:
		return "PromQL"
	}
}

// ValidateOptions carries dialect-agnostic runtime validation toggles.
// Dialect-specific static safety bounds live inside each Adapter implementation.
type ValidateOptions struct {
	RequireCatalogMatch bool
}

// ValidationResult holds deterministic findings for one validated query expression.
type ValidationResult struct {
	ReferencedMetrics   []string
	ReferencedVariables []string
	Warnings            []string
}

// Adapter abstracts query parsing, static safety enforcement, variable interpolation,
// and prompt guidance for a single query dialect.
type Adapter interface {
	Kind() Kind
	DisplayName() string
	ValidateScopeFilter(scope *ir.ScopeFilter) error
	ValidateExprWithVariables(
		expr string,
		catalog []ir.MetricCatalogItem,
		scope *ir.ScopeFilter,
		variables []ir.VariableIR,
		opts ValidateOptions,
	) (ValidationResult, error)
	InterpolateVariables(expr string, variables []ir.VariableIR) string
	PromptRules() []string
}

// Registry maps query Kinds to concrete Adapter implementations.
type Registry struct {
	adapters map[Kind]Adapter
}

// NewRegistry constructs an immutable Adapter registry.
func NewRegistry(adapters ...Adapter) *Registry {
	m := make(map[Kind]Adapter, len(adapters))
	for _, a := range adapters {
		if a != nil {
			m[a.Kind()] = a
		}
	}
	return &Registry{adapters: m}
}

// Get resolves an Adapter for the given Kind.
func (r *Registry) Get(kind Kind) (Adapter, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedKind, kind)
	}
	if kind == "" {
		kind = KindPromQL
	}
	a, ok := r.adapters[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedKind, kind)
	}
	return a, nil
}

// ValidateDashboardIR runs IR validation, variable catalog checks, query dialect consistency checks,
// and per-query AST validation via the supplied Adapter.
func ValidateDashboardIR(
	adapter Adapter,
	doc ir.DashboardIR,
	catalog []ir.MetricCatalogItem,
	scope *ir.ScopeFilter,
	irOpts ir.ValidateOptions,
	opts ValidateOptions,
) ([]string, error) {
	if adapter == nil {
		return nil, fmt.Errorf("%w: nil adapter", ErrUnsupportedKind)
	}
	if err := ir.ValidateIR(doc, irOpts); err != nil {
		return nil, err
	}

	var warnings []string

	if err := ValidateScopeFilter(scope); err != nil {
		return nil, err
	}
	normalizedScope := NormalizeScope(scope)

	if normalizedScope != nil && len(doc.Variables) > 0 {
		for _, v := range doc.Variables {
			if v.Type == "query" && v.LabelName == normalizedScope.LabelName {
				return nil, fmt.Errorf("variable %q: %w: query variable cannot target locked scope label %q", v.Name, ErrPolicyViolation, normalizedScope.LabelName)
			}
		}
	}

	if opts.RequireCatalogMatch && len(doc.Variables) > 0 {
		catalogByName := IndexCatalogFirstWins(catalog)
		for _, v := range doc.Variables {
			if v.Type != "query" {
				continue
			}
			if strings.TrimSpace(v.MetricName) == "" {
				return nil, fmt.Errorf("variable %q: %w: query variable requires metricName in scoped catalog", v.Name, ErrPolicyViolation)
			}
			item, ok := catalogByName[v.MetricName]
			if !ok {
				return nil, fmt.Errorf("variable %q: %w: %q", v.Name, ErrMetricNotInCatalog, v.MetricName)
			}
			if len(item.Labels) > 0 {
				foundLbl := false
				for _, lbl := range item.Labels {
					if lbl == v.LabelName {
						foundLbl = true
						break
					}
				}
				if !foundLbl {
					warnings = append(warnings, fmt.Sprintf("variable %s: label %q not listed in sampled labels for %q", v.Name, v.LabelName, v.MetricName))
				}
			}
		}
	}

	expectedKind := string(adapter.Kind())
	for _, p := range doc.Panels {
		queries := p.EffectiveQueries()
		for qi, q := range queries {
			if q.Kind != "" && q.Kind != expectedKind {
				if len(queries) > 1 {
					return nil, fmt.Errorf("panel %q query[%d]: %w: query kind %q does not match active dialect %q", p.ID, qi, ErrPolicyViolation, q.Kind, expectedKind)
				}
				return nil, fmt.Errorf("panel %q: %w: query kind %q does not match active dialect %q", p.ID, ErrPolicyViolation, q.Kind, expectedKind)
			}
			res, err := adapter.ValidateExprWithVariables(q.Expr, catalog, scope, doc.Variables, opts)
			if err != nil {
				if len(queries) > 1 {
					return nil, fmt.Errorf("panel %q query[%d]: %w", p.ID, qi, err)
				}
				return nil, fmt.Errorf("panel %q: %w", p.ID, err)
			}
			for _, w := range res.Warnings {
				warnings = append(warnings, fmt.Sprintf("panel %s: %s", p.ID, w))
			}
			if strings.Contains(q.Expr, "_seconds") && (p.Unit == "bytes" || p.Unit == "percentunit" || p.Unit == "reqps") {
				warnings = append(warnings, fmt.Sprintf("panel %s: metric appears to measure seconds but unit is %q", p.ID, p.Unit))
			}
		}
	}
	return warnings, nil
}
