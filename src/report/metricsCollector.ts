import { getBackendSrv } from '@grafana/runtime';
import { extractApiError } from '../api/studioClient';
import {
  AnomalyHint,
  BaselineMode,
  CollectionWarningItem,
  MAX_METRIC_EVIDENCE_ITEMS,
  MetricBaselineStats,
  MetricEvidenceItem,
  MetricTrend,
  MetricWindowStats,
  ReportWindow,
  ThresholdBreach,
} from '../ir/reportTypes';
import { ExtractedReportPanel, ExtractedReportQuery, RawPanelThresholds } from './dashboardExtractor';

export interface CollectMetricsOptions {
  window: ReportWindow;
  baselineMode: BaselineMode;
  maxConcurrency?: number;
  perQueryTimeoutMs?: number;
  overallDeadlineMs?: number;
  nowMs?: number;
  signal?: AbortSignal;
}

export interface CollectMetricsResult {
  metrics: MetricEvidenceItem[];
  warnings: CollectionWarningItem[];
  analyzedPanelCount: number;
}

interface GrafanaFrameField {
  name?: string;
  type?: string;
  labels?: Record<string, string>;
}

interface GrafanaDataFrame {
  name?: string;
  schema?: {
    fields?: GrafanaFrameField[];
  };
  data?: {
    values?: unknown[][];
  };
}

interface GrafanaDSQueryResponse {
  results?: Record<
    string,
    {
      status?: number;
      error?: string;
      frames?: GrafanaDataFrame[];
    }
  >;
}

interface ParsedSeries {
  fullLabelKey: string;
  labelKey: string;
  timestamps: number[];
  values: number[];
}

const WINDOW_CONFIG: Record<ReportWindow, { durationMs: number; intervalMs: number }> = {
  '15m': { durationMs: 15 * 60 * 1000, intervalMs: 15_000 },
  '1h': { durationMs: 60 * 60 * 1000, intervalMs: 60_000 },
  '6h': { durationMs: 6 * 60 * 60 * 1000, intervalMs: 300_000 },
  '24h': { durationMs: 24 * 60 * 60 * 1000, intervalMs: 900_000 },
};

const LOWER_IS_WORSE_RE = /(\bup\b|avail|healthy|ready|free|hit_ratio|cache_hit|success_rate)/i;
const EPSILON_BASELINE = 1e-6;
const MAX_DATE_MS = 8.64e15;

function roundNum(v: number): number {
  if (!Number.isFinite(v)) {
    return 0;
  }
  const abs = Math.abs(v);
  if (abs >= 100) {
    return Math.round(v * 100) / 100;
  }
  if (abs >= 1) {
    return Math.round(v * 1000) / 1000;
  }
  return Math.round(v * 100000) / 100000;
}

function fullCanonicalLabelString(labels: Record<string, string> | undefined, fallbackName: string): string {
  if (!labels || Object.keys(labels).length === 0) {
    return fallbackName || 'series';
  }
  const keys = Object.keys(labels).sort();
  return keys.map((k) => `${k}=${labels[k]}`).join(', ');
}

function parseFiniteNumericCell(rawV: unknown): number {
  if (typeof rawV === 'number') {
    return Number.isFinite(rawV) ? rawV : Number.NaN;
  }
  if (typeof rawV === 'string') {
    const trimmed = rawV.trim();
    if (trimmed === '') {
      return Number.NaN;
    }
    const parsed = Number(trimmed);
    return Number.isFinite(parsed) ? parsed : Number.NaN;
  }
  return Number.NaN;
}

/**
 * Parses both long (string label columns + numeric column) and wide Grafana /api/ds/query frames,
 * filtering strictly to finite numbers and rejecting empty/whitespace strings.
 */
