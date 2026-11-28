import { getBackendSrv } from '@grafana/runtime';
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

function canonicalLabelString(labels: Record<string, string> | undefined, fallbackName: string): string {
  if (!labels || Object.keys(labels).length === 0) {
    return fallbackName || 'series';
  }
  const keys = Object.keys(labels).sort();
  return keys.map((k) => `${k}=${labels[k]}`).join(', ').slice(0, 140);
}

/**
 * Parses both long and wide Grafana /api/ds/query frames, filtering strictly to finite numbers.
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

    for (let colIdx = 0; colIdx < columns.length; colIdx++) {
      if (colIdx === timeColIdx) {
        continue;
      }
      const fieldMeta = fields[colIdx];
      if (fieldMeta?.type && fieldMeta.type !== 'number') {
        continue;
      }
      const rawVals = columns[colIdx] ?? [];
      const timestamps: number[] = [];
      const values: number[] = [];

      for (let i = 0; i < rawVals.length; i++) {
        const rawV = rawVals[i];
        const num = typeof rawV === 'number' ? rawV : typeof rawV === 'string' ? Number(rawV) : NaN;
        if (!Number.isFinite(num)) {
          continue;
        }
        const rawT = rawTimes[i];
        const ts = typeof rawT === 'number' && Number.isFinite(rawT) ? rawT : 0;
        timestamps.push(ts);
        values.push(num);
      }

      if (values.length > 0) {
        const labelKey = canonicalLabelString(fieldMeta?.labels, fieldMeta?.name || frame.name || `series_${colIdx}`);
        series.push({ labelKey, timestamps, values });
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
  const peakAt = peakTs > 0 ? new Date(peakTs).toISOString() : undefined;

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

function classifyTrend(values: number[], avg: number, max: number): MetricTrend {
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

  if (Math.abs(avg) > EPSILON_BASELINE && max / Math.abs(avg) >= 2.5) {
    return 'spiking';
  }
  if (relChange >= 0.2) {
    return 'rising';
  }
  if (relChange <= -0.2) {
    return 'falling';
  }
  const range = Math.max(...values) - Math.min(...values);
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
  const inverted = isInvertedThreshold(thresholds);
  const candidates = [stats.latest ?? stats.avg, inverted ? stats.min : stats.max];
  let worst: ThresholdBreach = 'none';
  for (const v of candidates) {
    const b = evaluateSingleThresholdValue(v, thresholds);
    if (rank[b] > rank[worst]) {
      worst = b;
    }
  }
  return worst;
}

async function runSingleRangeQuery(
  query: ExtractedReportQuery,
  fromMs: number,
  toMs: number,
  intervalMs: number,
  timeoutMs: number,
  signal?: AbortSignal
): Promise<{ status: 'ok' | 'empty' | 'error' | 'timeout'; series: ParsedSeries[]; errorText?: string }> {
  if (signal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }
  const controller = new AbortController();
  const onAbort = () => controller.abort();
  signal?.addEventListener('abort', onAbort, { once: true });
  const timer = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const resp = await getBackendSrv().post<GrafanaDSQueryResponse>(
      '/api/ds/query',
      {
        from: String(fromMs),
        to: String(toMs),
        queries: [
          {
            refId: query.refId,
            expr: query.expr,
            datasource: { uid: query.datasourceUid, type: query.datasourceType },
            range: true,
            instant: false,
            intervalMs,
            maxDataPoints: 100,
          },
        ],
      },
      {
        showErrorAlert: false,
        showSuccessAlert: false,
        signal: controller.signal,
      } as Record<string, unknown>
    );

    const refRes = resp?.results?.[query.refId];
    if (refRes?.error) {
      return { status: 'error', series: [], errorText: refRes.error.slice(0, 240) };
    }
    const parsed = parseFramesToSeries(refRes?.frames);
    if (parsed.length === 0) {
      return { status: 'empty', series: [] };
    }
    return { status: 'ok', series: parsed };
  } catch (err) {
    if (signal?.aborted) {
      throw new DOMException('Aborted', 'AbortError');
    }
    if (
      (err instanceof DOMException && err.name === 'AbortError') ||
      (err instanceof Error && err.name === 'AbortError')
    ) {
      return { status: 'timeout', series: [], errorText: `Query timed out after ${timeoutMs}ms` };
    }
    const msg = err instanceof Error ? err.message : 'Datasource query failed';
    return { status: 'error', series: [], errorText: msg.slice(0, 240) };
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener('abort', onAbort);
  }
}

/**
 * Executes bounded current + optional baseline queries for extracted dashboard panels
 * under the logged-in user's Grafana session with an overall 25s deadline and
 * skips baseline queries whenever the current window errors or times out.
 */
