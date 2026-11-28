package query

import (
	"fmt"
	"strings"
)

// PrescanExpr runs a lightweight, zero-allocation-risk lexical scan BEFORE invoking
// PromQL or MetricsQL parsers. It closes structural parser vulnerabilities:
//   - Rejects template variable tokens ($var, ${...}, [[...]]) inside single-quoted ('...'),
//     backtick (`...`), unquoted, or non-label-matcher string literals (prevents single-quote
//     string escape injection and MetricsQL simplifyConstants folding of "$var" == "x").
//   - For MetricsQL, rejects inline WITH (...) templates and built-in recursive macros ru() / ttf()
//     BEFORE metricsql.Parse() can trigger unbounded AST/memory expansion (OOM DoS).
//   - Rejects raw Grafana magic tokens ($__interval, $__rate_interval, $__range, etc.) outside quotes.
func PrescanExpr(expr string, kind Kind) error {
	n := len(expr)
	braceDepth := 0

	for i := 0; i < n; {
		ch := expr[i]
		switch {
		case ch == '#':
			i = skipLineComment(expr, i)
		case ch == '"' || ch == '\'' || ch == '`':
			nextIdx, err := prescanStringLiteral(expr, i, braceDepth)
			if err != nil {
				return err
			}
			i = nextIdx
		case ch == '{':
			braceDepth++
			i++
		case ch == '}':
			if braceDepth > 0 {
				braceDepth--
			}
			i++
		case ch == '$' && i+1 < n && (expr[i+1] == '{' || isIdentStart(expr[i+1])):
			return fmt.Errorf("%w: unquoted template variable or magic token is forbidden", ErrPolicyViolation)
		case ch == '[' && i+2 < n && expr[i+1] == '[' && isIdentStart(expr[i+2]):
			return fmt.Errorf("%w: legacy [[variable]] syntax is forbidden outside double-quoted label matchers", ErrPolicyViolation)
		case isIdentStart(ch):
			nextIdx, err := prescanIdentifier(expr, i, kind)
			if err != nil {
				return err
			}
			i = nextIdx
		default:
			i++
		}
	}

	return nil
}

func skipLineComment(expr string, i int) int {
	for i < len(expr) && expr[i] != '\n' {
		i++
	}
	return i
}

func scanQuotedString(expr string, i int, quote byte, allowEscape bool) (string, int) {
	n := len(expr)
	start := i + 1
	i++
	closed := false
	for i < n {
		if allowEscape && expr[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if expr[i] == quote {
			closed = true
			break
		}
		i++
	}
	content := expr[start:i]
	if closed {
		i++
	}
	return content, i
}

func prescanStringLiteral(expr string, i int, braceDepth int) (int, error) {
	quote := expr[i]
	allowEscape := quote != '`'
	content, nextIdx := scanQuotedString(expr, i, quote, allowEscape)
	if !ContainsVarSyntax.MatchString(content) {
		return nextIdx, nil
	}
	switch quote {
	case '"':
		if braceDepth == 0 {
			return nextIdx, fmt.Errorf("%w: template variable tokens are forbidden outside label matcher string literals", ErrPolicyViolation)
		}
	case '\'':
		return nextIdx, fmt.Errorf("%w: template variable tokens must use double-quoted label matchers, not single-quoted strings", ErrPolicyViolation)
	case '`':
		return nextIdx, fmt.Errorf("%w: template variable tokens must use double-quoted label matchers, not backtick strings", ErrPolicyViolation)
	}
	return nextIdx, nil
}

func prescanIdentifier(expr string, i int, kind Kind) (int, error) {
	start := i
	i++
	for i < len(expr) && isIdentPart(expr[i]) {
		i++
	}
	if kind != KindMetricsQL {
		return i, nil
	}
	ident := expr[start:i]
	if peekNextSignificantByte(expr, i) != '(' {
		return i, nil
	}
	switch {
	case strings.EqualFold(ident, "with"):
		return i, fmt.Errorf("%w: WITH (...) templates are disallowed", ErrPolicyViolation)
	case strings.EqualFold(ident, "ru"), strings.EqualFold(ident, "ttf"):
		return i, fmt.Errorf("%w: built-in recursive macro %s() is disallowed", ErrPolicyViolation, strings.ToLower(ident))
	}
	return i, nil
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func peekNextSignificantByte(s string, i int) byte {
	n := len(s)
	for i < n {
		ch := s[i]
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
			i++
			continue
		}
		if ch == '#' {
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}
		return ch
	}
	return 0
}
