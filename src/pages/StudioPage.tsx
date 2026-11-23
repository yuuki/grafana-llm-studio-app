import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { config, getDataSourceSrv } from '@grafana/runtime';
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
import {
  convertGrafanaDashboardToIR,
  ExistingDashboardSummary,
  fetchExistingDashboardByUid,
  ImportConversionReport,
  listExistingDashboards,
} from '../import/importDashboard';
import {
  computePanelQueryHash,
  JobSnapshot,
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
}

const VALIDATION_BADGE_COLOR: Record<ValidationCategory, 'green' | 'orange' | 'red' | 'blue'> = {
  ok: 'green',
  empty_result: 'blue',
  syntax_error: 'red',
  permission_error: 'red',
  timeout: 'orange',
  datasource_error: 'red',
};

const VALIDATION_LABEL: Record<ValidationCategory, string> = {
  ok: '正常',
  empty_result: '空結果（自動修復対象外）',
  syntax_error: '構文エラー（修復可）',
  permission_error: '権限エラー（自動修復不可）',
  timeout: 'タイムアウト（自動修復不可）',
  datasource_error: 'データソースエラー',
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
  const [promptText, setPromptText] = useState(
    '選択したサービスのリクエスト数、5xxエラー率、p95レイテンシを見たい'
  );
  const [editPanelId, setEditPanelId] = useState<string>('');
  const [job, setJob] = useState<JobSnapshot | null>(null);
  const [busyState, setBusyState] = useState<
    'idle' | 'generating' | 'importing' | 'patching' | 'validating' | 'repairing' | 'undoing' | 'saving'
  >('idle');
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
  const [folders, setFolders] = useState<GrafanaFolderOption[]>([{ uid: '', title: 'General (Root)' }]);
  const [selectedFolderUid, setSelectedFolderUid] = useState<string>('');
  const [optInUpdateExisting, setOptInUpdateExisting] = useState(false);
  const [savedResult, setSavedResult] = useState<SaveDashboardResult | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const metadataAbortRef = useRef<AbortController | null>(null);
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
    validatedFingerprintsRef.current.clear();
    latestImportMetaRef.current.clear();
    setCatalog([]);
    setCatalogScopeKey('');
    setCatalogTruncated(false);
    setLoadingCatalog(false);
    setScopeLabelValues([]);
    setScopeLabelValue('');
    setJob(null);
    setShowImportPanel(false);
    setImportUid('');
    setExistingDashboards([]);
    setImportReport(null);
    setSelectedFolderUid('');
    setOptInUpdateExisting(false);
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
        setSelectedDsUid(filtered.length > 0 ? filtered[0].uid : '');
      } catch (err: unknown) {
        if (active && !initController.signal.aborted) {
          setUiError(
            `初期化に失敗しました（App Config で許可データソースと LLM モードを確認してください）: ${extractApiError(
              err
            )}`
          );
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
          setFolders([{ uid: '', title: 'General (Root)' }]);
        }
      }
    })();
    return () => {
      active = false;
      initController.abort();
    };
  }, [currentOrgId]);

  const selectedDatasource = useMemo(
    () => allowedDatasources.find((d) => d.uid === selectedDsUid) ?? null,
    [allowedDatasources, selectedDsUid]
  );

  const folderOptions = useMemo(() => {
    const opts = folders.map((f) => ({ label: f.title, value: f.uid }));
    if (selectedFolderUid && !folders.some((f) => f.uid === selectedFolderUid)) {
      opts.push({ label: `Imported Folder (${selectedFolderUid})`, value: selectedFolderUid });
    }
    return opts;
  }, [folders, selectedFolderUid]);

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
        controller.signal
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
        setUiError(`スコープ値取得エラー: ${extractApiError(err)}`);
      }
    }
  };

  const handleLoadScopedCatalog = async () => {
    if (!selectedDsUid || !scopeLabelValue.trim()) {
      setUiError('データソースと対象スコープ値（例: checkout-api）を指定してください。');
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
        setUiError(`メタデータ取得エラー: ${extractApiError(err)}`);
      }
    } finally {
      if (metadataAbortRef.current === controller && seq === contextSeqRef.current) {
        setLoadingCatalog(false);
      }
    }
  };

  const handleGenerateOrPatch = async () => {
    if (!selectedDsUid) {
      setUiError('許可された Prometheus データソースを選択してください。');
      return;
    }
    if (!isCatalogFresh) {
      setUiError(
        '現在のスコープに対応するメトリクス候補を取得してください（「スコープのメトリクスを取得」を実行）。'
      );
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const seq = contextSeqRef.current;
    jobOpSeqRef.current += 1;

    const isPatch = Boolean(job?.currentIR && editPanelId !== '__new__');
    setBusyState(isPatch ? 'patching' : 'generating');
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
              controller.signal
            )
          : await generateDraft(
              {
                datasourceUid: selectedDsUid,
                scope,
                userIntent: promptText,
                catalog: narrowedCatalog,
              },
              controller.signal
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
        setUiError(`ダッシュボード一覧取得エラー: ${extractApiError(err)}`);
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
      const { draft, report } = convertGrafanaDashboardToIR(fetched.rawDashboard, {
        selectedDatasourceUid: selectedDatasource.uid,
        selectedDatasourceName: selectedDatasource.name,
        maxPanels: publicCfg?.maxPanels ?? 6,
      });
      const scope: ScopeFilter | undefined =
        isCatalogFresh && scopeLabelValue.trim()
          ? { labelName: scopeLabelName, labelValue: scopeLabelValue.trim() }
          : undefined;
      const importedJob = await importDraft(
        {
          datasourceUid: selectedDatasource.uid,
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
        latestImportMetaRef.current.set(
          `${importedJob.jobId}:${importedJob.importSource.uid}`,
          {
            resourceVersion: importedJob.importSource.resourceVersion,
            folderUid: importedJob.importSource.folderUid ?? '',
            preservedAnnotations: fetched.preservedAnnotations,
            preservedLabels: fetched.preservedLabels,
          }
        );
      }
      applyJobSnapshot(importedJob);
      setImportReport(report);
      if (fetched.source.folderUid !== undefined) {
        setSelectedFolderUid(fetched.source.folderUid);
      }
      reconcileEditPanelId(importedJob);
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(`既存ダッシュボードのインポートに失敗しました: ${extractApiError(err)}`);
      }
    } finally {
      if (abortRef.current === controller && seq === contextSeqRef.current) {
        setBusyState('idle');
      }
    }
  };

  const handleCancel = async () => {
    const activeState = busyState;
    if (activeState === 'idle' || activeState === 'saving') {
      return;
    }
    abortRef.current?.abort();
    setBusyState('idle');

    // Only notify Backend cancel when an LLM backend operation on an existing job was in-flight (M-11).
    if (
      (activeState === 'patching' || activeState === 'repairing') &&
      job?.jobId
    ) {
      const seq = contextSeqRef.current;
      const opSeq = ++jobOpSeqRef.current;
      try {
        const updated = await cancelDraftJob(job.jobId);
        if (seq === contextSeqRef.current && opSeq === jobOpSeqRef.current) {
          applyJobSnapshot(updated);
        }
      } catch {
        // Ignore cancel race errors if job already completed.
      }
    }
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
    const targetJobId = job.jobId;
    const targetRevision = job.currentIR.revision;

    setBusyState('validating');
    setUiError(null);

    try {
      const normalized = normalizeDashboardIR(
        job.currentIR,
        selectedDatasource,
        publicCfg?.maxPanels ?? 6
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
        setUiError(
          'Dry-Run 検証でエラー（構文エラー・権限エラー・タイムアウト等）が検出されたため、Scenes プレビューと保存をブロックしました。'
        );
        return;
      }

      const scene = buildPreviewScene(normalized, { maxDataPoints: 200 });
      setPreviewSpec(normalized);
      setPreviewScene(scene);
      setPreviewMeta({ jobId: targetJobId, revision: targetRevision });
      setRightTab('preview');
    } catch (err: unknown) {
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        setUiError(`検証・プレビューに失敗しました: ${extractApiError(err)}`);
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

    setBusyState('repairing');
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
        controller.signal
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
        setBusyState('idle');
      }
    }
  };

  const canOptInUpdate = Boolean(
    job?.importSource?.uid && job?.importSource?.resourceVersion
  );

  const handleSaveDashboard = async () => {
    if (!job?.currentIR || !selectedDatasource || !previewSpec || !previewMeta) {
      setUiError('保存前に「検証・プレビューを実行」して最新ドラフトを確認してください。');
      return;
    }
    if (previewMeta.jobId !== job.jobId || previewMeta.revision !== job.currentIR.revision) {
      setUiError('最新ドラフトが未検証です。先に「検証・プレビューを実行」を完了してください。');
      return;
    }
    if (hasBlockingValidationErrors) {
      setUiError('検証エラーが残っているドラフトは保存できません。');
      return;
    }

    const seq = contextSeqRef.current;
    setBusyState('saving');
    setUiError(null);
    setSavedResult(null);
    try {
      // Build spec freshly from current job.currentIR so DashboardSaver verifies real revision match (C-2).
      const currentSpec = normalizeDashboardIR(
        job.currentIR,
        selectedDatasource,
        publicCfg?.maxPanels ?? 6
      );
      const useUpdate =
        optInUpdateExisting &&
        Boolean(job.importSource?.uid) &&
        Boolean(job.importSource?.resourceVersion);

      const importMetaKey = job.importSource?.uid
        ? `${job.jobId}:${job.importSource.uid}`
        : '';
      const preservedMeta = importMetaKey
        ? latestImportMetaRef.current.get(importMetaKey)
        : undefined;
      const updateFolderUid = job.importSource?.folderUid ?? preservedMeta?.folderUid ?? '';

      const res = useUpdate
        ? await saverRef.current.updateExistingDashboard({
            spec: currentSpec,
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
        publicCfg?.maxPanels ?? 6
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
      setUiError(`JSON エクスポートに失敗しました: ${extractApiError(err)}`);
    }
  };

  const foundationJsonPreview = useMemo(() => {
    if (!job?.currentIR || !selectedDatasource) {
      return '';
    }
    try {
      const spec = normalizeDashboardIR(
        job.currentIR,
        selectedDatasource,
        publicCfg?.maxPanels ?? 6
      );
      return buildExportableDashboardJson(spec);
    } catch {
      return '';
    }
  }, [job?.currentIR, selectedDatasource, publicCfg?.maxPanels]);

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
          <Alert title="開発・テスト用モックLLMモード" severity="warning">
            現在 llmMode=mock で動作しています。本番利用時は App Config で grafana-llm-app モードへ切り替えてください。
          </Alert>
        </div>
      )}

      {uiError && (
        <div data-testid="studio-error-alert">
          <Alert title="エラー" severity="error" onRemove={() => setUiError(null)}>
            {uiError}
          </Alert>
        </div>
      )}

      {savedResult && (
        <div data-testid="studio-saved-alert">
          <Alert
            title={
              savedResult.mode === 'updated'
                ? '既存ダッシュボードを更新しました（revision 検証済み）'
                : 'ダッシュボードを新規保存しました'
            }
            severity="success"
          >
            UID: <code data-testid="studio-saved-uid">{savedResult.uid}</code> (rev {savedResult.revision}) —{' '}
            <a href={savedResult.url} target="_blank" rel="noreferrer">
              保存したダッシュボードを開いてクエリ・単位・閾値・配置を確認
            </a>
          </Alert>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: '420px 1fr', gap: 16, alignItems: 'start' }}>
        {/* Left Pane: Scope, Optional Safe Import, Chat, Status, Diff, Validation, Save/Export */}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12, border: '1px solid #2c3235', borderRadius: 6, padding: 12 }}>
          <h4 style={{ margin: 0 }}>LLM Dashboard Studio (MVP)</h4>

          <Field label="許可済み Prometheus データソース">
            <Select
              options={allowedDatasources.map((d) => ({ label: `${d.name} (${d.uid})`, value: d.uid }))}
              value={selectedDsUid}
              onChange={(v) => setSelectedDsUid(v.value ?? '')}
              placeholder="データソースを選択"
            />
          </Field>

          <div style={{ display: 'grid', gridTemplateColumns: '120px 1fr', gap: 8 }}>
            <Field label="スコープラベル">
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
            <Field label="対象サービス / ジョブ値">
              <Input
                value={scopeLabelValue}
                onChange={(e) => setScopeLabelValue(e.currentTarget.value)}
                placeholder="checkout-api"
              />
            </Field>
          </div>

          <div style={{ display: 'flex', gap: 8 }}>
            <Button size="sm" variant="secondary" onClick={handleLoadScopeValues} disabled={!selectedDsUid}>
              値候補を取得
            </Button>
            <Button
              size="sm"
              variant="secondary"
              onClick={handleLoadScopedCatalog}
              disabled={!selectedDsUid || !scopeLabelValue.trim() || loadingCatalog}
            >
              {loadingCatalog ? '取得中...' : 'スコープのメトリクスを取得'}
            </Button>
          </div>

          {scopeLabelValues.length > 0 && (
            <Select
              options={scopeLabelValues.map((v) => ({ label: v, value: v }))}
              value={scopeLabelValue}
              onChange={(v) => setScopeLabelValue(v.value ?? '')}
              placeholder="候補から選択"
            />
          )}

          <div style={{ fontSize: 12, color: '#9fa7b3' }}>
            読込済みメトリクス候補（ブラウザメモリのみ）:{' '}
            <strong data-testid="studio-catalog-count">{catalog.length}</strong> 件
            {catalogTruncated ? '（上限で打ち切り）' : ''}
            {catalog.length > 0 && !isCatalogFresh ? '（スコープ変更済み・再取得が必要）' : ''}
          </div>

          <div style={{ borderTop: '1px solid #2c3235', paddingTop: 8 }}>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setShowImportPanel((prev) => !prev)}
              disabled={!selectedDsUid}
            >
              {showImportPanel ? '既存インポートを閉じる' : '既存ダッシュボードをインポート（任意）'}
            </Button>
            {showImportPanel && (
              <div
                data-testid="studio-import-section"
                style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 8 }}
              >
                <Field label="既存ダッシュボード UID">
                  <Input
                    value={importUid}
                    onChange={(e) => setImportUid(e.currentTarget.value)}
                    placeholder="existing-dashboard-uid"
                  />
                </Field>
                <div style={{ display: 'flex', gap: 8 }}>
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={handleListExistingDashboards}
                    disabled={busyState !== 'idle'}
                  >
                    一覧取得
                  </Button>
                  <Button
                    size="sm"
                    variant="primary"
                    onClick={handleImportExistingDashboard}
                    disabled={!importUid.trim() || !selectedDatasource || busyState !== 'idle'}
                  >
                    {busyState === 'importing' ? 'インポート中...' : '安全な IR としてインポート'}
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
                    placeholder="既存ダッシュボードを選択"
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
              <strong>インポート結果:</strong> パネル {importReport.importedPanelCount} 件 / 変数{' '}
              {importReport.importedVariableCount} 件
              {importReport.skippedPanels.length > 0 && (
                <div>スキップしたパネル: {importReport.skippedPanels.map((s) => `${s.title} (${s.reason})`).join(', ')}</div>
              )}
              {importReport.skippedVariables.length > 0 && (
                <div>スキップした変数: {importReport.skippedVariables.map((s) => `${s.title} (${s.reason})`).join(', ')}</div>
              )}
              {importReport.warnings.length > 0 && (
                <div style={{ color: '#ff9830' }}>{importReport.warnings.join(' / ')}</div>
              )}
            </div>
          )}

          {job?.currentIR && (
            <Field label="編集モード / 対象パネル">
              <Select
                options={[
                  { label: '既存ドラフトを部分修正（自動判定）', value: '' },
                  ...job.currentIR.panels.map((p) => ({
                    label: `部分修正: ${p.title} (${p.id})`,
                    value: p.id,
                  })),
                  { label: '新規ドラフトとして最初から生成', value: '__new__' },
                ]}
                value={editPanelId}
                onChange={(v) => setEditPanelId(v.value ?? '')}
              />
            </Field>
          )}

          <Field label="作成・修正したい内容">
            <TextArea
              rows={3}
              value={promptText}
              onChange={(e) => setPromptText(e.currentTarget.value)}
              placeholder="例: エラー率のパネルだけ timeseries に変更して"
            />
          </Field>

          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <Button
              onClick={handleGenerateOrPatch}
              disabled={busyState !== 'idle' || !isCatalogFresh}
            >
              {busyState === 'generating' || busyState === 'patching'
                ? '生成中...'
                : job?.currentIR && editPanelId !== '__new__'
                ? '部分修正を適用'
                : 'ダッシュボード案を生成'}
            </Button>
            {canCancel && (
              <Button variant="destructive" size="sm" onClick={handleCancel}>
                キャンセル
              </Button>
            )}
            {job?.canUndo && busyState === 'idle' && (
              <Button variant="secondary" size="sm" onClick={handleUndo}>
                取り消し (Undo)
              </Button>
            )}
          </div>

          {job && (
            <div
              data-testid="studio-job-status"
              style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span>
                  状態: <Badge text={job.status} color={job.status === 'failed' ? 'red' : 'blue'} />
                </span>
                {job.currentIR && (
                  <span data-testid="studio-job-revision" style={{ fontSize: 12 }}>
                    rev {job.currentIR.revision}
                  </span>
                )}
              </div>
              {job.currentIR?.variables && job.currentIR.variables.length > 0 && (
                <div data-testid="studio-ir-variables" style={{ fontSize: 12, color: '#9fa7b3' }}>
                  変数:{' '}
                  {job.currentIR.variables.map((v) => `$${v.name} (${v.type})`).join(', ')}
                </div>
              )}
              {job.summary && <div style={{ fontSize: 13 }}>{job.summary}</div>}
              {job.warnings && job.warnings.length > 0 && (
                <div data-testid="studio-job-warnings" style={{ fontSize: 12, color: '#ff9830' }}>
                  <strong>警告:</strong> {job.warnings.join(' / ')}
                </div>
              )}
              {job.errorMessage && (
                <Alert title="生成・検査メッセージ" severity="warning">
                  {job.errorMessage}
                </Alert>
              )}
              {job.lastDiff && job.lastDiff.length > 0 && (
                <div data-testid="studio-job-diff" style={{ fontSize: 12 }}>
                  <strong>変更内容 (Diff):</strong>
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
                修復予算: {job.repairAttemptsUsed} / {job.maxRepairAttempts} 回使用
              </div>
            </div>
          )}

          {validationResults.length > 0 && (
            <div
              data-testid="studio-validation-results"
              style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}
            >
              <strong>検証結果（Dry-Run: 直近5m / maxDataPoints=10）</strong>
              {validationResults.map((r) => (
                <div
                  key={r.panelId}
                  data-testid={`studio-validation-row-${r.panelId}`}
                  style={{ fontSize: 12, display: 'flex', flexDirection: 'column', gap: 2 }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between' }}>
                    <code>{r.panelId}</code>
                    <Badge text={VALIDATION_LABEL[r.category]} color={VALIDATION_BADGE_COLOR[r.category]} />
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
                      {busyState === 'repairing' ? '修復中...' : '構文エラーのみ自動修復'}
                    </Button>
                    {!isCatalogFresh && (
                      <div style={{ fontSize: 11, color: '#9fa7b3' }}>
                        自動修復を実行するには先に「スコープのメトリクスを取得」を実行してください。
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
                    ? '保存先フォルダ（既存更新時は元のフォルダを維持）'
                    : '保存先フォルダ（新規作成のみ）'
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
                    インポート元 (<code>{job.importSource?.uid}</code>, rv=
                    <code>{job.importSource?.resourceVersion}</code>) を revision 検証付きで更新（任意）
                  </span>
                </label>
              )}
              <div style={{ display: 'flex', gap: 8 }}>
                <Button onClick={handleSaveDashboard} disabled={!canSave}>
                  {busyState === 'saving'
                    ? '保存中...'
                    : optInUpdateExisting && canOptInUpdate
                    ? '既存ダッシュボードを更新'
                    : '新規ダッシュボード保存'}
                </Button>
                <Button variant="secondary" onClick={handleExportJson} disabled={busyState !== 'idle'}>
                  JSONエクスポート
                </Button>
              </div>
              {!canSave && job.currentIR && (
                <div style={{ fontSize: 12, color: '#9fa7b3' }}>
                  最新リビジョン (rev {job.currentIR.revision}) の「検証・プレビューを実行」がエラーなしで完了すると保存できます。
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
                Scenes プレビュー
              </Button>
              <Button
                size="sm"
                variant={rightTab === 'ir' ? 'primary' : 'secondary'}
                onClick={() => setRightTab('ir')}
              >
                IR JSON
              </Button>
              <Button
                size="sm"
                variant={rightTab === 'foundation' ? 'primary' : 'secondary'}
                onClick={() => setRightTab('foundation')}
              >
                Foundation SDK JSON
              </Button>
            </div>

            <Button
              variant="primary"
              onClick={handleValidateAndPreview}
              disabled={!job?.currentIR || !selectedDatasource || busyState !== 'idle'}
            >
              {busyState === 'validating' ? (
                <>
                  <Spinner inline /> 検証中...
                </>
              ) : (
                '検証・プレビューを実行'
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
                  ? `ドラフト rev ${job.currentIR.revision} の IR 検査が完了しています。「検証・プレビューを実行」を押すとクエリ検証と Scenes 描画を行います（自動更新は無効です）。`
                  : '左ペインでデータソースとスコープのメトリクスを読み込み、ダッシュボード案を生成してください。'}
              </div>
            )}
          </div>

          {rightTab === 'ir' && (
            <pre
              data-testid="studio-ir-json"
              style={{ margin: 0, padding: 12, background: '#111217', borderRadius: 4, overflow: 'auto', maxHeight: 520, fontSize: 12 }}
            >
              {job?.currentIR ? JSON.stringify(job.currentIR, null, 2) : '// まだ IR が生成されていません'}
            </pre>
          )}

          {rightTab === 'foundation' && (
            <pre
              data-testid="studio-foundation-json"
              style={{ margin: 0, padding: 12, background: '#111217', borderRadius: 4, overflow: 'auto', maxHeight: 520, fontSize: 12 }}
            >
              {foundationJsonPreview || '// まだ Foundation SDK 出力がありません'}
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
