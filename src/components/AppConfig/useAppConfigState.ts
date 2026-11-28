import { useState } from 'react';
import { AppPluginMeta } from '@grafana/data';
import { getBackendSrv } from '@grafana/runtime';
import { lastValueFrom } from 'rxjs';
import { extractApiError } from '../../api/studioClient';
import { QueryKind } from '../../ir/types';
import {
  encodeBoundGrafanaToken,
  encodeBoundPostgresPassword,
  encodeBoundRedisPassword,
} from './secretBinding';
import { JobStoreBackend, PostgresSslMode, StudioJsonData } from './types';
import { ALLOWED_POSTGRES_SSL_MODES, TranslateFn, validateAppConfig } from './validation';

function initDatasourceDialects(raw?: Record<string, QueryKind>): Record<string, QueryKind | ''> {
  const init: Record<string, QueryKind | ''> = {};
  if (!raw) {
    return init;
  }
  for (const [uid, rawKind] of Object.entries(raw)) {
    const normalizedKind = String(rawKind ?? '')
      .trim()
      .toLowerCase();
    if (uid.trim() && (normalizedKind === 'promql' || normalizedKind === 'metricsql')) {
      init[uid.trim()] = normalizedKind;
    }
  }
  return init;
}

function resolveInitialJobStoreBackend(raw?: string): JobStoreBackend {
  const normalized = String(raw ?? '')
    .trim()
    .toLowerCase();
  if (normalized === 'redis') {
    return 'redis';
  }
  if (normalized === 'postgres') {
    return 'postgres';
  }
  return 'memory';
}

function resolveInitialPostgresSslMode(raw?: string): PostgresSslMode {
  const normalized = String(raw ?? '')
    .trim()
    .toLowerCase();
  return ALLOWED_POSTGRES_SSL_MODES.has(normalized) ? (normalized as PostgresSslMode) : 'prefer';
}

function normalizeInitialLlmConfig(jsonData?: StudioJsonData) {
  const structuredMode = String(jsonData?.structuredOutputMode ?? '')
    .trim()
    .toLowerCase();
  const limitField = String(jsonData?.tokenLimitField ?? '')
    .trim()
    .toLowerCase();
  return {
    allowedDsText: (jsonData?.allowedDatasourceUids ?? []).join(', '),
    llmMode: (jsonData?.llmMode === 'mock' ? 'mock' : 'grafana-llm-app') as 'grafana-llm-app' | 'mock',
    allowMockInDev: Boolean(jsonData?.allowMockInDev),
    grafanaUrl: jsonData?.grafanaUrl ?? '',
    savedGrafanaUrl: (jsonData?.grafanaUrl ?? '').trim(),
    modelTier: (jsonData?.modelTier === 'large' ? 'large' : 'base') as 'base' | 'large',
    structuredOutputMode: (structuredMode === 'json_object' ? 'json_object' : 'json_schema') as
      | 'json_schema'
      | 'json_object',
    tokenLimitField: (limitField === 'max_tokens' ? 'max_tokens' : 'max_completion_tokens') as
      | 'max_completion_tokens'
      | 'max_tokens',
    reasoningEffort: String(jsonData?.reasoningEffort ?? '')
      .trim()
      .toLowerCase(),
    enableSseProgress: Boolean(jsonData?.enableSseProgress),
  };
}

function normalizeInitialLimitsConfig(jsonData?: StudioJsonData) {
  return {
    maxPanels: jsonData?.maxPanels ?? 6,
    maxRepairAttempts: jsonData?.maxRepairAttempts ?? 2,
    maxOutputTokens: jsonData?.maxOutputTokens ?? 2048,
    llmTimeoutSeconds: jsonData?.llmTimeoutSeconds ?? 45,
    jobDeadlineSeconds: jsonData?.jobDeadlineSeconds ?? 90,
    maxConcurrentJobs: jsonData?.maxConcurrentJobs ?? 4,
    validationTimeoutSeconds: jsonData?.validationTimeoutSeconds ?? 10,
    maxConcurrentQueries: jsonData?.maxConcurrentQueries ?? 2,
    jobTtlMinutes: jsonData?.jobTtlMinutes ?? 30,
  };
}

