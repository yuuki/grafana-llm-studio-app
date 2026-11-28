import { BackendSrvRequest, getBackendSrv } from '@grafana/runtime';
import { MetricCatalogItem, ScopeFilter } from '../ir/types';

export interface MetadataFetchOptions {
  datasourceUid: string;
  datasourceType?: string;
  scope: ScopeFilter;
  /**
   * Lookback window in seconds for scoped series/label discovery (default 3600s = 1h).
   * Note: VictoriaMetrics rounds start..end to UTC day granularity for series/label APIs.
   */
  lookbackSeconds?: number;
  /**
   * Hard cap on returned metrics/series/metadata entries to avoid full-store scans.
   */
  metricLimit?: number;
  seriesLimit?: number;
  metadataLimit?: number;
  signal?: AbortSignal;
}

export interface ScopedCatalogResult {
  items: MetricCatalogItem[];
  truncated: boolean;
  fetchedAt: string;
}

interface PromResponse<T> {
  status: 'success' | 'error';
  data?: T;
  error?: string;
}

interface PromMetadataEntry {
  type?: string;
  help?: string;
  unit?: string;
}

const VALID_LABEL_NAME = /^[a-zA-Z_][a-zA-Z0-9_]*$/;

function escapePromLabelValue(val: string): string {
  return val
    .replace(/\\/g, '\\\\')
    .replace(/"/g, '\\"')
    .replace(/\n/g, '\\n')
    .replace(/\r/g, '\\r');
}

function isVMBackendPlugin(datasourceType?: string): boolean {
  return (datasourceType || '').trim().toLowerCase() === 'victoriametrics-metrics-datasource';
}

function getHttpErrorStatus(err: unknown): number | undefined {
  if (typeof err === 'object' && err !== null) {
    const status = (err as { status?: unknown }).status;
    if (typeof status === 'number') {
      return status;
    }
  }
  return undefined;
}

function buildMetadataBaseUrls(datasourceUid: string, datasourceType?: string): string[] {
  const encodedUid = encodeURIComponent(datasourceUid);
  const proxyBase = `/api/datasources/proxy/uid/${encodedUid}/api/v1`;
  if (isVMBackendPlugin(datasourceType)) {
    // victoriametrics-metrics-datasource is a backend plugin exposing VMAPIQuery via Grafana's Resource API;
    // prefer resources so customQueryParameters, Secure Socks Proxy, TLS, and POST settings are honored.
    return [`/api/datasources/uid/${encodedUid}/resources/api/v1`, proxyBase];
  }
  return [proxyBase];
}

async function fetchPromApiWithBaseFallback<T>(
  baseUrls: string[],
  subPath: string,
  params: Record<string, string>,
  signal?: AbortSignal
): Promise<T> {
  let lastErr: unknown;
  for (let i = 0; i < baseUrls.length; i++) {
    try {
      return await fetchPromApi<T>(
        {
          url: `${baseUrls[i]}${subPath}`,
          method: 'GET',
          params,
        },
        signal
      );
    } catch (err) {
      lastErr = err;
      if (signal?.aborted) {
        throw err;
      }
      const status = getHttpErrorStatus(err);
      const canFallback = i + 1 < baseUrls.length && (status === 404 || status === 501);
      if (!canFallback) {
        throw err;
      }
    }
  }
  throw lastErr;
}

function fetchPromApi<T>(req: BackendSrvRequest, signal?: AbortSignal): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Aborted', 'AbortError'));
      return;
    }
    let settled = false;
    let sub: { unsubscribe: () => void } | undefined;
    const cleanup = () => {
      signal?.removeEventListener('abort', onAbort);
    };
    const onAbort = () => {
      if (settled) {
        return;
      }
      settled = true;
      sub?.unsubscribe();
      cleanup();
      reject(new DOMException('Aborted', 'AbortError'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });

    sub = getBackendSrv()
      .fetch<T>({
        ...req,
        showErrorAlert: false,
      })
      .subscribe({
        next: (res) => {
          if (settled) {
            return;
          }
          settled = true;
          cleanup();
          resolve(res.data);
        },
        error: (err) => {
          if (settled) {
            return;
          }
          settled = true;
          cleanup();
          reject(err);
        },
        complete: () => {
          if (!settled) {
            settled = true;
            cleanup();
            reject(new Error('Empty response from Prometheus API'));
          }
        },
      });
  });
}

