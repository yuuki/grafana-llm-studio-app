import { getBackendSrv } from '@grafana/runtime';
import {
  AlertEvidenceItem,
  AlertEvidenceState,
  CollectionWarningItem,
  MAX_ALERT_EVIDENCE_ITEMS,
  MAX_RULE_EVIDENCE_ITEMS,
  ReportCollectionStatus,
  RuleEvidenceHealth,
  RuleEvidenceItem,
  RuleEvidenceState,
  StatusNoteEvidenceItem,
} from '../ir/reportTypes';
import type { ScopeFilter } from '../ir/types';

export interface CollectAlertsOptions {
  scope?: ScopeFilter;
  includeOrgCriticalAlerts?: boolean;
  timeoutMs?: number;
  signal?: AbortSignal;
}

export interface CollectAlertsResult {
  alerts: AlertEvidenceItem[];
  rules: RuleEvidenceItem[];
  statusNotes: StatusNoteEvidenceItem[];
  warnings: CollectionWarningItem[];
  collectionStatus: ReportCollectionStatus;
}

interface RawAlertmanagerAlert {
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  startsAt?: string;
  status?: {
    state?: string;
  };
}

interface RawPrometheusRuleAlert {
  labels?: Record<string, string>;
  state?: string;
}

interface RawPrometheusRule {
  name?: string;
  type?: string;
  state?: string;
  health?: string;
  lastError?: string;
  labels?: Record<string, string>;
  alerts?: RawPrometheusRuleAlert[];
}

interface RawPrometheusRulesResponse {
  data?: {
    groups?: Array<{
      name?: string;
      rules?: RawPrometheusRule[];
    }>;
  };
}

const PRIORITY_LABEL_KEYS = [
  'alertname',
  'rulename',
  'severity',
  'priority',
  'service',
  'job',
  'namespace',
  'instance',
  'pod',
  'grafana_folder',
];

const LABEL_KEY_RE = /^[a-zA-Z_][a-zA-Z0-9_]{0,63}$/;
const MAX_RULES_RESPONSE_ITEMS = 2000;

function trimRunes(val: string | undefined, maxRunes: number): string {
  if (!val) {
    return '';
  }
  const clean = val.replace(/[\r\n\t]+/g, ' ').trim();
  const chars = Array.from(clean);
  if (chars.length <= maxRunes) {
    return clean;
  }
  return chars.slice(0, maxRunes).join('');
}

function normalizeAlertState(raw?: string): AlertEvidenceState {
  const lower = (raw ?? '').toLowerCase();
  if (lower === 'suppressed' || lower === 'silenced' || lower === 'inhibited') {
    return 'suppressed';
  }
  if (lower === 'unprocessed') {
    return 'unprocessed';
  }
  return 'active';
}

function normalizeRuleState(raw?: string): RuleEvidenceState {
  const lower = (raw ?? '').toLowerCase();
  if (lower === 'firing' || lower === 'alerting') {
    return 'firing';
  }
  if (lower === 'recovering') {
    return 'recovering';
  }
  if (lower === 'pending') {
    return 'pending';
  }
  return 'inactive';
}

function normalizeRuleHealth(raw?: string): RuleEvidenceHealth {
  const lower = (raw ?? '').toLowerCase();
  if (lower === 'error' || lower === 'err') {
    return 'error';
  }
  if (lower === 'nodata') {
    return 'nodata';
  }
  if (lower === 'ok') {
    return 'ok';
  }
  return 'unknown';
}

function isCriticalSeverity(sev?: string): boolean {
  const lower = (sev ?? '').toLowerCase();
  return lower === 'critical' || lower === 'page' || lower === 'error' || lower === 'p1' || lower === 'sev1';
}

function matchesScopeLabels(labels: Record<string, string> | undefined, scope?: ScopeFilter): boolean {
  if (!scope || !scope.labelName || !scope.labelValue || !labels) {
    return false;
  }
  return labels[scope.labelName] === scope.labelValue;
}

