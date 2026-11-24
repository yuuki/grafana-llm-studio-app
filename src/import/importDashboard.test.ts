import { of, throwError } from 'rxjs';
import { validateIRClient } from '../ir/types';
import { normalizeDashboardIR } from '../spec/normalize';
import {
  convertGrafanaDashboardToIR,
  fetchExistingDashboardByUid,
} from './importDashboard';

const mockFetch = jest.fn();

jest.mock('@grafana/runtime', () => ({
  config: {
    namespace: 'default',
    bootData: { user: { orgId: 1 } },
  },
  getBackendSrv: () => ({
    fetch: mockFetch,
  }),
}));

describe('Safe Grafana dashboard import into constrained IR v1.1 draft', () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  test('converts supported panel types, row-nested panels, multi-query targets, and scoped variables while skipping unsafe/foreign items', () => {
    const rawDashboard = {
      title: 'Production <script>alert(1)</script> Overview',
      description: 'Imported <b>summary</b>',
      time: { from: 'now-1h', to: 'now' },
      templating: {
        list: [
          {
            name: 'service',
            label: 'Service',
            type: 'query',
            datasource: { type: 'prometheus', uid: 'prom-main' },
            query: 'label_values(http_requests_total, service)',
            current: { value: 'checkout-api' },
          },
          {
            name: 'status_class',
            type: 'custom',
            query: '5..,4..|5..',
            current: { value: '5..' },
          },
          {
            name: 'foreign_var',
            type: 'query',
            datasource: { type: 'loki', uid: 'loki-prod' },
            query: 'label_values(app)',
          },
          {
            name: 'arbitrary_text',
            type: 'textbox',
            query: 'abc',
          },
        ],
      },
      panels: [
        {
          type: 'row',
          title: 'HTTP Row',
          panels: [
            {
              type: 'timeseries',
              title: 'Latency Percentiles',
              datasource: { type: 'prometheus', uid: 'prom-main' },
              gridPos: { x: 0, y: 0, w: 24, h: 8 },
              fieldConfig: {
                defaults: {
                  unit: 's',
                  thresholds: {
                    steps: [
                      { value: null, color: 'green' },
                      { value: 0.5, color: 'yellow' },
                      { value: 1.0, color: 'red' },
                    ],
                  },
                },
              },
              targets: [
                { expr: 'histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))', legendFormat: 'p50' },
                { expr: 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))', legendFormat: 'p95' },
                { expr: 'histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))', legendFormat: 'p99' },
                { expr: 'histogram_quantile(0.999, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))', legendFormat: 'p999-dropped' },
              ],
            },
          ],
        },
        {
          type: 'gauge',
          title: 'Error Gauge',
          datasource: { type: 'prometheus', uid: 'prom-main' },
          gridPos: { x: 0, y: 8, w: 12, h: 6 },
          fieldConfig: { defaults: { unit: 'percentunit' } },
          targets: [
            { expr: 'sum(rate(http_requests_total{service="$service",status=~"$status_class"}[5m]))' },
            { datasource: { type: 'prometheus', uid: 'other-secret-prom' }, expr: 'secret_metric' },
          ],
        },
        {
          type: 'text',
          title: 'Unsafe HTML Text Panel',
          gridPos: { x: 12, y: 8, w: 12, h: 6 },
          options: { content: '<script>evil()</script>' },
        },
        {
          type: 'table',
          title: 'Status Table',
          datasource: { type: 'prometheus', uid: 'prom-main' },
          gridPos: { x: 0, y: 14, w: 12, h: 8 },
          fieldConfig: { defaults: { unit: 'custom_unsupported_unit' } },
          targets: [{ expr: 'sum by (status) (rate(http_requests_total{service="$service"}[5m]))' }],
        },
      ],
    };

    const { draft, report } = convertGrafanaDashboardToIR(rawDashboard, {
      selectedDatasourceUid: 'prom-main',
      selectedDatasourceName: 'Prometheus Main',
      maxPanels: 6,
    });

    expect(draft.schemaVersion).toBe('1.1');
    expect(draft.title).toBe('Production alert(1) Overview');
    expect(draft.description).toBe('Imported summary');
    expect(draft.timeRange).toEqual({ from: 'now-1h', to: 'now' });

    // Only safe query + custom variables imported; foreign loki & textbox skipped.
    expect(draft.variables).toHaveLength(2);
    expect(draft.variables?.map((v) => v.name)).toEqual(['service', 'status_class']);
    expect(report.skippedVariables).toHaveLength(2);

    // 3 valid panels imported; text panel skipped; 4th query truncated; foreign prom target skipped.
    expect(draft.panels).toHaveLength(3);
    expect(draft.panels.map((p) => p.type)).toEqual(['timeseries', 'gauge', 'table']);
    expect(draft.panels[0].queries).toHaveLength(3);
    expect(draft.panels[1].query?.expr).toContain('http_requests_total');
    expect(draft.panels[2].unit).toBe('short');
    expect(report.skippedPanels).toHaveLength(1);
    expect(report.skippedPanels[0].type).toBe('text');
    expect(report.warnings.some((w) => w.includes('truncated to max 3 queries'))).toBe(true);
    expect(report.warnings.some((w) => w.includes('another datasource'))).toBe(true);
    expect(report.warnings.some((w) => w.includes('custom_unsupported_unit'))).toBe(true);

    // Resulting IR must pass strict client validation and normalization cleanly.
    expect(validateIRClient(draft)).toEqual([]);
    const spec = normalizeDashboardIR(draft, { uid: 'prom-main', type: 'prometheus' });
    expect(spec.panels).toHaveLength(3);
    expect(spec.variables).toHaveLength(2);
  });

  test('fetchExistingDashboardByUid captures metadata.resourceVersion from v1beta1 and falls back cleanly on legacy 404', async () => {
    mockFetch.mockReturnValueOnce(
      of({
        data: {
          metadata: {
            name: 'dash-123',
            resourceVersion: '17',
            annotations: { 'grafana.app/folder': 'team-sre' },
          },
          spec: {
            title: 'Checkout Dashboard',
            version: 5,
            panels: [],
          },
        },
      })
    );

    const res = await fetchExistingDashboardByUid('dash-123');
    expect(res.source).toEqual({
      uid: 'dash-123',
      resourceVersion: '17',
      folderUid: 'team-sre',
      title: 'Checkout Dashboard',
      version: 5,
    });

    // Legacy fallback omits resourceVersion so opt-in revision-checked PUT is safely unavailable.
    mockFetch
      .mockReturnValueOnce(throwError(() => ({ status: 404 })))
      .mockReturnValueOnce(
        of({
          data: {
            meta: { folderUid: 'legacy-folder' },
            dashboard: { uid: 'dash-legacy', title: 'Legacy Dash', version: 2, panels: [] },
          },
        })
      );

    const legacyRes = await fetchExistingDashboardByUid('dash-legacy');
    expect(legacyRes.source.uid).toBe('dash-legacy');
    expect(legacyRes.source.resourceVersion).toBeUndefined();
    expect(legacyRes.source.folderUid).toBe('legacy-folder');

    // Non-404/410/501 errors (e.g. HTTP 500) must NOT silently fall back to legacy endpoint
    mockFetch.mockReturnValueOnce(throwError(() => ({ status: 500, message: 'Internal Server Error' })));
    await expect(fetchExistingDashboardByUid('dash-err')).rejects.toMatchObject({ status: 500 });
    expect(mockFetch).toHaveBeenCalledTimes(4);

    // Captures preserved non-folder annotations and labels from v1beta1 metadata
    mockFetch.mockReturnValueOnce(
      of({
        data: {
          metadata: {
            name: 'dash-meta',
            resourceVersion: '22',
            annotations: {
              'grafana.app/folder': 'team-sre',
              'grafana.app/managedBy': 'gitops',
            },
            labels: { team: 'payments' },
          },
          spec: { title: 'Meta Dash', version: 3, panels: [] },
        },
      })
    );
    const metaRes = await fetchExistingDashboardByUid('dash-meta');
    expect(metaRes.preservedAnnotations).toEqual({ 'grafana.app/managedBy': 'gitops' });
    expect(metaRes.preservedLabels).toEqual({ team: 'payments' });
  });

  test('defeats adversarial import edge cases: nested XSS, multi-line PromQL, Loki null-ds smuggling, filtered label_values, $__all, undeclared $vars (H-2, H-3, H-4)', () => {
    const adversarial = {
      title: 'Nested <scr<script>ipt>ononclick=alert(1) Dash',
      time: { from: 'now-15m', to: 'now' },
      templating: {
        list: [
          {
            // H-4: filtered label_values MUST be rejected (must not silently widen scope)
            name: 'tenant_svc',
            type: 'query',
            datasource: { type: 'prometheus', uid: 'prom-main' },
            query: 'label_values(http_requests_total{tenant="a"}, service)',
            current: { value: 'checkout-api' },
          },
          {
            // $__all without concrete fallback option MUST be skipped
            name: 'all_only_var',
            type: 'query',
            datasource: { type: 'prometheus', uid: 'prom-main' },
            query: 'label_values(http_requests_total, env)',
            current: { value: '$__all' },
          },
          {
            name: 'service',
            type: 'query',
            datasource: { type: 'prometheus', uid: 'prom-main' },
            query: 'label_values(http_requests_total, service)',
            current: { value: '$__all' },
            options: [
              { value: '$__all', text: 'All' },
              { value: 'checkout-api', text: 'checkout-api' },
            ],
          },
        ],
      },
      panels: [
        {
          // H-3: Loki panel with target.datasource = null MUST NOT smuggle LogQL
          type: 'timeseries',
          title: 'Smuggled Loki Panel',
          datasource: { type: 'loki', uid: 'loki-prod' },
          gridPos: { x: 0, y: 0, w: 12, h: 8 },
          targets: [{ datasource: null, expr: '{app="checkout"} |= "error"' }],
        },
        {
          // H-3: Empty object datasource {} MUST be rejected (fail-closed)
          type: 'stat',
          title: 'Empty DS Panel',
          datasource: {},
          gridPos: { x: 12, y: 0, w: 12, h: 6 },
          targets: [{ expr: 'up' }],
        },
        {
          // Undeclared variable reference must be skipped cleanly
          type: 'stat',
          title: 'Undeclared Var Panel',
          datasource: { type: 'prometheus', uid: 'prom-main' },
          gridPos: { x: 0, y: 8, w: 12, h: 6 },
          targets: [{ expr: 'sum(rate(http_requests_total{service="$tenant_svc"}[5m]))' }],
        },
        {
          // Valid multi-line PromQL panel with boolean/empty thresholds filtered out
          type: 'timeseries',
          title: 'Multi-line Valid Panel',
          datasource: { type: 'prometheus', uid: 'prom-main' },
          gridPos: { x: 12, y: 8, w: 12, h: 8 },
          fieldConfig: {
            defaults: {
              unit: 'reqps',
              thresholds: {
                steps: [
                  { value: true, color: 'red' },
                  { value: '', color: 'yellow' },
                  { value: 10, color: 'green' },
                ],
              },
            },
          },
          targets: [
            {
              expr: 'sum by (status) (\n\trate(http_requests_total{service="$service"}[5m])\n)',
              legendFormat: '{{status}} $service',
            },
          ],
        },
      ],
    };

    const { draft, report } = convertGrafanaDashboardToIR(adversarial, {
      selectedDatasourceUid: 'prom-main',
      selectedDatasourceName: 'Prometheus Main',
    });

    // Nested XSS angle brackets and event handler removed cleanly
    expect(draft.title).not.toContain('<');
    expect(draft.title).not.toContain('>');
    expect(draft.title.toLowerCase()).not.toContain('onclick=');

    // Only concrete safe `service` variable imported; filtered & $__all-only skipped
    expect(draft.variables).toHaveLength(1);
    expect(draft.variables?.[0]).toMatchObject({
      name: 'service',
      defaultValue: 'checkout-api',
    });
    expect(report.skippedVariables).toHaveLength(2);

    // Only valid multi-line PromQL panel imported; Loki, empty DS, and undeclared-var panels skipped
    expect(draft.panels).toHaveLength(1);
    expect(draft.panels[0].query?.expr).toBe(
      'sum by (status) ( rate(http_requests_total{service="$service"}[5m]) )'
    );
    // legendFormat with $var stripped; boolean/empty thresholds filtered out
    expect(draft.panels[0].query?.legendFormat).toBeUndefined();
    expect(draft.panels[0].thresholds).toEqual([{ value: 10, color: 'green' }]);
    expect(report.skippedPanels).toHaveLength(3);
    expect(validateIRClient(draft)).toEqual([]);
  });

  test('imports VictoriaMetrics MetricsQL dashboards: stamps query.kind, skips hide=2 vars and withTemplate targets, and parses unfiltered qryType:1 object variables', () => {
    const vmDashboard = {
      title: 'VictoriaMetrics Service Overview',
      time: { from: 'now-15m', to: 'now' },
      templating: {
        list: [
          {
            name: 'service',
            type: 'query',
            datasource: { type: 'victoriametrics-metrics-datasource', uid: 'vm-ds' },
            query: { qryType: 1, label: 'service', metric: 'http_requests_total' },
            current: { value: 'checkout' },
          },
          {
            name: 'filtered_service',
            type: 'query',
            datasource: { type: 'victoriametrics-metrics-datasource', uid: 'vm-ds' },
            query: {
              qryType: 1,
              label: 'service',
              metric: 'http_requests_total',
              labelFilters: [{ label: 'tenant', operator: '=', value: 'acme' }],
            },
            current: { value: 'checkout' },
          },
          {
            name: 'hidden_tenant',
            type: 'query',
            hide: 2,
            datasource: { type: 'victoriametrics-metrics-datasource', uid: 'vm-ds' },
            query: 'label_values(http_requests_total, tenant)',
            current: { value: 'acme' },
          },
        ],
      },
      panels: [
        {
          type: 'timeseries',
          title: 'Checkout Pure Rate',
          datasource: { type: 'victoriametrics-metrics-datasource', uid: 'vm-ds' },
          gridPos: { x: 0, y: 0, w: 12, h: 8 },
          fieldConfig: { defaults: { unit: 'reqps' } },
          targets: [
            {
              expr: 'sum(rate(http_requests_total{service="$service"}[5m])) default 0',
              legendFormat: '{{service}}',
            },
          ],
        },
        {
          type: 'timeseries',
          title: 'Panel With Template',
          datasource: { type: 'victoriametrics-metrics-datasource', uid: 'vm-ds' },
          gridPos: { x: 12, y: 0, w: 12, h: 8 },
          targets: [
            {
              expr: 'my_macro(http_requests_total)',
              withTemplate: 'WITH (my_macro(x) = rate(x[5m]))',
            },
          ],
        },
      ],
    };

    const { draft, report } = convertGrafanaDashboardToIR(vmDashboard, {
      selectedDatasourceUid: 'vm-ds',
      selectedDatasourceName: 'VictoriaMetrics',
      queryKind: 'metricsql',
    });

    // Only unfiltered object-form qryType:1 variable imported; filtered and hide=2 skipped
    expect(draft.variables).toHaveLength(1);
    expect(draft.variables?.[0]).toMatchObject({
      name: 'service',
      type: 'query',
      labelName: 'service',
      metricName: 'http_requests_total',
      defaultValue: 'checkout',
    });
    expect(report.skippedVariables).toHaveLength(2);
    expect(report.skippedVariables.some((v) => v.title === 'hidden_tenant')).toBe(true);
    expect(report.skippedVariables.some((v) => v.title === 'filtered_service')).toBe(true);

    // Valid MetricsQL panel stamped with kind: 'metricsql'; target.withTemplate panel skipped
    expect(draft.panels).toHaveLength(1);
    expect(draft.panels[0].query?.kind).toBe('metricsql');
    expect(draft.panels[0].query?.expr).toBe(
      'sum(rate(http_requests_total{service="$service"}[5m])) default 0'
    );
    expect(report.skippedPanels).toHaveLength(1);
    expect(report.skippedPanels[0].title).toBe('Panel With Template');
    expect(report.warnings.some((w) => w.includes('WITH template'))).toBe(true);
    expect(validateIRClient(draft)).toEqual([]);
  });
});
