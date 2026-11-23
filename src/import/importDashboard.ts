import { getBackendSrv } from '@grafana/runtime';
import { lastValueFrom } from 'rxjs';
import {
  ALLOWED_PANEL_TYPES,
  ALLOWED_QUERY_REF_IDS,
  ALLOWED_THRESHOLD_COLORS,
  ALLOWED_TIME_FROM,
  ALLOWED_UNITS,
  AllowedTimeFrom,
  AllowedUnit,
  DashboardIR,
  ImportSource,
  IR_SCHEMA_VERSION,
  MAX_QUERIES_PER_PANEL,
  MAX_VARIABLE_OPTIONS,
  MAX_VARIABLES,
  PanelIR,
  PanelType,
  QueryIR,
  SELECTED_DATASOURCE_REF,
  ThresholdColor,
  ThresholdIR,
  VariableIR,
} from '../ir/types';
import { getOrgNamespace } from '../save/saveDashboard';

export interface ExistingDashboardSummary {
  uid: string;
  title: string;
  folderUid?: string;
  folderTitle?: string;
}

export interface FetchedExistingDashboard {
  rawDashboard: Record<string, unknown>;
  source: ImportSource;
}

export interface SkippedImportItem {
  title: string;
  type: string;
  reason: string;
}

export interface ImportConversionReport {
  importedPanelCount: number;
  skippedPanels: SkippedImportItem[];
  importedVariableCount: number;
  skippedVariables: SkippedImportItem[];
  warnings: string[];
}

export interface ImportConversionOptions {
  selectedDatasourceUid: string;
  selectedDatasourceName?: string;
  maxPanels?: number;
}

export interface ImportConversionResult {
  draft: DashboardIR;
  report: ImportConversionReport;
}

interface RawDashboardV1Beta1 {
  metadata?: {
    name?: string;
    resourceVersion?: string;
    annotations?: Record<string, string>;
  };
  spec?: Record<string, unknown>;
}

interface RawLegacyDashboardEnvelope {
  meta?: {
    folderUid?: string;
  };
  dashboard?: Record<string, unknown>;
}

