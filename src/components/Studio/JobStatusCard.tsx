import React from 'react';
import { Alert, Badge } from '@grafana/ui';
import { JobSnapshot } from '../../ir/types';
import { StudioTranslateFn } from './types';

export interface JobStatusCardProps {
  t: StudioTranslateFn;
  job: JobSnapshot;
}

export const JobStatusCard: React.FC<JobStatusCardProps> = ({ t, job }) => (
  <div
    data-testid="studio-job-status"
    style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}
  >
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
      <span>
        {t('studio.job.statusLabel')}{' '}
        <Badge text={job.status} color={job.status === 'failed' ? 'red' : 'blue'} />
      </span>
      {job.currentIR && (
        <span data-testid="studio-job-revision" style={{ fontSize: 12 }}>
          {t('studio.job.revision', { revision: job.currentIR.revision })}
        </span>
      )}
    </div>
    {job.currentIR?.variables && job.currentIR.variables.length > 0 && (
      <div data-testid="studio-ir-variables" style={{ fontSize: 12, color: '#9fa7b3' }}>
        {t('studio.job.variablesLabel')}{' '}
        {job.currentIR.variables.map((v) => `$${v.name} (${v.type})`).join(', ')}
      </div>
    )}
    {job.summary && <div style={{ fontSize: 13 }}>{job.summary}</div>}
    {job.warnings && job.warnings.length > 0 && (
      <div data-testid="studio-job-warnings" style={{ fontSize: 12, color: '#ff9830' }}>
        <strong>{t('studio.job.warningsLabel')}</strong> {job.warnings.join(' / ')}
      </div>
    )}
    {job.errorMessage && (
      <Alert title={t('studio.job.messageTitle')} severity="warning">
        {job.errorMessage}
      </Alert>
    )}
    {job.lastDiff && job.lastDiff.length > 0 && (
      <div data-testid="studio-job-diff" style={{ fontSize: 12 }}>
        <strong>{t('studio.job.diffTitle')}</strong>
        <ul style={{ margin: '4px 0', paddingLeft: 18 }}>
          {job.lastDiff.map((d, i) => (
            <li key={i}>
              <code>{d.panelId ?? 'dashboard'}</code>.{d.field}: {d.before ? `${d.before} → ` : ''}
              <strong>{d.after}</strong>
            </li>
          ))}
        </ul>
      </div>
    )}
    <div style={{ fontSize: 12, color: '#9fa7b3' }}>
      {t('studio.job.repairBudget', {
        used: job.repairAttemptsUsed,
        max: job.maxRepairAttempts,
      })}
    </div>
  </div>
);
