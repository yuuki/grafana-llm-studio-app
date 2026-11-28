import { getDataSourceSrv } from '@grafana/runtime';
import { REPORT_MAX_PANELS } from '../ir/reportTypes';
import type { ScopeFilter } from '../ir/types';

export interface RawThresholdStep {
  value: number | null;
  color: string;
}

export interface RawPanelThresholds {
  mode: 'absolute' | 'percentage';
  steps: RawThresholdStep[];
}

export interface ExtractedReportQuery {
  refId: string;
  expr: string;
  datasourceUid: string;
  datasourceType: string;
}

export interface ExtractedReportPanel {
  panelKey: string;
  title: string;
  unit?: string;
  thresholds?: RawPanelThresholds;
  queries: ExtractedReportQuery[];
}

export interface DashboardExtractionResult {
  dashboardTitle: string;
  totalPanelCount: number;
  panels: ExtractedReportPanel[];
  skippedPanelCount: number;
  warnings: string[];
}

export interface DashboardExtractionOptions {
  selectedDatasourceUid?: string;
  allowedDatasourceUids: string[];
  scope?: ScopeFilter;
  maxPanels?: number;
}

// Built-in variables preserved for Grafana's Prometheus backend or expanded by metricsCollector.ts.
const BUILTIN_VAR_NAMES = new Set([
  '__rate_interval',
  '__interval',
  '__interval_ms',
  '__range',
  '__range_s',
  '__range_ms',
  '__from',
  '__to',
]);

const GENERIC_PROM_DS_VAR_RE = /^\$(?:\{)?(DS_[A-Za-z0-9_]+|datasource|prometheus)(?:\})?$/i;
const ANY_VAR_TOKEN_RE = /^\$(?:\{)?([a-zA-Z_][a-zA-Z0-9_]*)(?:\})?$/;
const NON_PROM_DS_NAME_RE = /(loki|tempo|jaeger|zipkin|elastic|opensearch|postgres|mysql|mssql|clickhouse|cloudwatch|influx|graphite|bigquery|datadog|splunk|azure|stackdriver|cloudmonitoring|sql)/i;
const VAR_REF_PATTERN =
  /\$\{([a-zA-Z_][a-zA-Z0-9_]*)(?::([a-zA-Z]+))?\}|\$([a-zA-Z_][a-zA-Z0-9_]*)|\[\[([a-zA-Z_][a-zA-Z0-9_]*)(?::([a-zA-Z]+))?\]\]/g;
const PRIORITY_TITLE_RE =
  /(error|5xx|4xx|fail|latency|duration|p95|p99|cpu|mem|sat|avail|up|health|queue|timeout|drop|restart)/i;

function isPromCompatibleType(dsType: string): boolean {
  const lower = (dsType || '').toLowerCase();
  return lower === 'prometheus' || lower.includes('victoriametrics');
}

function stripControlChars(v: string): string {
  return v.replace(/[\x00-\x1f\x7f]/g, '');
}

