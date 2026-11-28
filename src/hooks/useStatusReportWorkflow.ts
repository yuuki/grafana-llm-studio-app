import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { locationService } from '@grafana/runtime';
import { cancelJobBounded } from '../api/cancelHelper';
import { analyzeStatus, extractApiError } from '../api/studioClient';
import { ReportBusyState } from '../components/StatusReport/ReportControlsPanel';
import { AllowedDatasourceOption, loadPublicConfigAndDatasources } from '../datasource/allowlist';
import { getActiveLocale, TranslationKey } from '../i18n';
import {
  ExistingDashboardSummary,
  fetchExistingDashboardByUid,
  listExistingDashboards,
} from '../import/importDashboard';
import {
  AnalyzeStatusPayload,
  BaselineMode,
  CollectionWarningItem,
  JobSnapshot,
  JobStageEvent,
  MAX_ANALYZE_FOCUS_RUNES,
  PublicConfig,
  ReportLanguage,
  ReportWindow,
  ScopeFilter,
} from '../ir/types';
import { collectGrafanaAlertingEvidence } from '../report/alertsCollector';
import { extractDashboardReportQueries } from '../report/dashboardExtractor';
import { copyTextWithFallback, formatStatusReportMarkdown } from '../report/markdownExport';
import { collectDashboardMetricEvidence } from '../report/metricsCollector';
import { redactAndBoundAnalyzePayload } from '../report/redact';
import { downloadBlob } from '../utils/downloadBlob';

type TranslateFn = (key: TranslationKey, vars?: Record<string, string | number>) => string;

interface DashboardReportEvidence {
  metrics: AnalyzeStatusPayload['evidence']['metrics'];
  dashboardContext?: AnalyzeStatusPayload['dashboardContext'];
  queriedDatasourceUid: string;
}

function appendBoundedWarning(
  warnings: CollectionWarningItem[],
  warning: Omit<CollectionWarningItem, 'id'>
): void {
  if (warnings.length < 12) {
    warnings.push({
      ...warning,
      id: `ev-warn-${warnings.length + 1}`,
    });
  }
}

async function collectDashboardReportEvidence(
  selectedDashboardUid: string,
  selectedDsUid: string,
  publicCfg: PublicConfig,
  activeScope: ScopeFilter | undefined,
  windowVal: ReportWindow,
  baselineMode: BaselineMode,
  warnings: CollectionWarningItem[],
  signal: AbortSignal
): Promise<DashboardReportEvidence> {
  const fetched = await fetchExistingDashboardByUid(selectedDashboardUid, signal);
  const extracted = extractDashboardReportQueries(fetched.rawDashboard, {
    selectedDatasourceUid: selectedDsUid || undefined,
    allowedDatasourceUids: publicCfg.allowedDatasourceUids,
    scope: activeScope,
  });

  for (const msg of extracted.warnings) {
    appendBoundedWarning(warnings, { code: 'dashboard_partial', message: msg });
  }

  if (extracted.panels.length === 0) {
    appendBoundedWarning(warnings, {
      code: 'dashboard_no_panels',
      message: `No allowlisted Prometheus/VictoriaMetrics panels could be extracted from "${extracted.dashboardTitle}".`,
    });
    return {
      metrics: [],
      dashboardContext: undefined,
      queriedDatasourceUid: selectedDsUid,
    };
  }

  const queriedDatasourceUid = extracted.panels[0]?.queries[0]?.datasourceUid || selectedDsUid;
  const metricRes = await collectDashboardMetricEvidence(extracted.panels, {
    window: windowVal,
    baselineMode,
    maxConcurrency: publicCfg.maxConcurrentQueries ?? 2,
    perQueryTimeoutMs: (publicCfg.validationTimeoutSeconds ?? 10) * 1000,
    signal,
  });
  for (const mw of metricRes.warnings) {
    appendBoundedWarning(warnings, mw);
  }
  return {
    metrics: metricRes.metrics,
    dashboardContext: {
      uid: selectedDashboardUid,
      title: extracted.dashboardTitle,
      panelCount: extracted.totalPanelCount,
      analyzedPanelCount: metricRes.analyzedPanelCount,
    },
    queriedDatasourceUid,
  };
}

