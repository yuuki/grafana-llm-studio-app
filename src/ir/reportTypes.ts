import type { ScopeFilter } from './types';

export const ALLOWED_REPORT_SCHEMA_VERSIONS = ['1.0'] as const;
export const ALLOWED_REPORT_OVERALL_STATUSES = [
  'healthy',
  'degraded',
  'critical',
  'insufficient_data',
] as const;
export const ALLOWED_REPORT_CONFIDENCES = ['high', 'medium', 'low'] as const;
export const ALLOWED_REPORT_FINDING_CATEGORIES = [
  'alert',
  'performance',
  'incident',
  'monitoring_gap',
] as const;
export const ALLOWED_REPORT_FINDING_SEVERITIES = ['critical', 'warning', 'info'] as const;
export const ALLOWED_REPORT_LIKELIHOODS = ['high', 'medium', 'low'] as const;
export const ALLOWED_REPORT_ACTION_PRIORITIES = ['immediate', 'short_term', 'follow_up'] as const;

export const ALLOWED_REPORT_WINDOWS = ['15m', '1h', '6h', '24h'] as const;
export const ALLOWED_BASELINE_MODES = ['preceding', '24h_ago', 'none'] as const;
export const ALLOWED_REPORT_LANGUAGES = ['en', 'ja'] as const;

export const ALLOWED_ALERT_STATES = ['active', 'suppressed', 'unprocessed'] as const;
export const ALLOWED_RULE_STATES = ['firing', 'recovering', 'pending', 'inactive'] as const;
export const ALLOWED_RULE_HEALTHS = ['ok', 'error', 'nodata', 'unknown'] as const;
export const ALLOWED_METRIC_QUERY_STATUSES = ['ok', 'empty', 'error', 'timeout'] as const;
export const ALLOWED_METRIC_TRENDS = [
  'rising',
  'falling',
  'stable',
  'spiking',
  'volatile',
  'no_data',
] as const;
export const ALLOWED_THRESHOLD_BREACHES = ['none', 'warning', 'critical'] as const;
export const ALLOWED_ANOMALY_HINTS = [
  'normal',
  'elevated',
  'degraded',
  'missing_data',
  'query_error',
] as const;
export const ALLOWED_COLLECTION_WARNING_CODES = [
  'alerting_forbidden',
  'alerting_unavailable',
  'dashboard_partial',
  'dashboard_no_panels',
  'collection_timeout',
  'evidence_truncated',
  'baseline_skipped',
] as const;

export const MAX_ANALYZE_PAYLOAD_BYTES = 24 * 1024;
export const MAX_ALERT_EVIDENCE_ITEMS = 25;
export const MAX_RULE_EVIDENCE_ITEMS = 20;
export const MAX_METRIC_EVIDENCE_ITEMS = 20;
export const MAX_STATUS_NOTE_ITEMS = 4;
export const MAX_COLLECTION_WARNINGS = 12;
export const MAX_ANALYZE_FOCUS_RUNES = 300;
export const REPORT_MAX_PANELS = 8;

export type ReportSchemaVersion = (typeof ALLOWED_REPORT_SCHEMA_VERSIONS)[number];
export type ReportOverallStatus = (typeof ALLOWED_REPORT_OVERALL_STATUSES)[number];
export type ReportConfidence = (typeof ALLOWED_REPORT_CONFIDENCES)[number];
export type ReportFindingCategory = (typeof ALLOWED_REPORT_FINDING_CATEGORIES)[number];
export type ReportFindingSeverity = (typeof ALLOWED_REPORT_FINDING_SEVERITIES)[number];
export type ReportLikelihood = (typeof ALLOWED_REPORT_LIKELIHOODS)[number];
export type ReportActionPriority = (typeof ALLOWED_REPORT_ACTION_PRIORITIES)[number];

export type ReportWindow = (typeof ALLOWED_REPORT_WINDOWS)[number];
export type BaselineMode = (typeof ALLOWED_BASELINE_MODES)[number];
export type ReportLanguage = (typeof ALLOWED_REPORT_LANGUAGES)[number];

