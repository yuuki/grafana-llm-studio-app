import { getBackendSrv } from '@grafana/runtime';
import { MetricCatalogItem, ScopeFilter } from '../ir/types';

export interface MetadataFetchOptions {
  datasourceUid: string;
  scope: ScopeFilter;
  /**
   * Lookback window in seconds for scoped series/label discovery (default 3600s = 1h).
   * Note: VictoriaMetrics rounds start..end to UTC day granularity for series/label APIs.
   */
  lookbackSeconds?: number;
  /**
   * Hard cap on returned series/metadata entries to avoid full-store scans.
   */
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

/**
 * Fetches candidate scope values (e.g. values of `service` or `job`) with explicit time bounds and limit.
 * Executes strictly from the Frontend using the logged-in user's Grafana session.
 */
export async function fetchScopeLabelValues(
  datasourceUid: string,
  labelName: string,
  lookbackSeconds = 3600,
  limit = 100
): Promise<string[]> {
  if (!VALID_LABEL_NAME.test(labelName)) {
    throw new Error(`Invalid Prometheus label name: ${labelName}`);
  }
  const nowSec = Math.floor(Date.now() / 1000);
  const startSec = nowSec - Math.min(Math.max(lookbackSeconds, 300), 86400);
  const url = `/api/datasources/proxy/uid/${encodeURIComponent(datasourceUid)}/api/v1/label/${encodeURIComponent(labelName)}/values`;

  const resp = await getBackendSrv().get<PromResponse<string[]>>(url, {
    start: String(startSec),
    end: String(nowSec),
    limit: String(Math.min(limit, 200)),
  });
  if (resp.status !== 'success' || !Array.isArray(resp.data)) {
    throw new Error(resp.error ?? 'Failed to fetch scope label values');
  }
  return resp.data.slice(0, limit).sort();
}

/**
 * Fetches scoped Prometheus/VictoriaMetrics metadata using the logged-in user's Grafana session.
 * Distinguishes:
 * - /api/v1/series (filtered by required match[] + time window + limit) to discover metrics & sampled labels
 * - /api/v1/metadata (bounded by limit) to enrich TYPE/HELP/UNIT without scanning all label values
 *
 * Results stay strictly in React memory for the active user session and are NEVER sent to a shared backend cache.
 */
export async function fetchScopedMetricCatalog(
  options: MetadataFetchOptions
): Promise<ScopedCatalogResult> {
  const { datasourceUid, scope } = options;
  if (!VALID_LABEL_NAME.test(scope.labelName) || !scope.labelValue.trim()) {
    throw new Error('A valid scope labelName and non-empty labelValue are required');
  }

  const nowSec = Math.floor(Date.now() / 1000);
  const lookback = Math.min(Math.max(options.lookbackSeconds ?? 3600, 300), 86400);
  const startSec = nowSec - lookback;
  const seriesLimit = Math.min(options.seriesLimit ?? 400, 500);
  const metadataLimit = Math.min(options.metadataLimit ?? 500, 500);

  const escapedVal = scope.labelValue.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
  const matcher = `{${scope.labelName}="${escapedVal}"}`;
  const baseUrl = `/api/datasources/proxy/uid/${encodeURIComponent(datasourceUid)}/api/v1`;

  // 1. Bounded /api/v1/series call scoped strictly to the selected service/job.
  const seriesResp = await getBackendSrv().get<PromResponse<Array<Record<string, string>>>>(
    `${baseUrl}/series`,
    {
      'match[]': matcher,
      start: String(startSec),
      end: String(nowSec),
      limit: String(seriesLimit),
    }
  );
  if (seriesResp.status !== 'success' || !Array.isArray(seriesResp.data)) {
    throw new Error(seriesResp.error ?? 'Failed to query scoped /api/v1/series');
  }

  const metricLabels = new Map<string, Set<string>>();
  for (const s of seriesResp.data) {
    const name = s.__name__;
    if (!name) {
      continue;
    }
    let lbls = metricLabels.get(name);
    if (!lbls) {
      lbls = new Set<string>();
      metricLabels.set(name, lbls);
    }
    for (const k of Object.keys(s)) {
      if (k !== '__name__' && lbls.size < 16) {
        lbls.add(k);
      }
    }
  }

  // 2. Best-effort bounded /api/v1/metadata enrichment (some Prometheus/VM setups restrict or omit metadata).
  let metaMap: Record<string, PromMetadataEntry[]> = {};
  try {
    const metaResp = await getBackendSrv().get<PromResponse<Record<string, PromMetadataEntry[]>>>(
      `${baseUrl}/metadata`,
      { limit: String(metadataLimit) }
    );
    if (metaResp.status === 'success' && metaResp.data) {
      metaMap = metaResp.data;
    }
  } catch {
    // Metadata enrichment is optional; continue with discovered metric names and labels.
  }

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

  return {
    items,
    truncated: seriesResp.data.length >= seriesLimit,
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
