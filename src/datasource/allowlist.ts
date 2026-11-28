import { getDataSourceSrv } from '@grafana/runtime';
import { fetchPublicConfig } from '../api/studioClient';
import { PublicConfig, QueryKind } from '../ir/types';

export interface AllowedDatasourceOption {
  uid: string;
  name: string;
  type: string;
}

export const SCOPE_LABELS = ['service', 'job', 'namespace'] as const;
export type ScopeLabelName = (typeof SCOPE_LABELS)[number];

export function isScopeLabelName(val: string): val is ScopeLabelName {
  return val === 'service' || val === 'job' || val === 'namespace';
}

export function isPrometheusCompatibleType(dsType: string): boolean {
  const lower = (dsType || '').toLowerCase();
  return lower === 'prometheus' || lower.includes('victoriametrics');
}

export function filterAllowlistedDatasources(
  allDatasources: ReadonlyArray<{ uid: string; name: string; type: string }>,
  allowedDatasourceUids: readonly string[]
): AllowedDatasourceOption[] {
  const allowSet = new Set(allowedDatasourceUids);
  return allDatasources
    .filter((d) => allowSet.has(d.uid) && isPrometheusCompatibleType(d.type))
    .map((d) => ({ uid: d.uid, name: d.name, type: d.type }));
}

export async function loadPublicConfigAndDatasources(signal?: AbortSignal): Promise<{
  publicConfig: PublicConfig;
  datasources: AllowedDatasourceOption[];
}> {
  const publicConfig = await fetchPublicConfig(signal);
  const allDs = getDataSourceSrv().getList({ metrics: true });
  const datasources = filterAllowlistedDatasources(allDs, publicConfig.allowedDatasourceUids);
  return { publicConfig, datasources };
}

export function resolveDatasourceDialect(
  ds: AllowedDatasourceOption | null,
  pinnedDialects?: Record<string, QueryKind>
): { kind: QueryKind; pinned: boolean } {
  if (!ds) {
    return { kind: 'promql', pinned: false };
  }
  const pinned = pinnedDialects?.[ds.uid];
  if (pinned === 'promql' || pinned === 'metricsql') {
    return { kind: pinned, pinned: true };
  }
  const lower = (ds.type || '').toLowerCase();
  if (lower.includes('victoriametrics')) {
    return { kind: 'metricsql', pinned: false };
  }
  return { kind: 'promql', pinned: false };
}
