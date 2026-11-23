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

func TestValidateExprWithVariables_AndMultiQueryDashboard(t *testing.T) {
	cat := sampleCatalog()
	scope := &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"}
	pol := promql.DefaultPolicy()

	vars := []ir.VariableIR{
		{
			Name:          "service",
			Type:          "query",
			DatasourceRef: ir.SelectedDatasourceRef,
			LabelName:     "service",
			MetricName:    "http_requests_total",
			DefaultValue:  "checkout-api",
		},
		{
			Name:         "status_class",
			Type:         "custom",
			Options:      []string{"5..", "4..|5.."},
			DefaultValue: "5..",
		},
		{
			Name:          "multi_status",
			Type:          "query",
			DatasourceRef: ir.SelectedDatasourceRef,
			LabelName:     "status",
			MetricName:    "http_requests_total",
			DefaultValue:  "500",
			Multi:         true,
		},
	}

	validExprs := []string{
		`sum by (status) (rate(http_requests_total{service="$service"}[5m]))`,
		`sum(rate(http_requests_total{service="$service",status=~"$status_class"}[5m]))`,
		`sum(rate(http_requests_total{service="${service}",status=~"${multi_status:regex}"}[5m]))`,
		`sum(rate(http_requests_total{service="$service",status=~"${multi_status:pipe}"}[5m]))`,
	}
	for _, expr := range validExprs {
		res, err := promql.ValidateExprWithVariables(expr, cat, scope, vars, pol)
		if err != nil {
			t.Fatalf("expected valid templated expr %q to pass, got %v", expr, err)
		}
		if len(res.ReferencedVariables) == 0 {
			t.Fatalf("expected ReferencedVariables to be recorded for %q", expr)
		}
	}

	rejected := []struct {
		name      string
		expr      string
		customVar []ir.VariableIR
		wantErr   error
	}{
		{
			name:    "undeclared variable rejected",
			expr:    `sum(rate(http_requests_total{service="$unknown_svc"}[5m]))`,
			wantErr: promql.ErrPolicyViolation,
		},
		{
			name:    "mixed wildcard regex with variable rejected",
			expr:    `sum(rate(http_requests_total{service="$service",status=~".*$status_class.*"}[5m]))`,
			wantErr: promql.ErrPolicyViolation,
		},
		{
			name:    "variable in negative matcher rejected",
			expr:    `sum(rate(http_requests_total{service="$service",status!="$status_class"}[5m]))`,
			wantErr: promql.ErrPolicyViolation,
		},
		{
			name:    "multi variable with equality matcher rejected",
			expr:    `sum(rate(http_requests_total{service="$service",status="$multi_status"}[5m]))`,
			wantErr: promql.ErrPolicyViolation,
		},
		{
			name: "includeAll variable cannot satisfy required scope",
			expr: `sum(rate(http_requests_total{service=~"$service"}[5m]))`,
			customVar: []ir.VariableIR{
				{
					Name:          "service",
					Type:          "query",
					DatasourceRef: ir.SelectedDatasourceRef,
					LabelName:     "service",
					DefaultValue:  "checkout-api",
					IncludeAll:    true,
				},
			},
			wantErr: promql.ErrScopeFilterMissing,
		},
		{
			name: "custom variable with catch-all option rejected in =~ matcher",
			expr: `sum(rate(http_requests_total{service="checkout-api",status=~"$bad_regex"}[5m]))`,
			customVar: []ir.VariableIR{
				{
					Name:         "bad_regex",
					Type:         "custom",
					Options:      []string{"5..", ".*"},
					DefaultValue: "5..",
				},
			},
			wantErr: promql.ErrPolicyViolation,
		},
	}

	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			v := vars
			if tc.customVar != nil {
				v = tc.customVar
			}
			_, err := promql.ValidateExprWithVariables(tc.expr, cat, scope, v, pol)
			if err == nil {
				t.Fatalf("expected %q to be rejected", tc.expr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}
		})
	}

	t.Run("ValidateDashboardIR validates query variable metricName against catalog and all multi-query entries", func(t *testing.T) {
		doc := ir.DashboardIR{
			SchemaVersion: ir.SchemaVersion,
			Revision:      1,
			Title:         "Multi-query + Vars",
			TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
			Variables:     vars[:2],
			Panels: []ir.PanelIR{
				{
					ID:            "panel-multi",
					Type:          "timeseries",
					Title:         "Latency",
					DatasourceRef: ir.SelectedDatasourceRef,
					Queries: []ir.QueryIR{
						{RefID: "A", Expr: `histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))`},
						{RefID: "B", Expr: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))`},
					},
					Unit:   "s",
					Layout: ir.LayoutHint{Row: 0, Col: 0, Width: "full"},
				},
			},
		}
		if _, err := promql.ValidateDashboardIR(doc, cat, scope, ir.ValidateOptions{MaxPanels: 6}, pol); err != nil {
			t.Fatalf("expected valid multi-query dashboard to pass, got %v", err)
		}

		// Query B referencing unknown metric must fail.
		badMulti := doc.Clone()
		badMulti.Panels[0].Queries[1].Expr = `rate(nonexistent_metric{service="$service"}[5m])`
		if _, err := promql.ValidateDashboardIR(badMulti, cat, scope, ir.ValidateOptions{MaxPanels: 6}, pol); !errors.Is(err, promql.ErrMetricNotInCatalog) {
			t.Fatalf("expected ErrMetricNotInCatalog on query B, got %v", err)
		}

		// Variable metricName outside catalog must fail.
		badVarMetric := doc.Clone()
		badVarMetric.Variables[0].MetricName = "fabricated_metric_total"
		if _, err := promql.ValidateDashboardIR(badVarMetric, cat, scope, ir.ValidateOptions{MaxPanels: 6}, pol); !errors.Is(err, promql.ErrMetricNotInCatalog) {
			t.Fatalf("expected ErrMetricNotInCatalog on variable metricName, got %v", err)
		}
	})

	t.Run("InterpolateVariables substitutes default values cleanly", func(t *testing.T) {
		in := `sum(rate(http_requests_total{service="$service",status=~"${status_class:pipe}"}[5m]))`
		got := promql.InterpolateVariables(in, vars[:2])
		want := `sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m]))`
		if got != want {
			t.Fatalf("InterpolateVariables = %q, want %q", got, want)
		}
	})
}
