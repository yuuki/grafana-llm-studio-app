import type { StatusReportIR } from './reportTypes';
export * from './reportTypes';

export const ALLOWED_SCHEMA_VERSIONS = ['1.0', '1.1'] as const;
export type IRSchemaVersion = (typeof ALLOWED_SCHEMA_VERSIONS)[number];
export const IR_SCHEMA_VERSION: IRSchemaVersion = '1.1';
export const SELECTED_DATASOURCE_REF = 'selected_prometheus' as const;

export const MAX_QUERIES_PER_PANEL = 3;
export const MAX_VARIABLES = 4;
export const MAX_VARIABLE_OPTIONS = 20;

export const ALLOWED_TIME_FROM = [
  'now-5m',
  'now-15m',
  'now-30m',
  'now-1h',
  'now-3h',
  'now-6h',
] as const;
export type AllowedTimeFrom = (typeof ALLOWED_TIME_FROM)[number];

export const ALLOWED_PANEL_TYPES = [
  'timeseries',
  'stat',
  'table',
  'gauge',
  'bargauge',
  'piechart',
  'state-timeline',
] as const;
export type PanelType = (typeof ALLOWED_PANEL_TYPES)[number];

export const ALLOWED_UNITS = [
  'short',
  'none',
  'percent',
  'percentunit',
  'reqps',
  'ops',
  's',
  'ms',
  'bytes',
  'decbytes',
  'Bps',
] as const;
export type AllowedUnit = (typeof ALLOWED_UNITS)[number];

export const ALLOWED_THRESHOLD_COLORS = ['green', 'yellow', 'orange', 'red', 'blue'] as const;
export type ThresholdColor = (typeof ALLOWED_THRESHOLD_COLORS)[number];

export const ALLOWED_WIDTHS = ['half', 'full'] as const;
export type LayoutWidth = (typeof ALLOWED_WIDTHS)[number];

export const ALLOWED_QUERY_REF_IDS = ['A', 'B', 'C'] as const;
export type QueryRefId = (typeof ALLOWED_QUERY_REF_IDS)[number];

export const ALLOWED_QUERY_KINDS = ['promql', 'metricsql'] as const;
export type QueryKind = (typeof ALLOWED_QUERY_KINDS)[number];

export const ALLOWED_VARIABLE_TYPES = ['query', 'custom'] as const;
export type VariableType = (typeof ALLOWED_VARIABLE_TYPES)[number];

export const ALLOWED_PATCH_OPS = [
  'update_panel',
  'add_panel',
  'remove_panel',
  'update_meta',
  'add_variable',
  'update_variable',
  'remove_variable',
] as const;
export type PatchOp = (typeof ALLOWED_PATCH_OPS)[number];

export interface TimeRangeIR {
  from: AllowedTimeFrom;
  to: 'now';
}

export interface VariableIR {
  name: string;
  label?: string;
  type: VariableType;
  datasourceRef?: typeof SELECTED_DATASOURCE_REF;
  labelName?: string;
  metricName?: string;
  options?: string[];
  defaultValue?: string;
  multi?: boolean;
  includeAll?: boolean;
}

export interface QueryIR {
  refId?: QueryRefId;
  kind?: QueryKind;
  expr: string;
  legendFormat?: string;
}

export interface ThresholdIR {
  value: number;
  color: ThresholdColor;
}

export interface LayoutHintIR {
  row: number;
  col: 0 | 1;
  width: LayoutWidth;
}

export interface PanelIR {
  id: string;
  type: PanelType;
  title: string;
  description?: string;
  datasourceRef: typeof SELECTED_DATASOURCE_REF;
  query?: QueryIR;
  queries?: QueryIR[];
  unit: AllowedUnit;
  thresholds?: ThresholdIR[];
  layout: LayoutHintIR;
}

export interface DashboardIR {
  schemaVersion: IRSchemaVersion;
  revision: number;
  title: string;
  description?: string;
  timeRange: TimeRangeIR;
  variables?: VariableIR[];
  panels: PanelIR[];
}

export interface ScopeFilter {
  labelName: string;
  labelValue: string;
}

export interface ImportSource {
  uid: string;
  resourceVersion?: string;
  folderUid?: string;
  title?: string;
  version?: number;
}

export interface MetricCatalogItem {
  name: string;
  type?: string;
  help?: string;
  unit?: string;
  labels?: string[];
}

