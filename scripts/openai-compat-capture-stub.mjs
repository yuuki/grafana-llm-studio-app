#!/usr/bin/env node
/**
 * Deterministic OpenAI-compatible stub & wire-capture server used ONLY to verify
 * the REAL `grafana-llm-app` container wire protocol (json_schema, json_object,
 * max_completion_tokens, max_tokens, reasoning_effort, finish_reason, and HTTP error
 * wrapping) alongside `cmd/llm-bench` and live Prometheus.
 *
 * Not intended for production use.
 */

import http from 'node:http';

const port = Number(process.env.STUB_PORT || 18080);
const host = process.env.STUB_HOST || '0.0.0.0';

const captured = [];
let faultConfig = { status: 0, finishReason: '', remaining: 0 };

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function buildResponseContent(reqBody) {
  const msgs = Array.isArray(reqBody?.messages) ? reqBody.messages : [];
  const userMsg = msgs.find((m) => m?.role === 'user') || msgs[msgs.length - 1];
  let payload = {};
  try {
    payload = userMsg?.content ? JSON.parse(userMsg.content) : {};
  } catch {
    payload = {};
  }

  switch (payload.task) {
    case 'generate_dashboard_ir':
      return JSON.stringify({
        schemaVersion: '1.0',
        revision: 1,
        title: 'checkout-api Observability Overview',
        description: 'Generated via real grafana-llm-app wire verification stub.',
        timeRange: { from: 'now-15m', to: 'now' },
        panels: [
          {
            id: 'panel-request-rate',
            type: 'timeseries',
            title: 'Request Rate',
            datasourceRef: 'selected_prometheus',
            query: {
              expr: 'sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))',
              legendFormat: '{{status}}',
            },
            unit: 'reqps',
            layout: { row: 0, col: 0, width: 'half' },
          },
          {
            id: 'panel-error-rate',
            type: 'stat',
            title: '5xx Error Ratio',
            datasourceRef: 'selected_prometheus',
            query: {
              expr: 'sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m])) / clamp_min(sum(rate(http_requests_total{service="checkout-api"}[5m])), 1e-9)',
              legendFormat: '5xx ratio',
            },
            unit: 'percentunit',
            thresholds: [
              { value: 0, color: 'green' },
              { value: 0.01, color: 'yellow' },
              { value: 0.05, color: 'red' },
            ],
            layout: { row: 0, col: 1, width: 'half' },
          },
          {
            id: 'panel-latency-p95',
            type: 'timeseries',
            title: 'p95 Request Latency',
            datasourceRef: 'selected_prometheus',
            query: {
              expr: 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout-api"}[5m])))',
              legendFormat: 'p95',
            },
            unit: 's',
            layout: { row: 1, col: 0, width: 'half' },
          },
          {
            id: 'panel-memory-bytes',
            type: 'stat',
            title: 'Resident Memory',
            datasourceRef: 'selected_prometheus',
            query: {
              expr: 'avg(process_resident_memory_bytes{service="checkout-api"})',
              legendFormat: 'memory',
            },
            unit: 'bytes',
            layout: { row: 1, col: 1, width: 'half' },
          },
        ],
      });

    case 'generate_dashboard_patch': {
      const targetId = payload.targetPanelId || '';
      const editReq = String(payload.editRequest || '').toLowerCase();
      if (targetId === 'panel-latency-p95') {
        return JSON.stringify({
          schemaVersion: '1.0',
          baseRevision: payload.baseRevision || 1,
          summary: 'Updated latency quantile from p95 to p99',
          operations: [
            {
              op: 'update_panel',
              panelId: 'panel-latency-p95',
              panel: {
                title: 'p99 Request Latency',
                query: {
                  expr: 'histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout-api"}[5m])))',
                  legendFormat: 'p99',
                },
              },
            },
          ],
        });
      }
      if (editReq.includes('add a new stat panel')) {
        return JSON.stringify({
          schemaVersion: '1.0',
          baseRevision: payload.baseRevision || 1,
          summary: 'Added resident memory stat panel on row 1',
          operations: [
            {
              op: 'add_panel',
              panelId: 'panel-memory-bytes',
              panel: {
                id: 'panel-memory-bytes',
                type: 'stat',
                title: 'Resident Memory',
                datasourceRef: 'selected_prometheus',
                query: {
                  expr: 'avg(process_resident_memory_bytes{service="checkout-api"})',
                  legendFormat: 'memory',
                },
                unit: 'bytes',
                layout: { row: 1, col: 0, width: 'full' },
              },
            },
          ],
        });
      }
      return JSON.stringify({
        schemaVersion: '1.0',
        baseRevision: payload.baseRevision || 1,
        summary: 'Updated timeRange to now-1h and expanded error ratio to 4xx/5xx',
        operations: [
          {
            op: 'update_meta',
            timeRange: { from: 'now-1h', to: 'now' },
          },
          {
            op: 'update_panel',
            panelId: 'panel-error-rate',
            panel: {
              query: {
                expr: 'sum(rate(http_requests_total{service="checkout-api",status=~"4..|5.."}[5m])) / clamp_min(sum(rate(http_requests_total{service="checkout-api"}[5m])), 1e-9)',
              },
            },
          },
        ],
      });
    }

    case 'repair_syntax_errors': {
      const failing = Array.isArray(payload.failingPanels) ? payload.failingPanels : [];
      const firstId = failing[0]?.panelId || 'panel-request-rate';
      const repairedExpr =
        firstId === 'panel-error-ratio'
          ? 'sum(rate(http_requests_total{service="checkout-api",status="500"}[5m])) / clamp_min(sum(rate(http_requests_total{service="checkout-api"}[5m])), 1e-9)'
          : 'sum by (status) (rate(http_requests_total{service="checkout-api"}[5m]))';
      return JSON.stringify({
        schemaVersion: '1.0',
        baseRevision: payload.baseRevision || 1,
        summary: `Repaired PromQL syntax error on ${firstId}`,
        operations: [
          {
            op: 'update_panel',
            panelId: firstId,
            panel: {
              query: { expr: repairedExpr },
            },
          },
        ],
      });
    }

    case 'analyze_operational_status':
      return JSON.stringify({
        schemaVersion: '1.0',
        overallStatus: 'degraded',
        confidence: 'high',
        executiveSummary: 'Checkout API is degraded with HighErrorRate active.',
        timeline: [
          {
            timestamp: '2026-11-27T00:05:00Z',
            event: 'HighErrorRate alert fired on checkout-api',
            evidenceIds: ['ev-alert-1'],
          },
        ],
        findings: [
          {
            id: 'finding-1',
            category: 'alert',
            severity: 'critical',
            title: 'HighErrorRate firing',
            detail: 'Checkout API 5xx ratio exceeded 5 percent.',
            evidenceIds: ['ev-alert-1'],
          },
        ],
        hypotheses: [
          {
            title: 'Upstream dependency timeout',
            likelihood: 'high',
            rationale: 'Error alert correlates with recent latency increase.',
            evidenceIds: ['ev-alert-1'],
            verificationSteps: ['Check upstream payment gateway latency'],
          },
        ],
        recommendedActions: [
          {
            priority: 'immediate',
            action: 'Inspect recent checkout-api rollout',
            relatedFindingIds: ['finding-1'],
          },
        ],
        dataGaps: ['Only Grafana Unified Alerting evidence provided'],
        suggestedInvestigationIntent: 'Build a troubleshooting dashboard for checkout-api 5xx errors',
      });

    default:
      // Fallback for grafana-llm-app health check probe ("Hello" / short test message)
      return JSON.stringify({ ok: true });
  }
}

