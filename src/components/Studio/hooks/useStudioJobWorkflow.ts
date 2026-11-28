import { MutableRefObject, useCallback, useRef, useState } from 'react';
import { cancelJobBounded } from '../../../api/cancelHelper';
import {
  extractApiError,
  generateDraft,
  patchDraft,
  repairDraft,
  undoDraft,
} from '../../../api/studioClient';
import { selectMetricCandidates } from '../../../datasource/bm25';
import {
  JobSnapshot,
  JobStageEvent,
  MetricCatalogItem,
  PanelValidationResult,
  PublicConfig,
  QueryKind,
  ScopeFilter,
} from '../../../ir/types';
import { StudioBusyState, StudioTranslateFn } from '../types';

export interface UseStudioJobWorkflowParams {
  job: JobSnapshot | null;
  applyJobSnapshot: (nextJob: JobSnapshot) => void;
  editPanelId: string;
  reconcileEditPanelId: (nextJob: JobSnapshot) => void;
  selectedDsUid: string;
  activeDialect: { kind: QueryKind; pinned: boolean };
  publicCfg: PublicConfig | null;
  catalog: MetricCatalogItem[];
  isCatalogFresh: boolean;
  scopeLabelName: string;
  scopeLabelValue: string;
  syntaxErrors: PanelValidationResult[];
  busyState: StudioBusyState;
  setBusyState: (state: StudioBusyState) => void;
  abortRef: MutableRefObject<AbortController | null>;
  contextSeqRef: MutableRefObject<number>;
  jobOpSeqRef: MutableRefObject<number>;
  clearPreviewAndValidation: () => void;
  clearValidationResultsOnly: () => void;
  invalidateRepairedPanels: (jobId: string, panelIds: string[]) => void;
  clearImportReportAndOptIn: () => void;
  clearSavedResult: () => void;
  clearOptInUpdate: () => void;
  setUiError: (err: string | null) => void;
  t: StudioTranslateFn;
}

export function useStudioJobWorkflow({
  job,
  applyJobSnapshot,
  editPanelId,
  reconcileEditPanelId,
  selectedDsUid,
  activeDialect,
  publicCfg,
  catalog,
  isCatalogFresh,
  scopeLabelName,
  scopeLabelValue,
  syntaxErrors,
  busyState,
  setBusyState,
  abortRef,
  contextSeqRef,
  jobOpSeqRef,
  clearPreviewAndValidation,
  clearValidationResultsOnly,
  invalidateRepairedPanels,
  clearImportReportAndOptIn,
  clearSavedResult,
  clearOptInUpdate,
  setUiError,
  t,
}: UseStudioJobWorkflowParams) {
  const [promptText, setPromptText] = useState(() => t('studio.prompt.defaultText'));
  const [liveStage, setLiveStage] = useState<JobStageEvent | null>(null);
  const [sseFallbackNotice, setSseFallbackNotice] = useState(false);

  const inFlightJobIdRef = useRef<string | null>(null);

  const resetJobWorkflow = useCallback(() => {
    inFlightJobIdRef.current = null;
    setLiveStage(null);
    setSseFallbackNotice(false);
  }, []);

  const syncJobFromError = (err: unknown) => {
    if (typeof err === 'object' && err !== null) {
      const maybeJob = (err as { data?: { job?: JobSnapshot } })?.data?.job;
      if (maybeJob && maybeJob.jobId) {
        applyJobSnapshot(maybeJob);
      }
    }
  };

  const makeStreamOptions = (controller: AbortController, seq: number) => ({
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
  });

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
    clearSavedResult();

    // Starting a brand-new draft MUST clear previous job's preview state immediately (C-2).
    if (!isPatch) {
      clearPreviewAndValidation();
      clearImportReportAndOptIn();
    }

    const narrowedCatalog = selectMetricCandidates(catalog, promptText, {
      maxItems: 40,
      maxTotalChars: 11000,
    });
    const scope: ScopeFilter = {
      labelName: scopeLabelName,
      labelValue: scopeLabelValue.trim(),
    };
    const streamOptions = makeStreamOptions(controller, seq);

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
        clearValidationResultsOnly();
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
      setBusyState('cancelling');
      try {
        const updated = await cancelJobBounded(cancelTargetJobId, {
          controller: cancelController,
        });
        if (updated && seq === contextSeqRef.current && opSeq === jobOpSeqRef.current) {
          applyJobSnapshot(updated);
        }
      } finally {
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
      clearPreviewAndValidation();
      clearOptInUpdate();
      applyJobSnapshot(updated);
      reconcileEditPanelId(updated);
      clearSavedResult();
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
        makeStreamOptions(controller, seq)
      );
      if (!controller.signal.aborted && seq === contextSeqRef.current) {
        applyJobSnapshot(updated);
        // Clear stale syntax_error results so repaired panels must be re-validated (M-11).
        invalidateRepairedPanels(
          job.jobId,
          syntaxErrors.map((s) => s.panelId)
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

  const canCancel =
    busyState === 'generating' ||
    busyState === 'importing' ||
    busyState === 'patching' ||
    busyState === 'validating' ||
    busyState === 'repairing';

  return {
    promptText,
    setPromptText,
    liveStage,
    sseFallbackNotice,
    canCancel,
    resetJobWorkflow,
    handleGenerateOrPatch,
    handleCancel,
    handleUndo,
    handleRepairSyntaxErrors,
  };
}
