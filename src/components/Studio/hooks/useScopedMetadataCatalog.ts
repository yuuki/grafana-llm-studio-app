import { MutableRefObject, useCallback, useState } from 'react';
import { extractApiError } from '../../../api/studioClient';
import { AllowedDatasourceOption } from '../../../datasource/allowlist';
import { fetchScopedMetricCatalog, fetchScopeLabelValues } from '../../../datasource/metadata';
import { MetricCatalogItem } from '../../../ir/types';
import { makeScopeKey, StudioTranslateFn } from '../types';

export interface UseScopedMetadataCatalogParams {
  selectedDsUid: string;
  selectedDatasource: AllowedDatasourceOption | null;
  metadataAbortRef: MutableRefObject<AbortController | null>;
  contextSeqRef: MutableRefObject<number>;
  setUiError: (err: string | null) => void;
  t: StudioTranslateFn;
}

export function useScopedMetadataCatalog({
  selectedDsUid,
  selectedDatasource,
  metadataAbortRef,
  contextSeqRef,
  setUiError,
  t,
}: UseScopedMetadataCatalogParams) {
  const [scopeLabelName, setScopeLabelName] = useState<string>('service');
  const [scopeLabelValues, setScopeLabelValues] = useState<string[]>([]);
  const [scopeLabelValue, setScopeLabelValue] = useState<string>('');

  // In-memory only scoped metadata bound to catalogScopeKey (cleared on org/datasource switch or unmount).
  const [catalog, setCatalog] = useState<MetricCatalogItem[]>([]);
  const [catalogScopeKey, setCatalogScopeKey] = useState<string>('');
  const [catalogTruncated, setCatalogTruncated] = useState(false);
  const [loadingCatalog, setLoadingCatalog] = useState(false);

  const currentScopeKey = makeScopeKey(selectedDsUid, scopeLabelName, scopeLabelValue);
  const isCatalogFresh = catalog.length > 0 && catalogScopeKey === currentScopeKey;

  const resetMetadata = useCallback(() => {
    setCatalog([]);
    setCatalogScopeKey('');
    setCatalogTruncated(false);
    setLoadingCatalog(false);
    setScopeLabelValues([]);
    setScopeLabelValue('');
  }, []);

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

  return {
    scopeLabelName,
    setScopeLabelName,
    scopeLabelValues,
    scopeLabelValue,
    setScopeLabelValue,
    catalog,
    catalogTruncated,
    loadingCatalog,
    isCatalogFresh,
    resetMetadata,
    handleLoadScopeValues,
    handleLoadScopedCatalog,
  };
}