function selectPriorityLabels(raw?: Record<string, string>): Record<string, string> | undefined {
  if (!raw) {
    return undefined;
  }
  const out: Record<string, string> = {};
  const addKey = (k: string) => {
    if (Object.keys(out).length >= 10) {
      return;
    }
    if (!LABEL_KEY_RE.test(k) || k.startsWith('__')) {
      return;
    }
    const v = raw[k];
    if (typeof v === 'string' && v.trim() !== '') {
      out[k] = trimRunes(v, 128);
    }
  };
  for (const key of PRIORITY_LABEL_KEYS) {
    if (key in raw) {
      addKey(key);
    }
  }
  for (const key of Object.keys(raw).sort()) {
    if (!(key in out)) {
      addKey(key);
    }
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

function extractHttpStatus(err: unknown): number {
  if (!err || typeof err !== 'object') {
    return 0;
  }
  const rec = err as Record<string, unknown>;
  if (typeof rec.status === 'number') {
    return rec.status;
  }
  if (typeof rec.statusCode === 'number') {
    return rec.statusCode;
  }
  return 0;
}

function isAbortError(err: unknown): boolean {
  if (!err) {
    return false;
  }
  if (err instanceof DOMException && err.name === 'AbortError') {
    return true;
  }
  if (typeof err === 'object' && (err as { cancelled?: unknown }).cancelled === true) {
    return true;
  }
  if (err instanceof Error && (err.name === 'AbortError' || err.message.toLowerCase().includes('abort'))) {
    return true;
  }
  return false;
}

/**
 * Fetches JSON via Grafana's BackendSrv.fetch Observable with proper abortSignal + unsubscribe()
 * so timeouts, Cancel clicks, org switches, and component unmount abort in-flight HTTP requests.
 */
async function fetchWithTimeout<T>(url: string, timeoutMs: number, externalSignal?: AbortSignal): Promise<T> {
  if (externalSignal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }
  let timedOut = false;
  let externallyAborted = false;
  const controller = new AbortController();
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort(new Error('timeout'));
  }, timeoutMs);
  const onExternalAbort = () => {
    externallyAborted = true;
    controller.abort(externalSignal?.reason);
  };
  externalSignal?.addEventListener('abort', onExternalAbort, { once: true });

  try {
    return await new Promise<T>((resolve, reject) => {
      if (externalSignal?.aborted || externallyAborted) {
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
        if (externallyAborted || externalSignal?.aborted) {
          reject(new DOMException('Aborted', 'AbortError'));
        } else {
          reject(new Error('timeout'));
        }
      };
      controller.signal.addEventListener('abort', onAbort, { once: true });

      sub = getBackendSrv()
        .fetch<T>({
          url,
          method: 'GET',
          showErrorAlert: false,
          showSuccessAlert: false,
          abortSignal: controller.signal,
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
            if (externallyAborted || externalSignal?.aborted) {
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
              reject(new Error('Empty response from Grafana Alerting API'));
            }
          },
        });
    });
  } finally {
    clearTimeout(timer);
    externalSignal?.removeEventListener('abort', onExternalAbort);
  }
}

/**
 * Collects bounded Grafana Unified Alerting evidence under the logged-in user's Grafana session:
 * - GET /api/alertmanager/grafana/api/v2/alerts
 * - GET /api/prometheus/grafana/api/v1/rules?limit_alerts=20
 * Handles 403/404 graceful degradation, deduplication via __alert_rule_uid__, and healthy-cluster summaries.
 */
interface RankedAlert {
  priority: number;
  startsAtMs: number;
  rawRuleUid: string;
  rawAlertName: string;
  rawRuleName: string;
  item: Omit<AlertEvidenceItem, 'id'>;
}

type PushWarningFn = (code: CollectionWarningItem['code'], message: string) => void;

export async function collectGrafanaAlertingEvidence(
  options: CollectAlertsOptions = {}
): Promise<CollectAlertsResult> {
  const timeoutMs = Math.max(2000, Math.min(options.timeoutMs ?? 8000, 20000));
  const includeOrgCritical = options.includeOrgCriticalAlerts ?? true;
  const warnings: CollectionWarningItem[] = [];
  const pushWarning: PushWarningFn = (code, message) => {
    if (warnings.length >= 12) {
      return;
    }
    warnings.push({
      id: `ev-warn-${warnings.length + 1}`,
      code,
      message: trimRunes(message, 200),
    });
  };

  const [alertsRes, rulesRes] = await Promise.allSettled([
    fetchWithTimeout<RawAlertmanagerAlert[]>(
      '/api/alertmanager/grafana/api/v2/alerts?active=true&silenced=true&inhibited=true&unprocessed=true',
      timeoutMs,
      options.signal
    ),
    fetchWithTimeout<RawPrometheusRulesResponse>(
      '/api/prometheus/grafana/api/v1/rules?limit_alerts=20',
      timeoutMs,
      options.signal
    ),
  ]);

  if (options.signal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }

  const { alertsChecked, rawAlerts } = resolveSettledAlerts(alertsRes, options.signal, pushWarning);
  const { rulesChecked, rawRulesResp } = resolveSettledRules(rulesRes, options.signal, pushWarning);

  const { alerts, activeAlertsTotal, coveredRuleUIDs, coveredRuleNames } = buildAlertEvidence(
    rawAlerts,
    options.scope,
    includeOrgCritical,
    pushWarning
  );

  const { rules, totalRulesEvaluated, abnormalRulesTotal } = buildRuleEvidence(
    rawRulesResp?.data?.groups ?? [],
    coveredRuleUIDs,
    coveredRuleNames,
    options.scope,
    includeOrgCritical,
    pushWarning
  );

  const statusNotes: StatusNoteEvidenceItem[] = [];
  if ((alertsChecked || rulesChecked) && alerts.length === 0 && rules.length === 0) {
    statusNotes.push({
      id: 'ev-status-1',
      message: `Grafana Alerting checked: ${activeAlertsTotal} active alerts across ${totalRulesEvaluated} evaluated alerting rules (no abnormal alert states matched).`,
    });
  }

  return {
    alerts,
    rules,
    statusNotes,
    warnings,
    collectionStatus: {
      alertsChecked,
      rulesChecked,
      totalRulesEvaluated,
      activeAlertsTotal,
      abnormalRulesTotal,
    },
  };
}

