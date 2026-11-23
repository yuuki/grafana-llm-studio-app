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
  fetchPublicConfig,
  generateDraft,
  patchDraft,
  repairDraft,
  undoDraft,
} from '../api/studioClient';
import { selectMetricCandidates } from '../datasource/bm25';
import { fetchScopedMetricCatalog, fetchScopeLabelValues } from '../datasource/metadata';
import { runDryRunValidation } from '../datasource/validator';
import {
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

export const StudioPage: React.FC = () => {
  const currentOrgId = config.bootData?.user?.orgId ?? 1;

  const [publicCfg, setPublicCfg] = useState<PublicConfig | null>(null);
  const [allowedDatasources, setAllowedDatasources] = useState<AllowedDatasourceOption[]>([]);
  const [selectedDsUid, setSelectedDsUid] = useState<string>('');
  const [scopeLabelName, setScopeLabelName] = useState<string>('service');
  const [scopeLabelValues, setScopeLabelValues] = useState<string[]>([]);
  const [scopeLabelValue, setScopeLabelValue] = useState<string>('');

  // In-memory only scoped metadata (cleared on org/datasource change or unmount).
  const [catalog, setCatalog] = useState<MetricCatalogItem[]>([]);
  const [catalogTruncated, setCatalogTruncated] = useState(false);
  const [loadingCatalog, setLoadingCatalog] = useState(false);

  // Studio conversation & job state.
  const [promptText, setPromptText] = useState(
    '選択したサービスのリクエスト数、5xxエラー率、p95レイテンシを見たい'
  );
  const [editPanelId, setEditPanelId] = useState<string>('');
  const [job, setJob] = useState<JobSnapshot | null>(null);
  const [busyState, setBusyState] = useState<
    'idle' | 'generating' | 'patching' | 'validating' | 'repairing' | 'saving'
  >('idle');
  const [uiError, setUiError] = useState<string | null>(null);

  // Dry-run validation and explicit Scenes preview state.
  const [validationResults, setValidationResults] = useState<PanelValidationResult[]>([]);
  const [previewSpec, setPreviewSpec] = useState<NormalizedDashboardSpec | null>(null);
  const [previewScene, setPreviewScene] = useState<EmbeddedScene | null>(null);
  const [previewedRevision, setPreviewedRevision] = useState<number | null>(null);
  const [rightTab, setRightTab] = useState<'preview' | 'ir' | 'foundation'>('preview');

  // Create-only save state.
  const [folders, setFolders] = useState<GrafanaFolderOption[]>([{ uid: '', title: 'General (Root)' }]);
  const [selectedFolderUid, setSelectedFolderUid] = useState<string>('');
  const [savedResult, setSavedResult] = useState<SaveDashboardResult | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const saverRef = useRef<DashboardSaver>(new DashboardSaver());
  const prevContextKeyRef = useRef<string>(`${currentOrgId}:${selectedDsUid}`);

  const clearDraftAndMetadata = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    setCatalog([]);
    setCatalogTruncated(false);
    setJob(null);
    setValidationResults([]);
    setPreviewSpec(null);
    setPreviewScene(null);
    setPreviewedRevision(null);
    setSavedResult(null);
    setEditPanelId('');
    setUiError(null);
    setBusyState('idle');
  }, []);

  // Reset draft and in-memory metadata on org or datasource switch (and unmount / logout).
  useEffect(() => {
    const key = `${currentOrgId}:${selectedDsUid}`;
    if (prevContextKeyRef.current !== key) {
      prevContextKeyRef.current = key;
      clearDraftAndMetadata();
    }
  }, [currentOrgId, selectedDsUid, clearDraftAndMetadata]);

  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, []);

  // Load secret-free public config, allowlisted datasources, and writable folders.
  useEffect(() => {
    let active = true;
    (async () => {
      try {
        const cfg = await fetchPublicConfig();
        if (!active) {
          return;
        }
        setPublicCfg(cfg);

        const allDs = getDataSourceSrv().getList({ metrics: true });
        const allowSet = new Set(cfg.allowedDatasourceUids);
        const filtered: AllowedDatasourceOption[] = allDs
          .filter((d) => allowSet.has(d.uid))
          .map((d) => ({ uid: d.uid, name: d.name, type: d.type }));
        setAllowedDatasources(filtered);
        if (filtered.length > 0) {
          setSelectedDsUid(filtered[0].uid);
        }

        const writable = await listWritableFolders();
        if (active) {
          setFolders(writable);
        }
      } catch (err: unknown) {
        if (active) {
          setUiError(
            `初期化に失敗しました（App Config で許可データソースと LLM モードを確認してください）: ${
              err instanceof Error ? err.message : String(err)
            }`
          );
        }
      }
    })();
    return () => {
      active = false;
    };
  }, [currentOrgId]);

  const selectedDatasource = useMemo(
    () => allowedDatasources.find((d) => d.uid === selectedDsUid) ?? null,
    [allowedDatasources, selectedDsUid]
  );

  const handleLoadScopeValues = async () => {
    if (!selectedDsUid) {
      return;
    }
    setUiError(null);
    try {
      const vals = await fetchScopeLabelValues(selectedDsUid, scopeLabelName, 3600, 100);
      setScopeLabelValues(vals);
      if (vals.length > 0 && !scopeLabelValue) {
        setScopeLabelValue(vals[0]);
      }
    } catch (err: unknown) {
      setUiError(`スコープ値取得エラー: ${err instanceof Error ? err.message : String(err)}`);
    }
  };

  const handleLoadScopedCatalog = async () => {
    if (!selectedDsUid || !scopeLabelValue.trim()) {
      setUiError('データソースと対象スコープ値（例: checkout-api）を指定してください。');
      return;
    }
    setLoadingCatalog(true);
    setUiError(null);
    try {
      const res = await fetchScopedMetricCatalog({
        datasourceUid: selectedDsUid,
        scope: { labelName: scopeLabelName, labelValue: scopeLabelValue.trim() },
      });
      setCatalog(res.items);
      setCatalogTruncated(res.truncated);
    } catch (err: unknown) {
      setUiError(`メタデータ取得エラー: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setLoadingCatalog(false);
    }
  };

  const handleGenerateOrPatch = async () => {
    if (!selectedDsUid) {
      setUiError('許可された Prometheus データソースを選択してください。');
      return;
    }
    if (catalog.length === 0) {
      setUiError('先に「スコープのメトリクスを取得」を実行し、実在するメトリクス候補を読み込んでください。');
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    const isPatch = Boolean(job?.currentIR && editPanelId !== '__new__');
    setBusyState(isPatch ? 'patching' : 'generating');
    setUiError(null);
    setSavedResult(null);

    const narrowedCatalog = selectMetricCandidates(catalog, promptText, {
      maxItems: 40,
      maxTotalChars: 11000,
    });
    const scope: ScopeFilter = {
      labelName: scopeLabelName,
      labelValue: scopeLabelValue.trim(),
    };

    try {
      const nextJob = isPatch && job?.currentIR
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

      if (controller.signal.aborted) {
        return;
      }
      setJob(nextJob);
      // Do NOT auto-apply to Scene or auto-query Prometheus; require explicit user action.
      setValidationResults([]);
    } catch (err: unknown) {
      if (controller.signal.aborted) {
        return;
      }
      setUiError(extractApiError(err));
    } finally {
      if (abortRef.current === controller) {
        setBusyState('idle');
      }
    }
  };

  const handleCancel = async () => {
    abortRef.current?.abort();
    setBusyState('idle');
    if (job?.jobId) {
      try {
        const updated = await cancelDraftJob(job.jobId);
        setJob(updated);
      } catch {
        // Ignore cancel race errors if job already completed.
      }
    }
  };

  const handleUndo = async () => {
    if (!job?.jobId) {
      return;
    }
    setUiError(null);
    try {
      const updated = await undoDraft(job.jobId);
      setJob(updated);
      setValidationResults([]);
      setSavedResult(null);
    } catch (err: unknown) {
      setUiError(extractApiError(err));
    }
  };

  /**
   * Explicit user action: runs bounded Dry-Run validation (5m, maxDataPoints=10, concurrency<=2)
   * and then constructs a memoized @grafana/scenes preview ONLY for the verified revision.
   */
  const handleValidateAndPreview = async () => {
    if (!job?.currentIR || !selectedDatasource) {
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setBusyState('validating');
    setUiError(null);

    try {
      const normalized = normalizeDashboardIR(job.currentIR, selectedDatasource);

      // Re-validate only changed panels if a previous revision already validated unchanged panels.
      const changedIds =
        previewedRevision !== null && job.lastDiff && job.lastDiff.length > 0
          ? Array.from(
              new Set(
                job.lastDiff
                  .map((d) => d.panelId)
                  .filter((id): id is string => Boolean(id))
              )
            )
          : undefined;

      const results = await runDryRunValidation(normalized, {
        maxConcurrency: publicCfg?.maxConcurrentQueries ?? 2,
        timeoutMs: (publicCfg?.validationTimeoutSeconds ?? 10) * 1000,
        from: 'now-5m',
        to: 'now',
        maxDataPoints: 10,
        onlyPanelIds: changedIds && changedIds.length > 0 ? changedIds : undefined,
        signal: controller.signal,
      });

      if (controller.signal.aborted) {
        return;
      }

      const mergedResults =
        changedIds && changedIds.length > 0
          ? mergeValidationResults(validationResults, results, normalized)
          : results;
      setValidationResults(mergedResults);

      // Build memoized Scenes preview ONCE for this revision (never recreated on unrelated React renders).
      const scene = buildPreviewScene(normalized, { maxDataPoints: 200 });
      setPreviewSpec(normalized);
      setPreviewScene(scene);
      setPreviewedRevision(normalized.revision);
      setRightTab('preview');
    } catch (err: unknown) {
      if (!controller.signal.aborted) {
        setUiError(`検証・プレビューに失敗しました: ${err instanceof Error ? err.message : String(err)}`);
      }
    } finally {
      if (abortRef.current === controller) {
        setBusyState('idle');
      }
    }
  };

  const syntaxErrors = useMemo(
    () => validationResults.filter((r) => r.category === 'syntax_error'),
    [validationResults]
  );

  const handleRepairSyntaxErrors = async () => {
    if (!job?.currentIR || syntaxErrors.length === 0) {
      return;
    }
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
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
      if (!controller.signal.aborted) {
        setJob(updated);
      }
    } catch (err: unknown) {
      if (!controller.signal.aborted) {
        setUiError(extractApiError(err));
      }
    } finally {
      if (abortRef.current === controller) {
        setBusyState('idle');
      }
    }
  };

  const handleSaveNewDashboard = async () => {
    if (!previewSpec || previewedRevision === null) {
      setUiError('保存前に「検証・プレビューを実行」して最新ドラフトを確認してください。');
      return;
    }
    setBusyState('saving');
    setUiError(null);
    setSavedResult(null);
    try {
      const res = await saverRef.current.createNewDashboard({
        spec: previewSpec,
        previewedRevision,
        folderUid: selectedFolderUid,
      });
      setSavedResult(res);
    } catch (err: unknown) {
      setUiError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyState('idle');
    }
  };

  const handleExportJson = () => {
    if (!previewSpec && (!job?.currentIR || !selectedDatasource)) {
      return;
    }
    const spec = previewSpec ?? normalizeDashboardIR(job!.currentIR!, selectedDatasource!);
    const json = buildExportableDashboardJson(spec);
    const blob = new Blob([json], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `dashboard-rev-${spec.revision}.json`;
    a.click();
    URL.revokeObjectURL(url);
  };

  const foundationJsonPreview = useMemo(() => {
    if (!job?.currentIR || !selectedDatasource) {
      return '';
    }
    try {
      const spec = normalizeDashboardIR(job.currentIR, selectedDatasource);
      return buildExportableDashboardJson(spec);
    } catch {
      return '';
    }
  }, [job?.currentIR, selectedDatasource]);

  const canSave =
    Boolean(previewSpec) &&
    Boolean(job?.currentIR) &&
    previewedRevision === job?.currentIR?.revision &&
    busyState === 'idle';

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12, padding: 16 }}>
      {publicCfg?.mockBanner && (
        <Alert title="開発・テスト用モックLLMモード" severity="warning">
          現在 llmMode=mock で動作しています。本番利用時は App Config で grafana-llm-app モードへ切り替えてください。
        </Alert>
      )}

      {uiError && (
        <Alert title="エラー" severity="error" onRemove={() => setUiError(null)}>
          {uiError}
        </Alert>
      )}

      {savedResult && (
        <Alert title="ダッシュボードを新規保存しました" severity="success">
          UID: <code>{savedResult.uid}</code> (rev {savedResult.revision}) —{' '}
          <a href={savedResult.url} target="_blank" rel="noreferrer">
            保存したダッシュボードを開いてクエリ・単位・閾値・配置を確認
          </a>
        </Alert>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: '420px 1fr', gap: 16, alignItems: 'start' }}>
        {/* Left Pane: Scope, Chat, Status, Diff, Validation, Save/Export */}
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
            読込済みメトリクス候補（ブラウザメモリのみ）: <strong>{catalog.length}</strong> 件
            {catalogTruncated ? '（上限で打ち切り）' : ''}
          </div>

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
              disabled={busyState !== 'idle' || catalog.length === 0}
            >
              {busyState === 'generating' || busyState === 'patching'
                ? '生成中...'
                : job?.currentIR && editPanelId !== '__new__'
                ? '部分修正を適用'
                : 'ダッシュボード案を生成'}
            </Button>
            {busyState !== 'idle' && (
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
            <div style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span>
                  状態: <Badge text={job.status} color={job.status === 'failed' ? 'red' : 'blue'} />
                </span>
                {job.currentIR && (
                  <span style={{ fontSize: 12 }}>rev {job.currentIR.revision}</span>
                )}
              </div>
              {job.summary && <div style={{ fontSize: 13 }}>{job.summary}</div>}
              {job.errorMessage && (
                <Alert title="生成・検査メッセージ" severity="warning">
                  {job.errorMessage}
                </Alert>
              )}
              {job.lastDiff && job.lastDiff.length > 0 && (
                <div style={{ fontSize: 12 }}>
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
            <div style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}>
              <strong>検証結果（Dry-Run: 直近5m / maxDataPoints=10）</strong>
              {validationResults.map((r) => (
                <div key={r.panelId} style={{ fontSize: 12, display: 'flex', flexDirection: 'column', gap: 2 }}>
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
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={handleRepairSyntaxErrors}
                    disabled={busyState !== 'idle'}
                  >
                    {busyState === 'repairing' ? '修復中...' : '構文エラーのみ自動修復'}
                  </Button>
                )}
            </div>
          )}

          {job?.currentIR && (
            <div style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 8 }}>
              <Field label="保存先フォルダ（新規作成のみ）">
                <Select
                  options={folders.map((f) => ({ label: f.title, value: f.uid }))}
                  value={selectedFolderUid}
                  onChange={(v) => setSelectedFolderUid(v.value ?? '')}
                />
              </Field>
              <div style={{ display: 'flex', gap: 8 }}>
                <Button onClick={handleSaveNewDashboard} disabled={!canSave}>
                  {busyState === 'saving' ? '保存中...' : '新規ダッシュボード保存'}
                </Button>
                <Button variant="secondary" onClick={handleExportJson} disabled={busyState !== 'idle'}>
                  JSONエクスポート
                </Button>
              </div>
              {!canSave && job.currentIR && (
                <div style={{ fontSize: 12, color: '#9fa7b3' }}>
                  最新リビジョン (rev {job.currentIR.revision}) の「検証・プレビューを実行」完了後に保存できます。
                </div>
              )}
            </div>
          )}
        </div>

        {/* Right Pane: Explicit Validation Trigger + Scenes Preview / IR / Foundation JSON */}
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

          {rightTab === 'preview' && (
            <div style={{ minHeight: 460 }}>
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
          )}

          {rightTab === 'ir' && (
            <pre style={{ margin: 0, padding: 12, background: '#111217', borderRadius: 4, overflow: 'auto', maxHeight: 520, fontSize: 12 }}>
              {job?.currentIR ? JSON.stringify(job.currentIR, null, 2) : '// まだ IR が生成されていません'}
            </pre>
          )}

          {rightTab === 'foundation' && (
            <pre style={{ margin: 0, padding: 12, background: '#111217', borderRadius: 4, overflow: 'auto', maxHeight: 520, fontSize: 12 }}>
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

function extractApiError(err: unknown): string {
  if (typeof err === 'object' && err !== null) {
    const e = err as { data?: { error?: string; message?: string }; message?: string };
    return e.data?.error || e.data?.message || e.message || 'リクエストに失敗しました';
  }
  return String(err);
}
