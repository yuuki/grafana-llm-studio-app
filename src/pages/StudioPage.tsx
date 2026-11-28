import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { config, getDataSourceSrv, locationService } from '@grafana/runtime';
import { EmbeddedScene } from '@grafana/scenes';
import {
  Alert,
  Badge,
  Button,
  Field,
  Input,
  Select,
  Spinner,
  TextArea,
} from '@grafana/ui';
import {
  cancelDraftJob,
  extractApiError,
  fetchPublicConfig,
  generateDraft,
  importDraft,
  patchDraft,
  repairDraft,
  undoDraft,
} from '../api/studioClient';
import { selectMetricCandidates } from '../datasource/bm25';
import { fetchScopedMetricCatalog, fetchScopeLabelValues } from '../datasource/metadata';
import { runDryRunValidation } from '../datasource/validator';
import { TranslationKey, useStudioTranslation } from '../i18n';
import {
  convertGrafanaDashboardToIR,
  ExistingDashboardSummary,
  fetchExistingDashboardByUid,
  ImportConversionReport,
  ImportPatchContext,
  listExistingDashboards,
} from '../import/importDashboard';
import {
  computePanelQueryHash,
  JobSnapshot,
  JobStageEvent,
  JobStatus,
  MetricCatalogItem,
  PanelValidationResult,
  PublicConfig,
  ScopeFilter,
  ValidationCategory,
} from '../ir/types';
import {
  buildExportableDashboardJson,
  DashboardSaver,
  GrafanaFolderOption,
  listWritableFolders,
  SaveDashboardResult,
} from '../save/saveDashboard';
import { normalizeDashboardIR, NormalizedDashboardSpec } from '../spec/normalize';
import { buildPreviewScene } from '../spec/toScene';

interface AllowedDatasourceOption {
  uid: string;
  name: string;
  type: string;
}

interface PreviewMeta {
  jobId: string;
  revision: number;
}

interface ValidatedPanelCacheEntry {
  rawQueryHash: string;
  varsFingerprint: string;
  category: ValidationCategory;
}

interface PreservedImportMeta {
  resourceVersion?: string;
  folderUid?: string;
  preservedAnnotations?: Record<string, string>;
  preservedLabels?: Record<string, string>;
  patchContext?: ImportPatchContext;
}

const VALIDATION_BADGE_COLOR: Record<ValidationCategory, 'green' | 'orange' | 'red' | 'blue'> = {
  ok: 'green',
  empty_result: 'blue',
  syntax_error: 'red',
  permission_error: 'red',
  timeout: 'orange',
  datasource_error: 'red',
};

const VALIDATION_LABEL_KEYS: Record<ValidationCategory, TranslationKey> = {
  ok: 'studio.validation.category.ok',
  empty_result: 'studio.validation.category.empty_result',
  syntax_error: 'studio.validation.category.syntax_error',
  permission_error: 'studio.validation.category.permission_error',
  timeout: 'studio.validation.category.timeout',
  datasource_error: 'studio.validation.category.datasource_error',
};

const STAGE_LABEL_KEYS: Record<JobStatus, TranslationKey> = {
  generating: 'studio.progress.stage.generating',
  repairing: 'studio.progress.stage.repairing',
  validating_ir: 'studio.progress.stage.validating_ir',
  analyzing: 'studio.progress.stage.analyzing',
  validating_report: 'studio.progress.stage.validating_report',
  awaiting_preview: 'studio.progress.stage.awaiting_preview',
  ready: 'studio.progress.stage.ready',
  insufficient_metrics: 'studio.progress.stage.insufficient_metrics',
  cancelled: 'studio.progress.stage.cancelled',
  failed: 'studio.progress.stage.failed',
};

const BLOCKING_CATEGORIES = new Set<ValidationCategory>([
  'syntax_error',
  'permission_error',
  'timeout',
  'datasource_error',
]);

function isPrometheusCompatibleType(dsType: string): boolean {
  const lower = (dsType || '').toLowerCase();
  return lower === 'prometheus' || lower.includes('victoriametrics');
}

function resolveDatasourceDialect(
  ds: AllowedDatasourceOption | null,
  pinnedDialects?: Record<string, 'promql' | 'metricsql'>
): { kind: 'promql' | 'metricsql'; pinned: boolean } {
  if (!ds) {
    return { kind: 'promql', pinned: false };
  }
  const pinned = pinnedDialects?.[ds.uid];
  if (pinned === 'promql' || pinned === 'metricsql') {
    return { kind: pinned, pinned: true };
  }
  const lower = (ds.type || '').toLowerCase();
  if (lower.includes('victoriametrics')) {
    return { kind: 'metricsql', pinned: false };
  }
  return { kind: 'promql', pinned: false };
}

function makeScopeKey(dsUid: string, labelName: string, labelValue: string): string {
  return `${dsUid}::${labelName}::${labelValue.trim()}`;
}

function computeVariablesFingerprint(spec: NormalizedDashboardSpec): string {
  return JSON.stringify(
    spec.variables.map((v) => ({
      name: v.name,
      type: v.type,
      query: v.query,
      options: v.options,
      defaultValue: v.defaultValue,
      multi: v.multi,
      includeAll: v.includeAll,
    }))
  );
}