function resolveSettledAlerts(
  alertsRes: PromiseSettledResult<RawAlertmanagerAlert[]>,
  signal: AbortSignal | undefined,
  pushWarning: PushWarningFn
): { alertsChecked: boolean; rawAlerts: RawAlertmanagerAlert[] } {
  if (alertsRes.status === 'fulfilled' && Array.isArray(alertsRes.value)) {
    return { alertsChecked: true, rawAlerts: alertsRes.value };
  }
  if (alertsRes.status === 'rejected') {
    if (isAbortError(alertsRes.reason) && signal?.aborted) {
      throw alertsRes.reason;
    }
    const status = extractHttpStatus(alertsRes.reason);
    if (status === 403) {
      pushWarning('alerting_forbidden', 'Grafana Alertmanager alerts endpoint returned HTTP 403 Forbidden.');
    } else {
      pushWarning('alerting_unavailable', 'Grafana Alertmanager alerts endpoint was unavailable or timed out.');
    }
  }
  return { alertsChecked: false, rawAlerts: [] };
}

function resolveSettledRules(
  rulesRes: PromiseSettledResult<RawPrometheusRulesResponse>,
  signal: AbortSignal | undefined,
  pushWarning: PushWarningFn
): { rulesChecked: boolean; rawRulesResp: RawPrometheusRulesResponse | null } {
  if (rulesRes.status === 'fulfilled' && rulesRes.value && typeof rulesRes.value === 'object') {
    return { rulesChecked: true, rawRulesResp: rulesRes.value };
  }
  if (rulesRes.status === 'rejected') {
    if (isAbortError(rulesRes.reason) && signal?.aborted) {
      throw rulesRes.reason;
    }
    const status = extractHttpStatus(rulesRes.reason);
    if (status === 403) {
      pushWarning('alerting_forbidden', 'Grafana Alerting rules endpoint returned HTTP 403 Forbidden.');
    } else {
      pushWarning('alerting_unavailable', 'Grafana Alerting rules endpoint was unavailable or timed out.');
    }
  }
  return { rulesChecked: false, rawRulesResp: null };
}

function computeAlertPriority(
  state: AlertEvidenceItem['state'],
  matchesScope: boolean,
  severity: string
): number {
  const isCritical = isCriticalSeverity(severity);
  if (state === 'active' && matchesScope && isCritical) {
    return 1;
  }
  if (state === 'active' && isCritical) {
    return 10;
  }
  if (state === 'active' && matchesScope) {
    return 15;
  }
  if (state === 'active') {
    return 20;
  }
  return state === 'suppressed' ? 30 : 40;
}

function resolveAlertDisplayName(labels: Record<string, string>): string {
  const rawName = labels.alertname || '';
  if (rawName === 'DatasourceNoData' || rawName === 'DatasourceError') {
    return labels.rulename || rawName || 'UnnamedAlert';
  }
  return rawName || labels.rulename || 'UnnamedAlert';
}

function parseAlertStartsAt(rawStartsAt: unknown): { startsAtMs: number; startsAt?: string } {
  const parsed = typeof rawStartsAt === 'string' ? Date.parse(rawStartsAt) : Number.NaN;
  if (!Number.isFinite(parsed)) {
    return { startsAtMs: 0 };
  }
  return {
    startsAtMs: parsed,
    startsAt: Math.abs(parsed) <= 8.64e15 ? new Date(parsed).toISOString() : undefined,
  };
}

