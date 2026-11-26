import fs from 'node:fs';
import { expect, test } from '@grafana/plugin-e2e';
import type { Page, Request } from '@playwright/test';

const PLUGIN_ID = 'yuuki-llmstudio-app';

interface DsQueryRecord {
  url: string;
  queries: Array<{
    refId?: string;
    expr?: string;
    maxDataPoints?: number;
  }>;
}

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

      // Fail on any React / React 19 / module loading / plugin runtime console errors.
      const criticalConsoleErrors = consoleErrors.filter((text) =>
        /SECRET_INTERNALS|__CLIENT_INTERNALS|Minified React error|Invalid hook call|React\.|react-dom|yuuki-llmstudio-app|SystemJS|ChunkLoadError|TypeError|ReferenceError|SyntaxError|Uncaught|Cannot read/i.test(
          text
        )
      );
      expect(
        criticalConsoleErrors,
        `[${contextLabel}] critical React/plugin console errors (all console errors: ${JSON.stringify(consoleErrors)})`
      ).toEqual([]);
    },
    getConsoleErrors() {
      return [...consoleErrors];
    },
  };
}

function attachDsQueryTracker(page: Page) {
  const records: DsQueryRecord[] = [];

  const handler = (req: Request) => {
    if (req.method() !== 'POST' || !req.url().includes('/api/ds/query')) {
      return;
    }
    try {
      const payload = req.postDataJSON() as {
        queries?: Array<{ refId?: string; expr?: string; maxDataPoints?: number }>;
      };
      records.push({
        url: req.url(),
        queries: Array.isArray(payload?.queries) ? payload.queries : [],
      });
    } catch {
      records.push({ url: req.url(), queries: [] });
    }
  };

  page.on('request', handler);

  return {
    records,
    count() {
      return records.length;
    },
    dryRunRecordsSince(startIndex: number) {
      return records
        .slice(startIndex)
        .filter((r) => r.queries.some((q) => q.maxDataPoints === 10));
    },
    dispose() {
      page.off('request', handler);
    },
  };
}