export type ValidationCategory =
  | 'ok'
  | 'empty_result'
  | 'syntax_error'
  | 'permission_error'
  | 'timeout'
  | 'datasource_error';

export interface PanelValidationResult {
  panelId: string;
  queryHash: string;
  category: ValidationCategory;
  errorText?: string;
  seriesCount?: number;
  durationMs?: number;
}

export interface DiffEntry {
  op: string;
  panelId?: string;
  field?: string;
  before?: string;
  after?: string;
}

export type JobStatus =
  | 'generating'
  | 'validating_ir'
  | 'awaiting_preview'
  | 'repairing'
  | 'analyzing'
  | 'validating_report'
  | 'ready'
  | 'insufficient_metrics'
  | 'cancelled'
  | 'failed';

export type JobKind = 'dashboard' | 'report';

export interface CompletionMeta {
  finishReason: string;
  promptTokens?: number;
  completionTokens?: number;
  model?: string;
  providerMode: string;
}

export interface JobSnapshot {
  jobId: string;
  jobKind?: JobKind;
  status: JobStatus;
  datasourceUid: string;
  queryKind?: QueryKind;
  scope?: ScopeFilter;
  importSource?: ImportSource;
  currentIR?: DashboardIR;
  statusReport?: StatusReportIR;
  lastDiff?: DiffEntry[];
  summary?: string;
  warnings?: string[];
  errorMessage?: string;
  repairAttemptsUsed: number;
  maxRepairAttempts: number;
  canUndo: boolean;
  llm?: CompletionMeta;
  updatedAt: string;
}

export type JobOperation = 'generate' | 'patch' | 'repair' | 'analyze';

export interface JobStageEvent {
  jobId?: string;
  operation: JobOperation;
  stage: JobStatus;
  timestamp: string;
}

export interface PublicConfig {
  allowedDatasourceUids: string[];
  datasourceDialects?: Record<string, QueryKind>;
  llmMode: 'grafana-llm-app' | 'mock';
  modelTier: 'base' | 'large';
  maxPanels: number;
  maxRepairAttempts: number;
  validationTimeoutSeconds: number;
  maxConcurrentQueries: number;
  enableSseProgress?: boolean;
  mockBanner: boolean;
}

const PANEL_ID_REGEX = /^panel-[a-z0-9][a-z0-9-]{0,31}$/;
const VAR_NAME_REGEX = /^[a-zA-Z_][a-zA-Z0-9_]{0,31}$/;
const LABEL_NAME_REGEX = /^[a-zA-Z_][a-zA-Z0-9_]{0,63}$/;
const METRIC_NAME_REGEX = /^[a-zA-Z_:][a-zA-Z0-9_:]{0,127}$/;

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
  'timeFrom',
  'timeTo',
]);

