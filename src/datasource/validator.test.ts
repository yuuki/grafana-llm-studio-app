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

  test('validates multi-query panels in one /api/ds/query request with variable interpolation and per-refId error surfacing', async () => {
    mockFetch
      .mockReturnValueOnce(
        of({
          data: {
            results: {
              A: { frames: [{ data: { values: [[1, 2], [0.1, 0.2]] } }] },
              B: { frames: [{ data: { values: [[1, 2], [0.5, 0.6]] } }] },
            },
          },
        })
      )
      .mockReturnValueOnce(
        of({
          data: {
            results: {
              A: { frames: [{ data: { values: [[1, 2], [0.1, 0.2]] } }] },
              B: { status: 400, error: 'parse error: unexpected ")"' },
            },
          },
        })
      );

    const multiVarIR: DashboardIR = {
      schemaVersion: '1.1',
      revision: 1,
      title: 'Multi-query + Vars',
      timeRange: { from: 'now-15m', to: 'now' },
      variables: [
        {
          name: 'service',
          type: 'query',
          datasourceRef: 'selected_prometheus',
          labelName: 'service',
          defaultValue: 'checkout-api',
        },
      ],
      panels: [
        {
          id: 'panel-lat',
          type: 'timeseries',
          title: 'Latency',
          datasourceRef: 'selected_prometheus',
          queries: [
            { refId: 'A', expr: 'histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))' },
            { refId: 'B', expr: 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))' },
          ],
          unit: 's',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };

    const spec = normalizeDashboardIR(multiVarIR, { uid: 'prom-main', type: 'prometheus' });
    const okRes = await runDryRunValidation(spec, { maxConcurrency: 1 });
    expect(okRes).toHaveLength(1);
    expect(okRes[0].category).toBe('ok');
    expect(okRes[0].seriesCount).toBe(2);

    // Verify variable interpolation happened before /api/ds/query was called.
    const sentQueries = mockFetch.mock.calls[0][0].data.queries as Array<{ refId: string; expr: string }>;
    expect(sentQueries).toHaveLength(2);
    expect(sentQueries[0].refId).toBe('A');
    expect(sentQueries[0].expr).toContain('service="checkout-api"');
    expect(sentQueries[1].refId).toBe('B');
    expect(sentQueries[1].expr).toContain('service="checkout-api"');

    // Second run: refId B fails with syntax_error -> surfaced with [B] prefix.
    const errRes = await runDryRunValidation(spec, { maxConcurrency: 1 });
    expect(errRes[0].category).toBe('syntax_error');
    expect(errRes[0].errorText).toContain('[B] parse error');
  });

  test('enforces permission/timeout precedence over syntax_error, handles metric names containing timeout/permission, and flags missing refId (H-5)', async () => {
    // Metric names like http_timeout_total or auth_permission_checks_total in 400 parse errors MUST stay syntax_error
    expect(
      classifyDatasourceError(400, 'parse error at char 5: unexpected metric http_timeout_total')
    ).toBe('syntax_error');
    expect(
      classifyDatasourceError(400, 'bad_data: parse error near auth_permission_checks_total')
    ).toBe('syntax_error');

    const twoQueryIR: DashboardIR = {
      schemaVersion: '1.1',
      revision: 1,
      title: 'Error Precedence',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-multi',
          type: 'timeseries',
          title: 'Multi',
          datasourceRef: 'selected_prometheus',
          queries: [
            { refId: 'A', expr: 'up' },
            { refId: 'B', expr: 'up' },
          ],
          unit: 'short',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };
    const spec = normalizeDashboardIR(twoQueryIR, { uid: 'prom-main', type: 'prometheus' });

    // 1. Non-throwing response with A=403 forbidden + B=400 parse error -> permission_error dominates (never triggers auto-repair)
    mockFetch.mockReturnValueOnce(
      of({
        data: {
          results: {
            A: { status: 403, error: 'Forbidden' },
            B: { status: 400, error: 'parse error: unexpected ")"' },
          },
        },
      })
    );
    const res1 = await runDryRunValidation(spec, { maxConcurrency: 1 });
    expect(res1[0].category).toBe('permission_error');

    // 2. Throwing FetchError with A=400 parse error + B=403 forbidden -> permission_error still dominates
    mockFetch.mockReturnValueOnce(
      throwError(() => ({
        status: 400,
        data: {
          results: {
            A: { status: 400, error: 'parse error: unexpected ")"' },
            B: { status: 403, error: 'Forbidden' },
          },
        },
      }))
    );
    const res2 = await runDryRunValidation(spec, { maxConcurrency: 1 });
    expect(res2[0].category).toBe('permission_error');

    // 3. Missing refId B in response -> datasource_error instead of silent ok/empty_result
    mockFetch.mockReturnValueOnce(
      of({
        data: {
          results: {
            A: { frames: [{ data: { values: [[1], [1]] } }] },
          },
        },
      })
    );
    const res3 = await runDryRunValidation(spec, { maxConcurrency: 1 });
    expect(res3[0].category).toBe('datasource_error');
    expect(res3[0].errorText).toContain('[B]');
  });

  test('localizes dry-run query timeout messages across en and ja locales', async () => {
    const { DEFAULT_LOCALE, setStudioLocale } = await import('../i18n');
    mockFetch.mockImplementation(() => new Observable(() => {}));

    const irDoc: DashboardIR = {
      schemaVersion: '1.0',
      revision: 1,
      title: 'Timeout Test',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-slow',
          type: 'stat',
          title: 'Slow',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'up' },
          unit: 'short',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };
    const spec = normalizeDashboardIR(irDoc, { uid: 'prom-main', type: 'prometheus' });

    try {
      await setStudioLocale('en');
      const enRes = await runDryRunValidation(spec, { maxConcurrency: 1, timeoutMs: 1000 });
      expect(enRes[0].category).toBe('timeout');
      expect(enRes[0].errorText).toBe('Validation query timed out (1s)');

      await setStudioLocale('ja-JP');
      const jaRes = await runDryRunValidation(spec, { maxConcurrency: 1, timeoutMs: 1000 });
      expect(jaRes[0].category).toBe('timeout');
      expect(jaRes[0].errorText).toBe('検証クエリがタイムアウトしました (1s)');
    } finally {
      await setStudioLocale(DEFAULT_LOCALE);
    }
  });

  test('hardens VictoriaMetrics HTTP 400/422 classification to prevent false syntax_error repairs', () => {
    // Transport / DNS / TLS / proxy errors returned as HTTP 400 by VM datasource plugin -> datasource_error
    expect(classifyDatasourceError(400, 'dial tcp 10.0.0.5:8428: connect: connection refused')).toBe(
      'datasource_error'
    );
    expect(classifyDatasourceError(400, 'Post "https://vm:8428/api/v1/query_range": no such host')).toBe(
      'datasource_error'
    );
    expect(classifyDatasourceError(400, 'x509: certificate signed by unknown authority')).toBe(
      'datasource_error'
    );
    // VM HTTP 422 timeout -> timeout (never syntax_error)
    expect(
      classifyDatasourceError(422, 'cannot execute query: timeout exceeded (search.maxQueryDuration=30s)')
    ).toBe('timeout');
    // VM HTTP 422 resource limit -> datasource_error (never syntax_error)
    expect(
      classifyDatasourceError(
        422,
        'error when executing query: the number of matching unique timeseries exceeds -search.maxUniqueTimeseries=300000'
      )
    ).toBe('datasource_error');
    // VM HTTP 422 explicit syntax / function error -> syntax_error
    expect(classifyDatasourceError(422, 'cannot parse query: unexpected token ")"')).toBe('syntax_error');
    expect(classifyDatasourceError(422, 'unsupported function "foo_bar"')).toBe('syntax_error');
    // Metric name containing "timeout" in parse error stays syntax_error
    expect(classifyDatasourceError(400, 'parse error: http_timeout_total')).toBe('syntax_error');
    // Unknown HTTP 400 without parse/syntax markers defaults safely to datasource_error
    expect(classifyDatasourceError(400, 'Bad Request')).toBe('datasource_error');
  });
});
