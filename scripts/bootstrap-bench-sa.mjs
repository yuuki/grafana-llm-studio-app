#!/usr/bin/env node
/**
 * Creates (or reuses) a minimum-privilege Viewer Grafana Service Account and prints
 * a fresh token (`glsa_...`) to stdout for use with `cmd/llm-bench` (GRAFANA_SA_TOKEN).
 *
 * Usage:
 *   export GRAFANA_SA_TOKEN="$(node scripts/bootstrap-bench-sa.mjs)"
 */

const grafanaUrl = (process.env.GRAFANA_URL || 'http://127.0.0.1:3000').replace(/\/+$/, '');
const adminUser = process.env.GRAFANA_ADMIN_USER || process.env.GF_SECURITY_ADMIN_USER || 'admin';
const adminPassword =
  process.env.GRAFANA_ADMIN_PASSWORD || process.env.GF_SECURITY_ADMIN_PASSWORD || 'change-me-local-dev';
const saName = process.env.BENCH_SA_NAME || 'llm-studio-bench-viewer';
const tokenTTLSeconds = Number(process.env.BENCH_SA_TTL_SECONDS || 3600);

const authHeader = `Basic ${Buffer.from(`${adminUser}:${adminPassword}`).toString('base64')}`;

async function api(path, options = {}) {
  const res = await fetch(`${grafanaUrl}${path}`, {
    ...options,
    headers: {
      Authorization: authHeader,
      'Content-Type': 'application/json',
      ...(options.headers || {}),
    },
  });
  const text = await res.text();
  let json;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    json = null;
  }
  return { ok: res.ok, status: res.status, text, json };
}

async function main() {
  // 1. Search for an existing Viewer service account with the same name.
  const search = await api(`/api/serviceaccounts/search?query=${encodeURIComponent(saName)}`);
  if (!search.ok) {
    throw new Error(`search service accounts failed (HTTP ${search.status}): ${search.text}`);
  }
  const existing = (search.json?.serviceAccounts || []).find((sa) => sa.name === saName);
  let saId = existing?.id;

  if (!saId) {
    const created = await api('/api/serviceaccounts', {
      method: 'POST',
      body: JSON.stringify({
        name: saName,
        role: 'Viewer',
        isDisabled: false,
      }),
    });
    if (!created.ok) {
      throw new Error(`create Viewer service account failed (HTTP ${created.status}): ${created.text}`);
    }
    saId = created.json?.id;
  }

  if (!saId) {
    throw new Error('could not determine service account ID');
  }

  // 2. Mint a short-lived token scoped strictly to this Viewer service account.
  const tokenName = `bench-${Date.now()}`;
  const tokRes = await api(`/api/serviceaccounts/${saId}/tokens`, {
    method: 'POST',
    body: JSON.stringify({
      name: tokenName,
      secondsToLive: tokenTTLSeconds,
    }),
  });
  if (!tokRes.ok || !tokRes.json?.key) {
    throw new Error(`create service account token failed (HTTP ${tokRes.status}): ${tokRes.text}`);
  }

  // Output ONLY the token to stdout so callers can capture it via $(node scripts/bootstrap-bench-sa.mjs).
  process.stdout.write(`${tokRes.json.key}\n`);
}

main().catch((err) => {
  console.error(err instanceof Error ? err.message : String(err));
  process.exit(1);
});