const HTML_EVENT_HANDLER_REGEX = /\bon[a-z]+\s*=/i;
const LEGEND_VAR_TOKEN_REGEX = /(?:\$(?:\{|[a-zA-Z_])|\[\[[a-zA-Z_])/;
const FORBIDDEN_ACTIVE_FRAGMENTS = [
  '<script',
  '</script',
  '<iframe',
  '<svg',
  '<img',
  '<object',
  '<embed',
  'javascript:',
  'vbscript:',
  'data:text/html',
];

export function isReservedVariableName(name: string): boolean {
  return name.startsWith('__') || RESERVED_VAR_NAMES.has(name);
}

function countRunes(value: string): number {
  return Array.from(value).length;
}

function checkForbiddenRunes(field: string, value: string): string | null {
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code < 0x20 || (code >= 0x7f && code <= 0x9f)) {
      return `${field} contains forbidden control character`;
    }
    if (
      code === 0x200e ||
      code === 0x200f ||
      (code >= 0x202a && code <= 0x202e) ||
      (code >= 0x2066 && code <= 0x2069)
    ) {
      return `${field} contains forbidden bidirectional control character`;
    }
  }
  return null;
}

function validatePlainTextClient(
  field: string,
  value: string,
  minRunes: number,
  maxRunes: number
): string | null {
  const runeErr = checkForbiddenRunes(field, value);
  if (runeErr) {
    return runeErr;
  }
  const trimmed = value.trim();
  const n = countRunes(trimmed);
  if (n < minRunes || n > maxRunes) {
    return `${field} length must be between ${minRunes} and ${maxRunes} characters`;
  }
  if (trimmed.includes('<') || trimmed.includes('>')) {
    return `${field} must be plain text without HTML angle brackets`;
  }
  const lower = trimmed.toLowerCase();
  for (const frag of FORBIDDEN_ACTIVE_FRAGMENTS) {
    if (lower.includes(frag)) {
      return `${field} contains forbidden markup or script fragment`;
    }
  }
  if (HTML_EVENT_HANDLER_REGEX.test(trimmed)) {
    return `${field} contains forbidden inline event handler`;
  }
  return null;
}

function validateExprTextClient(field: string, value: string, maxRunes: number): string | null {
  const runeErr = checkForbiddenRunes(field, value);
  if (runeErr) {
    return runeErr;
  }
  const trimmed = value.trim();
  const n = countRunes(trimmed);
  if (n < 1 || n > maxRunes) {
    return `${field} length must be between 1 and ${maxRunes} characters`;
  }
  const lower = trimmed.toLowerCase();
  for (const frag of FORBIDDEN_ACTIVE_FRAGMENTS) {
    if (lower.includes(frag)) {
      return `${field} contains forbidden markup or script fragment`;
    }
  }
  if (HTML_EVENT_HANDLER_REGEX.test(trimmed)) {
    return `${field} contains forbidden inline event handler`;
  }
  return null;
}

export function validateVariableValueClient(
  field: string,
  value: string,
  minRunes: number,
  maxRunes: number,
  isCustomOption: boolean
): string | null {
  const textErr = validatePlainTextClient(field, value, minRunes, maxRunes);
  if (textErr) {
    return textErr;
  }
  if (
    value.includes('"') ||
    value.includes("'") ||
    value.includes('\\') ||
    value.includes('`') ||
    value.includes('$') ||
    value.includes('[[')
  ) {
    return `${field} contains forbidden quote, escape, or template variable token`;
  }
  if (isCustomOption && (value.includes(',') || value.includes(' : '))) {
    return `${field} cannot contain comma or key-value separator`;
  }
  return null;
}

/**
 * Returns the ordered list of PromQL queries defined on a panel (supports both v1.0 `query` and v1.1 `queries`).
 */
export function getPanelQueries(panel: PanelIR): QueryIR[] {
  if (Array.isArray(panel.queries) && panel.queries.length > 0) {
    return panel.queries;
  }
  if (panel.query) {
    return [panel.query];
  }
  return [];
}

/**
 * Deterministic client-side guard preventing invalid IR from ever reaching
 * Scenes preview or Foundation SDK export/save.
 */
export function validateIRClient(doc: DashboardIR, maxPanels = 6): string[] {
  const issues: string[] = [];
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) {
    return ['DashboardIR must be an object'];
  }
  validateDashboardMetaClient(doc, issues);
  validateVariablesClient(doc.variables, issues);

  const cap = Math.max(1, Math.min(maxPanels, 6));
  if (!Array.isArray(doc.panels) || doc.panels.length < 1 || doc.panels.length > cap) {
    issues.push(`Panel count must be between 1 and ${cap}`);
  }

  const seenIds = new Set<string>();
  const occupiedCells = new Set<string>();
  for (const p of Array.isArray(doc.panels) ? doc.panels : []) {
    validatePanelClient(p, seenIds, occupiedCells, issues);
  }
  return issues;
}

function validateDashboardMetaClient(doc: DashboardIR, issues: string[]): void {
  if (!ALLOWED_SCHEMA_VERSIONS.includes(doc.schemaVersion)) {
    issues.push(`Unsupported schemaVersion: ${doc.schemaVersion}`);
  }
  if (!Number.isInteger(doc.revision) || doc.revision < 1 || doc.revision > 10000) {
    issues.push(`Invalid revision: ${doc.revision}`);
  }
  if (typeof doc.title !== 'string') {
    issues.push('Dashboard title must be a string');
  } else {
    const titleErr = validatePlainTextClient('title', doc.title, 1, 120);
    if (titleErr) {
      issues.push(titleErr);
    }
  }
  if (doc.description !== undefined) {
    if (typeof doc.description !== 'string') {
      issues.push('Dashboard description must be a string');
    } else if (doc.description !== '') {
      const descErr = validatePlainTextClient('description', doc.description, 0, 400);
      if (descErr) {
        issues.push(descErr);
      }
    }
  }
  if (!doc.timeRange || !ALLOWED_TIME_FROM.includes(doc.timeRange.from) || doc.timeRange.to !== 'now') {
    issues.push(`Unsupported timeRange: ${doc.timeRange?.from}..${doc.timeRange?.to}`);
  }
}

