package metricsql

import (
	"testing"

	vmql "github.com/VictoriaMetrics/metricsql"
)

// FuzzValidateExpr feeds arbitrary MetricsQL, as an LLM may write it, to the
// validator. Every `or` branch of every selector in an accepted expression
// must keep the user's scope and read a catalog metric, as checked here on the
// VictoriaMetrics parser's own AST rather than through the validator's walk.
func FuzzValidateExpr(f *testing.F) {
	for _, s := range []string{
		`sum by (status) (rate(http_requests_total{service="checkout"}[5m]))`,
		`histogram_share(0.5, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout"}[5m])))`,
		`sum(increase_pure(http_requests_total{service="checkout"}[5m])) default 0`,
		`sum(rate(http_requests_total{service="checkout"}[5i]))`,
		`sum(rate(http_requests_total{service="checkout", status="500" or service="checkout", status="503"}[5m]))`,
		`sum(rate(http_requests_total{service="checkout"}[5m])) if sum(up_gauge{service="checkout"}) > 0`,
		`max_over_time(rate(http_requests_total{service="checkout"}[5m])[30m:30s])`,
		`sum(rate(http_requests_total{service="checkout", status="500" or status="503"}[5m]))`,
		`WITH (x=up_gauge{service="checkout"}) x`,
		`union(http_requests_total{service="checkout"}, up_gauge)`,
		`{__name__="up_gauge",service="checkout"}`,
	} {
		f.Add(s)
	}

	catalog := testCatalog()
	inCatalog := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		inCatalog[m.Name] = true
	}
	scope := testScope()
	policy := DefaultPolicy()
	policy.RequireCatalogMatch = true

	f.Fuzz(func(t *testing.T, expr string) {
		if _, err := ValidateExpr(expr, catalog, scope, policy); err != nil {
			return
		}
		ast, err := vmql.Parse(expr)
		if err != nil {
			t.Fatalf("accepted expression does not parse: %v", err)
		}
		walkMetricExprs(ast, func(me *vmql.MetricExpr) {
			for _, branch := range me.LabelFilterss {
				name, scoped := "", false
				for _, lf := range branch {
					if lf.IsRegexp || lf.IsNegative {
						continue
					}
					switch lf.Label {
					case "__name__":
						name = lf.Value
					case scope.LabelName:
						scoped = scoped || lf.Value == scope.LabelValue
					}
				}
				if !scoped {
					t.Fatalf("accepted %q: selector %s has an or branch without the scope %s=%q", expr, me.AppendString(nil), scope.LabelName, scope.LabelValue)
				}
				if !inCatalog[name] {
					t.Fatalf("accepted %q: selector %s reads metric %q outside the catalog", expr, me.AppendString(nil), name)
				}
			}
		})
	})
}

func walkMetricExprs(e vmql.Expr, fn func(*vmql.MetricExpr)) {
	switch t := e.(type) {
	case *vmql.MetricExpr:
		fn(t)
	case *vmql.RollupExpr:
		walkMetricExprs(t.Expr, fn)
		if t.At != nil {
			walkMetricExprs(t.At, fn)
		}
	case *vmql.FuncExpr:
		for _, a := range t.Args {
			walkMetricExprs(a, fn)
		}
	case *vmql.AggrFuncExpr:
		for _, a := range t.Args {
			walkMetricExprs(a, fn)
		}
	case *vmql.BinaryOpExpr:
		walkMetricExprs(t.Left, fn)
		walkMetricExprs(t.Right, fn)
	}
}