interface RawAnalyzePayloadArgs {
  collectedAt: string;
  alertRes: Awaited<ReturnType<typeof collectGrafanaAlertingEvidence>>;
  metrics: AnalyzeStatusPayload['evidence']['metrics'];
  dashboardContext?: AnalyzeStatusPayload['dashboardContext'];
  queriedDatasourceUid: string;
  activeScope?: ScopeFilter;
  windowVal: ReportWindow;
  baselineMode: BaselineMode;
  reportLanguage: ReportLanguage;
  userFocus: string;
  warnings: CollectionWarningItem[];
}

function buildRawAnalyzePayload(args: RawAnalyzePayloadArgs): AnalyzeStatusPayload {
  const trimmedFocus = args.userFocus.trim();
  return {
    ...(args.queriedDatasourceUid && (args.metrics.length > 0 || args.dashboardContext)
      ? { datasourceUid: args.queriedDatasourceUid }
      : {}),
    ...(args.activeScope ? { scope: args.activeScope } : {}),
    window: args.windowVal,
    baselineMode: args.baselineMode,
    language: args.reportLanguage,
    ...(trimmedFocus
      ? { userFocus: Array.from(trimmedFocus).slice(0, MAX_ANALYZE_FOCUS_RUNES).join('') }
      : {}),
    ...(args.dashboardContext ? { dashboardContext: args.dashboardContext } : {}),
    collectionStatus: args.alertRes.collectionStatus,
    evidence: {
      collectedAt: args.collectedAt,
      alerts: args.alertRes.alerts,
      rules: args.alertRes.rules,
      metrics: args.metrics,
      ...(args.alertRes.statusNotes.length > 0 ? { statusNotes: args.alertRes.statusNotes } : {}),
    },
    ...(args.warnings.length > 0 ? { collectionWarnings: args.warnings } : {}),
  };
}

