import React from 'react';
import { Checkbox, Field, FieldSet, Input, SecretInput, Select } from '@grafana/ui';
import { JobStoreBackend, PostgresSslMode } from '../types';
import { formatNumberValue, parseNumberInput, TranslateFn } from '../validation';

export interface JobStorageSectionProps {
  t: TranslateFn;
  jobStoreBackend: JobStoreBackend;
  onJobStoreBackendChange: (backend: JobStoreBackend) => void;
  jobTtlMinutes: number;
  onJobTtlMinutesChange: (val: number) => void;
  redisAddr: string;
  onRedisAddrChange: (val: string) => void;
  redisDb: number;
  onRedisDbChange: (val: number) => void;
  redisKeyPrefix: string;
  onRedisKeyPrefixChange: (val: string) => void;
  redisUsername: string;
  onRedisUsernameChange: (val: string) => void;
  isRedisPasswordConfigured: boolean;
  redisPassword: string;
  onRedisPasswordChange: (val: string) => void;
  onResetRedisPassword: () => void;
  redisTls: boolean;
  onRedisTlsChange: (checked: boolean) => void;
  postgresAddr: string;
  onPostgresAddrChange: (val: string) => void;
  postgresDatabase: string;
  onPostgresDatabaseChange: (val: string) => void;
  postgresSchema: string;
  onPostgresSchemaChange: (val: string) => void;
  postgresTablePrefix: string;
  onPostgresTablePrefixChange: (val: string) => void;
  postgresUsername: string;
  onPostgresUsernameChange: (val: string) => void;
  isPostgresPasswordConfigured: boolean;
  postgresPassword: string;
  onPostgresPasswordChange: (val: string) => void;
  onResetPostgresPassword: () => void;
  postgresSslMode: PostgresSslMode;
  onPostgresSslModeChange: (mode: PostgresSslMode) => void;
}

