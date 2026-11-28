import React from 'react';
import { AppPluginMeta, PluginConfigPageProps } from '@grafana/data';
import { Alert, Button } from '@grafana/ui';
import { useStudioTranslation } from '../../i18n';
import { BudgetsAndLimitsSection } from './sections/BudgetsAndLimitsSection';
import { DatasourceAllowlistSection } from './sections/DatasourceAllowlistSection';
import { JobStorageSection } from './sections/JobStorageSection';
import { LlmConnectionSection } from './sections/LlmConnectionSection';
import { StudioJsonData } from './types';
import { useAppConfigState } from './useAppConfigState';

export type { JobStoreBackend, PostgresSslMode, StudioJsonData } from './types';
export {
  BOUND_GRAFANA_TOKEN_PREFIX,
  BOUND_POSTGRES_PASSWORD_PREFIX,
  BOUND_REDIS_PASSWORD_PREFIX,
  encodeBoundGrafanaToken,
  encodeBoundPostgresPassword,
  encodeBoundRedisPassword,
} from './secretBinding';
export { isValidRedisHostPort } from './validation';

export const AppConfig: React.FC<PluginConfigPageProps<AppPluginMeta<StudioJsonData>>> = ({ plugin }) => {
  const { t } = useStudioTranslation();
  const state = useAppConfigState(plugin.meta, t);

  return (
    <div style={{ maxWidth: 760 }}>
      <Alert title={t('appConfig.securityAlert.title')} severity="info">
        {t('appConfig.securityAlert.body')}
      </Alert>

      {state.statusMessage && (
        <Alert
          title={
            state.statusMessage.severity === 'success'
              ? t('appConfig.status.savedTitle')
              : t('appConfig.status.errorTitle')
          }
          severity={state.statusMessage.severity}
        >
          {state.statusMessage.text}
        </Alert>
      )}

      <DatasourceAllowlistSection
        t={t}
        allowedDsText={state.allowedDsText}
        onAllowedDsTextChange={state.setAllowedDsText}
        parsedAllowedUids={state.parsedAllowedUids}
        datasourceDialects={state.datasourceDialects}
        onDialectChange={(uid, dialect) =>
          state.setDatasourceDialects((prev) => ({ ...prev, [uid]: dialect }))
        }
      />

      <LlmConnectionSection
        t={t}
        llmMode={state.llmMode}
        onLlmModeChange={state.setLlmMode}
        allowMockInDev={state.allowMockInDev}
        onAllowMockInDevChange={state.setAllowMockInDev}
        grafanaUrl={state.grafanaUrl}
        onGrafanaUrlChange={state.setGrafanaUrl}
        modelTier={state.modelTier}
        onModelTierChange={state.setModelTier}
        structuredOutputMode={state.structuredOutputMode}
        onStructuredOutputModeChange={state.setStructuredOutputMode}
        tokenLimitField={state.tokenLimitField}
        onTokenLimitFieldChange={state.setTokenLimitField}
        reasoningEffort={state.reasoningEffort}
        onReasoningEffortChange={state.setReasoningEffort}
        isTokenConfigured={state.isTokenConfigured}
        llmServiceAccountToken={state.llmServiceAccountToken}
        onLlmServiceAccountTokenChange={state.setLlmServiceAccountToken}
        onResetLlmToken={state.resetLlmToken}
        enableSseProgress={state.enableSseProgress}
        onEnableSseProgressChange={state.setEnableSseProgress}
      />

      <BudgetsAndLimitsSection
        t={t}
        maxPanels={state.maxPanels}
        onMaxPanelsChange={state.setMaxPanels}
        maxRepairAttempts={state.maxRepairAttempts}
        onMaxRepairAttemptsChange={state.setMaxRepairAttempts}
        maxOutputTokens={state.maxOutputTokens}
        onMaxOutputTokensChange={state.setMaxOutputTokens}
        llmTimeoutSeconds={state.llmTimeoutSeconds}
        onLlmTimeoutSecondsChange={state.setLlmTimeoutSeconds}
        jobDeadlineSeconds={state.jobDeadlineSeconds}
        onJobDeadlineSecondsChange={state.setJobDeadlineSeconds}
        maxConcurrentJobs={state.maxConcurrentJobs}
        onMaxConcurrentJobsChange={state.setMaxConcurrentJobs}
        validationTimeoutSeconds={state.validationTimeoutSeconds}
        onValidationTimeoutSecondsChange={state.setValidationTimeoutSeconds}
        maxConcurrentQueries={state.maxConcurrentQueries}
        onMaxConcurrentQueriesChange={state.setMaxConcurrentQueries}
      />

      <JobStorageSection
        t={t}
        jobStoreBackend={state.jobStoreBackend}
        onJobStoreBackendChange={state.handleJobStoreBackendChange}
        jobTtlMinutes={state.jobTtlMinutes}
        onJobTtlMinutesChange={state.setJobTtlMinutes}
        redisAddr={state.redisAddr}
        onRedisAddrChange={state.setRedisAddr}
        redisDb={state.redisDb}
        onRedisDbChange={state.setRedisDb}
        redisKeyPrefix={state.redisKeyPrefix}
        onRedisKeyPrefixChange={state.setRedisKeyPrefix}
        redisUsername={state.redisUsername}
        onRedisUsernameChange={state.setRedisUsername}
        isRedisPasswordConfigured={state.isRedisPasswordConfigured}
        redisPassword={state.redisPassword}
        onRedisPasswordChange={state.setRedisPassword}
        onResetRedisPassword={state.resetRedisPassword}
        redisTls={state.redisTls}
        onRedisTlsChange={state.setRedisTls}
        postgresAddr={state.postgresAddr}
        onPostgresAddrChange={state.setPostgresAddr}
        postgresDatabase={state.postgresDatabase}
        onPostgresDatabaseChange={state.setPostgresDatabase}
        postgresSchema={state.postgresSchema}
        onPostgresSchemaChange={state.setPostgresSchema}
        postgresTablePrefix={state.postgresTablePrefix}
        onPostgresTablePrefixChange={state.setPostgresTablePrefix}
        postgresUsername={state.postgresUsername}
        onPostgresUsernameChange={state.setPostgresUsername}
        isPostgresPasswordConfigured={state.isPostgresPasswordConfigured}
        postgresPassword={state.postgresPassword}
        onPostgresPasswordChange={state.setPostgresPassword}
        onResetPostgresPassword={state.resetPostgresPassword}
        postgresSslMode={state.postgresSslMode}
        onPostgresSslModeChange={state.setPostgresSslMode}
      />

      <Button onClick={state.handleSave} disabled={state.saving}>
        {state.saving ? t('appConfig.actions.saving') : t('appConfig.actions.save')}
      </Button>
    </div>
  );
};
