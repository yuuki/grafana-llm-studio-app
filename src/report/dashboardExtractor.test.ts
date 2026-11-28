import { extractDashboardReportQueries } from './dashboardExtractor';

const mockGetInstanceSettings = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getDataSourceSrv: () => ({
    getInstanceSettings: (...args: unknown[]) => mockGetInstanceSettings(...args),
  }),
}));

describe('report/dashboardExtractor', () => {
  beforeEach(() => {
    mockGetInstanceSettings.mockReset();
    mockGetInstanceSettings.mockImplementation((uid: string | null) => {
      if (uid === null) {
        return { uid: 'prom-default', type: 'prometheus' };
      }
      if (uid === 'prom-main' || uid === 'prom-default') {
        return { uid, type: 'prometheus' };
      }
      if (uid === 'loki-main') {
        return { uid, type: 'loki' };
      }
      return undefined;
    });
  });

  test('preserves $__rate_interval, resolves null default & ${DS_PROM} datasources, interpolates scope vars, and keeps raw thresholds', () => {
    const rawDashboard = {
      title: 'Checkout Production',
      templating: {
        list: [
          { name: 'env', type: 'custom', current: { value: 'prod' } },
          { name: 'service', type: 'query', current: { value: 'old-svc' } },
        ],
      },
      panels: [
        {
          id: 1,
          type: 'timeseries',
          title: 'HTTP Error Rate',
          datasource: null, // default datasource -> prom-default
          fieldConfig: {
            defaults: {
              unit: 'percentunit',
              thresholds: {
                mode: 'absolute',
                steps: [
                  { value: null, color: 'green' },
                  { value: 0.05, color: 'red' },
                ],
              },
            },
          },
          targets: [
            {
              refId: 'A',
              expr: 'sum(rate(http_requests_total{service="$service",env="$env",status=~"5.."}[$__rate_interval]))',
            },
          ],
        },
        {
          id: 2,
          type: 'row',
          title: 'Collapsed Row',
          panels: [
            {
              id: 3,
              type: 'stat',
              title: 'P95 Latency',
              datasource: { uid: '${DS_PROMETHEUS}', type: 'prometheus' },
              targets: [
                {
                  refId: 'A',
                  expr: 'histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{service="${service:regex}"}[$__rate_interval])) by (le))',
                },
              ],
            },
            {
              id: 4,
              type: 'logs',
              title: 'Loki Logs (Unsupported)',
              datasource: { uid: 'loki-main', type: 'loki' },
              targets: [{ refId: 'A', expr: '{app="checkout"}' }],
            },
          ],
        },
      ],
    };

    const res = extractDashboardReportQueries(rawDashboard, {
      selectedDatasourceUid: 'prom-main',
      allowedDatasourceUids: ['prom-default', 'prom-main'],
      scope: { labelName: 'service', labelValue: 'checkout.api' },
    });

    expect(res.dashboardTitle).toBe('Checkout Production');
    expect(res.panels).toHaveLength(2);
    expect(res.skippedPanelCount).toBe(1);

    const errPanel = res.panels.find((p) => p.title === 'HTTP Error Rate')!;
    expect(errPanel.queries[0].datasourceUid).toBe('prom-default');
    expect(errPanel.queries[0].expr).toContain('service="checkout.api"');
    expect(errPanel.queries[0].expr).toContain('env="prod"');
    expect(errPanel.queries[0].expr).toContain('[$__rate_interval]');
    expect(errPanel.thresholds).toEqual({
      mode: 'absolute',
      steps: [
        { value: null, color: 'green' },
        { value: 0.05, color: 'red' },
      ],
    });

    const p95Panel = res.panels.find((p) => p.title === 'P95 Latency')!;
    expect(p95Panel.queries[0].datasourceUid).toBe('prom-main');
    expect(p95Panel.queries[0].expr).toContain('service="checkout\\.api"');
    expect(p95Panel.queries[0].expr).toContain('[$__rate_interval]');
  });

  test('never throws when a dashboard has zero compatible or allowlisted panels', () => {
    const res = extractDashboardReportQueries(
      {
        title: 'Logs Only',
        panels: [
          {
            id: 1,
            type: 'logs',
            title: 'App Logs',
            datasource: { uid: 'loki-main', type: 'loki' },
            targets: [{ refId: 'A', expr: '{job="varlogs"}' }],
          },
        ],
      },
      {
        selectedDatasourceUid: 'prom-main',
        allowedDatasourceUids: ['prom-main'],
      }
    );

    expect(res.panels).toEqual([]);
    expect(res.skippedPanelCount).toBe(1);
    expect(res.warnings.length).toBeGreaterThan(0);
  });
});
