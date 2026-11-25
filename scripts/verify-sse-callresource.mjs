#!/usr/bin/env node
/**
 * Live Grafana CallResource SSE verification harness (ADR-004 / Issue #7).
 *
 * Empirically verifies on a running Grafana OSS container (12.4.15 or 13.3.1):
 *   1. http.Flusher incremental chunk delivery over CallResource (generating stage arrives
 *      immediately while mock LLM sleeps, well before validating_ir / awaiting_preview / result).
 *   2. Patch SSE stage progression (generating -> validating_ir -> awaiting_preview -> result).
 *   3. Client-disconnect cancellation propagation through Grafana -> gRPC CallResource ->
 *      r.Context().Done() -> job cancelled + concurrency slot released well before LLM delay expires.
 *   4. Admin opt-in gate (enableSseProgress=false returns application/json even when client sends
 *      Accept: text/event-stream) and non-streaming JSON negotiation (Accept: application/json).
 */

const grafanaUrl = (process.env.GRAFANA_URL || 'http://127.0.0.1:3000').replace(/\/+$/, '');
const adminUser = process.env.GRAFANA_ADMIN_USER || process.env.GF_SECURITY_ADMIN_USER || 'admin';
const adminPassword =
  process.env.GRAFANA_ADMIN_PASSWORD || process.env.GF_SECURITY_ADMIN_PASSWORD || 'change-me-local-dev';
const pluginId = 'yuuki-llmstudio-app';
const resourceBase = `${grafanaUrl}/api/plugins/${pluginId}/resources`;
const authHeader = `Basic ${Buffer.from(`${adminUser}:${adminPassword}`).toString('base64')}`;

const SAMPLE_CATALOG = [
  { name: 'http_requests_total', type: 'counter', labels: ['service', 'status'] },
  { name: 'http_request_duration_seconds_bucket', type: 'histogram', labels: ['service', 'le'] },
];

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function assert(cond, msg) {
  if (!cond) {
    throw new Error(`ASSERTION FAILED: ${msg}`);
  }
}

async function getPluginSettings() {
  const res = await fetch(`${grafanaUrl}/api/plugins/${pluginId}/settings`, {
    headers: { Authorization: authHeader },
  });
  assert(res.ok, `GET plugin settings failed: HTTP ${res.status}`);
  return res.json();
}

let lastPluginSettingsPostMs = 0;

async function updatePluginSettings(baseSettings, jsonDataPatch) {
  // Grafana stores plugin_setting.updated at 1-second resolution in SQLite, and
  // grafana-plugin-sdk-go/backend/instancemgmt keys App instance invalidation on
  // AppInstanceSettings.Updated. Ensure consecutive POSTs land in distinct seconds.
  const elapsedSinceLastPost = Date.now() - lastPluginSettingsPostMs;
  if (lastPluginSettingsPostMs > 0 && elapsedSinceLastPost < 1100) {
    await sleep(1100 - elapsedSinceLastPost);
  }

  const nextJsonData = {
    ...(baseSettings.jsonData || {}),
    ...jsonDataPatch,
  };
  lastPluginSettingsPostMs = Date.now();
  const res = await fetch(`${grafanaUrl}/api/plugins/${pluginId}/settings`, {
    method: 'POST',
    headers: {
      Authorization: authHeader,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      enabled: baseSettings.enabled ?? true,
      pinned: baseSettings.pinned ?? true,
      jsonData: nextJsonData,
    }),
  });
  if (!res.ok) {
    throw new Error(`POST plugin settings failed: HTTP ${res.status} ${await res.text()}`);
  }

  // Wait until Grafana persists the updated jsonData and serves the reloaded plugin instance.
  const deadline = Date.now() + 15_000;
  let lastPersistedDelay;
  let lastPublicEnableSse;
  while (Date.now() < deadline) {
    try {
      if (jsonDataPatch.mockDelayMs !== undefined) {
        const persisted = await getPluginSettings();
        lastPersistedDelay = persisted?.jsonData?.mockDelayMs;
        if (lastPersistedDelay !== jsonDataPatch.mockDelayMs) {
          await sleep(200);
          continue;
        }
      }
      const cfgRes = await fetch(`${resourceBase}/config/public`, {
        headers: { Authorization: authHeader },
      });
      if (cfgRes.ok) {
        const cfg = await cfgRes.json();
        lastPublicEnableSse = cfg.enableSseProgress;
        if (
          jsonDataPatch.enableSseProgress === undefined ||
          cfg.enableSseProgress === jsonDataPatch.enableSseProgress
        ) {
          assert(!('mockDelayMs' in cfg), 'mockDelayMs must never leak in /config/public');
          return cfg;
        }
      }
    } catch {
      // Retry while plugin reloads.
    }
    await sleep(200);
  }
  throw new Error(
    `Timed out waiting for updated plugin settings to take effect ` +
      `(expected=${JSON.stringify(jsonDataPatch)}, lastPersistedMockDelayMs=${JSON.stringify(lastPersistedDelay)}, ` +
      `lastPublicEnableSse=${JSON.stringify(lastPublicEnableSse)})`
  );
}

