import React from 'react';
import { Badge, Button } from '@grafana/ui';
import { TranslationKey } from '../../i18n';
import { ReportOverallStatus, StatusReportIR } from '../../ir/types';

const STATUS_BADGE_COLOR: Record<ReportOverallStatus, 'green' | 'orange' | 'red' | 'blue'> = {
  healthy: 'green',
  degraded: 'orange',
  critical: 'red',
  insufficient_data: 'blue',
};

const SEVERITY_BADGE_COLOR: Record<'critical' | 'warning' | 'info', 'red' | 'orange' | 'blue'> = {
  critical: 'red',
  warning: 'orange',
  info: 'blue',
};

export interface StatusReportResultPaneProps {
  t: (key: TranslationKey, vars?: Record<string, string | number>) => string;
  report: StatusReportIR | null;
  onJumpToEvidence: (evidenceId: string) => void;
  onCopyMarkdown: () => void;
  onDownloadMarkdown: () => void;
  onExportJson: () => void;
  onInvestigateInStudio: () => void;
}

export const StatusReportResultPane: React.FC<StatusReportResultPaneProps> = ({
  t,
  report,
  onJumpToEvidence,
  onCopyMarkdown,
  onDownloadMarkdown,
  onExportJson,
  onInvestigateInStudio,
}) => (
  <div
    style={{
      border: '1px solid rgba(204, 204, 220, 0.2)',
      borderRadius: '4px',
      padding: '16px',
      display: 'flex',
      flexDirection: 'column',
      gap: '14px',
    }}
    data-testid="report-result-pane"
  >
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '8px' }}>
      <h3 style={{ margin: 0 }}>{t('report.result.heading')}</h3>
      {report && (
        <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap' }}>
          <Button variant="secondary" size="sm" onClick={onCopyMarkdown} data-testid="report-copy-md">
            {t('report.result.copyMarkdown')}
          </Button>
          <Button variant="secondary" size="sm" onClick={onDownloadMarkdown}>
            {t('report.result.downloadMarkdown')}
          </Button>
          <Button variant="secondary" size="sm" onClick={onExportJson}>
            {t('report.result.exportJson')}
          </Button>
          {report.suggestedInvestigationIntent && (
            <Button
              variant="primary"
              size="sm"
              onClick={onInvestigateInStudio}
              data-testid="report-investigate-studio"
            >
              {t('report.result.investigateInStudio')}
            </Button>
          )}
        </div>
      )}
    </div>

    {!report ? (
      <p style={{ opacity: 0.75 }}>{t('report.result.empty')}</p>
    ) : (
      <>
        <div style={{ display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' }}>
          <span>{t('report.result.overallStatus')}:</span>
          <Badge
            text={t(`report.status.${report.overallStatus}` as TranslationKey)}
            color={STATUS_BADGE_COLOR[report.overallStatus]}
          />
          <span>{t('report.result.confidence')}:</span>
          <Badge text={report.confidence} color="blue" />
        </div>

        <div>
          <h4 style={{ margin: '0 0 6px' }}>{t('report.result.executiveSummary')}</h4>
          <p style={{ margin: 0 }} data-testid="report-executive-summary">
            {report.executiveSummary}
          </p>
        </div>

        <div>
          <h4 style={{ margin: '0 0 6px' }}>{t('report.result.findings')}</h4>
          {report.findings.length === 0 ? (
            <p style={{ margin: 0, opacity: 0.75 }}>{t('report.result.noFindings')}</p>
          ) : (
            report.findings.map((f) => (
              <div
                key={f.id}
                style={{
                  padding: '10px',
                  marginBottom: '8px',
                  borderRadius: '4px',
                  border: '1px solid rgba(204, 204, 220, 0.2)',
                }}
              >
                <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                  <Badge text={f.severity} color={SEVERITY_BADGE_COLOR[f.severity]} />
                  <Badge text={f.category} color="blue" />
                  <strong>{f.title}</strong>
                  <span style={{ fontSize: '12px', opacity: 0.7 }}>({f.id})</span>
                </div>
                <p style={{ margin: '6px 0' }}>{f.detail}</p>
                <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }}>
                  {f.evidenceIds.map((evId) => (
                    <Button
                      key={evId}
                      variant="secondary"
                      size="sm"
                      onClick={() => onJumpToEvidence(evId)}
                      data-testid={`cite-${evId}`}
                    >
                      {evId}
                    </Button>
                  ))}
                </div>
              </div>
            ))
          )}
        </div>

        {report.timeline.length > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.result.timeline')}</h4>
            {report.timeline.map((item, idx) => (
              <div key={idx} style={{ display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '6px', flexWrap: 'wrap' }}>
                <code>{item.timestamp}</code>
                <span>{item.event}</span>
                {item.evidenceIds.map((evId) => (
                  <Button key={evId} variant="secondary" size="sm" onClick={() => onJumpToEvidence(evId)}>
                    {evId}
                  </Button>
                ))}
              </div>
            ))}
          </div>
        )}

        {report.hypotheses.length > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.result.hypotheses')}</h4>
            {report.hypotheses.map((h, idx) => (
              <div
                key={idx}
                style={{
                  padding: '10px',
                  marginBottom: '8px',
                  borderRadius: '4px',
                  border: '1px solid rgba(204, 204, 220, 0.2)',
                }}
              >
                <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                  <Badge text={h.likelihood} color="orange" />
                  <strong>{h.title}</strong>
                </div>
                <p style={{ margin: '6px 0' }}>{h.rationale}</p>
                {h.verificationSteps.length > 0 && (
                  <div style={{ margin: '4px 0 6px' }}>
                    <div style={{ fontSize: '12px', fontWeight: 600 }}>{t('report.result.verificationSteps')}</div>
                    <ul style={{ margin: '4px 0 0 18px' }}>
                      {h.verificationSteps.map((step, sIdx) => (
                        <li key={sIdx}>{step}</li>
                      ))}
                    </ul>
                  </div>
                )}
                <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }}>
                  {h.evidenceIds.map((evId) => (
                    <Button key={evId} variant="secondary" size="sm" onClick={() => onJumpToEvidence(evId)}>
                      {evId}
                    </Button>
                  ))}
                </div>
              </div>
            ))}
          </div>
        )}

        {report.recommendedActions.length > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.result.recommendedActions')}</h4>
            <ul style={{ margin: '0 0 0 18px' }}>
              {report.recommendedActions.map((act, idx) => (
                <li key={idx} style={{ marginBottom: '4px' }}>
                  <Badge text={act.priority} color={act.priority === 'immediate' ? 'red' : 'blue'} />{' '}
                  <span>{act.action}</span>
                  {act.relatedFindingIds.length > 0 && (
                    <span style={{ fontSize: '12px', opacity: 0.7 }}>
                      {' '}
                      ({act.relatedFindingIds.join(', ')})
                    </span>
                  )}
                </li>
              ))}
            </ul>
          </div>
        )}

        {report.dataGaps.length > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.result.dataGaps')}</h4>
            <ul style={{ margin: '0 0 0 18px' }}>
              {report.dataGaps.map((gap, idx) => (
                <li key={idx}>{gap}</li>
              ))}
            </ul>
          </div>
        )}

        {report.suggestedInvestigationIntent && (
          <div style={{ fontSize: '12px', opacity: 0.85 }}>
            <strong>{t('report.result.suggestedIntentLabel')}</strong> {report.suggestedInvestigationIntent}
          </div>
        )}
      </>
    )}
  </div>
);
