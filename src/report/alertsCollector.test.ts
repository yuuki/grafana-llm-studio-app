import { collectGrafanaAlertingEvidence } from './alertsCollector';

const mockGet = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    get: (...args: unknown[]) => mockGet(...args),
  }),
}));

describe('report/alertsCollector', () => {
  beforeEach(() => {
    mockGet.mockReset();
  });

  test('collects v2 alerts and v1 rules, handles DatasourceNoData rulename, annotations.message, and deduplicates via __alert_rule_uid__', async () => {
    mockGet.mockImplementation(async (url: string) => {
      if (url.startsWith('/api/alertmanager/grafana/api/v2/alerts')) {
        return [
          {
            labels: {
              alertname: 'HighErrorRate',
              severity: 'critical',
              service: 'checkout-api',
              __alert_rule_uid__: 'uid-err-1',
            },
            annotations: { message: '5xx exceeded 5%' },
            startsAt: '2026-11-27T00:05:00Z',
            status: { state: 'active' },
          },
          {
            labels: {
              alertname: 'DatasourceNoData',
              rulename: 'CheckoutScrapeMissing',
              severity: 'warning',
              service: 'checkout-api',
            },
            annotations: { summary: 'No data from checkout scrape' },
            startsAt: '2026-11-27T00:02:00Z',
            status: { state: 'active' },
          },
          {
            labels: {
              alertname: 'BillingCriticalOutage',
              severity: 'critical',
              service: 'billing-api',
            },
            annotations: { summary: 'Billing DB unreachable' },
            startsAt: '2026-11-27T00:08:00Z',
            status: { state: 'active' },
          },
          {
            labels: {
              alertname: 'UnrelatedWarning',
              severity: 'warning',
              service: 'search-api',
            },
            status: { state: 'active' },
          },
        ];
      }
      if (url.startsWith('/api/prometheus/grafana/api/v1/rules')) {
        return {
          data: {
            groups: [
              {
                name: 'checkout-rules',
                rules: [
                  // Recording rule: must be skipped
                  { name: 'job:http_requests:rate5m', type: 'recording', state: 'inactive', health: 'ok' },
                  // Already covered in /v2/alerts via __alert_rule_uid__ and health=ok: must be deduplicated
                  {
                    name: 'HighErrorRate',
                    type: 'alerting',
                    state: 'firing',
                    health: 'ok',
                    labels: { __alert_rule_uid__: 'uid-err-1', service: 'checkout-api' },
                    alerts: [{ state: 'firing', labels: { service: 'checkout-api' } }],
                  },
                  // Firing rule routed to external Alertmanager (not in /v2/alerts): must be kept!
                  {
                    name: 'ExternalAMRule',
                    type: 'alerting',
                    state: 'firing',
                    health: 'ok',
                    labels: { service: 'checkout-api' },
                    alerts: [{ state: 'firing', labels: { service: 'checkout-api' } }],
                  },
                  // Rule with evaluation error: must be kept even if org-wide
                  {
                    name: 'BrokenQueryRule',
                    type: 'alerting',
                    state: 'inactive',
                    health: 'error',
                    lastError: 'failed to query datasource',
                    labels: { service: 'payments' },
                  },
                ],
              },
            ],
          },
        };
      }
      throw new Error(`unexpected URL ${url}`);
    });

    const res = await collectGrafanaAlertingEvidence({
      scope: { labelName: 'service', labelValue: 'checkout-api' },
      includeOrgCriticalAlerts: true,
    });

    // Unrelated warning on search-api filtered out; BillingCriticalOutage kept because includeOrgCriticalAlerts=true
    expect(res.alerts.map((a) => a.alertName)).toEqual([
      'HighErrorRate',
      'BillingCriticalOutage',
      'CheckoutScrapeMissing',
    ]);
    expect(res.alerts[0].summary).toBe('5xx exceeded 5%');
    expect(res.alerts[0].matchesScope).toBe(true);
    expect(res.alerts[1].matchesScope).toBeUndefined();
    expect(res.alerts[2].alertName).toBe('CheckoutScrapeMissing');

    // HighErrorRate rule deduplicated; BrokenQueryRule (health=error) & ExternalAMRule kept
    expect(res.rules.map((r) => r.ruleName)).toEqual(['BrokenQueryRule', 'ExternalAMRule']);
    expect(res.statusNotes).toEqual([]);
    expect(res.collectionStatus).toEqual({
      alertsChecked: true,
      rulesChecked: true,
      totalRulesEvaluated: 3,
      activeAlertsTotal: 4,
      abnormalRulesTotal: 3,
    });
  });

  test('emits synthetic ev-status-1 note when alerting endpoints succeed with zero firing alerts/rules', async () => {
    mockGet.mockImplementation(async (url: string) => {
      if (url.includes('/v2/alerts')) {
        return [];
      }
      return {
        data: {
          groups: [
            {
              name: 'healthy-group',
              rules: [{ name: 'AllGood', type: 'alerting', state: 'inactive', health: 'ok' }],
            },
          ],
        },
      };
    });

    const res = await collectGrafanaAlertingEvidence();
    expect(res.alerts).toEqual([]);
    expect(res.rules).toEqual([]);
    expect(res.statusNotes).toHaveLength(1);
    expect(res.statusNotes[0].id).toBe('ev-status-1');
    expect(res.statusNotes[0].message).toContain('0 active alerts');
    expect(res.statusNotes[0].message).toContain('1 evaluated alerting rules');
  });

  test('degrades gracefully on HTTP 403/404 without throwing', async () => {
    mockGet.mockImplementation(async (url: string) => {
      if (url.includes('/v2/alerts')) {
        throw { status: 403, message: 'Forbidden' };
      }
      throw { status: 404, message: 'Not Found' };
    });

    const res = await collectGrafanaAlertingEvidence();
    expect(res.alerts).toEqual([]);
    expect(res.rules).toEqual([]);
    expect(res.statusNotes).toEqual([]);
    expect(res.warnings.map((w) => w.code)).toEqual(['alerting_forbidden', 'alerting_unavailable']);
    expect(res.collectionStatus.alertsChecked).toBe(false);
    expect(res.collectionStatus.rulesChecked).toBe(false);
  });
});