function validateVariablesClient(variables: VariableIR[] | undefined, issues: string[]): void {
  if (variables === undefined) {
    return;
  }
  if (!Array.isArray(variables)) {
    issues.push('variables must be an array');
    return;
  }
  if (variables.length > MAX_VARIABLES) {
    issues.push(`Variable count ${variables.length} exceeds max ${MAX_VARIABLES}`);
  }
  const seenVars = new Set<string>();
  for (const v of variables) {
    validateSingleVariableClient(v, seenVars, issues);
  }
}

function validateSingleVariableClient(
  v: VariableIR,
  seenVars: Set<string>,
  issues: string[]
): void {
  if (!v || typeof v !== 'object' || Array.isArray(v)) {
    issues.push('Each variable must be an object');
    return;
  }
  if (
    typeof v.name !== 'string' ||
    !VAR_NAME_REGEX.test(v.name) ||
    isReservedVariableName(v.name) ||
    seenVars.has(v.name)
  ) {
    issues.push(`Invalid, reserved, or duplicate variable name: ${String(v.name)}`);
  }
  seenVars.add(v.name);
  if (v.label !== undefined) {
    if (typeof v.label !== 'string') {
      issues.push(`Variable ${v.name} label must be a string`);
    } else if (v.label !== '') {
      const labelErr = validatePlainTextClient(`variables.${v.name}.label`, v.label, 0, 64);
      if (labelErr) {
        issues.push(labelErr);
      }
    }
  }
  if (!ALLOWED_VARIABLE_TYPES.includes(v.type)) {
    issues.push(`Unsupported variable type on ${v.name}: ${v.type}`);
    return;
  }
  if (v.type === 'query') {
    validateQueryVariableClient(v, issues);
  } else if (v.type === 'custom') {
    validateCustomVariableClient(v, issues);
  }
}

function validateQueryVariableClient(v: VariableIR, issues: string[]): void {
  if (v.datasourceRef !== SELECTED_DATASOURCE_REF) {
    issues.push(`Forbidden datasourceRef on query variable ${v.name}: ${v.datasourceRef}`);
  }
  if (
    typeof v.labelName !== 'string' ||
    !LABEL_NAME_REGEX.test(v.labelName) ||
    v.labelName === '__name__'
  ) {
    issues.push(`Invalid labelName on query variable ${v.name}`);
  }
  if (v.metricName !== undefined && v.metricName !== '') {
    if (typeof v.metricName !== 'string' || !METRIC_NAME_REGEX.test(v.metricName)) {
      issues.push(`Invalid metricName on query variable ${v.name}`);
    }
  }
  if (v.options && v.options.length > 0) {
    issues.push(`Query variable ${v.name} must not define custom options`);
  }
  if (typeof v.defaultValue !== 'string') {
    issues.push(`Query variable ${v.name} must define a non-empty defaultValue`);
  } else {
    const defErr = validateVariableValueClient(
      `variables.${v.name}.defaultValue`,
      v.defaultValue,
      1,
      128,
      false
    );
    if (defErr) {
      issues.push(defErr);
    }
  }
}

function validateCustomVariableClient(v: VariableIR, issues: string[]): void {
  if (v.datasourceRef) {
    issues.push(`Custom variable ${v.name} must not define datasourceRef`);
  }
  if (v.labelName || v.metricName) {
    issues.push(`Custom variable ${v.name} must not define labelName or metricName`);
  }
  if (!Array.isArray(v.options) || v.options.length < 1 || v.options.length > MAX_VARIABLE_OPTIONS) {
    issues.push(`Custom variable ${v.name} must define 1..${MAX_VARIABLE_OPTIONS} options`);
    return;
  }
  const seenOpts = new Set<string>();
  for (let oi = 0; oi < v.options.length; oi++) {
    const opt = v.options[oi];
    if (typeof opt !== 'string') {
      issues.push(`Custom variable ${v.name} option[${oi}] must be a string`);
      continue;
    }
    const optErr = validateVariableValueClient(
      `variables.${v.name}.options[${oi}]`,
      opt,
      1,
      64,
      true
    );
    if (optErr) {
      issues.push(optErr);
    }
    if (seenOpts.has(opt)) {
      issues.push(`Custom variable ${v.name} has duplicate option ${opt}`);
    }
    seenOpts.add(opt);
  }
  if (v.defaultValue !== undefined && v.defaultValue !== '') {
    if (typeof v.defaultValue !== 'string') {
      issues.push(`Custom variable ${v.name} defaultValue must be a string`);
    } else {
      const defErr = validateVariableValueClient(
        `variables.${v.name}.defaultValue`,
        v.defaultValue,
        1,
        128,
        false
      );
      if (defErr) {
        issues.push(defErr);
      } else if (!v.options.includes(v.defaultValue)) {
        issues.push(`Custom variable ${v.name} defaultValue must be in options`);
      }
    }
  }
}