function toRankedAlert(
  raw: RawAlertmanagerAlert,
  state: AlertEvidenceItem['state'],
  scope: ScopeFilter | undefined,
  includeOrgCritical: boolean
): RankedAlert | null {
  const labels = raw.labels ?? {};
  const annotations = raw.annotations ?? {};
  const severity = labels.severity || labels.priority || '';
  const matchesScope = matchesScopeLabels(labels, scope);

  if (scope?.labelName && scope?.labelValue && !matchesScope) {
    if (!(includeOrgCritical && state === 'active' && isCriticalSeverity(severity))) {
      return null;
    }
  }

  const { startsAtMs, startsAt } = parseAlertStartsAt(raw.startsAt);
  return {
    priority: computeAlertPriority(state, matchesScope, severity),
    startsAtMs,
    rawRuleUid: labels.__alert_rule_uid__ ?? '',
    rawAlertName: labels.alertname ?? '',
    rawRuleName: labels.rulename ?? '',
    item: {
      alertName: trimRunes(resolveAlertDisplayName(labels), 128),
      state,
      severity: trimRunes(severity, 32) || undefined,
      startsAt,
      labels: selectPriorityLabels(labels),
      summary: trimRunes(annotations.summary || annotations.message, 200) || undefined,
      description: trimRunes(annotations.description, 300) || undefined,
      matchesScope: matchesScope || undefined,
    },
  };
}

function buildAlertEvidence(
  rawAlerts: RawAlertmanagerAlert[],
  scope: ScopeFilter | undefined,
  includeOrgCritical: boolean,
  pushWarning: PushWarningFn
): {
  alerts: AlertEvidenceItem[];
  activeAlertsTotal: number;
  coveredRuleUIDs: Set<string>;
  coveredRuleNames: Set<string>;
} {
  let activeAlertsTotal = 0;
  const rankedAlerts: RankedAlert[] = [];
  for (const raw of rawAlerts) {
    const state = normalizeAlertState(raw.status?.state);
    if (state === 'active') {
      activeAlertsTotal++;
    }
    const ranked = toRankedAlert(raw, state, scope, includeOrgCritical);
    if (ranked) {
      rankedAlerts.push(ranked);
    }
  }

  rankedAlerts.sort((a, b) =>
    a.priority !== b.priority ? a.priority - b.priority : b.startsAtMs - a.startsAtMs
  );

  if (rankedAlerts.length > MAX_ALERT_EVIDENCE_ITEMS) {
    pushWarning(
      'evidence_truncated',
      `Truncated Grafana alerts from ${rankedAlerts.length} to top ${MAX_ALERT_EVIDENCE_ITEMS} prioritized items.`
    );
  }

  const keptAlerts = rankedAlerts.slice(0, MAX_ALERT_EVIDENCE_ITEMS);
  const coveredRuleUIDs = new Set<string>();
  const coveredRuleNames = new Set<string>();
  for (const kept of keptAlerts) {
    // Only deduplicate firing rules that are actually covered by a KEPT active alert.
    if (kept.item.state !== 'active') {
      continue;
    }
    if (kept.rawRuleUid) {
      coveredRuleUIDs.add(kept.rawRuleUid);
    }
    if (kept.rawRuleName) {
      coveredRuleNames.add(kept.rawRuleName);
    }
    if (kept.rawAlertName) {
      coveredRuleNames.add(kept.rawAlertName);
    }
  }

  const alerts: AlertEvidenceItem[] = keptAlerts.map((r, idx) => ({
    id: `ev-alert-${idx + 1}`,
    ...r.item,
  }));
  return { alerts, activeAlertsTotal, coveredRuleUIDs, coveredRuleNames };
}

function isRuleCoveredByAlertmanager(
  rule: RawPrometheusRule,
  ruleName: string,
  coveredRuleUIDs: Set<string>,
  coveredRuleNames: Set<string>
): boolean {
  const ruleUID = rule.labels?.__alert_rule_uid__ ?? '';
  const instanceCoveredByUID = (rule.alerts ?? []).some((inst) => {
    const instUID = inst.labels?.__alert_rule_uid__ ?? '';
    return instUID !== '' && coveredRuleUIDs.has(instUID);
  });
  const hasAnyUID =
    ruleUID !== '' || (rule.alerts ?? []).some((inst) => Boolean(inst.labels?.__alert_rule_uid__));
  return (
    (ruleUID !== '' && coveredRuleUIDs.has(ruleUID)) ||
    instanceCoveredByUID ||
    (!hasAnyUID && coveredRuleNames.has(ruleName))
  );
}

