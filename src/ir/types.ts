export const IR_SCHEMA_VERSION = '1.0' as const;
export const SELECTED_DATASOURCE_REF = 'selected_prometheus' as const;

export const ALLOWED_TIME_FROM = [
  'now-5m',
  'now-15m',
  'now-30m',
  'now-1h',
  'now-3h',
  'now-6h',
] as const;
export type AllowedTimeFrom = (typeof ALLOWED_TIME_FROM)[number];

export const ALLOWED_PANEL_TYPES = ['timeseries', 'stat'] as const;
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

export interface TimeRangeIR {
  from: AllowedTimeFrom;
  to: 'now';
}

export interface QueryIR {
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
  query: QueryIR;
  unit: AllowedUnit;
  thresholds?: ThresholdIR[];
  layout: LayoutHintIR;
}

export interface DashboardIR {
  schemaVersion: typeof IR_SCHEMA_VERSION;
  revision: number;
  title: string;
  description?: string;
  timeRange: TimeRangeIR;
  panels: PanelIR[];
}

export interface ScopeFilter {
  labelName: string;
  labelValue: string;
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
  | 'ready'
  | 'insufficient_metrics'
  | 'cancelled'
  | 'failed';

export interface CompletionMeta {
  finishReason: string;
  promptTokens?: number;
  completionTokens?: number;
  model?: string;
  providerMode: string;
}

export interface JobSnapshot {
  jobId: string;
  status: JobStatus;
  datasourceUid: string;
  scope?: ScopeFilter;
  currentIR?: DashboardIR;
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

export interface PublicConfig {
  allowedDatasourceUids: string[];
  llmMode: 'grafana-llm-app' | 'mock';
  modelTier: 'base' | 'large';
  maxPanels: number;
  maxRepairAttempts: number;
  validationTimeoutSeconds: number;
  maxConcurrentQueries: number;
  mockBanner: boolean;
}

const PANEL_ID_REGEX = /^panel-[a-z0-9][a-z0-9-]{0,31}$/;

/**
 * Deterministic client-side guard preventing invalid IR from ever reaching
 * Scenes preview or Foundation SDK export/save.
 */
export function validateIRClient(doc: DashboardIR, maxPanels = 6): string[] {
  const issues: string[] = [];
  if (!doc || typeof doc !== 'object') {
    return ['DashboardIR must be an object'];
  }
  if (doc.schemaVersion !== IR_SCHEMA_VERSION) {
    issues.push(`Unsupported schemaVersion: ${doc.schemaVersion}`);
  }
  if (!Number.isInteger(doc.revision) || doc.revision < 1 || doc.revision > 10000) {
    issues.push(`Invalid revision: ${doc.revision}`);
  }
  if (!doc.title || doc.title.trim().length === 0 || doc.title.length > 120) {
    issues.push('Dashboard title must be 1..120 characters');
  }
  if (!doc.timeRange || !ALLOWED_TIME_FROM.includes(doc.timeRange.from) || doc.timeRange.to !== 'now') {
    issues.push(`Unsupported timeRange: ${doc.timeRange?.from}..${doc.timeRange?.to}`);
  }
  const cap = Math.max(1, Math.min(maxPanels, 6));
  if (!Array.isArray(doc.panels) || doc.panels.length < 1 || doc.panels.length > cap) {
    issues.push(`Panel count must be between 1 and ${cap}`);
  }

  const seenIds = new Set<string>();
  const occupiedCells = new Set<string>();
  for (const p of doc.panels ?? []) {
    if (!PANEL_ID_REGEX.test(p.id) || seenIds.has(p.id)) {
      issues.push(`Invalid or duplicate panel id: ${p.id}`);
    }
    seenIds.add(p.id);
    if (!ALLOWED_PANEL_TYPES.includes(p.type)) {
      issues.push(`Unsupported panel type: ${p.type}`);
    }
    if (!p.title || p.title.trim().length === 0 || p.title.length > 80) {
      issues.push(`Invalid panel title on ${p.id}`);
    }
    if (p.datasourceRef !== SELECTED_DATASOURCE_REF) {
      issues.push(`Forbidden datasourceRef on ${p.id}: ${p.datasourceRef}`);
    }
    if (!p.query?.expr || p.query.expr.trim().length === 0 || p.query.expr.length > 512) {
      issues.push(`Invalid query expr on ${p.id}`);
    }
    if (!ALLOWED_UNITS.includes(p.unit)) {
      issues.push(`Unsupported unit on ${p.id}: ${p.unit}`);
    }
    if (p.thresholds) {
      if (p.thresholds.length > 4) {
        issues.push(`Too many thresholds on ${p.id}`);
      }
      let prev = -Infinity;
      for (const th of p.thresholds) {
        if (!Number.isFinite(th.value) || th.value < -1e9 || th.value > 1e9) {
          issues.push(`Threshold value out of range on ${p.id}`);
        }
        if (th.value < prev) {
          issues.push(`Thresholds must be ascending on ${p.id}`);
        }
        prev = th.value;
        if (!ALLOWED_THRESHOLD_COLORS.includes(th.color)) {
          issues.push(`Unsupported threshold color on ${p.id}: ${th.color}`);
        }
      }
    }
    if (!p.layout || !Number.isInteger(p.layout.row) || p.layout.row < 0 || p.layout.row > 5) {
      issues.push(`Invalid layout.row on ${p.id}`);
    } else if (p.layout.col !== 0 && p.layout.col !== 1) {
      issues.push(`Invalid layout.col on ${p.id}`);
    } else if (!ALLOWED_WIDTHS.includes(p.layout.width)) {
      issues.push(`Invalid layout.width on ${p.id}`);
    } else if (p.layout.width === 'full' && p.layout.col !== 0) {
      issues.push(`Full-width panel ${p.id} must have col=0`);
    } else {
      const cols = p.layout.width === 'full' ? [0, 1] : [p.layout.col];
      for (const c of cols) {
        const key = `${p.layout.row}:${c}`;
        if (occupiedCells.has(key)) {
          issues.push(`Layout overlap on ${p.id} at row=${p.layout.row}, col=${c}`);
        }
        occupiedCells.add(key);
      }
    }
  }
  return issues;
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
    let code = str.charCodeAt(i);
    if (code < 0x80) {
      out.push(code);
    } else if (code < 0x800) {
      out.push(0xc0 | (code >> 6), 0x80 | (code & 0x3f));
    } else if (code >= 0xd800 && code <= 0xdbff && i + 1 < str.length) {
      const next = str.charCodeAt(++i);
      const cp = 0x10000 + (((code & 0x3ff) << 10) | (next & 0x3ff));
      out.push(
        0xf0 | (cp >> 18),
        0x80 | ((cp >> 12) & 0x3f),
        0x80 | ((cp >> 6) & 0x3f),
        0x80 | (cp & 0x3f)
      );
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
