import { Observable, of, throwError } from 'rxjs';
import { collectGrafanaAlertingEvidence } from './alertsCollector';

const mockFetch = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    fetch: (...args: unknown[]) => mockFetch(...args),
  }),
}));

describe('report/alertsCollector', () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  test('collects v2 alerts and v1 rules via BackendSrv.fetch with abortSignal, handles DatasourceNoData, and deduplicates kept active alerts', async () => {
    mockFetch.mockImplementation((req: { url: string; abortSignal?: AbortSignal }) => {
      expect(req.abortSignal).toBeDefined();
      if (req.url.startsWith('/api/alertmanager/grafana/api/v2/alerts')) {
        return of({
          data: [
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
              startsAt: 'Fri, 27 Nov 2026 00:02:00 GMT', // non-RFC3339 -> must normalize to ISO RFC3339
              status: { state: 'active' },
            },
            {
              labels: {
                alertname: 'SilencedRuleAlert',
                severity: 'warning',
                service: 'checkout-api',
                __alert_rule_uid__: 'uid-silenced',
              },
              status: { state: 'suppressed' },
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
          ],
        });
      }
      if (req.url.startsWith('/api/prometheus/grafana/api/v1/rules')) {
        return of({
          data: {
            data: {
              groups: [
                {
                  name: 'checkout-rules',
                  rules: [
                    { name: 'job:http_requests:rate5m', type: 'recording', state: 'inactive', health: 'ok' },
                    // Covered via instance alert __alert_rule_uid__ on kept active alert -> deduplicated
                    {
                      name: 'HighErrorRate',
                      type: 'alerting',
                      state: 'firing',
                      health: 'ok',
                      alerts: [{ state: 'firing', labels: { __alert_rule_uid__: 'uid-err-1', service: 'checkout-api' } }],
                    },
                    // Silenced alert in Alertmanager must NOT suppress firing rule in /v1/rules!
                    {
                      name: 'SilencedRuleAlert',
                      type: 'alerting',
                      state: 'firing',
                      health: 'ok',
                      labels: { __alert_rule_uid__: 'uid-silenced', service: 'checkout-api' },
                      alerts: [{ state: 'firing', labels: { service: 'checkout-api' } }],
                    },
                    // External Alertmanager firing rule -> kept
                    {
                      name: 'ExternalAMRule',
                      type: 'alerting',
                      state: 'firing',
                      health: 'ok',
                      labels: { service: 'checkout-api' },
                      alerts: [{ state: 'firing', labels: { service: 'checkout-api' } }],
                    },
                    // Evaluation error rule -> kept
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
          },
        });
      }
      throw new Error(`unexpected URL ${req.url}`);
    });

    const res = await collectGrafanaAlertingEvidence({
      scope: { labelName: 'service', labelValue: 'checkout-api' },
      includeOrgCriticalAlerts: true,
    });

    expect(res.alerts.map((a) => a.alertName)).toEqual([
      'HighErrorRate',
      'BillingCriticalOutage',
      'CheckoutScrapeMissing',
      'SilencedRuleAlert',
    ]);
    expect(res.alerts[2].startsAt).toBe('2026-11-27T00:02:00.000Z');
    expect(res.rules.map((r) => r.ruleName)).toEqual(['BrokenQueryRule', 'SilencedRuleAlert', 'ExternalAMRule']);
  });

  test('unsubscribes in-flight BackendSrv.fetch Observable immediately when external AbortSignal aborts', async () => {
    const unsubSpy = jest.fn();
    mockFetch.mockImplementation(
      () =>
        new Observable(() => {
          return () => {
            unsubSpy();
          };
        })
    );

    const ctrl = new AbortController();
    const promise = collectGrafanaAlertingEvidence({ signal: ctrl.signal });
    ctrl.abort();

    await expect(promise).rejects.toThrow();
    expect(unsubSpy).toHaveBeenCalledTimes(2);
  });

  test('emits synthetic ev-status-1 note when alerting endpoints succeed with zero firing alerts/rules', async () => {
    mockFetch.mockImplementation((req: { url: string }) => {
      if (req.url.includes('/v2/alerts')) {
        return of({ data: [] });
      }
      return of({
        data: {
          data: {
            groups: [
              {
                name: 'healthy-group',
                rules: [{ name: 'AllGood', type: 'alerting', state: 'inactive', health: 'ok' }],
              },
            ],
          },
        },
      });
    });

    const res = await collectGrafanaAlertingEvidence();
    expect(res.alerts).toEqual([]);
    expect(res.rules).toEqual([]);
    expect(res.statusNotes).toHaveLength(1);
    expect(res.statusNotes[0].id).toBe('ev-status-1');
  });

  test('degrades gracefully on HTTP 403/404 without throwing', async () => {
    mockFetch.mockImplementation((req: { url: string }) => {
      if (req.url.includes('/v2/alerts')) {
        return throwError(() => ({ status: 403, message: 'Forbidden' }));
      }
      return throwError(() => ({ status: 404, message: 'Not Found' }));
    });

    const res = await collectGrafanaAlertingEvidence();
    expect(res.warnings.map((w) => w.code)).toEqual(['alerting_forbidden', 'alerting_unavailable']);
    expect(res.collectionStatus.alertsChecked).toBe(false);
    expect(res.collectionStatus.rulesChecked).toBe(false);
  });
});
