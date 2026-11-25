package promql_test

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/promql"
)

// FuzzValidateExpr feeds arbitrary PromQL, as an LLM may write it, to the
// validator. An expression it accepts must keep the user's scope and stay on
// the metric catalog in every selector, as checked here on the parser's own
// AST rather than through the validator's walk.
func FuzzValidateExpr(f *testing.F) {
	for _, s := range []string{
		`sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))`,
		`sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m])) or vector(0)`,
		`histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout-api"}[5m])))`,
		`avg(process_resident_memory_bytes{service="checkout-api"})`,
		`max_over_time(rate(http_requests_total{service="checkout-api"}[5m])[30m:1m])`,
		`{__name__="http_requests_total",service="checkout-api"}`,
		`rate(http_requests_total[5m])`,
		`rate(http_requests_total{service=~".*"}[5m])`,
		`label_replace(up, "a", "$1", "b", "(.*)")`,
	} {
		f.Add(s)
	}

	catalog := sampleCatalog()
	inCatalog := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		inCatalog[m.Name] = true
	}
	scope := &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"}
	policy := promql.DefaultPolicy()
	policy.RequireCatalogMatch = true
	p := parser.NewParser(parser.Options{})

	f.Fuzz(func(t *testing.T, expr string) {
		if _, err := promql.ValidateExpr(expr, catalog, scope, policy); err != nil {
			return
		}
		ast, err := p.ParseExpr(expr)
		if err != nil {
			t.Fatalf("accepted expression does not parse: %v", err)
		}
		parser.Inspect(ast, func(node parser.Node, _ []parser.Node) error {
			vs, ok := node.(*parser.VectorSelector)
			if !ok {
				return nil
			}
			name, scoped := vs.Name, false
			for _, m := range vs.LabelMatchers {
				if m.Type != labels.MatchEqual {
					continue
				}
				switch m.Name {
				case model.MetricNameLabel:
					name = m.Value
				case scope.LabelName:
					scoped = scoped || m.Value == scope.LabelValue
				}
			}
			if !scoped {
				t.Fatalf("accepted %q: selector %s drops the scope %s=%q", expr, vs, scope.LabelName, scope.LabelValue)
			}
			if !inCatalog[name] {
				t.Fatalf("accepted %q: selector %s reads metric %q outside the catalog", expr, vs, name)
			}
			return nil
		})
	})
}