export function useStatusReportWorkflow(currentOrgId: number, t: TranslateFn) {
  const [publicCfg, setPublicCfg] = useState<PublicConfig | null>(null);
  const [allowedDatasources, setAllowedDatasources] = useState<AllowedDatasourceOption[]>([]);
  const [selectedDsUid, setSelectedDsUid] = useState<string>('');
  const [dashboards, setDashboards] = useState<ExistingDashboardSummary[]>([]);
  const [selectedDashboardUid, setSelectedDashboardUid] = useState<string>('');

  const [scopeLabelName, setScopeLabelName] = useState<string>('service');
  const [scopeLabelValue, setScopeLabelValue] = useState<string>('');
  const [windowVal, setWindowVal] = useState<ReportWindow>('1h');
  const [baselineMode, setBaselineMode] = useState<BaselineMode>('preceding');
  const [reportLanguage, setReportLanguage] = useState<ReportLanguage>(() => getActiveLocale());
  const [userFocus, setUserFocus] = useState<string>('');
  const [includeOrgCritical, setIncludeOrgCritical] = useState<boolean>(true);
  const [previewBeforeSend, setPreviewBeforeSend] = useState<boolean>(false);

  const [busyState, setBusyState] = useState<ReportBusyState>('idle');
  const [liveStage, setLiveStage] = useState<JobStageEvent | null>(null);
  const [pendingPayload, setPendingPayload] = useState<AnalyzeStatusPayload | null>(null);
  const [collectedPayload, setCollectedPayload] = useState<AnalyzeStatusPayload | null>(null);
  const [redactionCount, setRedactionCount] = useState<number>(0);
  const [job, setJob] = useState<JobSnapshot | null>(null);
  const [highlightedEvidenceId, setHighlightedEvidenceId] = useState<string | null>(null);
  const [uiError, setUiError] = useState<string | null>(null);
  const [copyNotice, setCopyNotice] = useState<string | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const inFlightJobIdRef = useRef<string | null>(null);
  const opSeqRef = useRef<number>(0);
  const prevOrgIdRef = useRef<number>(currentOrgId);
  const prevContextKeyRef = useRef<string>(`${currentOrgId}:`);
  const evidenceCardRefs = useRef<Record<string, HTMLDivElement | null>>({});

  const resetReportState = useCallback(() => {
    opSeqRef.current += 1;
    abortRef.current?.abort();
    abortRef.current = null;
    inFlightJobIdRef.current = null;
    setBusyState('idle');
    setLiveStage(null);
    setPendingPayload(null);
    setCollectedPayload(null);
    setRedactionCount(0);
    setJob(null);
    setHighlightedEvidenceId(null);
    setUiError(null);
    setCopyNotice(null);
  }, []);

  // Strict browser state clearing on org or datasource switch (ADR-002 / ADR-005).
  useEffect(() => {
    const orgChanged = prevOrgIdRef.current !== currentOrgId;
    const key = `${currentOrgId}:${selectedDsUid}`;
    if (orgChanged || prevContextKeyRef.current !== key) {
      prevOrgIdRef.current = currentOrgId;
      prevContextKeyRef.current = key;
      resetReportState();
      if (orgChanged) {
        setPublicCfg(null);
        setDashboards([]);
        setSelectedDashboardUid('');
        setScopeLabelValue('');
        setUserFocus('');
      }
    }
  }, [currentOrgId, selectedDsUid, resetReportState]);

  useEffect(() => {
    return () => {
      opSeqRef.current += 1;
      abortRef.current?.abort();
    };
  }, []);

  // Load public config, allowlisted datasources, and searchable dashboards.
  useEffect(() => {
    let active = true;
    const ctrl = new AbortController();
    (async () => {
      try {
        const { publicConfig: cfg, datasources: filtered } = await loadPublicConfigAndDatasources(ctrl.signal);
        if (!active) {
          return;
        }
        setPublicCfg(cfg);
        setAllowedDatasources(filtered);
        const nextDs = filtered.length > 0 ? filtered[0].uid : '';
        prevContextKeyRef.current = `${currentOrgId}:${nextDs}`;
        setSelectedDsUid(nextDs);
      } catch (err) {
        if (active && !ctrl.signal.aborted) {
          setUiError(t('report.errors.loadConfigFailed', { message: extractApiError(err) }));
        }
        return;
      }

      try {
        const list = await listExistingDashboards(50, ctrl.signal);
        if (active) {
          setDashboards(list);
        }
      } catch {
        // Non-fatal: alerts-only mode remains usable even without dashboard search access.
        if (active) {
          setDashboards([]);
        }
      }
    })();
    return () => {
      active = false;
      ctrl.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentOrgId]);

  const activeScope = useMemo<ScopeFilter | undefined>(() => {
    const val = scopeLabelValue.trim();
    if (!scopeLabelName || !val) {
      return undefined;
    }
    return { labelName: scopeLabelName, labelValue: val };
  }, [scopeLabelName, scopeLabelValue]);

  const sendPayloadToBackend = useCallback(
    async (payload: AnalyzeStatusPayload, opSeq: number, ctrl: AbortController) => {
      setBusyState('analyzing');
      setLiveStage(null);
      setUiError(null);
      inFlightJobIdRef.current = null;

      try {
        const snap = await analyzeStatus(payload, ctrl.signal, {
          enableSse: Boolean(publicCfg?.enableSseProgress),
          onStage: (ev) => {
            if (opSeqRef.current !== opSeq) {
              return;
            }
            if (ev.jobId) {
              inFlightJobIdRef.current = ev.jobId;
            }
            setLiveStage(ev);
          },
        });
        if (opSeqRef.current !== opSeq) {
          return;
        }
        inFlightJobIdRef.current = null;
        setLiveStage(null);
        setJob(snap);
        setPendingPayload(null);
        setBusyState('idle');
      } catch (err) {
        if (opSeqRef.current !== opSeq) {
          return;
        }
        inFlightJobIdRef.current = null;
        setLiveStage(null);
        if (ctrl.signal.aborted) {
          setBusyState('idle');
          return;
        }
        setUiError(t('report.errors.analyzeFailed', { message: extractApiError(err) }));
        setBusyState('idle');
      }
    },
    [publicCfg, t]
  );

  const handleCollectAndMaybeAnalyze = useCallback(async () => {
    if (!publicCfg) {
      return;
    }
    opSeqRef.current += 1;
    const opSeq = opSeqRef.current;
    abortRef.current?.abort();
    const ctrl = new AbortController();
    abortRef.current = ctrl;

    setBusyState('collecting');
    setLiveStage(null);
    setUiError(null);
    setCopyNotice(null);
    setJob(null);
    setPendingPayload(null);
    setCollectedPayload(null);
    setRedactionCount(0);
    setHighlightedEvidenceId(null);

    try {
      const collectedAt = new Date().toISOString();
      const alertRes = await collectGrafanaAlertingEvidence({
        scope: activeScope,
        includeOrgCriticalAlerts: includeOrgCritical,
        signal: ctrl.signal,
      });

      const warnings: CollectionWarningItem[] = [...alertRes.warnings];
      const dashEvidence = selectedDashboardUid
        ? await collectDashboardReportEvidence(
            selectedDashboardUid,
            selectedDsUid,
            publicCfg,
            activeScope,
            windowVal,
            baselineMode,
            warnings,
            ctrl.signal
          )
        : {
            metrics: [] as AnalyzeStatusPayload['evidence']['metrics'],
            dashboardContext: undefined,
            queriedDatasourceUid: selectedDsUid,
          };

      if (opSeqRef.current !== opSeq || ctrl.signal.aborted) {
        return;
      }

      if (
        !alertRes.collectionStatus.alertsChecked &&
        !alertRes.collectionStatus.rulesChecked &&
        dashEvidence.metrics.length === 0
      ) {
        setUiError(t('report.errors.noSources'));
        setBusyState('idle');
        return;
      }

      const rawPayload = buildRawAnalyzePayload({
        collectedAt,
        alertRes,
        metrics: dashEvidence.metrics,
        dashboardContext: dashEvidence.dashboardContext,
        queriedDatasourceUid: dashEvidence.queriedDatasourceUid,
        activeScope,
        windowVal,
        baselineMode,
        reportLanguage,
        userFocus,
        warnings,
      });

      const bounded = redactAndBoundAnalyzePayload(rawPayload);
      setCollectedPayload(bounded.payload);
      setRedactionCount(bounded.redactionCount);

      if (previewBeforeSend) {
        setPendingPayload(bounded.payload);
        setBusyState('idle');
        return;
      }

      await sendPayloadToBackend(bounded.payload, opSeq, ctrl);
    } catch (err) {
      if (opSeqRef.current !== opSeq) {
        return;
      }
      setLiveStage(null);
      if (ctrl.signal.aborted) {
        setBusyState('idle');
        return;
      }
      setUiError(t('report.errors.collectionFailed', { message: extractApiError(err) }));
      setBusyState('idle');
    }
  }, [
    activeScope,
    baselineMode,
    includeOrgCritical,
    previewBeforeSend,
    publicCfg,
    reportLanguage,
    selectedDashboardUid,
    selectedDsUid,
    sendPayloadToBackend,
    t,
    userFocus,
    windowVal,
  ]);

  const handleConfirmPreviewSend = useCallback(async () => {
    if (!pendingPayload) {
      return;
    }
    opSeqRef.current += 1;
    const opSeq = opSeqRef.current;
    abortRef.current?.abort();
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    await sendPayloadToBackend(pendingPayload, opSeq, ctrl);
  }, [pendingPayload, sendPayloadToBackend]);

  const handleDiscardPreview = useCallback(() => {
    setPendingPayload(null);
    setCollectedPayload(null);
    setRedactionCount(0);
  }, []);

  const handleCancel = useCallback(async () => {
    setBusyState('cancelling');
    setLiveStage(null);
    opSeqRef.current += 1;
    const cancelOpSeq = opSeqRef.current;
    abortRef.current?.abort();
    abortRef.current = null;
    const activeJobId = inFlightJobIdRef.current;
    inFlightJobIdRef.current = null;
    if (activeJobId) {
      await cancelJobBounded(activeJobId);
    }
    if (opSeqRef.current === cancelOpSeq) {
      setBusyState('idle');
    }
  }, []);

  const handleJumpToEvidence = useCallback((evidenceId: string) => {
    setHighlightedEvidenceId(evidenceId);
    const el = evidenceCardRefs.current[evidenceId];
    if (el) {
      if (typeof el.scrollIntoView === 'function') {
        el.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
      }
      if (typeof el.focus === 'function') {
        el.focus({ preventScroll: true });
      }
    }
  }, []);

  const report = job?.statusReport ?? null;

  const buildMarkdown = useCallback(() => {
    if (!report) {
      return '';
    }
    const snapLang = collectedPayload?.language ?? reportLanguage;
    const snapWindow = collectedPayload?.window ?? windowVal;
    const snapBaseline = collectedPayload?.baselineMode ?? baselineMode;
    const snapScope = collectedPayload?.scope;
    return formatStatusReportMarkdown(report, snapLang, {
      generatedAt: job?.updatedAt,
      window: snapWindow,
      baselineMode: snapBaseline,
      dashboardTitle: collectedPayload?.dashboardContext?.title,
      datasourceUid: collectedPayload?.datasourceUid || selectedDsUid || undefined,
      scopeText: snapScope ? `${snapScope.labelName}=${snapScope.labelValue}` : undefined,
    });
  }, [baselineMode, collectedPayload, job, report, reportLanguage, selectedDsUid, windowVal]);

  const handleCopyMarkdown = useCallback(async () => {
    const md = buildMarkdown();
    if (!md) {
      return;
    }
    const ok = await copyTextWithFallback(md);
    setCopyNotice(ok ? t('report.result.copiedMarkdown') : t('report.result.copyFailed'));
  }, [buildMarkdown, t]);

  const handleDownloadMarkdown = useCallback(() => {
    const md = buildMarkdown();
    if (!md) {
      return;
    }
    const snapWindow = collectedPayload?.window ?? windowVal;
    downloadBlob(`status-report-${snapWindow}.md`, md, 'text/markdown;charset=utf-8');
  }, [buildMarkdown, collectedPayload, windowVal]);

  const handleExportJson = useCallback(() => {
    if (!report) {
      return;
    }
    const snapWindow = collectedPayload?.window ?? windowVal;
    downloadBlob(
      `status-report-${snapWindow}.json`,
      JSON.stringify(report, null, 2),
      'application/json;charset=utf-8'
    );
  }, [collectedPayload, report, windowVal]);

  const handleInvestigateInStudio = useCallback(() => {
    if (!report?.suggestedInvestigationIntent) {
      return;
    }
    const params = new URLSearchParams();
    const handoffDs = collectedPayload?.datasourceUid || selectedDsUid;
    const handoffScope = collectedPayload?.scope ?? activeScope;
    if (handoffDs) {
      params.set('datasource', handoffDs);
    }
    if (handoffScope) {
      params.set('scopeLabel', handoffScope.labelName);
      params.set('scopeValue', handoffScope.labelValue);
    }
    params.set('intent', report.suggestedInvestigationIntent);
    locationService.push(`/a/yuuki-llmstudio-app?${params.toString()}`);
  }, [activeScope, collectedPayload, report, selectedDsUid]);

  return {
    publicCfg,
    allowedDatasources,
    selectedDsUid,
    setSelectedDsUid: (uid: string) => {
      setSelectedDsUid(uid);
      setPendingPayload(null);
    },
    dashboards,
    selectedDashboardUid,
    setSelectedDashboardUid: (uid: string) => {
      setSelectedDashboardUid(uid);
      setPendingPayload(null);
    },
    scopeLabelName,
    setScopeLabelName: (val: string) => {
      setScopeLabelName(val);
      setPendingPayload(null);
    },
    scopeLabelValue,
    setScopeLabelValue: (val: string) => {
      setScopeLabelValue(val);
      setPendingPayload(null);
    },
    windowVal,
    setWindowVal: (val: ReportWindow) => {
      setWindowVal(val);
      setPendingPayload(null);
    },
    baselineMode,
    setBaselineMode: (val: BaselineMode) => {
      setBaselineMode(val);
      setPendingPayload(null);
    },
    reportLanguage,
    setReportLanguage: (val: ReportLanguage) => {
      setReportLanguage(val);
      setPendingPayload(null);
    },
    userFocus,
    setUserFocus: (val: string) => {
      setUserFocus(val);
      setPendingPayload(null);
    },
    includeOrgCritical,
    setIncludeOrgCritical: (checked: boolean) => {
      setIncludeOrgCritical(checked);
      setPendingPayload(null);
    },
    previewBeforeSend,
    setPreviewBeforeSend,
    busyState,
    liveStage,
    pendingPayload,
    collectedPayload,
    redactionCount,
    report,
    highlightedEvidenceId,
    evidenceCardRefs,
    uiError,
    setUiError,
    copyNotice,
    setCopyNotice,
    handleCollectAndMaybeAnalyze,
    handleConfirmPreviewSend,
    handleDiscardPreview,
    handleCancel,
    handleJumpToEvidence,
    handleCopyMarkdown,
    handleDownloadMarkdown,
    handleExportJson,
    handleInvestigateInStudio,
  };
}