export function parseFramesToSeries(frames: GrafanaDataFrame[] | undefined): ParsedSeries[] {
  if (!Array.isArray(frames) || frames.length === 0) {
    return [];
  }
  const series: ParsedSeries[] = [];
  for (const frame of frames) {
    const fields = frame.schema?.fields ?? [];
    const columns = frame.data?.values ?? [];
    if (columns.length === 0) {
      continue;
    }

    let timeColIdx = fields.findIndex((f) => f.type === 'time');
    if (timeColIdx === -1 && columns.length >= 2) {
      timeColIdx = 0;
    }
    const rawTimes = timeColIdx >= 0 ? columns[timeColIdx] ?? [] : [];

    // Detect long-format frames: string label columns alongside numeric columns.
    const stringColIndices: number[] = [];
    for (let i = 0; i < columns.length; i++) {
      if (i !== timeColIdx && fields[i]?.type === 'string') {
        stringColIndices.push(i);
      }
    }

    for (let colIdx = 0; colIdx < columns.length; colIdx++) {
      if (colIdx === timeColIdx) {
        continue;
      }
      const fieldMeta = fields[colIdx];
      if (fieldMeta?.type && fieldMeta.type !== 'number') {
        continue;
      }
      const rawVals = columns[colIdx] ?? [];

      if (stringColIndices.length > 0 && (!fieldMeta?.labels || Object.keys(fieldMeta.labels).length === 0)) {
        // Long DataFrame: partition rows by tuple of string label columns.
        const grouped = new Map<string, { timestamps: number[]; values: number[] }>();
        for (let i = 0; i < rawVals.length; i++) {
          const num = parseFiniteNumericCell(rawVals[i]);
          if (!Number.isFinite(num)) {
            continue;
          }
          const labelPairs = stringColIndices
            .map((si) => {
              const colName = fields[si]?.name || `label_${si}`;
              const cell = columns[si]?.[i];
              return `${colName}=${cell === null || cell === undefined ? '' : String(cell)}`;
            })
            .sort();
          const fullKey = labelPairs.join(', ') || fieldMeta?.name || frame.name || `series_${colIdx}`;
          let bucket = grouped.get(fullKey);
          if (!bucket) {
            bucket = { timestamps: [], values: [] };
            grouped.set(fullKey, bucket);
          }
          const rawT = rawTimes[i];
          const ts = typeof rawT === 'number' && Number.isFinite(rawT) ? rawT : 0;
          bucket.timestamps.push(ts);
          bucket.values.push(num);
        }
        for (const [fullKey, bucket] of grouped.entries()) {
          if (bucket.values.length > 0) {
            series.push({
              fullLabelKey: fullKey,
              labelKey: fullKey.slice(0, 140),
              timestamps: bucket.timestamps,
              values: bucket.values,
            });
          }
        }
        continue;
      }

      const timestamps: number[] = [];
      const values: number[] = [];

      for (let i = 0; i < rawVals.length; i++) {
        const num = parseFiniteNumericCell(rawVals[i]);
        if (!Number.isFinite(num)) {
          continue;
        }
        const rawT = rawTimes[i];
        const ts = typeof rawT === 'number' && Number.isFinite(rawT) ? rawT : 0;
        timestamps.push(ts);
        values.push(num);
      }

      if (values.length > 0) {
        const fullLabelKey = fullCanonicalLabelString(
          fieldMeta?.labels,
          fieldMeta?.name || frame.name || `series_${colIdx}`
        );
        series.push({
          fullLabelKey,
          labelKey: fullLabelKey.slice(0, 140),
          timestamps,
          values,
        });
      }
    }
  }
  return series;
}

function percentile(sorted: number[], p: number): number {
  if (sorted.length === 0) {
    return 0;
  }
  const idx = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
  return sorted[idx];
}

function computeWindowStats(s: ParsedSeries, expectedSteps: number): MetricWindowStats {
  const n = s.values.length;
  let sum = 0;
  let minVal = s.values[0];
  let maxVal = s.values[0];
  let peakIdx = 0;

  for (let i = 0; i < n; i++) {
    const v = s.values[i];
    sum += v;
    if (v < minVal) {
      minVal = v;
    }
    if (v > maxVal) {
      maxVal = v;
      peakIdx = i;
    }
  }

  const sorted = [...s.values].sort((a, b) => a - b);
  const avg = sum / n;
  const p95 = percentile(sorted, 95);
  const latest = s.values[n - 1];
  const expected = Math.max(1, expectedSteps);
  const missingRatio = Math.max(0, Math.min(1, roundNum(1 - n / expected)));

  const peakTs = s.timestamps[peakIdx];
  const peakAt =
    Number.isFinite(peakTs) && peakTs > 0 && peakTs <= MAX_DATE_MS ? new Date(peakTs).toISOString() : undefined;

  return {
    latest: roundNum(latest),
    avg: roundNum(avg),
    p95: roundNum(p95),
    min: roundNum(minVal),
    max: roundNum(maxVal),
    sampleCount: n,
    missingRatio,
    peakAt,
  };
}

