import React, { useEffect } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { computeQueryHash } from '../ir/types';
import { StudioPage } from './StudioPage';

let sceneMountCount = 0;
let sceneUnmountCount = 0;

const MockSceneComponent: React.FC = () => {
  useEffect(() => {
    sceneMountCount += 1;
    return () => {
      sceneUnmountCount += 1;
    };
  }, []);
  return <div data-testid="mock-scene-canvas">Scenes Canvas</div>;
};

const mockBuildPreviewScene = jest.fn((..._args: unknown[]) => ({
  Component: MockSceneComponent,
}));

jest.mock('../spec/toScene', () => ({
  buildPreviewScene: (...args: unknown[]) => mockBuildPreviewScene(...args),
}));

const mockGenerateDraft = jest.fn();
const mockImportDraft = jest.fn();
const mockPatchDraft = jest.fn();
const mockRepairDraft = jest.fn();
const mockUndoDraft = jest.fn();
const mockCancelDraftJob = jest.fn();

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
  generateDraft: (...args: unknown[]) => mockGenerateDraft(...args),
  importDraft: (...args: unknown[]) => mockImportDraft(...args),
  patchDraft: (...args: unknown[]) => mockPatchDraft(...args),
  repairDraft: (...args: unknown[]) => mockRepairDraft(...args),
  undoDraft: (...args: unknown[]) => mockUndoDraft(...args),
  cancelDraftJob: (...args: unknown[]) => mockCancelDraftJob(...args),
  extractApiError: (err: unknown) => (err instanceof Error ? err.message : String(err)),
}));

jest.mock('../datasource/metadata', () => ({
  fetchScopeLabelValues: jest.fn(async () => ['checkout-api']),
  fetchScopedMetricCatalog: jest.fn(async () => ({
    items: [
      { name: 'http_requests_total', type: 'counter', labels: ['service', 'status'] },
      { name: 'http_request_duration_seconds_bucket', type: 'histogram', labels: ['service', 'le'] },
    ],
    truncated: false,
    fetchedAt: '2026-11-22T00:00:00Z',
  })),
}));

const mockRunDryRunValidation = jest.fn();

jest.mock('../datasource/validator', () => ({
  runDryRunValidation: (...args: unknown[]) => mockRunDryRunValidation(...args),
}));

const mockFetchExistingDashboardByUid = jest.fn();
const mockListExistingDashboards = jest.fn();

jest.mock('../import/importDashboard', () => {
  const actual = jest.requireActual('../import/importDashboard');
  return {
    ...actual,
    fetchExistingDashboardByUid: (...args: unknown[]) => mockFetchExistingDashboardByUid(...args),
    listExistingDashboards: (...args: unknown[]) => mockListExistingDashboards(...args),
  };
});

const mockCreateNewDashboard = jest.fn();
const mockUpdateExistingDashboard = jest.fn();

