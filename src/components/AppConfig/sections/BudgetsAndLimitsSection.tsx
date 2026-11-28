import React from 'react';
import { Field, FieldSet, Input } from '@grafana/ui';
import { formatNumberValue, parseNumberInput, TranslateFn } from '../validation';

export interface BudgetsAndLimitsSectionProps {
  t: TranslateFn;
  maxPanels: number;
  onMaxPanelsChange: (val: number) => void;
  maxRepairAttempts: number;
  onMaxRepairAttemptsChange: (val: number) => void;
  maxOutputTokens: number;
  onMaxOutputTokensChange: (val: number) => void;
  llmTimeoutSeconds: number;
  onLlmTimeoutSecondsChange: (val: number) => void;
  jobDeadlineSeconds: number;
  onJobDeadlineSecondsChange: (val: number) => void;
  maxConcurrentJobs: number;
  onMaxConcurrentJobsChange: (val: number) => void;
  validationTimeoutSeconds: number;
  onValidationTimeoutSecondsChange: (val: number) => void;
  maxConcurrentQueries: number;
  onMaxConcurrentQueriesChange: (val: number) => void;
}

export const BudgetsAndLimitsSection: React.FC<BudgetsAndLimitsSectionProps> = ({
  t,
  maxPanels,
  onMaxPanelsChange,
  maxRepairAttempts,
  onMaxRepairAttemptsChange,
  maxOutputTokens,
  onMaxOutputTokensChange,
  llmTimeoutSeconds,
  onLlmTimeoutSecondsChange,
  jobDeadlineSeconds,
  onJobDeadlineSecondsChange,
  maxConcurrentJobs,
  onMaxConcurrentJobsChange,
  validationTimeoutSeconds,
  onValidationTimeoutSecondsChange,
  maxConcurrentQueries,
  onMaxConcurrentQueriesChange,
}) => (
  <FieldSet label={t('appConfig.limits.fieldset')}>
    <Field label={t('appConfig.limits.maxPanels')}>
      <Input
        type="number"
        min={1}
        max={6}
        value={formatNumberValue(maxPanels)}
        onChange={(e) => onMaxPanelsChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
    <Field label={t('appConfig.limits.maxRepairAttempts')}>
      <Input
        type="number"
        min={0}
        max={2}
        value={formatNumberValue(maxRepairAttempts)}
        onChange={(e) => onMaxRepairAttemptsChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
    <Field label={t('appConfig.limits.maxOutputTokens')}>
      <Input
        type="number"
        min={256}
        max={8192}
        value={formatNumberValue(maxOutputTokens)}
        onChange={(e) => onMaxOutputTokensChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
    <Field label={t('appConfig.limits.llmTimeoutSeconds')}>
      <Input
        type="number"
        min={5}
        max={120}
        value={formatNumberValue(llmTimeoutSeconds)}
        onChange={(e) => onLlmTimeoutSecondsChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
    <Field label={t('appConfig.limits.jobDeadlineSeconds')}>
      <Input
        type="number"
        min={10}
        max={180}
        value={formatNumberValue(jobDeadlineSeconds)}
        onChange={(e) => onJobDeadlineSecondsChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
    <Field label={t('appConfig.limits.maxConcurrentJobs')}>
      <Input
        type="number"
        min={1}
        max={16}
        value={formatNumberValue(maxConcurrentJobs)}
        onChange={(e) => onMaxConcurrentJobsChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
    <Field label={t('appConfig.limits.validationTimeoutSeconds')}>
      <Input
        type="number"
        min={2}
        max={30}
        value={formatNumberValue(validationTimeoutSeconds)}
        onChange={(e) => onValidationTimeoutSecondsChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
    <Field label={t('appConfig.limits.maxConcurrentQueries')}>
      <Input
        type="number"
        min={1}
        max={4}
        value={formatNumberValue(maxConcurrentQueries)}
        onChange={(e) => onMaxConcurrentQueriesChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>
  </FieldSet>
);
