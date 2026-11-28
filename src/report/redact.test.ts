import { AnalyzeStatusPayload } from '../ir/reportTypes';
import { redactAndBoundAnalyzePayload, redactSecrets, redactSecretsAndURLs } from './redact';

describe('report/redact', () => {
  test('redacts high-confidence secrets while avoiding K8s pod/label false positives', () => {
    const input = [
      'Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.abcdefghijklmnop',
      'token=glsa_abcdefghijklmnopqrstuvwxyz1234',
      'postgres://admin:supersecret@db.internal:5432/prod',
      'api_key=sk-proj-1234567890abcdefghijklmno',
      'pod=secret-manager-5f8d9c7b6-abcde',
      'namespace=token-service',
    ].join(' | ');

    const res = redactSecrets(input);
    expect(res.count).toBeGreaterThanOrEqual(4);
    expect(res.text).not.toContain('eyJhbGciOiJIUzI1NiJ9');
    expect(res.text).not.toContain('glsa_');
    expect(res.text).not.toContain('supersecret');
    expect(res.text).not.toContain('sk-proj-');
    expect(res.text).toContain('[REDACTED_URI]');
    // Kubernetes pod & namespace names must remain intact.
    expect(res.text).toContain('pod=secret-manager-5f8d9c7b6-abcde');
    expect(res.text).toContain('namespace=token-service');
  });

  test('scrubs http(s) URLs alongside secrets in redactSecretsAndURLs', () => {
    const res = redactSecretsAndURLs('See https://runbook.internal/ops?token=abc123456789 for checkout-api');
    expect(res.text).toBe('See [URL] for checkout-api');
    expect(res.count).toBeGreaterThanOrEqual(1);
  });

  test('deterministically trims oversized payloads under 24 KiB and records evidence_truncated warning', () => {
    const bigAlerts = Array.from({ length: 25 }, (_, i) => ({
      id: `ev-alert-${i + 1}`,
      alertName: `HighErrorRate_${i + 1}_${'N'.repeat(40)}`,
      state: 'active' as const,
      severity: 'critical',
      summary: 'A'.repeat(190),
      description: 'B'.repeat(290),
      labels: {
        service: 'checkout-api',
        instance: `10.0.0.${i + 1}:8080`,
        pod: `checkout-api-7f8d9c7b6-${i + 1000}`,
        cluster: 'prod-us-east-1-eks-primary',
        detail: 'https://internal.example.com/runbook?api_key=secretvalue12345',
      },
    }));
    const bigRules = Array.from({ length: 20 }, (_, i) => ({
      id: `ev-rule-${i + 1}`,
      ruleName: `RuleEvalFailure_${i + 1}_${'R'.repeat(40)}`,
      group: 'checkout-alert-rules-production',
      state: 'firing' as const,
      health: 'error' as const,
      lastError: 'E'.repeat(220),
      activeAlertsCount: 3,
    }));
    const bigMetrics = Array.from({ length: 20 }, (_, i) => ({
      id: `ev-metric-${i + 1}`,
      panelTitle: `Panel ${i + 1} ${'X'.repeat(80)}`,
      seriesLabel: `service=checkout-api,pod=pod-${i + 1},namespace=payments-prod`,
      queryStatus: 'ok' as const,
      current: { latest: 10 + i, avg: 9, p95: 12, min: 5, max: 15, sampleCount: 60, missingRatio: 0 },
      trend: 'rising' as const,
      thresholdBreached: 'warning' as const,
      anomalyHint: 'elevated' as const,
    }));
    const payload: AnalyzeStatusPayload = {
      language: 'en',
      window: '1h',
      baselineMode: 'preceding',
      collectionStatus: { alertsChecked: true, rulesChecked: true, totalRulesEvaluated: 25, activeAlertsTotal: 25, abnormalRulesTotal: 20 },
      evidence: {
        collectedAt: '2026-11-27T00:00:00Z',
        alerts: bigAlerts,
        rules: bigRules,
        metrics: bigMetrics,
      },
    };

    const bounded = redactAndBoundAnalyzePayload(payload);
    expect(bounded.redactionCount).toBeGreaterThan(0);
    expect(bounded.truncated).toBe(true);
    expect(Buffer.byteLength(JSON.stringify(bounded.payload), 'utf8')).toBeLessThanOrEqual(24 * 1024);
    expect(bounded.payload.collectionWarnings?.some((w) => w.code === 'evidence_truncated')).toBe(true);
    expect(bounded.payload.evidence.alerts[0].labels?.detail).toBe('[URL]');
  });

  test('redacts password-only Redis URI, quoted KV, truncated PEM, multi-cookie, scope.labelValue and re-clamps expanded strings to Go rune caps', () => {
    const raw = [
      'redis://:s3cr3tpass@redis.internal:6379/0',
      'password="quotedVal99" api_key=\'tok_live_12345\'',
      '-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAtokenBodyTruncatedHere',
      'Cookie: theme=dark; grafana_session=sessSecret999',
      'X-Api-Key: apiKeyHeader777',
    ].join('\n');
    const res = redactSecretsAndURLs(raw);
    for (const leak of ['s3cr3tpass', 'quotedVal99', 'tok_live_12345', 'MIIEpAIBAAKCAQEA', 'sessSecret999', 'apiKeyHeader777']) {
      expect(res.text).not.toContain(leak);
    }

    // Post-redaction expansion ("password=1" -> "password=[REDACTED]" adds +9 runes) must still stay <= 200 runes!
    const nearCapSummary = 'A'.repeat(188) + ' password=1';
    const bounded = redactAndBoundAnalyzePayload({
      language: 'en',
      window: '1h',
      baselineMode: 'preceding',
      scope: { labelName: 'service', labelValue: 'https://user:secret@svc.internal' },
      collectionStatus: { alertsChecked: true, rulesChecked: true, totalRulesEvaluated: 1, activeAlertsTotal: 1, abnormalRulesTotal: 0 },
      evidence: {
        collectedAt: '2026-11-27T00:00:00Z',
        alerts: [{ id: 'ev-alert-1', alertName: 'A'.repeat(120) + ' password=1', state: 'active', summary: nearCapSummary }],
        rules: [],
        metrics: [],
      },
    });
    expect(Array.from(bounded.payload.evidence.alerts[0].alertName).length).toBeLessThanOrEqual(128);
    expect(Array.from(bounded.payload.evidence.alerts[0].summary ?? '').length).toBeLessThanOrEqual(200);
    expect(bounded.payload.scope?.labelValue).not.toContain('secret');
    expect(bounded.payload.scope?.labelValue).not.toContain('https://');
  });
});
