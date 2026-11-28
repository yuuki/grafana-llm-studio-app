import React from 'react';
import { Alert, Badge, Button } from '@grafana/ui';
import { TranslationKey } from '../../i18n';
import { AnalyzeStatusPayload } from '../../ir/types';

export interface EvidenceInspectorPaneProps {
  t: (key: TranslationKey, vars?: Record<string, string | number>) => string;
  collectedPayload: AnalyzeStatusPayload | null;
  pendingPayload: AnalyzeStatusPayload | null;
  redactionCount: number;
  highlightedEvidenceId: string | null;
  evidenceCardRefs: React.MutableRefObject<Record<string, HTMLDivElement | null>>;
  isBusy: boolean;
  onConfirmPreviewSend: () => void;
  onDiscardPreview: () => void;
}

export const EvidenceInspectorPane: React.FC<EvidenceInspectorPaneProps> = ({
  t,
  collectedPayload,
  pendingPayload,
  redactionCount,
  highlightedEvidenceId,
  evidenceCardRefs,
  isBusy,
  onConfirmPreviewSend,
  onDiscardPreview,
}) => (
  <div
    style={{
      border: '1px solid rgba(204, 204, 220, 0.2)',
      borderRadius: '4px',
      padding: '16px',
      display: 'flex',
      flexDirection: 'column',
      gap: '12px',
    }}
    data-testid="report-evidence-pane"
  >
    <h3 style={{ margin: 0 }}>{t('report.evidence.heading')}</h3>

    {!collectedPayload ? (
      <p style={{ opacity: 0.75 }}>{t('report.evidence.empty')}</p>
    ) : (
      <>
        <div style={{ fontSize: '12px', opacity: 0.85 }}>
          {t('report.evidence.summaryBadges', {
            alerts: collectedPayload.evidence.alerts.length,
            rules: collectedPayload.evidence.rules.length,
            metrics: collectedPayload.evidence.metrics.length,
            redacted: redactionCount,
          })}
        </div>

        {pendingPayload && (
          <Alert title={t('report.evidence.previewReady')} severity="info">
            <div style={{ display: 'flex', gap: '8px', marginTop: '8px' }}>
              <Button
                variant="primary"
                size="sm"
                onClick={onConfirmPreviewSend}
                disabled={isBusy}
                data-testid="report-confirm-send-button"
              >
                {t('report.controls.sendToLlmButton')}
              </Button>
              <Button variant="secondary" size="sm" onClick={onDiscardPreview} disabled={isBusy}>
                {t('report.controls.discardPreviewButton')}
              </Button>
            </div>
          </Alert>
        )}

        {(collectedPayload.collectionWarnings?.length ?? 0) > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.warningsHeading')}</h4>
            {collectedPayload.collectionWarnings!.map((w) => (
              <div
                key={w.id}
                ref={(el) => {
                  evidenceCardRefs.current[w.id] = el;
                }}
                tabIndex={-1}
                aria-current={highlightedEvidenceId === w.id ? 'true' : undefined}
                style={{
                  padding: '8px',
                  marginBottom: '6px',
                  borderRadius: '4px',
                  border:
                    highlightedEvidenceId === w.id
                      ? '2px solid #5794f2'
                      : '1px solid rgba(255, 152, 48, 0.4)',
                }}
              >
                <Badge text={w.id} color="orange" /> <strong style={{ marginLeft: '6px' }}>{w.code}</strong>
                <div style={{ fontSize: '12px', marginTop: '4px' }}>{w.message}</div>
              </div>
            ))}
          </div>
        )}

        {collectedPayload.evidence.alerts.length > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.alertsHeading')}</h4>
            {collectedPayload.evidence.alerts.map((a) => (
              <div
                key={a.id}
                ref={(el) => {
                  evidenceCardRefs.current[a.id] = el;
                }}
                tabIndex={-1}
                aria-current={highlightedEvidenceId === a.id ? 'true' : undefined}
                data-testid={`evidence-card-${a.id}`}
                style={{
                  padding: '8px',
                  marginBottom: '6px',
                  borderRadius: '4px',
                  border:
                    highlightedEvidenceId === a.id
                      ? '2px solid #5794f2'
                      : '1px solid rgba(204, 204, 220, 0.2)',
                }}
              >
                <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                  <Badge text={a.id} color="blue" />
                  <strong>{a.alertName}</strong>
                  <Badge text={a.state} color={a.state === 'active' ? 'red' : 'orange'} />
                  {a.severity && <Badge text={a.severity} color="orange" />}
                </div>
                {a.summary && <div style={{ fontSize: '12px', marginTop: '4px' }}>{a.summary}</div>}
              </div>
            ))}
          </div>
        )}

        {collectedPayload.evidence.rules.length > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.rulesHeading')}</h4>
            {collectedPayload.evidence.rules.map((r) => (
              <div
                key={r.id}
                ref={(el) => {
                  evidenceCardRefs.current[r.id] = el;
                }}
                style={{
                  padding: '8px',
                  marginBottom: '6px',
                  borderRadius: '4px',
                  border:
                    highlightedEvidenceId === r.id
                      ? '2px solid #5794f2'
                      : '1px solid rgba(204, 204, 220, 0.2)',
                }}
              >
                <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                  <Badge text={r.id} color="blue" />
                  <strong>{r.ruleName}</strong>
                  <Badge text={`${r.state}/${r.health}`} color={r.health === 'error' ? 'red' : 'orange'} />
                </div>
                {r.lastError && <div style={{ fontSize: '12px', marginTop: '4px' }}>{r.lastError}</div>}
              </div>
            ))}
          </div>
        )}

        {collectedPayload.evidence.metrics.length > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.metricsHeading')}</h4>
            {collectedPayload.evidence.metrics.map((m) => (
              <div
                key={m.id}
                ref={(el) => {
                  evidenceCardRefs.current[m.id] = el;
                }}
                data-testid={`evidence-card-${m.id}`}
                style={{
                  padding: '8px',
                  marginBottom: '6px',
                  borderRadius: '4px',
                  border:
                    highlightedEvidenceId === m.id
                      ? '2px solid #5794f2'
                      : '1px solid rgba(204, 204, 220, 0.2)',
                }}
              >
                <div style={{ display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' }}>
                  <Badge text={m.id} color="blue" />
                  <strong>{m.panelTitle}</strong>
                  <Badge
                    text={m.anomalyHint}
                    color={
                      m.anomalyHint === 'degraded' || m.anomalyHint === 'query_error'
                        ? 'red'
                        : m.anomalyHint === 'elevated'
                          ? 'orange'
                          : 'green'
                    }
                  />
                </div>
                {m.seriesLabel && <div style={{ fontSize: '11px', opacity: 0.75 }}>{m.seriesLabel}</div>}
                {m.current && (
                  <div style={{ fontSize: '12px', marginTop: '4px' }}>
                    avg={m.current.avg}, p95={m.current.p95}, max={m.current.max}
                    {m.deltaPctAvg !== undefined &&
                      ` (${t('report.evidence.deltaLabel', { delta: m.deltaPctAvg })})`}
                  </div>
                )}
                {m.errorText && <div style={{ fontSize: '12px', color: '#f2495c' }}>{m.errorText}</div>}
              </div>
            ))}
          </div>
        )}

        {(collectedPayload.evidence.statusNotes?.length ?? 0) > 0 && (
          <div>
            <h4 style={{ margin: '0 0 6px' }}>{t('report.evidence.statusNotesHeading')}</h4>
            {collectedPayload.evidence.statusNotes!.map((n) => (
              <div
                key={n.id}
                ref={(el) => {
                  evidenceCardRefs.current[n.id] = el;
                }}
                style={{
                  padding: '8px',
                  marginBottom: '6px',
                  borderRadius: '4px',
                  border:
                    highlightedEvidenceId === n.id
                      ? '2px solid #5794f2'
                      : '1px solid rgba(204, 204, 220, 0.2)',
                }}
              >
                <Badge text={n.id} color="green" /> <span style={{ marginLeft: '6px' }}>{n.message}</span>
              </div>
            ))}
          </div>
        )}
      </>
    )}
  </div>
);