function classifyTrend(values: number[], avg: number, minVal: number, maxVal: number): MetricTrend {
  if (values.length === 0) {
    return 'no_data';
  }
  if (values.length < 4) {
    return 'stable';
  }
  const mid = Math.floor(values.length / 2);
  const firstHalf = values.slice(0, mid);
  const secondHalf = values.slice(mid);
  const mean = (arr: number[]) => arr.reduce((acc, v) => acc + v, 0) / arr.length;
  const firstAvg = mean(firstHalf);
  const secondAvg = mean(secondHalf);
  const denom = Math.max(Math.abs(firstAvg), EPSILON_BASELINE);
  const relChange = (secondAvg - firstAvg) / denom;

  if (Math.abs(avg) > EPSILON_BASELINE && maxVal / Math.abs(avg) >= 2.5) {
    return 'spiking';
  }
  if (relChange >= 0.2) {
    return 'rising';
  }
  if (relChange <= -0.2) {
    return 'falling';
  }
  const range = maxVal - minVal;
  if (Math.abs(avg) > EPSILON_BASELINE && range / Math.abs(avg) >= 1.0) {
    return 'volatile';
  }
  return 'stable';
}

function colorToBreach(color: string): ThresholdBreach {
  const lower = color.toLowerCase();
  if (lower.includes('red') || lower === '#f2495c' || lower === '#e02f44' || lower === '#c4162a') {
    return 'critical';
  }
  if (lower.includes('orange') || lower.includes('yellow') || lower === '#ff9830' || lower === '#fade2a') {
    return 'warning';
  }
  return 'none';
}

function isInvertedThreshold(thresholds?: RawPanelThresholds): boolean {
  if (!thresholds || thresholds.mode !== 'absolute' || thresholds.steps.length < 2) {
    return false;
  }
  const baseStep = thresholds.steps.find((s) => s.value === null);
  return baseStep ? colorToBreach(baseStep.color) !== 'none' : false;
}

function evaluateSingleThresholdValue(val: number, thresholds?: RawPanelThresholds): ThresholdBreach {
  if (!thresholds || thresholds.mode !== 'absolute' || thresholds.steps.length === 0) {
    return 'none';
  }
  const baseStep = thresholds.steps.find((s) => s.value === null);
  const numericSteps = thresholds.steps
    .filter((s): s is { value: number; color: string } => s.value !== null)
    .sort((a, b) => a.value - b.value);

  let matchedColor = baseStep?.color ?? '';
  for (const step of numericSteps) {
    if (val >= step.value) {
      matchedColor = step.color;
    }
  }
  return matchedColor ? colorToBreach(matchedColor) : 'none';
}

function evaluateThresholdBreach(stats: MetricWindowStats, thresholds?: RawPanelThresholds): ThresholdBreach {
  if (!thresholds || thresholds.mode !== 'absolute') {
    return 'none';
  }
  const rank: Record<ThresholdBreach, number> = { none: 0, warning: 1, critical: 2 };
  // Evaluate latest, min, and max so normal, inverted, and band (red -> green -> red) thresholds all work.
  const candidates = [stats.latest ?? stats.avg, stats.min, stats.max];
  let worst: ThresholdBreach = 'none';
  for (const v of candidates) {
    const b = evaluateSingleThresholdValue(v, thresholds);
    if (rank[b] > rank[worst]) {
      worst = b;
    }
  }
  return worst;
}

function expandTimeRangeMacros(expr: string, fromMs: number, toMs: number): string {
  return expr
    .replace(/\$\{__from\}|\$__from\b/g, String(fromMs))
    .replace(/\$\{__to\}|\$__to\b/g, String(toMs));
}

function extractDatasourceQueryError(err: unknown, refId: string): string {
  if (err && typeof err === 'object') {
    const rec = err as { data?: { results?: Record<string, { error?: unknown }> } };
    const inner = rec.data?.results?.[refId]?.error;
    if (typeof inner === 'string' && inner.trim() !== '') {
      return inner.trim().slice(0, 240);
    }
  }
  const extracted = extractApiError(err);
  return (extracted || 'Datasource query failed').slice(0, 240);
}

