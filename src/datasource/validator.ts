import { getBackendSrv } from '@grafana/runtime';
import { t } from '../i18n';
import {
  computeQueryHash,
  PanelValidationResult,
  ValidationCategory,
} from '../ir/types';
import { NormalizedDashboardSpec, NormalizedPanelSpec } from '../spec/normalize';

export interface DryRunOptions {
  /**
   * Max concurrent datasource queries (MVP default: 2).
   */
  maxConcurrency?: number;
  /**
   * Per-query timeout in milliseconds (MVP default: 10,000ms).
   */
  timeoutMs?: number;
  /**
   * Validation window start (MVP default: now-5m).
   * Note: Narrow window + maxDataPoints=10 is a safeguard against accidental UI load,
   * NOT a performance guarantee or DoS boundary (enforce limits on Prometheus/VM too).
   */
  from?: string;
  to?: string;
  maxDataPoints?: number;
  /**
   * Optional subset of panel IDs to re-validate after partial edit or repair.
   */
  onlyPanelIds?: string[];
  signal?: AbortSignal;
}

interface GrafanaDSQueryResponse {
  results?: Record<
    string,
    {
      status?: number;
      error?: string;
      frames?: Array<{
        data?: {
          values?: unknown[][];
        };
      }>;
    }
  >;
}

/**
 * Executes real bounded dry-run validation queries from the Frontend using the
 * logged-in user's Grafana session (/api/ds/query).
 * - Enforces concurrency cap (default 2) and per-query timeout (default 10s).
 * - Properly extracts inner /api/ds/query FetchError.data.results[refId] errors (H-10).
 * - Propagates external AbortSignal cancellation without misclassifying as timeout (M-12).
 */
export async function runDryRunValidation(
  spec: NormalizedDashboardSpec,
  options: DryRunOptions = {}
): Promise<PanelValidationResult[]> {
  if (options.signal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }

  const maxConcurrency = Math.max(1, Math.min(options.maxConcurrency ?? 2, 4));
  const timeoutMs = Math.max(1000, Math.min(options.timeoutMs ?? 10000, 30000));
  const from = options.from ?? 'now-5m';
  const to = options.to ?? 'now';
  const maxDataPoints = options.maxDataPoints ?? 10;

  const targetPanels = options.onlyPanelIds
    ? spec.panels.filter((p) => options.onlyPanelIds!.includes(p.id))
    : spec.panels;

  const results: PanelValidationResult[] = new Array(targetPanels.length);
  let cursor = 0;

  const worker = async () => {
    while (cursor < targetPanels.length) {
      if (options.signal?.aborted) {
        throw new DOMException('Aborted', 'AbortError');
      }
      const idx = cursor++;
      results[idx] = await validateSinglePanel(
        targetPanels[idx],
        from,
        to,
        maxDataPoints,
        timeoutMs,
        options.signal
      );
    }
  };

  const workers = Array.from(
    { length: Math.min(maxConcurrency, targetPanels.length) },
    () => worker()
  );
  await Promise.all(workers);
  return results.filter(Boolean);
}

