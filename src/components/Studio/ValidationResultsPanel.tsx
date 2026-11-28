import React from 'react';
import { Badge, Button } from '@grafana/ui';
import { JobSnapshot, PanelValidationResult } from '../../ir/types';
import {
  StudioBusyState,
  StudioTranslateFn,
  VALIDATION_BADGE_COLOR,
  VALIDATION_LABEL_KEYS,
} from './types';

export interface ValidationResultsPanelProps {
  t: StudioTranslateFn;
  validationResults: PanelValidationResult[];
  syntaxErrorCount: number;
  job: JobSnapshot | null;
  busyState: StudioBusyState;
  isCatalogFresh: boolean;
  onRepairSyntaxErrors: () => void;
}

export const ValidationResultsPanel: React.FC<ValidationResultsPanelProps> = ({
  t,
  validationResults,
  syntaxErrorCount,
  job,
  busyState,
  isCatalogFresh,
  onRepairSyntaxErrors,
}) => (
  <div
    data-testid="studio-validation-results"
    style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }}
  >
    <strong>{t('studio.validation.heading')}</strong>
    {validationResults.map((r) => (
      <div
        key={r.panelId}
        data-testid={`studio-validation-row-${r.panelId}`}
        style={{ fontSize: 12, display: 'flex', flexDirection: 'column', gap: 2 }}
      >
        <div style={{ display: 'flex', justifyContent: 'space-between' }}>
          <code>{r.panelId}</code>
          <Badge text={t(VALIDATION_LABEL_KEYS[r.category])} color={VALIDATION_BADGE_COLOR[r.category]} />
        </div>
        {r.errorText && <div style={{ color: '#ff7383' }}>{r.errorText}</div>}
      </div>
    ))}
    {syntaxErrorCount > 0 && job && job.repairAttemptsUsed < job.maxRepairAttempts && (
      <>
        <Button
          size="sm"
          variant="secondary"
          onClick={onRepairSyntaxErrors}
          disabled={busyState !== 'idle' || !isCatalogFresh}
        >
          {busyState === 'repairing'
            ? t('studio.validation.repairing')
            : t('studio.validation.repairSyntaxOnly')}
        </Button>
        {!isCatalogFresh && (
          <div style={{ fontSize: 11, color: '#9fa7b3' }}>
            {t('studio.validation.repairCatalogRequiredHint')}
          </div>
        )}
      </>
    )}
  </div>
);
