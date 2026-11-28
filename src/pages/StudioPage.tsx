import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { config, locationService } from '@grafana/runtime';
import { extractApiError } from '../api/studioClient';
import { DatasourceScopeSelector } from '../components/Studio/DatasourceScopeSelector';
import { useScopedMetadataCatalog } from '../components/Studio/hooks/useScopedMetadataCatalog';
import { useStudioImportSave } from '../components/Studio/hooks/useStudioImportSave';
import { useStudioJobWorkflow } from '../components/Studio/hooks/useStudioJobWorkflow';
import { useStudioValidationPreview } from '../components/Studio/hooks/useStudioValidationPreview';
import { JobStatusCard } from '../components/Studio/JobStatusCard';
import { PromptAndActionsSection } from '../components/Studio/PromptAndActionsSection';
import { SafeImportPanel } from '../components/Studio/SafeImportPanel';
import { SaveAndExportSection } from '../components/Studio/SaveAndExportSection';
import { StudioAlertsBanner } from '../components/Studio/StudioAlertsBanner';
import { StudioPreviewPane } from '../components/Studio/StudioPreviewPane';
import { PreservedImportMeta, StudioBusyState } from '../components/Studio/types';
import { ValidationResultsPanel } from '../components/Studio/ValidationResultsPanel';
import {
  AllowedDatasourceOption,
  isScopeLabelName,
  loadPublicConfigAndDatasources,
  resolveDatasourceDialect,
} from '../datasource/allowlist';
import { useStudioTranslation } from '../i18n';
import { JobSnapshot, PublicConfig } from '../ir/types';
import { listWritableFolders } from '../save/saveDashboard';

