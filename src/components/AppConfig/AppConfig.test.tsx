import React from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { of } from 'rxjs';
import { AppPluginMeta } from '@grafana/data';
import { DEFAULT_LOCALE, setStudioLocale, SUPPORTED_LOCALES, t } from '../../i18n';
import { AppConfig, StudioJsonData } from './AppConfig';

const mockFetch = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    fetch: mockFetch,
  }),
}));

jest.mock('@grafana/ui', () => ({
  Alert: ({ title, children }: { title: string; children?: React.ReactNode }) => (
    <div>
      <strong>{title}</strong>
      {children}
    </div>
  ),
  Button: ({
    children,
    onClick,
    disabled,
  }: {
    children?: React.ReactNode;
    onClick?: () => void;
    disabled?: boolean;
  }) => (
    <button type="button" onClick={onClick} disabled={disabled}>
      {children}
    </button>
  ),
  Checkbox: ({
    value,
    onChange,
    label,
  }: {
    value?: boolean;
    onChange?: React.ChangeEventHandler<HTMLInputElement>;
    label?: string;
  }) => (
    <label>
      <input type="checkbox" checked={Boolean(value)} onChange={onChange} />
      {label}
    </label>
  ),
  Field: ({ label, children }: { label: string; children?: React.ReactNode }) => (
    <label>
      <span>{label}</span>
      {children}
    </label>
  ),
  FieldSet: ({ label, children }: { label: string; children?: React.ReactNode }) => (
    <fieldset>
      <legend>{label}</legend>
      {children}
    </fieldset>
  ),
  Input: (props: React.InputHTMLAttributes<HTMLInputElement>) => <input {...props} />,
  SecretInput: ({
    isConfigured,
    value,
    onChange,
    onReset,
    placeholder,
  }: {
    isConfigured?: boolean;
    value?: string;
    onChange?: React.ChangeEventHandler<HTMLInputElement>;
    onReset?: () => void;
    placeholder?: string;
  }) => (
    <div>
      {isConfigured ? (
        <button type="button" onClick={onReset}>
          Reset Secret
        </button>
      ) : (
        <input value={value} onChange={onChange} placeholder={placeholder} />
      )}
    </div>
  ),
  Select: ({
    options,
    value,
    onChange,
    'aria-label': ariaLabel,
  }: {
    options: Array<{ label: string; value: string }>;
    value?: string;
    onChange: (v: { value: string }) => void;
    'aria-label'?: string;
  }) => (
    <select aria-label={ariaLabel} value={value} onChange={(e) => onChange({ value: e.target.value })}>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  ),
}));

function makePlugin(
  jsonData: StudioJsonData & Record<string, unknown>,
  secureJsonFields: Record<string, boolean> = {}
) {
  return {
    meta: {
      id: 'yuuki-llmstudio-app',
      enabled: true,
      pinned: true,
      jsonData,
      secureJsonFields,
    } as unknown as AppPluginMeta<StudioJsonData>,
  };
}

