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

	state := &promqlWalkState{
		policy:          policy,
		catalogByName:   query.IndexCatalogFirstWins(catalog),
		varByName:       query.IndexVariablesFirstWins(variables),
		normalizedScope: normalizedScope,
		seen:            make(map[string]bool),
		seenVars:        make(map[string]bool),
	}

	parser.Inspect(ast, state.inspectNode)

	if state.firstErr != nil {
		return ValidationResult{}, state.firstErr
	}

	if len(state.res.ReferencedMetrics) == 0 {
		return ValidationResult{}, fmt.Errorf("%w: expression must reference at least one metric", ErrPolicyViolation)
	}
	return state.res, nil
}

type promqlWalkState struct {
	policy          Policy
	catalogByName   map[string]ir.MetricCatalogItem
	varByName       map[string]ir.VariableIR
	normalizedScope *ir.ScopeFilter
	res             ValidationResult
	seen            map[string]bool
	seenVars        map[string]bool
	selectorCount   int
	firstErr        error
}

func (s *promqlWalkState) setErr(e error) {
	if s.firstErr == nil && e != nil {
		s.firstErr = e
	}
}

func (s *promqlWalkState) inspectNode(node parser.Node, path []parser.Node) error {
	if s.firstErr != nil || node == nil {
		return nil
	}

	switch n := node.(type) {
	case *parser.StringLiteral:
		if query.ContainsVarSyntax.MatchString(n.Val) {
			s.setErr(fmt.Errorf("%w: template variable tokens are forbidden in PromQL string literals (%q)", ErrPolicyViolation, n.Val))
		}
	case *parser.AggregateExpr:
		if n.Op == parser.COUNT_VALUES {
			s.setErr(fmt.Errorf("%w: count_values aggregation is disabled to prevent cardinality explosion", ErrPolicyViolation))
		}
	case *parser.Call:
		s.setErr(s.validateCall(n))
	case *parser.MatrixSelector:
		if n.Range <= 0 || (s.policy.MaxRangeDuration > 0 && n.Range > s.policy.MaxRangeDuration) {
			s.setErr(fmt.Errorf("%w: range selector [%s] out of allowed bounds (0, %s]", ErrPolicyViolation, n.Range, s.policy.MaxRangeDuration))
		}
	case *parser.SubqueryExpr:
		s.setErr(s.validateSubquery(n, path))
	case *parser.BinaryExpr:
		s.setErr(s.validateBinaryExpr(n))
	case *parser.VectorSelector:
		s.setErr(s.validateVectorSelector(n))
	}
	return nil
}

func (s *promqlWalkState) validateCall(n *parser.Call) error {
	if n.Func == nil {
		return nil
	}
	fn := n.Func.Name
	if forbiddenFunctions[fn] {
		return fmt.Errorf("%w: function %s() is disallowed", ErrPolicyViolation, fn)
	}
	if fn != "rate" && fn != "irate" && fn != "increase" && fn != "resets" {
		return nil
	}
	if len(n.Args) == 0 {
		return nil
	}
	for _, mName := range collectMetricNamesInNode(n.Args[0]) {
		if item, exists := s.catalogByName[mName]; exists && strings.EqualFold(item.Type, "gauge") {
			return fmt.Errorf("%w: %s() cannot be applied to gauge metric %q", ErrPolicyViolation, fn, mName)
		}
	}
	return nil
}

func (s *promqlWalkState) validateSubquery(n *parser.SubqueryExpr, path []parser.Node) error {
	subDepth := 1
	for _, ancestor := range path {
		if _, ok := ancestor.(*parser.SubqueryExpr); ok {
			subDepth++
		}
	}
	maxDepth := s.policy.MaxSubqueryDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if subDepth > maxDepth {
		return fmt.Errorf("%w: nested subquery depth %d exceeds max %d", ErrPolicyViolation, subDepth, maxDepth)
	}
	if n.Range <= 0 || (s.policy.MaxSubqueryRange > 0 && n.Range > s.policy.MaxSubqueryRange) {
		return fmt.Errorf("%w: subquery range %s out of allowed bounds (0, %s]", ErrPolicyViolation, n.Range, s.policy.MaxSubqueryRange)
	}
	// Require explicit step >= MinSubqueryStep (omitted step == 0 is rejected).
	if s.policy.MinSubqueryStep > 0 && n.Step < s.policy.MinSubqueryStep {
		return fmt.Errorf("%w: subquery step must be explicitly >= %s (got %s)", ErrPolicyViolation, s.policy.MinSubqueryStep, n.Step)
	}
	if n.OriginalOffset < 0 || (s.policy.MaxOffsetDuration > 0 && n.OriginalOffset > s.policy.MaxOffsetDuration) {
		return fmt.Errorf("%w: subquery offset %s out of allowed bounds [0, %s]", ErrPolicyViolation, n.OriginalOffset, s.policy.MaxOffsetDuration)
	}
	if n.Timestamp != nil || n.StartOrEnd != 0 {
		return fmt.Errorf("%w: @ modifier is not permitted in studio queries", ErrPolicyViolation)
	}
	return nil
}

func (s *promqlWalkState) validateBinaryExpr(n *parser.BinaryExpr) error {
	// Allow set operators (and/or/unless), which PromQL parses with CardManyToMany,
	// while still blocking arithmetic/comparison many-to-one / one-to-many / many-to-many joins.
	if s.policy.AllowManyToOneJoins || n.Op.IsSetOperator() || n.VectorMatching == nil {
		return nil
	}
	card := n.VectorMatching.Card
	if card == parser.CardManyToOne || card == parser.CardOneToMany || card == parser.CardManyToMany {
		return fmt.Errorf("%w: vector join (%s) is disabled to avoid cardinality explosion", ErrPolicyViolation, card)
	}
	return nil
}

