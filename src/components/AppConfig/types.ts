import { QueryKind } from '../../ir/types';

export type JobStoreBackend = 'memory' | 'redis' | 'postgres';
export type PostgresSslMode = 'disable' | 'prefer' | 'require' | 'verify-ca' | 'verify-full';

export interface StudioJsonData {
  allowedDatasourceUids?: string[];
  datasourceDialects?: Record<string, QueryKind>;
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
  enableSseProgress?: boolean;
  jobStoreBackend?: JobStoreBackend;
  jobTtlMinutes?: number;
  redisAddr?: string;
  redisDb?: number;
  redisKeyPrefix?: string;
  redisTls?: boolean;
  redisUsername?: string;
  postgresAddr?: string;
  postgresDatabase?: string;
  postgresSchema?: string;
  postgresTablePrefix?: string;
  postgresUsername?: string;
  postgresSslMode?: PostgresSslMode;
}
