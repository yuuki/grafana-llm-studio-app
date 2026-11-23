import { getBackendSrv } from '@grafana/runtime';
import {
  computePanelQueryHash,
  PanelValidationResult,
  ValidationCategory,
} from '../ir/types';
import {
  interpolatePromQLVariables,
  NormalizedDashboardSpec,
  NormalizedPanelSpec,
  NormalizedVariableSpec,
} from '../spec/normalize';

export interface DryRunOptions {
  /**
   * Max concurrent datasource queries (default: 2).
   */
  maxConcurrency?: number;
  /**
   * Per-query timeout in milliseconds (default: 10,000ms).
   */
  timeoutMs?: number;
  /**
   * Validation window start (default: now-5m).
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

const CATEGORY_SEVERITY: Record<ValidationCategory, number> = {
  ok: 0,
  empty_result: 1,
  datasource_error: 2,
  timeout: 3,
  permission_error: 4,
  syntax_error: 5,
};

/**
 * Executes real bounded dry-run validation queries from the Frontend using the
 * logged-in user's Grafana session (/api/ds/query).
 * - Supports single-query and bounded multi-query panels (1..3 queries per panel).
 * - Interpolates declared template variable defaults so $var queries validate cleanly.
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
        spec.variables,
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
  variables: NormalizedVariableSpec[],
  from: string,
  to: string,
  maxDataPoints: number,
  timeoutMs: number,
  outerSignal?: AbortSignal
): Promise<PanelValidationResult> {
  if (outerSignal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }

  const queryHash = await computePanelQueryHash(panel.targets);
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

  const refIds = panel.targets.map((t) => t.refId);

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
            queries: panel.targets.map((target) => ({
              refId: target.refId,
              datasource: target.datasource,
              expr: interpolatePromQLVariables(target.expr, variables),
              legendFormat: target.legendFormat,
              maxDataPoints,
              intervalMs: 30000,
              range: true,
              instant: false,
            })),
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
    let worstCategory: ValidationCategory = 'ok';
    const errorParts: string[] = [];
    let totalNonEmptySeries = 0;

    for (const refId of refIds) {
      const refRes = responseData?.results?.[refId];
      if (!refRes) {
        continue;
      }
      if (refRes.error) {
        const cat = classifyDatasourceError(refRes.status, refRes.error);
        if (CATEGORY_SEVERITY[cat] > CATEGORY_SEVERITY[worstCategory]) {
          worstCategory = cat;
        }
        const prefix = refIds.length > 1 ? `[${refId}] ` : '';
        errorParts.push(`${prefix}${refRes.error}`);
        continue;
      }
      const frames = refRes.frames ?? [];
      const nonEmptySeries = frames.filter(
        (f) => Array.isArray(f.data?.values) && f.data!.values!.some((col) => col.length > 0)
      ).length;
      totalNonEmptySeries += nonEmptySeries;
    }

    if (errorParts.length > 0) {
      return {
        panelId: panel.id,
        queryHash,
        category: worstCategory,
        errorText: errorParts.join('; ').slice(0, 300),
        durationMs,
      };
    }

    if (totalNonEmptySeries === 0) {
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
      seriesCount: totalNonEmptySeries,
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
        errorText: `検証クエリがタイムアウトしました (${Math.round(timeoutMs / 1000)}s)`,
        durationMs,
      };
    }
    const { status, message } = extractErrorInfo(err, refIds);
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
  refIds: string[]
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
    // Prioritize inner /api/ds/query results[refId] error and status across all targets (H-10).
    const innerMessages: string[] = [];
    let innerStatus: number | undefined;
    for (const refId of refIds) {
      const inner = e.data?.results?.[refId];
      if (inner?.error) {
        if (innerStatus === undefined && inner.status !== undefined) {
          innerStatus = inner.status;
        }
        const prefix = refIds.length > 1 ? `[${refId}] ` : '';
        innerMessages.push(`${prefix}${String(inner.error)}`);
      }
    }
    if (innerMessages.length > 0) {
      return {
        status: innerStatus ?? e.status,
        message: innerMessages.join('; '),
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
