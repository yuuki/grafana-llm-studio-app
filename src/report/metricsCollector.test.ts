import { collectDashboardMetricEvidence, parseFramesToSeries } from './metricsCollector';
import { ExtractedReportPanel } from './dashboardExtractor';

const mockPost = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    post: (...args: unknown[]) => mockPost(...args),
  }),
}));

describe('report/metricsCollector', () => {
  beforeEach(() => {
    mockPost.mockReset();
  });

  test('parseFramesToSeries parses wide DataFrames and filters NaN/Infinity/null values', () => {
    const frames = [
      {
        name: 'wide',
        schema: {
          fields: [
            { name: 'Time', type: 'time' },
            { name: 'Value', type: 'number', labels: { service: 'checkout-api', status: '500' } },
            { name: 'Value', type: 'number', labels: { service: 'checkout-api', status: '200' } },
          ],
        },
        data: {
          values: [
            [1700000000000, 1700000060000, 1700000120000, 1700000180000],
            [1, null, Number.NaN, 5],
            [100, Number.POSITIVE_INFINITY, 102, 104],
          ],
        },
      },
    ];

    const series = parseFramesToSeries(frames);
    expect(series).toHaveLength(2);
    expect(series[0].labelKey).toBe('service=checkout-api, status=500');
    expect(series[0].values).toEqual([1, 5]);
    expect(series[1].values).toEqual([100, 102, 104]);
  });

  test('computes preceding baseline delta, ranks up==0 & inverted thresholds as critical, and skips baseline on current query error', async () => {
    const panels: ExtractedReportPanel[] = [
      {
        panelKey: 'panel-1',
        title: 'Service Up Availability',
        thresholds: {
          mode: 'absolute',
          steps: [
            { value: null, color: 'red' },
            { value: 1, color: 'green' },
          ],
        },
        queries: [{ refId: 'A', expr: 'up{service="checkout-api"}', datasourceUid: 'prom-main', datasourceType: 'prometheus' }],
      },
      {
        panelKey: 'panel-2',
        title: 'HTTP Error Rate',
        unit: 'percentunit',
        thresholds: {
          mode: 'absolute',
          steps: [
            { value: null, color: 'green' },
            { value: 0.05, color: 'red' },
          ],
        },
        queries: [{ refId: 'A', expr: 'rate(http_errors_total[5m])', datasourceUid: 'prom-main', datasourceType: 'prometheus' }],
      },
      {
        panelKey: 'panel-3',
        title: 'Broken Panel',
        queries: [{ refId: 'A', expr: 'broken_metric', datasourceUid: 'prom-main', datasourceType: 'prometheus' }],
      },
    ];

    const nowMs = Date.parse('2026-11-27T12:00:00Z');
    mockPost.mockImplementation(async (_url: string, body: { from: string; to: string; queries: Array<{ refId: string; expr: string }> }) => {
      const q = body.queries[0];
      const isBaseline = Number(body.to) < nowMs;
      if (q.expr === 'broken_metric') {
        return {
          results: {
            [q.refId]: { status: 400, error: 'parse error' },
          },
        };
      }
      if (q.expr.startsWith('up{')) {
        const vals = isBaseline ? [1, 1, 1, 1] : [1, 1, 0, 0];
        return {
          results: {
            [q.refId]: {
              frames: [
                {
                  schema: {
                    fields: [
                      { name: 'Time', type: 'time' },
                      { name: 'Value', type: 'number', labels: { instance: 'pod-1' } },
                    ],
                  },
                  data: {
                    values: [[nowMs - 180000, nowMs - 120000, nowMs - 60000, nowMs], vals],
                  },
                },
              ],
            },
          },
        };
      }
      const vals = isBaseline ? [0.01, 0.01, 0.01, 0.01] : [0.02, 0.04, 0.09, 0.12];
      return {
        results: {
          [q.refId]: {
            frames: [
              {
                schema: {
                  fields: [
                    { name: 'Time', type: 'time' },
                    { name: 'Value', type: 'number', labels: { service: 'checkout-api' } },
                  ],
                },
                data: {
                  values: [[nowMs - 180000, nowMs - 120000, nowMs - 60000, nowMs], vals],
                },
              },
            ],
          },
        },
      };
    });

    const res = await collectDashboardMetricEvidence(panels, {
      window: '1h',
      baselineMode: 'preceding',
      nowMs,
      maxConcurrency: 2,
    });

    // Broken Panel must NOT trigger a baseline query (total calls = 2 + 2 + 1 = 5)
    expect(mockPost).toHaveBeenCalledTimes(5);
    expect(res.analyzedPanelCount).toBe(3);

    const upMetric = res.metrics.find((m) => m.panelTitle === 'Service Up Availability')!;
    expect(upMetric.thresholdBreached).toBe('critical');
    expect(upMetric.anomalyHint).toBe('degraded');
    expect(upMetric.baseline?.avg).toBe(1);
    expect(upMetric.deltaPctAvg).toBe(-50);

    const errMetric = res.metrics.find((m) => m.panelTitle === 'HTTP Error Rate')!;
    expect(errMetric.thresholdBreached).toBe('critical');
    expect(errMetric.baseline?.avg).toBe(0.01);
    expect(errMetric.deltaPctAvg).toBeCloseTo(575, 0);

    const brokenMetric = res.metrics.find((m) => m.panelTitle === 'Broken Panel')!;
    expect(brokenMetric.queryStatus).toBe('error');
    expect(brokenMetric.errorText).toContain('parse error');
  });

  test('does not execute baseline queries when baselineMode is none and computes missingRatio from expected steps', async () => {
    const panels: ExtractedReportPanel[] = [
      {
        panelKey: 'panel-1',
        title: 'Queue Depth',
        queries: [{ refId: 'A', expr: 'queue_depth', datasourceUid: 'prom-main', datasourceType: 'prometheus' }],
      },
    ];
    mockPost.mockResolvedValue({
      results: {
        A: {
          frames: [
            {
              schema: {
                fields: [
                  { name: 'Time', type: 'time' },
                  { name: 'Value', type: 'number', labels: { queue: 'orders' } },
                ],
              },
              data: { values: [[1700000000000, 1700000060000], [5, 7]] },
            },
          ],
        },
      },
    });

    const res = await collectDashboardMetricEvidence(panels, {
      window: '15m',
      baselineMode: 'none',
    });
    expect(mockPost).toHaveBeenCalledTimes(1);
    expect(res.metrics).toHaveLength(1);
    expect(res.metrics[0].baseline).toBeUndefined();
    expect(res.metrics[0].current?.missingRatio).toBeGreaterThan(0);
  });
});
