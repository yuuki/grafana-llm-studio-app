import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { StudioPage } from './StudioPage';

const mockBuildPreviewScene = jest.fn((..._args: unknown[]) => ({
  Component: () => <div data-testid="mock-scene-canvas">Scenes Canvas</div>,
}));

jest.mock('../spec/toScene', () => ({
  buildPreviewScene: (...args: unknown[]) => mockBuildPreviewScene(...args),
}));

jest.mock('../api/studioClient', () => ({
  fetchPublicConfig: jest.fn(async () => ({
    allowedDatasourceUids: ['prom-main'],
    llmMode: 'mock',
    modelTier: 'base',
    maxPanels: 6,
    maxRepairAttempts: 2,
    validationTimeoutSeconds: 10,
    maxConcurrentQueries: 2,
    mockBanner: true,
  })),
  generateDraft: jest.fn(async () => ({
    jobId: 'job-1',
    status: 'awaiting_preview',
    datasourceUid: 'prom-main',
    currentIR: {
      schemaVersion: '1.0',
      revision: 1,
      title: 'checkout-api Overview',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-req',
          type: 'timeseries',
          title: 'Request Rate',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'sum(rate(http_requests_total{service="checkout-api"}[5m]))' },
          unit: 'reqps',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    },
    repairAttemptsUsed: 0,
    maxRepairAttempts: 2,
    canUndo: false,
    updatedAt: '2026-11-22T00:00:00Z',
  })),
  patchDraft: jest.fn(),
  repairDraft: jest.fn(),
  undoDraft: jest.fn(),
  cancelDraftJob: jest.fn(),
}));

jest.mock('../datasource/metadata', () => ({
  fetchScopeLabelValues: jest.fn(async () => ['checkout-api']),
  fetchScopedMetricCatalog: jest.fn(async () => ({
    items: [{ name: 'http_requests_total', type: 'counter', labels: ['service', 'status'] }],
    truncated: false,
    fetchedAt: '2026-11-22T00:00:00Z',
  })),
}));

const mockRunDryRunValidation = jest.fn(async (..._args: unknown[]) => [
  { panelId: 'panel-req', queryHash: 'abc', category: 'ok', seriesCount: 2, durationMs: 12 },
]);

jest.mock('../datasource/validator', () => ({
  runDryRunValidation: (...args: unknown[]) => mockRunDryRunValidation(...args),
}));

jest.mock('../save/saveDashboard', () => ({
  listWritableFolders: jest.fn(async () => [{ uid: '', title: 'General (Root)' }]),
  buildExportableDashboardJson: jest.fn(() => '{}'),
  DashboardSaver: jest.fn().mockImplementation(() => ({
    createNewDashboard: jest.fn(),
  })),
}));

jest.mock('@grafana/runtime', () => ({
  config: { bootData: { user: { orgId: 1 } } },
  getDataSourceSrv: () => ({
    getList: () => [{ uid: 'prom-main', name: 'Prometheus', type: 'prometheus' }],
  }),
}));

jest.mock('@grafana/ui', () => ({
  Alert: ({ title, children }: { title: string; children?: React.ReactNode }) => (
    <div>
      <strong>{title}</strong>
      {children}
    </div>
  ),
  Badge: ({ text }: { text: string }) => <span>{text}</span>,
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
  Field: ({ label, children }: { label: string; children?: React.ReactNode }) => (
    <label>
      <span>{label}</span>
      {children}
    </label>
  ),
  Input: (props: React.InputHTMLAttributes<HTMLInputElement>) => <input {...props} />,
  TextArea: (props: React.TextareaHTMLAttributes<HTMLTextAreaElement>) => <textarea {...props} />,
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
  Spinner: () => <span>...</span>,
}));

describe('StudioPage Scenes anti-re-query behavior', () => {
  beforeEach(() => {
    mockBuildPreviewScene.mockClear();
    mockRunDryRunValidation.mockClear();
  });

  test('does not construct Scene on generation, and unrelated re-renders do not recreate Scene or re-run queries', async () => {
    render(<StudioPage />);

    await waitFor(() => expect(screen.getByText('開発・テスト用モックLLMモード')).toBeTruthy());

    // Enter scope and load scoped catalog.
    const scopeInput = screen.getByPlaceholderText('checkout-api');
    fireEvent.change(scopeInput, { target: { value: 'checkout-api' } });
    fireEvent.click(screen.getByText('スコープのメトリクスを取得'));
    await waitFor(() => expect(screen.getByText('1')).toBeTruthy());

    // Generate draft -> Scene MUST NOT be constructed or queried yet.
    fireEvent.click(screen.getByText('ダッシュボード案を生成'));
    await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());
    expect(mockBuildPreviewScene).not.toHaveBeenCalled();
    expect(mockRunDryRunValidation).not.toHaveBeenCalled();

    // Explicit "検証・プレビューを実行" -> runs dry-run once and constructs Scene once.
    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());
    expect(mockRunDryRunValidation).toHaveBeenCalledTimes(1);
    expect(mockBuildPreviewScene).toHaveBeenCalledTimes(1);

    // Unrelated UI re-renders (typing in prompt, switching tabs) MUST NOT recreate Scene or re-run queries.
    const textarea = screen.getByPlaceholderText(/エラー率のパネルだけ/);
    fireEvent.change(textarea, { target: { value: '別の入力テキスト' } });
    fireEvent.click(screen.getByText('IR JSON'));
    fireEvent.click(screen.getByText('Scenes プレビュー'));

    expect(mockBuildPreviewScene).toHaveBeenCalledTimes(1);
    expect(mockRunDryRunValidation).toHaveBeenCalledTimes(1);
  });
});
