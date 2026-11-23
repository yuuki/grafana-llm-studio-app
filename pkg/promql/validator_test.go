package promql_test

import (
	"errors"
	"testing"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/promql"
)

func sampleCatalog() []ir.MetricCatalogItem {
	return []ir.MetricCatalogItem{
		{
			Name:   "http_requests_total",
			Type:   "counter",
			Help:   "Total HTTP requests",
			Labels: []string{"service", "status", "method"},
		},
		{
			Name:   "http_request_duration_seconds_bucket",
			Type:   "histogram",
			Help:   "HTTP request duration histogram buckets",
			Labels: []string{"service", "le"},
		},
		{
			Name:   "process_resident_memory_bytes",
			Type:   "gauge",
			Help:   "Resident memory size in bytes",
			Labels: []string{"service", "instance"},
		},
	}
}

func TestValidateExpr_PolicyAndGrounding(t *testing.T) {
	cat := sampleCatalog()
	scope := &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"}
	pol := promql.DefaultPolicy()

	validCases := []string{
		`sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))`,
		`sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m])) or vector(0)`,
		`histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout-api"}[5m])))`,
		`avg(process_resident_memory_bytes{service="checkout-api"})`,
		`max_over_time(rate(http_requests_total{service="checkout-api"}[5m])[30m:1m])`,
	}
	for _, expr := range validCases {
		if _, err := promql.ValidateExpr(expr, cat, scope, pol); err != nil {
			t.Fatalf("expected valid expr %q to pass, got %v", expr, err)
		}
	}

	rejectedCases := []struct {
		name      string
		expr      string
		wantError error
	}{
		{
			name:      "excessive range selector",
			expr:      `rate(http_requests_total{service="checkout-api"}[30d])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "expensive subquery range",
			expr:      `max_over_time(rate(http_requests_total{service="checkout-api"}[5m])[6h:5s])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "omitted subquery step rejected (H-2)",
			expr:      `max_over_time(rate(http_requests_total{service="checkout-api"}[5m])[30m:])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "nested subquery rejected (H-2)",
			expr:      `max_over_time(max_over_time(rate(http_requests_total{service="checkout-api"}[5m])[15m:1m])[15m:1m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "negative offset rejected (H-2)",
			expr:      `rate(http_requests_total{service="checkout-api"}[5m] offset -365d)`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "excessive offset",
			expr:      `rate(http_requests_total{service="checkout-api"}[5m] offset 7d)`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "catch-all regex",
			expr:      `rate(http_requests_total{service="checkout-api",status=~".*"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "parenthesized catch-all regex bypass rejected (H-3)",
			expr:      `rate(http_requests_total{service="checkout-api",status=~"(.*)"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "non-greedy catch-all regex bypass rejected (H-3)",
			expr:      `rate(http_requests_total{service="checkout-api",status=~".*?"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "quantifier range catch-all regex bypass rejected (H-3)",
			expr:      `rate(http_requests_total{service="checkout-api",status=~".{0,}"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "alternation catch-all regex bypass rejected (H-3)",
			expr:      `rate(http_requests_total{service="checkout-api",status=~"500|.*"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "negative empty regex catch-all rejected (H-3)",
			expr:      `rate(http_requests_total{service="checkout-api",status!~"^$"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "negative empty inequality rejected (H-3)",
			expr:      `rate(http_requests_total{service="checkout-api",status!=""}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "nested quantifier regex rejected (H-3)",
			expr:      `rate(http_requests_total{service="checkout-api",status=~"([a-z]+)+"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "many-to-one join blocked",
			expr:      `rate(http_requests_total{service="checkout-api"}[5m]) * on(service) group_left() process_resident_memory_bytes{service="checkout-api"}`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "rate on gauge rejected",
			expr:      `rate(process_resident_memory_bytes{service="checkout-api"}[5m])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "rate on parenthesized gauge rejected (M-2)",
			expr:      `rate((process_resident_memory_bytes{service="checkout-api"}[5m]))`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "rate on subquery gauge rejected (M-2)",
			expr:      `rate(process_resident_memory_bytes{service="checkout-api"}[5m:15s])`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "count_values aggregation rejected (M-2)",
			expr:      `count_values("val", process_resident_memory_bytes{service="checkout-api"})`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "label_replace rejected (M-2)",
			expr:      `label_replace(process_resident_memory_bytes{service="checkout-api"}, "service", "other", "service", "checkout-api")`,
			wantError: promql.ErrPolicyViolation,
		},
		{
			name:      "hallucinated metric outside catalog rejected",
			expr:      `rate(fabricated_orders_total{service="checkout-api"}[5m])`,
			wantError: promql.ErrMetricNotInCatalog,
		},
		{
			name:      "dropped scope filter rejected",
			expr:      `sum(rate(http_requests_total[5m]))`,
			wantError: promql.ErrScopeFilterMissing,
		},
	}

	for _, tc := range rejectedCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := promql.ValidateExpr(tc.expr, cat, scope, pol)
			if err == nil {
				t.Fatalf("expected expr %q to be rejected", tc.expr)
			}
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("expected error wrapping %v, got %v", tc.wantError, err)
			}
		})
	}
}
