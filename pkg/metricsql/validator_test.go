package metricsql

import (
	"errors"
	"strings"
	"testing"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/promql"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

func testCatalog() []ir.MetricCatalogItem {
	return []ir.MetricCatalogItem{
		{
			Name:   "http_requests_total",
			Type:   "counter",
			Help:   "Total HTTP requests",
			Labels: []string{"service", "status", "instance", "method"},
		},
		{
			Name:   "http_request_duration_seconds_bucket",
			Type:   "histogram",
			Help:   "HTTP request duration histogram",
			Labels: []string{"service", "le", "instance"},
		},
		{
			Name:   "up_gauge",
			Type:   "gauge",
			Help:   "Service up status",
			Labels: []string{"service", "instance"},
		},
		{
			Name:   "with_total",
			Type:   "counter",
			Help:   "Metric whose name starts with 'with'",
			Labels: []string{"service"},
		},
		{
			Name:   "ru_total",
			Type:   "counter",
			Help:   "Metric whose name starts with 'ru'",
			Labels: []string{"service"},
		},
	}
}

func testScope() *ir.ScopeFilter {
	return &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout"}
}

func TestValidateExpr_ValidMetricsQL(t *testing.T) {
	catalog := testCatalog()
	scope := testScope()
	policy := DefaultPolicy()

	validCases := []struct {
		name string
		expr string
		vars []ir.VariableIR
	}{
		{
			name: "standard PromQL compatible query",
			expr: `sum by (status) (rate(http_requests_total{service="checkout"}[5m]))`,
		},
		{
			name: "share_le_over_time MetricsQL function",
			expr: `share_le_over_time(http_request_duration_seconds_bucket{service="checkout"}[5m], 0.5)`,
		},
		{
			name: "histogram_share MetricsQL function",
			expr: `histogram_share(0.5, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout"}[5m])))`,
		},
		{
			name: "increase_pure with default binary operator",
			expr: `sum(increase_pure(http_requests_total{service="checkout"}[5m])) default 0`,
		},
		{
			name: "implicit lookback window on non-subquery rollup",
			expr: `sum(rate(http_requests_total{service="checkout"}))`,
		},
		{
			name: "bounded step-relative 5i window on non-subquery rollup",
			expr: `sum(rate(http_requests_total{service="checkout"}[5i]))`,
		},
		{
			name: "OR label filter group where every branch retains metric and scope",
			expr: `sum(rate(http_requests_total{service="checkout", status="500" or service="checkout", status="503"}[5m]))`,
		},
		{
			name: "conditional if operator without join modifier",
			expr: `sum(rate(http_requests_total{service="checkout"}[5m])) if sum(up_gauge{service="checkout"}) > 0`,
		},
		{
			name: "valid bounded subquery with explicit range and step",
			expr: `max_over_time(rate(http_requests_total{service="checkout"}[5m])[30m:30s])`,
		},
		{
			name: "metric names starting with with_ or ru_ and comments containing WITH/ru do not false-positive",
			expr: "# WITH (a=1) ru(1,2)\nsum(rate(with_total{service=\"checkout\"}[5m])) + sum(rate(ru_total{service=\"checkout\"}[5m]))",
		},
		{
			name: "anchored scope variable and regex query variable in double quotes",
			expr: `sum(rate(http_requests_total{service="$service", instance=~"${instance:regex}"}[5m]))`,
			vars: []ir.VariableIR{
				{
					Name:          "service",
					Type:          "query",
					DatasourceRef: ir.SelectedDatasourceRef,
					LabelName:     "service",
					MetricName:    "http_requests_total",
					DefaultValue:  "checkout",
				},
				{
					Name:          "instance",
					Type:          "query",
					DatasourceRef: ir.SelectedDatasourceRef,
					LabelName:     "instance",
					MetricName:    "http_requests_total",
					DefaultValue:  "pod-1",
					Multi:         true,
				},
			},
		},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ValidateExprWithVariables(tc.expr, catalog, scope, tc.vars, policy)
			if err != nil {
				t.Fatalf("expected valid MetricsQL, got error: %v", err)
			}
			if len(res.ReferencedMetrics) == 0 {
				t.Fatalf("expected at least one referenced metric")
			}
		})
	}
}

