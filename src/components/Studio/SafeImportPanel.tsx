import React from 'react';
import { Button, Field, Input, Select } from '@grafana/ui';
import { AllowedDatasourceOption } from '../../datasource/allowlist';
import { ExistingDashboardSummary, ImportConversionReport } from '../../import/importDashboard';
import { StudioBusyState, StudioTranslateFn } from './types';

export interface SafeImportPanelProps {
  t: StudioTranslateFn;
  selectedDsUid: string;
  selectedDatasource: AllowedDatasourceOption | null;
  busyState: StudioBusyState;
  showImportPanel: boolean;
  onToggleImportPanel: () => void;
  importUid: string;
  onChangeImportUid: (uid: string) => void;
  existingDashboards: ExistingDashboardSummary[];
  importReport: ImportConversionReport | null;
  onListExistingDashboards: () => void;
  onImportExistingDashboard: () => void;
}

export const SafeImportPanel: React.FC<SafeImportPanelProps> = ({
  t,
  selectedDsUid,
  selectedDatasource,
  busyState,
  showImportPanel,
  onToggleImportPanel,
  importUid,
  onChangeImportUid,
  existingDashboards,
  importReport,
  onListExistingDashboards,
  onImportExistingDashboard,
}) => (
  <>
    <div style={{ borderTop: '1px solid #2c3235', paddingTop: 8 }}>
      <Button size="sm" variant="secondary" onClick={onToggleImportPanel} disabled={!selectedDsUid}>
        {showImportPanel ? t('studio.import.closeButton') : t('studio.import.openButton')}
      </Button>
      {showImportPanel && (
        <div
          data-testid="studio-import-section"
          style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 8 }}
        >
          <Field label={t('studio.import.uidLabel')}>
            <Input
              value={importUid}
              onChange={(e) => onChangeImportUid(e.currentTarget.value)}
              placeholder={t('studio.import.uidPlaceholder')}
            />
          </Field>
          <div style={{ display: 'flex', gap: 8 }}>
            <Button
              size="sm"
              variant="secondary"
              onClick={onListExistingDashboards}
              disabled={busyState !== 'idle'}
            >
              {t('studio.import.listButton')}
            </Button>
            <Button
              size="sm"
              variant="primary"
              onClick={onImportExistingDashboard}
              disabled={!importUid.trim() || !selectedDatasource || busyState !== 'idle'}
            >
              {busyState === 'importing'
                ? t('studio.import.importing')
                : t('studio.import.importButton')}
            </Button>
          </div>
          {existingDashboards.length > 0 && (
            <Select
              options={existingDashboards.map((d) => ({
                label: `${d.title} (${d.uid})`,
                value: d.uid,
              }))}
              value={importUid}
              onChange={(v) => onChangeImportUid(v.value ?? '')}
              placeholder={t('studio.import.selectPlaceholder')}
            />
          )}
        </div>
      )}
    </div>

    {importReport && (
      <div
        data-testid="studio-import-report"
        style={{ fontSize: 12, border: '1px solid #2c3235', borderRadius: 4, padding: 8 }}
      >
        <strong>{t('studio.import.reportTitle')}</strong>{' '}
        {t('studio.import.reportCounts', {
          panels: importReport.importedPanelCount,
          variables: importReport.importedVariableCount,
        })}
        {importReport.skippedPanels.length > 0 && (
          <div>
            {t('studio.import.skippedPanels', {
              items: importReport.skippedPanels.map((s) => `${s.title} (${s.reason})`).join(', '),
            })}
          </div>
        )}
        {importReport.skippedVariables.length > 0 && (
          <div>
            {t('studio.import.skippedVariables', {
              items: importReport.skippedVariables.map((s) => `${s.title} (${s.reason})`).join(', '),
            })}
          </div>
        )}
        {importReport.warnings.length > 0 && (
          <div style={{ color: '#ff9830' }}>{importReport.warnings.join(' / ')}</div>
        )}
      </div>
    )}
  </>
);
