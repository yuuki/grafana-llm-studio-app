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

const DS_VAR_PATTERN = /^\$(?:\{)?(?:DS_[A-Za-z0-9_]+|datasource|prometheus)(?:\})?$/i;
const VAR_REF_PATTERN = /\$\{([a-zA-Z_][a-zA-Z0-9_]*)(?::([a-zA-Z]+))?\}|\$([a-zA-Z_][a-zA-Z0-9_]*)/g;
const PRIORITY_TITLE_RE = /(error|5xx|4xx|fail|latency|duration|p95|p99|cpu|mem|sat|avail|up|health|queue|timeout|drop|restart)/i;

function isPromCompatibleType(dsType: string): boolean {
  const lower = (dsType || '').toLowerCase();
  return lower === 'prometheus' || lower.includes('victoriametrics');
}

function escapeRegexLiteral(v: string): string {
  return v.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
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

function extractConcreteVariableDefaults(
  rawDashboard: Record<string, unknown>,
  scope?: ScopeFilter
): Map<string, string> {
  const vars = new Map<string, string>();
  const templating = rawDashboard.templating as { list?: unknown[] } | undefined;
  const list = Array.isArray(templating?.list) ? templating.list : [];

  for (const entry of list) {
    if (!entry || typeof entry !== 'object') {
      continue;
    }
    const v = entry as Record<string, unknown>;
    const name = typeof v.name === 'string' ? v.name.trim() : '';
    if (!name || v.type === 'datasource') {
      continue;
    }
    const current = v.current as { value?: unknown } | undefined;
    const rawVal = current?.value;
    if (typeof rawVal === 'string' && rawVal.trim() !== '' && rawVal !== '$__all') {
      vars.set(name, rawVal.trim());
    } else if (Array.isArray(rawVal) && rawVal.length > 0 && typeof rawVal[0] === 'string' && rawVal[0] !== '$__all') {
      vars.set(name, rawVal[0].trim());
    } else if (Array.isArray(v.options)) {
      for (const opt of v.options) {
        if (opt && typeof opt === 'object') {
          const ov = (opt as { value?: unknown }).value;
          if (typeof ov === 'string' && ov.trim() !== '' && ov !== '$__all') {
            vars.set(name, ov.trim());
            break;
          }
        }
      }
    }
  }

  // Override matching variable with user's active scope so dashboard queries align with alert scope.
  if (scope?.labelName && scope.labelValue) {
    vars.set(scope.labelName, scope.labelValue);
  }
  return vars;
}

function resolveExprVariables(
  expr: string,
  varValues: Map<string, string>
): { resolved: string; unresolved: string[] } {
  const unresolved: string[] = [];
  VAR_REF_PATTERN.lastIndex = 0;
  const resolved = expr.replace(VAR_REF_PATTERN, (match, bracedName?: string, fmt?: string, bareName?: string) => {
    const name = bracedName || bareName || '';
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
      return escapeRegexLiteral(val);
    }
    return val;
  });
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

function resolveTargetDatasource(
  targetDs: unknown,
  panelDs: unknown,
  selectedDatasourceUid: string | undefined,
  allowSet: Set<string>
): { uid: string; type: string } | null {
  const raw = targetDs ?? panelDs;
  let candidateUid = '';
  let candidateType = '';

  if (raw === null || raw === undefined || raw === '') {
    // Org default datasource
    try {
      const def = getDataSourceSrv().getInstanceSettings(null);
      if (def?.uid) {
        candidateUid = def.uid;
        candidateType = def.type || 'prometheus';
      }
    } catch {
      // Fallback to selected datasource below.
    }
    if (!candidateUid && selectedDatasourceUid) {
      candidateUid = selectedDatasourceUid;
    }
  } else if (typeof raw === 'string') {
    if (DS_VAR_PATTERN.test(raw.trim())) {
      candidateUid = selectedDatasourceUid ?? '';
    } else {
      candidateUid = raw.trim();
    }
  } else if (typeof raw === 'object') {
    const rec = raw as { uid?: unknown; type?: unknown };
    const uidStr = typeof rec.uid === 'string' ? rec.uid.trim() : '';
    if (uidStr === '' || DS_VAR_PATTERN.test(uidStr)) {
      candidateUid = selectedDatasourceUid ?? '';
    } else {
      candidateUid = uidStr;
    }
    if (typeof rec.type === 'string') {
      candidateType = rec.type;
    }
  }

  if (!candidateUid || !allowSet.has(candidateUid)) {
    return null;
  }

  if (!candidateType) {
    try {
      const settings = getDataSourceSrv().getInstanceSettings(candidateUid);
      candidateType = settings?.type || 'prometheus';
    } catch {
      candidateType = 'prometheus';
    }
  }
  if (!isPromCompatibleType(candidateType)) {
    return null;
  }
  return { uid: candidateUid, type: candidateType };
}

/**
 * Dedicated read-only extractor for Operational Status Reports.
 * Unlike convertGrafanaDashboardToIR (which enforces Studio's strict editable IR v1.1 subset),
 * this preserves Grafana built-in time variables ($__rate_interval, $__interval, $__range),
 * resolves default/template datasources against allowedDatasourceUids, prioritizes anomaly-rich
 * panels up to REPORT_MAX_PANELS, and never throws on zero compatible panels.
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
  const varValues = extractConcreteVariableDefaults(rawDashboard, options.scope);
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
      const ds = resolveTargetDatasource(target.datasource, p.datasource, options.selectedDatasourceUid, allowSet);
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
    const unit = typeof fc?.defaults?.unit === 'string' && fc.defaults.unit.trim() !== '' ? fc.defaults.unit.trim() : undefined;
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
    warnings.push(
      `Analyzed top ${maxPanels} prioritized panels out of ${scored.length} compatible panels.`
    );
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
