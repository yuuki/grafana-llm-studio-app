import React from 'react';
import { Alert } from '@grafana/ui';
import { PublicConfig } from '../../ir/types';
import { SaveDashboardResult } from '../../save/saveDashboard';
import { StudioTranslateFn } from './types';

export interface StudioAlertsBannerProps {
  t: StudioTranslateFn;
  publicCfg: PublicConfig | null;
  handoffFromReport: boolean;
  onDismissHandoff: () => void;
  uiError: string | null;
  onDismissError: () => void;
  savedResult: SaveDashboardResult | null;
}

export const StudioAlertsBanner: React.FC<StudioAlertsBannerProps> = ({
  t,
  publicCfg,
  handoffFromReport,
  onDismissHandoff,
  uiError,
  onDismissError,
  savedResult,
}) => (
  <>
    {publicCfg?.mockBanner && (
      <div data-testid="studio-mock-banner">
        <Alert title={t('studio.mockBanner.title')} severity="warning">
          {t('studio.mockBanner.body')}
        </Alert>
      </div>
    )}

    {handoffFromReport && (
      <div data-testid="studio-handoff-alert">
        <Alert title={t('studio.progress.handoffNotice')} severity="info" onRemove={onDismissHandoff} />
      </div>
    )}

    {uiError && (
      <div data-testid="studio-error-alert">
        <Alert title={t('studio.alert.errorTitle')} severity="error" onRemove={onDismissError}>
          {uiError}
        </Alert>
      </div>
    )}

    {savedResult && (
      <div data-testid="studio-saved-alert">
        <Alert
          title={
            savedResult.mode === 'updated'
              ? t('studio.savedAlert.updatedTitle')
              : t('studio.savedAlert.title')
          }
          severity="success"
        >
          {t('studio.savedAlert.uidPrefix')}{' '}
          <code data-testid="studio-saved-uid">{savedResult.uid}</code>{' '}
          {t('studio.savedAlert.revision', { revision: savedResult.revision })} —{' '}
          <a href={savedResult.url} target="_blank" rel="noreferrer">
            {t('studio.savedAlert.openLink')}
          </a>
        </Alert>
      </div>
    )}
  </>
);
