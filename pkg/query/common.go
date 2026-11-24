package query

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
)

// MatchType is a parser-agnostic label matcher operator.
type MatchType int

const (
	MatchEqual MatchType = iota
	MatchNotEqual
	MatchRegexp
	MatchNotRegexp
)

func (m MatchType) String() string {
	switch m {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	default:
		return "?"
	}
}

// Matcher is a parser-agnostic label matcher used by shared security helpers.
type Matcher struct {
	Name  string
	Value string
	Type  MatchType
}

var (
	// ValidPromLabelName matches standard Prometheus/MetricsQL ASCII label names.
	ValidPromLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

	// ContainsVarSyntax detects Grafana variable interpolation tokens ($name, ${...}, or legacy [[...]])
	// while ignoring regex end-of-line anchors like ^$ or /api$.
	ContainsVarSyntax = regexp.MustCompile(`(?:\$(?:\{|[a-zA-Z_])|\[\[[a-zA-Z_])`)

	// ExactVarRef matches ONLY a standalone Grafana template variable reference:
	//   $var, ${var}, ${var:regex}, or ${var:pipe}
	ExactVarRef = regexp.MustCompile(`^(?:\$([a-zA-Z_][a-zA-Z0-9_]*)|\$\{([a-zA-Z_][a-zA-Z0-9_]*)(?::(regex|pipe))?\})$`)

	// SinglePassVarToken finds standalone Grafana template variable tokens in one pass for interpolation.
	SinglePassVarToken = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)(?::(regex|pipe))?\}|\$([a-zA-Z_][a-zA-Z0-9_]*)`)
)

// IndexCatalogFirstWins deduplicates catalog entries using first-wins semantics
// so forged trailing catalog entries cannot override metric types.
func IndexCatalogFirstWins(catalog []ir.MetricCatalogItem) map[string]ir.MetricCatalogItem {
	byName := make(map[string]ir.MetricCatalogItem, len(catalog))
	for _, item := range catalog {
		if _, exists := byName[item.Name]; !exists {
			byName[item.Name] = item
		}
	}
	return byName
}

// IndexVariablesFirstWins deduplicates variable definitions using first-wins semantics.
func IndexVariablesFirstWins(variables []ir.VariableIR) map[string]ir.VariableIR {
	byName := make(map[string]ir.VariableIR, len(variables))
	for _, v := range variables {
		if _, exists := byName[v.Name]; !exists {
			byName[v.Name] = v
		}
	}
	return byName
}

// ValidateScopeFilter validates a user-supplied ScopeFilter to prevent query or prompt injection.
func ValidateScopeFilter(scope *ir.ScopeFilter) error {
	if scope == nil {
		return nil
	}
	name := strings.TrimSpace(scope.LabelName)
	val := strings.TrimSpace(scope.LabelValue)
	if name == "" && val == "" {
		return nil
	}
	if !ValidPromLabelName.MatchString(name) || name == "__name__" {
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

// NormalizeScope returns a trimmed non-empty ScopeFilter or nil if empty.
func NormalizeScope(scope *ir.ScopeFilter) *ir.ScopeFilter {
	if scope == nil {
		return nil
	}
	sName := strings.TrimSpace(scope.LabelName)
	sVal := strings.TrimSpace(scope.LabelValue)
	if sName == "" || sVal == "" {
		return nil
	}
	return &ir.ScopeFilter{LabelName: sName, LabelValue: sVal}
}

// ValidateVariableMatcher enforces strict template variable rules inside a label matcher.
func ValidateVariableMatcher(
	m Matcher,
	varByName map[string]ir.VariableIR,
	maxRegexLen int,
	maxRegexRepeat int,
) (ir.VariableIR, error) {
	if m.Type == MatchNotEqual || m.Type == MatchNotRegexp {
		return ir.VariableIR{}, fmt.Errorf("%w: template variable in negative matcher %s%s%q is disallowed", ErrPolicyViolation, m.Name, m.Type, m.Value)
	}
	sub := ExactVarRef.FindStringSubmatch(m.Value)
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

	if m.Type == MatchEqual {
		if format != "" {
			return ir.VariableIR{}, fmt.Errorf("%w: format :%s on variable $%s requires =~ matcher", ErrPolicyViolation, format, varName)
		}
		if v.Multi || v.IncludeAll {
			return ir.VariableIR{}, fmt.Errorf("%w: multi/includeAll variable $%s must use =~ matcher instead of =", ErrPolicyViolation, varName)
		}
	}

	if m.Type == MatchRegexp {
		if v.Type == "query" && format != "regex" {
			return ir.VariableIR{}, fmt.Errorf("%w: query variable $%s in =~ matcher must use ${%s:regex} format", ErrPolicyViolation, varName, varName)
		}
		if v.Type == "custom" && format != "regex" {
			for _, opt := range v.Options {
				if err := ValidateRegexMatcher(m.Name, MatchRegexp, opt, maxRegexLen, maxRegexRepeat); err != nil {
					return ir.VariableIR{}, fmt.Errorf("%w: custom variable $%s option %q violates regex policy: %v", ErrPolicyViolation, varName, opt, err)
				}
			}
		}
	}

	return v, nil
}

// MatcherSatisfiesScopeWithVariable returns true if matcher m uses an anchored single-value variable
// matching the mandatory scope label and value.
func MatcherSatisfiesScopeWithVariable(m Matcher, v ir.VariableIR, scope *ir.ScopeFilter) bool {
	if scope == nil || m.Name != scope.LabelName {
		return false
	}
	if m.Type != MatchEqual {
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

// EscapeQueryString escapes a value for safe embedding inside a PromQL/MetricsQL quoted string literal.
func EscapeQueryString(val string) string {
	var b strings.Builder
	b.Grow(len(val))
	for _, r := range val {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\'':
			b.WriteString(`\'`)
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
// placeholders ($var, ${var}, ${var:regex}, ${var:pipe}) with their default values for raw execution.
// Substituted values are string-escaped and never re-scanned.
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
	return SinglePassVarToken.ReplaceAllStringFunc(expr, func(match string) string {
		sub := SinglePassVarToken.FindStringSubmatch(match)
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
			return EscapeQueryString(regexp.QuoteMeta(val))
		}
		return EscapeQueryString(val)
	})
}

// ValidateRegexMatcher parses the regex AST with regexp/syntax to block catch-all scans,
// nested quantifiers, excessive repetition, and empty-only negative matchers.
func ValidateRegexMatcher(labelName string, matchType MatchType, pattern string, maxLen, maxRepeat int) error {
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
	if err := inspectRegexAST(reAST, labelName, false, maxRepeat); err != nil {
		return err
	}

	compiled, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return fmt.Errorf("%w: regex compile failed on label %q: %v", ErrPolicyViolation, labelName, err)
	}
	if matchType == MatchRegexp && compiled.MatchString("") {
		return fmt.Errorf("%w: regex %q on label %q matches empty string (unbounded scan)", ErrPolicyViolation, pattern, labelName)
	}
	if matchType == MatchNotRegexp && compiled.MatchString("") && !compiled.MatchString("a") && !compiled.MatchString("0") {
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