const VALID_VAR_NAME = /^[a-zA-Z_][a-zA-Z0-9_]{0,31}$/;
const VALID_LABEL_NAME = /^[a-zA-Z_][a-zA-Z0-9_]{0,63}$/;
const VALID_METRIC_NAME = /^[a-zA-Z_:][a-zA-Z0-9_:]{0,127}$/;
const LABEL_VALUES_WITH_METRIC = /^label_values\(\s*([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{[^}]*\})?\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$/;
const LABEL_VALUES_ONLY = /^label_values\(\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$/;

const RESERVED_VAR_NAMES = new Set([
  '__name__',
  '__from',
  '__to',
  '__interval',
  '__interval_ms',
  '__rate_interval',
  '__range',
  '__range_s',
  '__range_ms',
  '__dashboard',
  '__org',
  '__user',
  'timeFilter',
]);

const ALLOWED_PANEL_TYPE_SET = new Set<string>(ALLOWED_PANEL_TYPES);
const ALLOWED_UNIT_SET = new Set<string>(ALLOWED_UNITS);
const ALLOWED_COLOR_SET = new Set<string>(ALLOWED_THRESHOLD_COLORS);
const ALLOWED_TIME_FROM_SET = new Set<string>(ALLOWED_TIME_FROM);

/**
 * Lists existing Grafana dashboards visible to the logged-in user via /api/search.
 */
export async function listExistingDashboards(limit = 50): Promise<ExistingDashboardSummary[]> {
  const res = await lastValueFrom(
    getBackendSrv().fetch<
      Array<{
        uid?: string;
        title?: string;
        folderUid?: string;
        folderTitle?: string;
      }>
    >({
      url: '/api/search',
      method: 'GET',
      params: {
        type: 'dash-db',
        limit: Math.max(1, Math.min(limit, 100)),
      },
      showErrorAlert: false,
    })
  );
  if (!Array.isArray(res.data)) {
    return [];
  }
  return res.data
    .filter((d) => Boolean(d.uid && d.title))
    .map((d) => ({
      uid: String(d.uid),
      title: String(d.title),
      folderUid: d.folderUid ? String(d.folderUid) : '',
      folderTitle: d.folderTitle ? String(d.folderTitle) : undefined,
    }));
}

/**
 * Fetches an existing Grafana dashboard by UID using the logged-in user's session.
 * Prefers Grafana's Kubernetes-style v1beta1 API so `metadata.resourceVersion` is captured
 * for optional opt-in optimistic-concurrency updates; falls back to `/api/dashboards/uid/{uid}`
 * (without resourceVersion, restricting save mode to create-as-copy).
 */
export async function fetchExistingDashboardByUid(uid: string): Promise<FetchedExistingDashboard> {
  const cleanUid = uid.trim();
  if (!cleanUid) {
    throw new Error('Dashboard UID is required');
  }

  const namespace = getOrgNamespace();
  try {
    const res = await lastValueFrom(
      getBackendSrv().fetch<RawDashboardV1Beta1>({
        url: `/apis/dashboard.grafana.app/v1beta1/namespaces/${encodeURIComponent(namespace)}/dashboards/${encodeURIComponent(cleanUid)}`,
        method: 'GET',
        showErrorAlert: false,
      })
    );
    const spec = res.data?.spec;
    if (spec && typeof spec === 'object') {
      return {
        rawDashboard: spec,
        source: {
          uid: res.data?.metadata?.name || cleanUid,
          resourceVersion: res.data?.metadata?.resourceVersion || undefined,
          folderUid: res.data?.metadata?.annotations?.['grafana.app/folder'] || '',
          title: typeof spec.title === 'string' ? sanitizePlainText(spec.title, 120) : undefined,
          version: typeof spec.version === 'number' ? spec.version : undefined,
        },
      };
    }
  } catch (err: unknown) {
    const status = (err as { status?: number })?.status;
    if (status === 401 || status === 403) {
      throw err;
    }
  }

  const legacy = await lastValueFrom(
    getBackendSrv().fetch<RawLegacyDashboardEnvelope>({
      url: `/api/dashboards/uid/${encodeURIComponent(cleanUid)}`,
      method: 'GET',
      showErrorAlert: false,
    })
  );
  const rawDash = legacy.data?.dashboard;
  if (!rawDash || typeof rawDash !== 'object') {
    throw new Error(`Dashboard ${cleanUid} returned no spec`);
  }
  return {
    rawDashboard: rawDash,
    source: {
      uid: typeof rawDash.uid === 'string' && rawDash.uid ? rawDash.uid : cleanUid,
      // Legacy /api/dashboards/uid does not provide Kubernetes resourceVersion;
      // leave empty so opt-in revision-checked PUT is safely unavailable.
      folderUid: legacy.data?.meta?.folderUid || '',
      title: typeof rawDash.title === 'string' ? sanitizePlainText(rawDash.title, 120) : undefined,
      version: typeof rawDash.version === 'number' ? rawDash.version : undefined,
    },
  };
}

/**
 * Deterministically converts an untrusted Grafana Dashboard JSON object into a
 * constrained Dashboard IR v1.1 draft + explicit conversion report.
 * - Strips HTML/JS/control/Bidi chars from all text fields.
 * - Filters to allowlisted visualization types and selected Prometheus datasource targets only.
 * - Rebinds datasource references strictly to `selected_prometheus`.
 * - Caps panels (<= maxPanels), queries per panel (<= 3), thresholds (<= 4), and variables (<= 4).
 * - Packs gridPos cleanly onto the non-overlapping 2-column IR grid.
 */
export function convertGrafanaDashboardToIR(
  rawInput: unknown,
  options: ImportConversionOptions
): ImportConversionResult {
  if (!rawInput || typeof rawInput !== 'object') {
    throw new Error('Dashboard JSON must be an object');
  }
  const raw = rawInput as Record<string, unknown>;
  const maxPanels = Math.max(1, Math.min(options.maxPanels ?? 6, 6));
  const warnings: string[] = [];
  const skippedPanels: SkippedImportItem[] = [];
  const skippedVariables: SkippedImportItem[] = [];

  const title = sanitizePlainText(typeof raw.title === 'string' ? raw.title : '', 120) || 'Imported Dashboard';
  const description = sanitizePlainText(typeof raw.description === 'string' ? raw.description : '', 400);

  const rawTime = raw.time as { from?: unknown; to?: unknown } | undefined;
  let timeFrom: AllowedTimeFrom = 'now-15m';
  if (typeof rawTime?.from === 'string' && ALLOWED_TIME_FROM_SET.has(rawTime.from)) {
    timeFrom = rawTime.from as AllowedTimeFrom;
  } else if (typeof rawTime?.from === 'string' && rawTime.from !== '') {
    warnings.push(`Unsupported time.from "${rawTime.from}" normalized to "now-15m"`);
  }

  // 1. Safely import up to 4 scoped template variables.
  const variables: VariableIR[] = [];
  const rawTemplating = raw.templating as { list?: unknown[] } | undefined;
  const rawVarList = Array.isArray(rawTemplating?.list) ? rawTemplating.list : [];
  const seenVarNames = new Set<string>();

  for (const item of rawVarList) {
    if (!item || typeof item !== 'object') {
      continue;
    }
    const v = item as Record<string, unknown>;
    const rawName = typeof v.name === 'string' ? v.name.trim() : '';
    const rawType = typeof v.type === 'string' ? v.type.trim() : 'unknown';

    if (variables.length >= MAX_VARIABLES) {
      skippedVariables.push({
        title: rawName || '(unnamed)',
        type: rawType,
        reason: `exceeded max ${MAX_VARIABLES} variables`,
      });
      continue;
    }
    if (!VALID_VAR_NAME.test(rawName) || RESERVED_VAR_NAMES.has(rawName) || seenVarNames.has(rawName)) {
      skippedVariables.push({
        title: rawName || '(unnamed)',
        type: rawType,
        reason: 'invalid, reserved, or duplicate variable name',
      });
      continue;
    }

    const label = sanitizePlainText(typeof v.label === 'string' ? v.label : '', 64);
    const multi = Boolean(v.multi);
    const includeAll = Boolean(v.includeAll);
    const currentVal = extractVariableCurrentValue(v.current);

    if (rawType === 'query') {
      if (!isDatasourceCompatible(v.datasource, options)) {
        skippedVariables.push({
          title: rawName,
          type: rawType,
          reason: 'variable datasource does not match selected Prometheus datasource',
        });
        continue;
      }
      const parsed = parseLabelValuesQuery(v.query);
      if (!parsed) {
        skippedVariables.push({
          title: rawName,
          type: rawType,
          reason: 'only Prometheus label_values(...) query variables are supported in IR v1.1',
        });
        continue;
      }
      const varIR: VariableIR = {
        name: rawName,
        type: 'query',
        datasourceRef: SELECTED_DATASOURCE_REF,
        labelName: parsed.labelName,
        ...(label ? { label } : {}),
        ...(parsed.metricName ? { metricName: parsed.metricName } : {}),
        ...(currentVal ? { defaultValue: sanitizePlainText(currentVal, 128) } : {}),
        ...(multi ? { multi: true } : {}),
        ...(includeAll ? { includeAll: true } : {}),
      };
      seenVarNames.add(rawName);
      variables.push(varIR);
    } else if (rawType === 'custom') {
      const opts = extractCustomVariableOptions(v);
      if (opts.length === 0) {
        skippedVariables.push({
          title: rawName,
          type: rawType,
          reason: 'no valid custom options found',
        });
        continue;
      }
      const boundedOpts = opts.slice(0, MAX_VARIABLE_OPTIONS);
      if (opts.length > MAX_VARIABLE_OPTIONS) {
        warnings.push(`Variable $${rawName}: truncated custom options from ${opts.length} to ${MAX_VARIABLE_OPTIONS}`);
      }
      const defVal =
        currentVal && boundedOpts.includes(currentVal) ? currentVal : boundedOpts[0];
      const varIR: VariableIR = {
        name: rawName,
        type: 'custom',
        options: boundedOpts,
        defaultValue: defVal,
        ...(label ? { label } : {}),
        ...(multi ? { multi: true } : {}),
        ...(includeAll ? { includeAll: true } : {}),
      };
      seenVarNames.add(rawName);
      variables.push(varIR);
    } else {
      skippedVariables.push({
        title: rawName,
        type: rawType,
        reason: `variable type "${rawType}" is not supported in IR v1.1`,
      });
    }
  }

  // 2. Flatten row-nested panels and convert up to maxPanels supported panels.
  const rawPanels = flattenRawPanels(Array.isArray(raw.panels) ? raw.panels : []);
  rawPanels.sort((a, b) => {
    const ga = (a.gridPos as { y?: number; x?: number } | undefined) ?? {};
    const gb = (b.gridPos as { y?: number; x?: number } | undefined) ?? {};
    const dy = (Number(ga.y) || 0) - (Number(gb.y) || 0);
    if (dy !== 0) {
      return dy;
    }
    return (Number(ga.x) || 0) - (Number(gb.x) || 0);
  });

  const panels: PanelIR[] = [];
  const usedPanelIds = new Set<string>();
  const widthHints: Array<'half' | 'full'> = [];

  for (let i = 0; i < rawPanels.length; i++) {
    const rp = rawPanels[i];
    const pType = typeof rp.type === 'string' ? rp.type.trim() : 'unknown';
    const rawTitle = typeof rp.title === 'string' ? rp.title : `Panel ${i + 1}`;
    const safeTitle = sanitizePlainText(rawTitle, 80) || `Panel ${i + 1}`;

    if (pType === 'row') {
      continue;
    }
    if (!ALLOWED_PANEL_TYPE_SET.has(pType)) {
      skippedPanels.push({
        title: safeTitle,
        type: pType,
        reason: `panel type "${pType}" is not supported in IR v1.1`,
      });
      continue;
    }
    if (panels.length >= maxPanels) {
      skippedPanels.push({
        title: safeTitle,
        type: pType,
        reason: `exceeded max ${maxPanels} panels`,
      });
      continue;
    }

    const panelDsCompatible = isDatasourceCompatible(rp.datasource, options);
    const rawTargets = Array.isArray(rp.targets) ? rp.targets : [];
    const importedQueries: QueryIR[] = [];
    let skippedTargetsCount = 0;

    for (const rt of rawTargets) {
      if (!rt || typeof rt !== 'object') {
        continue;
      }
      const target = rt as Record<string, unknown>;
      if (target.hide === true) {
        continue;
      }
      const targetDs = target.datasource !== undefined ? target.datasource : rp.datasource;
      if (!panelDsCompatible && target.datasource === undefined) {
        skippedTargetsCount++;
        continue;
      }
      if (!isDatasourceCompatible(targetDs, options)) {
        skippedTargetsCount++;
        continue;
      }
      const rawExpr = typeof target.expr === 'string' ? target.expr.trim() : '';
      if (!rawExpr) {
        continue;
      }
      if (rawExpr.length > 512) {
        warnings.push(`Panel "${safeTitle}": skipped query exceeding 512 chars`);
        continue;
      }
      if (importedQueries.length >= MAX_QUERIES_PER_PANEL) {
        warnings.push(`Panel "${safeTitle}": truncated to max ${MAX_QUERIES_PER_PANEL} queries`);
        break;
      }
      const legend = sanitizePlainText(
        typeof target.legendFormat === 'string' ? target.legendFormat : '',
        80
      );
      importedQueries.push({
        refId: ALLOWED_QUERY_REF_IDS[importedQueries.length],
        expr: rawExpr,
        ...(legend ? { legendFormat: legend } : {}),
      });
    }

    if (skippedTargetsCount > 0) {
      warnings.push(
        `Panel "${safeTitle}": skipped ${skippedTargetsCount} query target(s) bound to another datasource`
      );
    }
    if (importedQueries.length === 0) {
      skippedPanels.push({
        title: safeTitle,
        type: pType,
        reason: 'no compatible Prometheus PromQL queries found for selected datasource',
      });
      continue;
    }

    const fieldDefaults =
      (rp.fieldConfig as { defaults?: Record<string, unknown> } | undefined)?.defaults ?? {};
    const rawUnit = typeof fieldDefaults.unit === 'string' ? fieldDefaults.unit : '';
    let unit: AllowedUnit = 'short';
    if (rawUnit && ALLOWED_UNIT_SET.has(rawUnit)) {
      unit = rawUnit as AllowedUnit;
    } else if (rawUnit) {
      warnings.push(`Panel "${safeTitle}": unsupported unit "${rawUnit}" mapped to "short"`);
    }

    const thresholds = convertThresholds(fieldDefaults.thresholds);
    const panelDesc = sanitizePlainText(typeof rp.description === 'string' ? rp.description : '', 240);
    const panelId = makeUniquePanelId(safeTitle, panels.length + 1, usedPanelIds);
    const gridPos = rp.gridPos as { w?: number } | undefined;
    const widthHint: 'half' | 'full' = (Number(gridPos?.w) || 12) > 12 ? 'full' : 'half';
    widthHints.push(widthHint);

    const panelIR: PanelIR = {
      id: panelId,
      type: pType as PanelType,
      title: safeTitle,
      ...(panelDesc ? { description: panelDesc } : {}),
      datasourceRef: SELECTED_DATASOURCE_REF,
      ...(importedQueries.length === 1
        ? { query: { expr: importedQueries[0].expr, ...(importedQueries[0].legendFormat ? { legendFormat: importedQueries[0].legendFormat } : {}) } }
        : { queries: importedQueries }),
      unit,
      ...(thresholds.length > 0 ? { thresholds } : {}),
      layout: { row: 0, col: 0, width: widthHint },
    };
    panels.push(panelIR);
  }

  if (panels.length === 0) {
    throw new Error(
      `No compatible panels could be imported (skipped ${skippedPanels.length} panel(s))`
    );
  }

  // 3. Assign non-overlapping 2-column grid layout (rows 0..5).
  assignPackedGrid(panels, widthHints);

  const draft: DashboardIR = {
    schemaVersion: IR_SCHEMA_VERSION,
    revision: 1,
    title,
    ...(description ? { description } : {}),
    timeRange: {
      from: timeFrom,
      to: 'now',
    },
    ...(variables.length > 0 ? { variables } : {}),
    panels,
  };

  return {
    draft,
    report: {
      importedPanelCount: panels.length,
      skippedPanels,
      importedVariableCount: variables.length,
      skippedVariables,
      warnings,
    },
  };
}

function flattenRawPanels(rawPanels: unknown[]): Array<Record<string, unknown>> {
  const out: Array<Record<string, unknown>> = [];
  for (const item of rawPanels) {
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
      continue;
    }
    out.push(p);
  }
  return out;
}

function isDatasourceCompatible(
  ds: unknown,
  options: ImportConversionOptions
): boolean {
  if (ds === undefined || ds === null || ds === '') {
    return true;
  }
  if (typeof ds === 'string') {
    const trimmed = ds.trim();
    return (
      trimmed === '' ||
      trimmed === '-- Grafana --' ||
      trimmed === options.selectedDatasourceUid ||
      (Boolean(options.selectedDatasourceName) && trimmed === options.selectedDatasourceName)
    );
  }
  if (typeof ds === 'object') {
    const obj = ds as { uid?: unknown; type?: unknown };
    const uid = typeof obj.uid === 'string' ? obj.uid.trim() : '';
    const type = typeof obj.type === 'string' ? obj.type.trim().toLowerCase() : '';
    if (type && type !== 'prometheus' && !type.includes('victoriametrics')) {
      return false;
    }
    if (!uid) {
      return true;
    }
    return uid === options.selectedDatasourceUid;
  }
  return false;
}

function parseLabelValuesQuery(
  rawQuery: unknown
): { labelName: string; metricName?: string } | null {
  let queryStr = '';
  if (typeof rawQuery === 'string') {
    queryStr = rawQuery.trim();
  } else if (rawQuery && typeof rawQuery === 'object') {
    const qObj = rawQuery as { query?: unknown };
    if (typeof qObj.query === 'string') {
      queryStr = qObj.query.trim();
    }
  }
  if (!queryStr) {
    return null;
  }
  const withMetric = queryStr.match(LABEL_VALUES_WITH_METRIC);
  if (withMetric) {
    const metricName = withMetric[1];
    const labelName = withMetric[2];
    if (
      VALID_METRIC_NAME.test(metricName) &&
      VALID_LABEL_NAME.test(labelName) &&
      labelName !== '__name__'
    ) {
      return { metricName, labelName };
    }
    return null;
  }
  const labelOnly = queryStr.match(LABEL_VALUES_ONLY);
  if (labelOnly) {
    const labelName = labelOnly[1];
    if (VALID_LABEL_NAME.test(labelName) && labelName !== '__name__') {
      return { labelName };
    }
  }
  return null;
}

function extractVariableCurrentValue(rawCurrent: unknown): string {
  if (!rawCurrent || typeof rawCurrent !== 'object') {
    return '';
  }
  const val = (rawCurrent as { value?: unknown }).value;
  if (typeof val === 'string') {
    return val.trim();
  }
  if (Array.isArray(val) && typeof val[0] === 'string') {
    return val[0].trim();
  }
  return '';
}

function extractCustomVariableOptions(rawVar: Record<string, unknown>): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const pushOpt = (raw: string) => {
    const clean = sanitizePlainText(raw, 64);
    if (clean && !seen.has(clean)) {
      seen.add(clean);
      out.push(clean);
    }
  };

  if (Array.isArray(rawVar.options)) {
    for (const o of rawVar.options) {
      if (o && typeof o === 'object') {
        const val = (o as { value?: unknown }).value;
        if (typeof val === 'string' && val !== '$__all') {
          pushOpt(val);
        }
      }
    }
  }
  if (out.length === 0 && typeof rawVar.query === 'string') {
    for (const part of rawVar.query.split(',')) {
      pushOpt(part);
    }
  }
  return out;
}

