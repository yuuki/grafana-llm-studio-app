import { BackendSrvRequest, getBackendSrv } from '@grafana/runtime';
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
  isReservedVariableName,
  MAX_QUERIES_PER_PANEL,
  MAX_VARIABLE_OPTIONS,
  MAX_VARIABLES,
  PanelIR,
  PanelType,
  QueryIR,
  QueryKind,
  SELECTED_DATASOURCE_REF,
  ThresholdColor,
  ThresholdIR,
  validateVariableValueClient,
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
  preservedAnnotations?: Record<string, string>;
  preservedLabels?: Record<string, string>;
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
  queryKind?: QueryKind;
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
    annotations?: Record<string, unknown>;
    labels?: Record<string, unknown>;
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
// Explicitly reject filtered label_values(metric{...}, label) so import never silently widens variable scope.
const LABEL_VALUES_WITH_METRIC =
  /^label_values\(\s*([a-zA-Z_:][a-zA-Z0-9_:]*)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$/;
const LABEL_VALUES_ONLY = /^label_values\(\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$/;
const LEGEND_VAR_TOKEN_REGEX = /(?:\$(?:\{|[a-zA-Z_])|\[\[[a-zA-Z_])/;
const EXPR_VAR_REF_REGEX =
  /\$\{([a-zA-Z_][a-zA-Z0-9_]*)(?::[^}]*)?\}|\$([a-zA-Z_][a-zA-Z0-9_]*)|\[\[([a-zA-Z_][a-zA-Z0-9_]*)(?::[^\]]*)?\]\]/g;
const FORBIDDEN_EXPR_MARKUP =
  /(?:<script|<\/script|<iframe|<svg|<img|<object|<embed|javascript:|vbscript:|data:text\/html|\bon[a-z]+\s*=)/i;

const ALLOWED_PANEL_TYPE_SET = new Set<string>(ALLOWED_PANEL_TYPES);
const ALLOWED_UNIT_SET = new Set<string>(ALLOWED_UNITS);
const ALLOWED_COLOR_SET = new Set<string>(ALLOWED_THRESHOLD_COLORS);
const ALLOWED_TIME_FROM_SET = new Set<string>(ALLOWED_TIME_FROM);
const ALLOWED_PROM_DS_TYPES = new Set<string>([
  'prometheus',
  'victoriametrics-metrics-datasource',
  'victoriametrics-datasource',
]);

function fetchWithOptionalAbort<T>(req: BackendSrvRequest, signal?: AbortSignal): Promise<{ data: T }> {
  return new Promise<{ data: T }>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Request aborted', 'AbortError'));
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
      reject(new DOMException('Request aborted', 'AbortError'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });

    sub = getBackendSrv()
      .fetch<T>(req)
      .subscribe({
        next: (res) => {
          if (settled) {
            return;
          }
          settled = true;
          cleanup();
          resolve(res);
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
            reject(new Error('Empty response from Grafana API'));
          }
        },
      });
  });
}

function extractStringRecord(
  input: unknown,
  excludeKeys?: Set<string>
): Record<string, string> | undefined {
  if (!input || typeof input !== 'object' || Array.isArray(input)) {
    return undefined;
  }
  const out: Record<string, string> = {};
  let count = 0;
  for (const [k, v] of Object.entries(input as Record<string, unknown>)) {
    if (excludeKeys?.has(k)) {
      continue;
    }
    if (typeof v === 'string') {
      out[k] = v;
      count++;
    }
  }
  return count > 0 ? out : undefined;
}

function sanitizeVersion(raw: unknown): number | undefined {
  if (typeof raw === 'number' && Number.isInteger(raw) && raw >= 0 && raw <= 1000000) {
    return raw;
  }
  return undefined;
}

/**
 * Lists existing Grafana dashboards visible to the logged-in user via /api/search.
 */