function validatePanelClient(
  p: PanelIR,
  seenIds: Set<string>,
  occupiedCells: Set<string>,
  issues: string[]
): void {
  if (!p || typeof p !== 'object' || Array.isArray(p)) {
    issues.push('Each panel must be an object');
    return;
  }
  if (typeof p.id !== 'string' || !PANEL_ID_REGEX.test(p.id) || seenIds.has(p.id)) {
    issues.push(`Invalid or duplicate panel id: ${String(p.id)}`);
  }
  seenIds.add(p.id);
  if (!ALLOWED_PANEL_TYPES.includes(p.type)) {
    issues.push(`Unsupported panel type: ${p.type}`);
  }
  if (typeof p.title !== 'string') {
    issues.push(`Invalid panel title on ${p.id}`);
  } else {
    const pTitleErr = validatePlainTextClient(`panels.${p.id}.title`, p.title, 1, 80);
    if (pTitleErr) {
      issues.push(pTitleErr);
    }
  }
  if (p.description !== undefined) {
    if (typeof p.description !== 'string') {
      issues.push(`Invalid panel description on ${p.id}`);
    } else if (p.description !== '') {
      const pDescErr = validatePlainTextClient(`panels.${p.id}.description`, p.description, 0, 240);
      if (pDescErr) {
        issues.push(pDescErr);
      }
    }
  }
  if (p.datasourceRef !== SELECTED_DATASOURCE_REF) {
    issues.push(`Forbidden datasourceRef on ${p.id}: ${p.datasourceRef}`);
  }

  validatePanelQueriesClient(p, issues);

  if (!ALLOWED_UNITS.includes(p.unit)) {
    issues.push(`Unsupported unit on ${p.id}: ${p.unit}`);
  }
  validatePanelThresholdsClient(p.id, p.thresholds, issues);
  validatePanelLayoutClient(p.id, p.layout, occupiedCells, issues);
}

function validatePanelQueriesClient(p: PanelIR, issues: string[]): void {
  const hasSingle = p.query !== undefined;
  const hasQueriesField = p.queries !== undefined;
  const hasMulti = Array.isArray(p.queries) && p.queries.length > 0;
  if (hasSingle && hasQueriesField) {
    issues.push(`Panel ${p.id} cannot specify both query and queries`);
    return;
  }
  if (hasQueriesField && (!Array.isArray(p.queries) || p.queries.length === 0)) {
    issues.push(`Panel ${p.id} queries must contain 1..${MAX_QUERIES_PER_PANEL} items`);
    return;
  }
  if (!hasSingle && !hasMulti) {
    issues.push(`Panel ${p.id} must specify query or 1..${MAX_QUERIES_PER_PANEL} queries`);
    return;
  }

  const qs = getPanelQueries(p);
  if (qs.length > MAX_QUERIES_PER_PANEL) {
    issues.push(`Panel ${p.id} exceeds max ${MAX_QUERIES_PER_PANEL} queries`);
  }
  const seenRefIds = new Set<string>();
  qs.forEach((q, qi) => validatePanelQueryEntryClient(p.id, q, qi, seenRefIds, issues));
}

