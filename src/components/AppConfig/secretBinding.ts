import { PostgresSslMode } from './types';

export const BOUND_REDIS_PASSWORD_PREFIX = 'llmstudio-redis-bound-v1:';
export const BOUND_POSTGRES_PASSWORD_PREFIX = 'llmstudio-postgres-bound-v1:';
export const BOUND_GRAFANA_TOKEN_PREFIX = 'llmstudio-grafana-bound-v1:';

export function canonicalRedisAddrForCompare(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) {
    return '';
  }
  if (trimmed.startsWith('[')) {
    const closeIdx = trimmed.indexOf(']');
    if (closeIdx > 1 && trimmed[closeIdx + 1] === ':') {
      const host = trimmed.slice(1, closeIdx).toLowerCase();
      const port = trimmed.slice(closeIdx + 2);
      return `[${host}]:${port}`;
    }
  }
  const lastColon = trimmed.lastIndexOf(':');
  if (lastColon > 0) {
    return `${trimmed.slice(0, lastColon).toLowerCase()}:${trimmed.slice(lastColon + 1)}`;
  }
  return trimmed.toLowerCase();
}

export function canonicalGrafanaUrlForCompare(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) {
    return '';
  }
  try {
    const u = new URL(trimmed);
    const scheme = u.protocol.toLowerCase();
    const port = u.port || (scheme === 'https:' ? '443' : scheme === 'http:' ? '80' : '');
    const path = u.pathname.replace(/\/+$/, '');
    return `${scheme}//${u.hostname.toLowerCase()}:${port}${path}${u.search}${u.hash}`;
  } catch {
    return trimmed.replace(/\/+$/, '').toLowerCase();
  }
}

export function encodeBoundRedisPassword(addr: string, tls: boolean, password: string): string {
  return (
    BOUND_REDIS_PASSWORD_PREFIX +
    JSON.stringify({
      addr: addr.trim(),
      tls,
      password,
    })
  );
}

export function encodeBoundPostgresPassword(
  addr: string,
  sslMode: PostgresSslMode,
  password: string
): string {
  return (
    BOUND_POSTGRES_PASSWORD_PREFIX +
    JSON.stringify({
      addr: addr.trim(),
      sslMode,
      password,
    })
  );
}

export function encodeBoundGrafanaToken(grafanaUrl: string, token: string): string {
  return (
    BOUND_GRAFANA_TOKEN_PREFIX +
    JSON.stringify({
      url: grafanaUrl.trim(),
      token: token.trim(),
    })
  );
}
