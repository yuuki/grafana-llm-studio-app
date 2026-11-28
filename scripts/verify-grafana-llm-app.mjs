#!/usr/bin/env node
/**
 * End-to-end protocol verification against a REAL Grafana + grafana-llm-app + Prometheus
 * Docker stack (started via deploy/docker-compose.yml + deploy/docker-compose.bench-verify.yml).
 *
 * Verifies:
 * 1. Real grafana-llm-app provisioning & health check via Viewer-role Service Account token.
 * 2. Full 9-scenario cmd/llm-bench suite through real grafana-llm-app in:
 *    - openai-gpt41-mini preset (json_schema, strict=false, max_completion_tokens, temperature set, no reasoning_effort)
 *    - deepseek preset (json_object + embedded schema, max_tokens, no max_completion_tokens)
 *    - openai-o3 preset (large tier, json_schema, max_completion_tokens=4096, reasoning_effort=low, temperature omitted)
 * 3. Live PromQL execution of every generated/patched/repaired query against real Prometheus (prom/prometheus:v3.5.0).
 * 4. Real grafana-llm-app wire behavior for transient HTTP 503 retry recovery and finish_reason=length detection.
 */

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import assert from 'node:assert/strict';

const grafanaUrl = (process.env.GRAFANA_URL || 'http://127.0.0.1:3000').replace(/\/+$/, '');
const promUrl = (process.env.PROMETHEUS_URL || 'http://127.0.0.1:9090').replace(/\/+$/, '');
const stubUrl = (process.env.STUB_URL || 'http://127.0.0.1:18080').replace(/\/+$/, '');
const adminUser = process.env.GRAFANA_ADMIN_USER || 'admin';
const adminPassword = process.env.GRAFANA_ADMIN_PASSWORD || 'change-me-local-dev';
const benchBin = process.env.LLM_BENCH_BIN || 'go';
const useGoRun = benchBin === 'go';
const timeoutMs = Number(process.env.VERIFY_TIMEOUT_MS || 180_000);

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

async function waitFor(label, fn, deadline) {
  let lastErr = 'not ready';
  while (Date.now() < deadline) {
    try {
      const ok = await fn();
      if (ok) {
        console.log(`[ready] ${label}: ${typeof ok === 'string' ? ok : 'ok'}`);
        return ok;
      }
    } catch (err) {
      lastErr = err instanceof Error ? err.message : String(err);
    }
    await sleep(2000);
  }
  throw new Error(`Timed out waiting for ${label} (last error: ${lastErr})`);
}

async function fetchJSON(url, opts = {}) {
  const res = await fetch(url, opts);
  const text = await res.text();
  let json = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    json = null;
  }
  return { ok: res.ok, status: res.status, text, json };
}