function validatePanelQueryEntryClient(
  panelId: string,
  q: QueryIR,
  qi: number,
  seenRefIds: Set<string>,
  issues: string[]
): void {
  if (!q || typeof q !== 'object' || Array.isArray(q)) {
    issues.push(`Invalid query object on ${panelId}[${qi}]`);
    return;
  }
  if (typeof q.expr !== 'string') {
    issues.push(`Invalid query expr on ${panelId}[${qi}]`);
  } else {
    const exprErr = validateExprTextClient(`panels.${panelId}.queries[${qi}].expr`, q.expr, 512);
    if (exprErr) {
      issues.push(exprErr);
    }
  }
  if (q.legendFormat !== undefined) {
    if (typeof q.legendFormat !== 'string') {
      issues.push(`Invalid legendFormat on ${panelId}[${qi}]`);
    } else if (q.legendFormat !== '') {
      const lfErr = validatePlainTextClient(
        `panels.${panelId}.queries[${qi}].legendFormat`,
        q.legendFormat,
        0,
        80
      );
      if (lfErr) {
        issues.push(lfErr);
      } else if (LEGEND_VAR_TOKEN_REGEX.test(q.legendFormat)) {
        issues.push(`legendFormat on ${panelId}[${qi}] must not contain template variable tokens`);
      }
    }
  }
  const refId = q.refId || ALLOWED_QUERY_REF_IDS[qi];
  if (!refId || !ALLOWED_QUERY_REF_IDS.includes(refId)) {
    issues.push(`Invalid query refId on ${panelId}[${qi}]: ${q.refId}`);
  } else if (seenRefIds.has(refId)) {
    issues.push(`Duplicate query refId on ${panelId}: ${refId}`);
  } else {
    seenRefIds.add(refId);
  }
  if (q.kind !== undefined && !ALLOWED_QUERY_KINDS.includes(q.kind)) {
    issues.push(`Invalid query kind on ${panelId}[${qi}]: ${q.kind}`);
  }
}

function validatePanelThresholdsClient(
  panelId: string,
  thresholds: ThresholdIR[] | undefined,
  issues: string[]
): void {
  if (thresholds === undefined) {
    return;
  }
  if (!Array.isArray(thresholds)) {
    issues.push(`Thresholds must be an array on ${panelId}`);
    return;
  }
  if (thresholds.length > 4) {
    issues.push(`Too many thresholds on ${panelId}`);
  }
  let prev = -Infinity;
  for (const th of thresholds) {
    if (!th || typeof th !== 'object' || typeof th.value !== 'number') {
      issues.push(`Invalid threshold entry on ${panelId}`);
      continue;
    }
    if (!Number.isFinite(th.value) || th.value < -1e9 || th.value > 1e9) {
      issues.push(`Threshold value out of range on ${panelId}`);
    }
    if (th.value < prev) {
      issues.push(`Thresholds must be ascending on ${panelId}`);
    }
    prev = th.value;
    if (!ALLOWED_THRESHOLD_COLORS.includes(th.color)) {
      issues.push(`Unsupported threshold color on ${panelId}: ${th.color}`);
    }
  }
}

function validatePanelLayoutClient(
  panelId: string,
  layout: PanelIR['layout'],
  occupiedCells: Set<string>,
  issues: string[]
): void {
  if (
    !layout ||
    typeof layout !== 'object' ||
    !Number.isInteger(layout.row) ||
    layout.row < 0 ||
    layout.row > 5
  ) {
    issues.push(`Invalid layout.row on ${panelId}`);
    return;
  }
  if (layout.col !== 0 && layout.col !== 1) {
    issues.push(`Invalid layout.col on ${panelId}`);
    return;
  }
  if (!ALLOWED_WIDTHS.includes(layout.width)) {
    issues.push(`Invalid layout.width on ${panelId}`);
    return;
  }
  if (layout.width === 'full' && layout.col !== 0) {
    issues.push(`Full-width panel ${panelId} must have col=0`);
    return;
  }
  const cols = layout.width === 'full' ? [0, 1] : [layout.col];
  for (const c of cols) {
    const key = `${layout.row}:${c}`;
    if (occupiedCells.has(key)) {
      issues.push(`Layout overlap on ${panelId} at row=${layout.row}, col=${c}`);
    }
    occupiedCells.add(key);
  }
}

/**
 * Computes the first 16 bytes (32 hex chars) of SHA-256 digest matching Go ir.QueryHash.
 * Uses WebCrypto when available and falls back to a pure-TS SHA-256 implementation so
 * non-HTTPS air-gapped environments and jsdom tests work without Node polyfills.
 */
export async function computeQueryHash(expr: string): Promise<string> {
  const normalized = expr.trim();
  if (
    typeof globalThis.crypto !== 'undefined' &&
    globalThis.crypto.subtle &&
    typeof TextEncoder !== 'undefined'
  ) {
    const data = new TextEncoder().encode(normalized);
    const hashBuf = await globalThis.crypto.subtle.digest('SHA-256', data);
    const bytes = new Uint8Array(hashBuf).slice(0, 16);
    return Array.from(bytes)
      .map((b) => b.toString(16).padStart(2, '0'))
      .join('');
  }
  return sha256HexPure(normalized).slice(0, 32);
}

