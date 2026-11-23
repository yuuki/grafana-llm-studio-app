import React, { useEffect } from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { DEFAULT_LOCALE, setStudioLocale, SUPPORTED_LOCALES, t } from '../i18n';
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
  await waitFor(() => expect(screen.getByText(t('studio.mockBanner.title'))).toBeTruthy());
  const scopeInput = screen.getByPlaceholderText(t('studio.scope.placeholder'));
  fireEvent.change(scopeInput, { target: { value: 'checkout-api' } });
  fireEvent.click(screen.getByText(t('studio.catalog.fetch')));
  await waitFor(() => expect(screen.getByText('2')).toBeTruthy());
}

describe.each(SUPPORTED_LOCALES)(
  'StudioPage Scenes lifecycle, Dry-Run gating, and partial re-validation (locale: %s)',
  (locale) => {
    beforeEach(async () => {
      await setStudioLocale(locale);
      sceneMountCount = 0;
      sceneUnmountCount = 0;
      mockBuildPreviewScene.mockClear();
      mockRunDryRunValidation.mockReset();
      mockGenerateDraft.mockReset();
      mockPatchDraft.mockReset();
    });

    afterAll(async () => {
      await setStudioLocale(DEFAULT_LOCALE);
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
      fireEvent.click(screen.getByText(t('studio.actions.generateDraft')));
      await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());
      expect(mockBuildPreviewScene).not.toHaveBeenCalled();
      expect(mockRunDryRunValidation).not.toHaveBeenCalled();

      // Explicit Validate & Preview -> runs dry-run once and mounts Scene once.
      fireEvent.click(screen.getByText(t('studio.rightPane.validateAndPreview')));
      await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());
      expect(mockRunDryRunValidation).toHaveBeenCalledTimes(1);
      expect(mockBuildPreviewScene).toHaveBeenCalledTimes(1);
      expect(sceneMountCount).toBe(1);
      expect(sceneUnmountCount).toBe(0);

      // Unrelated UI re-renders and tab switches MUST NOT unmount or recreate Scene (H-8).
      const textarea = screen.getByPlaceholderText(t('studio.prompt.placeholder'));
      fireEvent.change(textarea, { target: { value: 'Another prompt text' } });
      fireEvent.click(screen.getByText(t('studio.rightPane.tabIr')));
      fireEvent.click(screen.getByText(t('studio.rightPane.tabFoundation')));
      fireEvent.click(screen.getByText(t('studio.rightPane.tabPreview')));

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

      fireEvent.click(screen.getByText(t('studio.actions.generateDraft')));
      await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());

      fireEvent.click(screen.getByText(t('studio.rightPane.validateAndPreview')));
      await waitFor(() =>
        expect(screen.getByText(t('studio.errors.validationBlocked'))).toBeTruthy()
      );
      expect(screen.getByText(t('studio.validation.category.syntax_error'))).toBeTruthy();

      expect(mockBuildPreviewScene).not.toHaveBeenCalled();
      expect(screen.queryByTestId('mock-scene-canvas')).toBeNull();
      const saveBtn = screen.getByText(t('studio.save.saveNewDashboard')) as HTMLButtonElement;
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

      fireEvent.click(screen.getByText(t('studio.actions.generateDraft')));
      await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());

      fireEvent.click(screen.getByText(t('studio.rightPane.validateAndPreview')));
      await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());
      const saveBtn = screen.getByText(t('studio.save.saveNewDashboard')) as HTMLButtonElement;
      expect(saveBtn.disabled).toBe(false);

      // Switch target to "__new__" and generate a brand-new job-2 (also revision 1!)
      const selects = screen.getAllByRole('combobox');
      const targetSelect = selects[selects.length - 2]; // target panel select (folder select is last)
      fireEvent.change(targetSelect, { target: { value: '__new__' } });
      fireEvent.click(screen.getByText(t('studio.actions.generateDraft')));

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
      fireEvent.click(screen.getByText(t('studio.actions.generateDraft')));
      await waitFor(() => expect(screen.getByText('awaiting_preview')).toBeTruthy());
      fireEvent.click(screen.getByText(t('studio.rightPane.validateAndPreview')));
      await waitFor(() => expect(screen.getByTestId('mock-scene-canvas')).toBeTruthy());
      expect(mockRunDryRunValidation).toHaveBeenNthCalledWith(
        1,
        expect.anything(),
        expect.objectContaining({ onlyPanelIds: ['panel-req', 'panel-err'] })
      );

      // Partial edit changes only panel-err query
      fireEvent.click(screen.getByText(t('studio.actions.applyPatch')));
      await waitFor(() => expect(mockPatchDraft).toHaveBeenCalledTimes(1));

      // Re-validate -> MUST ONLY query panel-err (panel-req queryHash unchanged!)
      fireEvent.click(screen.getByText(t('studio.rightPane.validateAndPreview')));
      await waitFor(() => expect(mockRunDryRunValidation).toHaveBeenCalledTimes(2));
      expect(mockRunDryRunValidation).toHaveBeenNthCalledWith(
        2,
        expect.anything(),
        expect.objectContaining({ onlyPanelIds: ['panel-err'] })
      );
    });
  }
);

describe('StudioPage reactive locale switching', () => {
  beforeEach(async () => {
    await setStudioLocale(DEFAULT_LOCALE);
  });

  afterEach(async () => {
    await act(async () => {
      await setStudioLocale(DEFAULT_LOCALE);
    });
  });

  test('renders English by default and re-renders in Japanese when shared i18next switches to ja-JP', async () => {
    render(<StudioPage />);
    await waitFor(() =>
      expect(screen.getByText('Development/Testing Mock LLM Mode')).toBeTruthy()
    );
    expect(screen.getByText('Fetch Scoped Metrics')).toBeTruthy();
    expect(screen.getByText('Validate & Preview')).toBeTruthy();

    await act(async () => {
      await setStudioLocale('ja-JP');
    });

    expect(screen.getByText('開発・テスト用モックLLMモード')).toBeTruthy();
    expect(screen.getByText('スコープのメトリクスを取得')).toBeTruthy();
    expect(screen.getByText('検証・プレビューを実行')).toBeTruthy();
  });
});