function createSseParser() {
  let buffer = '';
  let currentEvent = '';
  let currentData = [];
  const dispatch = (out) => {
    if (currentData.length === 0 && currentEvent === '') {
      return;
    }
    out.push({ event: currentEvent || 'message', data: currentData.join('\n') });
    currentEvent = '';
    currentData = [];
  };
  const consumeLine = (line, out) => {
    if (line === '') {
      dispatch(out);
      return;
    }
    if (line.startsWith(':')) {
      return;
    }
    const idx = line.indexOf(':');
    const field = idx === -1 ? line : line.slice(0, idx);
    let val = idx === -1 ? '' : line.slice(idx + 1);
    if (val.startsWith(' ')) {
      val = val.slice(1);
    }
    if (field === 'event') {
      currentEvent = val;
    } else if (field === 'data') {
      currentData.push(val);
    }
  };
  return {
    push(chunk) {
      buffer += chunk;
      const out = [];
      let nl = buffer.indexOf('\n');
      while (nl !== -1) {
        let line = buffer.slice(0, nl);
        buffer = buffer.slice(nl + 1);
        if (line.endsWith('\r')) {
          line = line.slice(0, -1);
        }
        consumeLine(line, out);
        nl = buffer.indexOf('\n');
      }
      return out;
    },
    flush() {
      const out = [];
      if (buffer.length > 0) {
        const trailing = buffer.endsWith('\r') ? buffer.slice(0, -1) : buffer;
        buffer = '';
        consumeLine(trailing, out);
      }
      dispatch(out);
      return out;
    },
  };
}

async function readSseRequest(url, body, opts = {}) {
  const startMs = Date.now();
  const res = await fetch(url, {
    method: 'POST',
    headers: {
      Authorization: authHeader,
      'Content-Type': 'application/json',
      Accept: opts.accept || 'text/event-stream, application/json',
    },
    body: JSON.stringify(body),
    signal: opts.signal,
  });

  const headersAtMs = Date.now() - startMs;
  const contentType = res.headers.get('content-type') || '';
  const cacheControl = res.headers.get('cache-control') || '';
  const accelBuffering = res.headers.get('x-accel-buffering') || '';

  if (!res.ok) {
    throw new Error(`Expected 200 OK from ${url}, got HTTP ${res.status}: ${await res.text()}`);
  }
  assert(
    contentType.toLowerCase().startsWith('text/event-stream'),
    `Expected text/event-stream Content-Type, got ${JSON.stringify(contentType)}`
  );

  const parser = createSseParser();
  const decoder = new TextDecoder();
  const reader = res.body.getReader();
  const events = [];

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        const tail = decoder.decode();
        if (tail) {
          for (const frame of parser.push(tail)) {
            events.push({ ...frame, elapsedMs: Date.now() - startMs });
          }
        }
        for (const frame of parser.flush()) {
          events.push({ ...frame, elapsedMs: Date.now() - startMs });
        }
        break;
      }
      const text = decoder.decode(value, { stream: true });
      const frames = parser.push(text);
      for (const frame of frames) {
        const stamped = { ...frame, elapsedMs: Date.now() - startMs };
        events.push(stamped);
        if (opts.onFrame) {
          opts.onFrame(stamped);
        }
      }
    }
  } finally {
    reader.releaseLock();
  }

  return {
    status: res.status,
    contentType,
    cacheControl,
    accelBuffering,
    headersAtMs,
    totalMs: Date.now() - startMs,
    events,
  };
}

