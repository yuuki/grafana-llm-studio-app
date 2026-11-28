import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { setStudioLocale } from '../i18n';
import { JobSnapshot } from '../ir/types';
import { StatusReportPage } from './StatusReportPage';

const mockPush = jest.fn();
const mockFetchPublicConfig = jest.fn();
const mockAnalyzeStatus = jest.fn();
const mockCancelDraftJob = jest.fn();
const mockListExistingDashboards = jest.fn();
const mockFetchExistingDashboardByUid = jest.fn();
const mockCollectAlerts = jest.fn();
const mockCollectMetrics = jest.fn();
const mockCopyText = jest.fn();

jest.mock('@grafana/runtime', () => ({
  config: { bootData: { user: { orgId: 1 } } },
  getDataSourceSrv: () => ({
    getList: () => [{ uid: 'prom-main', name: 'Prometheus Main', type: 'prometheus' }],
    getInstanceSettings: () => ({ uid: 'prom-main', type: 'prometheus' }),
  }),
  locationService: {
    push: (...args: unknown[]) => mockPush(...args),
  },
  PluginPage: ({ children }: { children?: React.ReactNode }) => <div data-testid="plugin-page">{children}</div>,
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
    'data-testid': testId,
  }: {
    children?: React.ReactNode;
    onClick?: () => void;
    disabled?: boolean;
    'data-testid'?: string;
  }) => (
    <button type="button" onClick={onClick} disabled={disabled} data-testid={testId}>
      {children}
    </button>
  ),
  Checkbox: ({
    label,
    value,
    onChange,
    disabled,
  }: {
    label: string;
    value?: boolean;
    onChange?: (e: React.ChangeEvent<HTMLInputElement>) => void;
    disabled?: boolean;
  }) => (
    <label>
      <input type="checkbox" checked={Boolean(value)} onChange={onChange} disabled={disabled} />
      <span>{label}</span>
    </label>
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
    inputId,
  }: {
    options: Array<{ label: string; value: string }>;
    value?: string;
    onChange: (v: { value: string }) => void;
    inputId?: string;
  }) => (
    <select id={inputId} value={value} onChange={(e) => onChange({ value: e.target.value })}>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  ),
  Spinner: () => <span>...</span>,
}));

jest.mock('../api/studioClient', () => ({
  fetchPublicConfig: (...args: unknown[]) => mockFetchPublicConfig(...args),
  analyzeStatus: (...args: unknown[]) => mockAnalyzeStatus(...args),
  cancelDraftJob: (...args: unknown[]) => mockCancelDraftJob(...args),
  extractApiError: (err: unknown) => (err instanceof Error ? err.message : String(err)),
}));

jest.mock('../import/importDashboard', () => ({
  listExistingDashboards: (...args: unknown[]) => mockListExistingDashboards(...args),
  fetchExistingDashboardByUid: (...args: unknown[]) => mockFetchExistingDashboardByUid(...args),
}));

jest.mock('../report/alertsCollector', () => ({
  collectGrafanaAlertingEvidence: (...args: unknown[]) => mockCollectAlerts(...args),
}));

jest.mock('../report/metricsCollector', () => ({
  collectDashboardMetricEvidence: (...args: unknown[]) => mockCollectMetrics(...args),
}));

jest.mock('../report/markdownExport', () => {
  const actual = jest.requireActual('../report/markdownExport');
  return {
    ...actual,
    copyTextWithFallback: (...args: unknown[]) => mockCopyText(...args),
  };
});

function makeReportSnapshot(): JobSnapshot {
  return {
    jobId: 'job-rep-1',
    jobKind: 'report',
    status: 'ready',
    datasourceUid: 'prom-main',
    repairAttemptsUsed: 0,
    maxRepairAttempts: 0,
    canUndo: false,
    updatedAt: '2026-11-27T00:10:00Z',
    statusReport: {
      schemaVersion: '1.0',
      overallStatus: 'degraded',
      confidence: 'high',
      executiveSummary: 'Checkout API is experiencing elevated 5xx errors.',
      findings: [
        {
          id: 'finding-1',
          severity: 'critical',
          category: 'alert',
          title: 'HighErrorRate firing',
          detail: '5xx error rate exceeded 5% for checkout-api.',
          evidenceIds: ['ev-alert-1'],
        },
      ],
      timeline: [],
      hypotheses: [],
      recommendedActions: [
        {
          priority: 'immediate',
          action: 'Check recent checkout-api deployment',
          relatedFindingIds: ['finding-1'],
        },
      ],
      dataGaps: [],
      suggestedInvestigationIntent: 'Build a troubleshooting dashboard for checkout-api 5xx errors and latency',
    },
  };
}