function normalizeInitialStoreConfig(jsonData?: StudioJsonData) {
  return {
    jobStoreBackend: resolveInitialJobStoreBackend(jsonData?.jobStoreBackend),
    redisAddr: jsonData?.redisAddr ?? '',
    savedRedisAddr: (jsonData?.redisAddr ?? '').trim(),
    redisDb: jsonData?.redisDb ?? 0,
    redisKeyPrefix: jsonData?.redisKeyPrefix ?? 'llmstudio:jobs',
    redisTls: Boolean(jsonData?.redisTls),
    redisUsername: jsonData?.redisUsername ?? '',
    postgresAddr: jsonData?.postgresAddr ?? '',
    savedPostgresAddr: (jsonData?.postgresAddr ?? '').trim(),
    postgresDatabase: jsonData?.postgresDatabase ?? '',
    postgresSchema: jsonData?.postgresSchema ?? 'public',
    postgresTablePrefix: jsonData?.postgresTablePrefix ?? 'llm_studio_',
    postgresUsername: jsonData?.postgresUsername ?? '',
    postgresSslMode: resolveInitialPostgresSslMode(jsonData?.postgresSslMode),
  };
}

function buildCleanedDialects(
  allowedDatasourceUids: string[],
  datasourceDialects: Record<string, QueryKind | ''>
): Record<string, QueryKind> {
  // Always write an explicit object (even `{}`) so H-11 `...savedJsonData` spread
  // never retains previously pinned dialects when all UIDs are reset back to Auto.
  const cleaned: Record<string, QueryKind> = {};
  for (const uid of allowedDatasourceUids) {
    const d = datasourceDialects[uid];
    if (d === 'promql' || d === 'metricsql') {
      cleaned[uid] = d;
    }
  }
  return cleaned;
}

interface SaveJsonDataDraft {
  savedJsonData: StudioJsonData;
  allowedDatasourceUids: string[];
  cleanedDialects: Record<string, QueryKind>;
  llmMode: 'grafana-llm-app' | 'mock';
  allowMockInDev: boolean;
  trimmedGrafanaUrl: string;
  modelTier: 'base' | 'large';
  structuredOutputMode: 'json_schema' | 'json_object';
  tokenLimitField: 'max_completion_tokens' | 'max_tokens';
  reasoningEffort: string;
  maxPanels: number;
  maxRepairAttempts: number;
  maxOutputTokens: number;
  llmTimeoutSeconds: number;
  jobDeadlineSeconds: number;
  maxConcurrentJobs: number;
  validationTimeoutSeconds: number;
  maxConcurrentQueries: number;
  enableSseProgress: boolean;
  jobStoreBackend: JobStoreBackend;
  jobTtlMinutes: number;
  trimmedRedisAddr: string;
  redisDb: number;
  effectiveRedisKeyPrefix: string;
  redisTls: boolean;
  redisUsername: string;
  trimmedPostgresAddr: string;
  postgresDatabase: string;
  effectivePostgresSchema: string;
  effectivePostgresTablePrefix: string;
  postgresUsername: string;
  postgresSslMode: PostgresSslMode;
}