function convertThresholds(rawThresholds: unknown): ThresholdIR[] {
  if (!rawThresholds || typeof rawThresholds !== 'object') {
    return [];
  }
  const steps = (rawThresholds as { steps?: unknown }).steps;
  if (!Array.isArray(steps)) {
    return [];
  }
  const converted: ThresholdIR[] = [];
  for (const s of steps) {
    if (!s || typeof s !== 'object') {
      continue;
    }
    const step = s as { value?: unknown; color?: unknown };
    if (step.value === null || step.value === undefined) {
      continue;
    }
    const num = Number(step.value);
    if (!Number.isFinite(num) || num < -1e9 || num > 1e9) {
      continue;
    }
    const rawColor = typeof step.color === 'string' ? step.color.toLowerCase() : 'green';
    const color: ThresholdColor = ALLOWED_COLOR_SET.has(rawColor)
      ? (rawColor as ThresholdColor)
      : 'orange';
    converted.push({ value: num, color });
  }
  converted.sort((a, b) => a.value - b.value);
  return converted.slice(0, 4);
}

function assignPackedGrid(panels: PanelIR[], widthHints: Array<'half' | 'full'>): void {
  let row = 0;
  let col: 0 | 1 = 0;
  for (let i = 0; i < panels.length; i++) {
    const wantFull = widthHints[i] === 'full';
    if (wantFull) {
      if (col !== 0) {
        row++;
        col = 0;
      }
      panels[i].layout = { row, col: 0, width: 'full' };
      row++;
      col = 0;
    } else {
      panels[i].layout = { row, col, width: 'half' };
      if (col === 0) {
        col = 1;
      } else {
        col = 0;
        row++;
      }
    }
  }

  // Guarantee zero row > 5 overflow and zero cell overlap even if mixed half/full pushed past row 5.
  if (panels.some((p) => p.layout.row > 5)) {
    for (let i = 0; i < panels.length; i++) {
      panels[i].layout = {
        row: Math.floor(i / 2),
        col: (i % 2) as 0 | 1,
        width: 'half',
      };
    }
  }
}

function makeUniquePanelId(title: string, index: number, used: Set<string>): string {
  const slug = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 22);
  const base = slug ? `panel-${slug}` : `panel-imported-${index}`;
  if (!used.has(base)) {
    used.add(base);
    return base;
  }
  for (let n = 2; n < 100; n++) {
    const candidate = `${base.slice(0, 28)}-${n}`;
    if (!used.has(candidate)) {
      used.add(candidate);
      return candidate;
    }
  }
  const fallback = `panel-imp-${index}`;
  used.add(fallback);
  return fallback;
}

function sanitizePlainText(input: string, maxRunes: number): string {
  const stripped = input
    .replace(/<[^>]*>/g, '')
    .replace(/\bon[a-z]+\s*=/gi, '')
    .replace(/javascript:|vbscript:|data:text\/html/gi, '')
    .replace(/[\u0000-\u001F\u007F-\u009F\u200E\u200F\u202A-\u202E\u2066-\u2069]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
  return Array.from(stripped).slice(0, maxRunes).join('').trim();
}