function escapePromQLString(v: string): string {
  return stripControlChars(v).replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

function escapeRegexLiteral(v: string): string {
  return stripControlChars(v).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

function collectRawPanels(rawDashboard: Record<string, unknown>): Array<Record<string, unknown>> {
  const out: Array<Record<string, unknown>> = [];
  const topPanels = Array.isArray(rawDashboard.panels) ? rawDashboard.panels : [];
  for (const item of topPanels) {
    if (!item || typeof item !== 'object') {
      continue;
    }
    const p = item as Record<string, unknown>;
    if (p.type === 'row' && Array.isArray(p.panels)) {
      for (const child of p.panels) {
        if (child && typeof child === 'object') {
          out.push(child as Record<string, unknown>);
        }
      }
    } else if (p.type !== 'row' && p.type !== 'text') {
      out.push(p);
    }
  }
  return out;
}

interface ExtractedVariables {
  values: Map<string, string>;
  promDatasourceVars: Set<string>;
  nonPromDatasourceVars: Set<string>;
}

function extractDashboardVariables(
  rawDashboard: Record<string, unknown>,
  scope?: ScopeFilter
): ExtractedVariables {
  const values = new Map<string, string>();
  const promDatasourceVars = new Set<string>();
  const nonPromDatasourceVars = new Set<string>();
  const templating = rawDashboard.templating as { list?: unknown[] } | undefined;
  const list = Array.isArray(templating?.list) ? templating.list : [];

  for (const entry of list) {
    if (!entry || typeof entry !== 'object') {
      continue;
    }
    const v = entry as Record<string, unknown>;
    const name = typeof v.name === 'string' ? v.name.trim() : '';
    if (!name) {
      continue;
    }
    if (v.type === 'datasource') {
      const dsQuery = typeof v.query === 'string' ? v.query.trim() : '';
      if (isPromCompatibleType(dsQuery)) {
        promDatasourceVars.add(name);
      } else if (dsQuery) {
        nonPromDatasourceVars.add(name);
      }
      continue;
    }
    const concrete = extractConcreteVariableValue(v);
    if (concrete !== undefined) {
      values.set(name, concrete);
    }
  }

  // Override matching variable with user's active scope so dashboard queries align with alert scope.
  if (scope?.labelName && scope.labelValue) {
    values.set(scope.labelName, scope.labelValue);
  }
  return { values, promDatasourceVars, nonPromDatasourceVars };
}

function extractConcreteVariableValue(v: Record<string, unknown>): string | undefined {
  const current = v.current as { value?: unknown } | undefined;
  const rawVal = current?.value;
  if (typeof rawVal === 'string' && rawVal.trim() !== '' && rawVal !== '$__all') {
    return rawVal.trim();
  }
  if (Array.isArray(rawVal) && rawVal.length > 0 && typeof rawVal[0] === 'string' && rawVal[0] !== '$__all') {
    return rawVal[0].trim();
  }
  if (!Array.isArray(v.options)) {
    return undefined;
  }
  for (const opt of v.options) {
    if (!opt || typeof opt !== 'object') {
      continue;
    }
    const ov = (opt as { value?: unknown }).value;
    if (typeof ov === 'string' && ov.trim() !== '' && ov !== '$__all') {
      return ov.trim();
    }
  }
  return undefined;
}

function resolveExprVariables(
  expr: string,
  varValues: Map<string, string>
): { resolved: string; unresolved: string[] } {
  const unresolved: string[] = [];
  VAR_REF_PATTERN.lastIndex = 0;
  const resolved = expr.replace(
    VAR_REF_PATTERN,
    (
      match,
      bracedName?: string,
      bracedFmt?: string,
      bareName?: string,
      bracketName?: string,
      bracketFmt?: string
    ) => {
      const name = bracedName || bareName || bracketName || '';
      const fmt = bracedFmt || bracketFmt || '';
      if (!name || BUILTIN_VAR_NAMES.has(name)) {
        return match;
      }
      const val = varValues.get(name);
      if (val === undefined) {
        if (!unresolved.includes(name)) {
          unresolved.push(name);
        }
        return match;
      }
      if (fmt === 'regex') {
        return escapePromQLString(escapeRegexLiteral(val));
      }
      return escapePromQLString(val);
    }
  );
  return { resolved, unresolved };
}

function extractRawThresholds(panel: Record<string, unknown>): RawPanelThresholds | undefined {
  const fc = panel.fieldConfig as { defaults?: { thresholds?: { mode?: unknown; steps?: unknown[] } } } | undefined;
  const th = fc?.defaults?.thresholds;
  if (!th || !Array.isArray(th.steps) || th.steps.length === 0) {
    return undefined;
  }
  const mode: 'absolute' | 'percentage' = th.mode === 'percentage' ? 'percentage' : 'absolute';
  const steps: RawThresholdStep[] = [];
  for (const s of th.steps) {
    if (!s || typeof s !== 'object') {
      continue;
    }
    const rec = s as { value?: unknown; color?: unknown };
    const color = typeof rec.color === 'string' ? rec.color.trim() : '';
    if (!color) {
      continue;
    }
    if (rec.value === null || rec.value === undefined) {
      steps.push({ value: null, color });
    } else if (typeof rec.value === 'number' && Number.isFinite(rec.value)) {
      steps.push({ value: rec.value, color });
    }
  }
  return steps.length > 0 ? { mode, steps } : undefined;
}

function isCompatibleDatasourceVar(
  rawRef: string,
  promDatasourceVars: Set<string>,
  nonPromDatasourceVars: Set<string>,
  explicitType?: string
): boolean {
  const m = ANY_VAR_TOKEN_RE.exec(rawRef.trim());
  if (!m) {
    return false;
  }
  const varName = m[1];
  if (nonPromDatasourceVars.has(varName)) {
    return false;
  }
  if (promDatasourceVars.has(varName)) {
    return true;
  }
  if (explicitType && !isPromCompatibleType(explicitType)) {
    return false;
  }
  if (!GENERIC_PROM_DS_VAR_RE.test(rawRef.trim())) {
    return false;
  }
  if (NON_PROM_DS_NAME_RE.test(varName)) {
    return false;
  }
  return true;
}

function resolveCandidateDatasourceUid(
  raw: unknown,
  selectedDatasourceUid: string | undefined,
  promDatasourceVars: Set<string>,
  nonPromDatasourceVars: Set<string>
): string | null {
  if (raw === null || raw === undefined || raw === '') {
    try {
      const def = getDataSourceSrv().getInstanceSettings(null);
      if (def?.uid) {
        return def.uid;
      }
    } catch {
      // Fallback to selected datasource below.
    }
    return selectedDatasourceUid ?? '';
  }
  if (typeof raw === 'string') {
    const trimmed = raw.trim();
    if (!ANY_VAR_TOKEN_RE.test(trimmed)) {
      return trimmed;
    }
    return isCompatibleDatasourceVar(trimmed, promDatasourceVars, nonPromDatasourceVars)
      ? selectedDatasourceUid ?? ''
      : null;
  }
  if (typeof raw === 'object') {
    const rec = raw as { uid?: unknown; type?: unknown };
    const uidStr = typeof rec.uid === 'string' ? rec.uid.trim() : '';
    const explicitType = typeof rec.type === 'string' ? rec.type.trim() : undefined;
    if (explicitType && !isPromCompatibleType(explicitType)) {
      return null;
    }
    if (uidStr === '') {
      return selectedDatasourceUid ?? '';
    }
    if (ANY_VAR_TOKEN_RE.test(uidStr)) {
      return isCompatibleDatasourceVar(uidStr, promDatasourceVars, nonPromDatasourceVars, explicitType)
        ? selectedDatasourceUid ?? ''
        : null;
    }
    return uidStr;
  }
  return '';
}

function resolveTargetDatasource(
  targetDs: unknown,
  panelDs: unknown,
  selectedDatasourceUid: string | undefined,
  allowSet: Set<string>,
  promDatasourceVars: Set<string>,
  nonPromDatasourceVars: Set<string>
): { uid: string; type: string } | null {
  const candidateUid = resolveCandidateDatasourceUid(
    targetDs ?? panelDs,
    selectedDatasourceUid,
    promDatasourceVars,
    nonPromDatasourceVars
  );
  if (!candidateUid || !allowSet.has(candidateUid)) {
    return null;
  }

  // Always verify authoritative datasource type via DataSourceSrv (fail closed).
  let resolvedType = '';
  try {
    const settings = getDataSourceSrv().getInstanceSettings(candidateUid);
    if (!settings?.type) {
      return null;
    }
    resolvedType = settings.type;
  } catch {
    return null;
  }
  if (!isPromCompatibleType(resolvedType)) {
    return null;
  }
  return { uid: candidateUid, type: resolvedType };
}

/**
 * Dedicated read-only extractor for Operational Status Reports.
 * Unlike convertGrafanaDashboardToIR (which enforces Studio's strict editable IR v1.1 subset),
 * this preserves Grafana built-in time variables ($__rate_interval, $__interval, $__range),
 * resolves default/template datasources against allowedDatasourceUids (fail-closed),
 * escapes interpolated variable values safely for PromQL, prioritizes anomaly-rich panels
 * up to REPORT_MAX_PANELS, and never throws on zero compatible panels.
 */
export function extractDashboardReportQueries(
  rawDashboard: Record<string, unknown>,
  options: DashboardExtractionOptions
): DashboardExtractionResult {
  const dashboardTitle =
    typeof rawDashboard.title === 'string' && rawDashboard.title.trim() !== ''
      ? rawDashboard.title.trim().slice(0, 120)
      : 'Selected Dashboard';
  const maxPanels = Math.max(1, Math.min(options.maxPanels ?? REPORT_MAX_PANELS, REPORT_MAX_PANELS));
  const allowSet = new Set(options.allowedDatasourceUids);
  const rawPanels = collectRawPanels(rawDashboard);
  const { values: varValues, promDatasourceVars, nonPromDatasourceVars } = extractDashboardVariables(
    rawDashboard,
    options.scope
  );
  const warnings: string[] = [];

  interface ScoredPanel {
    score: number;
    originalIndex: number;
    panel: ExtractedReportPanel;
  }

  const scored: ScoredPanel[] = [];
  let skippedPanelCount = 0;

  rawPanels.forEach((p, idx) => {
    const title =
      typeof p.title === 'string' && p.title.trim() !== ''
        ? p.title.trim().slice(0, 120)
        : `Panel ${idx + 1}`;
    const targets = Array.isArray(p.targets) ? p.targets : [];
    const queries: ExtractedReportQuery[] = [];

    for (let tIdx = 0; tIdx < targets.length && queries.length < 3; tIdx++) {
      const rawT = targets[tIdx];
      if (!rawT || typeof rawT !== 'object') {
        continue;
      }
      const target = rawT as Record<string, unknown>;
      if (target.hide === true) {
        continue;
      }
      const expr = typeof target.expr === 'string' ? target.expr.trim() : '';
      if (!expr) {
        continue;
      }
      const ds = resolveTargetDatasource(
        target.datasource,
        p.datasource,
        options.selectedDatasourceUid,
        allowSet,
        promDatasourceVars,
        nonPromDatasourceVars
      );
      if (!ds) {
        continue;
      }
      const { resolved, unresolved } = resolveExprVariables(expr, varValues);
      if (unresolved.length > 0) {
        continue;
      }
      const refId =
        typeof target.refId === 'string' && /^[A-Z]$/.test(target.refId)
          ? target.refId
          : String.fromCharCode(65 + queries.length);
      queries.push({
        refId,
        expr: resolved,
        datasourceUid: ds.uid,
        datasourceType: ds.type,
      });
    }

    if (queries.length === 0) {
      skippedPanelCount++;
      return;
    }

    const fc = p.fieldConfig as { defaults?: { unit?: unknown } } | undefined;
    const unit =
      typeof fc?.defaults?.unit === 'string' && fc.defaults.unit.trim() !== '' ? fc.defaults.unit.trim() : undefined;
    const thresholds = extractRawThresholds(p);

    let score = 0;
    if (thresholds && thresholds.steps.length > 1) {
      score += 3;
    }
    if (PRIORITY_TITLE_RE.test(title)) {
      score += 2;
    }
    if (p.type === 'timeseries' || p.type === 'graph') {
      score += 1;
    }

    scored.push({
      score,
      originalIndex: idx,
      panel: {
        panelKey: `p-${idx + 1}`,
        title,
        unit,
        thresholds,
        queries,
      },
    });
  });

  scored.sort((a, b) => {
    if (b.score !== a.score) {
      return b.score - a.score;
    }
    return a.originalIndex - b.originalIndex;
  });

  if (scored.length > maxPanels) {
    warnings.push(`Analyzed top ${maxPanels} prioritized panels out of ${scored.length} compatible panels.`);
  }
  if (skippedPanelCount > 0) {
    warnings.push(
      `Skipped ${skippedPanelCount} panel(s) due to non-allowlisted datasources, unresolved variables, or non-Prometheus queries.`
    );
  }

  return {
    dashboardTitle,
    totalPanelCount: rawPanels.length,
    panels: scored.slice(0, maxPanels).map((s) => s.panel),
    skippedPanelCount,
    warnings,
  };
}
