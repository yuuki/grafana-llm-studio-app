import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { config, getDataSourceSrv, locationService, PluginPage } from '@grafana/runtime';
import { Alert, Badge, Button, Checkbox, Field, Input, Select, Spinner, TextArea } from '@grafana/ui';
import {
  analyzeStatus,
  cancelDraftJob,
  extractApiError,
  fetchPublicConfig,
} from '../api/studioClient';
import { getActiveLocale, TranslationKey, useStudioTranslation } from '../i18n';
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
  JobStatus,
  MAX_ANALYZE_FOCUS_RUNES,
  PublicConfig,
  ReportLanguage,
  ReportOverallStatus,
  ReportWindow,
  ScopeFilter,
} from '../ir/types';
import { collectGrafanaAlertingEvidence } from '../report/alertsCollector';
import { extractDashboardReportQueries } from '../report/dashboardExtractor';
import { copyTextWithFallback, formatStatusReportMarkdown } from '../report/markdownExport';
import { collectDashboardMetricEvidence } from '../report/metricsCollector';
import { redactAndBoundAnalyzePayload } from '../report/redact';

interface AllowedDatasourceOption {
  uid: string;
  name: string;
  type: string;
}

const SCOPE_LABELS = ['service', 'job', 'namespace'] as const;

const STATUS_BADGE_COLOR: Record<ReportOverallStatus, 'green' | 'orange' | 'red' | 'blue'> = {
  healthy: 'green',
  degraded: 'orange',
  critical: 'red',
  insufficient_data: 'blue',
};

const SEVERITY_BADGE_COLOR: Record<'critical' | 'warning' | 'info', 'red' | 'orange' | 'blue'> = {
  critical: 'red',
  warning: 'orange',
  info: 'blue',
};

const STAGE_LABEL_KEYS: Partial<Record<JobStatus, TranslationKey>> = {
  analyzing: 'studio.progress.stage.analyzing',
  validating_report: 'studio.progress.stage.validating_report',
  ready: 'studio.progress.stage.ready',
  cancelled: 'studio.progress.stage.cancelled',
  failed: 'studio.progress.stage.failed',
};

function isPrometheusCompatibleType(dsType: string): boolean {
  const lower = (dsType || '').toLowerCase();
  return lower === 'prometheus' || lower.includes('victoriametrics');
}