jest.mock('../save/saveDashboard', () => ({
  listWritableFolders: jest.fn(async () => [{ uid: '', title: 'General (Root)' }]),
  buildExportableDashboardJson: jest.fn(() => '{}'),
  DashboardSaver: jest.fn().mockImplementation(() => ({
    createNewDashboard: (...args: unknown[]) => mockCreateNewDashboard(...args),
    updateExistingDashboard: (...args: unknown[]) => mockUpdateExistingDashboard(...args),
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

const REQ_EXPR = 'sum(rate(http_requests_total{service="checkout-api"}[5m]))';
const ERR_EXPR_V1 = 'sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m]))';
const ERR_EXPR_V2 = 'sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[1m]))';

function makeTwoPanelJob(jobId: string, revision: number, errExpr: string) {
  return {
    jobId,
    status: 'awaiting_preview',
    datasourceUid: 'prom-main',
    currentIR: {
      schemaVersion: '1.0',
      revision,
      title: 'checkout-api Overview',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-req',
          type: 'timeseries',
          title: 'Request Rate',
          datasourceRef: 'selected_prometheus',
          query: { expr: REQ_EXPR },
          unit: 'reqps',
          layout: { row: 0, col: 0, width: 'half' },
        },
        {
          id: 'panel-err',
          type: 'stat',
          title: 'Error Rate',
          datasourceRef: 'selected_prometheus',
          query: { expr: errExpr },
          unit: 'percentunit',
          layout: { row: 0, col: 1, width: 'half' },
        },
      ],
    },
    repairAttemptsUsed: 0,
    maxRepairAttempts: 2,
    canUndo: revision > 1,
    updatedAt: '2026-11-22T00:00:00Z',
  };
}

async function setupScopeAndCatalog() {
  await waitFor(() => expect(screen.getByText('開発・テスト用モックLLMモード')).toBeTruthy());
  const scopeInput = screen.getByPlaceholderText('checkout-api');
  fireEvent.change(scopeInput, { target: { value: 'checkout-api' } });
  fireEvent.click(screen.getByText('スコープのメトリクスを取得'));
  await waitFor(() => expect(screen.getByText('2')).toBeTruthy());
}

describe('StudioPage Scenes lifecycle, Dry-Run gating, and partial re-validation', () => {
  beforeEach(() => {
    sceneMountCount = 0;
    sceneUnmountCount = 0;
    mockBuildPreviewScene.mockClear();
    mockRunDryRunValidation.mockReset();
    mockGenerateDraft.mockReset();
    mockPatchDraft.mockReset();
  });

  test('does not construct Scene on generation, and tab switching keeps Scene mounted without unmounting (H-8)', async () => {
    const reqHash = await computeQueryHash(REQ_EXPR);
    const errHash = await computeQueryHash(ERR_EXPR_V1);

    mockGenerateDraft.mockResolvedValueOnce(makeTwoPanelJob('job-1', 1, ERR_EXPR_V1));
    mockRunDryRunValidation.mockResolvedValueOnce([
      { panelId: 'panel-req', queryHash: reqHash, category: 'ok', seriesCount: 2, durationMs: 12 },
      { panelId: 'panel-err', queryHash: errHash, category: 'ok', seriesCount: 1, durationMs: 10 },
    ]);

    render(<StudioPage />);
    await setupScopeAndCatalog();

    // Generate draft -> Scene MUST NOT be constructed or queried yet.
    fireEvent.click(screen.getByText('ダッシュボード案を生成'));
    await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());
    expect(mockBuildPreviewScene).not.toHaveBeenCalled();
    expect(mockRunDryRunValidation).not.toHaveBeenCalled();

    // Explicit "検証・プレビューを実行" -> runs dry-run once and mounts Scene once.
    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());
    expect(mockRunDryRunValidation).toHaveBeenCalledTimes(1);
    expect(mockBuildPreviewScene).toHaveBeenCalledTimes(1);
    expect(sceneMountCount).toBe(1);
    expect(sceneUnmountCount).toBe(0);

    // Unrelated UI re-renders and tab switches MUST NOT unmount or recreate Scene (H-8).
    const textarea = screen.getByPlaceholderText(/エラー率のパネルだけ/);
    fireEvent.change(textarea, { target: { value: '別の入力テキスト' } });
    fireEvent.click(screen.getByText('IR JSON'));
    fireEvent.click(screen.getByText('Foundation SDK JSON'));
    fireEvent.click(screen.getByText('Scenes プレビュー'));

    expect(mockBuildPreviewScene).toHaveBeenCalledTimes(1);
    expect(mockRunDryRunValidation).toHaveBeenCalledTimes(1);
    expect(sceneMountCount).toBe(1);
    expect(sceneUnmountCount).toBe(0);
  });

  test('blocks Scenes construction and keeps Save disabled when Dry-Run reports syntax_error (C-2)', async () => {
    const reqHash = await computeQueryHash(REQ_EXPR);
    const errHash = await computeQueryHash(ERR_EXPR_V1);

    mockGenerateDraft.mockResolvedValueOnce(makeTwoPanelJob('job-err', 1, ERR_EXPR_V1));
    mockRunDryRunValidation.mockResolvedValueOnce([
      { panelId: 'panel-req', queryHash: reqHash, category: 'ok', seriesCount: 2, durationMs: 10 },
      {
        panelId: 'panel-err',
        queryHash: errHash,
        category: 'syntax_error',
        errorText: 'parse error: unexpected ")"',
        durationMs: 8,
      },
    ]);

    render(<StudioPage />);
    await setupScopeAndCatalog();

    fireEvent.click(screen.getByText('ダッシュボード案を生成'));
    await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());

    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(screen.getByText(/Scenes プレビューと保存をブロックしました/)).toBeTruthy());

    expect(mockBuildPreviewScene).not.toHaveBeenCalled();
    expect(screen.queryByTestId('mock-scene-canvas')).toBeNull();
    const saveBtn = screen.getByText('新規ダッシュボード保存') as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(true);
  });

  test('starting a brand-new draft (__new__) clears previous preview and disables Save until re-validated (C-2)', async () => {
    const reqHash = await computeQueryHash(REQ_EXPR);
    const errHash = await computeQueryHash(ERR_EXPR_V1);

    mockGenerateDraft
      .mockResolvedValueOnce(makeTwoPanelJob('job-1', 1, ERR_EXPR_V1))
      .mockResolvedValueOnce(makeTwoPanelJob('job-2', 1, ERR_EXPR_V2));
    mockRunDryRunValidation.mockResolvedValueOnce([
      { panelId: 'panel-req', queryHash: reqHash, category: 'ok', seriesCount: 2, durationMs: 10 },
      { panelId: 'panel-err', queryHash: errHash, category: 'ok', seriesCount: 1, durationMs: 10 },
    ]);

    render(<StudioPage />);
    await setupScopeAndCatalog();

    fireEvent.click(screen.getByText('ダッシュボード案を生成'));
    await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());

    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());
    const saveBtn = screen.getByText('新規ダッシュボード保存') as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(false);

    // Switch target to "__new__" and generate a brand-new job-2 (also revision 1!)
    const selects = screen.getAllByRole('combobox');
    const targetSelect = selects[selects.length - 2]; // target panel select (folder select is last)
    fireEvent.change(targetSelect, { target: { value: '__new__' } });
    fireEvent.click(screen.getByText('ダッシュボード案を生成'));

    await waitFor(() => expect(mockGenerateDraft).toHaveBeenCalledTimes(2));
    expect(screen.queryByTestId('mock-scene-canvas')).toBeNull();
    expect(saveBtn.disabled).toBe(true);
  });

  test('partial patch re-validates only changed panels via queryHash and retains untouched panel results (H-9)', async () => {
    const reqHash = await computeQueryHash(REQ_EXPR);
    const errHashV1 = await computeQueryHash(ERR_EXPR_V1);
    const errHashV2 = await computeQueryHash(ERR_EXPR_V2);

    mockGenerateDraft.mockResolvedValueOnce(makeTwoPanelJob('job-1', 1, ERR_EXPR_V1));
    mockPatchDraft.mockResolvedValueOnce(makeTwoPanelJob('job-1', 2, ERR_EXPR_V2));

    mockRunDryRunValidation
      .mockResolvedValueOnce([
        { panelId: 'panel-req', queryHash: reqHash, category: 'ok', seriesCount: 2, durationMs: 10 },
        { panelId: 'panel-err', queryHash: errHashV1, category: 'ok', seriesCount: 1, durationMs: 10 },
      ])
      .mockResolvedValueOnce([
        { panelId: 'panel-err', queryHash: errHashV2, category: 'ok', seriesCount: 1, durationMs: 9 },
      ]);

    render(<StudioPage />);
    await setupScopeAndCatalog();

    // Generate + initial validate (both panels validated)
    fireEvent.click(screen.getByText('ダッシュボード案を生成'));
    await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());
    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());
    expect(mockRunDryRunValidation).toHaveBeenNthCalledWith(
      1,
      expect.anything(),
      expect.objectContaining({ onlyPanelIds: ['panel-req', 'panel-err'] })
    );

    // Partial edit changes only panel-err query
    fireEvent.click(screen.getByText('部分修正を適用'));
    await waitFor(() => expect(mockPatchDraft).toHaveBeenCalledTimes(1));

    // Re-validate -> MUST ONLY query panel-err (panel-req queryHash unchanged!)
    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(mockRunDryRunValidation).toHaveBeenCalledTimes(2));
    expect(mockRunDryRunValidation).toHaveBeenNthCalledWith(
      2,
      expect.anything(),
      expect.objectContaining({ onlyPanelIds: ['panel-err'] })
    );
  });

  test('safe import defaults to create-as-copy and only uses revision-checked PUT when explicitly opted in', async () => {
    const reqHash = await computeQueryHash(REQ_EXPR);

    mockFetchExistingDashboardByUid.mockResolvedValueOnce({
      source: {
        uid: 'existing-dash-1',
        resourceVersion: '27',
        folderUid: '',
        title: 'Existing Dash',
      },
      rawDashboard: {
        title: 'Existing Dash',
        time: { from: 'now-15m', to: 'now' },
        panels: [
          {
            type: 'gauge',
            title: 'Request Gauge',
            datasource: { type: 'prometheus', uid: 'prom-main' },
            gridPos: { x: 0, y: 0, w: 12, h: 6 },
            fieldConfig: { defaults: { unit: 'reqps' } },
            targets: [{ expr: REQ_EXPR }],
          },
        ],
      },
    });
    mockImportDraft.mockImplementationOnce(async (req: { draft: unknown; importSource: unknown }) => ({
      jobId: 'job-imported-1',
      status: 'awaiting_preview',
      datasourceUid: 'prom-main',
      importSource: req.importSource,
      currentIR: req.draft,
      repairAttemptsUsed: 0,
      maxRepairAttempts: 2,
      canUndo: false,
      updatedAt: '2026-11-23T00:00:00Z',
    }));
    mockRunDryRunValidation.mockResolvedValueOnce([
      { panelId: 'panel-request-gauge', queryHash: reqHash, category: 'ok', seriesCount: 1, durationMs: 8 },
    ]);
    mockCreateNewDashboard.mockResolvedValueOnce({
      uid: 'llmstudio-copy123',
      url: '/d/llmstudio-copy123',
      folderUid: '',
      revision: 1,
      mode: 'created',
    });
    mockUpdateExistingDashboard.mockResolvedValueOnce({
      uid: 'existing-dash-1',
      url: '/d/existing-dash-1',
      folderUid: '',
      revision: 1,
      mode: 'updated',
      resourceVersion: '28',
    });

    render(<StudioPage />);
    await waitFor(() => expect(screen.getByText('開発・テスト用モックLLMモード')).toBeTruthy());

    // Open collapsible safe import panel and import existing dashboard
    fireEvent.click(screen.getByText('既存ダッシュボードをインポート（任意）'));
    fireEvent.change(screen.getByPlaceholderText('existing-dashboard-uid'), {
      target: { value: 'existing-dash-1' },
    });
    fireEvent.click(screen.getByText('安全な IR としてインポート'));

    await waitFor(() => expect(screen.getByTestId('studio-import-report')).toBeTruthy());
    expect(mockImportDraft).toHaveBeenCalledTimes(1);

    // Dry-run & preview to unlock save
    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());

    // Default save mode is create-as-copy (POST)
    fireEvent.click(screen.getByText('新規ダッシュボード保存'));
    await waitFor(() => expect(mockCreateNewDashboard).toHaveBeenCalledTimes(1));
    expect(mockUpdateExistingDashboard).not.toHaveBeenCalled();

    // Explicitly opt in to revision-checked update (PUT with resourceVersion)
    fireEvent.click(screen.getByTestId('studio-optin-update-checkbox'));
    fireEvent.click(screen.getByText('既存ダッシュボードを更新'));
    await waitFor(() => expect(mockUpdateExistingDashboard).toHaveBeenCalledTimes(1));
    expect(mockUpdateExistingDashboard).toHaveBeenCalledWith(
      expect.objectContaining({
        targetUid: 'existing-dash-1',
        expectedResourceVersion: '27',
        previewedRevision: 1,
      })
    );
    // Opt-in checkbox resets to false after save so next save defaults safely to create-as-copy.
    expect((screen.getByTestId('studio-optin-update-checkbox') as HTMLInputElement).checked).toBe(false);
  });

  test('variable-only patch invalidates dry-run cache even when panel queryHash is unchanged (H-8)', async () => {
    const varExpr = 'sum(rate(http_requests_total{service="$service"}[5m]))';
    const varHash = await computeQueryHash(varExpr);

    mockGenerateDraft.mockResolvedValueOnce({
      jobId: 'job-var-cache',
      status: 'awaiting_preview',
      datasourceUid: 'prom-main',
      currentIR: {
        schemaVersion: '1.1',
        revision: 1,
        title: 'Var Draft',
        timeRange: { from: 'now-15m', to: 'now' },
        variables: [
          {
            name: 'service',
            type: 'query',
            datasourceRef: 'selected_prometheus',
            labelName: 'service',
            metricName: 'http_requests_total',
            defaultValue: 'checkout-api',
          },
        ],
        panels: [
          {
            id: 'panel-req',
            type: 'timeseries',
            title: 'Requests',
            datasourceRef: 'selected_prometheus',
            query: { expr: varExpr },
            unit: 'reqps',
            layout: { row: 0, col: 0, width: 'full' },
          },
        ],
      },
      repairAttemptsUsed: 0,
      maxRepairAttempts: 2,
      canUndo: false,
      updatedAt: '2026-11-23T00:00:00Z',
    });
    mockRunDryRunValidation
      .mockResolvedValueOnce([
        { panelId: 'panel-req', queryHash: varHash, category: 'ok', seriesCount: 1, durationMs: 5 },
      ])
      .mockResolvedValueOnce([
        { panelId: 'panel-req', queryHash: varHash, category: 'ok', seriesCount: 1, durationMs: 5 },
      ]);
    mockPatchDraft.mockResolvedValueOnce({
      jobId: 'job-var-cache',
      status: 'awaiting_preview',
      datasourceUid: 'prom-main',
      currentIR: {
        schemaVersion: '1.1',
        revision: 2,
        title: 'Var Draft',
        timeRange: { from: 'now-15m', to: 'now' },
        variables: [
          {
            name: 'service',
            type: 'query',
            datasourceRef: 'selected_prometheus',
            labelName: 'service',
            metricName: 'http_requests_total',
            defaultValue: 'payments-api',
          },
        ],
        panels: [
          {
            id: 'panel-req',
            type: 'timeseries',
            title: 'Requests',
            datasourceRef: 'selected_prometheus',
            query: { expr: varExpr },
            unit: 'reqps',
            layout: { row: 0, col: 0, width: 'full' },
          },
        ],
      },
      repairAttemptsUsed: 0,
      maxRepairAttempts: 2,
      canUndo: true,
      updatedAt: '2026-11-23T00:00:05Z',
    });

    render(<StudioPage />);
    await waitFor(() => expect(screen.getByText('開発・テスト用モックLLMモード')).toBeTruthy());

    fireEvent.change(screen.getByPlaceholderText('checkout-api'), { target: { value: 'checkout-api' } });
    fireEvent.click(screen.getByText('スコープのメトリクスを取得'));
    await waitFor(() => expect(screen.getByTestId('studio-catalog-count').textContent).toBe('2'));

    fireEvent.click(screen.getByText('ダッシュボード案を生成'));
    await waitFor(() => expect(screen.getByTestId('studio-job-revision').textContent).toBe('rev 1'));

    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(mockRunDryRunValidation).toHaveBeenCalledTimes(1));

    // Patch changes ONLY variable defaultValue; panel queryHash stays identical
    fireEvent.click(screen.getByText('部分修正を適用'));
    await waitFor(() => expect(screen.getByTestId('studio-job-revision').textContent).toBe('rev 2'));

    // Re-validate MUST re-run dry-run for panel-req because variablesFingerprint changed!
    fireEvent.click(screen.getByText('検証・プレビューを実行'));
    await waitFor(() => expect(mockRunDryRunValidation).toHaveBeenCalledTimes(2));
    expect(mockRunDryRunValidation).toHaveBeenNthCalledWith(
      2,
      expect.anything(),
      expect.objectContaining({ onlyPanelIds: ['panel-req'] })
    );
  });
});