export async function listExistingDashboards(
  limit = 50,
  signal?: AbortSignal
): Promise<ExistingDashboardSummary[]> {
  const res = await fetchWithOptionalAbort<
    Array<{
      uid?: string;
      title?: string;
      folderUid?: string;
      folderTitle?: string;
    }>
  >(
    {
      url: '/api/search',
      method: 'GET',
      params: {
        type: 'dash-db',
        limit: Math.max(1, Math.min(limit, 100)),
      },
      showErrorAlert: false,
    },
    signal
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
 * Prefers Grafana's Kubernetes-style v1beta1 API so `metadata.resourceVersion`, labels, and
 * non-folder annotations are captured for optional opt-in optimistic-concurrency updates;
 * falls back to `/api/dashboards/uid/{uid}` ONLY on HTTP 404/410/501 (without resourceVersion,
 * restricting save mode to create-as-copy).
 */
export async function fetchExistingDashboardByUid(
  uid: string,
  signal?: AbortSignal
): Promise<FetchedExistingDashboard> {
  const cleanUid = uid.trim();
  if (!cleanUid) {
    throw new Error('Dashboard UID is required');
  }

  const namespace = getOrgNamespace();
  try {
    const res = await fetchWithOptionalAbort<RawDashboardV1Beta1>(
      {
        url: `/apis/dashboard.grafana.app/v1beta1/namespaces/${encodeURIComponent(namespace)}/dashboards/${encodeURIComponent(cleanUid)}`,
        method: 'GET',
        showErrorAlert: false,
      },
      signal
    );
    const spec = res.data?.spec;
    if (spec && typeof spec === 'object' && !Array.isArray(spec)) {
      const rawAnnotations = res.data?.metadata?.annotations;
      const folderAnn =
        rawAnnotations && typeof rawAnnotations['grafana.app/folder'] === 'string'
          ? sanitizePlainText(rawAnnotations['grafana.app/folder'], 64)
          : '';
      const rawName =
        typeof res.data?.metadata?.name === 'string' && res.data.metadata.name.trim()
          ? res.data.metadata.name.trim()
          : cleanUid;
      const rawRV =
        typeof res.data?.metadata?.resourceVersion === 'string' &&
        res.data.metadata.resourceVersion.trim()
          ? res.data.metadata.resourceVersion.trim()
          : undefined;
      return {
        rawDashboard: spec,
        source: {
          uid: rawName,
          resourceVersion: rawRV,
          folderUid: folderAnn,
          title: typeof spec.title === 'string' ? sanitizePlainText(spec.title, 120) : undefined,
          version: sanitizeVersion(spec.version),
        },
        preservedAnnotations: extractStringRecord(
          rawAnnotations,
          new Set(['grafana.app/folder'])
        ),
        preservedLabels: extractStringRecord(res.data?.metadata?.labels),
      };
    }
    throw new Error(`Dashboard ${cleanUid} returned invalid spec`);
  } catch (err: unknown) {
    if (signal?.aborted) {
      throw err;
    }
    const status = (err as { status?: number })?.status;
    if (status !== 404 && status !== 410 && status !== 501) {
      throw err;
    }
  }

  const legacy = await fetchWithOptionalAbort<RawLegacyDashboardEnvelope>(
    {
      url: `/api/dashboards/uid/${encodeURIComponent(cleanUid)}`,
      method: 'GET',
      showErrorAlert: false,
    },
    signal
  );
  const rawDash = legacy.data?.dashboard;
  if (!rawDash || typeof rawDash !== 'object' || Array.isArray(rawDash)) {
    throw new Error(`Dashboard ${cleanUid} returned no spec`);
  }
  const legacyFolderUid =
    typeof legacy.data?.meta?.folderUid === 'string'
      ? sanitizePlainText(legacy.data.meta.folderUid, 64)
      : '';
  return {
    rawDashboard: rawDash,
    source: {
      uid: typeof rawDash.uid === 'string' && rawDash.uid.trim() ? rawDash.uid.trim() : cleanUid,
      // Legacy /api/dashboards/uid does not provide Kubernetes resourceVersion;
      // leave empty so opt-in revision-checked PUT is safely unavailable.
      folderUid: legacyFolderUid,
      title: typeof rawDash.title === 'string' ? sanitizePlainText(rawDash.title, 120) : undefined,
      version: sanitizeVersion(rawDash.version),
    },
  };
}

/**
 * Deterministically converts an untrusted Grafana Dashboard JSON object into a
 * constrained Dashboard IR v1.1 draft + explicit conversion report.
 * - Strips HTML/JS/control/Bidi chars from all text fields via fixed-point sanitization.
 * - Filters to allowlisted visualization types and selected Prometheus datasource targets only (fail-closed).
 * - Rebinds datasource references strictly to `selected_prometheus`.
 * - Caps panels (<= maxPanels), queries per panel (<= 3), thresholds (<= 4), and variables (<= 4).
 * - Packs gridPos cleanly onto the non-overlapping 2-column IR grid.
 */
export function convertGrafanaDashboardToIR(
  rawInput: unknown,
  options: ImportConversionOptions
): ImportConversionResult {
  if (!rawInput || typeof rawInput !== 'object' || Array.isArray(rawInput)) {
    throw new Error('Dashboard JSON must be an object');
  }
  const raw = rawInput as Record<string, unknown>;
  const maxPanels = Math.max(1, Math.min(options.maxPanels ?? 6, 6));
  const warnings: string[] = [];
  const skippedPanels: SkippedImportItem[] = [];
  const skippedVariables: SkippedImportItem[] = [];

  const title =
    sanitizePlainText(typeof raw.title === 'string' ? raw.title : '', 120) || 'Imported Dashboard';
  const description = sanitizePlainText(
    typeof raw.description === 'string' ? raw.description : '',
    400
  );

  const rawTime = raw.time as { from?: unknown; to?: unknown } | undefined;
  let timeFrom: AllowedTimeFrom = 'now-15m';
  if (typeof rawTime?.from === 'string' && ALLOWED_TIME_FROM_SET.has(rawTime.from)) {
    timeFrom = rawTime.from as AllowedTimeFrom;
  } else if (typeof rawTime?.from === 'string' && rawTime.from !== '') {
    warnings.push(`Unsupported time.from "${sanitizePlainText(rawTime.from, 40)}" normalized to "now-15m"`);
  }

  // 1. Safely import up to 4 scoped template variables.
  const variables: VariableIR[] = [];
  const rawTemplating = raw.templating as { list?: unknown[] } | undefined;
  const rawVarList = Array.isArray(rawTemplating?.list) ? rawTemplating.list : [];
  const seenVarNames = new Set<string>();

  for (const item of rawVarList) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) {
      continue;
    }
    const v = item as Record<string, unknown>;
    const rawName = typeof v.name === 'string' ? v.name.trim() : '';
    const rawType = typeof v.type === 'string' ? v.type.trim() : 'unknown';

    if (v.hide === 2) {
      skippedVariables.push({
        title: rawName || '(unnamed)',
        type: rawType,
        reason: 'hidden dashboard variables (e.g., VictoriaMetrics WITH template variables) are not imported into IR v1.1',
      });
      continue;
    }

    if (variables.length >= MAX_VARIABLES) {
      skippedVariables.push({
        title: rawName || '(unnamed)',
        type: rawType,
        reason: `exceeded max ${MAX_VARIABLES} variables`,
      });
      continue;
    }
    if (
      !VALID_VAR_NAME.test(rawName) ||
      isReservedVariableName(rawName) ||
      seenVarNames.has(rawName)
    ) {
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
          reason:
            'only unfiltered Prometheus label_values(metric, label) or label_values(label) query variables are supported in IR v1.1',
        });
        continue;
      }
      const candidateDefault = currentVal || extractFirstConcreteOption(v.options);
      const safeDefault = sanitizePlainText(candidateDefault, 128);
      if (
        !safeDefault ||
        validateVariableValueClient(`variables.${rawName}.defaultValue`, safeDefault, 1, 128, false) !==
          null
      ) {
        skippedVariables.push({
          title: rawName,
          type: rawType,
          reason: 'query variable has no concrete safe defaultValue required by IR v1.1',
        });
        continue;
      }
      const varIR: VariableIR = {
        name: rawName,
        type: 'query',
        datasourceRef: SELECTED_DATASOURCE_REF,
        labelName: parsed.labelName,
        defaultValue: safeDefault,
        ...(label ? { label } : {}),
        ...(parsed.metricName ? { metricName: parsed.metricName } : {}),
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
          reason: 'no valid safe custom options found',
        });
        continue;
      }
      const boundedOpts = opts.slice(0, MAX_VARIABLE_OPTIONS);
      if (opts.length > MAX_VARIABLE_OPTIONS) {
        warnings.push(
          `Variable $${rawName}: truncated custom options from ${opts.length} to ${MAX_VARIABLE_OPTIONS}`
        );
      }
      const cleanCurrent = sanitizePlainText(currentVal, 64);
      const defVal =
        cleanCurrent && boundedOpts.includes(cleanCurrent) ? cleanCurrent : boundedOpts[0];
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
        reason: `variable type "${sanitizePlainText(rawType, 40)}" is not supported in IR v1.1`,
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
        reason: `panel type "${sanitizePlainText(pType, 40)}" is not supported in IR v1.1`,
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

    const rawTargets = Array.isArray(rp.targets) ? rp.targets : [];
    const importedQueries: QueryIR[] = [];
    let skippedTargetsCount = 0;
    let skippedUndeclaredVarCount = 0;
    let skippedWithTemplateCount = 0;

    for (const rt of rawTargets) {
      if (!rt || typeof rt !== 'object' || Array.isArray(rt)) {
        continue;
      }
      const target = rt as Record<string, unknown>;
      if (target.hide === true) {
        continue;
      }
      const hasTargetDs =
        target.datasource !== undefined && target.datasource !== null && target.datasource !== '';
      const targetDs = hasTargetDs ? target.datasource : rp.datasource;
      if (!isDatasourceCompatible(targetDs, options)) {
        skippedTargetsCount++;
        continue;
      }
      if (typeof target.withTemplate === 'string' && target.withTemplate.trim() !== '') {
        skippedWithTemplateCount++;
        warnings.push(
          `Panel "${safeTitle}": skipped query referencing external VictoriaMetrics WITH template`
        );
        continue;
      }
      if (typeof target.expr !== 'string' || target.expr.trim() === '') {
        continue;
      }
      const cleanExpr = sanitizeExprText(target.expr, 512);
      if (!cleanExpr) {
        warnings.push(`Panel "${safeTitle}": skipped query exceeding 512 chars or containing forbidden chars/markup`);
        continue;
      }
      const undeclaredVar = findUndeclaredOrLegacyVarRef(cleanExpr, seenVarNames);
      if (undeclaredVar) {
        skippedUndeclaredVarCount++;
        warnings.push(
          `Panel "${safeTitle}": skipped query referencing undeclared/unsupported variable "${undeclaredVar}"`
        );
        continue;
      }
      if (importedQueries.length >= MAX_QUERIES_PER_PANEL) {
        warnings.push(`Panel "${safeTitle}": truncated to max ${MAX_QUERIES_PER_PANEL} queries`);
        break;
      }
      let legend = sanitizePlainText(
        typeof target.legendFormat === 'string' ? target.legendFormat : '',
        80
      );
      if (legend && LEGEND_VAR_TOKEN_REGEX.test(legend)) {
        warnings.push(
          `Panel "${safeTitle}": omitted legendFormat containing template variable token`
        );
        legend = '';
      }
      importedQueries.push({
        refId: ALLOWED_QUERY_REF_IDS[importedQueries.length],
        ...(options.queryKind ? { kind: options.queryKind } : {}),
        expr: cleanExpr,
        ...(legend ? { legendFormat: legend } : {}),
      });
    }

    if (skippedTargetsCount > 0) {
      warnings.push(
        `Panel "${safeTitle}": skipped ${skippedTargetsCount} query target(s) bound to another datasource`
      );
    }
    if (importedQueries.length === 0) {
      const dialectLabel = options.queryKind === 'metricsql' ? 'MetricsQL' : 'Prometheus PromQL';
      const reason =
        skippedWithTemplateCount > 0
          ? 'queries referenced unsupported external VictoriaMetrics WITH templates'
          : skippedUndeclaredVarCount > 0
            ? 'queries referenced undeclared or unsupported template variables'
            : `no compatible ${dialectLabel} queries found for selected datasource`;
      skippedPanels.push({
        title: safeTitle,
        type: pType,
        reason,
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
      warnings.push(
        `Panel "${safeTitle}": unsupported unit "${sanitizePlainText(rawUnit, 40)}" mapped to "short"`
      );
    }

    const thresholds = convertThresholds(fieldDefaults.thresholds);
    const panelDesc = sanitizePlainText(
      typeof rp.description === 'string' ? rp.description : '',
      240
    );
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
        ? {
            query: {
              ...(importedQueries[0].kind ? { kind: importedQueries[0].kind } : {}),
              expr: importedQueries[0].expr,
              ...(importedQueries[0].legendFormat
                ? { legendFormat: importedQueries[0].legendFormat }
                : {}),
            },
          }
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
    if (!item || typeof item !== 'object' || Array.isArray(item)) {
      continue;
    }
    const p = item as Record<string, unknown>;
    if (p.type === 'row' && Array.isArray(p.panels)) {
      for (const child of p.panels) {
        if (child && typeof child === 'object' && !Array.isArray(child)) {
          out.push(child as Record<string, unknown>);
        }
      }
      continue;
    }
    out.push(p);
  }
  return out;
}

