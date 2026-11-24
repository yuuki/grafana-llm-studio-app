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
      fireEvent.click(screen.getByRole('checkbox'));
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

      const storeSelect = screen.getByLabelText('Job store backend');
      fireEvent.change(storeSelect, { target: { value: 'redis' } });

      // Saving redis without redisAddr MUST fail client-side validation before fetch.
      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() =>
        expect(screen.getByText(t('appConfig.validation.redisAddrRequired'))).toBeTruthy()
      );
      expect(mockFetch).not.toHaveBeenCalled();

      // Enter valid host:port, reset existing redisPassword, and enable TLS.
      fireEvent.change(screen.getByPlaceholderText('redis:6379'), { target: { value: 'redis.ha:6379' } });
      fireEvent.click(screen.getByText('Reset Secret'));
      fireEvent.click(screen.getByRole('checkbox'));

      fireEvent.click(screen.getByText(t('appConfig.actions.save')));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      const payload = mockFetch.mock.calls[0][0].data;
      expect(payload.jsonData.jobStoreBackend).toBe('redis');
      expect(payload.jsonData.redisAddr).toBe('redis.ha:6379');
      expect(payload.jsonData.redisKeyPrefix).toBe('llmstudio:jobs');
      expect(payload.jsonData.redisTls).toBe(true);
      expect(payload.secureJsonData).toEqual({ redisPassword: '' });
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
});