export const StudioPage: React.FC = () => {
  const { t, locale } = useStudioTranslation();
  const currentOrgId = config.bootData?.user?.orgId ?? 1;

  const [publicCfg, setPublicCfg] = useState<PublicConfig | null>(null);
  const [allowedDatasources, setAllowedDatasources] = useState<AllowedDatasourceOption[]>([]);
  const [selectedDsUid, setSelectedDsUid] = useState<string>('');
  const [job, setJob] = useState<JobSnapshot | null>(null);
  const [editPanelId, setEditPanelId] = useState<string>('');
  const [busyState, setBusyState] = useState<StudioBusyState>('idle');
  const [handoffFromReport, setHandoffFromReport] = useState(false);
  const [uiError, setUiError] = useState<string | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const metadataAbortRef = useRef<AbortController | null>(null);
  const contextSeqRef = useRef<number>(0);
  const jobOpSeqRef = useRef<number>(0);
  const latestImportMetaRef = useRef<Map<string, PreservedImportMeta>>(new Map());
  const prevContextKeyRef = useRef<string>(`${currentOrgId}:${selectedDsUid}`);

  const selectedDatasource = useMemo(
    () => allowedDatasources.find((d) => d.uid === selectedDsUid) ?? null,
    [allowedDatasources, selectedDsUid]
  );

  const activeDialect = useMemo(
    () => resolveDatasourceDialect(selectedDatasource, publicCfg?.datasourceDialects),
    [selectedDatasource, publicCfg?.datasourceDialects]
  );

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

  const reconcileEditPanelId = useCallback((nextJob: JobSnapshot) => {
    setEditPanelId((prev) => {
      if (prev === '__new__') {
        return '';
      }
      if (prev && nextJob.currentIR && !nextJob.currentIR.panels.some((p) => p.id === prev)) {
        return '';
      }
      return prev;
    });
  }, []);

  const metadata = useScopedMetadataCatalog({
    selectedDsUid,
    selectedDatasource,
    metadataAbortRef,
    contextSeqRef,
    setUiError,
    t,
  });

  const validation = useStudioValidationPreview({
    job,
    selectedDatasource,
    publicCfg,
    busyState,
    setBusyState,
    abortRef,
    contextSeqRef,
    jobOpSeqRef,
    setUiError,
    t,
  });

  const importSave = useStudioImportSave({
    job,
    setJob,
    applyJobSnapshot,
    reconcileEditPanelId,
    selectedDatasource,
    activeDialect,
    publicCfg,
    catalog: metadata.catalog,
    isCatalogFresh: metadata.isCatalogFresh,
    scopeLabelName: metadata.scopeLabelName,
    scopeLabelValue: metadata.scopeLabelValue,
    previewSpec: validation.previewSpec,
    previewMeta: validation.previewMeta,
    hasBlockingValidationErrors: validation.hasBlockingValidationErrors,
    busyState,
    setBusyState,
    abortRef,
    metadataAbortRef,
    contextSeqRef,
    jobOpSeqRef,
    latestImportMetaRef,
    clearPreviewAndValidation: validation.clearPreviewAndValidation,
    setUiError,
    t,
    locale,
  });

  const jobWorkflow = useStudioJobWorkflow({
    job,
    applyJobSnapshot,
    editPanelId,
    reconcileEditPanelId,
    selectedDsUid,
    activeDialect,
    publicCfg,
    catalog: metadata.catalog,
    isCatalogFresh: metadata.isCatalogFresh,
    scopeLabelName: metadata.scopeLabelName,
    scopeLabelValue: metadata.scopeLabelValue,
    syntaxErrors: validation.syntaxErrors,
    busyState,
    setBusyState,
    abortRef,
    contextSeqRef,
    jobOpSeqRef,
    clearPreviewAndValidation: validation.clearPreviewAndValidation,
    clearValidationResultsOnly: () => validation.setValidationResults([]),
    invalidateRepairedPanels: validation.invalidateRepairedPanels,
    clearImportReportAndOptIn: () => {
      importSave.setImportReport(null);
      importSave.setOptInUpdateExisting(false);
    },
    clearSavedResult: () => importSave.setSavedResult(null),
    clearOptInUpdate: () => importSave.setOptInUpdateExisting(false),
    setUiError,
    t,
  });

  const { resetMetadata } = metadata;
  const { clearPreviewAndValidation } = validation;
  const { resetImportAndSave } = importSave;
  const { resetJobWorkflow } = jobWorkflow;

  const clearDraftAndMetadata = useCallback(() => {
    contextSeqRef.current += 1;
    jobOpSeqRef.current += 1;
    abortRef.current?.abort();
    abortRef.current = null;
    metadataAbortRef.current?.abort();
    metadataAbortRef.current = null;
    resetMetadata();
    resetImportAndSave();
    clearPreviewAndValidation();
    resetJobWorkflow();
    setJob(null);
    setHandoffFromReport(false);
    setEditPanelId('');
    setUiError(null);
    setBusyState('idle');
  }, [clearPreviewAndValidation, resetImportAndSave, resetJobWorkflow, resetMetadata]);

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
        const { publicConfig: cfg, datasources: filtered } = await loadPublicConfigAndDatasources(
          initController.signal
        );
        if (!active) {
          return;
        }
        setPublicCfg(cfg);
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
          if (isScopeLabelName(handoffScopeLabel)) {
            metadata.setScopeLabelName(handoffScopeLabel);
          }
          if (handoffScopeValue) {
            metadata.setScopeLabelValue(handoffScopeValue);
          }
          if (handoffIntent) {
            jobWorkflow.setPromptText(handoffIntent);
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
          importSave.setFolders(writable);
        }
      } catch {
        if (active) {
          importSave.setFolders([{ uid: '', title: t('save.folder.generalRoot') }]);
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

  const canSave =
    Boolean(validation.previewSpec) &&
    Boolean(validation.previewMeta) &&
    Boolean(job?.currentIR) &&
    validation.previewMeta?.jobId === job?.jobId &&
    validation.previewMeta?.revision === job?.currentIR?.revision &&
    !validation.hasBlockingValidationErrors &&
    busyState === 'idle';

  return (
    <div data-testid="studio-page" style={{ display: 'flex', flexDirection: 'column', gap: 12, padding: 16 }}>
      <StudioAlertsBanner
        t={t}
        publicCfg={publicCfg}
        handoffFromReport={handoffFromReport}
        onDismissHandoff={() => setHandoffFromReport(false)}
        uiError={uiError}
        onDismissError={() => setUiError(null)}
        savedResult={importSave.savedResult}
      />

      <div style={{ display: 'grid', gridTemplateColumns: '420px 1fr', gap: 16, alignItems: 'start' }}>
        {/* Left Pane: Scope, Optional Safe Import, Chat, Status, Diff, Validation, Save/Export */}
        <div
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 12,
            border: '1px solid #2c3235',
            borderRadius: 6,
            padding: 12,
          }}
        >
          <h4 style={{ margin: 0 }}>{t('studio.title')}</h4>

          <DatasourceScopeSelector
            t={t}
            allowedDatasources={allowedDatasources}
            selectedDsUid={selectedDsUid}
            onSelectDatasource={setSelectedDsUid}
            selectedDatasource={selectedDatasource}
            activeDialect={activeDialect}
            scopeLabelName={metadata.scopeLabelName}
            onSelectScopeLabelName={metadata.setScopeLabelName}
            scopeLabelValue={metadata.scopeLabelValue}
            onChangeScopeLabelValue={metadata.setScopeLabelValue}
            scopeLabelValues={metadata.scopeLabelValues}
            loadingCatalog={metadata.loadingCatalog}
            catalogCount={metadata.catalog.length}
            catalogTruncated={metadata.catalogTruncated}
            isCatalogFresh={metadata.isCatalogFresh}
            onLoadScopeValues={metadata.handleLoadScopeValues}
            onLoadScopedCatalog={metadata.handleLoadScopedCatalog}
          />

          <SafeImportPanel
            t={t}
            selectedDsUid={selectedDsUid}
            selectedDatasource={selectedDatasource}
            busyState={busyState}
            showImportPanel={importSave.showImportPanel}
            onToggleImportPanel={() => importSave.setShowImportPanel((prev) => !prev)}
            importUid={importSave.importUid}
            onChangeImportUid={importSave.setImportUid}
            existingDashboards={importSave.existingDashboards}
            importReport={importSave.importReport}
            onListExistingDashboards={importSave.handleListExistingDashboards}
            onImportExistingDashboard={importSave.handleImportExistingDashboard}
          />

          <PromptAndActionsSection
            t={t}
            job={job}
            editPanelId={editPanelId}
            onChangeEditPanelId={setEditPanelId}
            promptText={jobWorkflow.promptText}
            onChangePromptText={jobWorkflow.setPromptText}
            busyState={busyState}
            isCatalogFresh={metadata.isCatalogFresh}
            canCancel={jobWorkflow.canCancel}
            liveStage={jobWorkflow.liveStage}
            sseFallbackNotice={jobWorkflow.sseFallbackNotice}
            onGenerateOrPatch={jobWorkflow.handleGenerateOrPatch}
            onCancel={jobWorkflow.handleCancel}
            onUndo={jobWorkflow.handleUndo}
          />

          {job && <JobStatusCard t={t} job={job} />}

          {validation.validationResults.length > 0 && (
            <ValidationResultsPanel
              t={t}
              validationResults={validation.validationResults}
              syntaxErrorCount={validation.syntaxErrors.length}
              job={job}
              busyState={busyState}
              isCatalogFresh={metadata.isCatalogFresh}
              onRepairSyntaxErrors={jobWorkflow.handleRepairSyntaxErrors}
            />
          )}

          {job?.currentIR && (
            <SaveAndExportSection
              t={t}
              job={job}
              folderOptions={importSave.folderOptions}
              selectedFolderUid={importSave.selectedFolderUid}
              onSelectFolderUid={importSave.setSelectedFolderUid}
              canOptInUpdate={importSave.canOptInUpdate}
              optInUpdateExisting={importSave.optInUpdateExisting}
              onChangeOptInUpdateExisting={importSave.setOptInUpdateExisting}
              canSave={canSave}
              busyState={busyState}
              onSaveDashboard={importSave.handleSaveDashboard}
              onExportJson={importSave.handleExportJson}
            />
          )}
        </div>

        {/* Right Pane: Explicit Validation Trigger + Mounted Scenes Preview / IR / Foundation JSON */}
        <StudioPreviewPane
          t={t}
          rightTab={validation.rightTab}
          onChangeRightTab={validation.setRightTab}
          job={job}
          selectedDatasource={selectedDatasource}
          busyState={busyState}
          previewScene={validation.previewScene}
          foundationJsonPreview={validation.foundationJsonPreview}
          onValidateAndPreview={validation.handleValidateAndPreview}
        />
      </div>
    </div>
  );
};