/**
 * Fail-closed datasource compatibility check:
 * Requires an explicit reference matching the user-selected Prometheus/VictoriaMetrics datasource.
 * Never permits null/empty/{} or "-- Grafana --" to smuggle foreign targets.
 */
function isDatasourceCompatible(
  ds: unknown,
  options: ImportConversionOptions
): boolean {
  if (ds === undefined || ds === null || ds === '') {
    return false;
  }
  if (typeof ds === 'string') {
    const trimmed = ds.trim();
    if (!trimmed || trimmed === '-- Grafana --' || trimmed === '-- Mixed --') {
      return false;
    }
    return (
      trimmed === options.selectedDatasourceUid ||
      (Boolean(options.selectedDatasourceName) && trimmed === options.selectedDatasourceName)
    );
  }
  if (typeof ds === 'object' && !Array.isArray(ds)) {
    const obj = ds as { uid?: unknown; type?: unknown };
    const uid = typeof obj.uid === 'string' ? obj.uid.trim() : '';
    const type = typeof obj.type === 'string' ? obj.type.trim().toLowerCase() : '';
    if (!uid || uid !== options.selectedDatasourceUid) {
      return false;
    }
    if (type && !ALLOWED_PROM_DS_TYPES.has(type)) {
      return false;
    }
    return true;
  }
  return false;
}