function downloadBlob(filename: string, content: string, mimeType: string) {
  const blob = new Blob([content], { type: mimeType });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

export const StatusReportPage: React.FC = () => {
  const { t } = useStudioTranslation();
  const currentOrgId = config?.bootData?.user?.orgId ?? 0;

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

  const [busyState, setBusyState] = useState<'idle' | 'collecting' | 'analyzing' | 'cancelling'>('idle');
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
        const cfg = await fetchPublicConfig(ctrl.signal);
        if (!active) {
          return;
        }
        setPublicCfg(cfg);
        const allDs = getDataSourceSrv().getList({ metrics: true });
        const allowSet = new Set(cfg.allowedDatasourceUids);
        const filtered: AllowedDatasourceOption[] = allDs
          .filter((d) => allowSet.has(d.uid) && isPrometheusCompatibleType(d.type))
          .map((d) => ({ uid: d.uid, name: d.name, type: d.type }));
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
      let metrics = [] as AnalyzeStatusPayload['evidence']['metrics'];
      let dashboardContext: AnalyzeStatusPayload['dashboardContext'];
      let queriedDatasourceUid = selectedDsUid;

      if (selectedDashboardUid) {
        const fetched = await fetchExistingDashboardByUid(selectedDashboardUid, ctrl.signal);
        const extracted = extractDashboardReportQueries(fetched.rawDashboard, {
          selectedDatasourceUid: selectedDsUid || undefined,
          allowedDatasourceUids: publicCfg.allowedDatasourceUids,
          scope: activeScope,
        });

        for (const msg of extracted.warnings) {
          if (warnings.length < 12) {
            warnings.push({
              id: `ev-warn-${warnings.length + 1}`,
              code: 'dashboard_partial',
              message: msg,
            });
          }
        }

        if (extracted.panels.length === 0) {
          if (warnings.length < 12) {
            warnings.push({
              id: `ev-warn-${warnings.length + 1}`,
              code: 'dashboard_no_panels',
              message: `No allowlisted Prometheus/VictoriaMetrics panels could be extracted from "${extracted.dashboardTitle}".`,
            });
          }
        } else {
          queriedDatasourceUid = extracted.panels[0]?.queries[0]?.datasourceUid || selectedDsUid;
          const metricRes = await collectDashboardMetricEvidence(extracted.panels, {
            window: windowVal,
            baselineMode,
            maxConcurrency: publicCfg.maxConcurrentQueries ?? 2,
            perQueryTimeoutMs: (publicCfg.validationTimeoutSeconds ?? 10) * 1000,
            signal: ctrl.signal,
          });
          metrics = metricRes.metrics;
          for (const mw of metricRes.warnings) {
            if (warnings.length < 12) {
              warnings.push({
                ...mw,
                id: `ev-warn-${warnings.length + 1}`,
              });
            }
          }
          dashboardContext = {
            uid: selectedDashboardUid,
            title: extracted.dashboardTitle,
            panelCount: extracted.totalPanelCount,
            analyzedPanelCount: metricRes.analyzedPanelCount,
          };
        }
      }

      if (opSeqRef.current !== opSeq || ctrl.signal.aborted) {
        return;
      }

      if (!alertRes.collectionStatus.alertsChecked && !alertRes.collectionStatus.rulesChecked && metrics.length === 0) {
        setUiError(t('report.errors.noSources'));
        setBusyState('idle');
        return;
      }

      const rawPayload: AnalyzeStatusPayload = {
        ...(queriedDatasourceUid && (metrics.length > 0 || dashboardContext)
          ? { datasourceUid: queriedDatasourceUid }
          : {}),
        ...(activeScope ? { scope: activeScope } : {}),
        window: windowVal,
        baselineMode,
        language: reportLanguage,
        ...(userFocus.trim()
          ? { userFocus: Array.from(userFocus.trim()).slice(0, MAX_ANALYZE_FOCUS_RUNES).join('') }
          : {}),
        ...(dashboardContext ? { dashboardContext } : {}),
        collectionStatus: alertRes.collectionStatus,
        evidence: {
          collectedAt,
          alerts: alertRes.alerts,
          rules: alertRes.rules,
          metrics,
          ...(alertRes.statusNotes.length > 0 ? { statusNotes: alertRes.statusNotes } : {}),
        },
        ...(warnings.length > 0 ? { collectionWarnings: warnings } : {}),
      };

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
      const cancelCtrl = new AbortController();
      const cancelTimer = setTimeout(() => cancelCtrl.abort(), 5000);
      try {
        await cancelDraftJob(activeJobId, cancelCtrl.signal);
      } catch {
        // Best-effort bounded backend cancel.
      } finally {
        clearTimeout(cancelTimer);
      }
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

  const isBusy = busyState !== 'idle';

  const dashboardOptions = useMemo(
    () => [
      { label: t('report.controls.dashboardAlertsOnly'), value: '' },
      ...dashboards.map((d) => ({
        label: d.folderTitle ? `${d.title} (${d.folderTitle})` : d.title,
        value: d.uid,
      })),
    ],
    [dashboards, t]
  );

  const dsOptions = useMemo(
    () => allowedDatasources.map((d) => ({ label: `${d.name} (${d.uid})`, value: d.uid })),
    [allowedDatasources]
  );

  const stageText = liveStage?.stage && STAGE_LABEL_KEYS[liveStage.stage] ? t(STAGE_LABEL_KEYS[liveStage.stage]!) : null;

  return (
    <PluginPage>
      <div style={{ display: 'flex', flexDirection: 'column', gap: '16px', paddingBottom: '24px' }}>
        <div>
          <h2 style={{ margin: 0 }}>{t('report.pageTitle')}</h2>
          <p style={{ margin: '4px 0 0', opacity: 0.8 }}>{t('report.pageSubtitle')}</p>
        </div>

        {publicCfg?.mockBanner && (
          <Alert title={t('studio.mockBanner.title')} severity="warning">
            {t('studio.mockBanner.body')}
          </Alert>
        )}

        {uiError && (
          <Alert title={t('studio.alert.errorTitle')} severity="error" onRemove={() => setUiError(null)}>
            {uiError}
          </Alert>
        )}

        {copyNotice && (
          <Alert title={copyNotice} severity="info" onRemove={() => setCopyNotice(null)} />
        )}

        {/* Controls */}
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
            gap: '12px',
            padding: '16px',
            border: '1px solid rgba(204, 204, 220, 0.2)',
            borderRadius: '4px',
          }}
        >
          <Field label={t('report.controls.dashboardLabel')}>
            <Select
              inputId="report-dashboard-select"
              options={dashboardOptions}
              value={selectedDashboardUid}
              onChange={(v) => {
                setSelectedDashboardUid(v.value ?? '');
                setPendingPayload(null);
              }}
              disabled={isBusy}
            />
          </Field>

          <Field label={t('report.controls.datasourceLabel')}>
            <Select
              inputId="report-datasource-select"
              options={dsOptions}
              value={selectedDsUid}
              onChange={(v) => {
                setSelectedDsUid(v.value ?? '');
                setPendingPayload(null);
              }}
              disabled={isBusy || dsOptions.length === 0}
            />
          </Field>

          <Field label={t('report.controls.windowLabel')}>
            <Select
              inputId="report-window-select"
              options={[
                { label: '15m', value: '15m' as ReportWindow },
                { label: '1h', value: '1h' as ReportWindow },
                { label: '6h', value: '6h' as ReportWindow },
                { label: '24h', value: '24h' as ReportWindow },
              ]}
              value={windowVal}
              onChange={(v) => {
                if (v.value) {
                  setWindowVal(v.value);
                  setPendingPayload(null);
                }
              }}
              disabled={isBusy}
            />
          </Field>

          <Field label={t('report.controls.baselineLabel')}>
            <Select
              inputId="report-baseline-select"
              options={[
                { label: t('report.controls.baselinePreceding'), value: 'preceding' as BaselineMode },
                { label: t('report.controls.baseline24h'), value: '24h_ago' as BaselineMode },
                { label: t('report.controls.baselineNone'), value: 'none' as BaselineMode },
              ]}
              value={baselineMode}
              onChange={(v) => {
                if (v.value) {
                  setBaselineMode(v.value);
                  setPendingPayload(null);
                }
              }}
              disabled={isBusy}
            />
          </Field>

          <Field label={t('report.controls.scopeLabel')}>
            <Select
              inputId="report-scope-label-select"
              options={SCOPE_LABELS.map((l) => ({ label: l, value: l }))}
              value={scopeLabelName}
              onChange={(v) => {
                if (v.value) {
                  setScopeLabelName(v.value);
                  setPendingPayload(null);
                }
              }}
              disabled={isBusy}
            />
          </Field>

          <Field label={t('report.controls.scopeValueLabel')}>
            <Input
              id="report-scope-value-input"
              value={scopeLabelValue}
              placeholder={t('report.controls.scopeValuePlaceholder')}
              onChange={(e) => {
                setScopeLabelValue(e.currentTarget.value);
                setPendingPayload(null);
              }}
              disabled={isBusy}
            />
          </Field>

          <Field label={t('report.controls.languageLabel')}>
            <Select
              inputId="report-language-select"
              options={[
                { label: t('report.controls.languageJa'), value: 'ja' as ReportLanguage },
                { label: t('report.controls.languageEn'), value: 'en' as ReportLanguage },
              ]}
              value={reportLanguage}
              onChange={(v) => {
                if (v.value) {
                  setReportLanguage(v.value);
                  setPendingPayload(null);
                }
              }}
              disabled={isBusy}
            />
          </Field>

          <Field label={t('report.controls.focusLabel')}>
            <TextArea
              id="report-focus-input"
              rows={2}
              value={userFocus}
              placeholder={t('report.controls.focusPlaceholder')}
              onChange={(e) => {
                setUserFocus(e.currentTarget.value);
                setPendingPayload(null);
              }}
              disabled={isBusy}
            />
          </Field>
        </div>

        <div style={{ display: 'flex', flexWrap: 'wrap', gap: '16px', alignItems: 'center' }}>
          <Checkbox
            label={t('report.controls.includeOrgCritical')}
            value={includeOrgCritical}
            onChange={(e) => {
              setIncludeOrgCritical(e.currentTarget.checked);
              setPendingPayload(null);
            }}
            disabled={isBusy}
          />
          <Checkbox
            label={t('report.controls.previewBeforeSend')}
            value={previewBeforeSend}
            onChange={(e) => setPreviewBeforeSend(e.currentTarget.checked)}
            disabled={isBusy}
          />
          <div style={{ display: 'flex', gap: '8px', marginLeft: 'auto' }}>
            <Button
              variant="primary"
              onClick={handleCollectAndMaybeAnalyze}
              disabled={isBusy || !publicCfg}
              data-testid="report-analyze-button"
            >
              {busyState === 'collecting'
                ? t('report.controls.collecting')
                : busyState === 'analyzing'
                  ? t('report.controls.analyzing')
                  : previewBeforeSend
                    ? t('report.controls.collectPreviewButton')
                    : t('report.controls.analyzeButton')}
            </Button>
            {isBusy && (
              <Button variant="destructive" onClick={handleCancel} disabled={busyState === 'cancelling'}>
                {busyState === 'cancelling' ? t('report.controls.cancelling') : t('report.controls.cancelButton')}
              </Button>
            )}
          </div>
        </div>

        {isBusy && (
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }} data-testid="report-progress">
            <Spinner />
            <span>
              {stageText ??
                (busyState === 'collecting' ? t('report.controls.collecting') : t('report.controls.analyzing'))}
            </span>
          </div>
        )}

        {/* Split Pane: Evidence Inspector (Left) + Status Report (Right) */}
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))',
            gap: '16px',
            alignItems: 'start',
          }}
        >
          {/* Left Pane: Evidence Inspector */}
          <div
            style={{
              border: '1px solid rgba(204, 204, 220, 0.2)',
              borderRadius: '4px',
              padding: '16px',
              display: 'flex',
              flexDirection: 'column',
              gap: '12px',
            }}
            data-testid="report-evidence-pane"
          >
            <h3 style={{ margin: 0 }}>{t('report.evidence.heading')}</h3>

            {!collectedPayload ? (
              <p style={{ opacity: 0.75 }}>{t('report.evidence.empty')}</p>
            ) : (
              <>
                <div style={{ fontSize: '12px', opacity: 0.85 }}>
                  {t('report.evidence.summaryBadges', {
                    alerts: collectedPayload.evidence.alerts.length,
                    rules: collectedPayload.evidence.rules.length,
                    metrics: collectedPayload.evidence.metrics.length,
                    redacted: redactionCount,
                  })}
                </div>

                {pendingPayload && (
                  <Alert title={t('report.evidence.previewReady')} severity="info">
                    <div style={{ display: 'flex', gap: '8px', marginTop: '8px' }}>
                      <Button
                        variant="primary"
                        size="sm"
                        onClick={handleConfirmPreviewSend}
                        disabled={isBusy}
                        data-testid="report-confirm-send-button"
                      >
                        {t('report.controls.sendToLlmButton')}
                      </Button>
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() => {
                          setPendingPayload(null);
                          setCollectedPayload(null);
                          setRedactionCount(0);
                        }}
                        disabled={isBusy}
                      >
                        {t('report.controls.discardPreviewButton')}
                      </Button>
                    </div>
                  </Alert>
                )}

                {(collectedPayload.collectionWarnings?.length ?? 0) > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.warningsHeading')}</h4>
                    {collectedPayload.collectionWarnings!.map((w) => (
                      <div
                        key={w.id}
                        ref={(el) => {
                          evidenceCardRefs.current[w.id] = el;
                        }}
                        tabIndex={-1}
                        aria-current={highlightedEvidenceId === w.id ? 'true' : undefined}
                        style={{
                          padding: '8px',
                          marginBottom: '6px',
                          borderRadius: '4px',
                          border:
                            highlightedEvidenceId === w.id
                              ? '2px solid #5794f2'
                              : '1px solid rgba(255, 152, 48, 0.4)',
                        }}
                      >
                        <Badge text={w.id} color="orange" /> <strong style={{ marginLeft: '6px' }}>{w.code}</strong>
                        <div style={{ fontSize: '12px', marginTop: '4px' }}>{w.message}</div>
                      </div>
                    ))}
                  </div>
                )}

                {collectedPayload.evidence.alerts.length > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.alertsHeading')}</h4>
                    {collectedPayload.evidence.alerts.map((a) => (
                      <div
                        key={a.id}
                        ref={(el) => {
                          evidenceCardRefs.current[a.id] = el;
                        }}
                        tabIndex={-1}
                        aria-current={highlightedEvidenceId === a.id ? 'true' : undefined}
                        data-testid={`evidence-card-${a.id}`}
                        style={{
                          padding: '8px',
                          marginBottom: '6px',
                          borderRadius: '4px',
                          border:
                            highlightedEvidenceId === a.id
                              ? '2px solid #5794f2'
                              : '1px solid rgba(204, 204, 220, 0.2)',
                        }}
                      >
                        <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                          <Badge text={a.id} color="blue" />
                          <strong>{a.alertName}</strong>
                          <Badge text={a.state} color={a.state === 'active' ? 'red' : 'orange'} />
                          {a.severity && <Badge text={a.severity} color="orange" />}
                        </div>
                        {a.summary && <div style={{ fontSize: '12px', marginTop: '4px' }}>{a.summary}</div>}
                      </div>
                    ))}
                  </div>
                )}

                {collectedPayload.evidence.rules.length > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.rulesHeading')}</h4>
                    {collectedPayload.evidence.rules.map((r) => (
                      <div
                        key={r.id}
                        ref={(el) => {
                          evidenceCardRefs.current[r.id] = el;
                        }}
                        style={{
                          padding: '8px',
                          marginBottom: '6px',
                          borderRadius: '4px',
                          border:
                            highlightedEvidenceId === r.id
                              ? '2px solid #5794f2'
                              : '1px solid rgba(204, 204, 220, 0.2)',
                        }}
                      >
                        <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                          <Badge text={r.id} color="blue" />
                          <strong>{r.ruleName}</strong>
                          <Badge text={`${r.state}/${r.health}`} color={r.health === 'error' ? 'red' : 'orange'} />
                        </div>
                        {r.lastError && <div style={{ fontSize: '12px', marginTop: '4px' }}>{r.lastError}</div>}
                      </div>
                    ))}
                  </div>
                )}

                {collectedPayload.evidence.metrics.length > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.metricsHeading')}</h4>
                    {collectedPayload.evidence.metrics.map((m) => (
                      <div
                        key={m.id}
                        ref={(el) => {
                          evidenceCardRefs.current[m.id] = el;
                        }}
                        data-testid={`evidence-card-${m.id}`}
                        style={{
                          padding: '8px',
                          marginBottom: '6px',
                          borderRadius: '4px',
                          border:
                            highlightedEvidenceId === m.id
                              ? '2px solid #5794f2'
                              : '1px solid rgba(204, 204, 220, 0.2)',
                        }}
                      >
                        <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                          <Badge text={m.id} color="blue" />
                          <strong>{m.panelTitle}</strong>
                          <Badge
                            text={m.anomalyHint}
                            color={
                              m.anomalyHint === 'degraded' || m.anomalyHint === 'query_error'
                                ? 'red'
                                : m.anomalyHint === 'elevated'
                                  ? 'orange'
                                  : 'green'
                            }
                          />
                        </div>
                        {m.seriesLabel && <div style={{ fontSize: '11px', opacity: 0.75 }}>{m.seriesLabel}</div>}
                        {m.current && (
                          <div style={{ fontSize: '12px', marginTop: '4px' }}>
                            avg={m.current.avg}, p95={m.current.p95}, max={m.current.max}
                            {m.deltaPctAvg !== undefined &&
                              ` (${t('report.evidence.deltaLabel', { delta: m.deltaPctAvg })})`}
                          </div>
                        )}
                        {m.errorText && <div style={{ fontSize: '12px', color: '#f2495c' }}>{m.errorText}</div>}
                      </div>
                    ))}
                  </div>
                )}

                {(collectedPayload.evidence.statusNotes?.length ?? 0) > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.statusNotesHeading')}</h4>
                    {collectedPayload.evidence.statusNotes!.map((n) => (
                      <div
                        key={n.id}
                        ref={(el) => {
                          evidenceCardRefs.current[n.id] = el;
                        }}
                        style={{
                          padding: '8px',
                          marginBottom: '6px',
                          borderRadius: '4px',
                          border:
                            highlightedEvidenceId === n.id
                              ? '2px solid #5794f2'
                              : '1px solid rgba(204, 204, 220, 0.2)',
                        }}
                      >
                        <Badge text={n.id} color="green" /> <span style={{ marginLeft: '6px' }}>{n.message}</span>
                      </div>
                    ))}
                  </div>
                )}
              </>
            )}
          </div>

          {/* Right Pane: Structured Status Report */}
          <div
            style={{
              border: '1px solid rgba(204, 204, 220, 0.2)',
              borderRadius: '4px',
              padding: '16px',
              display: 'flex',
              flexDirection: 'column',
              gap: '14px',
            }}
            data-testid="report-result-pane"
          >
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '8px' }}>
              <h3 style={{ margin: 0 }}>{t('report.result.heading')}</h3>
              {report && (
                <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap' }}>
                  <Button variant="secondary" size="sm" onClick={handleCopyMarkdown} data-testid="report-copy-md">
                    {t('report.result.copyMarkdown')}
                  </Button>
                  <Button variant="secondary" size="sm" onClick={handleDownloadMarkdown}>
                    {t('report.result.downloadMarkdown')}
                  </Button>
                  <Button variant="secondary" size="sm" onClick={handleExportJson}>
                    {t('report.result.exportJson')}
                  </Button>
                  {report.suggestedInvestigationIntent && (
                    <Button
                      variant="primary"
                      size="sm"
                      onClick={handleInvestigateInStudio}
                      data-testid="report-investigate-studio"
                    >
                      {t('report.result.investigateInStudio')}
                    </Button>
                  )}
                </div>
              )}
            </div>

            {!report ? (
              <p style={{ opacity: 0.75 }}>{t('report.result.empty')}</p>
            ) : (
              <>
                <div style={{ display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' }}>
                  <span>{t('report.result.overallStatus')}:</span>
                  <Badge
                    text={t(`report.status.${report.overallStatus}` as TranslationKey)}
                    color={STATUS_BADGE_COLOR[report.overallStatus]}
                  />
                  <span>{t('report.result.confidence')}:</span>
                  <Badge text={report.confidence} color="blue" />
                </div>

                <div>
                  <h4 style={{ margin: '0 0 6px' }}>{t('report.result.executiveSummary')}</h4>
                  <p style={{ margin: 0 }} data-testid="report-executive-summary">
                    {report.executiveSummary}
                  </p>
                </div>

                <div>
                  <h4 style={{ margin: '0 0 6px' }}>{t('report.result.findings')}</h4>
                  {report.findings.length === 0 ? (
                    <p style={{ margin: 0, opacity: 0.75 }}>{t('report.result.noFindings')}</p>
                  ) : (
                    report.findings.map((f) => (
                      <div
                        key={f.id}
                        style={{
                          padding: '10px',
                          marginBottom: '8px',
                          borderRadius: '4px',
                          border: '1px solid rgba(204, 204, 220, 0.2)',
                        }}
                      >
                        <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                          <Badge text={f.severity} color={SEVERITY_BADGE_COLOR[f.severity]} />
                          <Badge text={f.category} color="blue" />
                          <strong>{f.title}</strong>
                          <span style={{ fontSize: '12px', opacity: 0.7 }}>({f.id})</span>
                        </div>
                        <p style={{ margin: '6px 0' }}>{f.detail}</p>
                        <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }}>
                          {f.evidenceIds.map((evId) => (
                            <Button
                              key={evId}
                              variant="secondary"
                              size="sm"
                              onClick={() => handleJumpToEvidence(evId)}
                              data-testid={`cite-${evId}`}
                            >
                              {evId}
                            </Button>
                          ))}
                        </div>
                      </div>
                    ))
                  )}
                </div>

                {report.timeline.length > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.result.timeline')}</h4>
                    {report.timeline.map((item, idx) => (
                      <div key={idx} style={{ display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '6px', flexWrap: 'wrap' }}>
                        <code>{item.timestamp}</code>
                        <span>{item.event}</span>
                        {item.evidenceIds.map((evId) => (
                          <Button key={evId} variant="secondary" size="sm" onClick={() => handleJumpToEvidence(evId)}>
                            {evId}
                          </Button>
                        ))}
                      </div>
                    ))}
                  </div>
                )}

                {report.hypotheses.length > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.result.hypotheses')}</h4>
                    {report.hypotheses.map((h, idx) => (
                      <div
                        key={idx}
                        style={{
                          padding: '10px',
                          marginBottom: '8px',
                          borderRadius: '4px',
                          border: '1px solid rgba(204, 204, 220, 0.2)',
                        }}
                      >
                        <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                          <Badge text={h.likelihood} color="orange" />
                          <strong>{h.title}</strong>
                        </div>
                        <p style={{ margin: '6px 0' }}>{h.rationale}</p>
                        {h.verificationSteps.length > 0 && (
                          <div style={{ margin: '4px 0 6px' }}>
                            <div style={{ fontSize: '12px', fontWeight: 600 }}>{t('report.result.verificationSteps')}</div>
                            <ul style={{ margin: '4px 0 0 18px' }}>
                              {h.verificationSteps.map((step, sIdx) => (
                                <li key={sIdx}>{step}</li>
                              ))}
                            </ul>
                          </div>
                        )}
                        <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }}>
                          {h.evidenceIds.map((evId) => (
                            <Button key={evId} variant="secondary" size="sm" onClick={() => handleJumpToEvidence(evId)}>
                              {evId}
                            </Button>
                          ))}
                        </div>
                      </div>
                    ))}
                  </div>
                )}

                {report.recommendedActions.length > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.result.recommendedActions')}</h4>
                    <ul style={{ margin: '0 0 0 18px' }}>
                      {report.recommendedActions.map((act, idx) => (
                        <li key={idx} style={{ marginBottom: '4px' }}>
                          <Badge text={act.priority} color={act.priority === 'immediate' ? 'red' : 'blue'} />{' '}
                          <span>{act.action}</span>
                          {act.relatedFindingIds.length > 0 && (
                            <span style={{ fontSize: '12px', opacity: 0.7 }}>
                              {' '}
                              ({act.relatedFindingIds.join(', ')})
                            </span>
                          )}
                        </li>
                      ))}
                    </ul>
                  </div>
                )}

                {report.dataGaps.length > 0 && (
                  <div>
                    <h4 style={{ margin: '0 0 6px' }}>{t('report.result.dataGaps')}</h4>
                    <ul style={{ margin: '0 0 0 18px' }}>
                      {report.dataGaps.map((gap, idx) => (
                        <li key={idx}>{gap}</li>
                      ))}
                    </ul>
                  </div>
                )}

                {report.suggestedInvestigationIntent && (
                  <div style={{ fontSize: '12px', opacity: 0.85 }}>
                    <strong>{t('report.result.suggestedIntentLabel')}</strong> {report.suggestedInvestigationIntent}
                  </div>
                )}
              </>
            )}
          </div>
        </div>
      </div>
    </PluginPage>
  );
};