/**
 * Computes the deterministic hash over all queries on a panel, matching Go ir.PanelQueryHash byte-for-byte.
 * For single-query panels this equals computeQueryHash(expr).
 */
export async function computePanelQueryHash(
  queries: Array<{ refId?: string; expr: string }>
): Promise<string> {
  if (queries.length === 0) {
    return computeQueryHash('');
  }
  if (queries.length === 1) {
    return computeQueryHash(queries[0].expr);
  }
  const canonical = queries
    .map((q, idx) => {
      const refId = q.refId || ALLOWED_QUERY_REF_IDS[idx] || '';
      return `${refId}:${q.expr.trim()}`;
    })
    .join('\n');
  return computeQueryHash(canonical);
}

const SHA256_K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

function utf8Bytes(str: string): Uint8Array {
  const out: number[] = [];
  for (let i = 0; i < str.length; i++) {
    const code = str.charCodeAt(i);
    if (code < 0x80) {
      out.push(code);
    } else if (code < 0x800) {
      out.push(0xc0 | (code >> 6), 0x80 | (code & 0x3f));
    } else if (code >= 0xd800 && code <= 0xdbff && i + 1 < str.length) {
      const next = str.charCodeAt(i + 1);
      if (next >= 0xdc00 && next <= 0xdfff) {
        i++;
        const cp = 0x10000 + (((code & 0x3ff) << 10) | (next & 0x3ff));
        out.push(
          0xf0 | (cp >> 18),
          0x80 | ((cp >> 12) & 0x3f),
          0x80 | ((cp >> 6) & 0x3f),
          0x80 | (cp & 0x3f)
        );
      } else {
        out.push(0xef, 0xbf, 0xbd);
      }
    } else if (code >= 0xd800 && code <= 0xdfff) {
      out.push(0xef, 0xbf, 0xbd);
    } else {
      out.push(0xe0 | (code >> 12), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f));
    }
  }
  return new Uint8Array(out);
}

function sha256HexPure(input: string): string {
  const msg = utf8Bytes(input);
  const bitLen = msg.length * 8;
  const paddedLen = Math.ceil((msg.length + 9) / 64) * 64;
  const buf = new Uint8Array(paddedLen);
  buf.set(msg);
  buf[msg.length] = 0x80;
  const view = new DataView(buf.buffer);
  view.setUint32(paddedLen - 8, Math.floor(bitLen / 0x100000000), false);
  view.setUint32(paddedLen - 4, bitLen >>> 0, false);

  let h0 = 0x6a09e667;
  let h1 = 0xbb67ae85;
  let h2 = 0x3c6ef372;
  let h3 = 0xa54ff53a;
  let h4 = 0x510e527f;
  let h5 = 0x9b05688c;
  let h6 = 0x1f83d9ab;
  let h7 = 0x5be0cd19;

  const w = new Uint32Array(64);
  const rotr = (x: number, n: number) => (x >>> n) | (x << (32 - n));

  for (let offset = 0; offset < paddedLen; offset += 64) {
    for (let i = 0; i < 16; i++) {
      w[i] = view.getUint32(offset + i * 4, false);
    }
    for (let i = 16; i < 64; i++) {
      const s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
      const s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let a = h0;
    let b = h1;
    let c = h2;
    let d = h3;
    let e = h4;
    let f = h5;
    let g = h6;
    let h = h7;
    for (let i = 0; i < 64; i++) {
      const S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
      const ch = (e & f) ^ (~e & g);
      const temp1 = (h + S1 + ch + SHA256_K[i] + w[i]) >>> 0;
      const S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const temp2 = (S0 + maj) >>> 0;
      h = g;
      g = f;
      f = e;
      e = (d + temp1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (temp1 + temp2) >>> 0;
    }
    h0 = (h0 + a) >>> 0;
    h1 = (h1 + b) >>> 0;
    h2 = (h2 + c) >>> 0;
    h3 = (h3 + d) >>> 0;
    h4 = (h4 + e) >>> 0;
    h5 = (h5 + f) >>> 0;
    h6 = (h6 + g) >>> 0;
    h7 = (h7 + h) >>> 0;
  }

  return [h0, h1, h2, h3, h4, h5, h6, h7]
    .map((v) => v.toString(16).padStart(8, '0'))
    .join('');
}
