#!/usr/bin/env node
/**
 * Waits until the local Docker Compose stack (Grafana + plugin backend + Prometheus with
 * at least 2 sample-metrics scrapes for rate() queries) is ready for live E2E tests.
 */

const grafanaUrl = (process.env.GRAFANA_URL || 'http://127.0.0.1:3000').replace(/\/+$/, '');
const adminUser = process.env.GRAFANA_ADMIN_USER || process.env.GF_SECURITY_ADMIN_USER || 'admin';
const adminPassword =
  process.env.GRAFANA_ADMIN_PASSWORD || process.env.GF_SECURITY_ADMIN_PASSWORD || 'change-me-local-dev';
const timeoutMs = Number(process.env.E2E_WAIT_TIMEOUT_MS || 120_000);
const pollIntervalMs = 2000;

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitFor(label, checkFn, deadline) {
  let lastError = 'not ready';
  while (Date.now() < deadline) {
    try {
      const result = await checkFn();
      if (result) {
        console.log(`[ready] ${label}: ${typeof result === 'string' ? result : 'ok'}`);
        return result;
      }
    } catch (err) {
      lastError = err instanceof Error ? err.message : String(err);
    }
    await sleep(pollIntervalMs);
  }
  throw new Error(`Timed out waiting for ${label} after ${timeoutMs}ms (last error: ${lastError})`);
}

async function main() {
  const deadline = Date.now() + timeoutMs;

  // 1. Wait for Grafana /api/health
  await waitFor(
    `Grafana (${grafanaUrl}/api/health)`,
    async () => {
      const res = await fetch(`${grafanaUrl}/api/health`);
      if (!res.ok) {
        throw new Error(`HTTP ${res.status}`);
      }
      const body = await res.json();
      if (body.database !== 'ok') {
        throw new Error(`database=${body.database}`);
      }
      return `version=${body.version || 'unknown'}`;
    },
    deadline
  );

  // 2. Wait for yuuki-llmstudio-app backend plugin resource API (provisioned in mock mode)
  const basicAuth = Buffer.from(`${adminUser}:${adminPassword}`).toString('base64');
  await waitFor(
    'yuuki-llmstudio-app backend (/config/public)',
    async () => {
      const res = await fetch(
        `${grafanaUrl}/api/plugins/yuuki-llmstudio-app/resources/config/public`,
        {
          headers: {
            Authorization: `Basic ${basicAuth}`,
          },
        }
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(`HTTP ${res.status}: ${text.slice(0, 200)}`);
      }
      const cfg = await res.json();
      if (!Array.isArray(cfg.allowedDatasourceUids) || cfg.allowedDatasourceUids.length === 0) {
        throw new Error(`allowedDatasourceUids empty: ${JSON.stringify(cfg)}`);
      }
      return `llmMode=${cfg.llmMode}, allowedDatasources=${cfg.allowedDatasourceUids.join(',')}`;
    },
    deadline
  );

  // 3. Wait for a Grafana /api/ds/query range query (matching Dry-Run's now-5m / maxDataPoints=10 / intervalMs=30000
  //    step alignment) to return non-empty series so Dry-Run validation never races a 30s step boundary.
  await waitFor(
    `Grafana Prometheus range query (${grafanaUrl}/api/ds/query)`,
    async () => {
      const res = await fetch(`${grafanaUrl}/api/ds/query`, {
        method: 'POST',
        headers: {
          Authorization: `Basic ${basicAuth}`,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({
          from: 'now-5m',
          to: 'now',
          queries: [
            {
              refId: 'A',
              datasource: { type: 'prometheus', uid: 'prom-main' },
              expr: 'sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))',
              maxDataPoints: 10,
              intervalMs: 30000,
              range: true,
              instant: false,
            },
          ],
        }),
      });
      if (!res.ok) {
        const text = await res.text();
        throw new Error(`HTTP ${res.status}: ${text.slice(0, 200)}`);
      }
      const body = await res.json();
      const frames = body?.results?.A?.frames ?? [];
      const nonEmpty = frames.filter(
        (f) => Array.isArray(f?.data?.values) && f.data.values.some((col) => Array.isArray(col) && col.length > 0)
      ).length;
      if (nonEmpty === 0) {
        throw new Error('waiting for aligned 30s step range query to return non-empty series');
      }
      return `${nonEmpty} non-empty range series`;
    },
    deadline
  );
}

main().catch((err) => {
  console.error(err instanceof Error ? err.message : err);
  process.exit(1);
});