async function main() {
  const healthRes = await fetch(`${grafanaUrl}/api/health`);
  assert(healthRes.ok, `Grafana health failed: HTTP ${healthRes.status}`);
  const health = await healthRes.json();
  const grafanaVersion = health.version || 'unknown';
  console.log(`[sse-verify] Target Grafana version: ${grafanaVersion} (${grafanaUrl})`);

  const originalSettings = await getPluginSettings();
  assert(
    originalSettings?.jsonData?.llmMode === 'mock' && originalSettings?.jsonData?.allowMockInDev === true,
    'verify-sse-callresource.mjs requires dev mock mode (llmMode=mock, allowMockInDev=true)'
  );

  try {
    // Enable SSE + 1200ms artificial mock delay to prove real incremental flushing (not buffered end-of-request).
    await updatePluginSettings(originalSettings, {
      enableSseProgress: true,
      mockDelayMs: 1200,
    });

    // 1. Verify incremental flush on POST /jobs/generate
    const genRes = await readSseRequest(`${resourceBase}/jobs/generate`, {
      datasourceUid: 'prom-main',
      scope: { labelName: 'service', labelValue: 'checkout-api' },
      userIntent: 'Show request rate, 5xx error rate, and p95 latency',
      catalog: SAMPLE_CATALOG,
    });

    console.log(
      `[sse-verify] generate response headers: content-type=${JSON.stringify(genRes.contentType)}, cache-control=${JSON.stringify(genRes.cacheControl)}, x-accel-buffering=${JSON.stringify(genRes.accelBuffering)}`
    );
    assert(
      genRes.cacheControl.toLowerCase().includes('no-cache') ||
        genRes.cacheControl.toLowerCase().includes('no-store'),
      `Expected Cache-Control no-cache or no-store, got ${JSON.stringify(genRes.cacheControl)}`
    );
    assert(
      genRes.accelBuffering.toLowerCase() === 'no',
      `Expected X-Accel-Buffering: no, got ${JSON.stringify(genRes.accelBuffering)}`
    );

    const genStages = genRes.events.filter((e) => e.event === 'stage');
    const genResult = genRes.events.find((e) => e.event === 'result');
    assert(genResult, 'Missing terminal SSE result event on generate');

    const parsedGenStages = genStages.map((e) => ({
      elapsedMs: e.elapsedMs,
      payload: JSON.parse(e.data),
      raw: e.data,
    }));
    for (const s of parsedGenStages) {
      assert(
        !s.raw.includes('"panels"') && !s.raw.includes('"currentIR"'),
        `Stage event leaked IR/token fragments: ${s.raw}`
      );
    }
    assert(
      JSON.stringify(parsedGenStages.map((s) => s.payload.stage)) ===
        JSON.stringify(['generating', 'validating_ir', 'awaiting_preview']),
      `Unexpected generate stages: ${JSON.stringify(parsedGenStages)}`
    );

    const genFirstMs = parsedGenStages[0].elapsedMs;
    const genFlushDeltaMs = genResult.elapsedMs - genFirstMs;
    assert(
      genFirstMs < 800,
      `First stage (generating) did not flush incrementally (arrived at ${genFirstMs}ms, expected < 800ms)`
    );
    assert(
      genFlushDeltaMs >= 800,
      `Chunk flush delta too small (${genFlushDeltaMs}ms, expected >= 800ms); response appears buffered!`
    );

    const genSnap = JSON.parse(genResult.data);
    assert(genSnap.status === 'awaiting_preview' && genSnap.currentIR?.revision === 1, 'Invalid generate result snapshot');
    console.log(
      `[sse-verify] PASS generate incremental flush: firstStage=${genFirstMs}ms, result=${genResult.elapsedMs}ms, delta=${genFlushDeltaMs}ms`
    );

    // 2. Verify incremental flush on POST /jobs/{id}/patch
    const patchRes = await readSseRequest(`${resourceBase}/jobs/${encodeURIComponent(genSnap.jobId)}/patch`, {
      baseRevision: 1,
      editIntent: 'Change error rate panel to timeseries',
      catalog: SAMPLE_CATALOG,
    });
    const patchStages = patchRes.events.filter((e) => e.event === 'stage').map((e) => ({
      elapsedMs: e.elapsedMs,
      payload: JSON.parse(e.data),
    }));
    const patchResult = patchRes.events.find((e) => e.event === 'result');
    assert(patchResult, 'Missing terminal SSE result event on patch');
    assert(
      JSON.stringify(patchStages.map((s) => s.payload.stage)) ===
        JSON.stringify(['generating', 'validating_ir', 'awaiting_preview']),
      `Unexpected patch stages: ${JSON.stringify(patchStages)}`
    );
    const patchFlushDeltaMs = patchResult.elapsedMs - patchStages[0].elapsedMs;
    assert(
      patchFlushDeltaMs >= 800,
      `Patch flush delta too small (${patchFlushDeltaMs}ms, expected >= 800ms)`
    );
    const patchSnap = JSON.parse(patchResult.data);
    assert(patchSnap.currentIR?.revision === 2, `Expected patch revision 2, got ${patchSnap.currentIR?.revision}`);
    console.log(
      `[sse-verify] PASS patch incremental flush: firstStage=${patchStages[0].elapsedMs}ms, result=${patchResult.elapsedMs}ms, delta=${patchFlushDeltaMs}ms`
    );

    // 3. Verify client-disconnect cancellation propagation over live CallResource
    await updatePluginSettings(originalSettings, {
      enableSseProgress: true,
      mockDelayMs: 6000,
    });

    const abortController = new AbortController();
    let disconnectedJobId = '';
    let disconnectStartMs = 0;

    let sawAbort = false;
    try {
      await readSseRequest(
        `${resourceBase}/jobs/generate`,
        {
          datasourceUid: 'prom-main',
          scope: { labelName: 'service', labelValue: 'checkout-api' },
          userIntent: 'Show request rate and latency',
          catalog: SAMPLE_CATALOG,
        },
        {
          signal: abortController.signal,
          onFrame(frame) {
            if (frame.event === 'stage' && !disconnectedJobId) {
              const ev = JSON.parse(frame.data);
              if (ev.jobId && ev.stage === 'generating') {
                disconnectedJobId = ev.jobId;
                disconnectStartMs = Date.now();
                abortController.abort();
              }
            }
          },
        }
      );
    } catch (err) {
      if (err && (err.name === 'AbortError' || String(err).includes('abort'))) {
        sawAbort = true;
      } else {
        throw err;
      }
    }
    assert(sawAbort && disconnectedJobId, 'Expected client abort right after generating stage emitted jobId');

    // Poll GET /jobs/{id} to confirm backend observed client disconnect and cancelled job well before 6000ms.
    let cancelledStatus = '';
    let cancelPropagatedMs = -1;
    const cancelDeadline = Date.now() + 3500;
    while (Date.now() < cancelDeadline) {
      const getRes = await fetch(`${resourceBase}/jobs/${encodeURIComponent(disconnectedJobId)}`, {
        headers: { Authorization: authHeader },
      });
      if (getRes.ok) {
        const snap = await getRes.json();
        cancelledStatus = snap.status;
        if (snap.status === 'cancelled') {
          cancelPropagatedMs = Date.now() - disconnectStartMs;
          break;
        }
      }
      await sleep(100);
    }
    assert(
      cancelledStatus === 'cancelled' && cancelPropagatedMs >= 0 && cancelPropagatedMs < 3500,
      `Client disconnect did not cancel CallResource job promptly (status=${cancelledStatus}, elapsed=${cancelPropagatedMs}ms)`
    );
    console.log(
      `[sse-verify] PASS client-disconnect cancellation: jobId=${disconnectedJobId}, status=cancelled in ${cancelPropagatedMs}ms (mockDelay=6000ms)`
    );

    // 3b. M-4: Verify per-user concurrency slot (limit=1) was genuinely released on the SAME plugin instance
    //     BEFORE calling updatePluginSettings (which would recycle the App and create a fresh MemoryStore).
    const slotProbeAbort = new AbortController();
    let slotProbeJobId = '';
    try {
      await readSseRequest(
        `${resourceBase}/jobs/generate`,
        {
          datasourceUid: 'prom-main',
          scope: { labelName: 'service', labelValue: 'checkout-api' },
          userIntent: 'Show request rate',
          catalog: SAMPLE_CATALOG,
        },
        {
          signal: slotProbeAbort.signal,
          onFrame(frame) {
            if (frame.event === 'stage' && !slotProbeJobId) {
              const ev = JSON.parse(frame.data);
              if (ev.jobId && ev.stage === 'generating') {
                slotProbeJobId = ev.jobId;
                slotProbeAbort.abort();
              }
            }
          },
        }
      );
    } catch (err) {
      if (!(err && (err.name === 'AbortError' || String(err).includes('abort')))) {
        throw err;
      }
    }
    assert(
      Boolean(slotProbeJobId) && slotProbeJobId !== disconnectedJobId,
      'Follow-up Generate on the same plugin instance failed to acquire slot immediately after client disconnect'
    );
    const probeDeadline = Date.now() + 3500;
    let probeCancelled = false;
    while (Date.now() < probeDeadline) {
      const getRes = await fetch(`${resourceBase}/jobs/${encodeURIComponent(slotProbeJobId)}`, {
        headers: { Authorization: authHeader },
      });
      if (getRes.ok && (await getRes.json()).status === 'cancelled') {
        probeCancelled = true;
        break;
      }
      await sleep(100);
    }
    assert(probeCancelled, `Slot probe job ${slotProbeJobId} did not unwind to cancelled`);
    console.log(
      `[sse-verify] PASS same-instance slot release: follow-up jobId=${slotProbeJobId} acquired slot immediately`
    );

    // 4. Verify Accept: application/json returns plain JSON + enableSseProgress=false gate
    await updatePluginSettings(originalSettings, {
      enableSseProgress: true,
      mockDelayMs: 0,
    });
    const jsonNegRes = await fetch(`${resourceBase}/jobs/generate`, {
      method: 'POST',
      headers: {
        Authorization: authHeader,
        'Content-Type': 'application/json',
        Accept: 'application/json',
      },
      body: JSON.stringify({
        datasourceUid: 'prom-main',
        scope: { labelName: 'service', labelValue: 'checkout-api' },
        userIntent: 'Show request rate',
        catalog: SAMPLE_CATALOG,
      }),
    });
    assert(jsonNegRes.ok, `Non-streaming Accept: application/json failed: HTTP ${jsonNegRes.status}`);
    assert(
      (jsonNegRes.headers.get('content-type') || '').includes('application/json'),
      `Expected application/json for Accept: application/json, got ${jsonNegRes.headers.get('content-type')}`
    );

    await updatePluginSettings(originalSettings, {
      enableSseProgress: false,
      mockDelayMs: 0,
    });
    const disabledGateRes = await fetch(`${resourceBase}/jobs/generate`, {
      method: 'POST',
      headers: {
        Authorization: authHeader,
        'Content-Type': 'application/json',
        Accept: 'text/event-stream, application/json',
      },
      body: JSON.stringify({
        datasourceUid: 'prom-main',
        scope: { labelName: 'service', labelValue: 'checkout-api' },
        userIntent: 'Show request rate',
        catalog: SAMPLE_CATALOG,
      }),
    });
    assert(disabledGateRes.ok, `SSE-disabled gate request failed: HTTP ${disabledGateRes.status}`);
    assert(
      (disabledGateRes.headers.get('content-type') || '').includes('application/json'),
      `Expected application/json when enableSseProgress=false, got ${disabledGateRes.headers.get('content-type')}`
    );

    console.log(
      `[sse-verify] ALL CHECKS PASSED on Grafana ${grafanaVersion}: ` +
        JSON.stringify({
          grafanaVersion,
          genFirstMs,
          genResultMs: genResult.elapsedMs,
          genFlushDeltaMs,
          patchFlushDeltaMs,
          cancelPropagatedMs,
        })
    );
  } finally {
    await updatePluginSettings(originalSettings, {
      enableSseProgress: Boolean(originalSettings?.jsonData?.enableSseProgress),
      mockDelayMs: 0,
    }).catch((err) => {
      console.error('[sse-verify] warning: failed to restore plugin settings:', err);
    });
  }
}

main().catch((err) => {
  console.error('[sse-verify] FAILED:', err instanceof Error ? err.message : err);
  process.exit(1);
});