describe.each(SUPPORTED_LOCALES)(
  'AppConfig settings preservation, secret reset, and validation (locale: %s)',
  (locale) => {
    beforeEach(async () => {
      await setStudioLocale(locale);
      mockFetch.mockReset();
      mockFetch.mockReturnValue(of({ data: {} }));
    });

    afterAll(async () => {
      await setStudioLocale(DEFAULT_LOCALE);
    });

    test('preserves existing jsonData fields and sends empty secureJsonData string on SecretInput reset (H-11)', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobDeadlineSeconds: 120,
          maxConcurrentJobs: 6,
          customProvisionedKey: 'keep-me',
        },
        { llmServiceAccountToken: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // Reset the configured secret token without entering a replacement.
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));

      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      const req = mockFetch.mock.calls[0][0];
      expect(req.url).toBe('/api/plugins/yuuki-llmstudio-app/settings');
      expect(req.data.jsonData.jobDeadlineSeconds).toBe(120);
      expect(req.data.jsonData.maxConcurrentJobs).toBe(6);
      expect(req.data.jsonData.customProvisionedKey).toBe('keep-me');
      expect(req.data.secureJsonData).toEqual({ llmServiceAccountToken: '' });
    });

    test('rejects saving llmMode=mock without allowMockInDev and normalizes allowMockInDev=false outside mock (M-13)', async () => {
      const plugin = makePlugin({
        allowedDatasourceUids: ['prom-main'],
        llmMode: 'mock',
        allowMockInDev: false,
      });

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // Attempting to save mock without allowMockInDev MUST fail client-side before fetch.
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.allowMockRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Enable allowMockInDev, then switch back to grafana-llm-app -> saved payload MUST normalize allowMockInDev: false.
      // Selects: [0]=dialect(prom-main), [1]=llmMode, [2]=modelTier, [3]=structuredOutputMode, [4]=tokenLimitField, [5]=reasoningEffort
      fireEvent.click(screen.getByLabelText(t('appConfig.llm.allowMockCheckbox')));
      const modeSelect = screen.getAllByRole('combobox')[1];
      fireEvent.change(modeSelect, { target: { value: 'grafana-llm-app' } });

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      expect(mockFetch.mock.calls[0][0].data.jsonData.llmMode).toBe('grafana-llm-app');
      expect(mockFetch.mock.calls[0][0].data.jsonData.allowMockInDev).toBe(false);
      expect(mockFetch.mock.calls[0][0].data.jsonData.structuredOutputMode).toBe('json_schema');
      expect(mockFetch.mock.calls[0][0].data.jsonData.tokenLimitField).toBe('max_completion_tokens');
      expect(mockFetch.mock.calls[0][0].data.jsonData.reasoningEffort).toBe('');
    });

    test('allows configuring json_object, max_tokens, and reasoningEffort for third-party OpenAI-compatible APIs', async () => {
      const plugin = makePlugin({
        allowedDatasourceUids: ['prom-main'],
        llmMode: 'grafana-llm-app',
      });

      render(<AppConfig plugin={plugin as never} query={{}} />);
      const selects = screen.getAllByRole('combobox');
      // Selects: [0]=dialect(prom-main), [1]=llmMode, [2]=modelTier, [3]=structuredOutputMode, [4]=tokenLimitField, [5]=reasoningEffort
      fireEvent.change(selects[3], { target: { value: 'json_object' } });
      fireEvent.change(selects[4], { target: { value: 'max_tokens' } });
      fireEvent.change(selects[5], { target: { value: 'low' } });

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      expect(mockFetch.mock.calls[0][0].data.jsonData.structuredOutputMode).toBe('json_object');
      expect(mockFetch.mock.calls[0][0].data.jsonData.tokenLimitField).toBe('max_tokens');
      expect(mockFetch.mock.calls[0][0].data.jsonData.reasoningEffort).toBe('low');
    });

    test('pins datasourceDialects per allowed UID and explicitly sends empty object {} when cleared back to Auto (H-11)', async () => {
      const plugin = makePlugin({
        allowedDatasourceUids: ['prom-main', 'vm-prom'],
        datasourceDialects: { 'vm-prom': 'metricsql' },
        llmMode: 'grafana-llm-app',
      });

      render(<AppConfig plugin={plugin as never} query={{}} />);

      const promDialectSelect = screen.getByLabelText('Query dialect for prom-main');
      const vmDialectSelect = screen.getByLabelText('Query dialect for vm-prom');
      expect((promDialectSelect as HTMLSelectElement).value).toBe('');
      expect((vmDialectSelect as HTMLSelectElement).value).toBe('metricsql');

      // Pin prom-main -> metricsql and save.
      fireEvent.change(promDialectSelect, { target: { value: 'metricsql' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      expect(mockFetch.mock.calls[0][0].data.jsonData.datasourceDialects).toEqual({
        'prom-main': 'metricsql',
        'vm-prom': 'metricsql',
      });

      // Reset both back to Auto ("") -> saved payload MUST contain explicit {} rather than omitting the key.
      fireEvent.change(promDialectSelect, { target: { value: '' } });
      fireEvent.change(vmDialectSelect, { target: { value: '' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
      expect(mockFetch.mock.calls[1][0].data.jsonData.datasourceDialects).toEqual({});
    });

    test('validates redisAddr, saves HA Redis job store settings, and supports resetting redisPassword secret', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobStoreBackend: 'memory',
        },
        { redisPassword: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      const storeSelect = screen.getByLabelText(t('appConfig.jobStorage.backendLabel'));
      fireEvent.change(storeSelect, { target: { value: 'redis' } });

      // Saving redis without redisAddr MUST fail client-side validation before fetch.
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.redisAddrRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Reject addresses that Go net.SplitHostPort would reject (spaces, unbracketed IPv6, empty brackets, +port).
      for (const badAddr of ['redis : 6379', '::1:6379', '[]:6379', 'redis:+6379', 'redis:06379', 'redis/x:6379']) {
        fireEvent.change(screen.getByPlaceholderText('redis:6379'), { target: { value: badAddr } });
        fireEvent.click(screen.getByText(t('appConfig.actions.save')));
        await waitFor(() =>
          expect(screen.getByText(t('appConfig.validation.redisAddrRequired'))).toBeTruthy()
        );
      }
      expect(mockFetch).not.toHaveBeenCalled();

      // Reject >128 char redisUsername.
      fireEvent.change(screen.getByPlaceholderText('redis:6379'), { target: { value: '[::1]:6379' } });
      fireEvent.change(screen.getByPlaceholderText('default'), { target: { value: 'a'.repeat(129) } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.redisUsernameTooLong'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Valid IPv6 + valid username + reset existing redisPassword + enable TLS.
      fireEvent.change(screen.getByPlaceholderText('default'), { target: { value: 'studio' } });
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.click(screen.getByLabelText(t('appConfig.jobStorage.redisTlsCheckbox')));

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      const payload = mockFetch.mock.calls[0][0].data;
      expect(payload.jsonData.jobStoreBackend).toBe('redis');
      expect(payload.jsonData.redisAddr).toBe('[::1]:6379');
      expect(payload.jsonData.redisUsername).toBe('studio');
      expect(payload.jsonData.redisKeyPrefix).toBe('llmstudio:jobs');
      expect(payload.jsonData.redisTls).toBe(true);
      expect(payload.secureJsonData).toEqual({ redisPassword: '' });
    });

    test('switching redis -> memory does not reset hidden redisPassword or persist unvalidated hidden Redis edits', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobStoreBackend: 'redis',
          redisAddr: 'redis.ha:6379',
          redisDb: 2,
        },
        { redisPassword: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // User clicks Reset Secret on Redis password and types invalid redisDb, then switches back to memory.
      fireEvent.click(screen.getByText('Reset Secret'));
      const redisDbInput = screen.getByLabelText(t('appConfig.jobStorage.redisDbLabel'));
      fireEvent.change(redisDbInput, { target: { value: '99' } });

      const storeSelect = screen.getByLabelText(t('appConfig.jobStorage.backendLabel'));
      fireEvent.change(storeSelect, { target: { value: 'memory' } });

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      const payload = mockFetch.mock.calls[0][0].data;
      expect(payload.jsonData.jobStoreBackend).toBe('memory');
      expect(payload.jsonData.redisDb).toBe(2);
      expect(payload.secureJsonData).toBeUndefined();
    });

    test('blocks changing redisAddr or disabling redisTls while redisPassword is configured until reset/re-entered, and binds new secrets to endpoint+TLS', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobStoreBackend: 'redis',
          redisAddr: 'redis.ha:6379',
          redisTls: true,
        },
        { redisPassword: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // Changing redisAddr without resetting/re-entering redisPassword MUST be blocked before fetch.
      const addrInput = screen.getByPlaceholderText('redis:6379');
      fireEvent.change(addrInput, { target: { value: 'attacker.invalid:6379' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.redisPasswordReentryRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Restoring redisAddr but disabling redisTls without resetting redisPassword MUST also be blocked.
      fireEvent.change(addrInput, { target: { value: 'redis.ha:6379' } });
      fireEvent.click(screen.getByLabelText(t('appConfig.jobStorage.redisTlsCheckbox')));
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.redisPasswordReentryRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Re-enable TLS, change addr to new legitimate Redis, reset & re-enter password -> saves destination-bound secret.
      fireEvent.click(screen.getByLabelText(t('appConfig.jobStorage.redisTlsCheckbox')));
      fireEvent.change(addrInput, { target: { value: 'redis2.ha:6379' } });
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.change(screen.getByPlaceholderText('redis-password'), {
        target: { value: 'new-redis-secret' },
      });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

      const savedSecret = mockFetch.mock.calls[0][0].data.secureJsonData.redisPassword as string;
      expect(savedSecret.startsWith('llmstudio-redis-bound-v1:')).toBe(true);
      expect(JSON.parse(savedSecret.slice('llmstudio-redis-bound-v1:'.length))).toEqual({
        addr: 'redis2.ha:6379',
        tls: true,
        password: 'new-redis-secret',
      });
    });

    test('blocks changing grafanaUrl while SA token is configured until reset/re-entered, and binds new SA token to grafanaUrl', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          grafanaUrl: 'https://grafana.internal',
        },
        { llmServiceAccountToken: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      const urlInput = screen.getByPlaceholderText('http://localhost:3000');
      fireEvent.change(urlInput, { target: { value: 'http://127.0.0.1:9999' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.saTokenReentryRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Reset & re-enter token for new URL -> saves destination-bound token envelope.
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.change(screen.getByPlaceholderText('glsa_...'), {
        target: { value: 'glsa_new_bound_token' },
      });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

      const savedToken = mockFetch.mock.calls[0][0].data.secureJsonData
        .llmServiceAccountToken as string;
      expect(savedToken.startsWith('llmstudio-grafana-bound-v1:')).toBe(true);
      expect(JSON.parse(savedToken.slice('llmstudio-grafana-bound-v1:'.length))).toEqual({
        url: 'http://127.0.0.1:9999',
        token: 'glsa_new_bound_token',
      });
    });

    test('validates PostgreSQL fields, saves HA Postgres job store settings, and supports resetting postgresPassword secret', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobStoreBackend: 'memory',
        },
        { postgresPassword: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      const storeSelect = screen.getByLabelText(t('appConfig.jobStorage.backendLabel'));
      fireEvent.change(storeSelect, { target: { value: 'postgres' } });

      // Saving postgres without postgresAddr MUST fail client-side validation before fetch.
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresAddrRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Reject URL/credentials in postgresAddr.
      fireEvent.change(screen.getByPlaceholderText('postgres:5432'), {
        target: { value: 'postgres://user:pass@pg:5432/grafana' },
      });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresAddrRequired'))).toBeTruthy()
      );

      // Valid addr, missing database -> rejected.
      fireEvent.change(screen.getByPlaceholderText('postgres:5432'), {
        target: { value: 'postgres.ha:5432' },
      });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresDatabaseRequired'))).toBeTruthy()
      );

      // Valid database, missing username -> rejected.
      const dbInput = screen.getByLabelText(t('appConfig.jobStorage.postgresDatabaseLabel'));
      fireEvent.change(dbInput, { target: { value: 'grafana' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresUsernameRequired'))).toBeTruthy()
      );

      // Invalid schema -> rejected.
      const userInput = screen.getByLabelText(t('appConfig.jobStorage.postgresUsernameLabel'));
      fireEvent.change(userInput, { target: { value: 'grafana_app' } });
      const schemaInput = screen.getByLabelText(t('appConfig.jobStorage.postgresSchemaLabel'));
      fireEvent.change(schemaInput, { target: { value: 'bad;schema' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresSchemaInvalid'))).toBeTruthy()
      );

      // Invalid table prefix -> rejected.
      fireEvent.change(schemaInput, { target: { value: 'llm_studio' } });
      const prefixInput = screen.getByLabelText(t('appConfig.jobStorage.postgresTablePrefixLabel'));
      fireEvent.change(prefixInput, { target: { value: 'bad-prefix' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresTablePrefixInvalid'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Fix prefix, select verify-full SSL mode, reset existing postgresPassword, and save.
      fireEvent.change(prefixInput, { target: { value: 'studio_' } });
      const sslSelect = screen.getByLabelText(t('appConfig.jobStorage.postgresSslModeLabel'));
      fireEvent.change(sslSelect, { target: { value: 'verify-full' } });
      fireEvent.click(screen.getByText('Reset Secret'));

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      const payload = mockFetch.mock.calls[0][0].data;
      expect(payload.jsonData.jobStoreBackend).toBe('postgres');
      expect(payload.jsonData.postgresAddr).toBe('postgres.ha:5432');
      expect(payload.jsonData.postgresDatabase).toBe('grafana');
      expect(payload.jsonData.postgresSchema).toBe('llm_studio');
      expect(payload.jsonData.postgresTablePrefix).toBe('studio_');
      expect(payload.jsonData.postgresUsername).toBe('grafana_app');
      expect(payload.jsonData.postgresSslMode).toBe('verify-full');
      expect(payload.secureJsonData).toEqual({ postgresPassword: '' });
    });

    test('blocks changing postgresAddr or postgresSslMode while postgresPassword is configured until reset/re-entered, and binds new secrets to endpoint+SSL', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobStoreBackend: 'postgres',
          postgresAddr: 'postgres.ha:5432',
          postgresDatabase: 'grafana',
          postgresUsername: 'grafana',
          postgresSslMode: 'require',
        },
        { postgresPassword: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // Changing postgresAddr without resetting/re-entering postgresPassword MUST be blocked before fetch.
      const addrInput = screen.getByPlaceholderText('postgres:5432');
      fireEvent.change(addrInput, { target: { value: 'attacker.invalid:5432' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresPasswordReentryRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Restoring postgresAddr but downgrading postgresSslMode without resetting postgresPassword MUST also be blocked.
      fireEvent.change(addrInput, { target: { value: 'postgres.ha:5432' } });
      const sslSelect = screen.getByLabelText(t('appConfig.jobStorage.postgresSslModeLabel'));
      fireEvent.change(sslSelect, { target: { value: 'disable' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresPasswordReentryRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Switch to verify-full + new legitimate host, reset & re-enter password -> saves destination-bound secret.
      fireEvent.change(sslSelect, { target: { value: 'verify-full' } });
      fireEvent.change(addrInput, { target: { value: 'postgres2.ha:5432' } });
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.change(screen.getByPlaceholderText('postgres-password'), {
        target: { value: 'new-pg-secret' },
      });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

      const savedSecret = mockFetch.mock.calls[0][0].data.secureJsonData.postgresPassword as string;
      expect(savedSecret.startsWith('llmstudio-postgres-bound-v1:')).toBe(true);
      expect(JSON.parse(savedSecret.slice('llmstudio-postgres-bound-v1:'.length))).toEqual({
        addr: 'postgres2.ha:5432',
        sslMode: 'verify-full',
        password: 'new-pg-secret',
      });
    });

    test('switching postgres -> memory does not reset hidden postgresPassword or persist unvalidated hidden Postgres edits', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobStoreBackend: 'postgres',
          postgresAddr: 'postgres.ha:5432',
          postgresDatabase: 'grafana',
          postgresSchema: 'llm_studio',
          postgresUsername: 'grafana',
          postgresSslMode: 'require',
        },
        { postgresPassword: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // User clicks Reset Secret on Postgres password and types invalid schema, then switches back to memory.
      fireEvent.click(screen.getByText('Reset Secret'));
      const schemaInput = screen.getByLabelText(t('appConfig.jobStorage.postgresSchemaLabel'));
      fireEvent.change(schemaInput, { target: { value: 'invalid;schema' } });

      const storeSelect = screen.getByLabelText(t('appConfig.jobStorage.backendLabel'));
      fireEvent.change(storeSelect, { target: { value: 'memory' } });

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      const payload = mockFetch.mock.calls[0][0].data;
      expect(payload.jsonData.jobStoreBackend).toBe('memory');
      expect(payload.jsonData.postgresSchema).toBe('llm_studio');
      expect(payload.secureJsonData).toBeUndefined();
    });

    test('normalizes uppercase/whitespace provisioned jobStoreBackend and postgresSslMode without silent downgrade', async () => {
      const plugin = makePlugin({
        allowedDatasourceUids: ['prom-main'],
        llmMode: 'grafana-llm-app',
        jobStoreBackend: ' POSTGRES ' as unknown as 'postgres',
        postgresAddr: 'postgres.ha:5432',
        postgresDatabase: 'grafana',
        postgresUsername: 'grafana',
        postgresSslMode: ' VERIFY-FULL ' as unknown as 'verify-full',
      });

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // Save an unrelated setting without touching Job Storage -> MUST NOT downgrade to memory or prefer.
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      const payload = mockFetch.mock.calls[0][0].data;
      expect(payload.jsonData.jobStoreBackend).toBe('postgres');
      expect(payload.jsonData.postgresSslMode).toBe('verify-full');
    });

    test('multi-save without reload preserves newly saved Postgres fields and switching away disarms secret reset', async () => {
      const plugin = makePlugin(
        {
          allowedDatasourceUids: ['prom-main'],
          llmMode: 'grafana-llm-app',
          jobStoreBackend: 'memory',
        },
        { postgresPassword: true }
      );

      render(<AppConfig plugin={plugin as never} query={{}} />);
      const storeSelect = screen.getByLabelText(t('appConfig.jobStorage.backendLabel'));

      // Save #1: configure postgres with a new password (preserving leading/trailing space inside bound envelope).
      fireEvent.change(storeSelect, { target: { value: 'postgres' } });
      fireEvent.change(screen.getByPlaceholderText('postgres:5432'), {
        target: { value: '[::ffff:127.0.0.1]:5432' },
      });
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresDatabaseLabel')), {
        target: { value: 'grafana' },
      });
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresUsernameLabel')), {
        target: { value: 'grafana@flex' },
      });
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresSchemaLabel')), {
        target: { value: '' },
      });
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresTablePrefixLabel')), {
        target: { value: '' },
      });
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresSslModeLabel')), {
        target: { value: 'require' },
      });
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.change(screen.getByPlaceholderText('postgres-password'), {
        target: { value: ' pg-pass-1 ' },
      });

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      expect(mockFetch.mock.calls[0][0].data.jsonData.postgresAddr).toBe('[::ffff:127.0.0.1]:5432');
      expect(mockFetch.mock.calls[0][0].data.jsonData.postgresSchema).toBe('public');
      expect(mockFetch.mock.calls[0][0].data.jsonData.postgresTablePrefix).toBe('llm_studio_');
      const savedPgSecret = mockFetch.mock.calls[0][0].data.secureJsonData.postgresPassword as string;
      expect(savedPgSecret.startsWith('llmstudio-postgres-bound-v1:')).toBe(true);
      expect(JSON.parse(savedPgSecret.slice('llmstudio-postgres-bound-v1:'.length))).toEqual({
        addr: '[::ffff:127.0.0.1]:5432',
        sslMode: 'require',
        password: ' pg-pass-1 ',
      });

      // Click Reset Secret on postgres password, then switch to memory and Save #2 (without reload).
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.change(storeSelect, { target: { value: 'memory' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
      const save2 = mockFetch.mock.calls[1][0].data;
      expect(save2.jsonData.jobStoreBackend).toBe('memory');
      // Newly saved Postgres fields from Save #1 MUST still be preserved (not wiped by stale mount jsonData).
      expect(save2.jsonData.postgresAddr).toBe('[::ffff:127.0.0.1]:5432');
      expect(save2.jsonData.postgresSslMode).toBe('require');
      expect(save2.secureJsonData).toBeUndefined();

      // Switch back to postgres and Save #3 -> armed reset from before switching MUST have been disarmed!
      fireEvent.change(storeSelect, { target: { value: 'postgres' } });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(3));
      expect(mockFetch.mock.calls[2][0].data.secureJsonData).toBeUndefined();
    });

    test('enforces UTF-8 byte limits, strict RFC 4291 IPv6, and reserved/uppercase PostgreSQL schemas', async () => {
      const plugin = makePlugin({
        allowedDatasourceUids: ['prom-main'],
        llmMode: 'grafana-llm-app',
        jobStoreBackend: 'postgres',
      });

      render(<AppConfig plugin={plugin as never} query={{}} />);

      // Malformed IPv6 and bracketed non-IPv6 MUST be rejected client-side.
      for (const badAddr of ['[:::]:5432', '[1::2::3]:5432', '[:]:5432', '[127.0.0.1]:5432', '[postgres]:5432']) {
        fireEvent.change(screen.getByPlaceholderText('postgres:5432'), { target: { value: badAddr } });
        fireEvent.click(screen.getByText(t('appConfig.actions.save')));
        await waitFor(() =>
          expect(screen.getByText(t('appConfig.validation.postgresAddrRequired'))).toBeTruthy()
        );
      }

      // Valid IPv6, but multi-byte Japanese DB name exceeding 63 UTF-8 bytes (22 chars = 66 bytes) MUST fail.
      fireEvent.change(screen.getByPlaceholderText('postgres:5432'), { target: { value: '[::1]:5432' } });
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresDatabaseLabel')), {
        target: { value: 'あ'.repeat(22) },
      });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresDatabaseRequired'))).toBeTruthy()
      );

      // Valid DB, multi-byte username exceeding 128 UTF-8 bytes (43 chars = 129 bytes) MUST fail.
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresDatabaseLabel')), {
        target: { value: 'grafana' },
      });
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresUsernameLabel')), {
        target: { value: 'あ'.repeat(43) },
      });
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.postgresUsernameRequired'))).toBeTruthy()
      );

      // Reserved system schemas and uppercase schema MUST fail.
      fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresUsernameLabel')), {
        target: { value: 'grafana' },
      });
      for (const badSchema of ['PUBLIC', 'pg_temp', 'pg_catalog', 'information_schema']) {
        fireEvent.change(screen.getByLabelText(t('appConfig.jobStorage.postgresSchemaLabel')), {
          target: { value: badSchema },
        });
        fireEvent.click(screen.getByText(t('appConfig.actions.save')));
        await waitFor(() =>
          expect(screen.getByText(t('appConfig.validation.postgresSchemaInvalid'))).toBeTruthy()
        );
      }
      expect(mockFetch).not.toHaveBeenCalled();
    });

    test('clearing a numeric input fails validation instead of silently coercing to 0', async () => {
      const plugin = makePlugin({
        allowedDatasourceUids: ['prom-main'],
        llmMode: 'grafana-llm-app',
        maxRepairAttempts: 2,
      });

      render(<AppConfig plugin={plugin as never} query={{}} />);

      const repairInput = screen.getByLabelText(t('appConfig.limits.maxRepairAttempts'));
      fireEvent.change(repairInput, { target: { value: '' } });

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(
          screen.getByText(
            t('appConfig.validation.numberRange', {
              label: t('appConfig.validation.fields.maxRepairAttempts'),
              min: 0,
              max: 2,
            })
          )
        ).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();
    });
  }
);

