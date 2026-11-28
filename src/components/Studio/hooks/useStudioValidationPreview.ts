import { MutableRefObject, useCallback, useMemo, useRef, useState } from 'react';
import { EmbeddedScene } from '@grafana/scenes';
import { extractApiError } from '../../../api/studioClient';
import { AllowedDatasourceOption } from '../../../datasource/allowlist';
import { runDryRunValidation } from '../../../datasource/validator';
import { computePanelQueryHash, JobSnapshot, PanelValidationResult, PublicConfig } from '../../../ir/types';
import { buildExportableDashboardJson } from '../../../save/saveDashboard';
import { normalizeDashboardIR, NormalizedDashboardSpec } from '../../../spec/normalize';
import { buildPreviewScene } from '../../../spec/toScene';
import {
  BLOCKING_CATEGORIES,
  computeVariablesFingerprint,
  mergeValidationResults,
  PreviewMeta,
  StudioBusyState,
  StudioRightTab,
  StudioTranslateFn,
  ValidatedPanelCacheEntry,
} from '../types';

export interface UseStudioValidationPreviewParams {
  job: JobSnapshot | null;
  selectedDatasource: AllowedDatasourceOption | null;
  publicCfg: PublicConfig | null;
  busyState: StudioBusyState;
  setBusyState: (state: StudioBusyState) => void;
  abortRef: MutableRefObject<AbortController | null>;
  contextSeqRef: MutableRefObject<number>;
  jobOpSeqRef: MutableRefObject<number>;
  setUiError: (err: string | null) => void;
  t: StudioTranslateFn;
}

async function findPanelsNeedingValidation(
  normalized: NormalizedDashboardSpec,
  targetJobId: string,
  varsFingerprint: string,
  validationResults: PanelValidationResult[],
  cacheMap: Map<string, ValidatedPanelCacheEntry>
): Promise<{ panelsNeedingValidation: string[]; panelHashMap: Map<string, string> }> {
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
    const cachedEntry = cacheMap.get(cacheKey);
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
  return { panelsNeedingValidation, panelHashMap };
}

function syncValidatedFingerprintsCache(
  cacheMap: Map<string, ValidatedPanelCacheEntry>,
  targetJobId: string,
  varsFingerprint: string,
  mergedResults: PanelValidationResult[],
  panelHashMap: Map<string, string>
): void {
  // Sync cache entries for the current jobId and current panel set.
  cacheMap.clear();
  for (const r of mergedResults) {
    cacheMap.set(`${targetJobId}:${r.panelId}`, {
      rawQueryHash: panelHashMap.get(r.panelId) ?? r.queryHash,
      varsFingerprint,
      category: r.category,
    });
  }
}

export function useStudioValidationPreview({
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
}: UseStudioValidationPreviewParams) {
  // Dry-run validation and explicit Scenes preview state bound to (jobId, revision).
  const [validationResults, setValidationResults] = useState<PanelValidationResult[]>([]);
  const [previewSpec, setPreviewSpec] = useState<NormalizedDashboardSpec | null>(null);
  const [previewScene, setPreviewScene] = useState<EmbeddedScene | null>(null);
  const [previewMeta, setPreviewMeta] = useState<PreviewMeta | null>(null);
  const [rightTab, setRightTab] = useState<StudioRightTab>('preview');

  const validatedFingerprintsRef = useRef<Map<string, ValidatedPanelCacheEntry>>(new Map());

  const clearPreviewAndValidation = useCallback(() => {
    validatedFingerprintsRef.current.clear();
    setValidationResults([]);
    setPreviewSpec(null);
    setPreviewScene(null);
    setPreviewMeta(null);
  }, []);

  const invalidateRepairedPanels = useCallback((jobId: string, panelIds: string[]) => {
    for (const panelId of panelIds) {
      validatedFingerprintsRef.current.delete(`${jobId}:${panelId}`);
    }
    const idSet = new Set(panelIds);
    setValidationResults((prev) => prev.filter((r) => !idSet.has(r.panelId)));
  }, []);

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
      const { panelsNeedingValidation, panelHashMap } = await findPanelsNeedingValidation(
        normalized,
        targetJobId,
        varsFingerprint,
        validationResults,
        validatedFingerprintsRef.current
      );

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
      syncValidatedFingerprintsCache(
        validatedFingerprintsRef.current,
        targetJobId,
        varsFingerprint,
        mergedResults,
        panelHashMap
      );
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

  return {
    validationResults,
    setValidationResults,
    previewSpec,
    previewScene,
    previewMeta,
    rightTab,
    setRightTab,
    syntaxErrors,
    hasBlockingValidationErrors,
    foundationJsonPreview,
    clearPreviewAndValidation,
    invalidateRepairedPanels,
    handleValidateAndPreview,
  };
}