async function runSingleRangeQuery(
  query: ExtractedReportQuery,
  fromMs: number,
  toMs: number,
  intervalMs: number,
  timeoutMs: number,
  outerSignal?: AbortSignal
): Promise<{ status: 'ok' | 'empty' | 'error' | 'timeout'; series: ParsedSeries[]; errorText?: string }> {
  if (outerSignal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }

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

  const resolvedExpr = expandTimeRangeMacros(query.expr, fromMs, toMs);

  try {
    const resp = await new Promise<GrafanaDSQueryResponse>((resolve, reject) => {
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
          showSuccessAlert: false,
          abortSignal: controller.signal,
          data: {
            from: String(fromMs),
            to: String(toMs),
            queries: [
              {
                refId: query.refId,
                datasource: { uid: query.datasourceUid, type: query.datasourceType },
                expr: resolvedExpr,
                range: true,
                instant: false,
                intervalMs,
                maxDataPoints: 60,
              },
            ],
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
            if (externallyAborted || outerSignal?.aborted) {
              reject(new DOMException('Aborted', 'AbortError'));
            } else if (timedOut) {
              reject(new Error('timeout'));
            } else {
              reject(err);
            }
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

    const entry = resp?.results?.[query.refId];
    if (!entry) {
      return { status: 'empty', series: [] };
    }
    if (entry.error) {
      return { status: 'error', series: [], errorText: String(entry.error).slice(0, 240) };
    }
    const parsed = parseFramesToSeries(entry.frames);
    if (parsed.length === 0) {
      return { status: 'empty', series: [] };
    }
    return { status: 'ok', series: parsed };
  } catch (err) {
    if (externallyAborted || outerSignal?.aborted) {
      throw new DOMException('Aborted', 'AbortError');
    }
    if (
      timedOut ||
      (err instanceof DOMException && err.name === 'AbortError') ||
      (err instanceof Error && err.message === 'timeout') ||
      (typeof err === 'object' && err !== null && (err as { cancelled?: unknown }).cancelled === true)
    ) {
      return { status: 'timeout', series: [], errorText: `Query timed out (${Math.round(timeoutMs / 1000)}s)` };
    }
    return {
      status: 'error',
      series: [],
      errorText: extractDatasourceQueryError(err, query.refId),
    };
  } finally {
    clearTimeout(timer);
    outerSignal?.removeEventListener('abort', onOuterAbort);
  }
}

/**
 * Executes bounded browser-session range queries for extracted dashboard panels over the selected
 * current window and optional baseline window, computing deterministic summary statistics and
 * ranking anomalous series (including low-value outages like up == 0 and zero-baseline spikes).
 */
export async function collectDashboardMetricEvidence(
  panels: ExtractedReportPanel[],
  options: CollectMetricsOptions
): Promise<CollectMetricsResult> {
  if (panels.length === 0) {
    return { metrics: [], warnings: [], analyzedPanelCount: 0 };
  }

  const winCfg = WINDOW_CONFIG[options.window] ?? WINDOW_CONFIG['1h'];
  const maxConcurrency = Math.max(1, Math.min(options.maxConcurrency ?? 2, 4));
  const perQueryTimeoutMs = Math.max(2000, Math.min(options.perQueryTimeoutMs ?? 10_000, 20_000));
  const overallDeadlineMs = Math.max(5000, Math.min(options.overallDeadlineMs ?? 25_000, 60_000));
  const nowMs = options.nowMs ?? Date.now();
  const currentToMs = nowMs;
  const currentFromMs = currentToMs - winCfg.durationMs;
  const expectedSteps = Math.max(1, Math.floor(winCfg.durationMs / winCfg.intervalMs) + 1);

  const deadlineCtrl = new AbortController();
  const onExternalAbort = () => deadlineCtrl.abort(options.signal?.reason);
  if (options.signal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }
  options.signal?.addEventListener('abort', onExternalAbort, { once: true });
  let hitOverallDeadline = false;
  const deadlineTimer = setTimeout(() => {
    hitOverallDeadline = true;
    deadlineCtrl.abort(new Error('overall_deadline'));
  }, overallDeadlineMs);

  interface RankedMetric {
    anomalyScore: number;
    item: Omit<MetricEvidenceItem, 'id'>;
  }

  const rankedItems: RankedMetric[] = [];
  let cursor = 0;
  let baselineSkippedCount = 0;
  let analyzedPanelCount = 0;

  const processPanel = async (panel: ExtractedReportPanel) => {
    if (deadlineCtrl.signal.aborted) {
      return;
    }

    let panelQueried = false;
    for (const q of panel.queries) {
      if (deadlineCtrl.signal.aborted) {
        break;
      }
      let curRes;
      try {
        curRes = await runSingleRangeQuery(
          q,
          currentFromMs,
          currentToMs,
          winCfg.intervalMs,
          perQueryTimeoutMs,
          deadlineCtrl.signal
        );
      } catch (err) {
        if (options.signal?.aborted) {
          throw err;
        }
        break;
      }
      panelQueried = true;

      if (curRes.status === 'error' || curRes.status === 'timeout') {
        if (options.baselineMode !== 'none') {
          baselineSkippedCount++;
        }
        rankedItems.push({
          anomalyScore: 85,
          item: {
            panelTitle: panel.title,
            unit: panel.unit,
            queryStatus: curRes.status,
            errorText: curRes.errorText,
            trend: 'no_data',
            thresholdBreached: 'none',
            anomalyHint: 'query_error',
          },
        });
        continue;
      }

      if (curRes.status === 'empty') {
        if (options.baselineMode !== 'none') {
          baselineSkippedCount++;
        }
        rankedItems.push({
          anomalyScore: 45,
          item: {
            panelTitle: panel.title,
            unit: panel.unit,
            queryStatus: 'empty',
            trend: 'no_data',
            thresholdBreached: 'none',
            anomalyHint: 'missing_data',
          },
        });
        continue;
      }

      // Current query succeeded: optionally fetch baseline window for comparison.
      const baselineByFullLabel = new Map<string, MetricBaselineStats>();
      if (options.baselineMode !== 'none' && !deadlineCtrl.signal.aborted) {
        const baseFromMs =
          options.baselineMode === '24h_ago'
            ? currentFromMs - 24 * 60 * 60 * 1000
            : currentFromMs - winCfg.durationMs;
        const baseToMs =
          options.baselineMode === '24h_ago' ? currentToMs - 24 * 60 * 60 * 1000 : currentFromMs;
        try {
          const baseRes = await runSingleRangeQuery(
            q,
            baseFromMs,
            baseToMs,
            winCfg.intervalMs,
            perQueryTimeoutMs,
            deadlineCtrl.signal
          );
          if (baseRes.status === 'ok') {
            for (const bs of baseRes.series) {
              const bStats = computeWindowStats(bs, expectedSteps);
              baselineByFullLabel.set(bs.fullLabelKey, {
                avg: bStats.avg,
                p95: bStats.p95,
                max: bStats.max,
              });
            }
          } else if (baseRes.status === 'error' || baseRes.status === 'timeout') {
            baselineSkippedCount++;
          }
        } catch (err) {
          if (options.signal?.aborted) {
            throw err;
          }
          baselineSkippedCount++;
        }
      }

      const lowerIsWorse = LOWER_IS_WORSE_RE.test(panel.title) || isInvertedThreshold(panel.thresholds);
      const perPanelCandidates: RankedMetric[] = [];

      for (const s of curRes.series) {
        const current = computeWindowStats(s, expectedSteps);
        const baseline = baselineByFullLabel.get(s.fullLabelKey);
        const trend = classifyTrend(s.values, current.avg, current.min, current.max);
        const thresholdBreached = evaluateThresholdBreach(current, panel.thresholds);

        let deltaPctAvg: number | undefined;
        let deltaPctP95: number | undefined;
        let baselineWasZero = false;
        if (baseline) {
          if (Math.abs(baseline.avg) >= EPSILON_BASELINE) {
            deltaPctAvg = roundNum(((current.avg - baseline.avg) / Math.abs(baseline.avg)) * 100);
          } else {
            baselineWasZero = true;
          }
          if (Math.abs(baseline.p95) >= EPSILON_BASELINE) {
            deltaPctP95 = roundNum(((current.p95 - baseline.p95) / Math.abs(baseline.p95)) * 100);
          } else {
            baselineWasZero = true;
          }
        }

        let anomalyHint: AnomalyHint = 'normal';
        let anomalyScore = 10;

        if (thresholdBreached === 'critical') {
          anomalyHint = 'degraded';
          anomalyScore = 100;
        } else if (thresholdBreached === 'warning') {
          anomalyHint = 'elevated';
          anomalyScore = 80;
        } else if (lowerIsWorse && current.min === 0 && (baseline === undefined || baseline.avg > 0.5)) {
          anomalyHint = 'degraded';
          anomalyScore = 95;
        } else if (baselineWasZero && Math.abs(current.avg) >= EPSILON_BASELINE) {
          // Series went from 0 baseline to non-zero activity (e.g., 5xx errors emerging from 0/s).
          anomalyHint = 'degraded';
          anomalyScore = 88;
        } else if (deltaPctAvg !== undefined && Math.abs(deltaPctAvg) >= 50) {
          anomalyHint = Math.abs(deltaPctAvg) >= 100 ? 'degraded' : 'elevated';
          anomalyScore = Math.min(90, 50 + Math.abs(deltaPctAvg) / 5);
        } else if (current.missingRatio >= 0.3) {
          anomalyHint = 'missing_data';
          anomalyScore = 60;
        } else if (trend === 'spiking') {
          anomalyHint = 'elevated';
          anomalyScore = 55;
        } else if (lowerIsWorse) {
          anomalyScore = 20 - Math.min(15, current.min);
        } else {
          anomalyScore = 15 + Math.min(10, Math.abs(current.max) / 100);
        }

        perPanelCandidates.push({
          anomalyScore,
          item: {
            panelTitle: panel.title,
            unit: panel.unit,
            seriesLabel: s.labelKey,
            queryStatus: 'ok',
            current,
            baseline,
            deltaPctAvg,
            deltaPctP95,
            baselineWasZero: baselineWasZero || undefined,
            trend,
            thresholdBreached,
            anomalyHint,
          },
        });
      }

      // Keep top 3 most anomalous series per query so outages on low-value series (e.g. up == 0) are never hidden.
      perPanelCandidates.sort((a, b) => b.anomalyScore - a.anomalyScore);
      rankedItems.push(...perPanelCandidates.slice(0, 3));
    }

    if (panelQueried) {
      analyzedPanelCount++;
    }
  };

  try {
    const worker = async () => {
      while (cursor < panels.length) {
        if (deadlineCtrl.signal.aborted) {
          return;
        }
        const idx = cursor++;
        if (idx >= panels.length) {
          return;
        }
        await processPanel(panels[idx]);
      }
    };

    const workers = Array.from({ length: Math.min(maxConcurrency, panels.length) }, () => worker());
    await Promise.all(workers);
  } finally {
    clearTimeout(deadlineTimer);
    options.signal?.removeEventListener('abort', onExternalAbort);
  }

  rankedItems.sort((a, b) => b.anomalyScore - a.anomalyScore);
  const warnings: CollectionWarningItem[] = [];
  if (hitOverallDeadline) {
    warnings.push({
      id: `ev-warn-${warnings.length + 1}`,
      code: 'collection_timeout',
      message: `Dashboard metric collection reached the ${Math.round(overallDeadlineMs / 1000)}s deadline; partial results included.`,
    });
  }
  if (baselineSkippedCount > 0) {
    warnings.push({
      id: `ev-warn-${warnings.length + 1}`,
      code: 'baseline_skipped',
      message: `Baseline comparison was skipped for ${baselineSkippedCount} query/queries due to current-window failure or timeout.`,
    });
  }
  if (rankedItems.length > MAX_METRIC_EVIDENCE_ITEMS) {
    warnings.push({
      id: `ev-warn-${warnings.length + 1}`,
      code: 'evidence_truncated',
      message: `Truncated dashboard metric series from ${rankedItems.length} to top ${MAX_METRIC_EVIDENCE_ITEMS} anomalous items.`,
    });
  }

  const metrics: MetricEvidenceItem[] = rankedItems
    .slice(0, MAX_METRIC_EVIDENCE_ITEMS)
    .map((entry, idx) => ({
      id: `ev-metric-${idx + 1}`,
      ...entry.item,
    }));

  return {
    metrics,
    warnings,
    analyzedPanelCount,
  };
}