test.describe('LLM Dashboard Studio live E2E', () => {
  test('App Config page renders cleanly without React or runtime errors', async ({
    gotoAppConfigPage,
    page,
  }) => {
    const errors = attachRuntimeErrorMonitor(page);

    await gotoAppConfigPage({ pluginId: PLUGIN_ID });
    await expect(page.getByText('Permissions & Security Policy')).toBeVisible();
    await expect(page.getByText('Datasource Allowlist')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Save settings' })).toBeVisible();

    errors.assertClean('AppConfig');
  });

  test('full Studio workflow: scoped catalog, draft, dry-run & Scenes, tab switch, patch, undo, create-only save, JSON export', async ({
    page,
    request,
    grafanaVersion,
  }) => {
    const errors = attachRuntimeErrorMonitor(page);
    const dsQueries = attachDsQueryTracker(page);
    let savedUid: string | null = null;

    try {
      // 1. Open Studio root page
      await page.goto(`/a/${PLUGIN_ID}`, { waitUntil: 'domcontentloaded' });
      await expect(page.getByTestId('studio-page')).toBeVisible();
      await expect(page.getByTestId('studio-mock-banner')).toBeVisible();
      await expect(page.getByText('LLM Dashboard Studio (MVP)')).toBeVisible();
      await expect(page.getByText(/Prometheus Main \(prom-main\)/)).toBeVisible();

      // 2. Scoped metric catalog loading from live Prometheus
      await page.getByRole('button', { name: 'Fetch Values' }).click();
      const scopeInput = page.getByPlaceholder('checkout-api');
      await expect(scopeInput).toHaveValue('checkout-api');

      await page.getByRole('button', { name: 'Fetch Scoped Metrics' }).click();
      await expect
        .poll(async () => Number((await page.getByTestId('studio-catalog-count').textContent()) || '0'))
        .toBeGreaterThanOrEqual(3);
      await expect(page.getByTestId('studio-error-alert')).toHaveCount(0);

      // 3. Draft generation (mock mode) — Scenes MUST NOT mount or query yet
      const queriesBeforeGenerate = dsQueries.count();
      const generateBtn = page.getByRole('button', { name: 'Generate Dashboard Draft' });
      await expect(generateBtn).toBeEnabled();
      await generateBtn.click();

      await expect(page.getByTestId('studio-job-status')).toBeVisible();
      await expect(page.getByTestId('studio-job-status')).toContainText('awaiting_preview');
      await expect(page.getByTestId('studio-job-revision')).toHaveText('rev 1');
      await expect(page.getByRole('button', { name: 'Save New Dashboard' })).toBeDisabled();
      await expect(page.getByTestId('studio-preview-pane')).toContainText(
        'IR checks passed for draft rev 1.'
      );
      expect(dsQueries.count()).toBe(queriesBeforeGenerate);

      // 4. Explicit Dry-Run validation & @grafana/scenes rendering
      const queriesBeforeValidate = dsQueries.count();
      await page.getByRole('button', { name: 'Validate & Preview' }).click();

      await expect(page.getByTestId('studio-validation-results')).toBeVisible();
      await expect(page.getByTestId('studio-validation-row-panel-request-rate')).toContainText('OK');
      await expect(page.getByTestId('studio-validation-row-panel-error-rate')).toContainText('OK');
      await expect(page.getByTestId('studio-validation-row-panel-latency-p95')).toContainText('OK');
      await expect(page.getByTestId('studio-error-alert')).toHaveCount(0);

      const initialDryRuns = dsQueries.dryRunRecordsSince(queriesBeforeValidate);
      expect(initialDryRuns.length).toBe(3);

      const previewPane = page.getByTestId('studio-preview-pane');
      await expect(previewPane.getByText('Request Rate')).toBeVisible();
      await expect(previewPane.getByText('5xx Error Ratio')).toBeVisible();
      await expect(previewPane.getByText('p95 Request Latency')).toBeVisible();

      // Wait for initial Scene preview queries to settle
      await expect.poll(() => dsQueries.count()).toBeGreaterThanOrEqual(queriesBeforeValidate + 6);
      await expect(page.getByRole('button', { name: 'Save New Dashboard' })).toBeEnabled();

      // 5. Tab switching MUST NOT unmount EmbeddedScene or trigger unexpected re-queries (H-8)
      await page.waitForTimeout(500);
      const queriesBeforeTabs = dsQueries.count();

      await page.getByRole('button', { name: 'IR JSON' }).click();
      const irJsonText = await page.getByTestId('studio-ir-json').textContent();
      const parsedIr = JSON.parse(irJsonText || '{}');
      expect(parsedIr.revision).toBe(1);
      expect(parsedIr.panels).toHaveLength(3);

      await page.getByRole('button', { name: 'Foundation SDK JSON' }).click();
      const foundationJsonText = await page.getByTestId('studio-foundation-json').textContent();
      const parsedFoundation = JSON.parse(foundationJsonText || '{}');
      expect(parsedFoundation.schemaVersion).toBe(42);
      expect(parsedFoundation.panels).toHaveLength(3);

      await page.getByRole('button', { name: 'Scenes Preview' }).click();
      await expect(previewPane.getByText('Request Rate')).toBeVisible();
      await page.waitForTimeout(1000);
      expect(dsQueries.count(), 'tab switching must not re-issue datasource queries').toBe(
        queriesBeforeTabs
      );

      const studio = page.getByTestId('studio-page');
      const undoBtn = studio.getByRole('button', { name: 'Undo', exact: true });

      // 6. Partial panel edit (patch) — updates only panel-error-rate and re-validates only changed panel
      const promptArea = studio.getByPlaceholder(/Change only the error rate panel/);
      await promptArea.fill('Change the error rate panel to a timeseries covering both 4xx and 5xx');

      await studio.getByRole('button', { name: 'Apply Partial Edit', exact: true }).click();
      await expect(page.getByTestId('studio-job-revision')).toHaveText('rev 2');
      await expect(page.getByTestId('studio-job-diff')).toContainText('panel-error-rate');
      await expect(undoBtn).toBeVisible();
      await expect(studio.getByRole('button', { name: 'Save New Dashboard', exact: true })).toBeDisabled();

      const queriesBeforePatchValidate = dsQueries.count();
      await studio.getByRole('button', { name: 'Validate & Preview', exact: true }).click();
      await expect(previewPane.getByText('4xx & 5xx Error Ratio')).toBeVisible();
      await expect(page.getByTestId('studio-validation-row-panel-error-rate')).toContainText('OK');

      const patchDryRuns = dsQueries.dryRunRecordsSince(queriesBeforePatchValidate);
      expect(
        patchDryRuns.length,
        'partial patch dry-run should only re-query the single changed panel'
      ).toBe(1);
      expect(patchDryRuns[0].queries[0]?.expr).toContain('4..|5..');

      // 7. Undo — restores rev 1 panels as monotonic rev 3
      await undoBtn.click();
      await expect(page.getByTestId('studio-job-revision')).toHaveText('rev 3');
      await expect(page.getByTestId('studio-job-diff')).toContainText('restored from rev 1 as rev 3');
      await expect(studio.getByRole('button', { name: 'Save New Dashboard', exact: true })).toBeDisabled();

      // Re-validate rev 3 so it is save-ready
      await studio.getByRole('button', { name: 'Validate & Preview', exact: true }).click();
      await expect(previewPane.getByText('5xx Error Ratio')).toBeVisible();
      await expect(studio.getByRole('button', { name: 'Save New Dashboard', exact: true })).toBeEnabled();

      // 8. JSON Export — downloads clean Foundation SDK Dashboard JSON without UID
      const downloadPromise = page.waitForEvent('download');
      await studio.getByRole('button', { name: 'Export JSON', exact: true }).click();
      const download = await downloadPromise;
      expect(download.suggestedFilename()).toBe('dashboard-rev-3.json');

      const downloadPath = await download.path();
      expect(downloadPath).toBeTruthy();
      const exportedJson = JSON.parse(fs.readFileSync(downloadPath!, 'utf8'));
      expect(exportedJson.schemaVersion).toBe(42);
      expect(exportedJson.uid).toBeUndefined();
      expect(exportedJson.panels).toHaveLength(3);
      expect(exportedJson.panels.map((p: { title: string }) => p.title)).toEqual([
        'Request Rate',
        '5xx Error Ratio',
        'p95 Request Latency',
      ]);

      // 9. Create-only dashboard save via POST /apis/dashboard.grafana.app/v1beta1/...
      let sawPutToDashboardApi = false;
      const putWatch = (req: Request) => {
        if (req.method() === 'PUT' && req.url().includes('/apis/dashboard.grafana.app/')) {
          sawPutToDashboardApi = true;
        }
      };
      page.on('request', putWatch);

      const saveResponsePromise = page.waitForResponse(
        (res) =>
          res.request().method() === 'POST' &&
          res.url().includes('/apis/dashboard.grafana.app/v1beta1/namespaces/') &&
          res.url().endsWith('/dashboards')
      );

      await studio.getByRole('button', { name: 'Save New Dashboard', exact: true }).click();
      const saveResponse = await saveResponsePromise;
      page.off('request', putWatch);

      expect(sawPutToDashboardApi, 'save must be create-only POST, never PUT').toBe(false);
      expect([200, 201]).toContain(saveResponse.status());

      await expect(page.getByTestId('studio-saved-alert')).toBeVisible();
      savedUid = (await page.getByTestId('studio-saved-uid').textContent())?.trim() || null;
      expect(savedUid).toMatch(/^llmstudio-[a-f0-9]+$/);

      const savedLink = page
        .getByTestId('studio-saved-alert')
        .getByRole('link', { name: /Open saved dashboard/ });
      await expect(savedLink).toHaveAttribute('href', `/d/${savedUid}`);

      // Verify persisted dashboard via Grafana's Kubernetes-style Dashboard API
      const getResp = await request.get(
        `/apis/dashboard.grafana.app/v1beta1/namespaces/default/dashboards/${savedUid}`
      );
      expect(getResp.ok()).toBe(true);
      const savedDash = await getResp.json();
      expect(savedDash.metadata?.name).toBe(savedUid);
      expect(savedDash.spec?.panels).toHaveLength(3);

      // Verify zero uncaught page errors or React/plugin runtime errors on this Grafana version
      errors.assertClean(`Studio E2E on Grafana ${grafanaVersion}`);
    } finally {
      dsQueries.dispose();
      if (savedUid) {
        await request
          .delete(`/apis/dashboard.grafana.app/v1beta1/namespaces/default/dashboards/${savedUid}`)
          .catch(() => {});
      }
    }
  });

  test('optional SSE stage-progress streaming updates UI incrementally without prematurely mounting Scenes', async ({
    page,
    request,
    grafanaVersion,
  }) => {
    const errors = attachRuntimeErrorMonitor(page);
    const dsQueries = attachDsQueryTracker(page);

    const origSettingsRes = await request.get(`/api/plugins/${PLUGIN_ID}/settings`);
    expect(origSettingsRes.ok()).toBe(true);
    const origSettings = await origSettingsRes.json();

    try {
      // Enable optional SSE stage progress with a short dev mock delay so the browser observes incremental stages.
      const enableRes = await request.post(`/api/plugins/${PLUGIN_ID}/settings`, {
        data: {
          enabled: origSettings.enabled ?? true,
          pinned: origSettings.pinned ?? true,
          jsonData: {
            ...(origSettings.jsonData ?? {}),
            enableSseProgress: true,
            mockDelayMs: 1200,
          },
        },
      });
      expect(enableRes.ok()).toBe(true);

      await expect
        .poll(async () => {
          const cfgRes = await request.get(`/api/plugins/${PLUGIN_ID}/resources/config/public`);
          if (!cfgRes.ok()) {
            return false;
          }
          const cfg = await cfgRes.json();
          return cfg.enableSseProgress === true;
        })
        .toBe(true);

      await page.goto(`/a/${PLUGIN_ID}`, { waitUntil: 'domcontentloaded' });
      await expect(page.getByTestId('studio-page')).toBeVisible();

      await page.getByRole('button', { name: 'Fetch Values' }).click();
      await expect(page.getByPlaceholder('checkout-api')).toHaveValue('checkout-api');
      await page.getByRole('button', { name: 'Fetch Scoped Metrics' }).click();
      await expect
        .poll(async () => Number((await page.getByTestId('studio-catalog-count').textContent()) || '0'))
        .toBeGreaterThanOrEqual(3);

      const queriesBeforeGenerate = dsQueries.count();
      await page.getByRole('button', { name: 'Generate Dashboard Draft' }).click();

      // Live SSE stage progress indicator MUST appear while mock LLM is sleeping (before completion).
      await expect(page.getByTestId('studio-sse-progress')).toBeVisible();
      await expect(page.getByTestId('studio-sse-progress')).toContainText('generating');
      expect(dsQueries.count(), 'SSE progress must never trigger premature datasource queries').toBe(
        queriesBeforeGenerate
      );

      // Once generation completes, progress indicator clears and validated rev 1 draft is shown.
      await expect(page.getByTestId('studio-job-revision')).toHaveText('rev 1', { timeout: 10000 });
      await expect(page.getByTestId('studio-job-status')).toContainText('awaiting_preview');
      await expect(page.getByTestId('studio-sse-progress')).toHaveCount(0);
      expect(dsQueries.count()).toBe(queriesBeforeGenerate);

      errors.assertClean(`Studio SSE E2E on Grafana ${grafanaVersion}`);
    } finally {
      dsQueries.dispose();
      const restoredSse = Boolean(origSettings.jsonData?.enableSseProgress);
      await request
        .post(`/api/plugins/${PLUGIN_ID}/settings`, {
          data: {
            enabled: origSettings.enabled ?? true,
            pinned: origSettings.pinned ?? true,
            jsonData: {
              ...(origSettings.jsonData ?? {}),
              enableSseProgress: restoredSse,
              mockDelayMs: 0,
            },
          },
        })
        .catch(() => {});
      await expect
        .poll(async () => {
          const cfgRes = await request.get(`/api/plugins/${PLUGIN_ID}/resources/config/public`);
          if (!cfgRes.ok()) {
            return false;
          }
          const cfg = await cfgRes.json();
          return Boolean(cfg.enableSseProgress) === restoredSse;
        })
        .toBe(true)
        .catch(() => {});
    }
  });
});
