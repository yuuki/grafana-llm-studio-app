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
  const [validationTimeoutSeconds, setValidationTimeoutSeconds] = useState(
    jsonData?.validationTimeoutSeconds ?? 10
  );
  const [maxConcurrentQueries, setMaxConcurrentQueries] = useState(
    jsonData?.maxConcurrentQueries ?? 2
  );

  const [isTokenConfigured, setIsTokenConfigured] = useState(
    Boolean(secureJsonFields?.llmServiceAccountToken)
  );
  const [llmServiceAccountToken, setLlmServiceAccountToken] = useState('');

  const [saving, setSaving] = useState(false);
  const [statusMessage, setStatusMessage] = useState<{ severity: 'success' | 'error'; text: string } | null>(
    null
  );

  const handleSave = async () => {
    setSaving(true);
    setStatusMessage(null);
    try {
      const allowedDatasourceUids = allowedDsText
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);

      const nextJsonData: StudioJsonData = {
        allowedDatasourceUids,
        llmMode,
        allowMockInDev,
        grafanaUrl: grafanaUrl.trim(),
        modelTier,
        reasoningEffort: reasoningEffort.trim() || undefined,
        maxPanels: Number(maxPanels),
        maxRepairAttempts: Number(maxRepairAttempts),
        maxOutputTokens: Number(maxOutputTokens),
        llmTimeoutSeconds: Number(llmTimeoutSeconds),
        validationTimeoutSeconds: Number(validationTimeoutSeconds),
        maxConcurrentQueries: Number(maxConcurrentQueries),
      };

      const secureJsonData =
        llmServiceAccountToken.trim().length > 0
          ? { llmServiceAccountToken: llmServiceAccountToken.trim() }
          : undefined;

      await lastValueFrom(
        getBackendSrv().fetch({
          url: `/api/plugins/${plugin.meta.id}/settings`,
          method: 'POST',
          data: {
            enabled,
            pinned,
            jsonData: nextJsonData,
            ...(secureJsonData ? { secureJsonData } : {}),
          },
        })
      );

      if (secureJsonData) {
        setIsTokenConfigured(true);
        setLlmServiceAccountToken('');
      }
      setStatusMessage({
        severity: 'success',
        text: '設定を保存しました。変更を反映するためページを再読み込みしてください。',
      });
    } catch (err: unknown) {
      setStatusMessage({
        severity: 'error',
        text: `設定保存に失敗しました: ${err instanceof Error ? err.message : String(err)}`,
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
          description="Go Backend の llmclient が /api/plugins/grafana-llm-app/resources/llm/v1 へ接続する URL です。"
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
          description="GLM-5.3 等でデフォルト max による長考・トークン上限超過を避ける場合は low を推奨します。"
        >
          <Input
            value={reasoningEffort}
            onChange={(e) => setReasoningEffort(e.currentTarget.value)}
            placeholder="low"
          />
        </Field>

        <Field
          label="LLM 専用サービスアカウントトークン (secureJsonData)"
          description="Grafana OSS では最小基本ロール Viewer を割り当て、llmclient 専用とします（managed SA が有効な場合は空欄可）。"
        >
          <SecretInput
            isConfigured={isTokenConfigured}
            value={llmServiceAccountToken}
            onChange={(e) => setLlmServiceAccountToken(e.currentTarget.value)}
            onReset={() => {
              setIsTokenConfigured(false);
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
        <Field label="自動構文修復の最大回数 (1..2)">
          <Input
            type="number"
            min={1}
            max={2}
            value={maxRepairAttempts}
            onChange={(e) => setMaxRepairAttempts(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="LLM 最大出力トークン数">
          <Input
            type="number"
            min={256}
            max={8192}
            value={maxOutputTokens}
            onChange={(e) => setMaxOutputTokens(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="LLM リクエストタイムアウト (秒)">
          <Input
            type="number"
            min={5}
            max={120}
            value={llmTimeoutSeconds}
            onChange={(e) => setLlmTimeoutSeconds(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label="検証クエリタイムアウト (秒)">
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
