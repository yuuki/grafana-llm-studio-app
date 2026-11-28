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

export function useAppConfigState(meta: AppPluginMeta<StudioJsonData>, t: TranslateFn) {
  const { enabled, pinned, jsonData, secureJsonFields } = meta;

  // Track the latest saved jsonData snapshot locally so multiple saves without a page reload
  // never revert or wipe previously saved fields for inactive job store backends.
  const [savedJsonData, setSavedJsonData] = useState<StudioJsonData>(() => ({ ...(jsonData ?? {}) }));

  const [allowedDsText, setAllowedDsText] = useState((jsonData?.allowedDatasourceUids ?? []).join(', '));
  const [datasourceDialects, setDatasourceDialects] = useState<Record<string, QueryKind | ''>>(() => {
    const init: Record<string, QueryKind | ''> = {};
    if (jsonData?.datasourceDialects) {
      for (const [uid, rawKind] of Object.entries(jsonData.datasourceDialects)) {
        const normalizedKind = String(rawKind ?? '')
          .trim()
          .toLowerCase();
        if (uid.trim() && (normalizedKind === 'promql' || normalizedKind === 'metricsql')) {
          init[uid.trim()] = normalizedKind;
        }
      }
    }
    return init;
  });
  const [llmMode, setLlmMode] = useState<'grafana-llm-app' | 'mock'>(
    jsonData?.llmMode === 'mock' ? 'mock' : 'grafana-llm-app'
  );
  const [allowMockInDev, setAllowMockInDev] = useState(Boolean(jsonData?.allowMockInDev));
  // Default empty so the Go backend uses Grafana's trusted AppURL automatically without hardcoding localhost:3000.
  const [grafanaUrl, setGrafanaUrl] = useState(jsonData?.grafanaUrl ?? '');
  const [savedGrafanaUrl, setSavedGrafanaUrl] = useState((jsonData?.grafanaUrl ?? '').trim());
  const [modelTier, setModelTier] = useState<'base' | 'large'>(
    jsonData?.modelTier === 'large' ? 'large' : 'base'
  );
  const [structuredOutputMode, setStructuredOutputMode] = useState<'json_schema' | 'json_object'>(() => {
    const normalized = String(jsonData?.structuredOutputMode ?? '')
      .trim()
      .toLowerCase();
    return normalized === 'json_object' ? 'json_object' : 'json_schema';
  });
  const [tokenLimitField, setTokenLimitField] = useState<'max_completion_tokens' | 'max_tokens'>(() => {
    const normalized = String(jsonData?.tokenLimitField ?? '')
      .trim()
      .toLowerCase();
    return normalized === 'max_tokens' ? 'max_tokens' : 'max_completion_tokens';
  });
  const [reasoningEffort, setReasoningEffort] = useState(() =>
    String(jsonData?.reasoningEffort ?? '')
      .trim()
      .toLowerCase()
  );
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
  const [enableSseProgress, setEnableSseProgress] = useState(Boolean(jsonData?.enableSseProgress));

  const [jobStoreBackend, setJobStoreBackend] = useState<JobStoreBackend>(() => {
    const normalized = String(jsonData?.jobStoreBackend ?? '')
      .trim()
      .toLowerCase();
    if (normalized === 'redis') {
      return 'redis';
    }
    if (normalized === 'postgres') {
      return 'postgres';
    }
    return 'memory';
  });
  const [jobTtlMinutes, setJobTtlMinutes] = useState(jsonData?.jobTtlMinutes ?? 30);
  const [redisAddr, setRedisAddr] = useState(jsonData?.redisAddr ?? '');
  const [savedRedisAddr, setSavedRedisAddr] = useState((jsonData?.redisAddr ?? '').trim());
  const [redisDb, setRedisDb] = useState(jsonData?.redisDb ?? 0);
  const [redisKeyPrefix, setRedisKeyPrefix] = useState(jsonData?.redisKeyPrefix ?? 'llmstudio:jobs');
  const [redisTls, setRedisTls] = useState(Boolean(jsonData?.redisTls));
  const [savedRedisTls, setSavedRedisTls] = useState(Boolean(jsonData?.redisTls));
  const [redisUsername, setRedisUsername] = useState(jsonData?.redisUsername ?? '');

  const initialPostgresSslMode = (() => {
    const normalized = String(jsonData?.postgresSslMode ?? '')
      .trim()
      .toLowerCase();
    return ALLOWED_POSTGRES_SSL_MODES.has(normalized) ? (normalized as PostgresSslMode) : 'prefer';
  })();
  const [postgresAddr, setPostgresAddr] = useState(jsonData?.postgresAddr ?? '');
  const [savedPostgresAddr, setSavedPostgresAddr] = useState((jsonData?.postgresAddr ?? '').trim());
  const [postgresDatabase, setPostgresDatabase] = useState(jsonData?.postgresDatabase ?? '');
  const [postgresSchema, setPostgresSchema] = useState(jsonData?.postgresSchema ?? 'public');
  const [postgresTablePrefix, setPostgresTablePrefix] = useState(
    jsonData?.postgresTablePrefix ?? 'llm_studio_'
  );
  const [postgresUsername, setPostgresUsername] = useState(jsonData?.postgresUsername ?? '');
  const [postgresSslMode, setPostgresSslMode] = useState<PostgresSslMode>(initialPostgresSslMode);
  const [savedPostgresSslMode, setSavedPostgresSslMode] = useState<PostgresSslMode>(initialPostgresSslMode);

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
      const allowedDatasourceUids = parsedAllowedUids;
      // Always write an explicit object (even `{}`) so H-11 `...savedJsonData` spread
      // never retains previously pinned dialects when all UIDs are reset back to Auto.
      const cleanedDialects: Record<string, QueryKind> = {};
      for (const uid of allowedDatasourceUids) {
        const d = datasourceDialects[uid];
        if (d === 'promql' || d === 'metricsql') {
          cleanedDialects[uid] = d;
        }
      }

      const effectiveRedisKeyPrefix = redisKeyPrefix.trim() || 'llmstudio:jobs';
      const effectivePostgresSchema = postgresSchema.trim() || 'public';
      const effectivePostgresTablePrefix = postgresTablePrefix.trim() || 'llm_studio_';

      // Preserve any existing/previously saved jsonData fields (H-11), strip dev-only mockDelayMs (L-1),
      // and normalize allowMockInDev=false outside mock mode (M-13).
      // Only write backend-specific jsonData/secureJsonData fields for the active jobStoreBackend so switching
      // backends never overwrites saved settings with unvalidated hidden fields or resets hidden passwords.
      const { mockDelayMs: _stripMockDelayMs, ...preservedJsonData } = savedJsonData as StudioJsonData & {
        mockDelayMs?: unknown;
      };
      const trimmedGrafanaUrl = grafanaUrl.trim();
      const trimmedRedisAddr = redisAddr.trim();
      const trimmedPostgresAddr = postgresAddr.trim();
      const nextJsonData: StudioJsonData = {
        ...preservedJsonData,
        allowedDatasourceUids,
        datasourceDialects: cleanedDialects,
        llmMode,
        allowMockInDev: llmMode === 'mock' ? allowMockInDev : false,
        grafanaUrl: trimmedGrafanaUrl,
        modelTier,
        structuredOutputMode,
        tokenLimitField,
        reasoningEffort: reasoningEffort.trim().toLowerCase(),
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
        ...(jobStoreBackend === 'redis'
          ? {
              redisAddr: trimmedRedisAddr,
              redisDb,
              redisKeyPrefix: effectiveRedisKeyPrefix,
              redisTls,
              redisUsername: redisUsername.trim(),
            }
          : {}),
        ...(jobStoreBackend === 'postgres'
          ? {
              postgresAddr: trimmedPostgresAddr,
              postgresDatabase: postgresDatabase.trim(),
              postgresSchema: effectivePostgresSchema,
              postgresTablePrefix: effectivePostgresTablePrefix,
              postgresUsername: postgresUsername.trim(),
              postgresSslMode,
            }
          : {}),
      };

      // Explicitly send empty string on reset so Grafana clears the stored encrypted secret (H-11).
      // Bind newly saved credentials to their destination inside the encrypted secret value so changing
      // the target endpoint via UI or direct API cannot redirect an existing secret to another server.
      const secureJsonData: Record<string, string> = {};
      if (llmServiceAccountToken.trim().length > 0) {
        secureJsonData.llmServiceAccountToken = encodeBoundGrafanaToken(
          trimmedGrafanaUrl,
          llmServiceAccountToken.trim()
        );
      } else if (wasTokenReset) {
        secureJsonData.llmServiceAccountToken = '';
      }
      if (jobStoreBackend === 'redis') {
        if (redisPassword.length > 0) {
          secureJsonData.redisPassword = encodeBoundRedisPassword(
            trimmedRedisAddr,
            redisTls,
            redisPassword
          );
        } else if (wasRedisPasswordReset) {
          secureJsonData.redisPassword = '';
        }
      }
      if (jobStoreBackend === 'postgres') {
        if (postgresPassword.length > 0) {
          secureJsonData.postgresPassword = encodeBoundPostgresPassword(
            trimmedPostgresAddr,
            postgresSslMode,
            postgresPassword
          );
        } else if (wasPostgresPasswordReset) {
          secureJsonData.postgresPassword = '';
        }
      }
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
