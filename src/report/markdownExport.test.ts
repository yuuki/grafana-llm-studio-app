import { setStudioLocale } from '../i18n';
import { StatusReportIR } from '../ir/reportTypes';
import { copyTextWithFallback, escapeMarkdownInline, formatStatusReportMarkdown } from './markdownExport';

const SAMPLE_REPORT: StatusReportIR = {
  schemaVersion: '1.0',
  overallStatus: 'degraded',
  confidence: 'high',
  executiveSummary: 'Checkout API 5xx rate spiked [click](http://bad) \n# injected',
  findings: [
    {
      id: 'finding-1',
      severity: 'critical',
      category: 'alert',
      title: 'HighErrorRate *firing*',
      detail: 'Error rate exceeded 5% on checkout-api.',
      evidenceIds: ['ev-alert-1', 'ev-metric-1'],
    },
  ],
  timeline: [
    {
      timestamp: '2026-11-27T00:10:00Z',
      event: 'HighErrorRate fired',
      evidenceIds: ['ev-alert-1'],
    },
  ],
  hypotheses: [
    {
      title: 'Upstream timeout',
      likelihood: 'high',
      rationale: 'Error spike aligns with p95 latency increase.',
      evidenceIds: ['ev-alert-1', 'ev-metric-1'],
      verificationSteps: ['Inspect payment gateway logs'],
    },
  ],
  recommendedActions: [
    {
      priority: 'immediate',
      action: 'Check recent rollout',
      relatedFindingIds: ['finding-1'],
    },
  ],
  dataGaps: ['External Alertmanager not inspected'],
};

describe('report/markdownExport', () => {
  afterEach(() => {
    setStudioLocale('en');
  });

  test('escapes Markdown control chars and flattens newlines against injection', () => {
    expect(escapeMarkdownInline('Hello [link](url)\n# Heading *bold*')).toBe(
      'Hello \\[link\\]\\(url\\) \\# Heading \\*bold\\*'
    );
  });

  test('uses reportLanguage headings independent of active Grafana UI locale', () => {
    setStudioLocale('en');
    const jaMd = formatStatusReportMarkdown(SAMPLE_REPORT, 'ja', {
      window: '1h',
      baselineMode: 'preceding',
      dashboardTitle: 'Checkout Overview',
      scopeText: 'service=checkout-api',
      generatedAt: '2026-11-27T00:15:00Z',
    });

    expect(jaMd).toContain('# 運用状況・障害分析レポート');
    expect(jaMd).toContain('## エグゼクティブサマリ');
    expect(jaMd).toContain('## 検出事項');
    expect(jaMd).toContain('## 推奨アクション');
    expect(jaMd).toContain('`ev-alert-1`, `ev-metric-1`');
    expect(jaMd).toContain('\\[click\\]\\(http://bad\\)');
    expect(jaMd).not.toContain('\n# injected');

    setStudioLocale('ja');
    const enMd = formatStatusReportMarkdown(SAMPLE_REPORT, 'en', {
      window: '1h',
      baselineMode: 'preceding',
    });
    expect(enMd).toContain('# Operational Status & Incident Analysis Report');
    expect(enMd).toContain('## Executive Summary');
  });

  test('copyTextWithFallback uses navigator.clipboard and falls back to execCommand when unavailable', async () => {
    const writeText = jest.fn().mockResolvedValueOnce(undefined);
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
    expect(await copyTextWithFallback('hello')).toBe(true);
    expect(writeText).toHaveBeenCalledWith('hello');

    Object.defineProperty(navigator, 'clipboard', {
      value: undefined,
      configurable: true,
    });
    const execCommand = jest.fn().mockReturnValueOnce(true);
    document.execCommand = execCommand;
    expect(await copyTextWithFallback('fallback')).toBe(true);
    expect(execCommand).toHaveBeenCalledWith('copy');
  });
});