func TestValidateExpr_AdversarialSecurityRejections(t *testing.T) {
	catalog := testCatalog()
	scope := testScope()
	policy := DefaultPolicy()

	vars := []ir.VariableIR{
		{
			Name:         "env",
			Type:         "custom",
			Options:      []string{"prod", "staging"},
			DefaultValue: "prod",
		},
		{
			Name:          "service",
			Type:          "query",
			DatasourceRef: ir.SelectedDatasourceRef,
			LabelName:     "service",
			MetricName:    "http_requests_total",
			DefaultValue:  "checkout",
		},
	}

	// Build 25-level nested ru() and ttf() OOM bomb strings (< 200 bytes)
	ruBomb := `up_gauge{service="checkout"}`
	ttfBomb := `up_gauge{service="checkout"}`
	for i := 0; i < 25; i++ {
		ruBomb = "ru(1," + ruBomb + ")"
		ttfBomb = "ttf(" + ttfBomb + ")"
	}
	// Build exponential WITH OOM bomb (< 300 bytes)
	withBomb := `WITH (a=up_gauge{service="checkout"}, b=a+a, c=b+b, d=c+c, e=d+d, f=e+e, g=f+f, h=g+g, i=h+h, j=i+i, k=j+j, l=k+k, m=l+l, n=m+m, o=n+n, p=o+o, q=p+p, r=q+q, s=r+r, t=s+s, u=t+t, v=u+u, w=v+v, x=w+w) x`

	rejectedCases := []struct {
		name    string
		expr    string
		wantErr error
	}{
		// C-1: Pre-parse OOM bombs
		{
			name:    "C-1a inline WITH OOM bomb rejected before Parse",
			expr:    withBomb,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-1a WITH with comment before paren rejected",
			expr:    "WITH # sneaky\n (x=up_gauge{service=\"checkout\"}) x",
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-1b built-in ru() 25-level OOM bomb rejected before Parse",
			expr:    ruBomb,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-1b built-in ttf() 25-level OOM bomb rejected before Parse",
			expr:    ttfBomb,
			wantErr: ErrPolicyViolation,
		},
		// C-2 & H-2: Single-quote / backtick / simplifyConstants variable smuggling
		{
			name:    "C-2 single-quoted variable matcher rejected",
			expr:    `http_requests_total{service="checkout", status='$env'}`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-2 backtick variable matcher rejected",
			expr:    "http_requests_total{service=\"checkout\", status=`$env`}",
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "H-2 simplifyConstants string comparison variable smuggling rejected",
			expr:    `up_gauge{service="checkout"} + ("$env" == "prod")`,
			wantErr: ErrPolicyViolation,
		},
		// C-3: Allowlist / anonymous union / aggr_over_time / label_match / AggrFuncExpr / gauge guards
		{
			name:    "C-3a anonymous union tuple (a, b) rejected",
			expr:    `(http_requests_total{service="checkout"}, up_gauge{service="checkout"})`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3b explicit union() rejected",
			expr:    `union(http_requests_total{service="checkout"}, up_gauge{service="checkout"})`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3c aggr_over_time meta-function rejected",
			expr:    `aggr_over_time(("rollup_candlestick"), http_requests_total{service="checkout"}[5m])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3d count_values_over_time rejected",
			expr:    `count_values_over_time("val", http_requests_total{service="checkout"}[5m])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3d count_values aggregation rejected",
			expr:    `count_values("val", http_requests_total{service="checkout"})`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3d quantiles aggregation rejected",
			expr:    `quantiles("phi", 0.5, 0.9, http_requests_total{service="checkout"})`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3d histogram aggregation rejected",
			expr:    `histogram(up_gauge{service="checkout"})`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3d 3x fan-out rollup_rate rejected by allowlist",
			expr:    `rollup_rate(http_requests_total{service="checkout"}[5m])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3e label_match regex bypass rejected",
			expr:    `label_match(http_requests_total{service="checkout"}, "status", ".*")`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3e label_set mutator rejected",
			expr:    `label_set(http_requests_total{service="checkout"}, "service", "admin")`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3e alias() rejected",
			expr:    `alias(up_gauge{service="checkout"}, "x")`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3f AggrFuncExpr limit > 100 rejected",
			expr:    `sum(rate(http_requests_total{service="checkout"}[5m])) by (status) limit 1000`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3g gauge metric rejected in increase_pure()",
			expr:    `increase_pure(up_gauge{service="checkout"}[5m])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3g gauge metric rejected in rate_prometheus()",
			expr:    `rate_prometheus(up_gauge{service="checkout"}[5m])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-3g gauge metric rejected in remove_resets()",
			expr:    `remove_resets(up_gauge{service="checkout"})`,
			wantErr: ErrPolicyViolation,
		},
		// C-4: Rollup / Subquery / Duration / Magic token edge cases
		{
			name:    "C-4a subquery with omitted window rejected",
			expr:    `rate(http_requests_total{service="checkout"}[:15s])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4b subquery with omitted step rejected",
			expr:    `max_over_time(rate(http_requests_total{service="checkout"}[5m])[30m:])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4c subquery with step-relative step 1i rejected",
			expr:    `max_over_time(rate(http_requests_total{service="checkout"}[5m])[30m:1i])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4c subquery with step-relative window 5i rejected",
			expr:    `max_over_time(rate(http_requests_total{service="checkout"}[5m])[5i:30s])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4d step-relative window 11i exceeds max 10i",
			expr:    `rate(http_requests_total{service="checkout"}[11i])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4d mixed window 1h5i rejected",
			expr:    `rate(http_requests_total{service="checkout"}[1h5i])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4d fixed window 2h exceeds max 1h",
			expr:    `rate(http_requests_total{service="checkout"}[2h])`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4e offset with i unit rejected",
			expr:    `up_gauge{service="checkout"} offset 5i`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4e compound negative offset 1h-2h rejected",
			expr:    `up_gauge{service="checkout"} offset 1h-2h`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4e negative offset -5m rejected",
			expr:    `up_gauge{service="checkout"} offset -5m`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4f @ modifier rejected",
			expr:    `up_gauge{service="checkout"} @ 1700000000`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4g implicit non-metric rollup timestamp(sum(m)) rejected",
			expr:    `timestamp(sum(up_gauge{service="checkout"}))`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "C-4h raw $__interval magic token rejected",
			expr:    `rate(http_requests_total{service="checkout"}[$__interval])`,
			wantErr: ErrPolicyViolation,
		},
		// H-1: BinaryOpExpr group_left/group_right on if/ifnot/default
		{
			name:    "H-1 conditional if with group_left(*) rejected",
			expr:    `http_requests_total{service="checkout"} if on(instance) group_left(*) up_gauge{service="checkout"}`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "H-1 default with group_right rejected",
			expr:    `http_requests_total{service="checkout"} default on(instance) group_right up_gauge{service="checkout"}`,
			wantErr: ErrPolicyViolation,
		},
		// H-3: LabelFilterss OR scope bypass, selector explosion, duplicate __name__, escaped labels
		{
			name:    "H-3 OR label filter branch missing scope rejected",
			expr:    `http_requests_total{service="checkout", status="500" or status="503"}`,
			wantErr: ErrScopeFilterMissing,
		},
		{
			name:    "H-3 OR label filter exceeding MaxSelectorsPerExpr (5 branches) rejected",
			expr:    `http_requests_total{service="checkout", status="500" or service="checkout", status="501" or service="checkout", status="502" or service="checkout", status="503" or service="checkout", status="504"}`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "H-3 mixed __name__= and __name__=~ in same selector rejected",
			expr:    `{__name__="http_requests_total", __name__=~"http.*", service="checkout"}`,
			wantErr: ErrPolicyViolation,
		},
		{
			name:    "H-3 unknown metric in catalog rejected",
			expr:    `rate(secret_internal_metric{service="checkout"}[5m])`,
			wantErr: ErrMetricNotInCatalog,
		},
	}

	for _, tc := range rejectedCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateExprWithVariables(tc.expr, catalog, scope, vars, policy)
			if err == nil {
				t.Fatalf("expected error %v for expr %q, got nil", tc.wantErr, tc.expr)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected errors.Is(..., %v), got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestPromQL_SingleQuoteVariableInjectionAlsoBlocked(t *testing.T) {
	catalog := testCatalog()
	scope := testScope()
	vars := []ir.VariableIR{
		{Name: "env", Type: "custom", Options: []string{"prod"}, DefaultValue: "prod"},
	}
	_, err := promql.ValidateExprWithVariables(
		`http_requests_total{service="checkout", status='$env'}`,
		catalog,
		scope,
		vars,
		promql.DefaultPolicy(),
	)
	if !errors.Is(err, query.ErrPolicyViolation) {
		t.Fatalf("expected PromQL validator to reject single-quoted variable matcher, got: %v", err)
	}
}

func TestValidateDashboardIR_DialectMismatchRejected(t *testing.T) {
	doc := ir.DashboardIR{
		SchemaVersion: ir.SchemaVersion,
		Revision:      1,
		Title:         "VM Test",
		TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
		Panels: []ir.PanelIR{
			{
				ID:            "panel-req",
				Type:          "timeseries",
				Title:         "Requests",
				DatasourceRef: ir.SelectedDatasourceRef,
				Query: ir.QueryIR{
					Kind: "promql",
					Expr: `sum(rate(http_requests_total{service="checkout"}[5m]))`,
				},
				Unit:   "reqps",
				Layout: ir.LayoutHint{Row: 0, Col: 0, Width: "full"},
			},
		},
	}
	_, err := ValidateDashboardIR(doc, testCatalog(), testScope(), ir.ValidateOptions{MaxPanels: 6}, DefaultPolicy())
	if err == nil || !strings.Contains(err.Error(), "does not match active dialect") {
		t.Fatalf("expected dialect mismatch error, got: %v", err)
	}

	// Matching kind succeeds
	doc.Panels[0].Query.Kind = "metricsql"
	if _, err := ValidateDashboardIR(doc, testCatalog(), testScope(), ir.ValidateOptions{MaxPanels: 6}, DefaultPolicy()); err != nil {
		t.Fatalf("expected matching metricsql kind to succeed, got: %v", err)
	}
}
