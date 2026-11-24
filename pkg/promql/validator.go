package promql

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

	validPromLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

	// containsVarSyntax detects Grafana variable interpolation tokens ($name, ${...}, or legacy [[...]])
	// while ignoring regex end-of-line anchors like ^$ or /api$.
	containsVarSyntax = regexp.MustCompile(`(?:\$(?:\{|[a-zA-Z_])|\[\[[a-zA-Z_])`)

	// exactVarRef matches ONLY a standalone Grafana template variable reference:
	//   $var, ${var}, ${var:regex}, or ${var:pipe}
	exactVarRef = regexp.MustCompile(`^(?:\$([a-zA-Z_][a-zA-Z0-9_]*)|\$\{([a-zA-Z_][a-zA-Z0-9_]*)(?::(regex|pipe))?\})$`)

	// singlePassVarToken finds standalone Grafana template variable tokens in one pass for interpolation.
	singlePassVarToken = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)(?::(regex|pipe))?\}|\$([a-zA-Z_][a-zA-Z0-9_]*)`)

	forbiddenFunctions = map[string]bool{
		"label_replace": true,
		"label_join":    true,
	}
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

// ValidateScopeFilter validates a user-supplied ScopeFilter to prevent PromQL or prompt injection.
func ValidateScopeFilter(scope *ir.ScopeFilter) error {
	if scope == nil {
		return nil
	}
	name := strings.TrimSpace(scope.LabelName)
	val := strings.TrimSpace(scope.LabelValue)
	if name == "" && val == "" {
		return nil
	}
	if !validPromLabelName.MatchString(name) || name == labels.MetricName {
		return fmt.Errorf("%w: invalid scope label name %q", ErrPolicyViolation, scope.LabelName)
	}
	if val == "" || !utf8.ValidString(val) || utf8.RuneCountInString(val) > 128 {
		return fmt.Errorf("%w: scope label value must be valid UTF-8 (1..128 runes)", ErrPolicyViolation)
	}
	for _, r := range val {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: scope label value contains forbidden control character", ErrPolicyViolation)
		}
	}
	return nil
}

// ValidationResult holds deterministic findings for one PromQL expression.
type ValidationResult struct {
	ReferencedMetrics   []string
	ReferencedVariables []string
	Warnings            []string
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
	if err := ValidateScopeFilter(scope); err != nil {
		return ValidationResult{}, err
	}
	var normalizedScope *ir.ScopeFilter
	if scope != nil {
		sName := strings.TrimSpace(scope.LabelName)
		sVal := strings.TrimSpace(scope.LabelValue)
		if sName != "" && sVal != "" {
			normalizedScope = &ir.ScopeFilter{LabelName: sName, LabelValue: sVal}
		}
	}

	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return ValidationResult{}, fmt.Errorf("%w: empty PromQL expression", ErrPolicyViolation)
	}
	if !utf8.ValidString(trimmed) || utf8.RuneCountInString(trimmed) > ir.MaxExprLen {
		return ValidationResult{}, fmt.Errorf("%w: expression exceeds %d runes or invalid UTF-8", ErrPolicyViolation, ir.MaxExprLen)
	}

	ast, err := parser.ParseExpr(trimmed)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("PromQL syntax error: %w", err)
	}

	// First-wins deduplication prevents forged trailing catalog entries from overriding metric types.
	catalogByName := make(map[string]ir.MetricCatalogItem, len(catalog))
	for _, item := range catalog {
		if _, exists := catalogByName[item.Name]; !exists {
			catalogByName[item.Name] = item
		}
	}

	varByName := make(map[string]ir.VariableIR, len(variables))
	for _, v := range variables {
		if _, exists := varByName[v.Name]; !exists {
			varByName[v.Name] = v
		}
	}

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
			if containsVarSyntax.MatchString(n.Val) {
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
			if containsVarSyntax.MatchString(metricName) {
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
				if m.Name == labels.MetricName {
					if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp || m.Type == labels.MatchNotEqual {
						setErr(fmt.Errorf("%w: non-equality matching on __name__ is disallowed", ErrPolicyViolation))
						return nil
					}
					if containsVarSyntax.MatchString(m.Value) {
						setErr(fmt.Errorf("%w: template variables are forbidden in __name__ matcher", ErrPolicyViolation))
						return nil
					}
					continue
				}
				if m.Type == labels.MatchNotEqual && m.Value == "" {
					setErr(fmt.Errorf("%w: catch-all negative matcher %s!=\"\" is disallowed", ErrPolicyViolation, m.Name))
					return nil
				}

				if containsVarSyntax.MatchString(m.Value) {
					v, err := validateVariableMatcher(m, varByName, policy)
					if err != nil {
						setErr(err)
						return nil
					}
					if !seenVars[v.Name] {
						seenVars[v.Name] = true
						res.ReferencedVariables = append(res.ReferencedVariables, v.Name)
					}
					if !scopeMatched && matcherSatisfiesScopeWithVariable(m, v, normalizedScope) {
						scopeMatched = true
					}
				} else {
					if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp {
						if err := validateRegexMatcher(m.Name, m.Type, m.Value, policy.MaxRegexLength, policy.MaxRegexRepeat); err != nil {
							setErr(err)
							return nil
						}
					}
					if !scopeMatched && m.Name == normalizedScope.LabelName && m.Type == labels.MatchEqual && m.Value == normalizedScope.LabelValue {
						scopeMatched = true
					}
				}

				if inCatalog && len(allowedLabelSet) > 0 && !allowedLabelSet[m.Name] {
					res.Warnings = append(res.Warnings, fmt.Sprintf("label %q not listed in sampled labels for %q", m.Name, metricName))
				}
			}

			if !scopeMatched {
				setErr(fmt.Errorf("%w: selector for %q must retain %s=%q (or an anchored single-value $%s variable)", ErrScopeFilterMissing, metricName, normalizedScope.LabelName, normalizedScope.LabelValue, normalizedScope.LabelName))
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

func validateVariableMatcher(
	m *labels.Matcher,
	varByName map[string]ir.VariableIR,
	policy Policy,
) (ir.VariableIR, error) {
	if m.Type == labels.MatchNotEqual || m.Type == labels.MatchNotRegexp {
		return ir.VariableIR{}, fmt.Errorf("%w: template variable in negative matcher %s%s%q is disallowed", ErrPolicyViolation, m.Name, m.Type, m.Value)
	}
	sub := exactVarRef.FindStringSubmatch(m.Value)
	if sub == nil {
		return ir.VariableIR{}, fmt.Errorf("%w: matcher on label %q must use standalone variable reference ($var, ${var}, ${var:regex}, or ${var:pipe}), got %q", ErrPolicyViolation, m.Name, m.Value)
	}
	varName := sub[1]
	if varName == "" {
		varName = sub[2]
	}
	format := sub[3]

	v, exists := varByName[varName]
	if !exists {
		return ir.VariableIR{}, fmt.Errorf("%w: undeclared template variable $%s on label %q", ErrPolicyViolation, varName, m.Name)
	}

	if m.Type == labels.MatchEqual {
		if format != "" {
			return ir.VariableIR{}, fmt.Errorf("%w: format :%s on variable $%s requires =~ matcher", ErrPolicyViolation, format, varName)
		}
		if v.Multi || v.IncludeAll {
			return ir.VariableIR{}, fmt.Errorf("%w: multi/includeAll variable $%s must use =~ matcher instead of =", ErrPolicyViolation, varName)
		}
	}

	if m.Type == labels.MatchRegexp {
		if v.Type == "query" && format != "regex" {
			// Query variables pull live label values (and defaultValue) which may contain regex metacharacters
			// or wildcards; require explicit :regex escaping so raw values cannot smuggle catch-all regexes.
			return ir.VariableIR{}, fmt.Errorf("%w: query variable $%s in =~ matcher must use ${%s:regex} format", ErrPolicyViolation, varName, varName)
		}
		if v.Type == "custom" && format != "regex" {
			// Both raw $var/${var} and ${var:pipe} substitute unescaped custom options into =~;
			// validate every option against regex safety rules to block catch-all patterns like ".*".
			for _, opt := range v.Options {
				if err := validateRegexMatcher(m.Name, labels.MatchRegexp, opt, policy.MaxRegexLength, policy.MaxRegexRepeat); err != nil {
					return ir.VariableIR{}, fmt.Errorf("%w: custom variable $%s option %q violates regex policy: %v", ErrPolicyViolation, varName, opt, err)
				}
			}
		}
	}

	return v, nil
}

func matcherSatisfiesScopeWithVariable(m *labels.Matcher, v ir.VariableIR, scope *ir.ScopeFilter) bool {
	if scope == nil || m.Name != scope.LabelName {
		return false
	}
	// Mandatory scope enforcement requires strict equality (=) on a single-value (non-multi, non-includeAll)
	// variable anchored to the exact required scope value.
	if m.Type != labels.MatchEqual {
		return false
	}
	if v.IncludeAll || v.Multi {
		return false
	}
	if v.DefaultValue != scope.LabelValue {
		return false
	}
	if v.Type == "query" {
		return v.LabelName == scope.LabelName
	}
	if v.Type == "custom" {
		return len(v.Options) == 1 && v.Options[0] == scope.LabelValue
	}
	return false
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
	if err := ir.ValidateIR(doc, irOpts); err != nil {
		return nil, err
	}

	var warnings []string

	if promPolicy.RequireCatalogMatch && len(doc.Variables) > 0 {
		catalogByName := make(map[string]ir.MetricCatalogItem, len(catalog))
		for _, item := range catalog {
			if _, exists := catalogByName[item.Name]; !exists {
				catalogByName[item.Name] = item
			}
		}
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

	for _, p := range doc.Panels {
		queries := p.EffectiveQueries()
		for qi, q := range queries {
			res, err := ValidateExprWithVariables(q.Expr, catalog, scope, doc.Variables, promPolicy)
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

// escapePromQLString escapes a value for safe embedding inside a PromQL double-quoted string literal.
func escapePromQLString(val string) string {
	var b strings.Builder
	b.Grow(len(val))
	for _, r := range val {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// InterpolateVariables performs single-pass, injection-safe substitution of declared template variable
// placeholders ($var, ${var}, ${var:regex}, ${var:pipe}) with their default values for raw Prometheus execution.
// Substituted values are PromQL-string-escaped and never re-scanned.
func InterpolateVariables(expr string, variables []ir.VariableIR) string {
	if len(variables) == 0 || !strings.Contains(expr, "$") {
		return expr
	}
	values := make(map[string]string, len(variables))
	for _, v := range variables {
		val := v.DefaultValue
		if val == "" && v.Type == "custom" && len(v.Options) > 0 {
			val = v.Options[0]
		}
		if val != "" {
			values[v.Name] = val
		}
	}
	if len(values) == 0 {
		return expr
	}
	return singlePassVarToken.ReplaceAllStringFunc(expr, func(match string) string {
		sub := singlePassVarToken.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		varName := sub[1]
		format := sub[2]
		if varName == "" {
			varName = sub[3]
		}
		val, ok := values[varName]
		if !ok {
			return match
		}
		if format == "regex" {
			// Apply RE2 meta escaping first, then PromQL double-quoted string escaping (so '.' -> '\\.').
			return escapePromQLString(regexp.QuoteMeta(val))
		}
		return escapePromQLString(val)
	})
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
		if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
			return m.Value
		}
	}
	return ""
}

// validateRegexMatcher parses the regex AST with regexp/syntax to block catch-all scans,
// nested quantifiers, excessive repetition, and empty-only negative matchers.
func validateRegexMatcher(labelName string, matchType labels.MatchType, pattern string, maxLen, maxRepeat int) error {
	if maxLen > 0 && utf8.RuneCountInString(pattern) > maxLen {
		return fmt.Errorf("%w: regex on label %q exceeds %d chars", ErrPolicyViolation, labelName, maxLen)
	}
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("%w: empty regex on label %q is forbidden", ErrPolicyViolation, labelName)
	}
	if maxRepeat <= 0 {
		maxRepeat = 20
	}

	reAST, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return fmt.Errorf("%w: invalid regex on label %q: %v", ErrPolicyViolation, labelName, err)
	}
	// Check raw AST before Simplify() for excessive repetition or nested quantifiers.
	if err := inspectRegexAST(reAST, labelName, false, maxRepeat); err != nil {
		return err
	}

	// Compile full-anchored regex (matching PromQL semantics) to test degenerate catch-all cases.
	compiled, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return fmt.Errorf("%w: regex compile failed on label %q: %v", ErrPolicyViolation, labelName, err)
	}
	if matchType == labels.MatchRegexp && compiled.MatchString("") {
		return fmt.Errorf("%w: regex %q on label %q matches empty string (unbounded scan)", ErrPolicyViolation, pattern, labelName)
	}
	if matchType == labels.MatchNotRegexp && compiled.MatchString("") && !compiled.MatchString("a") && !compiled.MatchString("0") {
		return fmt.Errorf("%w: negative regex %q on label %q acts as catch-all non-empty matcher", ErrPolicyViolation, pattern, labelName)
	}
	return nil
}

func inspectRegexAST(re *syntax.Regexp, labelName string, inQuantifier bool, maxRepeat int) error {
	if re == nil {
		return nil
	}
	isQuant := re.Op == syntax.OpStar || re.Op == syntax.OpPlus || re.Op == syntax.OpRepeat
	if isQuant {
		if inQuantifier {
			return fmt.Errorf("%w: nested quantifier in regex on label %q is forbidden", ErrPolicyViolation, labelName)
		}
		if re.Op == syntax.OpRepeat {
			if re.Min > maxRepeat || re.Max > maxRepeat {
				return fmt.Errorf("%w: regex repetition bound exceeds %d on label %q", ErrPolicyViolation, maxRepeat, labelName)
			}
		}
		unbounded := re.Op == syntax.OpStar || re.Op == syntax.OpPlus || (re.Op == syntax.OpRepeat && re.Max < 0)
		if unbounded && len(re.Sub) > 0 && (isUniversalCharClass(re.Sub[0]) || isUniversalCharClass(re.Sub[0].Simplify())) {
			return fmt.Errorf("%w: unbounded wildcard quantifier in regex on label %q is forbidden", ErrPolicyViolation, labelName)
		}
	}
	nextInQuant := inQuantifier || isQuant
	for _, sub := range re.Sub {
		if err := inspectRegexAST(sub, labelName, nextInQuant, maxRepeat); err != nil {
			return err
		}
	}
	return nil
}

func isUniversalCharClass(re *syntax.Regexp) bool {
	if re == nil {
		return false
	}
	switch re.Op {
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return true
	case syntax.OpCapture:
		if len(re.Sub) == 1 {
			return isUniversalCharClass(re.Sub[0])
		}
	case syntax.OpCharClass:
		return countRuneSpan(re.Rune) >= 0x100000
	case syntax.OpAlternate:
		// Detect (.|x)+ as well as (\s|\S)+ / (\d|\D)+ universal alternations.
		var merged []rune
		for _, sub := range re.Sub {
			if isUniversalCharClass(sub) {
				return true
			}
			if sub.Op == syntax.OpCapture && len(sub.Sub) == 1 {
				sub = sub.Sub[0]
			}
			if sub.Op == syntax.OpCharClass {
				merged = append(merged, sub.Rune...)
			}
		}
		if len(merged) >= 2 && countMergedRuneSpan(merged) >= 0x100000 {
			return true
		}
	}
	return false
}

func countRuneSpan(ranges []rune) int {
	total := 0
	for i := 0; i+1 < len(ranges); i += 2 {
		total += int(ranges[i+1]-ranges[i]) + 1
	}
	return total
}

func countMergedRuneSpan(ranges []rune) int {
	type interval struct{ lo, hi rune }
	intervals := make([]interval, 0, len(ranges)/2)
	for i := 0; i+1 < len(ranges); i += 2 {
		intervals = append(intervals, interval{lo: ranges[i], hi: ranges[i+1]})
	}
	if len(intervals) == 0 {
		return 0
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].lo != intervals[j].lo {
			return intervals[i].lo < intervals[j].lo
		}
		return intervals[i].hi < intervals[j].hi
	})
	total := 0
	curLo, curHi := intervals[0].lo, intervals[0].hi
	for _, iv := range intervals[1:] {
		if iv.lo <= curHi+1 {
			if iv.hi > curHi {
				curHi = iv.hi
			}
		} else {
			total += int(curHi-curLo) + 1
			curLo, curHi = iv.lo, iv.hi
		}
	}
	total += int(curHi-curLo) + 1
	return total
}