export const StudioPage: React.FC = () => {
  const { t, locale } = useStudioTranslation();
  const currentOrgId = config.bootData?.user?.orgId ?? 1;

  const [publicCfg, setPublicCfg] = useState<PublicConfig | null>(null);
  const [allowedDatasources, setAllowedDatasources] = useState<AllowedDatasourceOption[]>([]);
  const [selectedDsUid, setSelectedDsUid] = useState<string>('');
  const [scopeLabelName, setScopeLabelName] = useState<string>('service');
  const [scopeLabelValues, setScopeLabelValues] = useState<string[]>([]);
  const [scopeLabelValue, setScopeLabelValue] = useState<string>('');

  // In-memory only scoped metadata bound to catalogScopeKey (cleared on org/datasource switch or unmount).
  const [catalog, setCatalog] = useState<MetricCatalogItem[]>([]);
  const [catalogScopeKey, setCatalogScopeKey] = useState<string>('');
  const [catalogTruncated, setCatalogTruncated] = useState(false);
  const [loadingCatalog, setLoadingCatalog] = useState(false);

  // Studio conversation & job state.
  const [promptText, setPromptText] = useState(() => t('studio.prompt.defaultText'));
  const [editPanelId, setEditPanelId] = useState<string>('');
  const [job, setJob] = useState<JobSnapshot | null>(null);
  const [busyState, setBusyState] = useState<
    'idle' | 'generating' | 'importing' | 'patching' | 'validating' | 'repairing' | 'undoing' | 'saving' | 'cancelling'
  >('idle');
  const [liveStage, setLiveStage] = useState<JobStageEvent | null>(null);
  const [sseFallbackNotice, setSseFallbackNotice] = useState(false);
  const [handoffFromReport, setHandoffFromReport] = useState(false);
  const [uiError, setUiError] = useState<string | null>(null);

  // Optional safe import state.
  const [showImportPanel, setShowImportPanel] = useState(false);
  const [importUid, setImportUid] = useState('');
  const [existingDashboards, setExistingDashboards] = useState<ExistingDashboardSummary[]>([]);
  const [importReport, setImportReport] = useState<ImportConversionReport | null>(null);

  // Dry-run validation and explicit Scenes preview state bound to (jobId, revision).
  const [validationResults, setValidationResults] = useState<PanelValidationResult[]>([]);
  const [previewSpec, setPreviewSpec] = useState<NormalizedDashboardSpec | null>(null);
  const [previewScene, setPreviewScene] = useState<EmbeddedScene | null>(null);
  const [previewMeta, setPreviewMeta] = useState<PreviewMeta | null>(null);
  const [rightTab, setRightTab] = useState<'preview' | 'ir' | 'foundation'>('preview');

  // Save state (create-only copy by default; opt-in revision-checked update for imported dashboards).
  const [folders, setFolders] = useState<GrafanaFolderOption[]>(() => [
    { uid: '', title: t('save.folder.generalRoot') },
  ]);
  const [selectedFolderUid, setSelectedFolderUid] = useState<string>('');
  const [optInUpdateExisting, setOptInUpdateExisting] = useState(false);
  const [activeImportPatchKey, setActiveImportPatchKey] = useState<string | null>(null);
  const [savedResult, setSavedResult] = useState<SaveDashboardResult | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const metadataAbortRef = useRef<AbortController | null>(null);
  const inFlightJobIdRef = useRef<string | null>(null);
  const contextSeqRef = useRef<number>(0);
  const jobOpSeqRef = useRef<number>(0);
  const validatedFingerprintsRef = useRef<Map<string, ValidatedPanelCacheEntry>>(new Map());
  const latestImportMetaRef = useRef<Map<string, PreservedImportMeta>>(new Map());
  const saverRef = useRef<DashboardSaver>(new DashboardSaver());
  const prevContextKeyRef = useRef<string>(`${currentOrgId}:${selectedDsUid}`);

  const applyJobSnapshot = useCallback((nextJob: JobSnapshot) => {
    if (nextJob.importSource?.uid) {
      const metaKey = `${nextJob.jobId}:${nextJob.importSource.uid}`;
      const latest = latestImportMetaRef.current.get(metaKey);
      if (latest?.resourceVersion) {
        setJob({
          ...nextJob,
          importSource: {
            ...nextJob.importSource,
            resourceVersion: latest.resourceVersion,
          },
        });
        return;
      }
    }
    setJob(nextJob);
  }, []);

  const clearDraftAndMetadata = useCallback(() => {
    contextSeqRef.current += 1;
    jobOpSeqRef.current += 1;
    abortRef.current?.abort();
    abortRef.current = null;
    metadataAbortRef.current?.abort();
    metadataAbortRef.current = null;
    inFlightJobIdRef.current = null;
    validatedFingerprintsRef.current.clear();
    latestImportMetaRef.current.clear();
    setCatalog([]);
    setCatalogScopeKey('');
    setCatalogTruncated(false);
    setLoadingCatalog(false);
    setScopeLabelValues([]);
    setScopeLabelValue('');
    setJob(null);
    setLiveStage(null);
    setSseFallbackNotice(false);
    setHandoffFromReport(false);
    setShowImportPanel(false);
    setImportUid('');
    setExistingDashboards([]);
    setImportReport(null);
    setSelectedFolderUid('');
    setOptInUpdateExisting(false);
    setActiveImportPatchKey(null);
    setValidationResults([]);
    setPreviewSpec(null);
    setPreviewScene(null);
    setPreviewMeta(null);
    setSavedResult(null);
    setEditPanelId('');
    setUiError(null);
    setBusyState('idle');
  }, []);

  // Reset draft and in-memory metadata on org or datasource switch (H-13).
  useEffect(() => {
    const key = `${currentOrgId}:${selectedDsUid}`;
    if (prevContextKeyRef.current !== key) {
      prevContextKeyRef.current = key;
      clearDraftAndMetadata();
    }
  }, [currentOrgId, selectedDsUid, clearDraftAndMetadata]);

  useEffect(() => {
    return () => {
      contextSeqRef.current += 1;
      jobOpSeqRef.current += 1;
      abortRef.current?.abort();
      metadataAbortRef.current?.abort();
    };
  }, []);

  // Load secret-free public config, allowlisted Prometheus datasources, and writable folders independently.
  useEffect(() => {
    let active = true;
    const initController = new AbortController();
    (async () => {
      try {
        const cfg = await fetchPublicConfig(initController.signal);
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

        // Consume optional handoff query parameters from Operational Status Report ("Investigate in Studio").
        const search = locationService?.getLocation?.()?.search ?? '';
        const params = new URLSearchParams(search);
        const handoffDs = (params.get('datasource') ?? '').trim();
        const handoffScopeLabel = (params.get('scopeLabel') ?? '').trim();
        const handoffScopeValue = (params.get('scopeValue') ?? '').trim().slice(0, 128);
        const handoffIntent = (params.get('intent') ?? '').trim().slice(0, 300);
        const hasHandoff = Boolean(handoffDs || handoffScopeLabel || handoffScopeValue || handoffIntent);

        const resolvedDsUid =
          handoffDs && filtered.some((d) => d.uid === handoffDs)
            ? handoffDs
            : filtered.length > 0
              ? filtered[0].uid
              : '';

        if (hasHandoff) {
          // Update prevContextKeyRef BEFORE setSelectedDsUid so the context-switch effect
          // does not fire clearDraftAndMetadata() and wipe the pre-filled scopeLabelValue.
          prevContextKeyRef.current = `${currentOrgId}:${resolvedDsUid}`;
          setSelectedDsUid(resolvedDsUid);
          if (handoffScopeLabel === 'service' || handoffScopeLabel === 'job' || handoffScopeLabel === 'namespace') {
            setScopeLabelName(handoffScopeLabel);
          }
          if (handoffScopeValue) {
            setScopeLabelValue(handoffScopeValue);
          }
          if (handoffIntent) {
            setPromptText(handoffIntent);
          }
          setHandoffFromReport(true);
          locationService?.partial?.(
            { datasource: null, scopeLabel: null, scopeValue: null, intent: null },
            true
          );
        } else {
          setSelectedDsUid(resolvedDsUid);
        }
      } catch (err: unknown) {
        if (active && !initController.signal.aborted) {
          setUiError(t('studio.errors.initFailed', { error: extractApiError(err) }));
        }
        return;
      }

      // Folder listing is best-effort so Viewer roles without folders:read still work (M-10).
      try {
        const writable = await listWritableFolders();
        if (active) {
          setFolders(writable);
        }
      } catch {
        if (active) {
          setFolders([{ uid: '', title: t('save.folder.generalRoot') }]);
        }
      }
    })();
    return () => {
      active = false;
      initController.abort();
    };
    // `t` is left out on purpose: with an i18n instance override (tests) it is a new
    // function on every render, which would refetch the folders on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentOrgId]);

  const selectedDatasource = useMemo(
    () => allowedDatasources.find((d) => d.uid === selectedDsUid) ?? null,
    [allowedDatasources, selectedDsUid]
  );

  const activeDialect = useMemo(
    () => resolveDatasourceDialect(selectedDatasource, publicCfg?.datasourceDialects),
    [selectedDatasource, publicCfg?.datasourceDialects]
  );

  const folderOptions = useMemo(() => {
    const opts = folders.map((f) => ({
      label: f.uid === '' ? t('save.folder.generalRoot') : f.title,
      value: f.uid,
    }));
    if (selectedFolderUid && !folders.some((f) => f.uid === selectedFolderUid)) {
      opts.push({
        label: t('studio.import.folderFallback', { uid: selectedFolderUid }),
        value: selectedFolderUid,
      });
    }
    return opts;
    // `t` keeps its identity when the language changes; `locale` recomputes the labels.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [folders, selectedFolderUid, t, locale]);

  const currentScopeKey = makeScopeKey(selectedDsUid, scopeLabelName, scopeLabelValue);
  const isCatalogFresh = catalog.length > 0 && catalogScopeKey === currentScopeKey;

  const reconcileEditPanelId = (nextJob: JobSnapshot) => {
    setEditPanelId((prev) => {
      if (prev === '__new__') {
        return '';
      }
      if (prev && nextJob.currentIR && !nextJob.currentIR.panels.some((p) => p.id === prev)) {
        return '';
      }
      return prev;
    });
  };

  const handleLoadScopeValues = async () => {
    if (!selectedDsUid) {
      return;
    }
    metadataAbortRef.current?.abort();
    const controller = new AbortController();
    metadataAbortRef.current = controller;
    const seq = contextSeqRef.current;

    setUiError(null);
    try {
      const vals = await fetchScopeLabelValues(
        selectedDsUid,
        scopeLabelName,
        3600,
        100,
        controller.signal,
        selectedDatasource?.type
      );
      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      setScopeLabelValues(vals);
      if (vals.length > 0 && !scopeLabelValue) {
        setScopeLabelValue(vals[0]);
      }
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(t('studio.errors.scopeValuesFailed', { error: extractApiError(err) }));
      }
    }
  };

  const handleLoadScopedCatalog = async () => {
    if (!selectedDsUid || !scopeLabelValue.trim()) {
      setUiError(t('studio.errors.scopeRequired'));
      return;
    }
    metadataAbortRef.current?.abort();
    const controller = new AbortController();
    metadataAbortRef.current = controller;
    const seq = contextSeqRef.current;
    const targetScopeKey = makeScopeKey(selectedDsUid, scopeLabelName, scopeLabelValue);

    setLoadingCatalog(true);
    setUiError(null);
    try {
      const res = await fetchScopedMetricCatalog({
        datasourceUid: selectedDsUid,
        datasourceType: selectedDatasource?.type,
        scope: { labelName: scopeLabelName, labelValue: scopeLabelValue.trim() },
        signal: controller.signal,
      });
      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      setCatalog(res.items);
      setCatalogScopeKey(targetScopeKey);
      setCatalogTruncated(res.truncated);
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(t('studio.errors.metadataFailed', { error: extractApiError(err) }));
      }
    } finally {
      if (metadataAbortRef.current === controller && seq === contextSeqRef.current) {
        setLoadingCatalog(false);
      }
    }
  };

  const handleGenerateOrPatch = async () => {
    if (!selectedDsUid) {
      setUiError(t('studio.errors.datasourceRequired'));
      return;
    }
    if (!isCatalogFresh) {
      setUiError(t('studio.errors.catalogRequired'));
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const seq = contextSeqRef.current;
    jobOpSeqRef.current += 1;

    const isPatch = Boolean(job?.currentIR && editPanelId !== '__new__');
    inFlightJobIdRef.current = isPatch && job?.jobId ? job.jobId : null;
    setBusyState(isPatch ? 'patching' : 'generating');
    setLiveStage(null);
    setSseFallbackNotice(false);
    setUiError(null);
    setSavedResult(null);

    // Starting a brand-new draft MUST clear previous job's preview state immediately (C-2).
    if (!isPatch) {
      validatedFingerprintsRef.current.clear();
      setPreviewSpec(null);
      setPreviewScene(null);
      setPreviewMeta(null);
      setValidationResults([]);
      setImportReport(null);
      setOptInUpdateExisting(false);
    }

    const narrowedCatalog = selectMetricCandidates(catalog, promptText, {
      maxItems: 40,
      maxTotalChars: 11000,
    });
    const scope: ScopeFilter = {
      labelName: scopeLabelName,
      labelValue: scopeLabelValue.trim(),
    };
    const streamOptions = {
      enableSse: Boolean(publicCfg?.enableSseProgress),
      onStage: (ev: JobStageEvent) => {
        if (controller.signal.aborted || seq !== contextSeqRef.current) {
          return;
        }
        if (ev.jobId) {
          inFlightJobIdRef.current = ev.jobId;
        }
        setLiveStage(ev);
      },
      onFallback: () => {
        if (controller.signal.aborted || seq !== contextSeqRef.current) {
          return;
        }
        setSseFallbackNotice(true);
      },
    };

    try {
      const nextJob =
        isPatch && job?.currentIR
          ? await patchDraft(
              job.jobId,
              {
                baseRevision: job.currentIR.revision,
                targetPanelId: editPanelId || undefined,
                editIntent: promptText,
                catalog: narrowedCatalog,
              },
              controller.signal,
              streamOptions
            )
          : await generateDraft(
              {
                datasourceUid: selectedDsUid,
                queryKind: activeDialect.kind,
                scope,
                userIntent: promptText,
                catalog: narrowedCatalog,
              },
              controller.signal,
              streamOptions
            );

      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      applyJobSnapshot(nextJob);
      reconcileEditPanelId(nextJob);
      // Do NOT clear validationResults on patch: (queryHash, varsFingerprint) matching in
      // handleValidateAndPreview retains results for untouched panels and re-validates changed ones (H-9).
      if (!isPatch) {
        setValidationResults([]);
      }
    } catch (err: unknown) {
      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      syncJobFromError(err);
      setUiError(extractApiError(err));
    } finally {
      if (abortRef.current === controller && seq === contextSeqRef.current) {
        inFlightJobIdRef.current = null;
        setLiveStage(null);
        setBusyState('idle');
      }
    }
  };

  const handleListExistingDashboards = async () => {
    metadataAbortRef.current?.abort();
    const controller = new AbortController();
    metadataAbortRef.current = controller;
    const seq = contextSeqRef.current;

    setUiError(null);
    try {
      const list = await listExistingDashboards(50, controller.signal);
      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      setExistingDashboards(list);
      if (list.length > 0 && !importUid) {
        setImportUid(list[0].uid);
      }
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(t('studio.errors.listDashboardsFailed', { error: extractApiError(err) }));
      }
    }
  };

  const handleImportExistingDashboard = async () => {
    if (!selectedDatasource || !importUid.trim() || busyState !== 'idle') {
      return;
    }
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const seq = contextSeqRef.current;
    jobOpSeqRef.current += 1;

    setBusyState('importing');
    setUiError(null);

    try {
      const fetched = await fetchExistingDashboardByUid(importUid.trim(), controller.signal);
      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      const scope: ScopeFilter | undefined =
        isCatalogFresh && scopeLabelValue.trim()
          ? { labelName: scopeLabelName, labelValue: scopeLabelValue.trim() }
          : undefined;
      const { draft, report, patchContext } = convertGrafanaDashboardToIR(fetched.rawDashboard, {
        selectedDatasourceUid: selectedDatasource.uid,
        selectedDatasourceName: selectedDatasource.name,
        queryKind: activeDialect.kind,
        maxPanels: publicCfg?.maxPanels ?? 6,
        scope,
      });
      const importedJob = await importDraft(
        {
          datasourceUid: selectedDatasource.uid,
          queryKind: activeDialect.kind,
          scope,
          draft,
          importSource: fetched.source,
          catalog: isCatalogFresh ? catalog.slice(0, 50) : undefined,
        },
        controller.signal
      );
      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      // Only reset active preview/validation state once import has succeeded.
      validatedFingerprintsRef.current.clear();
      setPreviewSpec(null);
      setPreviewScene(null);
      setPreviewMeta(null);
      setValidationResults([]);
      setSavedResult(null);
      setOptInUpdateExisting(false);

      if (importedJob.importSource?.uid) {
        const metaKey = `${importedJob.jobId}:${importedJob.importSource.uid}`;
        latestImportMetaRef.current.set(metaKey, {
          resourceVersion: importedJob.importSource.resourceVersion,
          folderUid: importedJob.importSource.folderUid ?? '',
          preservedAnnotations: fetched.preservedAnnotations,
          preservedLabels: fetched.preservedLabels,
          patchContext,
        });
        setActiveImportPatchKey(metaKey);
      } else {
        setActiveImportPatchKey(null);
      }
      applyJobSnapshot(importedJob);
      setImportReport(report);
      if (fetched.source.folderUid !== undefined) {
        setSelectedFolderUid(fetched.source.folderUid);
      }
      reconcileEditPanelId(importedJob);
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(t('studio.errors.importFailed', { error: extractApiError(err) }));
      }
    } finally {
      if (abortRef.current === controller && seq === contextSeqRef.current) {
        setBusyState('idle');
      }
    }
  };

  const handleCancel = async () => {
    const activeState = busyState;
    if (activeState === 'idle' || activeState === 'saving' || activeState === 'cancelling') {
      return;
    }
    const cancelTargetJobId =
      activeState === 'generating'
        ? inFlightJobIdRef.current
        : activeState === 'patching' || activeState === 'repairing'
        ? inFlightJobIdRef.current || job?.jobId || null
        : null;

    abortRef.current?.abort();
    inFlightJobIdRef.current = null;
    setLiveStage(null);

    // Notify Backend cancel when an LLM backend job ID is known (patching, repairing, or SSE-started generating).
    // Keep busyState='cancelling' until cancel settles so a new Patch/Repair cannot be killed by a delayed Cancel (H-3).
    if (cancelTargetJobId) {
      const seq = contextSeqRef.current;
      const opSeq = ++jobOpSeqRef.current;
      const cancelController = new AbortController();
      abortRef.current = cancelController;
      const timer = setTimeout(() => cancelController.abort(), 5000);
      setBusyState('cancelling');
      try {
        const updated = await cancelDraftJob(cancelTargetJobId, cancelController.signal);
        if (seq === contextSeqRef.current && opSeq === jobOpSeqRef.current) {
          applyJobSnapshot(updated);
        }
      } catch {
        // Ignore cancel race errors if job already completed.
      } finally {
        clearTimeout(timer);
        if (
          abortRef.current === cancelController &&
          seq === contextSeqRef.current &&
          opSeq === jobOpSeqRef.current
        ) {
          setBusyState('idle');
        }
      }
      return;
    }
    setBusyState('idle');
  };

  const handleUndo = async () => {
    if (!job?.jobId || busyState !== 'idle') {
      return;
    }
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const seq = contextSeqRef.current;
    jobOpSeqRef.current += 1;

    setBusyState('undoing');
    setUiError(null);
    try {
      const updated = await undoDraft(job.jobId, controller.signal);
      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }
      validatedFingerprintsRef.current.clear();
      setValidationResults([]);
      setPreviewSpec(null);
      setPreviewScene(null);
      setPreviewMeta(null);
      setOptInUpdateExisting(false);
      applyJobSnapshot(updated);
      reconcileEditPanelId(updated);
      setSavedResult(null);
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(extractApiError(err));
      }
    } finally {
      if (abortRef.current === controller && seq === contextSeqRef.current) {
        setBusyState('idle');
      }
    }
  };

  /**
   * Explicit user action: runs bounded Dry-Run validation (5m, maxDataPoints=10, concurrency<=2)
   * ONLY for panels whose (queryHash, variablesFingerprint) changed or were not previously validated cleanly (H-9).
   * Blocks Scenes construction and Save if any panel has a blocking error (C-2).
   */
  const handleValidateAndPreview = async () => {
    if (!job?.currentIR || !selectedDatasource || busyState !== 'idle') {
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const seq = contextSeqRef.current;
    jobOpSeqRef.current += 1;
    const targetJobId = job.jobId;
    const targetRevision = job.currentIR.revision;

    setBusyState('validating');
    setUiError(null);

    try {
      const normalized = normalizeDashboardIR(
        job.currentIR,
        selectedDatasource,
        publicCfg?.maxPanels ?? 6,
        job.scope
      );
      const varsFingerprint = computeVariablesFingerprint(normalized);

      // Compare BOTH queryHash and variablesFingerprint for the SAME jobId so variable
      // edits always invalidate cached panel dry-run results.
      const prevByPanel = new Map<string, PanelValidationResult>();
      for (const r of validationResults) {
        prevByPanel.set(r.panelId, r);
      }

      const panelsNeedingValidation: string[] = [];
      const panelHashMap = new Map<string, string>();
      for (const panel of normalized.panels) {
        const expectedHash = await computePanelQueryHash(panel.targets);
        panelHashMap.set(panel.id, expectedHash);
        const cacheKey = `${targetJobId}:${panel.id}`;
        const cachedEntry = validatedFingerprintsRef.current.get(cacheKey);
        const prevResult = prevByPanel.get(panel.id);
        const cachedClean =
          Boolean(cachedEntry) &&
          Boolean(prevResult) &&
          cachedEntry!.rawQueryHash === expectedHash &&
          cachedEntry!.varsFingerprint === varsFingerprint &&
          prevResult!.queryHash === expectedHash &&
          !BLOCKING_CATEGORIES.has(cachedEntry!.category);
        if (!cachedClean) {
          panelsNeedingValidation.push(panel.id);
        }
      }

      const freshResults =
        panelsNeedingValidation.length > 0
          ? await runDryRunValidation(normalized, {
              maxConcurrency: publicCfg?.maxConcurrentQueries ?? 2,
              timeoutMs: (publicCfg?.validationTimeoutSeconds ?? 10) * 1000,
              from: 'now-5m',
              to: 'now',
              maxDataPoints: 10,
              onlyPanelIds: panelsNeedingValidation,
              signal: controller.signal,
            })
          : [];

      if (controller.signal.aborted || seq !== contextSeqRef.current) {
        return;
      }

      const mergedResults = mergeValidationResults(validationResults, freshResults, normalized);
      // Sync cache entries for the current jobId and current panel set.
      validatedFingerprintsRef.current.clear();
      for (const r of mergedResults) {
        validatedFingerprintsRef.current.set(`${targetJobId}:${r.panelId}`, {
          rawQueryHash: panelHashMap.get(r.panelId) ?? r.queryHash,
          varsFingerprint,
          category: r.category,
        });
      }
      setValidationResults(mergedResults);

      const hasBlockingErrors = mergedResults.some((r) => BLOCKING_CATEGORIES.has(r.category));
      if (hasBlockingErrors) {
        // Do NOT mount Scenes or mark revision as save-ready when Dry-Run failed (C-2).
        setPreviewSpec(null);
        setPreviewScene(null);
        setPreviewMeta(null);
        setUiError(t('studio.errors.validationBlocked'));
        return;
      }

      const scene = buildPreviewScene(normalized, { maxDataPoints: 200 });
      setPreviewSpec(normalized);
      setPreviewScene(scene);
      setPreviewMeta({ jobId: targetJobId, revision: targetRevision });
      setRightTab('preview');
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(t('studio.errors.validationFailed', { error: extractApiError(err) }));
      }
    } finally {
      if (abortRef.current === controller && seq === contextSeqRef.current) {
        setBusyState('idle');
      }
    }
  };

  const syntaxErrors = useMemo(
    () => validationResults.filter((r) => r.category === 'syntax_error'),
    [validationResults]
  );

  const hasBlockingValidationErrors = useMemo(
    () => validationResults.some((r) => BLOCKING_CATEGORIES.has(r.category)),
    [validationResults]
  );

  const syncJobFromError = (err: unknown) => {
    if (typeof err === 'object' && err !== null) {
      const maybeJob = (err as { data?: { job?: JobSnapshot } })?.data?.job;
      if (maybeJob && maybeJob.jobId) {
        applyJobSnapshot(maybeJob);
      }
    }
  };

  const handleRepairSyntaxErrors = async () => {
    if (!job?.currentIR || syntaxErrors.length === 0 || busyState !== 'idle' || !isCatalogFresh) {
      return;
    }
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const seq = contextSeqRef.current;
    jobOpSeqRef.current += 1;

    inFlightJobIdRef.current = job.jobId;
    setBusyState('repairing');
    setLiveStage(null);
    setSseFallbackNotice(false);
    setUiError(null);

    try {
      const narrowedCatalog = selectMetricCandidates(catalog, promptText, {
        maxItems: 40,
        maxTotalChars: 11000,
      });
      const updated = await repairDraft(
        job.jobId,
        {
          baseRevision: job.currentIR.revision,
          results: syntaxErrors,
          catalog: narrowedCatalog,
        },
        controller.signal,
        {
          enableSse: Boolean(publicCfg?.enableSseProgress),
          onStage: (ev: JobStageEvent) => {
            if (controller.signal.aborted || seq !== contextSeqRef.current) {
              return;
            }
            if (ev.jobId) {
              inFlightJobIdRef.current = ev.jobId;
            }
            setLiveStage(ev);
          },
          onFallback: () => {
            if (controller.signal.aborted || seq !== contextSeqRef.current) {
              return;
            }
            setSseFallbackNotice(true);
          },
        }
      );
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        applyJobSnapshot(updated);
        // Clear stale syntax_error results so repaired panels must be re-validated (M-11).
        for (const s of syntaxErrors) {
          validatedFingerprintsRef.current.delete(`${job.jobId}:${s.panelId}`);
        }
        setValidationResults((prev) =>
          prev.filter((r) => !syntaxErrors.some((s) => s.panelId === r.panelId))
        );
      }
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        // Sync updated repairAttemptsUsed from backend error payload (M-11).
        syncJobFromError(err);
        setUiError(extractApiError(err));
      }
    } finally {
      if (abortRef.current === controller && seq === contextSeqRef.current) {
        inFlightJobIdRef.current = null;
        setLiveStage(null);
        setBusyState('idle');
      }
    }
  };

  const activeImportKey = job?.importSource?.uid
    ? `${job.jobId}:${job.importSource.uid}`
    : '';

  const canOptInUpdate = Boolean(
    job?.importSource?.uid &&
      job?.importSource?.resourceVersion &&
      activeImportPatchKey !== null &&
      activeImportPatchKey === activeImportKey
  );

  const handleSaveDashboard = async () => {
    if (!job?.currentIR || !selectedDatasource || !previewSpec || !previewMeta) {
      setUiError(t('studio.errors.previewRequiredBeforeSave'));
      return;
    }
    if (previewMeta.jobId !== job.jobId || previewMeta.revision !== job.currentIR.revision) {
      setUiError(t('studio.errors.latestDraftUnvalidated'));
      return;
    }
    if (hasBlockingValidationErrors) {
      setUiError(t('studio.errors.validationErrorsRemain'));
      return;
    }

    const seq = contextSeqRef.current;
    jobOpSeqRef.current += 1;
    setBusyState('saving');
    setUiError(null);
    setSavedResult(null);
    try {
      // Build spec freshly from current job.currentIR so DashboardSaver verifies real revision match (C-2).
      const currentSpec = normalizeDashboardIR(
        job.currentIR,
        selectedDatasource,
        publicCfg?.maxPanels ?? 6,
        job.scope
      );
      const importMetaKey = job.importSource?.uid
        ? `${job.jobId}:${job.importSource.uid}`
        : '';
      const preservedMeta = importMetaKey
        ? latestImportMetaRef.current.get(importMetaKey)
        : undefined;
      const useUpdate =
        optInUpdateExisting &&
        Boolean(job.importSource?.uid) &&
        Boolean(job.importSource?.resourceVersion) &&
        Boolean(preservedMeta?.patchContext);

      const updateFolderUid = job.importSource?.folderUid ?? preservedMeta?.folderUid ?? '';

      const res = useUpdate
        ? await saverRef.current.updateExistingDashboard({
            spec: currentSpec,
            currentIR: job.currentIR,
            patchContext: preservedMeta!.patchContext!,
            previewedRevision: previewMeta.revision,
            targetUid: job.importSource!.uid,
            expectedResourceVersion:
              preservedMeta?.resourceVersion || job.importSource!.resourceVersion!,
            folderUid: updateFolderUid,
            preservedAnnotations: preservedMeta?.preservedAnnotations,
            preservedLabels: preservedMeta?.preservedLabels,
          })
        : await saverRef.current.createNewDashboard({
            spec: currentSpec,
            previewedRevision: previewMeta.revision,
            folderUid: selectedFolderUid,
          });
      if (seq === contextSeqRef.current) {
        setSavedResult(res);
        setOptInUpdateExisting(false);
        if (useUpdate && res.resourceVersion && importMetaKey) {
          latestImportMetaRef.current.set(importMetaKey, {
            ...preservedMeta,
            resourceVersion: res.resourceVersion,
            folderUid: updateFolderUid,
          });
          setJob((prev) =>
            prev?.importSource
              ? {
                  ...prev,
                  importSource: {
                    ...prev.importSource,
                    resourceVersion: res.resourceVersion,
                  },
                }
              : prev
          );
        }
      }
    } catch (err: unknown) {
      if (seq === contextSeqRef.current) {
        setUiError(extractApiError(err));
      }
    } finally {
      if (seq === contextSeqRef.current) {
        setBusyState('idle');
      }
    }
  };

  const handleExportJson = () => {
    if (!job?.currentIR || !selectedDatasource) {
      return;
    }
    try {
      // Always export the CURRENT IR revision, never a stale previewSpec (M-10).
      const spec = normalizeDashboardIR(
        job.currentIR,
        selectedDatasource,
        publicCfg?.maxPanels ?? 6,
        job.scope
      );
      const json = buildExportableDashboardJson(spec);
      const blob = new Blob([json], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `dashboard-rev-${spec.revision}.json`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (err: unknown) {
      setUiError(t('studio.errors.exportFailed', { error: extractApiError(err) }));
    }
  };

  const currentIR = job?.currentIR;
  const jobScope = job?.scope;
  const maxPanels = publicCfg?.maxPanels ?? 6;
  const foundationJsonPreview = useMemo(() => {
    if (!currentIR || !selectedDatasource) {
      return '';
    }
    try {
      const spec = normalizeDashboardIR(currentIR, selectedDatasource, maxPanels, jobScope);
      return buildExportableDashboardJson(spec);
    } catch {
      return '';
    }
  }, [currentIR, jobScope, selectedDatasource, maxPanels]);

  const canSave =
    Boolean(previewSpec) &&
    Boolean(previewMeta) &&
    Boolean(job?.currentIR) &&
    previewMeta?.jobId === job?.jobId &&
    previewMeta?.revision === job?.currentIR?.revision &&
    !hasBlockingValidationErrors &&
    busyState === 'idle';

  const canCancel =
    busyState === 'generating' ||
    busyState === 'importing' ||
    busyState === 'patching' ||
    busyState === 'validating' ||
    busyState === 'repairing';

  return (
    <div data-testid="studio-page" style={{ display: 'flex', flexDirection: 'column', gap: 12, padding: 16 }}>
      {publicCfg?.mockBanner && (
        <div data-testid="studio-mock-banner">
          <Alert title={t('studio.mockBanner.title')} severity="warning">
            {t('studio.mockBanner.body')}
          </Alert>
        </div>
      )}

      {handoffFromReport && (
        <div data-testid="studio-handoff-alert">
          <Alert
            title={t('studio.progress.handoffNotice')}
            severity="info"
            onRemove={() => setHandoffFromReport(false)}
          />
        </div>
      )}

      {uiError && (
        <div data-testid="studio-error-alert">
          <Alert title={t('studio.alert.errorTitle')} severity="error" onRemove={() => setUiError(null)}>
            {uiError}
          </Alert>
        </div>
      )}

      {savedResult && (
        <div data-testid="studio-saved-alert">
          <Alert
            title={
              savedResult.mode === 'updated'
                ? t('studio.savedAlert.updatedTitle')
                : t('studio.savedAlert.title')
            }
            severity="success"
          >
            {t('studio.savedAlert.uidPrefix')}{' '}
            <code data-testid="studio-saved-uid">{savedResult.uid}</code>{' '}
            {t('studio.savedAlert.revision', { revision: savedResult.revision })} —{' '}
            <a href={savedResult.url} target="_blank" rel="noreferrer">
              {t('studio.savedAlert.openLink')}
            </a>
          </Alert>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: '420px 1fr', gap: 16, alignItems: 'start' }}>
        {/* Left Pane: Scope, Optional Safe Import, Chat, Status, Diff, Validation, Save/Export */}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12, border: '1px solid #2c3235', borderRadius: 6, padding: 12 }}>
          <h4 style={{ margin: 0 }}>{t('studio.title')}</h4>

          <Field label={t('studio.datasource.label')}>
            <Select
              options={allowedDatasources.map((d) => ({ label: `${d.name} (${d.uid})`, value: d.uid }))}
              value={selectedDsUid}
              onChange={(v) => setSelectedDsUid(v.value ?? '')}
              placeholder={t('studio.datasource.placeholder')}
            />
          </Field>

          {selectedDatasource && (
            <div
              data-testid="studio-active-dialect"
              style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, color: '#9fa7b3' }}
            >
              <span>{t('studio.datasource.dialectLabel')}</span>
              <Badge
                text={activeDialect.kind === 'metricsql' ? 'MetricsQL' : 'PromQL'}
                color={activeDialect.kind === 'metricsql' ? 'purple' : 'blue'}
              />
              <span>
                {activeDialect.pinned
                  ? t('studio.datasource.dialectPinned')
                  : t('studio.datasource.dialectAuto')}
              </span>
            </div>
          )}

          <div style={{ display: 'grid', gridTemplateColumns: '120px 1fr', gap: 8 }}>
            <Field label={t('studio.scope.labelName')}>
              <Select
                options={[
                  { label: 'service', value: 'service' },
                  { label: 'job', value: 'job' },
                  { label: 'namespace', value: 'namespace' },
                ]}
                value={scopeLabelName}
                onChange={(v) => setScopeLabelName(v.value ?? 'service')}
              />
            </Field>
            <Field label={t('studio.scope.labelValue')}>
              <Input
                value={scopeLabelValue}
                onChange={(e) => setScopeLabelValue(e.currentTarget.value)}
                placeholder={t('studio.scope.placeholder')}
              />
            </Field>
          </div>

          <div style={{ display: 'flex', gap: 8 }}>
            <Button size="sm" variant="secondary" onClick={handleLoadScopeValues} disabled={!selectedDsUid}>
              {t('studio.scope.fetchValues')}
            </Button>
            <Button
              size="sm"
              variant="secondary"
              onClick={handleLoadScopedCatalog}
              disabled={!selectedDsUid || !scopeLabelValue.trim() || loadingCatalog}
            >
              {loadingCatalog ? t('studio.catalog.fetching') : t('studio.catalog.fetch')}
            </Button>
          </div>

          {scopeLabelValues.length > 0 && (
            <Select
              options={scopeLabelValues.map((v) => ({ label: v, value: v }))}
              value={scopeLabelValue}
              onChange={(v) => setScopeLabelValue(v.value ?? '')}
              placeholder={t('studio.scope.selectValuePlaceholder')}
            />
          )}

          <div style={{ fontSize: 12, color: '#9fa7b3' }}>
            {t('studio.catalog.loadedPrefix')}{' '}
            <strong data-testid="studio-catalog-count">{catalog.length}</strong>{' '}
            {t('studio.catalog.loadedUnit')}
            {catalogTruncated ? ` ${t('studio.catalog.truncatedSuffix')}` : ''}
            {catalog.length > 0 && !isCatalogFresh ? ` ${t('studio.catalog.staleSuffix')}` : ''}
          </div>

          <div style={{ borderTop: '1px solid #2c3235', paddingTop: 8 }}>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setShowImportPanel((prev) => !prev)}
              disabled={!selectedDsUid}
            >
              {showImportPanel ? t('studio.import.closeButton') : t('studio.import.openButton')}
            </Button>
            {showImportPanel && (
              <div
                data-testid="studio-import-section"
                style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 8 }}
              >
                <Field label={t('studio.import.uidLabel')}>
                  <Input
                    value={importUid}
                    onChange={(e) => setImportUid(e.currentTarget.value)}
                    placeholder={t('studio.import.uidPlaceholder')}
                  />
                </Field>
                <div style={{ display: 'flex', gap: 8 }}>
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={handleListExistingDashboards}
                    disabled={busyState !== 'idle'}
                  >
                    {t('studio.import.listButton')}
                  </Button>
                  <Button
                    size="sm"
                    variant="primary"
                    onClick={handleImportExistingDashboard}
                    disabled={!importUid.trim() || !selectedDatasource || busyState !== 'idle'}
                  >
                    {busyState === 'importing'
                      ? t('studio.import.importing')
                      : t('studio.import.importButton')}
                  </Button>
                </div>
                {existingDashboards.length > 0 && (
                  <Select
                    options={existingDashboards.map((d) => ({
                      label: `${d.title} (${d.uid})`,
                      value: d.uid,
                    }))}
                    value={importUid}
                    onChange={(v) => setImportUid(v.value ?? '')}
                    placeholder={t('studio.import.selectPlaceholder')}
                  />
                )}
              </div>
            )}
          </div>

          {importReport && (
            <div
              data-testid="studio-import-report"
              style={{ fontSize: 12, border: '1px solid #2c3235', borderRadius: 4, padding: 8 }}
            >
              <strong>{t('studio.import.reportTitle')}</strong>{' '}
              {t('studio.import.reportCounts', {
                panels: importReport.importedPanelCount,
                variables: importReport.importedVariableCount,
              })}
              {importReport.skippedPanels.length > 0 && (
                <div>
                  {t('studio.import.skippedPanels', {
                    items: importReport.skippedPanels.map((s) => `${s.title} (${s.reason})`).join(', '),
                  })}
                </div>
              )}
              {importReport.skippedVariables.length > 0 && (
                <div>
                  {t('studio.import.skippedVariables', {
                    items: importReport.skippedVariables.map((s) => `${s.title} (${s.reason})`).join(', '),
                  })}
                </div>
              )}
              {importReport.warnings.length > 0 && (
                <div style={{ color: '#ff9830' }}>{importReport.warnings.join(' / ')}</div>
              )}
            </div>
          )}

          {job?.currentIR && (
            <Field label={t('studio.edit.label')}>
              <Select
                options={[
                  { label: t('studio.edit.autoPatch'), value: '' },
                  ...job.currentIR.panels.map((p) => ({
                    label: t('studio.edit.panelPatch', { title: p.title, id: p.id }),
                    value: p.id,
                  })),
                  { label: t('studio.edit.newDraft'), value: '__new__' },
                ]}
                value={editPanelId}
                onChange={(v) => setEditPanelId(v.value ?? '')}
              />
            </Field>
          )}

          <Field label={t('studio.prompt.label')}>
            <TextArea
              rows={3}
              value={promptText}
              onChange={(e) => setPromptText(e.currentTarget.value)}
              placeholder={t('studio.prompt.placeholder')}
            />
          </Field>

          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <Button
              onClick={handleGenerateOrPatch}
              disabled={busyState !== 'idle' || !isCatalogFresh}
            >
              {busyState === 'generating' || busyState === 'patching'
                ? t('studio.actions.generating')
                : job?.currentIR && editPanelId !== '__new__'
                ? t('studio.actions.applyPatch')
                : t('studio.actions.generateDraft')}
            </Button>
            {canCancel && (
              <Button variant="destructive" size="sm" onClick={handleCancel}>
                {t('studio.actions.cancel')}
              </Button>
            )}
            {job?.canUndo && busyState === 'idle' && (
              <Button variant="secondary" size="sm" onClick={handleUndo}>
                {t('studio.actions.undo')}
              </Button>
            )}
          </div>

          {liveStage && (busyState === 'generating' || busyState === 'patching' || busyState === 'repairing') && (
            <div
              data-testid="studio-sse-progress"
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                fontSize: 12,
                padding: '6px 8px',
                border: '1px solid #2c3235',
                borderRadius: 4,
              }}
            >
              <Spinner inline />
              <span>{t('studio.progress.label')}</span>
              <Badge text={liveStage.stage} color="blue" />
              <span style={{ color: '#9fa7b3' }}>
                {Object.prototype.hasOwnProperty.call(STAGE_LABEL_KEYS, liveStage.stage)
                  ? t(STAGE_LABEL_KEYS[liveStage.stage])
                  : liveStage.stage}
              </span>
            </div>
          )}

          {sseFallbackNotice && (
            <div data-testid="studio-sse-fallback-notice" style={{ fontSize: 11, color: '#9fa7b3' }}>
              {t('studio.progress.fallbackNotice')}
            </div>
          )}

          {job && (
            <div
              data-testid="studio-job-status"
              style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span>
                  {t('studio.job.statusLabel')}{' '}
                  <Badge text={job.status} color={job.status === 'failed' ? 'red' : 'blue'} />
                </span>
                {job.currentIR && (
                  <span data-testid="studio-job-revision" style={{ fontSize: 12 }}>
                    {t('studio.job.revision', { revision: job.currentIR.revision })}
                  </span>
                )}
              </div>
              {job.currentIR?.variables && job.currentIR.variables.length > 0 && (
                <div data-testid="studio-ir-variables" style={{ fontSize: 12, color: '#9fa7b3' }}>
                  {t('studio.job.variablesLabel')}{' '}
                  {job.currentIR.variables.map((v) => `$${v.name} (${v.type})`).join(', ')}
                </div>
              )}
              {job.summary && <div style={{ fontSize: 13 }}>{job.summary}</div>}
              {job.warnings && job.warnings.length > 0 && (
                <div data-testid="studio-job-warnings" style={{ fontSize: 12, color: '#ff9830' }}>
                  <strong>{t('studio.job.warningsLabel')}</strong> {job.warnings.join(' / ')}
                </div>
              )}
              {job.errorMessage && (
                <Alert title={t('studio.job.messageTitle')} severity="warning">
                  {job.errorMessage}
                </Alert>
              )}
              {job.lastDiff && job.lastDiff.length > 0 && (
                <div data-testid="studio-job-diff" style={{ fontSize: 12 }}>
                  <strong>{t('studio.job.diffTitle')}</strong>
                  <ul style={{ margin: '4px 0', paddingLeft: 18 }}>
                    {job.lastDiff.map((d, i) => (
                      <li key={i}>
                        <code>{d.panelId ?? 'dashboard'}</code>.{d.field}: {d.before ? `${d.before} → ` : ''}
                        <strong>{d.after}</strong>
                      </li>
                    ))}
                  </ul>
                </div>
              )}
              <div style={{ fontSize: 12, color: '#9fa7b3' }}>
                {t('studio.job.repairBudget', {
                  used: job.repairAttemptsUsed,
                  max: job.maxRepairAttempts,
                })}
              </div>
            </div>
          )}

          {validationResults.length > 0 && (
            <div
              data-testid="studio-validation-results"
              style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}
            >
              <strong>{t('studio.validation.heading')}</strong>
              {validationResults.map((r) => (
                <div
                  key={r.panelId}
                  data-testid={`studio-validation-row-${r.panelId}`}
                  style={{ fontSize: 12, display: 'flex', flexDirection: 'column', gap: 2 }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between' }}>
                    <code>{r.panelId}</code>
                    <Badge text={t(VALIDATION_LABEL_KEYS[r.category])} color={VALIDATION_BADGE_COLOR[r.category]} />
                  </div>
                  {r.errorText && <div style={{ color: '#ff7383' }}>{r.errorText}</div>}
                </div>
              ))}
              {syntaxErrors.length > 0 &&
                job &&
                job.repairAttemptsUsed < job.maxRepairAttempts && (
                  <>
                    <Button
                      size="sm"
                      variant="secondary"
                      onClick={handleRepairSyntaxErrors}
                      disabled={busyState !== 'idle' || !isCatalogFresh}
                    >
                      {busyState === 'repairing'
                        ? t('studio.validation.repairing')
                        : t('studio.validation.repairSyntaxOnly')}
                    </Button>
                    {!isCatalogFresh && (
                      <div style={{ fontSize: 11, color: '#9fa7b3' }}>
                        {t('studio.validation.repairCatalogRequiredHint')}
                      </div>
                    )}
                  </>
                )}
            </div>
          )}

          {job?.currentIR && (
            <div style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 8 }}>
              <Field
                label={
                  optInUpdateExisting && canOptInUpdate
                    ? t('studio.save.folderLabelUpdateLocked')
                    : t('studio.save.folderLabel')
                }
              >
                <Select
                  options={folderOptions}
                  value={
                    optInUpdateExisting && canOptInUpdate
                      ? job.importSource?.folderUid ?? ''
                      : selectedFolderUid
                  }
                  onChange={(v) => setSelectedFolderUid(v.value ?? '')}
                  disabled={optInUpdateExisting && canOptInUpdate}
                />
              </Field>
              {canOptInUpdate && (
                <label
                  data-testid="studio-optin-update-label"
                  style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, cursor: 'pointer' }}
                >
                  <input
                    data-testid="studio-optin-update-checkbox"
                    type="checkbox"
                    checked={optInUpdateExisting}
                    onChange={(e) => setOptInUpdateExisting(e.currentTarget.checked)}
                  />
                  <span>
                    {t('studio.save.optInUpdateLabel', {
                      uid: job.importSource?.uid ?? '',
                      rv: job.importSource?.resourceVersion ?? '',
                    })}
                  </span>
                </label>
              )}
              <div style={{ display: 'flex', gap: 8 }}>
                <Button onClick={handleSaveDashboard} disabled={!canSave}>
                  {busyState === 'saving'
                    ? t('studio.save.saving')
                    : optInUpdateExisting && canOptInUpdate
                    ? t('studio.save.updateExistingDashboard')
                    : t('studio.save.saveNewDashboard')}
                </Button>
                <Button variant="secondary" onClick={handleExportJson} disabled={busyState !== 'idle'}>
                  {t('studio.save.exportJson')}
                </Button>
              </div>
              {!canSave && job.currentIR && (
                <div style={{ fontSize: 12, color: '#9fa7b3' }}>
                  {t('studio.save.requiresValidationHint', { revision: job.currentIR.revision })}
                </div>
              )}
            </div>
          )}
        </div>

        {/* Right Pane: Explicit Validation Trigger + Mounted Scenes Preview / IR / Foundation JSON */}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12, border: '1px solid #2c3235', borderRadius: 6, padding: 12, minHeight: 540 }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <div style={{ display: 'flex', gap: 8 }}>
              <Button
                size="sm"
                variant={rightTab === 'preview' ? 'primary' : 'secondary'}
                onClick={() => setRightTab('preview')}
              >
                {t('studio.rightPane.tabPreview')}
              </Button>
              <Button
                size="sm"
                variant={rightTab === 'ir' ? 'primary' : 'secondary'}
                onClick={() => setRightTab('ir')}
              >
                {t('studio.rightPane.tabIr')}
              </Button>
              <Button
                size="sm"
                variant={rightTab === 'foundation' ? 'primary' : 'secondary'}
                onClick={() => setRightTab('foundation')}
              >
                {t('studio.rightPane.tabFoundation')}
              </Button>
            </div>

            <Button
              variant="primary"
              onClick={handleValidateAndPreview}
              disabled={!job?.currentIR || !selectedDatasource || busyState !== 'idle'}
            >
              {busyState === 'validating' ? (
                <>
                  <Spinner inline /> {t('studio.rightPane.validating')}
                </>
              ) : (
                t('studio.rightPane.validateAndPreview')
              )}
            </Button>
          </div>

          {/* Keep Scenes container mounted via CSS display so tab switching NEVER unmounts/re-activates SceneQueryRunner (H-8). */}
          <div
            data-testid="studio-preview-pane"
            style={{ display: rightTab === 'preview' ? 'block' : 'none', minHeight: 460 }}
          >
            {previewScene ? (
              <previewScene.Component model={previewScene} />
            ) : (
              <div style={{ padding: 32, textAlign: 'center', color: '#9fa7b3' }}>
                {job?.currentIR
                  ? t('studio.rightPane.draftReadyHint', { revision: job.currentIR.revision })
                  : t('studio.rightPane.emptyHint')}
              </div>
            )}
          </div>

          {rightTab === 'ir' && (
            <pre
              data-testid="studio-ir-json"
              style={{ margin: 0, padding: 12, background: '#111217', borderRadius: 4, overflow: 'auto', maxHeight: 520, fontSize: 12 }}
            >
              {job?.currentIR ? JSON.stringify(job.currentIR, null, 2) : t('studio.rightPane.noIrYet')}
            </pre>
          )}

          {rightTab === 'foundation' && (
            <pre
              data-testid="studio-foundation-json"
              style={{ margin: 0, padding: 12, background: '#111217', borderRadius: 4, overflow: 'auto', maxHeight: 520, fontSize: 12 }}
            >
              {foundationJsonPreview || t('studio.rightPane.noFoundationYet')}
            </pre>
          )}
        </div>
      </div>
    </div>
  );
};

function mergeValidationResults(
  prev: PanelValidationResult[],
  updated: PanelValidationResult[],
  spec: NormalizedDashboardSpec
): PanelValidationResult[] {
  const byId = new Map<string, PanelValidationResult>();
  for (const r of prev) {
    byId.set(r.panelId, r);
  }
  for (const r of updated) {
    byId.set(r.panelId, r);
  }
  return spec.panels
    .map((p) => byId.get(p.id))
    .filter((r): r is PanelValidationResult => Boolean(r));
}