function buildNextJsonData(draft: SaveJsonDataDraft): StudioJsonData {
  // Preserve any existing/previously saved jsonData fields (H-11), strip dev-only mockDelayMs (L-1),
  // and normalize allowMockInDev=false outside mock mode (M-13).
  // Only write backend-specific jsonData/secureJsonData fields for the active jobStoreBackend so switching
  // backends never overwrites saved settings with unvalidated hidden fields or resets hidden passwords.
  const { mockDelayMs: _stripMockDelayMs, ...preservedJsonData } = draft.savedJsonData as StudioJsonData & {
    mockDelayMs?: unknown;
  };
  return {
    ...preservedJsonData,
    allowedDatasourceUids: draft.allowedDatasourceUids,
    datasourceDialects: draft.cleanedDialects,
    llmMode: draft.llmMode,
    allowMockInDev: draft.llmMode === 'mock' ? draft.allowMockInDev : false,
    grafanaUrl: draft.trimmedGrafanaUrl,
    modelTier: draft.modelTier,
    structuredOutputMode: draft.structuredOutputMode,
    tokenLimitField: draft.tokenLimitField,
    reasoningEffort: draft.reasoningEffort.trim().toLowerCase(),
    maxPanels: draft.maxPanels,
    maxRepairAttempts: draft.maxRepairAttempts,
    maxOutputTokens: draft.maxOutputTokens,
    llmTimeoutSeconds: draft.llmTimeoutSeconds,
    jobDeadlineSeconds: draft.jobDeadlineSeconds,
    maxConcurrentJobs: draft.maxConcurrentJobs,
    validationTimeoutSeconds: draft.validationTimeoutSeconds,
    maxConcurrentQueries: draft.maxConcurrentQueries,
    enableSseProgress: draft.enableSseProgress,
    jobStoreBackend: draft.jobStoreBackend,
    jobTtlMinutes: draft.jobTtlMinutes,
    ...(draft.jobStoreBackend === 'redis'
      ? {
          redisAddr: draft.trimmedRedisAddr,
          redisDb: draft.redisDb,
          redisKeyPrefix: draft.effectiveRedisKeyPrefix,
          redisTls: draft.redisTls,
          redisUsername: draft.redisUsername.trim(),
        }
      : {}),
    ...(draft.jobStoreBackend === 'postgres'
      ? {
          postgresAddr: draft.trimmedPostgresAddr,
          postgresDatabase: draft.postgresDatabase.trim(),
          postgresSchema: draft.effectivePostgresSchema,
          postgresTablePrefix: draft.effectivePostgresTablePrefix,
          postgresUsername: draft.postgresUsername.trim(),
          postgresSslMode: draft.postgresSslMode,
        }
      : {}),
  };
}

interface SaveSecretsDraft {
  jobStoreBackend: JobStoreBackend;
  trimmedGrafanaUrl: string;
  llmServiceAccountToken: string;
  wasTokenReset: boolean;
  trimmedRedisAddr: string;
  redisTls: boolean;
  redisPassword: string;
  wasRedisPasswordReset: boolean;
  trimmedPostgresAddr: string;
  postgresSslMode: PostgresSslMode;
  postgresPassword: string;
  wasPostgresPasswordReset: boolean;
}

function buildSecureJsonData(draft: SaveSecretsDraft): Record<string, string> {
  // Explicitly send empty string on reset so Grafana clears the stored encrypted secret (H-11).
  // Bind newly saved credentials to their destination inside the encrypted secret value so changing
  // the target endpoint via UI or direct API cannot redirect an existing secret to another server.
  const secureJsonData: Record<string, string> = {};
  if (draft.llmServiceAccountToken.trim().length > 0) {
    secureJsonData.llmServiceAccountToken = encodeBoundGrafanaToken(
      draft.trimmedGrafanaUrl,
      draft.llmServiceAccountToken.trim()
    );
  } else if (draft.wasTokenReset) {
    secureJsonData.llmServiceAccountToken = '';
  }
  if (draft.jobStoreBackend === 'redis') {
    if (draft.redisPassword.length > 0) {
      secureJsonData.redisPassword = encodeBoundRedisPassword(
        draft.trimmedRedisAddr,
        draft.redisTls,
        draft.redisPassword
      );
    } else if (draft.wasRedisPasswordReset) {
      secureJsonData.redisPassword = '';
    }
  }
  if (draft.jobStoreBackend === 'postgres') {
    if (draft.postgresPassword.length > 0) {
      secureJsonData.postgresPassword = encodeBoundPostgresPassword(
        draft.trimmedPostgresAddr,
        draft.postgresSslMode,
        draft.postgresPassword
      );
    } else if (draft.wasPostgresPasswordReset) {
      secureJsonData.postgresPassword = '';
    }
  }
  return secureJsonData;
}