/**
 * Fetches candidate scope values (e.g. values of `service` or `job`) with explicit time bounds,
 * AbortSignal cancellation, and client-side slicing.
 * Executes strictly from the Frontend using the logged-in user's Grafana session.
 */
export async function fetchScopeLabelValues(
  datasourceUid: string,
  labelName: string,
  lookbackSeconds = 3600,
  limit = 100,
  signal?: AbortSignal,
  datasourceType?: string
): Promise<string[]> {
  if (!VALID_LABEL_NAME.test(labelName) || labelName === '__name__') {
    throw new Error(`Invalid Prometheus label name: ${labelName}`);
  }
  const nowSec = Math.floor(Date.now() / 1000);
  const startSec = nowSec - Math.min(Math.max(lookbackSeconds, 300), 86400);
  const baseUrls = buildMetadataBaseUrls(datasourceUid, datasourceType);

  const resp = await fetchPromApiWithBaseFallback<PromResponse<string[]>>(
    baseUrls,
    `/label/${encodeURIComponent(labelName)}/values`,
    {
      start: String(startSec),
      end: String(nowSec),
      limit: String(Math.min(limit, 200)),
    },
    signal
  );
  if (resp.status !== 'success' || !Array.isArray(resp.data)) {
    throw new Error(resp.error ?? 'Failed to fetch scope label values');
  }
  return resp.data.slice(0, limit).sort();
}

/**
 * Fetches scoped Prometheus/VictoriaMetrics metadata using the logged-in user's Grafana session.
 * Distinguishes:
 * 1. /api/v1/label/__name__/values (with match[] + time window + limit) to list unique metric names
 *    without a single high-cardinality histogram starving the series budget (M-8).
 * 2. /api/v1/series (bounded + client-side sliced) to enrich sampled label names.
 * 3. /api/v1/metadata (best-effort, showErrorAlert=false) to enrich TYPE/HELP/UNIT.
 *
 * Results stay strictly in React memory for the active user session and are NEVER sent to a shared backend cache.
 */
async function discoverScopedMetricNames(
  baseUrls: string[],
  matcher: string,
  scopeLabelName: string,
  startSec: number,
  nowSec: number,
  metricLimit: number,
  metricLabels: Map<string, Set<string>>,
  signal?: AbortSignal
): Promise<boolean> {
  try {
    const namesResp = await fetchPromApiWithBaseFallback<PromResponse<string[]>>(
      baseUrls,
      '/label/__name__/values',
      {
        'match[]': matcher,
        start: String(startSec),
        end: String(nowSec),
        limit: String(metricLimit),
      },
      signal
    );
    if (namesResp.status !== 'success' || !Array.isArray(namesResp.data)) {
      return false;
    }
    for (const name of namesResp.data.slice(0, metricLimit)) {
      if (name && !metricLabels.has(name)) {
        metricLabels.set(name, new Set<string>([scopeLabelName]));
      }
    }
    return namesResp.data.length >= metricLimit;
  } catch (err) {
    if (signal?.aborted) {
      throw err;
    }
    // Fall back to bounded /api/v1/series below if server lacks match[] on label/__name__/values.
    return false;
  }
}

function mergeSeriesLabels(
  seriesList: Array<Record<string, string>>,
  seriesLimit: number,
  metricLimit: number,
  metricLabels: Map<string, Set<string>>
): void {
  // Slice client-side too in case older Prometheus (<2.47) ignores the limit query parameter.
  for (const s of seriesList.slice(0, seriesLimit)) {
    const name = s.__name__;
    if (!name) {
      continue;
    }
    let lbls = metricLabels.get(name);
    if (!lbls) {
      if (metricLabels.size >= metricLimit) {
        continue;
      }
      lbls = new Set<string>();
      metricLabels.set(name, lbls);
    }
    for (const k of Object.keys(s)) {
      if (k !== '__name__' && lbls.size < 16) {
        lbls.add(k);
      }
    }
  }
}

