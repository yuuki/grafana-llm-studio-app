import React from 'react';
import { Badge, Button, Field, Select, Spinner, TextArea } from '@grafana/ui';
import { JobSnapshot, JobStageEvent } from '../../ir/types';
import { getKnownStageLabel } from '../shared/stageLabels';
import { StudioBusyState, StudioTranslateFn } from './types';

export interface PromptAndActionsSectionProps {
  t: StudioTranslateFn;
  job: JobSnapshot | null;
  editPanelId: string;
  onChangeEditPanelId: (val: string) => void;
  promptText: string;
  onChangePromptText: (val: string) => void;
  busyState: StudioBusyState;
  isCatalogFresh: boolean;
  canCancel: boolean;
  liveStage: JobStageEvent | null;
  sseFallbackNotice: boolean;
  onGenerateOrPatch: () => void;
  onCancel: () => void;
  onUndo: () => void;
}

export const PromptAndActionsSection: React.FC<PromptAndActionsSectionProps> = ({
  t,
  job,
  editPanelId,
  onChangeEditPanelId,
  promptText,
  onChangePromptText,
  busyState,
  isCatalogFresh,
  canCancel,
  liveStage,
  sseFallbackNotice,
  onGenerateOrPatch,
  onCancel,
  onUndo,
}) => (
  <>
    {job?.currentIR && (
      <Field label={t('studio.edit.label')}>
        <Select
          options={[
            { label: t('studio.edit.autoPatch'), value: '' },
            ...job.currentIR.panels.map((p) => ({
              label: t('studio.edit.panelPatch', { title: p.title, id: p.id }),
              value: p.id,
            })),
            { label: t('studio.edit.newDraft'), value: '__new__' },
          ]}
          value={editPanelId}
          onChange={(v) => onChangeEditPanelId(v.value ?? '')}
        />
      </Field>
    )}

    <Field label={t('studio.prompt.label')}>
      <TextArea
        rows={3}
        value={promptText}
        onChange={(e) => onChangePromptText(e.currentTarget.value)}
        placeholder={t('studio.prompt.placeholder')}
      />
    </Field>

    <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
      <Button onClick={onGenerateOrPatch} disabled={busyState !== 'idle' || !isCatalogFresh}>
        {busyState === 'generating' || busyState === 'patching'
          ? t('studio.actions.generating')
          : job?.currentIR && editPanelId !== '__new__'
            ? t('studio.actions.applyPatch')
            : t('studio.actions.generateDraft')}
      </Button>
      {canCancel && (
        <Button variant="destructive" size="sm" onClick={onCancel}>
          {t('studio.actions.cancel')}
        </Button>
      )}
      {job?.canUndo && busyState === 'idle' && (
        <Button variant="secondary" size="sm" onClick={onUndo}>
          {t('studio.actions.undo')}
        </Button>
      )}
    </div>

    {liveStage && (busyState === 'generating' || busyState === 'patching' || busyState === 'repairing') && (
      <div
        data-testid="studio-sse-progress"
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          fontSize: 12,
          padding: '6px 8px',
          border: '1px solid #2c3235',
          borderRadius: 4,
        }}
      >
        <Spinner inline />
        <span>{t('studio.progress.label')}</span>
        <Badge text={liveStage.stage} color="blue" />
        <span style={{ color: '#9fa7b3' }}>
          {getKnownStageLabel(liveStage.stage, t) ?? liveStage.stage}
        </span>
      </div>
    )}

    {sseFallbackNotice && (
      <div data-testid="studio-sse-fallback-notice" style={{ fontSize: 11, color: '#9fa7b3' }}>
        {t('studio.progress.fallbackNotice')}
      </div>
    )}
  </>
);
