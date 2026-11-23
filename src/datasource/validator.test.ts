import { Observable, of, throwError } from 'rxjs';
import { DashboardIR, MetricCatalogItem } from '../ir/types';
import { normalizeDashboardIR } from '../spec/normalize';
import { selectMetricCandidates, tokenize } from './bm25';
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

  test('extracts detailed PromQL parse error from /api/ds/query HTTP 400 FetchError.data.results.A (H-10)', async () => {
    mockFetch.mockReturnValueOnce(
      throwError(() => ({
        status: 400,
        statusText: 'Bad Request',
        data: {
          message: 'Query data error',
          results: {
            A: {
              status: 400,
              error: 'bad_data: invalid parameter "query": 1:25: parse error: unexpected ")"',
            },
          },
        },
      }))
    );

    const irDoc: DashboardIR = {
      schemaVersion: '1.0',
      revision: 1,
      title: 'Syntax Error Test',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-bad',
          type: 'timeseries',
          title: 'Bad Query',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'rate(http_requests_total[5m])' },
          unit: 'reqps',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };
    const spec = normalizeDashboardIR(irDoc, { uid: 'prom-main', type: 'prometheus' });
    const res = await runDryRunValidation(spec, { maxConcurrency: 1 });
    expect(res[0].category).toBe('syntax_error');
    expect(res[0].errorText).toContain('parse error: unexpected ")"');
  });

  test('throws AbortError on external signal abort instead of misclassifying as timeout (M-12)', async () => {
    mockFetch.mockImplementation(
      () =>
        new Observable(() => {
          // Never emits; simulates slow network cancelled by user/org switch.
        })
    );

    const irDoc: DashboardIR = {
      schemaVersion: '1.0',
      revision: 1,
      title: 'Abort Test',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-abort',
          type: 'stat',
          title: 'Abort',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'up' },
          unit: 'short',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };
    const spec = normalizeDashboardIR(irDoc, { uid: 'prom-main', type: 'prometheus' });
    const controller = new AbortController();
    const promise = runDryRunValidation(spec, {
      maxConcurrency: 1,
      timeoutMs: 5000,
      signal: controller.signal,
    });
    setTimeout(() => controller.abort(), 15);

    await expect(promise).rejects.toMatchObject({ name: 'AbortError' });
  });

  test('BM25 splits mixed ASCII+Japanese tokens and skips oversized items without halting (L-3)', () => {
    expect(tokenize('5xxエラー率 gRPCレイテンシ CPU使用率')).toEqual([
      '5xx',
      'エラー率',
      'grpc',
      'レイテンシ',
      'cpu',
      '使用率',
    ]);

    const catalog: MetricCatalogItem[] = [
      { name: 'go_gc_duration_seconds', type: 'summary', help: 'GC pause duration' },
      { name: 'http_requests_total', type: 'counter', help: 'Total HTTP requests', labels: ['service', 'status'] },
      { name: 'http_request_duration_seconds_bucket', type: 'histogram', help: 'HTTP latency histogram', labels: ['service', 'le'] },
      { name: 'grpc_server_handling_seconds_bucket', type: 'histogram', help: 'gRPC latency histogram', labels: ['service', 'le'] },
      { name: 'disk_io_time_seconds_total', type: 'counter', help: 'Disk IO time' },
    ];
    const ranked = selectMetricCandidates(catalog, '5xxエラー率とgRPCレイテンシを見たい', {
      maxItems: 3,
    });
    const names = ranked.map((r) => r.name);
    expect(names).toContain('grpc_server_handling_seconds_bucket');
    expect(names).toContain('http_requests_total');

    // Oversized item should be skipped (`continue`), allowing smaller subsequent candidates in budget.
    const budgetCatalog: MetricCatalogItem[] = [
      { name: 'http_requests_giant_help', type: 'counter', help: 'request '.repeat(60) },
      { name: 'http_requests_total', type: 'counter', help: 'Total requests' },
    ];
    const budgeted = selectMetricCandidates(budgetCatalog, 'http requests', {
      maxItems: 5,
      maxTotalChars: 150,
    });
    expect(budgeted.map((m) => m.name)).toEqual(['http_requests_total']);
  });
});
