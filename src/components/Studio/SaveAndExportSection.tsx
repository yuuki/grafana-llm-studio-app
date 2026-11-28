import React from 'react';
import { Button, Field, Select } from '@grafana/ui';
import { JobSnapshot } from '../../ir/types';
import { StudioBusyState, StudioTranslateFn } from './types';

export interface SaveAndExportSectionProps {
  t: StudioTranslateFn;
  job: JobSnapshot;
  folderOptions: Array<{ label: string; value: string }>;
  selectedFolderUid: string;
  onSelectFolderUid: (uid: string) => void;
  canOptInUpdate: boolean;
  optInUpdateExisting: boolean;
  onChangeOptInUpdateExisting: (checked: boolean) => void;
  canSave: boolean;
  busyState: StudioBusyState;
  onSaveDashboard: () => void;
  onExportJson: () => void;
}

export const SaveAndExportSection: React.FC<SaveAndExportSectionProps> = ({
  t,
  job,
  folderOptions,
  selectedFolderUid,
  onSelectFolderUid,
  canOptInUpdate,
  optInUpdateExisting,
  onChangeOptInUpdateExisting,
  canSave,
  busyState,
  onSaveDashboard,
  onExportJson,
}) => (
  <div style={{ borderTop: '1px solid #2c3235', paddingTop: 10, display: 'flex', flexDirection: 'column', gap: 8 }}>
    <Field
      label={
        optInUpdateExisting && canOptInUpdate
          ? t('studio.save.folderLabelUpdateLocked')
          : t('studio.save.folderLabel')
      }
    >
      <Select
        options={folderOptions}
        value={
          optInUpdateExisting && canOptInUpdate ? job.importSource?.folderUid ?? '' : selectedFolderUid
        }
        onChange={(v) => onSelectFolderUid(v.value ?? '')}
        disabled={optInUpdateExisting && canOptInUpdate}
      />
    </Field>
    {canOptInUpdate && (
      <label
        data-testid="studio-optin-update-label"
        style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, cursor: 'pointer' }}
      >
        <input
          data-testid="studio-optin-update-checkbox"
          type="checkbox"
          checked={optInUpdateExisting}
          onChange={(e) => onChangeOptInUpdateExisting(e.currentTarget.checked)}
        />
        <span>
          {t('studio.save.optInUpdateLabel', {
            uid: job.importSource?.uid ?? '',
            rv: job.importSource?.resourceVersion ?? '',
          })}
        </span>
      </label>
    )}
    <div style={{ display: 'flex', gap: 8 }}>
      <Button onClick={onSaveDashboard} disabled={!canSave}>
        {busyState === 'saving'
          ? t('studio.save.saving')
          : optInUpdateExisting && canOptInUpdate
            ? t('studio.save.updateExistingDashboard')
            : t('studio.save.saveNewDashboard')}
      </Button>
      <Button variant="secondary" onClick={onExportJson} disabled={busyState !== 'idle'}>
        {t('studio.save.exportJson')}
      </Button>
    </div>
    {!canSave && job.currentIR && (
      <div style={{ fontSize: 12, color: '#9fa7b3' }}>
        {t('studio.save.requiresValidationHint', { revision: job.currentIR.revision })}
      </div>
    )}
  </div>
);
