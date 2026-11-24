import { getBackendSrv } from '@grafana/runtime';
import { t } from '../i18n';
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
  syntax_error: 2,
  datasource_error: 3,
  timeout: 4,
  permission_error: 5,
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
        if (CATEGORY_SEVERITY.datasource_error > CATEGORY_SEVERITY[worstCategory]) {
          worstCategory = 'datasource_error';
        }
        const prefix = refIds.length > 1 ? `[${refId}] ` : '';
        errorParts.push(`${prefix}Datasource returned no result entry for refId ${refId}`);
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
        errorText: t('validator.errors.timeout', { seconds: Math.round(timeoutMs / 1000) }),
        durationMs,
      };
    }
    const { category, message } = extractAndClassifyError(err, refIds);

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

const TIMEOUT_TOKEN_REGEX =
  /(?:^|[^a-z0-9_])(?:timeout|timed out|deadline exceeded|maxqueryduration|context canceled)(?:$|[^a-z0-9_])/i;

export function classifyDatasourceError(
  status?: number,
  message = ''
): ValidationCategory {
  const lower = message.toLowerCase();
  const isPermissionMsg =
    lower.includes('forbidden') ||
    lower.includes('unauthorized') ||
    lower.includes('access denied') ||
    lower.includes('permission denied');
  const isTimeoutMsg = TIMEOUT_TOKEN_REGEX.test(message);
  const isInfraOrLimitMsg =
    lower.includes('connection refused') ||
    lower.includes('no such host') ||
    lower.includes('dial tcp') ||
    lower.includes('unexpected eof') ||
    lower.includes('maxuniquetimeseries') ||
    lower.includes('maxmemory') ||
    lower.includes('maxsamples') ||
    lower.includes('out of memory') ||
    lower.includes('exceeds');
  const isSyntaxMsg =
    lower.includes('parse error') ||
    lower.includes('bad_data') ||
    lower.includes('unexpected') ||
    lower.includes('invalid parameter') ||
    lower.includes('unknown function') ||
    lower.includes('cannot parse') ||
    lower.includes('unsupported function') ||
    lower.includes('invalid number of args') ||
    lower.includes('unparsed data') ||
    lower.includes('cannot expand with') ||
    lower.includes('missing closing') ||
    lower.includes('many-to-many') ||
    lower.includes('duplicate match');

  // 1. Trust explicit HTTP status codes first so metric names (e.g. http_timeout_total,
  //    auth_permission_checks_total) inside 400 parse errors are never misclassified.
  if (status === 401 || status === 403) {
    return 'permission_error';
  }
  if (status === 408 || status === 504) {
    return 'timeout';
  }
  if (status === 400 || status === 422) {
    // Classify permission, timeout, and network/resource-limit errors BEFORE syntax patterns:
    // victoriametrics-metrics-datasource maps transport/timeout errors to HTTP 400 and VM maps
    // -search.maxQueryDuration / -search.maxUniqueTimeseries to HTTP 422.
    if (isPermissionMsg) {
      return 'permission_error';
    }
    if (isTimeoutMsg) {
      return 'timeout';
    }
    if (isInfraOrLimitMsg) {
      return 'datasource_error';
    }
    if (isSyntaxMsg) {
      return 'syntax_error';
    }
    return 'datasource_error';
  }
  if (status !== undefined && status >= 500) {
    if (isTimeoutMsg) {
      return 'timeout';
    }
    return 'datasource_error';
  }

  // 2. Fallback heuristic when status is omitted or 200 with embedded error text.
  if (isPermissionMsg) {
    return 'permission_error';
  }
  if (isTimeoutMsg) {
    return 'timeout';
  }
  if (isInfraOrLimitMsg) {
    return 'datasource_error';
  }
  if (isSyntaxMsg) {
    return 'syntax_error';
  }
  return 'datasource_error';
}

function extractAndClassifyError(
  err: unknown,
  refIds: string[]
): { category: ValidationCategory; message: string } {
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
    // Classify each inner /api/ds/query results[refId] individually using the same
    // severity precedence as the non-throwing response path (permission > timeout > datasource > syntax).
    const innerMessages: string[] = [];
    let worstCategory: ValidationCategory = 'ok';
    for (const refId of refIds) {
      const inner = e.data?.results?.[refId];
      if (inner?.error) {
        const errStr = String(inner.error);
        const cat = classifyDatasourceError(inner.status ?? e.status, errStr);
        if (CATEGORY_SEVERITY[cat] > CATEGORY_SEVERITY[worstCategory]) {
          worstCategory = cat;
        }
        const prefix = refIds.length > 1 ? `[${refId}] ` : '';
        innerMessages.push(`${prefix}${errStr}`);
      }
    }
    if (innerMessages.length > 0) {
      return {
        category: worstCategory,
        message: innerMessages.join('; '),
      };
    }
    const msg =
      (typeof e.data?.message === 'string' && e.data.message) ||
      (typeof e.data?.error === 'string' && e.data.error) ||
      (typeof e.message === 'string' && e.message) ||
      (typeof e.statusText === 'string' && e.statusText) ||
      'Datasource query failed';
    return {
      category: classifyDatasourceError(e.status, msg),
      message: msg,
    };
  }
  const msg = String(err);
  return {
    category: classifyDatasourceError(undefined, msg),
    message: msg,
  };
}
