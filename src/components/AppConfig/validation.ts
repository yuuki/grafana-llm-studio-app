import { TranslationKey } from '../../i18n';
import { canonicalGrafanaUrlForCompare, canonicalRedisAddrForCompare } from './secretBinding';
import { JobStoreBackend, PostgresSslMode } from './types';

export const REDIS_KEY_PREFIX_RE = /^[a-zA-Z0-9:_-]{1,64}$/;
const REDIS_PORT_RE = /^[1-9]\d{0,4}$/;
const REDIS_HOSTNAME_RE = /^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$/;
const IPV6_HEXTET_RE = /^[0-9a-fA-F]{1,4}$/;
const IPV4_OCTET_RE = /^(?:0|[1-9]\d{0,2})$/;
export const POSTGRES_SCHEMA_RE = /^[a-z_][a-z0-9_]{0,62}$/;
export const POSTGRES_TABLE_PREFIX_RE = /^[a-z_][a-z0-9_]{0,31}$/;
export const ALLOWED_POSTGRES_SSL_MODES: ReadonlySet<string> = new Set([
  'disable',
  'prefer',
  'require',
  'verify-ca',
  'verify-full',
]);

export function utf8ByteLength(raw: string): number {
  if (typeof TextEncoder !== 'undefined') {
    return new TextEncoder().encode(raw).length;
  }
  let bytes = 0;
  for (let i = 0; i < raw.length; i++) {
    const code = raw.charCodeAt(i);
    if (code <= 0x7f) {
      bytes += 1;
    } else if (code <= 0x7ff) {
      bytes += 2;
    } else if (code >= 0xd800 && code <= 0xdbff && i + 1 < raw.length) {
      bytes += 4;
      i++;
    } else {
      bytes += 3;
    }
  }
  return bytes;
}

export function hasForbiddenTextChar(raw: string, disallowSpace: boolean): boolean {
  for (let i = 0; i < raw.length; i++) {
    const code = raw.charCodeAt(i);
    if (code < 0x20 || code === 0x7f || (code >= 0x80 && code <= 0x9f) || code === 0xfeff) {
      return true;
    }
    if (disallowSpace && (code === 0x20 || code === 0x09 || code === 0x0a || code === 0x0d)) {
      return true;
    }
  }
  return false;
}

function isValidIpv4Literal(raw: string): boolean {
  const parts = raw.split('.');
  if (parts.length !== 4) {
    return false;
  }
  for (const p of parts) {
    if (!IPV4_OCTET_RE.test(p)) {
      return false;
    }
    const n = Number(p);
    if (n < 0 || n > 255) {
      return false;
    }
  }
  return true;
}

function isValidIpv6Literal(host: string): boolean {
  if (!host || !host.includes(':')) {
    return false;
  }
  // Reject triple colon or more than one "::" compression marker.
  if (host.includes(':::')) {
    return false;
  }
  const doubleColonParts = host.split('::');
  if (doubleColonParts.length > 2) {
    return false;
  }
  const hasCompression = doubleColonParts.length === 2;

  const parseSide = (side: string, allowTrailingIpv4: boolean): number | null => {
    if (side === '') {
      return 0;
    }
    const groups = side.split(':');
    let count = 0;
    for (let i = 0; i < groups.length; i++) {
      const g = groups[i];
      if (g === '') {
        return null;
      }
      if (allowTrailingIpv4 && i === groups.length - 1 && g.includes('.')) {
        if (!isValidIpv4Literal(g)) {
          return null;
        }
        count += 2;
      } else if (IPV6_HEXTET_RE.test(g)) {
        count += 1;
      } else {
        return null;
      }
    }
    return count;
  };

  if (hasCompression) {
    const leftCount = parseSide(doubleColonParts[0], false);
    const rightCount = parseSide(doubleColonParts[1], true);
    if (leftCount === null || rightCount === null) {
      return false;
    }
    // "::" must replace at least one 16-bit group.
    return leftCount + rightCount < 8;
  }

  const totalCount = parseSide(host, true);
  return totalCount === 8;
}

