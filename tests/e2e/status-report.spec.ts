import { expect, test } from '@grafana/plugin-e2e';
import type { Page } from '@playwright/test';

const PLUGIN_ID = 'yuuki-llmstudio-app';

function attachRuntimeErrorMonitor(page: Page) {
  const pageErrors: string[] = [];
  const consoleErrors: string[] = [];

  page.on('pageerror', (err) => {
    pageErrors.push(err.message || String(err));
  });

  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      consoleErrors.push(msg.text());
    }
  });

  return {
    assertClean(contextLabel: string) {
      expect(pageErrors, `[${contextLabel}] uncaught page errors`).toEqual([]);
      const criticalConsoleErrors = consoleErrors.filter((text) =>
        /SECRET_INTERNALS|__CLIENT_INTERNALS|Minified React error|Invalid hook call|React\.|react-dom|react-router|yuuki-llmstudio-app|SystemJS|ChunkLoadError|TypeError|ReferenceError|SyntaxError|Uncaught|Cannot read/i.test(
          text
        )
      );
      expect(
        criticalConsoleErrors,
        `[${contextLabel}] critical React/plugin console errors (all console errors: ${JSON.stringify(consoleErrors)})`
      ).toEqual([]);
    },
  };
}

test.describe('Operational Status Report live E2E', () => {
  test('navigates to /reports, previews redacted alerts, generates grounded report, jumps to evidence, and hands off to Studio', async ({
    page,
  }) => {
    const errors = attachRuntimeErrorMonitor(page);

    // Stub Grafana Unified Alerting APIs deterministically while keeping plugin backend & Prometheus live.
    await page.route('**/api/alertmanager/grafana/api/v2/alerts*', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify([
          {
            labels: {
              alertname: 'CheckoutHigh5xxRate',
              severity: 'critical',
              service: 'checkout-api',
              __alert_rule_uid__: 'e2e-rule-1',
            },
            annotations: {
              summary: '5xx error ratio exceeded 5% (token=glsa_abcdefghijklmnopqrstuvwxyz1234)',
              description: 'Runbook https://internal.example.local/ops?api_key=secret123456',
            },
            startsAt: new Date(Date.now() - 10 * 60 * 1000).toISOString(),
            status: { state: 'active' },
          },
        ]),
      });
    });

    await page.route('**/api/prometheus/grafana/api/v1/rules*', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          status: 'success',
          data: {
            groups: [
              {
                name: 'checkout-alerts',
                rules: [
                  {
                    name: 'CheckoutHigh5xxRate',
                    type: 'alerting',
                    state: 'firing',
                    health: 'ok',
                    labels: { __alert_rule_uid__: 'e2e-rule-1', service: 'checkout-api' },
                    alerts: [{ state: 'firing', labels: { service: 'checkout-api' } }],
                  },
                ],
              },
            ],
          },
        }),
      });
    });

    // 1. Navigate to the dedicated Operational Status Report page
    await page.goto(`/a/${PLUGIN_ID}/reports`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('report-analyze-button')).toBeVisible();

    // 2. Enter scope & enable Preview-before-send gate
    const scopeInput = page.locator('#report-scope-value-input');
    await scopeInput.fill('checkout-api');

    await page.getByText('Preview collected evidence before sending to LLM').click();
    await page.getByTestId('report-analyze-button').click();

    // 3. Verify preview gate holds before LLM call and secret was redacted client-side
    await expect(page.getByTestId('report-confirm-send-button')).toBeVisible();
    const evidencePane = page.getByTestId('report-evidence-pane');
    await expect(evidencePane).toContainText('CheckoutHigh5xxRate');
    await expect(evidencePane).not.toContainText('glsa_abcdefghijklmnopqrstuvwxyz1234');

    // 4. Confirm send to backend (/jobs/analyze in mock mode)
    await page.getByTestId('report-confirm-send-button').click();
    await expect(page.getByTestId('report-executive-summary')).toBeVisible();
    await expect(page.getByTestId('cite-ev-alert-1')).toBeVisible();

    // 5. Click citation badge to highlight evidence
    await page.getByTestId('cite-ev-alert-1').click();

    // 6. Handoff to Studio via "Investigate in Studio"
    const handoffBtn = page.getByTestId('report-investigate-studio');
    await expect(handoffBtn).toBeVisible();
    await handoffBtn.click();

    await expect(page.getByTestId('studio-page')).toBeVisible();
    await expect(page.getByTestId('studio-handoff-alert')).toBeVisible();
    await expect(page.getByPlaceholder('checkout-api')).toHaveValue('checkout-api');

    errors.assertClean('StatusReportE2E');
  });
});