function runBench(args, saToken, expectFailure = false) {
  const cmd = useGoRun ? 'go' : benchBin;
  const fullArgs = useGoRun ? ['run', './cmd/llm-bench', ...args] : args;
  try {
    const out = execFileSync(cmd, fullArgs, {
      encoding: 'utf8',
      env: { ...process.env, GRAFANA_SA_TOKEN: saToken },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    if (expectFailure) {
      throw new Error(`Expected llm-bench to exit non-zero, but succeeded:\n${out}`);
    }
    return { status: 0, stdout: out };
  } catch (err) {
    if (expectFailure && err.status && err.status !== 0) {
      return { status: err.status, stdout: String(err.stdout || ''), stderr: String(err.stderr || '') };
    }
    console.error('llm-bench stdout:\n', err.stdout ? String(err.stdout) : '');
    console.error('llm-bench stderr:\n', err.stderr ? String(err.stderr) : '');
    throw err;
  }
}

async function main() {
  const deadline = Date.now() + timeoutMs;

  // 1. Wait for stub, Grafana, grafana-llm-app health, and Prometheus sample-metrics scrape
  await waitFor(
    'OpenAI-compat capture stub',
    async () => {
      const r = await fetchJSON(`${stubUrl}/health`);
      return r.ok ? 'up' : false;
    },
    deadline
  );

  await waitFor(
    'Grafana /api/health',
    async () => {
      const r = await fetchJSON(`${grafanaUrl}/api/health`);
      if (!r.ok || r.json?.database !== 'ok') {
        throw new Error(`HTTP ${r.status}`);
      }
      return `version=${r.json.version}`;
    },
    deadline
  );

  // Mint minimum-privilege Viewer Service Account token
  const saToken = execFileSync('node', ['scripts/bootstrap-bench-sa.mjs'], {
    encoding: 'utf8',
    env: {
      ...process.env,
      GRAFANA_URL: grafanaUrl,
      GRAFANA_ADMIN_USER: adminUser,
      GRAFANA_ADMIN_PASSWORD: adminPassword,
    },
  }).trim();
  assert.ok(saToken.startsWith('glsa_'), 'expected Viewer SA token starting with glsa_');
  console.log('[ok] Bootstrapped Viewer Service Account token');

  await waitFor(
    'grafana-llm-app health (via Viewer SA token)',
    async () => {
      const r = await fetchJSON(`${grafanaUrl}/api/plugins/grafana-llm-app/health`, {
        headers: { Authorization: `Bearer ${saToken}` },
      });
      if (!r.ok) {
        throw new Error(`HTTP ${r.status}: ${r.text.slice(0, 200)}`);
      }
      const llmProvider = r.json?.details?.llmProvider;
      if (!llmProvider?.configured || !llmProvider?.ok) {
        throw new Error(`llmProvider not ok: ${JSON.stringify(llmProvider)}`);
      }
      return `version=${r.json?.details?.version || 'unknown'}, provider_ok=true`;
    },
    deadline
  );

  await waitFor(
    'Prometheus live rate() series for checkout-api',
    async () => {
      const now = Math.floor(Date.now() / 1000);
      const q = new URLSearchParams({
        query: 'sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))',
        start: String(now - 300),
        end: String(now),
        step: '30s',
      });
      const r = await fetchJSON(`${promUrl}/api/v1/query_range?${q.toString()}`);
      if (!r.ok || r.json?.status !== 'success') {
        throw new Error(`HTTP ${r.status}`);
      }
      const series = (r.json?.data?.result || []).filter((s) => Array.isArray(s.values) && s.values.length > 0);
      if (series.length === 0) {
        throw new Error('waiting for >=2 Prometheus scrapes');
      }
      return `${series.length} non-empty series`;
    },
    deadline
  );

  // 2. Verify Preset A: openai-gpt41-mini (json_schema, max_completion_tokens, no reasoning_effort)
  await fetchJSON(`${stubUrl}/_reset`, { method: 'POST' });
  console.log('\n--- Running Preset: openai-gpt41-mini (full 9 scenarios + live Prometheus) ---');
  runBench(
    [
      '--grafana-url', grafanaUrl,
      '--preset', 'openai-gpt41-mini',
      '--provider-label', 'real-grafana-llm-app/json_schema',
      '--prometheus-url', promUrl,
    ],
    saToken
  );
  const capA = (await fetchJSON(`${stubUrl}/_captured`)).json;
  assert.equal(capA.length, 9, `expected 9 upstream calls for openai-gpt41-mini, got ${capA.length}`);
  const reqA = capA[0].body;
  assert.equal(reqA.model, 'bench-base-model', 'base tier must map to bench-base-model');
  assert.equal(reqA.response_format?.type, 'json_schema');
  assert.equal(reqA.response_format?.json_schema?.name, 'dashboard_ir');
  assert.equal(Boolean(reqA.response_format?.json_schema?.strict), false);
  assert.equal(reqA.max_completion_tokens, 2048);
  assert.equal(reqA.max_tokens, undefined);
  assert.equal(reqA.reasoning_effort, undefined);
  assert.ok(typeof reqA.temperature === 'number' && reqA.temperature > 0, 'temperature should be forwarded when reasoning_effort is empty');
  const dsEnum = reqA.response_format?.json_schema?.schema?.properties?.panels?.items?.properties?.datasourceRef?.enum;
  assert.deepEqual(dsEnum, ['selected_prometheus'], 'string enum constraint must survive real grafana-llm-app relay');
  const patchReqA = capA.find((c) => c.body?.response_format?.json_schema?.name === 'dashboard_patch');
  assert.ok(patchReqA, 'dashboard_patch json_schema must be relayed by real grafana-llm-app');
  console.log('[PASS] Real grafana-llm-app verified: json_schema (dashboard_ir + dashboard_patch), max_completion_tokens, live Prometheus');

  // 3. Verify Preset B: deepseek (json_object + embedded schema, max_tokens)
  await fetchJSON(`${stubUrl}/_reset`, { method: 'POST' });
  console.log('\n--- Running Preset: deepseek (json_object + max_tokens + live Prometheus) ---');
  runBench(
    [
      '--grafana-url', grafanaUrl,
      '--preset', 'deepseek',
      '--provider-label', 'real-grafana-llm-app/deepseek-compat',
      '--prometheus-url', promUrl,
    ],
    saToken
  );
  const capB = (await fetchJSON(`${stubUrl}/_captured`)).json;
  assert.equal(capB.length, 9, `expected 9 upstream calls for deepseek, got ${capB.length}`);
  const reqB = capB[0].body;
  assert.equal(reqB.model, 'bench-base-model');
  assert.equal(reqB.response_format?.type, 'json_object');
  assert.equal(reqB.max_tokens, 2048);
  assert.equal(reqB.max_completion_tokens, undefined);
  assert.ok(reqB.messages?.[0]?.content?.includes('JSON Schema'), 'system prompt must embed canonical JSON Schema in json_object mode');
  console.log('[PASS] Real grafana-llm-app verified: json_object + embedded schema + max_tokens');

  // 4. Verify Preset C: openai-o3 (large tier, reasoning_effort=low, temperature omitted, max_completion_tokens=4096)
  await fetchJSON(`${stubUrl}/_reset`, { method: 'POST' });
  console.log('\n--- Running Preset: openai-o3 (large tier + reasoning_effort=low) ---');
  runBench(
    [
      '--grafana-url', grafanaUrl,
      '--preset', 'openai-o3',
      '--provider-label', 'real-grafana-llm-app/reasoning-o3',
      '--scenarios', 'gen-red-en,patch-single-panel-p99,repair-syntax-paren',
      '--prometheus-url', promUrl,
    ],
    saToken
  );
  const capC = (await fetchJSON(`${stubUrl}/_captured`)).json;
  assert.equal(capC.length, 3, `expected 3 upstream calls for openai-o3, got ${capC.length}`);
  const reqC = capC[0].body;
  assert.equal(reqC.model, 'bench-large-model', 'large tier must map to bench-large-model');
  assert.equal(reqC.reasoning_effort, 'low', 'reasoning_effort=low must pass through real grafana-llm-app');
  assert.equal(reqC.max_completion_tokens, 4096);
  assert.equal(reqC.temperature, undefined, 'temperature must be omitted when reasoning_effort is set');
  console.log('[PASS] Real grafana-llm-app verified: large model tier + reasoning_effort=low + omitted temperature');

  // 5. Verify transient upstream HTTP 503 recovery through real grafana-llm-app error wrapping!
  await fetchJSON(`${stubUrl}/_reset`, { method: 'POST' });
  await fetchJSON(`${stubUrl}/_fault`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ status: 503, count: 1 }),
  });
  console.log('\n--- Verifying transient upstream HTTP 503 transport retry recovery ---');
  runBench(
    [
      '--grafana-url', grafanaUrl,
      '--preset', 'openai-gpt41-mini',
      '--scenarios', 'gen-red-en',
    ],
    saToken
  );
  const cap503 = (await fetchJSON(`${stubUrl}/_captured`)).json;
  assert.equal(cap503.length, 2, `expected exactly 2 upstream calls (1 failed 503 + 1 retry), got ${cap503.length}`);
  console.log('[PASS] Real grafana-llm-app verified: wrapped upstream HTTP 503 retried once and succeeded');

  // 6. Verify finish_reason=length triggers ErrOutputTruncated (non-zero exit with finish_reason failure stage)
  await fetchJSON(`${stubUrl}/_reset`, { method: 'POST' });
  await fetchJSON(`${stubUrl}/_fault`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ finishReason: 'length', count: 1 }),
  });
  console.log('\n--- Verifying finish_reason=length detection ---');
  const truncRes = runBench(
    [
      '--grafana-url', grafanaUrl,
      '--preset', 'openai-gpt41-mini',
      '--scenarios', 'gen-red-en',
    ],
    saToken,
    true
  );
  assert.ok(truncRes.stdout.includes('finish_reason'), 'report must classify truncation under finish_reason stage');
  assert.ok(truncRes.stdout.includes('`length`: 1'), 'report must count finish_reason=length');
  console.log('[PASS] Real grafana-llm-app verified: finish_reason=length cleanly detected as ErrOutputTruncated');

  // 7. Verify status_report JSON Schema and json_object wire relay through real grafana-llm-app
  await fetchJSON(`${stubUrl}/_reset`, { method: 'POST' });
  console.log('\n--- Verifying status_report json_schema & json_object wire relay ---');
  const statusReportSchema = JSON.parse(fs.readFileSync('schemas/status-report.schema.json', 'utf8'));
  const chatEndpoint = `${grafanaUrl}/api/plugins/grafana-llm-app/resources/llm/v1/chat/completions`;
  const statusUserPayload = JSON.stringify({
    task: 'analyze_operational_status',
    language: 'en',
    window: '1h',
    evidence: {
      alerts: [{ id: 'ev-alert-1', alertName: 'HighErrorRate', state: 'active', severity: 'critical' }],
    },
  });

  const repSchemaRes = await fetchJSON(chatEndpoint, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${saToken}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      model: 'base',
      messages: [
        { role: 'system', content: 'Return JSON matching status_report schema.' },
        { role: 'user', content: statusUserPayload },
      ],
      max_completion_tokens: 2048,
      response_format: {
        type: 'json_schema',
        json_schema: {
          name: 'status_report',
          strict: false,
          schema: statusReportSchema,
        },
      },
    }),
  });
  assert.equal(repSchemaRes.ok, true, `status_report json_schema relay failed: HTTP ${repSchemaRes.status} ${repSchemaRes.text}`);
  const parsedReport = JSON.parse(repSchemaRes.json?.choices?.[0]?.message?.content || '{}');
  assert.equal(parsedReport.schemaVersion, '1.0');
  assert.match(parsedReport.findings?.[0]?.id || '', /^f-[1-9][0-9]?$/);
  assert.deepEqual(parsedReport.findings?.[0]?.evidenceIds, ['ev-alert-1']);

  const repObjRes = await fetchJSON(chatEndpoint, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${saToken}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      model: 'base',
      messages: [
        { role: 'system', content: `Return JSON matching JSON Schema:\n${JSON.stringify(statusReportSchema)}` },
        { role: 'user', content: statusUserPayload },
      ],
      max_tokens: 2048,
      response_format: { type: 'json_object' },
    }),
  });
  assert.equal(repObjRes.ok, true, `status_report json_object relay failed: HTTP ${repObjRes.status} ${repObjRes.text}`);

  const capReport = (await fetchJSON(`${stubUrl}/_captured`)).json;
  assert.equal(capReport.length, 2, `expected 2 status_report upstream calls, got ${capReport.length}`);
  const relayedSchema = capReport[0].body?.response_format?.json_schema;
  assert.equal(relayedSchema?.name, 'status_report');
  assert.deepEqual(
    relayedSchema?.schema?.properties?.overallStatus?.enum,
    ['healthy', 'degraded', 'critical', 'insufficient_data'],
    'status_report overallStatus string enum must survive real grafana-llm-app relay'
  );
  assert.deepEqual(
    relayedSchema?.schema?.properties?.findings?.items?.properties?.category?.enum,
    ['alert', 'performance', 'incident', 'monitoring_gap'],
    'status_report findings.category string enum must survive real grafana-llm-app relay'
  );
  assert.equal(capReport[1].body?.response_format?.type, 'json_object');
  console.log('[PASS] Real grafana-llm-app verified: status_report json_schema + json_object wire relay');

  console.log('\nALL REAL grafana-llm-app PROTOCOL & BENCHMARK VERIFICATIONS PASSED!');
}

main().catch((err) => {
  console.error('\n[FAIL]', err instanceof Error ? err.stack || err.message : err);
  process.exit(1);
});