function computeRulePriority(
  state: RuleEvidenceItem['state'],
  health: RuleEvidenceItem['health']
): number {
  if (health === 'error') {
    return 5;
  }
  if (state === 'firing') {
    return 10;
  }
  if (health === 'nodata') {
    return 15;
  }
  if (state === 'pending' || state === 'recovering') {
    return 20;
  }
  return 30;
}

function buildRuleEvidence(
  groups: NonNullable<NonNullable<RawPrometheusRulesResponse['data']>['groups']>,
  coveredRuleUIDs: Set<string>,
  coveredRuleNames: Set<string>,
  scope: ScopeFilter | undefined,
  includeOrgCritical: boolean,
  pushWarning: PushWarningFn
): {
  rules: RuleEvidenceItem[];
  totalRulesEvaluated: number;
  abnormalRulesTotal: number;
} {
  let totalRulesEvaluated = 0;
  let abnormalRulesTotal = 0;
  let rulesResponseTruncated = false;
  const candidateRules: Array<{ priority: number; item: Omit<RuleEvidenceItem, 'id'> }> = [];

  let inspected = 0;
  outerRulesLoop: for (const grp of groups) {
    for (const rule of grp.rules ?? []) {
      if (inspected >= MAX_RULES_RESPONSE_ITEMS) {
        rulesResponseTruncated = true;
        break outerRulesLoop;
      }
      inspected++;
      if ((rule.type ?? '').toLowerCase() === 'recording') {
        continue;
      }
      totalRulesEvaluated++;

      const state = normalizeRuleState(rule.state);
      const health = normalizeRuleHealth(rule.health);
      const isAbnormal =
        state === 'firing' ||
        state === 'recovering' ||
        state === 'pending' ||
        health === 'error' ||
        health === 'nodata' ||
        health === 'unknown';
      if (!isAbnormal) {
        continue;
      }
      abnormalRulesTotal++;

      const candidate = evaluateAbnormalRule(
        grp.name,
        rule,
        state,
        health,
        coveredRuleUIDs,
        coveredRuleNames,
        scope,
        includeOrgCritical
      );
      if (candidate) {
        candidateRules.push(candidate);
      }
    }
  }

  if (rulesResponseTruncated) {
    pushWarning(
      'evidence_truncated',
      `Inspected the first ${MAX_RULES_RESPONSE_ITEMS} Grafana alerting rules; remaining rules were omitted.`
    );
  }

  candidateRules.sort((a, b) => a.priority - b.priority);
  if (candidateRules.length > MAX_RULE_EVIDENCE_ITEMS) {
    pushWarning(
      'evidence_truncated',
      `Truncated abnormal alerting rules from ${candidateRules.length} to ${MAX_RULE_EVIDENCE_ITEMS}.`
    );
  }

  const rules: RuleEvidenceItem[] = candidateRules
    .slice(0, MAX_RULE_EVIDENCE_ITEMS)
    .map((r, idx) => ({
      id: `ev-rule-${idx + 1}`,
      ...r.item,
    }));
  return { rules, totalRulesEvaluated, abnormalRulesTotal };
}

function evaluateAbnormalRule(
  groupName: string | undefined,
  rule: RawPrometheusRule,
  state: RuleEvidenceItem['state'],
  health: RuleEvidenceItem['health'],
  coveredRuleUIDs: Set<string>,
  coveredRuleNames: Set<string>,
  scope: ScopeFilter | undefined,
  includeOrgCritical: boolean
): { priority: number; item: Omit<RuleEvidenceItem, 'id'> } | null {
  const ruleName = trimRunes(rule.name, 128) || 'UnnamedRule';
  // Avoid double-counting healthy firing rules already covered by kept active /v2/alerts,
  // while preserving error/nodata/pending/recovering rules and external-Alertmanager firing rules.
  if (
    state === 'firing' &&
    health === 'ok' &&
    isRuleCoveredByAlertmanager(rule, ruleName, coveredRuleUIDs, coveredRuleNames)
  ) {
    return null;
  }

  const ruleMatchesScope =
    matchesScopeLabels(rule.labels, scope) ||
    (rule.alerts ?? []).some((inst) => matchesScopeLabels(inst.labels, scope));

  if (scope?.labelName && scope?.labelValue && !ruleMatchesScope) {
    if (!(includeOrgCritical && (health === 'error' || state === 'firing'))) {
      return null;
    }
  }

  return {
    priority: computeRulePriority(state, health),
    item: {
      ruleName,
      group: trimRunes(groupName, 100) || undefined,
      state,
      health,
      lastError: trimRunes(rule.lastError, 240) || undefined,
      activeAlertsCount: Array.isArray(rule.alerts) ? rule.alerts.length : 0,
      matchesScope: ruleMatchesScope || undefined,
    },
  };
}