const server = http.createServer(async (req, res) => {
  try {
    const urlPath = (req.url || '/').split('?')[0];

    if (req.method === 'GET' && urlPath === '/health') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ status: 'ok' }));
      return;
    }

    if (req.method === 'GET' && urlPath === '/_captured') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(captured, null, 2));
      return;
    }

    if (req.method === 'POST' && urlPath === '/_reset') {
      captured.length = 0;
      faultConfig = { status: 0, finishReason: '', remaining: 0 };
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ reset: true }));
      return;
    }

    if (req.method === 'POST' && urlPath === '/_fault') {
      const raw = await readBody(req);
      const parsed = raw ? JSON.parse(raw) : {};
      faultConfig = {
        status: Number(parsed.status || 0),
        finishReason: String(parsed.finishReason || ''),
        remaining: Number(parsed.count || 1),
      };
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ faultConfig }));
      return;
    }

    if (req.method === 'GET' && urlPath === '/v1/models') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(
        JSON.stringify({
          object: 'list',
          data: [
            { id: 'bench-base-model', object: 'model' },
            { id: 'bench-large-model', object: 'model' },
          ],
        })
      );
      return;
    }

    if (req.method === 'POST' && urlPath === '/v1/chat/completions') {
      const raw = await readBody(req);
      const body = JSON.parse(raw);
      captured.push({
        timestamp: new Date().toISOString(),
        path: urlPath,
        body,
      });

      if (faultConfig.remaining > 0 && faultConfig.status > 0) {
        faultConfig.remaining -= 1;
        res.writeHead(faultConfig.status, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            error: {
              message: `injected upstream status ${faultConfig.status}`,
              type: 'server_error',
            },
          })
        );
        return;
      }

      let finishReason = 'stop';
      if (faultConfig.remaining > 0 && faultConfig.finishReason) {
        faultConfig.remaining -= 1;
        finishReason = faultConfig.finishReason;
      }

      const content = buildResponseContent(body);
      const model = body.model || 'bench-base-model';
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(
        JSON.stringify({
          id: `chatcmpl-stub-${captured.length}`,
          object: 'chat.completion',
          created: Math.floor(Date.now() / 1000),
          model,
          choices: [
            {
              index: 0,
              message: {
                role: 'assistant',
                content,
              },
              finish_reason: finishReason,
            },
          ],
          usage: {
            prompt_tokens: Math.max(100, Math.round(raw.length / 4)),
            completion_tokens: Math.max(40, Math.round(content.length / 4)),
            total_tokens: Math.max(140, Math.round((raw.length + content.length) / 4)),
          },
        })
      );
      return;
    }

    res.writeHead(404, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: `not found: ${req.method} ${urlPath}` }));
  } catch (err) {
    res.writeHead(500, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: err instanceof Error ? err.message : String(err) }));
  }
});

server.listen(port, host, () => {
  console.log(`[openai-compat-capture-stub] listening on http://${host}:${port}`);
});