describe('AppConfig reactive locale switching', () => {
  beforeEach(async () => {
    await setStudioLocale(DEFAULT_LOCALE);
  });

  afterEach(async () => {
    await act(async () => {
      await setStudioLocale(DEFAULT_LOCALE);
    });
  });

  test('renders English by default and re-renders in Japanese when shared i18next switches to ja-JP', async () => {
    const plugin = makePlugin({
      allowedDatasourceUids: ['prom-main'],
      llmMode: 'grafana-llm-app',
    });

    render(<AppConfig plugin={plugin as never} query={{}} />);
    expect(screen.getByText('Permissions & Security Policy')).toBeTruthy();
    expect(screen.getByText('Save settings')).toBeTruthy();

    await act(async () => {
      await setStudioLocale('ja-JP');
    });

    expect(screen.getByText('権限とセキュリティ方針')).toBeTruthy();
    expect(screen.getByText('設定を保存')).toBeTruthy();
  });

  test('defaults enableSseProgress to false, persists true when enabled, and strips hidden mockDelayMs (L-1)', async () => {
    mockFetch.mockReset();
    mockFetch.mockReturnValue(of({ data: {} }));

    const plugin = makePlugin({
      allowedDatasourceUids: ['prom-main'],
      llmMode: 'grafana-llm-app',
      mockDelayMs: 5000,
    });

    render(<AppConfig plugin={plugin as never} query={{}} />);
    const sseCheckbox = screen.getByLabelText(t('appConfig.llm.sseProgressCheckbox')) as HTMLInputElement;
    expect(sseCheckbox.checked).toBe(false);

    fireEvent.click(sseCheckbox);
    expect(sseCheckbox.checked).toBe(true);

    fireEvent.click(screen.getByText(t('appConfig.actions.save')));
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

    const payload = mockFetch.mock.calls[0][0].data;
    expect(payload.jsonData.enableSseProgress).toBe(true);
    expect('mockDelayMs' in payload.jsonData).toBe(false);
  });
});