export async function collectDashboardMetricEvidence(
  panels: ExtractedReportPanel[],
  options: CollectMetricsOptions
): Promise<CollectMetricsResult> {
  if (panels.length === 0) {
    return { metrics: [], warnings: [], analyzedPanelCount: 0 };
  }
  const winCfg = WINDOW_CONFIG[options.window];
  const nowMs = options.nowMs ?? Date.now();
  const currentFromMs = nowMs - winCfg.durationMs;
  const currentToMs = nowMs;
  const expectedSteps = Math.floor(winCfg.durationMs / winCfg.intervalMs) + 1;

  const maxConcurrency = Math.max(1, Math.min(options.maxConcurrency ?? 2, 4));
  const perQueryTimeoutMs = Math.max(2000, Math.min(options.perQueryTimeoutMs ?? 10_000, 30_000));
  const overallDeadlineMs = Math.max(5000, Math.min(options.overallDeadlineMs ?? 25_000, 60_000));

  const deadlineCtrl = new AbortController();
  const onExternalAbort = () => deadlineCtrl.abort();
  options.signal?.addEventListener('abort', onExternalAbort, { once: true });
  let hitOverallDeadline = false;
  const deadlineTimer = setTimeout(() => {
    hitOverallDeadline = true;
    deadlineCtrl.abort();
  }, overallDeadlineMs);

  const warnings: CollectionWarningItem[] = [];
  const pushWarning = (code: CollectionWarningItem['code'], message: string) => {
    warnings.push({
      id: `ev-warn-${warnings.length + 1}`,
      code,
      message,
    });
  };

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
    analyzedPanelCount++;

    for (const q of panel.queries) {
      if (deadlineCtrl.signal.aborted) {
        return;
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
        return;
      }

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

      const baselineByLabel = new Map<string, MetricBaselineStats>();
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
              baselineByLabel.set(bs.labelKey, {
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
        const baseline = baselineByLabel.get(s.labelKey);
        const trend = classifyTrend(s.values, current.avg, current.max);
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
  };

  try {
    const worker = async () => {
      while (cursor < panels.length) {
        if (deadlineCtrl.signal.aborted) {
          return;
        }
        const idx = cursor++;
        await processPanel(panels[idx]);
      }
    };

    const workers = Array.from({ length: Math.min(maxConcurrency, panels.length) }, () => worker());
    await Promise.all(workers);
  } finally {
    clearTimeout(deadlineTimer);
    options.signal?.removeEventListener('abort', onExternalAbort);
  }

  if (options.signal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }

  if (hitOverallDeadline) {
    pushWarning(
      'collection_timeout',
      `Dashboard metric collection reached the ${Math.round(overallDeadlineMs / 1000)}s deadline; partial panel summaries were collected.`
    );
  }
  if (baselineSkippedCount > 0) {
    pushWarning(
      'baseline_skipped',
      `Skipped ${baselineSkippedCount} baseline comparison query(s) after current-window errors or timeouts.`
    );
  }

  rankedItems.sort((a, b) => b.anomalyScore - a.anomalyScore);
  if (rankedItems.length > MAX_METRIC_EVIDENCE_ITEMS) {
    pushWarning(
      'evidence_truncated',
      `Trimmed dashboard series summaries from ${rankedItems.length} to top ${MAX_METRIC_EVIDENCE_ITEMS} most anomalous series.`
    );
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
