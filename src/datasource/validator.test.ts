import { Observable, of } from 'rxjs';
import { DashboardIR, MetricCatalogItem } from '../ir/types';
import { normalizeDashboardIR } from '../spec/normalize';
import { selectMetricCandidates } from './bm25';
import { classifyDatasourceError, runDryRunValidation } from './validator';

const mockFetch = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    fetch: mockFetch,
  }),
}));

describe('Dry-run validation concurrency and error classification', () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  test('enforces maxConcurrency=2 across 6 panels', async () => {
    let inFlight = 0;
    let peakInFlight = 0;

    mockFetch.mockImplementation(() => {
      return new Observable((subscriber) => {
        inFlight++;
        if (inFlight > peakInFlight) {
          peakInFlight = inFlight;
        }
        const timer = setTimeout(() => {
          inFlight--;
          subscriber.next({
            data: {
              results: {
                A: {
                  frames: [{ data: { values: [[1, 2], [10, 20]] } }],
                },
              },
            },
          });
          subscriber.complete();
        }, 25);
        return () => clearTimeout(timer);
      });
    });

    const sixPanelsIR: DashboardIR = {
      schemaVersion: '1.0',
      revision: 1,
      title: 'Six Panels',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: Array.from({ length: 6 }, (_, i) => ({
        id: `panel-p${i}`,
        type: 'timeseries',
        title: `Panel ${i}`,
        datasourceRef: 'selected_prometheus',
        query: { expr: `rate(http_requests_total{service="s"}[5m])` },
        unit: 'reqps',
        layout: { row: Math.floor(i / 2), col: (i % 2) as 0 | 1, width: 'half' },
      })),
    };

    const spec = normalizeDashboardIR(sixPanelsIR, { uid: 'prom-main', type: 'prometheus' });
    const results = await runDryRunValidation(spec, { maxConcurrency: 2, timeoutMs: 5000 });

    expect(results).toHaveLength(6);
    expect(peakInFlight).toBeLessThanOrEqual(2);
    expect(results.every((r) => r.category === 'ok')).toBe(true);
  });

  test('classifies empty_result, syntax_error, permission_error, and timeout accurately', async () => {
    expect(classifyDatasourceError(403, 'Access denied')).toBe('permission_error');
    expect(classifyDatasourceError(401, 'Unauthorized')).toBe('permission_error');
    expect(classifyDatasourceError(504, 'Gateway timeout')).toBe('timeout');
    expect(classifyDatasourceError(400, 'parse error: unexpected ")"')).toBe('syntax_error');

    mockFetch.mockReturnValueOnce(
      of({
        data: {
          results: {
            A: { frames: [{ data: { values: [] } }] },
          },
        },
      })
    );
    const irDoc: DashboardIR = {
      schemaVersion: '1.0',
      revision: 1,
      title: 'Empty Test',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-empty',
          type: 'stat',
          title: 'Empty',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'up{service="none"}' },
          unit: 'short',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };
    const spec = normalizeDashboardIR(irDoc, { uid: 'prom-main', type: 'prometheus' });
    const res = await runDryRunValidation(spec, { maxConcurrency: 2 });
    expect(res[0].category).toBe('empty_result');
  });

  test('BM25 selects relevant RED metrics for Japanese prompts within budget', () => {
    const catalog: MetricCatalogItem[] = [
      { name: 'go_gc_duration_seconds', type: 'summary', help: 'GC pause duration' },
      { name: 'http_requests_total', type: 'counter', help: 'Total HTTP requests', labels: ['service', 'status'] },
      { name: 'http_request_duration_seconds_bucket', type: 'histogram', help: 'HTTP latency histogram', labels: ['service', 'le'] },
      { name: 'disk_io_time_seconds_total', type: 'counter', help: 'Disk IO time' },
    ];
    const ranked = selectMetricCandidates(catalog, 'リクエスト数とエラー率とレイテンシを見たい', {
      maxItems: 2,
    });
    const names = ranked.map((r) => r.name);
    expect(names).toContain('http_requests_total');
    expect(names).toContain('http_request_duration_seconds_bucket');
  });
});
