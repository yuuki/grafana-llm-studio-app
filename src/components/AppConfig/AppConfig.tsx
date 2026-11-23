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
import { useStudioTranslation } from '../../i18n';

export interface StudioJsonData {
  allowedDatasourceUids?: string[];
  llmMode?: 'grafana-llm-app' | 'mock';
  allowMockInDev?: boolean;
  grafanaUrl?: string;
  modelTier?: 'base' | 'large';
  structuredOutputMode?: 'json_schema' | 'json_object';
  tokenLimitField?: 'max_completion_tokens' | 'max_tokens';
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
  const { t } = useStudioTranslation();
  const { enabled, pinned, jsonData, secureJsonFields } = plugin.meta;

  const [allowedDsText, setAllowedDsText] = useState((jsonData?.allowedDatasourceUids ?? []).join(', '));
  const [llmMode, setLlmMode] = useState<'grafana-llm-app' | 'mock'>(jsonData?.llmMode ?? 'grafana-llm-app');
  const [allowMockInDev, setAllowMockInDev] = useState(Boolean(jsonData?.allowMockInDev));
  const [grafanaUrl, setGrafanaUrl] = useState(jsonData?.grafanaUrl ?? 'http://localhost:3000');
  const [modelTier, setModelTier] = useState<'base' | 'large'>(jsonData?.modelTier ?? 'base');
  const [structuredOutputMode, setStructuredOutputMode] = useState<'json_schema' | 'json_object'>(
    jsonData?.structuredOutputMode ?? 'json_schema'
  );
  const [tokenLimitField, setTokenLimitField] = useState<'max_completion_tokens' | 'max_tokens'>(
    jsonData?.tokenLimitField ?? 'max_completion_tokens'
  );
  const [reasoningEffort, setReasoningEffort] = useState(jsonData?.reasoningEffort ?? '');
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
      return t('appConfig.validation.allowMockRequired');
    }
    const checks: Array<[string, number, number, number]> = [
      [t('appConfig.validation.fields.maxPanels'), maxPanels, 1, 6],
      [t('appConfig.validation.fields.maxRepairAttempts'), maxRepairAttempts, 0, 2],
      [t('appConfig.validation.fields.maxOutputTokens'), maxOutputTokens, 256, 8192],
      [t('appConfig.validation.fields.llmTimeoutSeconds'), llmTimeoutSeconds, 5, 120],
      [t('appConfig.validation.fields.jobDeadlineSeconds'), jobDeadlineSeconds, 10, 180],
      [t('appConfig.validation.fields.maxConcurrentJobs'), maxConcurrentJobs, 1, 16],
      [t('appConfig.validation.fields.validationTimeoutSeconds'), validationTimeoutSeconds, 2, 30],
      [t('appConfig.validation.fields.maxConcurrentQueries'), maxConcurrentQueries, 1, 4],
    ];
    for (const [label, val, minVal, maxVal] of checks) {
      if (!Number.isFinite(val) || val < minVal || val > maxVal) {
        return t('appConfig.validation.numberRange', { label, min: minVal, max: maxVal });
      }
    }
    const normalizedEffort = reasoningEffort.trim().toLowerCase();
    if (!['', 'low', 'medium', 'high'].includes(normalizedEffort)) {
      return t('appConfig.validation.reasoningEffort');
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
        structuredOutputMode,
        tokenLimitField,
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
        text: t('appConfig.status.savedMessage'),
      });
    } catch (err: unknown) {
      setStatusMessage({
        severity: 'error',
        text: t('appConfig.status.saveFailed', { error: extractApiError(err) }),
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div style={{ maxWidth: 760 }}>
      <Alert title={t('appConfig.securityAlert.title')} severity="info">
        {t('appConfig.securityAlert.body')}
      </Alert>

      {statusMessage && (
        <Alert
          title={
            statusMessage.severity === 'success'
              ? t('appConfig.status.savedTitle')
              : t('appConfig.status.errorTitle')
          }
          severity={statusMessage.severity}
        >
          {statusMessage.text}
        </Alert>
      )}

      <FieldSet label={t('appConfig.datasource.fieldset')}>
        <Field
          label={t('appConfig.datasource.allowedUidsLabel')}
          description={t('appConfig.datasource.allowedUidsDescription')}
        >
          <Input
            value={allowedDsText}
            onChange={(e) => setAllowedDsText(e.currentTarget.value)}
            placeholder="prom-main"
          />
        </Field>
      </FieldSet>

      <FieldSet label={t('appConfig.llm.fieldset')}>
        <Field label={t('appConfig.llm.modeLabel')}>
          <Select
            options={[
              { label: t('appConfig.llm.modeGrafanaLlmApp'), value: 'grafana-llm-app' },
              { label: t('appConfig.llm.modeMock'), value: 'mock' },
            ]}
            value={llmMode}
            onChange={(v) => setLlmMode((v.value as 'grafana-llm-app' | 'mock') ?? 'grafana-llm-app')}
          />
        </Field>

        {llmMode === 'mock' && (
          <Field
            label={t('appConfig.llm.allowMockLabel')}
            description={t('appConfig.llm.allowMockDescription')}
          >
            <Checkbox
              value={allowMockInDev}
              onChange={(e) => setAllowMockInDev(e.currentTarget.checked)}
              label={t('appConfig.llm.allowMockCheckbox')}
            />
          </Field>
        )}

        <Field
          label={t('appConfig.llm.grafanaUrlLabel')}
          description={t('appConfig.llm.grafanaUrlDescription')}
        >
          <Input
            value={grafanaUrl}
            onChange={(e) => setGrafanaUrl(e.currentTarget.value)}
            placeholder="http://localhost:3000"
          />
        </Field>

        <Field label={t('appConfig.llm.modelTierLabel')}>
          <Select
            options={[
              { label: t('appConfig.llm.modelTierBase'), value: 'base' },
              { label: t('appConfig.llm.modelTierLarge'), value: 'large' },
            ]}
            value={modelTier}
            onChange={(v) => setModelTier((v.value as 'base' | 'large') ?? 'base')}
          />
        </Field>

        <Field
          label={t('appConfig.llm.structuredOutputLabel')}
          description={t('appConfig.llm.structuredOutputDescription')}
        >
          <Select
            options={[
              { label: t('appConfig.llm.structuredOutputSchema'), value: 'json_schema' },
              { label: t('appConfig.llm.structuredOutputObject'), value: 'json_object' },
            ]}
            value={structuredOutputMode}
            onChange={(v) => setStructuredOutputMode((v.value as 'json_schema' | 'json_object') ?? 'json_schema')}
          />
        </Field>

        <Field
          label={t('appConfig.llm.tokenLimitLabel')}
          description={t('appConfig.llm.tokenLimitDescription')}
        >
          <Select
            options={[
              { label: t('appConfig.llm.tokenLimitCompletion'), value: 'max_completion_tokens' },
              { label: t('appConfig.llm.tokenLimitLegacy'), value: 'max_tokens' },
            ]}
            value={tokenLimitField}
            onChange={(v) =>
              setTokenLimitField((v.value as 'max_completion_tokens' | 'max_tokens') ?? 'max_completion_tokens')
            }
          />
        </Field>

        <Field
          label={t('appConfig.llm.reasoningEffortLabel')}
          description={t('appConfig.llm.reasoningEffortDescription')}
        >
          <Select
            options={[
              { label: t('appConfig.llm.reasoningEffortAuto'), value: '' },
              { label: t('appConfig.llm.reasoningEffortLow'), value: 'low' },
              { label: t('appConfig.llm.reasoningEffortMedium'), value: 'medium' },
              { label: t('appConfig.llm.reasoningEffortHigh'), value: 'high' },
            ]}
            value={reasoningEffort}
            onChange={(v) => setReasoningEffort(v.value ?? '')}
          />
        </Field>

        <Field
          label={t('appConfig.llm.saTokenLabel')}
          description={t('appConfig.llm.saTokenDescription')}
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

      <FieldSet label={t('appConfig.limits.fieldset')}>
        <Field label={t('appConfig.limits.maxPanels')}>
          <Input
            type="number"
            min={1}
            max={6}
            value={maxPanels}
            onChange={(e) => setMaxPanels(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label={t('appConfig.limits.maxRepairAttempts')}>
          <Input
            type="number"
            min={0}
            max={2}
            value={maxRepairAttempts}
            onChange={(e) => setMaxRepairAttempts(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label={t('appConfig.limits.maxOutputTokens')}>
          <Input
            type="number"
            min={256}
            max={8192}
            value={maxOutputTokens}
            onChange={(e) => setMaxOutputTokens(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label={t('appConfig.limits.llmTimeoutSeconds')}>
          <Input
            type="number"
            min={5}
            max={120}
            value={llmTimeoutSeconds}
            onChange={(e) => setLlmTimeoutSeconds(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label={t('appConfig.limits.jobDeadlineSeconds')}>
          <Input
            type="number"
            min={10}
            max={180}
            value={jobDeadlineSeconds}
            onChange={(e) => setJobDeadlineSeconds(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label={t('appConfig.limits.maxConcurrentJobs')}>
          <Input
            type="number"
            min={1}
            max={16}
            value={maxConcurrentJobs}
            onChange={(e) => setMaxConcurrentJobs(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label={t('appConfig.limits.validationTimeoutSeconds')}>
          <Input
            type="number"
            min={2}
            max={30}
            value={validationTimeoutSeconds}
            onChange={(e) => setValidationTimeoutSeconds(Number(e.currentTarget.value))}
          />
        </Field>
        <Field label={t('appConfig.limits.maxConcurrentQueries')}>
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
        {saving ? t('appConfig.actions.saving') : t('appConfig.actions.save')}
      </Button>
    </div>
  );
};
