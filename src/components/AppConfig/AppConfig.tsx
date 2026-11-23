import React, { useState } from 'react';
import { AppPluginMeta, PluginConfigPageProps } from '@grafana/data';
import { getBackendSrv } from '@grafana/runtime';
import {
  Alert,
  Button,
  Checkbox,
  Field,
  FieldSet,
  Input,
  SecretInput,
  Select,
} from '@grafana/ui';
import { lastValueFrom } from 'rxjs';
import { extractApiError } from '../../api/studioClient';

export interface StudioJsonData {
  allowedDatasourceUids?: string[];
  llmMode?: 'grafana-llm-app' | 'mock';
  allowMockInDev?: boolean;
  grafanaUrl?: string;
  modelTier?: 'base' | 'large';
  reasoningEffort?: string;
  maxPanels?: number;
  maxRepairAttempts?: number;
  maxOutputTokens?: number;
  llmTimeoutSeconds?: number;
  jobDeadlineSeconds?: number;
  maxConcurrentJobs?: number;
  validationTimeoutSeconds?: number;
  maxConcurrentQueries?: number;
}

export const AppConfig: React.FC<PluginConfigPageProps<AppPluginMeta<StudioJsonData>>> = ({ plugin }) => {
  const { enabled, pinned, jsonData, secureJsonFields } = plugin.meta;

  const [allowedDsText, setAllowedDsText] = useState((jsonData?.allowedDatasourceUids ?? []).join(', '));
  const [llmMode, setLlmMode] = useState<'grafana-llm-app' | 'mock'>(jsonData?.llmMode ?? 'grafana-llm-app');
  const [allowMockInDev, setAllowMockInDev] = useState(Boolean(jsonData?.allowMockInDev));
  const [grafanaUrl, setGrafanaUrl] = useState(jsonData?.grafanaUrl ?? 'http://localhost:3000');
  const [modelTier, setModelTier] = useState<'base' | 'large'>(jsonData?.modelTier ?? 'base');
  const [reasoningEffort, setReasoningEffort] = useState(jsonData?.reasoningEffort ?? 'low');
  const [maxPanels, setMaxPanels] = useState(jsonData?.maxPanels ?? 6);
  const [maxRepairAttempts, setMaxRepairAttempts] = useState(jsonData?.maxRepairAttempts ?? 2);
  const [maxOutputTokens, setMaxOutputTokens] = useState(jsonData?.maxOutputTokens ?? 2048);
  const [llmTimeoutSeconds, setLlmTimeoutSeconds] = useState(jsonData?.llmTimeoutSeconds ?? 45);
  const [jobDeadlineSeconds, setJobDeadlineSeconds] = useState(jsonData?.jobDeadlineSeconds ?? 90);
  const [maxConcurrentJobs, setMaxConcurrentJobs] = useState(jsonData?.maxConcurrentJobs ?? 4);
  const [validationTimeoutSeconds, setValidationTimeoutSeconds] = useState(
    jsonData?.validationTimeoutSeconds ?? 10
  );
  const [maxConcurrentQueries, setMaxConcurrentQueries] = useState(
    jsonData?.maxConcurrentQueries ?? 2
  );

  const [isTokenConfigured, setIsTokenConfigured] = useState(
    Boolean(secureJsonFields?.llmServiceAccountToken)
  );
  const [wasTokenReset, setWasTokenReset] = useState(false);
  const [llmServiceAccountToken, setLlmServiceAccountToken] = useState('');

  const [saving, setSaving] = useState(false);
  const [statusMessage, setStatusMessage] = useState<{ severity: 'success' | 'error'; text: string } | null>(
    null
  );

  const validateBeforeSave = (): string | null => {
    if (llmMode === 'mock' && !allowMockInDev) {
      return 'llmMode=mock を保存するには allowMockInDev を明示的に有効化してください。';
    }
    const checks: Array<[string, number, number, number]> = [
      ['最大パネル数', maxPanels, 1, 6],
      ['自動構文修復の最大回数', maxRepairAttempts, 0, 2],
      ['LLM 最大出力トークン数', maxOutputTokens, 256, 8192],
      ['LLM タイムアウト(秒)', llmTimeoutSeconds, 5, 120],
      ['ジョブ全体期限(秒)', jobDeadlineSeconds, 10, 180],
      ['LLM 同時実行ジョブ上限', maxConcurrentJobs, 1, 16],
      ['検証クエリタイムアウト(秒)', validationTimeoutSeconds, 2, 30],
      ['データソース並列クエリ上限', maxConcurrentQueries, 1, 4],
    ];
    for (const [label, val, minVal, maxVal] of checks) {
      if (!Number.isFinite(val) || val < minVal || val > maxVal) {
        return `${label} は ${minVal}〜${maxVal} の数値で指定してください。`;
      }
    }
    const normalizedEffort = reasoningEffort.trim().toLowerCase();
    if (!['', 'low', 'medium', 'high'].includes(normalizedEffort)) {
      return 'Reasoning Effort は空欄または low / medium / high のいずれかを指定してください。';
    }
    return null;
  };

  const handleSave = async () => {
    const validationErr = validateBeforeSave();
    if (validationErr) {
      setStatusMessage({ severity: 'error', text: validationErr });
      return;
    }

    setSaving(true);
    setStatusMessage(null);
    try {
      const allowedDatasourceUids = allowedDsText
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);

      // Preserve any existing jsonData fields (H-11) and normalize allowMockInDev=false outside mock mode (M-13).
      const nextJsonData: StudioJsonData = {
        ...(jsonData ?? {}),
        allowedDatasourceUids,
        llmMode,
        allowMockInDev: llmMode === 'mock' ? allowMockInDev : false,
        grafanaUrl: grafanaUrl.trim(),
        modelTier,
        reasoningEffort: reasoningEffort.trim().toLowerCase(),
        maxPanels: Math.trunc(Number(maxPanels)),
        maxRepairAttempts: Math.trunc(Number(maxRepairAttempts)),
        maxOutputTokens: Math.trunc(Number(maxOutputTokens)),
        llmTimeoutSeconds: Math.trunc(Number(llmTimeoutSeconds)),
        jobDeadlineSeconds: Math.trunc(Number(jobDeadlineSeconds)),
        maxConcurrentJobs: Math.trunc(Number(maxConcurrentJobs)),
        validationTimeoutSeconds: Math.trunc(Number(validationTimeoutSeconds)),
        maxConcurrentQueries: Math.trunc(Number(maxConcurrentQueries)),
      };

      // Explicitly send empty string on reset so Grafana clears the stored encrypted secret (H-11).
      let secureJsonData: Record<string, string> | undefined;
      if (llmServiceAccountToken.trim().length > 0) {
        secureJsonData = { llmServiceAccountToken: llmServiceAccountToken.trim() };
      } else if (wasTokenReset) {
        secureJsonData = { llmServiceAccountToken: '' };
      }

      await lastValueFrom(
        getBackendSrv().fetch({
          url: `/api/plugins/${plugin.meta.id}/settings`,
          method: 'POST',
          showErrorAlert: false,
          data: {
            enabled,
            pinned,
            jsonData: nextJsonData,
            ...(secureJsonData ? { secureJsonData } : {}),
          },
        })
      );

      if (llmServiceAccountToken.trim().length > 0) {
        setIsTokenConfigured(true);
        setWasTokenReset(false);
        setLlmServiceAccountToken('');
      } else if (wasTokenReset) {
        setIsTokenConfigured(false);
        setWasTokenReset(false);
      }
      setStatusMessage({
        severity: 'success',
        text: '設定を保存しました。変更を反映するためページを再読み込みしてください。',
      });
    } catch (err: unknown) {
      setStatusMessage({
        severity: 'error',
        text: `設定保存に失敗しました: ${extractApiError(err)}`,
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div style={{ maxWidth: 760 }}>
      <Alert title="権限とセキュリティ方針" severity="info">
        LLM 呼び出しはすべて grafana-llm-app に一本化されます。ここに入力するサービスアカウントトークンは
        secureJsonData として暗号化保存され、LLM 中継以外のデータ取得・ダッシュボード保存には一切流用されません。
      </Alert>

      {statusMessage && (
        <Alert title={statusMessage.severity === 'success' ? '保存完了' : 'エラー'} severity={statusMessage.severity}>
          {statusMessage.text}
        </Alert>
      )}

      <FieldSet label="データソース許可設定（MVP: 単一 Organization・Prometheus 互換）">
        <Field
          label="許可する Prometheus データソース UID（カンマ区切り）"
          description="ここで許可されたデータソースのみ Studio で選択可能になります。LLM が任意の UID を指定することはできません。"
        >
          <Input
            value={allowedDsText}
            onChange={(e) => setAllowedDsText(e.currentTarget.value)}
            placeholder="prom-main"
          />
        </Field>
      </FieldSet>

      <FieldSet label="LLM 接続設定（grafana-llm-app 一本化）">
        <Field label="LLM モード">
          <Select
            options={[
              { label: 'grafana-llm-app（本番推奨・ローカル vLLM 中継）', value: 'grafana-llm-app' },
              { label: 'mock（開発・テスト専用）', value: 'mock' },
            ]}
            value={llmMode}
            onChange={(v) => setLlmMode((v.value as 'grafana-llm-app' | 'mock') ?? 'grafana-llm-app')}
          />
        </Field>

        {llmMode === 'mock' && (
          <Field
            label="開発・テスト用モックモードを明示的に許可"
            description="本番運用では必ず無効にしてください。実 LLM 障害時にモックへ自動フォールバックすることはありません。"
          >
            <Checkbox
              value={allowMockInDev}
              onChange={(e) => setAllowMockInDev(e.currentTarget.checked)}
              label="allowMockInDev を有効化"
            />
          </Field>
        )}

        <Field
          label="Grafana 内部 URL"
          description="Go Backend の llmclient が /api/plugins/grafana-llm-app/resources/llm/v1 へ接続する URL です（loopback または Grafana AppURL 同一オリジンのみ許可）。"
        >
          <Input
            value={grafanaUrl}
            onChange={(e) => setGrafanaUrl(e.currentTarget.value)}
            placeholder="http://localhost:3000"
          />
        </Field>

        <Field label="grafana-llm-app モデル抽象化名">
          <Select
            options={[
              { label: 'base（高速・高スループット）', value: 'base' },
              { label: 'large（高精度）', value: 'large' },
            ]}
            value={modelTier}
            onChange={(v) => setModelTier((v.value as 'base' | 'large') ?? 'base')}
          />
        </Field>

        <Field
          label="Reasoning Effort（推論トークン抑制用・任意）"
          description="空欄または low / medium / high。GLM-5.3 等の長考抑制には low を推奨します。"
        >
          <Input
            value={reasoningEffort}
            onChange={(e) => setReasoningEffort(e.currentTarget.value)}
            placeholder="low"
          />
        </Field>

        <Field
          label="LLM 専用サービスアカウントトークン (secureJsonData)"
          description="Grafana OSS では最小基本ロール Viewer を割り当て、llmclient 専用とします（リセットして空欄保存すると既存トークンを削除します）。"
        >
          <SecretInput
            isConfigured={isTokenConfigured}
            value={llmServiceAccountToken}
            onChange={(e) => setLlmServiceAccountToken(e.currentTarget.value)}
            onReset={() => {
              setIsTokenConfigured(false);
              setWasTokenReset(true);
              setLlmServiceAccountToken('');
            }}
            placeholder="glsa_..."
          />
        </Field>
      </FieldSet>

      <FieldSet label="予算・クエリ上限（初期仮設定・環境に応じて調整）">
        <Field label="最大パネル数 (1..6)">
          <Input
            type="number"
            min={1}
            max={6}
            value={maxPanels}
            onChange={(e) => setMaxPanels(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="自動構文修復の最大回数 (0..2、0で無効)">
          <Input
            type="number"
            min={0}
            max={2}
            value={maxRepairAttempts}
            onChange={(e) => setMaxRepairAttempts(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="LLM 最大出力トークン数 (256..8192)">
          <Input
            type="number"
            min={256}
            max={8192}
            value={maxOutputTokens}
            onChange={(e) => setMaxOutputTokens(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="LLM リクエストタイムアウト (秒, 5..120)">
          <Input
            type="number"
            min={5}
            max={120}
            value={llmTimeoutSeconds}
            onChange={(e) => setLlmTimeoutSeconds(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="ジョブ全体期限 (秒, 10..180)">
          <Input
            type="number"
            min={10}
            max={180}
            value={jobDeadlineSeconds}
            onChange={(e) => setJobDeadlineSeconds(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="LLM 同時実行ジョブ上限 (1..16)">
          <Input
            type="number"
            min={1}
            max={16}
            value={maxConcurrentJobs}
            onChange={(e) => setMaxConcurrentJobs(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="検証クエリタイムアウト (秒, 2..30)">
          <Input
            type="number"
            min={2}
            max={30}
            value={validationTimeoutSeconds}
            onChange={(e) => setValidationTimeoutSeconds(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="データソース並列クエリ上限 (1..4)">
          <Input
            type="number"
            min={1}
            max={4}
            value={maxConcurrentQueries}
            onChange={(e) => setMaxConcurrentQueries(Number(e.currentTarget.value))}
          />
        </Field>
      </FieldSet>

      <Button onClick={handleSave} disabled={saving}>
        {saving ? '保存中...' : '設定を保存'}
      </Button>
    </div>
  );
};
