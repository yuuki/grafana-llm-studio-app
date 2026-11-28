import { Dispatch, MutableRefObject, SetStateAction, useCallback, useMemo, useRef, useState } from 'react';
import { extractApiError, importDraft } from '../../../api/studioClient';
import { AllowedDatasourceOption } from '../../../datasource/allowlist';
import {
  convertGrafanaDashboardToIR,
  ExistingDashboardSummary,
  fetchExistingDashboardByUid,
  ImportConversionReport,
  listExistingDashboards,
} from '../../../import/importDashboard';
import { JobSnapshot, MetricCatalogItem, PublicConfig, QueryKind, ScopeFilter } from '../../../ir/types';
import {
  buildExportableDashboardJson,
  DashboardSaver,
  GrafanaFolderOption,
  SaveDashboardResult,
} from '../../../save/saveDashboard';
import { normalizeDashboardIR, NormalizedDashboardSpec } from '../../../spec/normalize';
import { downloadBlob } from '../../../utils/downloadBlob';
import { PreservedImportMeta, PreviewMeta, StudioBusyState, StudioTranslateFn } from '../types';

export interface UseStudioImportSaveParams {
  job: JobSnapshot | null;
  setJob: Dispatch<SetStateAction<JobSnapshot | null>>;
  applyJobSnapshot: (nextJob: JobSnapshot) => void;
  reconcileEditPanelId: (nextJob: JobSnapshot) => void;
  selectedDatasource: AllowedDatasourceOption | null;
  activeDialect: { kind: QueryKind; pinned: boolean };
  publicCfg: PublicConfig | null;
  catalog: MetricCatalogItem[];
  isCatalogFresh: boolean;
  scopeLabelName: string;
  scopeLabelValue: string;
  previewSpec: NormalizedDashboardSpec | null;
  previewMeta: PreviewMeta | null;
  hasBlockingValidationErrors: boolean;
  busyState: StudioBusyState;
  setBusyState: (state: StudioBusyState) => void;
  abortRef: MutableRefObject<AbortController | null>;
  metadataAbortRef: MutableRefObject<AbortController | null>;
  contextSeqRef: MutableRefObject<number>;
  jobOpSeqRef: MutableRefObject<number>;
  latestImportMetaRef: MutableRefObject<Map<string, PreservedImportMeta>>;
  clearPreviewAndValidation: () => void;
  setUiError: (err: string | null) => void;
  t: StudioTranslateFn;
  locale: string;
}

export function useStudioImportSave({
  job,
  setJob,
  applyJobSnapshot,
  reconcileEditPanelId,
  selectedDatasource,
  activeDialect,
  publicCfg,
  catalog,
  isCatalogFresh,
  scopeLabelName,
  scopeLabelValue,
  previewSpec,
  previewMeta,
  hasBlockingValidationErrors,
  busyState,
  setBusyState,
  abortRef,
  metadataAbortRef,
  contextSeqRef,
  jobOpSeqRef,
  latestImportMetaRef,
  clearPreviewAndValidation,
  setUiError,
  t,
  locale,
}: UseStudioImportSaveParams) {
  // Optional safe import state.
  const [showImportPanel, setShowImportPanel] = useState(false);
  const [importUid, setImportUid] = useState('');
  const [existingDashboards, setExistingDashboards] = useState<ExistingDashboardSummary[]>([]);
  const [importReport, setImportReport] = useState<ImportConversionReport | null>(null);

  // Save state (create-only copy by default; opt-in revision-checked update for imported dashboards).
  const [folders, setFolders] = useState<GrafanaFolderOption[]>(() => [
    { uid: '', title: t('save.folder.generalRoot') },
  ]);
  const [selectedFolderUid, setSelectedFolderUid] = useState<string>('');
  const [optInUpdateExisting, setOptInUpdateExisting] = useState(false);
  const [activeImportPatchKey, setActiveImportPatchKey] = useState<string | null>(null);
  const [savedResult, setSavedResult] = useState<SaveDashboardResult | null>(null);

  const saverRef = useRef<DashboardSaver>(new DashboardSaver());

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

  const resetImportAndSave = useCallback(() => {
    latestImportMetaRef.current.clear();
    setShowImportPanel(false);
    setImportUid('');
    setExistingDashboards([]);
    setImportReport(null);
    setSelectedFolderUid('');
    setOptInUpdateExisting(false);
    setActiveImportPatchKey(null);
    setSavedResult(null);
  }, [latestImportMetaRef]);

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
      clearPreviewAndValidation();
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

  const activeImportKey = job?.importSource?.uid ? `${job.jobId}:${job.importSource.uid}` : '';
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
      const importMetaKey = job.importSource?.uid ? `${job.jobId}:${job.importSource.uid}` : '';
      const preservedMeta = importMetaKey ? latestImportMetaRef.current.get(importMetaKey) : undefined;
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
      downloadBlob(`dashboard-rev-${spec.revision}.json`, json, 'application/json');
    } catch (err: unknown) {
      setUiError(t('studio.errors.exportFailed', { error: extractApiError(err) }));
    }
  };

  return {
    showImportPanel,
    setShowImportPanel,
    importUid,
    setImportUid,
    existingDashboards,
    importReport,
    setImportReport,
    setFolders,
    folderOptions,
    selectedFolderUid,
    setSelectedFolderUid,
    optInUpdateExisting,
    setOptInUpdateExisting,
    savedResult,
    setSavedResult,
    canOptInUpdate,
    resetImportAndSave,
    handleListExistingDashboards,
    handleImportExistingDashboard,
    handleSaveDashboard,
    handleExportJson,
  };
}