describe('StatusReportPage', () => {
  beforeEach(() => {
    setStudioLocale('en');
    mockPush.mockReset();
    mockFetchPublicConfig.mockReset();
    mockAnalyzeStatus.mockReset();
    mockCancelDraftJob.mockReset();
    mockListExistingDashboards.mockReset();
    mockFetchExistingDashboardByUid.mockReset();
    mockCollectAlerts.mockReset();
    mockCollectMetrics.mockReset();
    mockCopyText.mockReset();

    mockFetchPublicConfig.mockResolvedValue({
      allowedDatasourceUids: ['prom-main'],
      llmMode: 'mock',
      modelTier: 'base',
      maxPanels: 6,
      maxRepairAttempts: 2,
      validationTimeoutSeconds: 10,
      maxConcurrentQueries: 2,
      enableSseProgress: false,
      mockBanner: true,
    });
    mockListExistingDashboards.mockResolvedValue([
      { uid: 'dash-checkout', title: 'Checkout Overview', folderUid: '', folderTitle: 'General' },
    ]);
    mockCollectAlerts.mockResolvedValue({
      alerts: [
        {
          id: 'ev-alert-1',
          alertName: 'HighErrorRate',
          state: 'active',
          severity: 'critical',
          summary: 'token=glsa_abcdefghijklmnopqrstuvwxyz1234',
          labels: { service: 'checkout-api' },
        },
      ],
      rules: [],
      statusNotes: [],
      warnings: [],
      collectionStatus: {
        alertsChecked: true,
        rulesChecked: true,
        totalRulesEvaluated: 4,
        activeAlertsTotal: 1,
        abnormalRulesTotal: 0,
      },
    });
    mockAnalyzeStatus.mockResolvedValue(makeReportSnapshot());
    mockCopyText.mockResolvedValue(true);
  });

  test('1-click alerts-only flow analyzes, redacts secrets, copies Markdown, and hands off to Studio', async () => {
    render(<StatusReportPage />);

    await waitFor(() => expect(mockFetchPublicConfig).toHaveBeenCalled());

    const scopeInput = screen.getByPlaceholderText('e.g., checkout-api');
    fireEvent.change(scopeInput, { target: { value: 'checkout-api' } });

    fireEvent.click(screen.getByTestId('report-analyze-button'));

    await waitFor(() => {
      expect(screen.getByTestId('report-executive-summary').textContent).toContain(
        'Checkout API is experiencing elevated 5xx errors.'
      );
    });

    // Secret in alert summary must have been redacted before sending to backend.
    expect(mockAnalyzeStatus).toHaveBeenCalledTimes(1);
    const sentPayload = mockAnalyzeStatus.mock.calls[0][0];
    expect(sentPayload.evidence.alerts[0].summary).not.toContain('glsa_');

    // Click citation jump button
    fireEvent.click(screen.getByTestId('cite-ev-alert-1'));

    // Copy Markdown
    fireEvent.click(screen.getByText('Copy Markdown'));
    await waitFor(() => expect(mockCopyText).toHaveBeenCalledTimes(1));
    expect(mockCopyText.mock.calls[0][0]).toContain('# Operational Status & Incident Analysis Report');

    // Investigate in Studio handoff
    fireEvent.click(screen.getByTestId('report-investigate-studio'));
    expect(mockPush).toHaveBeenCalledTimes(1);
    const pushedUrl = mockPush.mock.calls[0][0] as string;
    expect(pushedUrl).toContain('/a/yuuki-llmstudio-app?');
    expect(pushedUrl).toContain('datasource=prom-main');
    expect(pushedUrl).toContain('scopeLabel=service');
    expect(pushedUrl).toContain('scopeValue=checkout-api');
    expect(pushedUrl).toContain('intent=Build+a+troubleshooting+dashboard');
  });

  test('preview-before-send gate holds LLM call until user explicitly confirms', async () => {
    render(<StatusReportPage />);
    await waitFor(() => expect(mockFetchPublicConfig).toHaveBeenCalled());

    fireEvent.click(screen.getByLabelText('Preview collected evidence before sending to LLM'));
    fireEvent.click(screen.getByTestId('report-analyze-button'));

    await waitFor(() => {
      expect(screen.getByTestId('report-confirm-send-button')).toBeTruthy();
    });
    expect(mockAnalyzeStatus).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId('report-confirm-send-button'));
    await waitFor(() => {
      expect(mockAnalyzeStatus).toHaveBeenCalledTimes(1);
      expect(screen.getByTestId('report-executive-summary')).toBeTruthy();
    });
  });

  test('dashboard + alerts flow fetches selected dashboard and collects metric evidence', async () => {
    mockFetchExistingDashboardByUid.mockResolvedValue({
      uid: 'dash-checkout',
      rawDashboard: {
        title: 'Checkout Overview',
        panels: [
          {
            id: 1,
            type: 'timeseries',
            title: 'Error Rate',
            datasource: { uid: 'prom-main', type: 'prometheus' },
            targets: [{ refId: 'A', expr: 'rate(http_errors_total[$__rate_interval])' }],
          },
        ],
      },
    });
    mockCollectMetrics.mockResolvedValue({
      metrics: [
        {
          id: 'ev-metric-1',
          panelTitle: 'Error Rate',
          seriesLabel: 'service=checkout-api',
          queryStatus: 'ok',
          current: { latest: 0.08, avg: 0.06, p95: 0.09, min: 0.01, max: 0.09, sampleCount: 60, missingRatio: 0 },
          trend: 'rising',
          thresholdBreached: 'critical',
          anomalyHint: 'degraded',
        },
      ],
      warnings: [],
      analyzedPanelCount: 1,
    });

    render(<StatusReportPage />);
    await waitFor(() => expect(mockListExistingDashboards).toHaveBeenCalled());

    const dashSelect = screen.getByLabelText('Dashboard (Optional — leave empty for Alerts-only)');
    fireEvent.change(dashSelect, { target: { value: 'dash-checkout' } });

    fireEvent.click(screen.getByTestId('report-analyze-button'));

    await waitFor(() => {
      expect(mockAnalyzeStatus).toHaveBeenCalledTimes(1);
    });
    expect(mockFetchExistingDashboardByUid).toHaveBeenCalledWith('dash-checkout', expect.any(AbortSignal));
    expect(mockCollectMetrics).toHaveBeenCalledTimes(1);
    const sentPayload = mockAnalyzeStatus.mock.calls[0][0];
    expect(sentPayload.dashboardContext?.uid).toBe('dash-checkout');
    expect(sentPayload.evidence.metrics).toHaveLength(1);
  });
});
