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

		// Line comment: # ... \n
		if ch == '#' {
			for i < n && expr[i] != '\n' {
				i++
			}
			continue
		}

		// Double-quoted string literal: "..."
		if ch == '"' {
			start := i + 1
			i++
			closed := false
			for i < n {
				if expr[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if expr[i] == '"' {
					closed = true
					break
				}
				i++
			}
			content := expr[start:i]
			if closed {
				i++
			}
			if ContainsVarSyntax.MatchString(content) && braceDepth == 0 {
				return fmt.Errorf("%w: template variable tokens are forbidden outside label matcher string literals", ErrPolicyViolation)
			}
			continue
		}

		// Single-quoted string literal: '...'
		if ch == '\'' {
			start := i + 1
			i++
			closed := false
			for i < n {
				if expr[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if expr[i] == '\'' {
					closed = true
					break
				}
				i++
			}
			content := expr[start:i]
			if closed {
				i++
			}
			if ContainsVarSyntax.MatchString(content) {
				return fmt.Errorf("%w: template variable tokens must use double-quoted label matchers, not single-quoted strings", ErrPolicyViolation)
			}
			continue
		}

		// Backtick raw string literal: `...`
		if ch == '`' {
			start := i + 1
			i++
			closed := false
			for i < n {
				if expr[i] == '`' {
					closed = true
					break
				}
				i++
			}
			content := expr[start:i]
			if closed {
				i++
			}
			if ContainsVarSyntax.MatchString(content) {
				return fmt.Errorf("%w: template variable tokens must use double-quoted label matchers, not backtick strings", ErrPolicyViolation)
			}
			continue
		}

		// Track label matcher brace depth.
		if ch == '{' {
			braceDepth++
			i++
			continue
		}
		if ch == '}' {
			if braceDepth > 0 {
				braceDepth--
			}
			i++
			continue
		}

		// Reject unquoted Grafana variable / magic tokens ($__interval, $var, ${var}, [[var]]) outside strings.
		if ch == '$' && i+1 < n && (expr[i+1] == '{' || isIdentStart(expr[i+1])) {
			return fmt.Errorf("%w: unquoted template variable or magic token is forbidden", ErrPolicyViolation)
		}
		if ch == '[' && i+2 < n && expr[i+1] == '[' && isIdentStart(expr[i+2]) {
			return fmt.Errorf("%w: legacy [[variable]] syntax is forbidden outside double-quoted label matchers", ErrPolicyViolation)
		}

		// Scan identifier tokens for MetricsQL pre-parse macro/WITH guards.
		if isIdentStart(ch) {
			start := i
			i++
			for i < n && isIdentPart(expr[i]) {
				i++
			}
			if kind == KindMetricsQL {
				ident := expr[start:i]
				nextCh := peekNextSignificantByte(expr, i)
				if nextCh == '(' {
					switch {
					case strings.EqualFold(ident, "with"):
						return fmt.Errorf("%w: WITH (...) templates are disallowed", ErrPolicyViolation)
					case strings.EqualFold(ident, "ru"), strings.EqualFold(ident, "ttf"):
						return fmt.Errorf("%w: built-in recursive macro %s() is disallowed", ErrPolicyViolation, strings.ToLower(ident))
					}
				}
			}
			continue
		}

		i++
	}

	return nil
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
