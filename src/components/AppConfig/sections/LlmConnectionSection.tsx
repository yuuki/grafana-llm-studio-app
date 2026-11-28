import React from 'react';
import { Checkbox, Field, FieldSet, Input, SecretInput, Select } from '@grafana/ui';
import { TranslateFn } from '../validation';

export interface LlmConnectionSectionProps {
  t: TranslateFn;
  llmMode: 'grafana-llm-app' | 'mock';
  onLlmModeChange: (value: 'grafana-llm-app' | 'mock') => void;
  allowMockInDev: boolean;
  onAllowMockInDevChange: (checked: boolean) => void;
  grafanaUrl: string;
  onGrafanaUrlChange: (value: string) => void;
  modelTier: 'base' | 'large';
  onModelTierChange: (value: 'base' | 'large') => void;
  structuredOutputMode: 'json_schema' | 'json_object';
  onStructuredOutputModeChange: (value: 'json_schema' | 'json_object') => void;
  tokenLimitField: 'max_completion_tokens' | 'max_tokens';
  onTokenLimitFieldChange: (value: 'max_completion_tokens' | 'max_tokens') => void;
  reasoningEffort: string;
  onReasoningEffortChange: (value: string) => void;
  isTokenConfigured: boolean;
  llmServiceAccountToken: string;
  onLlmServiceAccountTokenChange: (value: string) => void;
  onResetLlmToken: () => void;
  enableSseProgress: boolean;
  onEnableSseProgressChange: (checked: boolean) => void;
}

export const LlmConnectionSection: React.FC<LlmConnectionSectionProps> = ({
  t,
  llmMode,
  onLlmModeChange,
  allowMockInDev,
  onAllowMockInDevChange,
  grafanaUrl,
  onGrafanaUrlChange,
  modelTier,
  onModelTierChange,
  structuredOutputMode,
  onStructuredOutputModeChange,
  tokenLimitField,
  onTokenLimitFieldChange,
  reasoningEffort,
  onReasoningEffortChange,
  isTokenConfigured,
  llmServiceAccountToken,
  onLlmServiceAccountTokenChange,
  onResetLlmToken,
  enableSseProgress,
  onEnableSseProgressChange,
}) => (
  <FieldSet label={t('appConfig.llm.fieldset')}>
    <Field label={t('appConfig.llm.modeLabel')}>
      <Select
        options={[
          { label: t('appConfig.llm.modeGrafanaLlmApp'), value: 'grafana-llm-app' },
          { label: t('appConfig.llm.modeMock'), value: 'mock' },
        ]}
        value={llmMode}
        onChange={(v) => onLlmModeChange((v.value as 'grafana-llm-app' | 'mock') ?? 'grafana-llm-app')}
      />
    </Field>

    {llmMode === 'mock' && (
      <Field
        label={t('appConfig.llm.allowMockLabel')}
        description={t('appConfig.llm.allowMockDescription')}
      >
        <Checkbox
          value={allowMockInDev}
          onChange={(e) => onAllowMockInDevChange(e.currentTarget.checked)}
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
        onChange={(e) => onGrafanaUrlChange(e.currentTarget.value)}
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
        onChange={(v) => onModelTierChange((v.value as 'base' | 'large') ?? 'base')}
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
        onChange={(v) => onStructuredOutputModeChange((v.value as 'json_schema' | 'json_object') ?? 'json_schema')}
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
          onTokenLimitFieldChange((v.value as 'max_completion_tokens' | 'max_tokens') ?? 'max_completion_tokens')
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
        onChange={(v) => onReasoningEffortChange(v.value ?? '')}
      />
    </Field>

    <Field
      label={t('appConfig.llm.saTokenLabel')}
      description={t('appConfig.llm.saTokenDescription')}
    >
      <SecretInput
        isConfigured={isTokenConfigured}
        value={llmServiceAccountToken}
        onChange={(e) => onLlmServiceAccountTokenChange(e.currentTarget.value)}
        onReset={onResetLlmToken}
        placeholder="glsa_..."
      />
    </Field>

    <Field
      label={t('appConfig.llm.sseProgressLabel')}
      description={t('appConfig.llm.sseProgressDescription')}
    >
      <Checkbox
        value={enableSseProgress}
        onChange={(e) => onEnableSseProgressChange(e.currentTarget.checked)}
        label={t('appConfig.llm.sseProgressCheckbox')}
      />
    </Field>
  </FieldSet>
);