export const JobStorageSection: React.FC<JobStorageSectionProps> = ({
  t,
  jobStoreBackend,
  onJobStoreBackendChange,
  jobTtlMinutes,
  onJobTtlMinutesChange,
  redisAddr,
  onRedisAddrChange,
  redisDb,
  onRedisDbChange,
  redisKeyPrefix,
  onRedisKeyPrefixChange,
  redisUsername,
  onRedisUsernameChange,
  isRedisPasswordConfigured,
  redisPassword,
  onRedisPasswordChange,
  onResetRedisPassword,
  redisTls,
  onRedisTlsChange,
  postgresAddr,
  onPostgresAddrChange,
  postgresDatabase,
  onPostgresDatabaseChange,
  postgresSchema,
  onPostgresSchemaChange,
  postgresTablePrefix,
  onPostgresTablePrefixChange,
  postgresUsername,
  onPostgresUsernameChange,
  isPostgresPasswordConfigured,
  postgresPassword,
  onPostgresPasswordChange,
  onResetPostgresPassword,
  postgresSslMode,
  onPostgresSslModeChange,
}) => (
  <FieldSet label={t('appConfig.jobStorage.fieldset')}>
    <Field
      label={t('appConfig.jobStorage.backendLabel')}
      description={t('appConfig.jobStorage.backendDescription')}
    >
      <Select
        aria-label={t('appConfig.jobStorage.backendLabel')}
        options={[
          { label: t('appConfig.jobStorage.backendMemory'), value: 'memory' },
          { label: t('appConfig.jobStorage.backendRedis'), value: 'redis' },
          { label: t('appConfig.jobStorage.backendPostgres'), value: 'postgres' },
        ]}
        value={jobStoreBackend}
        onChange={(v) => onJobStoreBackendChange((v.value as JobStoreBackend) ?? 'memory')}
      />
    </Field>
    <Field label={t('appConfig.jobStorage.jobTtlMinutesLabel')}>
      <Input
        type="number"
        min={1}
        max={1440}
        value={formatNumberValue(jobTtlMinutes)}
        onChange={(e) => onJobTtlMinutesChange(parseNumberInput(e.currentTarget.value))}
      />
    </Field>

    {jobStoreBackend === 'redis' && (
      <>
        <Field
          label={t('appConfig.jobStorage.redisAddrLabel')}
          description={t('appConfig.jobStorage.redisAddrDescription')}
        >
          <Input
            value={redisAddr}
            onChange={(e) => onRedisAddrChange(e.currentTarget.value)}
            placeholder="redis:6379"
          />
        </Field>
        <Field label={t('appConfig.jobStorage.redisDbLabel')}>
          <Input
            type="number"
            min={0}
            max={15}
            value={formatNumberValue(redisDb)}
            onChange={(e) => onRedisDbChange(parseNumberInput(e.currentTarget.value))}
          />
        </Field>
        <Field
          label={t('appConfig.jobStorage.redisKeyPrefixLabel')}
          description={t('appConfig.jobStorage.redisKeyPrefixDescription')}
        >
          <Input
            value={redisKeyPrefix}
            onChange={(e) => onRedisKeyPrefixChange(e.currentTarget.value)}
            placeholder="llmstudio:jobs"
          />
        </Field>
        <Field label={t('appConfig.jobStorage.redisUsernameLabel')}>
          <Input
            value={redisUsername}
            onChange={(e) => onRedisUsernameChange(e.currentTarget.value)}
            placeholder="default"
          />
        </Field>
        <Field
          label={t('appConfig.jobStorage.redisPasswordLabel')}
          description={t('appConfig.jobStorage.redisPasswordDescription')}
        >
          <SecretInput
            isConfigured={isRedisPasswordConfigured}
            value={redisPassword}
            onChange={(e) => onRedisPasswordChange(e.currentTarget.value)}
            onReset={onResetRedisPassword}
            placeholder="redis-password"
          />
        </Field>
        <Field label={t('appConfig.jobStorage.redisTlsLabel')}>
          <Checkbox
            value={redisTls}
            onChange={(e) => onRedisTlsChange(e.currentTarget.checked)}
            label={t('appConfig.jobStorage.redisTlsCheckbox')}
          />
        </Field>
      </>
    )}

    {jobStoreBackend === 'postgres' && (
      <>
        <Field
          label={t('appConfig.jobStorage.postgresAddrLabel')}
          description={t('appConfig.jobStorage.postgresAddrDescription')}
        >
          <Input
            value={postgresAddr}
            onChange={(e) => onPostgresAddrChange(e.currentTarget.value)}
            placeholder="postgres:5432"
          />
        </Field>
        <Field
          label={t('appConfig.jobStorage.postgresDatabaseLabel')}
          description={t('appConfig.jobStorage.postgresDatabaseDescription')}
        >
          <Input
            value={postgresDatabase}
            onChange={(e) => onPostgresDatabaseChange(e.currentTarget.value)}
            placeholder="grafana"
          />
        </Field>
        <Field
          label={t('appConfig.jobStorage.postgresSchemaLabel')}
          description={t('appConfig.jobStorage.postgresSchemaDescription')}
        >
          <Input
            value={postgresSchema}
            onChange={(e) => onPostgresSchemaChange(e.currentTarget.value)}
            placeholder="public"
          />
        </Field>
        <Field
          label={t('appConfig.jobStorage.postgresTablePrefixLabel')}
          description={t('appConfig.jobStorage.postgresTablePrefixDescription')}
        >
          <Input
            value={postgresTablePrefix}
            onChange={(e) => onPostgresTablePrefixChange(e.currentTarget.value)}
            placeholder="llm_studio_"
          />
        </Field>
        <Field label={t('appConfig.jobStorage.postgresUsernameLabel')}>
          <Input
            value={postgresUsername}
            onChange={(e) => onPostgresUsernameChange(e.currentTarget.value)}
            placeholder="grafana"
          />
        </Field>
        <Field
          label={t('appConfig.jobStorage.postgresPasswordLabel')}
          description={t('appConfig.jobStorage.postgresPasswordDescription')}
        >
          <SecretInput
            isConfigured={isPostgresPasswordConfigured}
            value={postgresPassword}
            onChange={(e) => onPostgresPasswordChange(e.currentTarget.value)}
            onReset={onResetPostgresPassword}
            placeholder="postgres-password"
          />
        </Field>
        <Field
          label={t('appConfig.jobStorage.postgresSslModeLabel')}
          description={t('appConfig.jobStorage.postgresSslModeDescription')}
        >
          <Select
            aria-label={t('appConfig.jobStorage.postgresSslModeLabel')}
            options={[
              { label: t('appConfig.jobStorage.postgresSslModeDisable'), value: 'disable' },
              { label: t('appConfig.jobStorage.postgresSslModePrefer'), value: 'prefer' },
              { label: t('appConfig.jobStorage.postgresSslModeRequire'), value: 'require' },
              { label: t('appConfig.jobStorage.postgresSslModeVerifyCa'), value: 'verify-ca' },
              { label: t('appConfig.jobStorage.postgresSslModeVerifyFull'), value: 'verify-full' },
            ]}
            value={postgresSslMode}
            onChange={(v) => onPostgresSslModeChange((v.value as PostgresSslMode) ?? 'prefer')}
          />
        </Field>
      </>
    )}
  </FieldSet>
);
