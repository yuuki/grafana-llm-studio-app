import { Observable, of, throwError } from 'rxjs';
import { collectDashboardMetricEvidence, parseFramesToSeries } from './metricsCollector';
import { ExtractedReportPanel } from './dashboardExtractor';

const mockFetch = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    fetch: (...args: unknown[]) => mockFetch(...args),
  }),
}));

describe('report/metricsCollector', () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  test('parseFramesToSeries parses wide & long DataFrames and rejects NaN, Infinity, null, and empty/whitespace strings', () => {
    const wideFrames = [
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
            [1, '', '   ', 5],
            [100, Number.POSITIVE_INFINITY, Number.NaN, 104],
          ],
        },
      },
    ];

    const wideSeries = parseFramesToSeries(wideFrames);
    expect(wideSeries).toHaveLength(2);
    expect(wideSeries[0].labelKey).toBe('service=checkout-api, status=500');
    // Empty and whitespace strings MUST NOT become 0 (M-1)
    expect(wideSeries[0].values).toEqual([1, 5]);
    expect(wideSeries[1].values).toEqual([100, 104]);

    const longFrames = [
      {
        name: 'long',
        schema: {
          fields: [
            { name: 'Time', type: 'time' },
            { name: 'service', type: 'string' },
            { name: 'Value', type: 'number' },
          ],
        },
        data: {
          values: [
            [1700000000000, 1700000000000, 1700000060000, 1700000060000],
            ['api-a', 'api-b', 'api-a', 'api-b'],
            [10, 90, 12, 95],
          ],
        },
      },
    ];
    const longSeries = parseFramesToSeries(longFrames);
    expect(longSeries).toHaveLength(2);
    expect(longSeries.find((s) => s.labelKey === 'service=api-a')?.values).toEqual([10, 12]);
    expect(longSeries.find((s) => s.labelKey === 'service=api-b')?.values).toEqual([90, 95]);
  });

  test('computes baseline deltas, ranks zero-baseline spikes and band thresholds, extracts FetchError inner message, and expands $__from/$__to', async () => {
    const panels: ExtractedReportPanel[] = [
      {
        panelKey: 'panel-1',
        title: 'Band Threshold Pool Usage',
        thresholds: {
          mode: 'absolute',
          steps: [
            { value: null, color: 'red' },
            { value: 0.2, color: 'green' },
            { value: 0.9, color: 'red' },
          ],
        },
        queries: [
          {
            refId: 'A',
            expr: 'pool_usage{from="$__from",to="$__to"}',
            datasourceUid: 'prom-main',
            datasourceType: 'prometheus',
          },
        ],
      },
      {
        panelKey: 'panel-2',
        title: '5xx Emergence (Zero Baseline)',
        queries: [{ refId: 'A', expr: 'rate(http_5xx_total[5m])', datasourceUid: 'prom-main', datasourceType: 'prometheus' }],
      },
      {
        panelKey: 'panel-3',
        title: 'Broken Panel',
        queries: [{ refId: 'A', expr: 'broken_metric', datasourceUid: 'prom-main', datasourceType: 'prometheus' }],
      },
    ];

    const nowMs = Date.parse('2026-11-27T12:00:00Z');
    mockFetch.mockImplementation(
      (req: { abortSignal?: AbortSignal; data: { from: string; to: string; queries: Array<{ refId: string; expr: string }> } }) => {
        expect(req.abortSignal).toBeDefined();
        const q = req.data.queries[0];
        const isBaseline = Number(req.data.to) < nowMs;
        if (q.expr === 'broken_metric') {
          // Simulate Grafana HTTP 400 FetchError (plain object, NOT instanceof Error)
          return throwError(() => ({
            status: 400,
            data: { results: { [q.refId]: { error: 'bad_data: parse error at char 12' } } },
          }));
        }
        if (q.expr.startsWith('pool_usage')) {
          expect(q.expr).not.toContain('$__from');
          expect(q.expr).not.toContain('$__to');
          // Latest recovered to 0.5 (green), but max spiked to 0.95 (red upper band!)
          const vals = isBaseline ? [0.5, 0.5, 0.5, 0.5] : [0.5, 0.95, 0.6, 0.5];
          return of({
            data: {
              results: {
                [q.refId]: {
                  frames: [
                    {
                      schema: {
                        fields: [
                          { name: 'Time', type: 'time' },
                          { name: 'Value', type: 'number', labels: { pool: 'db' } },
                        ],
                      },
                      data: {
                        values: [[nowMs - 180000, nowMs - 120000, nowMs - 60000, nowMs], vals],
                      },
                    },
                  ],
                },
              },
            },
          });
        }
        // 5xx went from baseline 0 to 120/s -> must be marked degraded even without thresholds!
        const vals = isBaseline ? [0, 0, 0, 0] : [50, 120, 140, 170];
        return of({
          data: {
            results: {
              [q.refId]: {
                frames: [
                  {
                    schema: {
                      fields: [
                        { name: 'Time', type: 'time' },
                        { name: 'Value', type: 'number', labels: { status: '500' } },
                      ],
                    },
                    data: {
                      values: [[nowMs - 180000, nowMs - 120000, nowMs - 60000, nowMs], vals],
                    },
                  },
                ],
              },
            },
          },
        });
      }
    );

    const res = await collectDashboardMetricEvidence(panels, {
      window: '1h',
      baselineMode: 'preceding',
      nowMs,
      maxConcurrency: 2,
    });

    // Broken Panel skips baseline (2 + 2 + 1 = 5 calls)
    expect(mockFetch).toHaveBeenCalledTimes(5);
    expect(res.analyzedPanelCount).toBe(3);

    const bandMetric = res.metrics.find((m) => m.panelTitle === 'Band Threshold Pool Usage')!;
    expect(bandMetric.thresholdBreached).toBe('critical');

    const zeroBaseMetric = res.metrics.find((m) => m.panelTitle === '5xx Emergence (Zero Baseline)')!;
    expect(zeroBaseMetric.baselineWasZero).toBe(true);
    expect(zeroBaseMetric.anomalyHint).toBe('degraded');

    const brokenMetric = res.metrics.find((m) => m.panelTitle === 'Broken Panel')!;
    expect(brokenMetric.queryStatus).toBe('error');
    expect(brokenMetric.errorText).toContain('bad_data: parse error at char 12');
  });

  test('unsubscribes in-flight BackendSrv.fetch Observable on external abort', async () => {
    const unsubSpy = jest.fn();
    mockFetch.mockImplementation(
      () =>
        new Observable(() => {
          return () => {
            unsubSpy();
          };
        })
    );
    const ctrl = new AbortController();
    const p = collectDashboardMetricEvidence(
      [
        {
          panelKey: 'p-1',
          title: 'Slow Query',
          queries: [{ refId: 'A', expr: 'up', datasourceUid: 'prom-main', datasourceType: 'prometheus' }],
        },
      ],
      { window: '15m', baselineMode: 'none', signal: ctrl.signal }
    );
    ctrl.abort();
    await expect(p).rejects.toThrow();
    expect(unsubSpy).toHaveBeenCalledTimes(1);
  });
});