async function enrichMetricLabelsFromSeries(
  baseUrls: string[],
  matcher: string,
  startSec: number,
  nowSec: number,
  seriesLimit: number,
  metricLimit: number,
  metricLabels: Map<string, Set<string>>,
  signal?: AbortSignal
): Promise<boolean> {
  const seriesResp = await fetchPromApiWithBaseFallback<PromResponse<Array<Record<string, string>>>>(
    baseUrls,
    '/series',
    {
      'match[]': matcher,
      start: String(startSec),
      end: String(nowSec),
      limit: String(seriesLimit),
    },
    signal
  );
  if (seriesResp.status !== 'success' || !Array.isArray(seriesResp.data)) {
    if (metricLabels.size === 0) {
      throw new Error(seriesResp.error ?? 'Failed to query scoped /api/v1/series');
    }
    return false;
  }
  mergeSeriesLabels(seriesResp.data, seriesLimit, metricLimit, metricLabels);
  return seriesResp.data.length >= seriesLimit;
}

async function fetchBestEffortMetadata(
  baseUrls: string[],
  metadataLimit: number,
  signal?: AbortSignal
): Promise<Record<string, PromMetadataEntry[]>> {
  try {
    const metaResp = await fetchPromApiWithBaseFallback<PromResponse<Record<string, PromMetadataEntry[]>>>(
      baseUrls,
      '/metadata',
      { limit: String(metadataLimit) },
      signal
    );
    if (metaResp.status === 'success' && metaResp.data) {
      return metaResp.data;
    }
  } catch (err) {
    if (signal?.aborted) {
      throw err;
    }
  }
  return {};
}

function buildCatalogItems(
  metricLabels: Map<string, Set<string>>,
  metaMap: Record<string, PromMetadataEntry[]>
): MetricCatalogItem[] {
  const items: MetricCatalogItem[] = [];
  for (const [name, lblSet] of metricLabels.entries()) {
    const baseName = name.replace(/_(bucket|count|sum)$/, '');
    const metaEntry = metaMap[name]?.[0] ?? metaMap[baseName]?.[0];
    items.push({
      name,
      type: metaEntry?.type ?? inferTypeFromName(name),
      help: metaEntry?.help?.slice(0, 160),
      unit: metaEntry?.unit?.slice(0, 32),
      labels: Array.from(lblSet).sort(),
    });
  }
  items.sort((a, b) => a.name.localeCompare(b.name));
  return items;
}

export async function fetchScopedMetricCatalog(
  options: MetadataFetchOptions
): Promise<ScopedCatalogResult> {
  const { datasourceUid, datasourceType, scope, signal } = options;
  if (!VALID_LABEL_NAME.test(scope.labelName) || scope.labelName === '__name__' || !scope.labelValue.trim()) {
    throw new Error('A valid scope labelName and non-empty labelValue are required');
  }

  const nowSec = Math.floor(Date.now() / 1000);
  const lookback = Math.min(Math.max(options.lookbackSeconds ?? 3600, 300), 86400);
  const startSec = nowSec - lookback;
  const metricLimit = Math.min(options.metricLimit ?? 200, 300);
  const seriesLimit = Math.min(options.seriesLimit ?? 400, 500);
  const metadataLimit = Math.min(options.metadataLimit ?? 500, 500);

  const matcher = `{${scope.labelName}="${escapePromLabelValue(scope.labelValue.trim())}"}`;
  const baseUrls = buildMetadataBaseUrls(datasourceUid, datasourceType);

  // 1. Discover unique metric names via /api/v1/label/__name__/values first (resists single high-cardinality metric starvation).
  const metricLabels = new Map<string, Set<string>>();
  const namesTruncated = await discoverScopedMetricNames(
    baseUrls,
    matcher,
    scope.labelName,
    startSec,
    nowSec,
    metricLimit,
    metricLabels,
    signal
  );

  // 2. Bounded /api/v1/series call scoped strictly to the selected service/job for label enrichment.
  const seriesTruncated = await enrichMetricLabelsFromSeries(
    baseUrls,
    matcher,
    startSec,
    nowSec,
    seriesLimit,
    metricLimit,
    metricLabels,
    signal
  );

  // 3. Best-effort bounded /api/v1/metadata enrichment (showErrorAlert=false avoids red error toast).
  const metaMap = await fetchBestEffortMetadata(baseUrls, metadataLimit, signal);

  return {
    items: buildCatalogItems(metricLabels, metaMap),
    truncated: namesTruncated || seriesTruncated,
    fetchedAt: new Date().toISOString(),
  };
}

function inferTypeFromName(name: string): string {
  if (name.endsWith('_bucket')) {
    return 'histogram';
  }
  if (name.endsWith('_total')) {
    return 'counter';
  }
  return 'unknown';
}
