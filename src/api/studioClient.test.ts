import { of } from 'rxjs';
import {
  analyzeStatus,
  extractApiError,
  generateDraft,
  patchDraft,
  repairDraft,
  SseEventParser,
} from './studioClient';
import { JobSnapshot, JobStageEvent } from '../ir/types';

const mockBackendFetch = jest.fn();
const mockConfig: { appSubUrl?: string; bootData?: { user?: { orgId?: number } } } = {
  appSubUrl: '',
  bootData: { user: { orgId: 7 } },
};

jest.mock('@grafana/runtime', () => ({
  get config() {
    return mockConfig;
  },
  getBackendSrv: () => ({
    fetch: (...args: unknown[]) => mockBackendFetch(...args),
  }),
}));

function makeSnapshot(overrides: Partial<JobSnapshot> = {}): JobSnapshot {
  return {
    jobId: 'job-sse-1',
    status: 'awaiting_preview',
    datasourceUid: 'prom-main',
    repairAttemptsUsed: 0,
    maxRepairAttempts: 2,
    canUndo: false,
    updatedAt: '2026-11-24T00:00:00Z',
    ...overrides,
  };
}

function makeMockReadableStream(chunks: string[], failAfterChunk?: Error): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let idx = 0;
  return {
    getReader() {
      return {
        async read() {
          if (idx < chunks.length) {
            const value = encoder.encode(chunks[idx]);
            idx += 1;
            return { done: false, value };
          }
          if (failAfterChunk) {
            throw failAfterChunk;
          }
          return { done: true, value: undefined };
        },
        releaseLock() {
          // no-op
        },
      };
    },
  } as unknown as ReadableStream<Uint8Array>;
}

describe('SseEventParser', () => {
  test('parses frames across arbitrary chunk splits, CRLF, and keepalive comments', () => {
    const parser = new SseEventParser();
    const f1 = parser.push(': keepalive\r\n\r\nevent: sta');
    expect(f1).toEqual([]);

    const f2 = parser.push('ge\r\ndata: {"stage":"gene');
    expect(f2).toEqual([]);

    const f3 = parser.push('rating"}\r\n\r\nevent: result\ndata: {"ok":true}');
    expect(f3).toEqual([{ event: 'stage', data: '{"stage":"generating"}' }]);

    const f4 = parser.flush();
    expect(f4).toEqual([{ event: 'result', data: '{"ok":true}' }]);
  });
});

