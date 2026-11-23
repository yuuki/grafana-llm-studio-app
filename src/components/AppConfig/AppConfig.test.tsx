import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { of } from 'rxjs';
import { AppPluginMeta } from '@grafana/data';
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
  }: {
    options: Array<{ label: string; value: string }>;
    value?: string;
    onChange: (v: { value: string }) => void;
  }) => (
    <select value={value} onChange={(e) => onChange({ value: e.target.value })}>
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

describe('AppConfig settings preservation, secret reset, and validation', () => {
  beforeEach(() => {
    mockFetch.mockReset();
    mockFetch.mockReturnValue(of({ data: {} }));
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
    fireEvent.click(screen.getByText('設定を保存'));

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
    fireEvent.click(screen.getByText('設定を保存'));
    await waitFor(() =>
      expect(screen.getByText(/allowMockInDev を明示的に有効化してください/)).toBeTruthy()
    );
    expect(mockFetch).not.toHaveBeenCalled();

    // Enable allowMockInDev, then switch back to grafana-llm-app -> saved payload MUST normalize allowMockInDev: false.
    fireEvent.click(screen.getByRole('checkbox'));
    const modeSelect = screen.getAllByRole('combobox')[0];
    fireEvent.change(modeSelect, { target: { value: 'grafana-llm-app' } });

    fireEvent.click(screen.getByText('設定を保存'));
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
    // Selects: [0]=llmMode, [1]=modelTier, [2]=structuredOutputMode, [3]=tokenLimitField, [4]=reasoningEffort
    fireEvent.change(selects[2], { target: { value: 'json_object' } });
    fireEvent.change(selects[3], { target: { value: 'max_tokens' } });
    fireEvent.change(selects[4], { target: { value: 'low' } });

    fireEvent.click(screen.getByText('設定を保存'));
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
    expect(mockFetch.mock.calls[0][0].data.jsonData.structuredOutputMode).toBe('json_object');
    expect(mockFetch.mock.calls[0][0].data.jsonData.tokenLimitField).toBe('max_tokens');
    expect(mockFetch.mock.calls[0][0].data.jsonData.reasoningEffort).toBe('low');
  });
});
