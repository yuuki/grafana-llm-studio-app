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
  if (err instanceof Error && (err.name === 'AbortError' || err.message.toLowerCase().includes('abort'))) {
    return true;
  }
  return false;
}

async function fetchWithTimeout<T>(url: string, timeoutMs: number, externalSignal?: AbortSignal): Promise<T> {
  if (externalSignal?.aborted) {
    throw new DOMException('Aborted', 'AbortError');
  }
  const controller = new AbortController();
  const onExternalAbort = () => controller.abort();
  externalSignal?.addEventListener('abort', onExternalAbort, { once: true });
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    return await getBackendSrv().get<T>(url, undefined, undefined, {
      showErrorAlert: false,
      showSuccessAlert: false,
      signal: controller.signal,
    } as Record<string, unknown>);
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
export async function collectGrafanaAlertingEvidence(
  options: CollectAlertsOptions = {}
): Promise<CollectAlertsResult> {
  const timeoutMs = Math.max(2000, Math.min(options.timeoutMs ?? 8000, 20000));
  const includeOrgCritical = options.includeOrgCriticalAlerts ?? true;
  const warnings: CollectionWarningItem[] = [];
  const pushWarning = (code: CollectionWarningItem['code'], message: string) => {
    warnings.push({
      id: `ev-warn-${warnings.length + 1}`,
      code,
      message: trimRunes(message, 200),
    });
  };

  let alertsChecked = false;
  let rulesChecked = false;
  let rawAlerts: RawAlertmanagerAlert[] = [];
  let rawRulesResp: RawPrometheusRulesResponse | null = null;

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

  if (alertsRes.status === 'fulfilled' && Array.isArray(alertsRes.value)) {
    alertsChecked = true;
    rawAlerts = alertsRes.value;
  } else if (alertsRes.status === 'rejected') {
    if (isAbortError(alertsRes.reason) && options.signal?.aborted) {
      throw alertsRes.reason;
    }
    const status = extractHttpStatus(alertsRes.reason);
    if (status === 403) {
      pushWarning('alerting_forbidden', 'Grafana Alertmanager alerts endpoint returned HTTP 403 Forbidden.');
    } else {
      pushWarning('alerting_unavailable', 'Grafana Alertmanager alerts endpoint was unavailable or timed out.');
    }
  }

  if (rulesRes.status === 'fulfilled' && rulesRes.value && typeof rulesRes.value === 'object') {
    rulesChecked = true;
    rawRulesResp = rulesRes.value;
  } else if (rulesRes.status === 'rejected') {
    if (isAbortError(rulesRes.reason) && options.signal?.aborted) {
      throw rulesRes.reason;
    }
    const status = extractHttpStatus(rulesRes.reason);
    if (status === 403) {
      pushWarning('alerting_forbidden', 'Grafana Alerting rules endpoint returned HTTP 403 Forbidden.');
    } else {
      pushWarning('alerting_unavailable', 'Grafana Alerting rules endpoint was unavailable or timed out.');
    }
  }

  const coveredRuleUIDs = new Set<string>();
  const coveredRuleNames = new Set<string>();
  let activeAlertsTotal = 0;

  interface RankedAlert {
    priority: number;
    startsAtMs: number;
    item: Omit<AlertEvidenceItem, 'id'>;
  }

  const rankedAlerts: RankedAlert[] = [];
  for (const raw of rawAlerts) {
    const labels = raw.labels ?? {};
    const annotations = raw.annotations ?? {};
    const state = normalizeAlertState(raw.status?.state);
    if (state === 'active') {
      activeAlertsTotal++;
    }
    const rawName = labels.alertname || '';
    const alertName =
      rawName === 'DatasourceNoData' || rawName === 'DatasourceError'
        ? labels.rulename || rawName || 'UnnamedAlert'
        : rawName || labels.rulename || 'UnnamedAlert';
    const severity = labels.severity || labels.priority || '';
    const matchesScope = matchesScopeLabels(labels, options.scope);

    if (options.scope?.labelName && options.scope?.labelValue && !matchesScope) {
      if (!(includeOrgCritical && state === 'active' && isCriticalSeverity(severity))) {
        continue;
      }
    }

    if (labels.__alert_rule_uid__) {
      coveredRuleUIDs.add(labels.__alert_rule_uid__);
    }
    if (labels.rulename) {
      coveredRuleNames.add(labels.rulename);
    }
    if (labels.alertname) {
      coveredRuleNames.add(labels.alertname);
    }

    let priority = 40;
    if (state === 'active' && matchesScope && isCriticalSeverity(severity)) {
      priority = 1;
    } else if (state === 'active' && isCriticalSeverity(severity)) {
      priority = 10;
    } else if (state === 'active' && matchesScope) {
      priority = 15;
    } else if (state === 'active') {
      priority = 20;
    } else if (state === 'suppressed') {
      priority = 30;
    }

    const startsAt = typeof raw.startsAt === 'string' && !Number.isNaN(Date.parse(raw.startsAt)) ? raw.startsAt : undefined;
    const startsAtMs = startsAt ? Date.parse(startsAt) : 0;

    rankedAlerts.push({
      priority,
      startsAtMs,
      item: {
        alertName: trimRunes(alertName, 128),
        state,
        severity: trimRunes(severity, 32) || undefined,
        startsAt,
        labels: selectPriorityLabels(labels),
        summary: trimRunes(annotations.summary || annotations.message, 200) || undefined,
        description: trimRunes(annotations.description, 300) || undefined,
        matchesScope: matchesScope || undefined,
      },
    });
  }

  rankedAlerts.sort((a, b) => {
    if (a.priority !== b.priority) {
      return a.priority - b.priority;
    }
    return b.startsAtMs - a.startsAtMs;
  });

  if (rankedAlerts.length > MAX_ALERT_EVIDENCE_ITEMS) {
    pushWarning(
      'evidence_truncated',
      `Truncated Grafana alerts from ${rankedAlerts.length} to top ${MAX_ALERT_EVIDENCE_ITEMS} prioritized items.`
    );
  }

  const alerts: AlertEvidenceItem[] = rankedAlerts
    .slice(0, MAX_ALERT_EVIDENCE_ITEMS)
    .map((r, idx) => ({
      id: `ev-alert-${idx + 1}`,
      ...r.item,
    }));

  // Extract abnormal alerting rules (skipping recording rules and deduplicating active alerts already covered).
  let totalRulesEvaluated = 0;
  let abnormalRulesTotal = 0;
  const candidateRules: Array<{ priority: number; item: Omit<RuleEvidenceItem, 'id'> }> = [];

  const groups = rawRulesResp?.data?.groups ?? [];
  let inspected = 0;
  for (const grp of groups) {
    const groupRules = grp.rules ?? [];
    for (const rule of groupRules) {
      if (inspected >= MAX_RULES_RESPONSE_ITEMS) {
        break;
      }
      inspected++;
      if ((rule.type ?? '').toLowerCase() === 'recording') {
        continue;
      }
      totalRulesEvaluated++;

      const state = normalizeRuleState(rule.state);
      const health = normalizeRuleHealth(rule.health);
      const isAbnormalState = state === 'firing' || state === 'recovering' || state === 'pending';
      const isAbnormalHealth = health === 'error' || health === 'nodata' || health === 'unknown';
      if (!isAbnormalState && !isAbnormalHealth) {
        continue;
      }
      abnormalRulesTotal++;

      const ruleName = trimRunes(rule.name, 128) || 'UnnamedRule';
      const ruleUID = rule.labels?.__alert_rule_uid__ ?? '';
      const alreadyInAlertmanager =
        (ruleUID !== '' && coveredRuleUIDs.has(ruleUID)) || coveredRuleNames.has(ruleName);

      // Avoid double-counting healthy firing rules already covered by /v2/alerts,
      // while preserving error/nodata/pending/recovering rules and external-Alertmanager firing rules.
      if (state === 'firing' && health === 'ok' && alreadyInAlertmanager) {
        continue;
      }

      const ruleMatchesScope =
        matchesScopeLabels(rule.labels, options.scope) ||
        (rule.alerts ?? []).some((inst) => matchesScopeLabels(inst.labels, options.scope));

      if (options.scope?.labelName && options.scope?.labelValue && !ruleMatchesScope) {
        if (!(includeOrgCritical && (health === 'error' || state === 'firing'))) {
          continue;
        }
      }

      let priority = 30;
      if (health === 'error') {
        priority = 5;
      } else if (state === 'firing') {
        priority = 10;
      } else if (health === 'nodata') {
        priority = 15;
      } else if (state === 'pending' || state === 'recovering') {
        priority = 20;
      }

      candidateRules.push({
        priority,
        item: {
          ruleName,
          group: trimRunes(grp.name, 100) || undefined,
          state,
          health,
          lastError: trimRunes(rule.lastError, 240) || undefined,
          activeAlertsCount: Array.isArray(rule.alerts) ? rule.alerts.length : 0,
          matchesScope: ruleMatchesScope || undefined,
        },
      });
    }
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