export function parseNumberInput(raw: string): number {
  const trimmed = raw.trim();
  if (trimmed === '') {
    return Number.NaN;
  }
  return Number(trimmed);
}

export function formatNumberValue(val: number): string | number {
  return Number.isNaN(val) ? '' : val;
}

export function isValidRedisHostPort(raw: string): boolean {
  const addr = raw.trim();
  if (!addr || addr.includes('://') || addr.includes('@')) {
    return false;
  }
  for (let i = 0; i < addr.length; i++) {
    const code = addr.charCodeAt(i);
    if (code <= 0x20 || code === 0x7f || addr[i] === '/' || addr[i] === '?' || addr[i] === '#') {
      return false;
    }
  }

  let host = '';
  let portStr = '';
  if (addr.startsWith('[')) {
    const closeIdx = addr.indexOf(']');
    if (closeIdx <= 1 || addr[closeIdx + 1] !== ':') {
      return false;
    }
    host = addr.slice(1, closeIdx);
    portStr = addr.slice(closeIdx + 2);
    if (!isValidIpv6Literal(host)) {
      return false;
    }
  } else {
    if (addr.includes('[') || addr.includes(']')) {
      return false;
    }
    const firstColon = addr.indexOf(':');
    const lastColon = addr.lastIndexOf(':');
    // Unbracketed multi-colon addresses (like ::1:6379) are rejected by Go net.SplitHostPort.
    if (firstColon <= 0 || firstColon !== lastColon || lastColon === addr.length - 1) {
      return false;
    }
    host = addr.slice(0, lastColon);
    portStr = addr.slice(lastColon + 1);
    if (!REDIS_HOSTNAME_RE.test(host) || host.includes('..')) {
      return false;
    }
  }

  if (!REDIS_PORT_RE.test(portStr)) {
    return false;
  }
  const port = Number(portStr);
  return Number.isInteger(port) && port >= 1 && port <= 65535;
}

export interface AppConfigValidationInput {
  llmMode: 'grafana-llm-app' | 'mock';
  allowMockInDev: boolean;
  isTokenConfigured: boolean;
  grafanaUrl: string;
  savedGrafanaUrl: string;
  maxPanels: number;
  maxRepairAttempts: number;
  maxOutputTokens: number;
  llmTimeoutSeconds: number;
  jobDeadlineSeconds: number;
  maxConcurrentJobs: number;
  validationTimeoutSeconds: number;
  maxConcurrentQueries: number;
  jobTtlMinutes: number;
  reasoningEffort: string;
  jobStoreBackend: JobStoreBackend;
  redisAddr: string;
  savedRedisAddr: string;
  redisDb: number;
  redisKeyPrefix: string;
  redisTls: boolean;
  savedRedisTls: boolean;
  redisUsername: string;
  isRedisPasswordConfigured: boolean;
  postgresAddr: string;
  savedPostgresAddr: string;
  postgresDatabase: string;
  postgresSchema: string;
  postgresTablePrefix: string;
  postgresUsername: string;
  postgresSslMode: PostgresSslMode;
  savedPostgresSslMode: PostgresSslMode;
  isPostgresPasswordConfigured: boolean;
}

export type TranslateFn = (key: TranslationKey, vars?: Record<string, string | number>) => string;