describe('studioClient optional SSE streaming and graceful fallback', () => {
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    mockBackendFetch.mockReset();
    mockConfig.appSubUrl = '';
    mockConfig.bootData = { user: { orgId: 7 } };
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  test('uses BackendSrv non-streaming JSON by default when enableSse is false/omitted', async () => {
    const nativeFetch = jest.fn();
    globalThis.fetch = nativeFetch as unknown as typeof fetch;

    const snap = makeSnapshot();
    mockBackendFetch.mockReturnValueOnce(of({ data: snap }));

    const result = await generateDraft({
      datasourceUid: 'prom-main',
      userIntent: 'RED metrics',
      catalog: [{ name: 'http_requests_total' }],
    });

    expect(result).toEqual(snap);
    expect(mockBackendFetch).toHaveBeenCalledTimes(1);
    expect(nativeFetch).not.toHaveBeenCalled();
  });

  test('streams incremental SSE stages and resolves final validated JobSnapshot with appSubUrl and org header', async () => {
    mockConfig.appSubUrl = '/grafana';
    const snap = makeSnapshot({ jobId: 'job-stream-42' });
    const stages: JobStageEvent[] = [];

    const stream = makeMockReadableStream([
      'event: stage\ndata: {"jobId":"job-stream-42","operation":"generate","stage":"generating","timestamp":"2026-11-24T00:00:00Z"}\n\n',
      ': keepalive\n\n',
      'event: stage\ndata: {"jobId":"job-stream-42","operation":"generate","stage":"validating_ir","timestamp":"2026-11-24T00:00:01Z"}\n\n',
      'event: stage\ndata: {"jobId":"job-stream-42","operation":"generate","stage":"awaiting_preview","timestamp":"2026-11-24T00:00:02Z"}\n\n',
      `event: result\ndata: ${JSON.stringify(snap)}\n\n`,
    ]);

    const nativeFetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream; charset=utf-8' }),
      body: stream,
    });
    globalThis.fetch = nativeFetch as unknown as typeof fetch;

    const result = await generateDraft(
      {
        datasourceUid: 'prom-main',
        userIntent: 'RED metrics',
        catalog: [{ name: 'http_requests_total' }],
      },
      undefined,
      {
        enableSse: true,
        onStage: (ev) => stages.push(ev),
      }
    );

    expect(result).toEqual(snap);
    expect(stages.map((s) => s.stage)).toEqual(['generating', 'validating_ir', 'awaiting_preview']);
    expect(nativeFetch).toHaveBeenCalledTimes(1);
    expect(nativeFetch.mock.calls[0][0]).toBe(
      '/grafana/api/plugins/yuuki-llmstudio-app/resources/jobs/generate'
    );
    expect(nativeFetch.mock.calls[0][1].headers).toEqual({
      'Content-Type': 'application/json',
      Accept: 'text/event-stream, application/json',
      'X-Grafana-Org-Id': '7',
    });
    expect(mockBackendFetch).not.toHaveBeenCalled();
  });

  test('handles direct application/json response without duplicate POST when server disables SSE', async () => {
    const snap = makeSnapshot({ jobId: 'job-json-only' });
    const nativeFetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => snap,
    });
    globalThis.fetch = nativeFetch as unknown as typeof fetch;

    const onFallback = jest.fn();
    const res = await patchDraft(
      'job-json-only',
      { baseRevision: 1, editIntent: 'make stat', catalog: [{ name: 'http_requests_total' }] },
      undefined,
      { enableSse: true, onFallback }
    );

    expect(res).toEqual(snap);
    expect(nativeFetch).toHaveBeenCalledTimes(1);
    expect(mockBackendFetch).not.toHaveBeenCalled();
    expect(onFallback).not.toHaveBeenCalled();
  });

  test('falls back gracefully to non-streaming BackendSrv on HTTP 406/501 or missing fetch', async () => {
    const snap = makeSnapshot({ jobId: 'job-fallback-1' });
    const nativeFetch = jest.fn().mockResolvedValueOnce({
      ok: false,
      status: 406,
      statusText: 'Not Acceptable',
      headers: new Headers({ 'Content-Type': 'text/plain' }),
      text: async () => 'text/event-stream not supported',
    });
    globalThis.fetch = nativeFetch as unknown as typeof fetch;
    mockBackendFetch.mockReturnValueOnce(of({ data: snap }));

    const reasons: string[] = [];
    const res = await generateDraft(
      {
        datasourceUid: 'prom-main',
        userIntent: 'RED metrics',
        catalog: [{ name: 'http_requests_total' }],
      },
      undefined,
      {
        enableSse: true,
        onFallback: (r) => reasons.push(r),
      }
    );

    expect(res).toEqual(snap);
    expect(reasons).toEqual(['sse_not_acceptable']);
    expect(mockBackendFetch).toHaveBeenCalledTimes(1);
  });

  test('propagates structured SSE event: error payloads with job snapshot intact', async () => {
    const failedSnap = makeSnapshot({ jobId: 'job-err-1', status: 'failed', repairAttemptsUsed: 1 });
    const stream = makeMockReadableStream([
      'event: stage\ndata: {"jobId":"job-err-1","operation":"repair","stage":"repairing","timestamp":"2026-11-24T00:00:00Z"}\n\n',
      `event: error\ndata: ${JSON.stringify({ status: 502, error: 'LLM output truncated', job: failedSnap })}\n\n`,
    ]);
    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream' }),
      body: stream,
    }) as unknown as typeof fetch;

    let caught: unknown;
    try {
      await repairDraft(
        'job-err-1',
        { baseRevision: 1, results: [], catalog: [{ name: 'http_requests_total' }] },
        undefined,
        { enableSse: true }
      );
    } catch (err) {
      caught = err;
    }

    expect(extractApiError(caught)).toBe('HTTP 502: LLM output truncated');
    expect((caught as { data?: { job?: JobSnapshot } })?.data?.job).toEqual(failedSnap);
  });

  test('recovers via GET /jobs/{id} without duplicate POST when SSE drops mid-flight after jobId emitted', async () => {
    const completedSnap = makeSnapshot({ jobId: 'job-drop-99', status: 'awaiting_preview' });
    const droppingStream = makeMockReadableStream(
      [
        'event: stage\ndata: {"jobId":"job-drop-99","operation":"generate","stage":"generating","timestamp":"2026-11-24T00:00:00Z"}\n\n',
      ],
      new Error('proxy idle timeout reset')
    );
    const nativeFetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream' }),
      body: droppingStream,
    });
    globalThis.fetch = nativeFetch as unknown as typeof fetch;
    mockBackendFetch.mockReturnValueOnce(of({ data: completedSnap }));

    const result = await generateDraft(
      {
        datasourceUid: 'prom-main',
        userIntent: 'RED metrics',
        catalog: [{ name: 'http_requests_total' }],
      },
      undefined,
      { enableSse: true }
    );

    expect(result).toEqual(completedSnap);
    expect(nativeFetch).toHaveBeenCalledTimes(1);
    expect(mockBackendFetch).toHaveBeenCalledTimes(1);
    expect(mockBackendFetch.mock.calls[0][0]).toMatchObject({
      url: '/api/plugins/yuuki-llmstudio-app/resources/jobs/job-drop-99',
      method: 'GET',
    });
  });

  test('does not fall back when caller aborts via AbortController', async () => {
    const controller = new AbortController();
    controller.abort();
    const onFallback = jest.fn();

    await expect(
      generateDraft(
        {
          datasourceUid: 'prom-main',
          userIntent: 'RED metrics',
          catalog: [{ name: 'http_requests_total' }],
        },
        controller.signal,
        { enableSse: true, onFallback }
      )
    ).rejects.toThrow('Request aborted');

    expect(onFallback).not.toHaveBeenCalled();
    expect(mockBackendFetch).not.toHaveBeenCalled();
  });

  test('H-1: rejects with 409 + job snapshot when repair SSE drops and backend RevertOnCancel keeps old revision', async () => {
    const revertedSnap = makeSnapshot({
      jobId: 'job-repair-1',
      status: 'awaiting_preview',
      repairAttemptsUsed: 1,
      currentIR: {
        schemaVersion: '1.1',
        revision: 1,
        title: 'Old Rev',
        timeRange: { from: 'now-15m', to: 'now' },
        panels: [],
      },
    });
    const droppingStream = makeMockReadableStream(
      [
        'event: stage\ndata: {"jobId":"job-repair-1","operation":"repair","stage":"repairing","timestamp":"2026-11-24T00:00:00Z"}\n\n',
      ],
      new Error('proxy reset')
    );
    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream' }),
      body: droppingStream,
    }) as unknown as typeof fetch;
    mockBackendFetch.mockReturnValueOnce(of({ data: revertedSnap }));

    let caught: unknown;
    try {
      await repairDraft(
        'job-repair-1',
        { baseRevision: 1, results: [], catalog: [{ name: 'http_requests_total' }] },
        undefined,
        { enableSse: true }
      );
    } catch (err) {
      caught = err;
    }

    expect(caught).toBeDefined();
    expect((caught as { status?: number }).status).toBe(409);
    expect((caught as { data?: { job?: JobSnapshot } }).data?.job).toEqual(revertedSnap);
  });

  test('H-2: never re-POSTs when 200 text/event-stream drops before first frame on repair (uses seeded jobId recovery)', async () => {
    const repairedSnap = makeSnapshot({
      jobId: 'job-repair-2',
      status: 'awaiting_preview',
      repairAttemptsUsed: 1,
      currentIR: {
        schemaVersion: '1.1',
        revision: 2,
        title: 'Repaired Rev',
        timeRange: { from: 'now-15m', to: 'now' },
        panels: [],
      },
    });
    const emptyDropStream = makeMockReadableStream([], new Error('reset before first frame'));
    const nativeFetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream' }),
      body: emptyDropStream,
    });
    globalThis.fetch = nativeFetch as unknown as typeof fetch;
    mockBackendFetch.mockReturnValueOnce(of({ data: repairedSnap }));

    const result = await repairDraft(
      'job-repair-2',
      { baseRevision: 1, results: [], catalog: [{ name: 'http_requests_total' }] },
      undefined,
      { enableSse: true }
    );

    expect(result).toEqual(repairedSnap);
    expect(nativeFetch).toHaveBeenCalledTimes(1);
    expect(mockBackendFetch).toHaveBeenCalledTimes(1);
    expect(mockBackendFetch.mock.calls[0][0]).toMatchObject({
      url: '/api/plugins/yuuki-llmstudio-app/resources/jobs/job-repair-2',
      method: 'GET',
    });
  });

  test('H-2: never blindly re-POSTs generate on post-send fetch transport reset', async () => {
    const nativeFetch = jest.fn().mockRejectedValueOnce(new TypeError('network reset'));
    globalThis.fetch = nativeFetch as unknown as typeof fetch;

    await expect(
      generateDraft(
        {
          datasourceUid: 'prom-main',
          userIntent: 'RED metrics',
          catalog: [{ name: 'http_requests_total' }],
        },
        undefined,
        { enableSse: true }
      )
    ).rejects.toMatchObject({ status: 502 });

    expect(nativeFetch).toHaveBeenCalledTimes(1);
    expect(mockBackendFetch).not.toHaveBeenCalled();
  });

  test('M-1 & L-2: SseEventParser handles standalone CR, split CRLF, enforces total data memory cap, and reader.cancel() runs on error', async () => {
    const parser = new SseEventParser();
    expect(parser.push('event: stage\rdata: {"stage":"generating"}\r\r: keepalive\r')).toEqual([
      { event: 'stage', data: '{"stage":"generating"}' },
    ]);
    // Split CRLF across chunk boundary must NOT be treated as a blank line prematurely.
    const splitParser = new SseEventParser();
    expect(splitParser.push('event: stage\r')).toEqual([]);
    expect(splitParser.push('\ndata: {"stage":"validating_ir"}\r')).toEqual([]);
    expect(splitParser.push('\n\r\n')).toEqual([
      { event: 'stage', data: '{"stage":"validating_ir"}' },
    ]);
    // Event with empty data buffer must be discarded per WHATWG SSE spec.
    expect(parser.push('event: stage\n\n')).toEqual([]);

    // Repeated data lines without blank-line terminator MUST trip MAX_SSE_BUFFER_CHARS cap.
    const capParser = new SseEventParser();
    const bigLine = `data: ${'x'.repeat(256 * 1024)}\n`;
    expect(() => {
      for (let i = 0; i < 12; i++) {
        capParser.push(bigLine);
      }
    }).toThrow('SSE stream buffer limit exceeded');

    // Reader cancel must be invoked when frame parsing throws mid-stream.
    const cancelSpy = jest.fn(async () => {});
    const releaseSpy = jest.fn();
    const encoder = new TextEncoder();
    let reads = 0;
    const badStream = {
      getReader() {
        return {
          async read() {
            reads += 1;
            if (reads === 1) {
              return { done: false, value: encoder.encode('event: result\ndata: {broken json\n\n') };
            }
            return { done: true, value: undefined };
          },
          cancel: cancelSpy,
          releaseLock: releaseSpy,
        };
      },
    } as unknown as ReadableStream<Uint8Array>;

    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream' }),
      body: badStream,
    }) as unknown as typeof fetch;

    await expect(
      generateDraft(
        { datasourceUid: 'prom-main', userIntent: 'test', catalog: [{ name: 'up' }] },
        undefined,
        { enableSse: true }
      )
    ).rejects.toThrow();

    expect(cancelSpy).toHaveBeenCalledTimes(1);
    expect(releaseSpy).toHaveBeenCalledTimes(1);
  });

  test('M-2: truncated or aborted 200 application/json response throws instead of returning undefined', async () => {
    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => {
        throw new SyntaxError('Unexpected end of JSON input');
      },
    }) as unknown as typeof fetch;

    await expect(
      generateDraft(
        { datasourceUid: 'prom-main', userIntent: 'test', catalog: [{ name: 'up' }] },
        undefined,
        { enableSse: true }
      )
    ).rejects.toThrow('Invalid JSON response from plugin backend');
  });

  test('M-3: ignores malformed stage frames with non-string stage while resolving valid result', async () => {
    const snap = makeSnapshot({ jobId: 'job-m3' });
    const stages: JobStageEvent[] = [];
    const stream = makeMockReadableStream([
      'event: stage\ndata: {"stage":{"nested":true}}\n\n',
      'event: stage\ndata: null\n\n',
      'event: stage\ndata: {"jobId":"job-m3","operation":"generate","stage":"generating","timestamp":"2026-11-24T00:00:00Z"}\n\n',
      `event: result\ndata: ${JSON.stringify(snap)}\n\n`,
    ]);
    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream' }),
      body: stream,
    }) as unknown as typeof fetch;

    const res = await generateDraft(
      { datasourceUid: 'prom-main', userIntent: 'test', catalog: [{ name: 'up' }] },
      undefined,
      { enableSse: true, onStage: (ev) => stages.push(ev) }
    );
    expect(res).toEqual(snap);
    expect(stages).toHaveLength(1);
    expect(stages[0].stage).toBe('generating');
  });

  test('analyzeStatus streams analyzing/validating_report SSE stages and recovers via GET /jobs/{id} if stream drops', async () => {
    const readyReportSnap = makeSnapshot({
      jobId: 'job-report-1',
      jobKind: 'report',
      status: 'ready',
      statusReport: {
        schemaVersion: '1.0',
        overallStatus: 'healthy',
        confidence: 'high',
        executiveSummary: 'All systems healthy.',
        findings: [
          {
            id: 'finding-1',
            severity: 'info',
            category: 'alert',
            title: 'No alerts',
            detail: 'Zero active alerts.',
            evidenceIds: ['ev-status-1'],
          },
        ],
        timeline: [],
        hypotheses: [],
        recommendedActions: [],
        dataGaps: [],
      },
    });
    const stages: JobStageEvent[] = [];

    const droppingStream = makeMockReadableStream(
      [
        'event: stage\ndata: {"jobId":"job-report-1","operation":"analyze","stage":"analyzing","timestamp":"2026-11-27T00:00:00Z"}\n\n',
      ],
      new Error('network reset')
    );

    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: new Headers({ 'Content-Type': 'text/event-stream' }),
      body: droppingStream,
    }) as unknown as typeof fetch;

    // First poll returns still-active validating_report, second poll returns terminal ready snapshot.
    mockBackendFetch
      .mockReturnValueOnce(of({ data: makeSnapshot({ jobId: 'job-report-1', jobKind: 'report', status: 'validating_report' }) }))
      .mockReturnValueOnce(of({ data: readyReportSnap }));

    const result = await analyzeStatus(
      {
        language: 'en',
        window: '1h',
        baselineMode: 'preceding',
        collectionStatus: {
          alertsChecked: true,
          rulesChecked: true,
          totalRulesEvaluated: 1,
          activeAlertsTotal: 0,
          abnormalRulesTotal: 0,
        },
        evidence: {
          collectedAt: '2026-11-27T00:00:00Z',
          alerts: [],
          rules: [],
          metrics: [],
          statusNotes: [{ id: 'ev-status-1', message: '0 active alerts' }],
        },
      },
      undefined,
      { enableSse: true, onStage: (ev) => stages.push(ev) }
    );

    expect(result).toEqual(readyReportSnap);
    expect(stages.map((s) => `${s.operation}:${s.stage}`)).toEqual(['analyze:analyzing']);
    expect(mockBackendFetch).toHaveBeenCalledTimes(2);
  });
});
