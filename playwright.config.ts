import { dirname } from 'node:path';
import type { PluginOptions } from '@grafana/plugin-e2e';
import { defineConfig, devices } from '@playwright/test';

const pluginE2eAuth = `${dirname(require.resolve('@grafana/plugin-e2e'))}/auth`;

const adminUser = {
  user: process.env.GRAFANA_ADMIN_USER || process.env.GF_SECURITY_ADMIN_USER || 'admin',
  password:
    process.env.GRAFANA_ADMIN_PASSWORD || process.env.GF_SECURITY_ADMIN_PASSWORD || 'change-me-local-dev',
};

export default defineConfig<PluginOptions>({
  testDir: './tests/e2e',
  fullyParallel: false,
  workers: 1,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  timeout: 90_000,
  expect: {
    timeout: 20_000,
  },
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: process.env.GRAFANA_URL || 'http://127.0.0.1:3000',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    acceptDownloads: true,
    user: adminUser,
    grafanaAPICredentials: adminUser,
  },
  projects: [
    {
      name: 'auth',
      testDir: pluginE2eAuth,
      testMatch: [/.*\.js/],
    },
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        storageState: `playwright/.auth/${adminUser.user}.json`,
      },
      dependencies: ['auth'],
    },
  ],
});