export function useAppConfigState(meta: AppPluginMeta<StudioJsonData>, t: TranslateFn) {
  const { enabled, pinned, jsonData, secureJsonFields } = meta;
  const llmInit = normalizeInitialLlmConfig(jsonData);
  const limitsInit = normalizeInitialLimitsConfig(jsonData);
  const storeInit = normalizeInitialStoreConfig(jsonData);

  // Track the latest saved jsonData snapshot locally so multiple saves without a page reload
  // never revert or wipe previously saved fields for inactive job store backends.
  const [savedJsonData, setSavedJsonData] = useState<StudioJsonData>(() => ({ ...(jsonData ?? {}) }));

  const [allowedDsText, setAllowedDsText] = useState(llmInit.allowedDsText);
  const [datasourceDialects, setDatasourceDialects] = useState<Record<string, QueryKind | ''>>(() =>
    initDatasourceDialects(jsonData?.datasourceDialects)
  );
  const [llmMode, setLlmMode] = useState<'grafana-llm-app' | 'mock'>(llmInit.llmMode);
  const [allowMockInDev, setAllowMockInDev] = useState(llmInit.allowMockInDev);
  // Default empty so the Go backend uses Grafana's trusted AppURL automatically without hardcoding localhost:3000.
  const [grafanaUrl, setGrafanaUrl] = useState(llmInit.grafanaUrl);
  const [savedGrafanaUrl, setSavedGrafanaUrl] = useState(llmInit.savedGrafanaUrl);
  const [modelTier, setModelTier] = useState<'base' | 'large'>(llmInit.modelTier);
  const [structuredOutputMode, setStructuredOutputMode] = useState<'json_schema' | 'json_object'>(
    llmInit.structuredOutputMode
  );
  const [tokenLimitField, setTokenLimitField] = useState<'max_completion_tokens' | 'max_tokens'>(
    llmInit.tokenLimitField
  );
  const [reasoningEffort, setReasoningEffort] = useState(llmInit.reasoningEffort);
  const [maxPanels, setMaxPanels] = useState(limitsInit.maxPanels);
  const [maxRepairAttempts, setMaxRepairAttempts] = useState(limitsInit.maxRepairAttempts);
  const [maxOutputTokens, setMaxOutputTokens] = useState(limitsInit.maxOutputTokens);
  const [llmTimeoutSeconds, setLlmTimeoutSeconds] = useState(limitsInit.llmTimeoutSeconds);
  const [jobDeadlineSeconds, setJobDeadlineSeconds] = useState(limitsInit.jobDeadlineSeconds);
  const [maxConcurrentJobs, setMaxConcurrentJobs] = useState(limitsInit.maxConcurrentJobs);
  const [validationTimeoutSeconds, setValidationTimeoutSeconds] = useState(
    limitsInit.validationTimeoutSeconds
  );
  const [maxConcurrentQueries, setMaxConcurrentQueries] = useState(limitsInit.maxConcurrentQueries);
  const [enableSseProgress, setEnableSseProgress] = useState(llmInit.enableSseProgress);

  const [jobStoreBackend, setJobStoreBackend] = useState<JobStoreBackend>(storeInit.jobStoreBackend);
  const [jobTtlMinutes, setJobTtlMinutes] = useState(limitsInit.jobTtlMinutes);
  const [redisAddr, setRedisAddr] = useState(storeInit.redisAddr);
  const [savedRedisAddr, setSavedRedisAddr] = useState(storeInit.savedRedisAddr);
  const [redisDb, setRedisDb] = useState(storeInit.redisDb);
  const [redisKeyPrefix, setRedisKeyPrefix] = useState(storeInit.redisKeyPrefix);
  const [redisTls, setRedisTls] = useState(storeInit.redisTls);
  const [savedRedisTls, setSavedRedisTls] = useState(storeInit.redisTls);
  const [redisUsername, setRedisUsername] = useState(storeInit.redisUsername);

  const [postgresAddr, setPostgresAddr] = useState(storeInit.postgresAddr);
  const [savedPostgresAddr, setSavedPostgresAddr] = useState(storeInit.savedPostgresAddr);
  const [postgresDatabase, setPostgresDatabase] = useState(storeInit.postgresDatabase);
  const [postgresSchema, setPostgresSchema] = useState(storeInit.postgresSchema);
  const [postgresTablePrefix, setPostgresTablePrefix] = useState(storeInit.postgresTablePrefix);
  const [postgresUsername, setPostgresUsername] = useState(storeInit.postgresUsername);
  const [postgresSslMode, setPostgresSslMode] = useState<PostgresSslMode>(storeInit.postgresSslMode);
  const [savedPostgresSslMode, setSavedPostgresSslMode] = useState<PostgresSslMode>(
    storeInit.postgresSslMode
  );

  const [isTokenConfigured, setIsTokenConfigured] = useState(
    Boolean(secureJsonFields?.llmServiceAccountToken)
  );
  const [wasTokenReset, setWasTokenReset] = useState(false);
  const [llmServiceAccountToken, setLlmServiceAccountToken] = useState('');

  const [savedRedisPasswordConfigured, setSavedRedisPasswordConfigured] = useState(
    Boolean(secureJsonFields?.redisPassword)
  );
  const [isRedisPasswordConfigured, setIsRedisPasswordConfigured] = useState(
    Boolean(secureJsonFields?.redisPassword)
  );
  const [wasRedisPasswordReset, setWasRedisPasswordReset] = useState(false);
  const [redisPassword, setRedisPassword] = useState('');

  const [savedPostgresPasswordConfigured, setSavedPostgresPasswordConfigured] = useState(
    Boolean(secureJsonFields?.postgresPassword)
  );
  const [isPostgresPasswordConfigured, setIsPostgresPasswordConfigured] = useState(
    Boolean(secureJsonFields?.postgresPassword)
  );
  const [wasPostgresPasswordReset, setWasPostgresPasswordReset] = useState(false);
  const [postgresPassword, setPostgresPassword] = useState('');

  const [saving, setSaving] = useState(false);
  const [statusMessage, setStatusMessage] = useState<{ severity: 'success' | 'error'; text: string } | null>(
    null
  );

  // Switching away from a backend discards unsaved secret edits/reset flags for the hidden backend
  // so switching back later never silently clears a password still stored on the server.
  const handleJobStoreBackendChange = (nextBackend: JobStoreBackend) => {
    if (nextBackend !== 'redis') {
      setRedisPassword('');
      setWasRedisPasswordReset(false);
      setIsRedisPasswordConfigured(savedRedisPasswordConfigured);
    }
    if (nextBackend !== 'postgres') {
      setPostgresPassword('');
      setWasPostgresPasswordReset(false);
      setIsPostgresPasswordConfigured(savedPostgresPasswordConfigured);
    }
    setJobStoreBackend(nextBackend);
  };

  const parsedAllowedUids = Array.from(
    new Set(
      allowedDsText
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
    )
  );

  const syncSavedBackendSecrets = (
    trimmedGrafanaUrl: string,
    trimmedRedisAddr: string,
    effectiveRedisKeyPrefix: string,
    trimmedPostgresAddr: string,
    effectivePostgresSchema: string,
    effectivePostgresTablePrefix: string
  ) => {
    setSavedGrafanaUrl(trimmedGrafanaUrl);
    if (llmServiceAccountToken.trim().length > 0) {
      setIsTokenConfigured(true);
      setWasTokenReset(false);
      setLlmServiceAccountToken('');
    } else if (wasTokenReset) {
      setIsTokenConfigured(false);
      setWasTokenReset(false);
    }
    if (jobStoreBackend === 'redis') {
      setRedisKeyPrefix(effectiveRedisKeyPrefix);
      setSavedRedisAddr(trimmedRedisAddr);
      setSavedRedisTls(redisTls);
      if (redisPassword.length > 0) {
        setSavedRedisPasswordConfigured(true);
        setIsRedisPasswordConfigured(true);
        setWasRedisPasswordReset(false);
        setRedisPassword('');
      } else if (wasRedisPasswordReset) {
        setSavedRedisPasswordConfigured(false);
        setIsRedisPasswordConfigured(false);
        setWasRedisPasswordReset(false);
      }
    }
    if (jobStoreBackend === 'postgres') {
      setPostgresSchema(effectivePostgresSchema);
      setPostgresTablePrefix(effectivePostgresTablePrefix);
      setSavedPostgresAddr(trimmedPostgresAddr);
      setSavedPostgresSslMode(postgresSslMode);
      if (postgresPassword.length > 0) {
        setSavedPostgresPasswordConfigured(true);
        setIsPostgresPasswordConfigured(true);
        setWasPostgresPasswordReset(false);
        setPostgresPassword('');
      } else if (wasPostgresPasswordReset) {
        setSavedPostgresPasswordConfigured(false);
        setIsPostgresPasswordConfigured(false);
        setWasPostgresPasswordReset(false);
      }
    }
  };

  const handleSave = async () => {
    const validationErr = validateAppConfig(
      {
        llmMode,
        allowMockInDev,
        isTokenConfigured,
        grafanaUrl,
        savedGrafanaUrl,
        maxPanels,
        maxRepairAttempts,
        maxOutputTokens,
        llmTimeoutSeconds,
        jobDeadlineSeconds,
        maxConcurrentJobs,
        validationTimeoutSeconds,
        maxConcurrentQueries,
        jobTtlMinutes,
        reasoningEffort,
        jobStoreBackend,
        redisAddr,
        savedRedisAddr,
        redisDb,
        redisKeyPrefix,
        redisTls,
        savedRedisTls,
        redisUsername,
        isRedisPasswordConfigured,
        postgresAddr,
        savedPostgresAddr,
        postgresDatabase,
        postgresSchema,
        postgresTablePrefix,
        postgresUsername,
        postgresSslMode,
        savedPostgresSslMode,
        isPostgresPasswordConfigured,
      },
      t
    );
    if (validationErr) {
      setStatusMessage({ severity: 'error', text: validationErr });
      return;
    }

    setSaving(true);
    setStatusMessage(null);
    try {
      const cleanedDialects = buildCleanedDialects(parsedAllowedUids, datasourceDialects);
      const effectiveRedisKeyPrefix = redisKeyPrefix.trim() || 'llmstudio:jobs';
      const effectivePostgresSchema = postgresSchema.trim() || 'public';
      const effectivePostgresTablePrefix = postgresTablePrefix.trim() || 'llm_studio_';
      const trimmedGrafanaUrl = grafanaUrl.trim();
      const trimmedRedisAddr = redisAddr.trim();
      const trimmedPostgresAddr = postgresAddr.trim();

      const nextJsonData = buildNextJsonData({
        savedJsonData,
        allowedDatasourceUids: parsedAllowedUids,
        cleanedDialects,
        llmMode,
        allowMockInDev,
        trimmedGrafanaUrl,
        modelTier,
        structuredOutputMode,
        tokenLimitField,
        reasoningEffort,
        maxPanels,
        maxRepairAttempts,
        maxOutputTokens,
        llmTimeoutSeconds,
        jobDeadlineSeconds,
        maxConcurrentJobs,
        validationTimeoutSeconds,
        maxConcurrentQueries,
        enableSseProgress,
        jobStoreBackend,
        jobTtlMinutes,
        trimmedRedisAddr,
        redisDb,
        effectiveRedisKeyPrefix,
        redisTls,
        redisUsername,
        trimmedPostgresAddr,
        postgresDatabase,
        effectivePostgresSchema,
        effectivePostgresTablePrefix,
        postgresUsername,
        postgresSslMode,
      });

      const secureJsonData = buildSecureJsonData({
        jobStoreBackend,
        trimmedGrafanaUrl,
        llmServiceAccountToken,
        wasTokenReset,
        trimmedRedisAddr,
        redisTls,
        redisPassword,
        wasRedisPasswordReset,
        trimmedPostgresAddr,
        postgresSslMode,
        postgresPassword,
        wasPostgresPasswordReset,
      });
      const hasSecureJsonData = Object.keys(secureJsonData).length > 0;

      await lastValueFrom(
        getBackendSrv().fetch({
          url: `/api/plugins/${meta.id}/settings`,
          method: 'POST',
          showErrorAlert: false,
          data: {
            enabled,
            pinned,
            jsonData: nextJsonData,
            ...(hasSecureJsonData ? { secureJsonData } : {}),
          },
        })
      );

      setSavedJsonData(nextJsonData);
      syncSavedBackendSecrets(
        trimmedGrafanaUrl,
        trimmedRedisAddr,
        effectiveRedisKeyPrefix,
        trimmedPostgresAddr,
        effectivePostgresSchema,
        effectivePostgresTablePrefix
      );
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

  return {
    allowedDsText,
    setAllowedDsText,
    parsedAllowedUids,
    datasourceDialects,
    setDatasourceDialects,
    llmMode,
    setLlmMode,
    allowMockInDev,
    setAllowMockInDev,
    grafanaUrl,
    setGrafanaUrl,
    modelTier,
    setModelTier,
    structuredOutputMode,
    setStructuredOutputMode,
    tokenLimitField,
    setTokenLimitField,
    reasoningEffort,
    setReasoningEffort,
    isTokenConfigured,
    llmServiceAccountToken,
    setLlmServiceAccountToken,
    resetLlmToken: () => {
      setIsTokenConfigured(false);
      setWasTokenReset(true);
      setLlmServiceAccountToken('');
    },
    enableSseProgress,
    setEnableSseProgress,
    maxPanels,
    setMaxPanels,
    maxRepairAttempts,
    setMaxRepairAttempts,
    maxOutputTokens,
    setMaxOutputTokens,
    llmTimeoutSeconds,
    setLlmTimeoutSeconds,
    jobDeadlineSeconds,
    setJobDeadlineSeconds,
    maxConcurrentJobs,
    setMaxConcurrentJobs,
    validationTimeoutSeconds,
    setValidationTimeoutSeconds,
    maxConcurrentQueries,
    setMaxConcurrentQueries,
    jobStoreBackend,
    handleJobStoreBackendChange,
    jobTtlMinutes,
    setJobTtlMinutes,
    redisAddr,
    setRedisAddr,
    redisDb,
    setRedisDb,
    redisKeyPrefix,
    setRedisKeyPrefix,
    redisTls,
    setRedisTls,
    redisUsername,
    setRedisUsername,
    isRedisPasswordConfigured,
    redisPassword,
    setRedisPassword,
    resetRedisPassword: () => {
      setIsRedisPasswordConfigured(false);
      setWasRedisPasswordReset(true);
      setRedisPassword('');
    },
    postgresAddr,
    setPostgresAddr,
    postgresDatabase,
    setPostgresDatabase,
    postgresSchema,
    setPostgresSchema,
    postgresTablePrefix,
    setPostgresTablePrefix,
    postgresUsername,
    setPostgresUsername,
    postgresSslMode,
    setPostgresSslMode,
    isPostgresPasswordConfigured,
    postgresPassword,
    setPostgresPassword,
    resetPostgresPassword: () => {
      setIsPostgresPasswordConfigured(false);
      setWasPostgresPasswordReset(true);
      setPostgresPassword('');
    },
    saving,
    statusMessage,
    handleSave,
  };
}