export type AlertEvidenceState = (typeof ALLOWED_ALERT_STATES)[number];
export type RuleEvidenceState = (typeof ALLOWED_RULE_STATES)[number];
export type RuleEvidenceHealth = (typeof ALLOWED_RULE_HEALTHS)[number];
export type MetricQueryStatus = (typeof ALLOWED_METRIC_QUERY_STATUSES)[number];
export type MetricTrend = (typeof ALLOWED_METRIC_TRENDS)[number];
export type ThresholdBreach = (typeof ALLOWED_THRESHOLD_BREACHES)[number];
export type AnomalyHint = (typeof ALLOWED_ANOMALY_HINTS)[number];
export type CollectionWarningCode = (typeof ALLOWED_COLLECTION_WARNING_CODES)[number];

export interface ReportTimelineItem {
  timestamp: string;
  event: string;
  evidenceIds: string[];
}

export interface ReportFinding {
  id: string;
  category: ReportFindingCategory;
  severity: ReportFindingSeverity;
  title: string;
  detail: string;
  evidenceIds: string[];
}

export interface ReportHypothesis {
  title: string;
  likelihood: ReportLikelihood;
  rationale: string;
  evidenceIds: string[];
  verificationSteps: string[];
}

export interface ReportAction {
  priority: ReportActionPriority;
  action: string;
  relatedFindingIds: string[];
}

export interface StatusReportIR {
  schemaVersion: ReportSchemaVersion;
  overallStatus: ReportOverallStatus;
  confidence: ReportConfidence;
  executiveSummary: string;
  timeline: ReportTimelineItem[];
  findings: ReportFinding[];
  hypotheses: ReportHypothesis[];
  recommendedActions: ReportAction[];
  dataGaps: string[];
  suggestedInvestigationIntent?: string;
}

export interface ReportDashboardContext {
  uid?: string;
  title: string;
  panelCount: number;
  analyzedPanelCount: number;
}

export interface ReportCollectionStatus {
  alertsChecked: boolean;
  rulesChecked: boolean;
  totalRulesEvaluated: number;
  activeAlertsTotal: number;
  abnormalRulesTotal: number;
}

export interface AlertEvidenceItem {
  id: string;
  alertName: string;
  state: AlertEvidenceState;
  severity?: string;
  startsAt?: string;
  labels?: Record<string, string>;
  summary?: string;
  description?: string;
  matchesScope?: boolean;
}

export interface RuleEvidenceItem {
  id: string;
  ruleName: string;
  group?: string;
  state: RuleEvidenceState;
  health: RuleEvidenceHealth;
  lastError?: string;
  activeAlertsCount: number;
  matchesScope?: boolean;
}

export interface MetricWindowStats {
  latest?: number;
  avg: number;
  p95: number;
  min: number;
  max: number;
  sampleCount: number;
  missingRatio: number;
  peakAt?: string;
}

export interface MetricBaselineStats {
  avg: number;
  p95: number;
  max: number;
}

export interface MetricEvidenceItem {
  id: string;
  panelTitle: string;
  unit?: string;
  seriesLabel?: string;
  queryStatus: MetricQueryStatus;
  errorText?: string;
  current?: MetricWindowStats;
  baseline?: MetricBaselineStats;
  deltaPctAvg?: number;
  deltaPctP95?: number;
  baselineWasZero?: boolean;
  trend: MetricTrend;
  thresholdBreached: ThresholdBreach;
  anomalyHint: AnomalyHint;
}

export interface StatusNoteEvidenceItem {
  id: string;
  message: string;
}

export interface CollectionWarningItem {
  id: string;
  code: CollectionWarningCode;
  message: string;
}

export interface ReportEvidenceBundle {
  collectedAt: string;
  alerts: AlertEvidenceItem[];
  rules: RuleEvidenceItem[];
  metrics: MetricEvidenceItem[];
  statusNotes?: StatusNoteEvidenceItem[];
}

export interface AnalyzeStatusPayload {
  datasourceUid?: string;
  scope?: ScopeFilter;
  window: ReportWindow;
  baselineMode: BaselineMode;
  language: ReportLanguage;
  userFocus?: string;
  dashboardContext?: ReportDashboardContext;
  collectionStatus: ReportCollectionStatus;
  evidence: ReportEvidenceBundle;
  collectionWarnings?: CollectionWarningItem[];
}
