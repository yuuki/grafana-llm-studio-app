import React from 'react';
import { EmbeddedScene } from '@grafana/scenes';
import { Button, Spinner } from '@grafana/ui';
import { AllowedDatasourceOption } from '../../datasource/allowlist';
import { JobSnapshot } from '../../ir/types';
import { StudioBusyState, StudioRightTab, StudioTranslateFn } from './types';

export interface StudioPreviewPaneProps {
  t: StudioTranslateFn;
  rightTab: StudioRightTab;
  onChangeRightTab: (tab: StudioRightTab) => void;
  job: JobSnapshot | null;
  selectedDatasource: AllowedDatasourceOption | null;
  busyState: StudioBusyState;
  previewScene: EmbeddedScene | null;
  foundationJsonPreview: string;
  onValidateAndPreview: () => void;
}

export const StudioPreviewPane: React.FC<StudioPreviewPaneProps> = ({
  t,
  rightTab,
  onChangeRightTab,
  job,
  selectedDatasource,
  busyState,
  previewScene,
  foundationJsonPreview,
  onValidateAndPreview,
}) => (
  <div
    style={{
      display: 'flex',
      flexDirection: 'column',
      gap: 12,
      border: '1px solid #2c3235',
      borderRadius: 6,
      padding: 12,
      minHeight: 540,
    }}
  >
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
      <div style={{ display: 'flex', gap: 8 }}>
        <Button
          size="sm"
          variant={rightTab === 'preview' ? 'primary' : 'secondary'}
          onClick={() => onChangeRightTab('preview')}
        >
          {t('studio.rightPane.tabPreview')}
        </Button>
        <Button
          size="sm"
          variant={rightTab === 'ir' ? 'primary' : 'secondary'}
          onClick={() => onChangeRightTab('ir')}
        >
          {t('studio.rightPane.tabIr')}
        </Button>
        <Button
          size="sm"
          variant={rightTab === 'foundation' ? 'primary' : 'secondary'}
          onClick={() => onChangeRightTab('foundation')}
        >
          {t('studio.rightPane.tabFoundation')}
        </Button>
      </div>

      <Button
        variant="primary"
        onClick={onValidateAndPreview}
        disabled={!job?.currentIR || !selectedDatasource || busyState !== 'idle'}
      >
        {busyState === 'validating' ? (
          <>
            <Spinner inline /> {t('studio.rightPane.validating')}
          </>
        ) : (
          t('studio.rightPane.validateAndPreview')
        )}
      </Button>
    </div>

    {/* Keep Scenes container mounted via CSS display so tab switching NEVER unmounts/re-activates SceneQueryRunner (H-8). */}
    <div
      data-testid="studio-preview-pane"
      style={{ display: rightTab === 'preview' ? 'block' : 'none', minHeight: 460 }}
    >
      {previewScene ? (
        <previewScene.Component model={previewScene} />
      ) : (
        <div style={{ padding: 32, textAlign: 'center', color: '#9fa7b3' }}>
          {job?.currentIR
            ? t('studio.rightPane.draftReadyHint', { revision: job.currentIR.revision })
            : t('studio.rightPane.emptyHint')}
        </div>
      )}
    </div>

    {rightTab === 'ir' && (
      <pre
        data-testid="studio-ir-json"
        style={{
          margin: 0,
          padding: 12,
          background: '#111217',
          borderRadius: 4,
          overflow: 'auto',
          maxHeight: 520,
          fontSize: 12,
        }}
      >
        {job?.currentIR ? JSON.stringify(job.currentIR, null, 2) : t('studio.rightPane.noIrYet')}
      </pre>
    )}

    {rightTab === 'foundation' && (
      <pre
        data-testid="studio-foundation-json"
        style={{
          margin: 0,
          padding: 12,
          background: '#111217',
          borderRadius: 4,
          overflow: 'auto',
          maxHeight: 520,
          fontSize: 12,
        }}
      >
        {foundationJsonPreview || t('studio.rightPane.noFoundationYet')}
      </pre>
    )}
  </div>
);