async function validateSinglePanel(
  panel: NormalizedPanelSpec,
  from: string,
  to: string,
  maxDataPoints: number,
  timeoutMs: number,
  outerSignal?: AbortSignal
): Promise<PanelValidationResult> {
  if (outerSignal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }

  const queryHash = await computeQueryHash(panel.target.expr);
  const started = Date.now();

  let timedOut = false;
  let externallyAborted = false;
  const controller = new AbortController();
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort(new Error('timeout'));
  }, timeoutMs);
  const onOuterAbort = () => {
    externallyAborted = true;
    controller.abort(outerSignal?.reason);
  };
  outerSignal?.addEventListener('abort', onOuterAbort, { once: true });

  try {
    const responseData = await new Promise<GrafanaDSQueryResponse>((resolve, reject) => {
      if (outerSignal?.aborted || externallyAborted) {
        reject(new DOMException('Aborted', 'AbortError'));
        return;
      }
      if (controller.signal.aborted) {
        reject(new Error('timeout'));
        return;
      }
      let settled = false;
      let sub: { unsubscribe: () => void } | undefined;
      const onAbort = () => {
        if (settled) {
          return;
        }
        settled = true;
        sub?.unsubscribe();
        controller.signal.removeEventListener('abort', onAbort);
        if (externallyAborted || outerSignal?.aborted) {
          reject(new DOMException('Aborted', 'AbortError'));
        } else {
          reject(new Error('timeout'));
        }
      };
      controller.signal.addEventListener('abort', onAbort, { once: true });

      sub = getBackendSrv()
        .fetch<GrafanaDSQueryResponse>({
          url: '/api/ds/query',
          method: 'POST',
          showErrorAlert: false,
          data: {
            from,
            to,
            queries: [
              {
                refId: panel.target.refId,
                datasource: panel.datasource,
                expr: panel.target.expr,
                legendFormat: panel.target.legendFormat,
                maxDataPoints,
                intervalMs: 30000,
                range: true,
                instant: false,
              },
            ],
          },
        })
        .subscribe({
          next: (res) => {
            if (settled) {
              return;
            }
            settled = true;
            controller.signal.removeEventListener('abort', onAbort);
            resolve(res.data);
          },
          error: (err) => {
            if (settled) {
              return;
            }
            settled = true;
            controller.signal.removeEventListener('abort', onAbort);
            reject(err);
          },
          complete: () => {
            if (!settled) {
              settled = true;
              controller.signal.removeEventListener('abort', onAbort);
              resolve({});
            }
          },
        });
    });

    const durationMs = Date.now() - started;
    const refRes = responseData?.results?.[panel.target.refId];
    if (!refRes) {
      return {
        panelId: panel.id,
        queryHash,
        category: 'empty_result',
        seriesCount: 0,
        durationMs,
      };
    }

    if (refRes.error) {
      const cat = classifyDatasourceError(refRes.status, refRes.error);
      return {
        panelId: panel.id,
        queryHash,
        category: cat,
        errorText: refRes.error.slice(0, 300),
        durationMs,
      };
    }

    const frames = refRes.frames ?? [];
    const nonEmptySeries = frames.filter(
      (f) => Array.isArray(f.data?.values) && f.data!.values!.some((col) => col.length > 0)
    ).length;

    if (nonEmptySeries === 0) {
      return {
        panelId: panel.id,
        queryHash,
        category: 'empty_result',
        seriesCount: 0,
        durationMs,
      };
    }

    return {
      panelId: panel.id,
      queryHash,
      category: 'ok',
      seriesCount: nonEmptySeries,
      durationMs,
    };
  } catch (err: unknown) {
    if (externallyAborted || outerSignal?.aborted) {
      throw new DOMException('Aborted', 'AbortError');
    }
    const durationMs = Date.now() - started;
    if (timedOut) {
      return {
        panelId: panel.id,
        queryHash,
        category: 'timeout',
        errorText: t('validator.errors.timeout', { seconds: Math.round(timeoutMs / 1000) }),
        durationMs,
      };
    }
    const { status, message } = extractErrorInfo(err, panel.target.refId);
    const category = classifyDatasourceError(status, message);

    return {
      panelId: panel.id,
      queryHash,
      category,
      errorText: message.slice(0, 300),
      durationMs,
    };
  } finally {
    clearTimeout(timer);
    outerSignal?.removeEventListener('abort', onOuterAbort);
  }
}

export function classifyDatasourceError(
  status?: number,
  message = ''
): ValidationCategory {
  const lower = message.toLowerCase();
  if (
    status === 401 ||
    status === 403 ||
    lower.includes('forbidden') ||
    lower.includes('unauthorized') ||
    lower.includes('access denied') ||
    lower.includes('permission')
  ) {
    return 'permission_error';
  }
  if (
    status === 408 ||
    status === 504 ||
    lower.includes('timeout') ||
    lower.includes('deadline exceeded')
  ) {
    return 'timeout';
  }
  if (
    status === 400 ||
    status === 422 ||
    lower.includes('parse error') ||
    lower.includes('bad_data') ||
    lower.includes('unexpected') ||
    lower.includes('invalid parameter') ||
    lower.includes('unknown function')
  ) {
    return 'syntax_error';
  }
  return 'datasource_error';
}

function extractErrorInfo(
  err: unknown,
  refId: string
): { status?: number; message: string } {
  if (typeof err === 'object' && err !== null) {
    const e = err as {
      status?: number;
      statusText?: string;
      message?: string;
      data?: {
        message?: string;
        error?: string;
        results?: Record<string, { status?: number; error?: string }>;
      };
    };
    // Prioritize inner /api/ds/query results[refId] error and status (H-10).
    const inner = e.data?.results?.[refId];
    if (inner?.error) {
      return {
        status: inner.status ?? e.status,
        message: String(inner.error),
      };
    }
    const msg =
      e.data?.message ||
      e.data?.error ||
      e.message ||
      e.statusText ||
      'Datasource query failed';
    return { status: e.status, message: String(msg) };
  }
  return { message: String(err) };
}