export function validateAppConfig(input: AppConfigValidationInput, t: TranslateFn): string | null {
  if (input.llmMode === 'mock' && !input.allowMockInDev) {
    return t('appConfig.validation.allowMockRequired');
  }
  if (
    input.isTokenConfigured &&
    canonicalGrafanaUrlForCompare(input.grafanaUrl) !== canonicalGrafanaUrlForCompare(input.savedGrafanaUrl)
  ) {
    return t('appConfig.validation.saTokenReentryRequired');
  }
  const checks: Array<[string, number, number, number]> = [
    [t('appConfig.validation.fields.maxPanels'), input.maxPanels, 1, 6],
    [t('appConfig.validation.fields.maxRepairAttempts'), input.maxRepairAttempts, 0, 2],
    [t('appConfig.validation.fields.maxOutputTokens'), input.maxOutputTokens, 256, 8192],
    [t('appConfig.validation.fields.llmTimeoutSeconds'), input.llmTimeoutSeconds, 5, 120],
    [t('appConfig.validation.fields.jobDeadlineSeconds'), input.jobDeadlineSeconds, 10, 180],
    [t('appConfig.validation.fields.maxConcurrentJobs'), input.maxConcurrentJobs, 1, 16],
    [t('appConfig.validation.fields.validationTimeoutSeconds'), input.validationTimeoutSeconds, 2, 30],
    [t('appConfig.validation.fields.maxConcurrentQueries'), input.maxConcurrentQueries, 1, 4],
    [t('appConfig.validation.fields.jobTtlMinutes'), input.jobTtlMinutes, 1, 1440],
  ];
  if (input.jobStoreBackend === 'redis') {
    checks.push([t('appConfig.validation.fields.redisDb'), input.redisDb, 0, 15]);
  }
  for (const [label, val, minVal, maxVal] of checks) {
    if (!Number.isInteger(val) || val < minVal || val > maxVal) {
      return t('appConfig.validation.numberRange', { label, min: minVal, max: maxVal });
    }
  }
  const normalizedEffort = input.reasoningEffort.trim().toLowerCase();
  if (!['', 'low', 'medium', 'high'].includes(normalizedEffort)) {
    return t('appConfig.validation.reasoningEffort');
  }
  if (input.jobStoreBackend === 'redis') {
    if (!isValidRedisHostPort(input.redisAddr)) {
      return t('appConfig.validation.redisAddrRequired');
    }
    const trimmedPrefix = input.redisKeyPrefix.trim();
    if (trimmedPrefix && !REDIS_KEY_PREFIX_RE.test(trimmedPrefix)) {
      return t('appConfig.validation.redisKeyPrefixInvalid');
    }
    if (utf8ByteLength(input.redisUsername.trim()) > 128) {
      return t('appConfig.validation.redisUsernameTooLong');
    }
    if (
      input.isRedisPasswordConfigured &&
      (canonicalRedisAddrForCompare(input.redisAddr) !== canonicalRedisAddrForCompare(input.savedRedisAddr) ||
        input.redisTls !== input.savedRedisTls)
    ) {
      return t('appConfig.validation.redisPasswordReentryRequired');
    }
  }
  if (input.jobStoreBackend === 'postgres') {
    if (!isValidRedisHostPort(input.postgresAddr)) {
      return t('appConfig.validation.postgresAddrRequired');
    }
    const trimmedDb = input.postgresDatabase.trim();
    if (!trimmedDb || utf8ByteLength(trimmedDb) > 63 || hasForbiddenTextChar(trimmedDb, true)) {
      return t('appConfig.validation.postgresDatabaseRequired');
    }
    const trimmedUser = input.postgresUsername.trim();
    if (!trimmedUser || utf8ByteLength(trimmedUser) > 128 || hasForbiddenTextChar(trimmedUser, false)) {
      return t('appConfig.validation.postgresUsernameRequired');
    }
    const trimmedSchema = input.postgresSchema.trim();
    if (
      trimmedSchema &&
      (!POSTGRES_SCHEMA_RE.test(trimmedSchema) ||
        trimmedSchema.startsWith('pg_') ||
        trimmedSchema === 'information_schema')
    ) {
      return t('appConfig.validation.postgresSchemaInvalid');
    }
    const trimmedTablePrefix = input.postgresTablePrefix.trim();
    if (trimmedTablePrefix && !POSTGRES_TABLE_PREFIX_RE.test(trimmedTablePrefix)) {
      return t('appConfig.validation.postgresTablePrefixInvalid');
    }
    if (!ALLOWED_POSTGRES_SSL_MODES.has(input.postgresSslMode)) {
      return t('appConfig.validation.postgresSslModeInvalid');
    }
    if (
      input.isPostgresPasswordConfigured &&
      (canonicalRedisAddrForCompare(input.postgresAddr) !== canonicalRedisAddrForCompare(input.savedPostgresAddr) ||
        input.postgresSslMode !== input.savedPostgresSslMode)
    ) {
      return t('appConfig.validation.postgresPasswordReentryRequired');
    }
  }
  return null;
}