func (s *promqlWalkState) validateVectorSelector(n *parser.VectorSelector) error {
	s.selectorCount++
	if s.policy.MaxSelectorsPerExpr > 0 && s.selectorCount > s.policy.MaxSelectorsPerExpr {
		return fmt.Errorf("%w: expression references %d selectors (max %d)", ErrPolicyViolation, s.selectorCount, s.policy.MaxSelectorsPerExpr)
	}
	if n.OriginalOffset < 0 || (s.policy.MaxOffsetDuration > 0 && n.OriginalOffset > s.policy.MaxOffsetDuration) {
		return fmt.Errorf("%w: offset %s out of allowed bounds [0, %s]", ErrPolicyViolation, n.OriginalOffset, s.policy.MaxOffsetDuration)
	}
	if n.Timestamp != nil || n.StartOrEnd != 0 {
		return fmt.Errorf("%w: @ modifier is not permitted in studio queries", ErrPolicyViolation)
	}

	metricName := resolveMetricName(n)
	if metricName == "" {
		return fmt.Errorf("%w: metric selector must specify an explicit metric name", ErrPolicyViolation)
	}
	if query.ContainsVarSyntax.MatchString(metricName) {
		return fmt.Errorf("%w: template variables cannot be used as metric names (%q)", ErrPolicyViolation, metricName)
	}
	if !s.seen[metricName] {
		s.seen[metricName] = true
		s.res.ReferencedMetrics = append(s.res.ReferencedMetrics, metricName)
	}

	var catalogItem ir.MetricCatalogItem
	inCatalog := false
	if s.policy.RequireCatalogMatch {
		item, ok := s.catalogByName[metricName]
		if !ok {
			return fmt.Errorf("%w: %q", ErrMetricNotInCatalog, metricName)
		}
		catalogItem = item
		inCatalog = true
	}

	return s.validateSelectorMatchers(n.LabelMatchers, metricName, catalogItem, inCatalog)
}

func (s *promqlWalkState) validateSelectorMatchers(
	matchers []*labels.Matcher,
	metricName string,
	catalogItem ir.MetricCatalogItem,
	inCatalog bool,
) error {
	scopeMatched := s.normalizedScope == nil
	allowedLabelSet := make(map[string]bool, len(catalogItem.Labels))
	for _, lbl := range catalogItem.Labels {
		allowedLabelSet[lbl] = true
	}

	for _, m := range matchers {
		if m.Name == model.MetricNameLabel {
			if err := validatePromMetricNameMatcher(m); err != nil {
				return err
			}
			continue
		}
		matched, err := s.validateSingleLabelMatcher(toQueryMatcher(m), metricName, allowedLabelSet, inCatalog)
		if err != nil {
			return err
		}
		if matched {
			scopeMatched = true
		}
	}

	if !scopeMatched {
		return fmt.Errorf("%w: selector for %q must retain literal %s=%q", ErrScopeFilterMissing, metricName, s.normalizedScope.LabelName, s.normalizedScope.LabelValue)
	}
	return nil
}

func validatePromMetricNameMatcher(m *labels.Matcher) error {
	if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp || m.Type == labels.MatchNotEqual {
		return fmt.Errorf("%w: non-equality matching on __name__ is disallowed", ErrPolicyViolation)
	}
	if query.ContainsVarSyntax.MatchString(m.Value) {
		return fmt.Errorf("%w: template variables are forbidden in __name__ matcher", ErrPolicyViolation)
	}
	return nil
}

func (s *promqlWalkState) validateSingleLabelMatcher(
	qm query.Matcher,
	metricName string,
	allowedLabelSet map[string]bool,
	inCatalog bool,
) (bool, error) {
	if qm.Type == query.MatchNotEqual && qm.Value == "" {
		return false, fmt.Errorf("%w: catch-all negative matcher %s!=\"\" is disallowed", ErrPolicyViolation, qm.Name)
	}

	scopeMatched := false
	if query.ContainsVarSyntax.MatchString(qm.Value) {
		v, err := query.ValidateVariableMatcher(qm, s.varByName, s.policy.MaxRegexLength, s.policy.MaxRegexRepeat)
		if err != nil {
			return false, err
		}
		if !s.seenVars[v.Name] {
			s.seenVars[v.Name] = true
			s.res.ReferencedVariables = append(s.res.ReferencedVariables, v.Name)
		}
	} else {
		if qm.Type == query.MatchRegexp || qm.Type == query.MatchNotRegexp {
			if err := query.ValidateRegexMatcher(qm.Name, qm.Type, qm.Value, s.policy.MaxRegexLength, s.policy.MaxRegexRepeat); err != nil {
				return false, err
			}
		}
		if s.normalizedScope != nil && qm.Name == s.normalizedScope.LabelName && qm.Type == query.MatchEqual && qm.Value == s.normalizedScope.LabelValue {
			scopeMatched = true
		}
	}

	if inCatalog && len(allowedLabelSet) > 0 && !allowedLabelSet[qm.Name] {
		s.res.Warnings = append(s.res.Warnings, fmt.Sprintf("label %q not listed in sampled labels for %q", qm.Name, metricName))
	}
	return scopeMatched, nil
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