function parseLabelValuesQuery(
  rawQuery: unknown
): { labelName: string; metricName?: string } | null {
  let queryStr = '';
  if (typeof rawQuery === 'string') {
    queryStr = rawQuery.trim();
  } else if (rawQuery && typeof rawQuery === 'object' && !Array.isArray(rawQuery)) {
    const qObj = rawQuery as {
      query?: unknown;
      qryType?: unknown;
      label?: unknown;
      metric?: unknown;
      labelFilters?: unknown;
    };
    if (typeof qObj.query === 'string' && qObj.query.trim() !== '') {
      queryStr = qObj.query.trim();
    } else if (qObj.qryType === 1 && typeof qObj.label === 'string') {
      // Grafana 10+ Prometheus & victoriametrics-metrics-datasource object-form Label Values variable.
      // Reject non-empty labelFilters so import never silently widens variable scope.
      if (Array.isArray(qObj.labelFilters) && qObj.labelFilters.length > 0) {
        return null;
      }
      const labelName = qObj.label.trim();
      const metricName = typeof qObj.metric === 'string' ? qObj.metric.trim() : '';
      if (!VALID_LABEL_NAME.test(labelName) || labelName === '__name__') {
        return null;
      }
      if (metricName) {
        if (!VALID_METRIC_NAME.test(metricName)) {
          return null;
        }
        return { metricName, labelName };
      }
      return { labelName };
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
  if (!rawCurrent || typeof rawCurrent !== 'object' || Array.isArray(rawCurrent)) {
    return '';
  }
  const val = (rawCurrent as { value?: unknown }).value;
  if (typeof val === 'string') {
    const trimmed = val.trim();
    return trimmed && trimmed !== '$__all' ? trimmed : '';
  }
  if (Array.isArray(val)) {
    for (const entry of val) {
      if (typeof entry === 'string') {
        const trimmed = entry.trim();
        if (trimmed && trimmed !== '$__all') {
          return trimmed;
        }
      }
    }
  }
  return '';
}

function extractFirstConcreteOption(rawOptions: unknown): string {
  if (!Array.isArray(rawOptions)) {
    return '';
  }
  for (const o of rawOptions) {
    if (o && typeof o === 'object' && !Array.isArray(o)) {
      const val = (o as { value?: unknown }).value;
      if (typeof val === 'string') {
        const trimmed = val.trim();
        if (trimmed && trimmed !== '$__all') {
          return trimmed;
        }
      }
    }
  }
  return '';
}

function extractCustomVariableOptions(rawVar: Record<string, unknown>): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const pushOpt = (raw: string) => {
    const clean = sanitizePlainText(raw, 64);
    if (
      clean &&
      clean !== '$__all' &&
      !seen.has(clean) &&
      validateVariableValueClient('option', clean, 1, 64, true) === null
    ) {
      seen.add(clean);
      out.push(clean);
    }
  };

  if (Array.isArray(rawVar.options)) {
    for (const o of rawVar.options) {
      if (o && typeof o === 'object' && !Array.isArray(o)) {
        const val = (o as { value?: unknown }).value;
        if (typeof val === 'string' && val.trim() !== '$__all') {
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
  if (!rawThresholds || typeof rawThresholds !== 'object' || Array.isArray(rawThresholds)) {
    return [];
  }
  const steps = (rawThresholds as { steps?: unknown }).steps;
  if (!Array.isArray(steps)) {
    return [];
  }
  const converted: ThresholdIR[] = [];
  for (const s of steps) {
    if (!s || typeof s !== 'object' || Array.isArray(s)) {
      continue;
    }
    const step = s as { value?: unknown; color?: unknown };
    if (
      step.value === null ||
      step.value === undefined ||
      typeof step.value === 'boolean' ||
      (typeof step.value === 'string' && step.value.trim() === '') ||
      (typeof step.value !== 'number' && typeof step.value !== 'string')
    ) {
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

function sanitizeExprText(input: string, maxRunes: number): string | null {
  // Collapse valid PromQL multi-line indentation/newlines to single spaces before validating.
  const collapsed = input.replace(/[\r\n\t]+/g, ' ').trim();
  if (!collapsed || Array.from(collapsed).length > maxRunes) {
    return null;
  }
  for (let i = 0; i < collapsed.length; i++) {
    const code = collapsed.charCodeAt(i);
    if (
      code < 0x20 ||
      (code >= 0x7f && code <= 0x9f) ||
      code === 0x200e ||
      code === 0x200f ||
      (code >= 0x202a && code <= 0x202e) ||
      (code >= 0x2066 && code <= 0x2069)
    ) {
      return null;
    }
  }
  if (FORBIDDEN_EXPR_MARKUP.test(collapsed)) {
    return null;
  }
  return collapsed;
}

function findUndeclaredOrLegacyVarRef(expr: string, declaredVars: Set<string>): string | null {
  EXPR_VAR_REF_REGEX.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = EXPR_VAR_REF_REGEX.exec(expr)) !== null) {
    if (m[3]) {
      // Legacy [[var]] syntax is rejected by backend policy.
      return `[[${m[3]}]]`;
    }
    const name = m[1] ?? m[2] ?? '';
    if (name && !declaredVars.has(name)) {
      return `$${name}`;
    }
  }
  return null;
}

function sanitizePlainText(input: string, maxRunes: number): string {
  let cur = input.replace(
    /[\u0000-\u001F\u007F-\u009F\u200E\u200F\u202A-\u202E\u2066-\u2069]/g,
    ' '
  );
  // Fixed-point loop with space substitution defeats nested/split bypasses like
  // `<scr<script>ipt>ononclick=` without gluing adjacent tokens across removed tags.
  for (let pass = 0; pass < 5; pass++) {
    const next = cur
      .replace(/<[^>]*>/g, ' ')
      .replace(/[<>]/g, ' ')
      .replace(/\bon[a-z]+\s*=/gi, ' ')
      .replace(/javascript:|vbscript:|data:text\/html/gi, ' ');
    if (next === cur) {
      break;
    }
    cur = next;
  }
  const normalized = cur.replace(/\s+/g, ' ').trim();
  return Array.from(normalized).slice(0, maxRunes).join('').trim();
}
