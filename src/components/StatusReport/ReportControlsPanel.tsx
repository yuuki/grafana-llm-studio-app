import React, { useMemo } from 'react';
import { Button, Checkbox, Field, Input, Select, TextArea } from '@grafana/ui';
import { AllowedDatasourceOption, SCOPE_LABELS } from '../../datasource/allowlist';
import { TranslationKey } from '../../i18n';
import { ExistingDashboardSummary } from '../../import/importDashboard';
import { BaselineMode, PublicConfig, ReportLanguage, ReportWindow } from '../../ir/types';

export type ReportBusyState = 'idle' | 'collecting' | 'analyzing' | 'cancelling';

export interface ReportControlsPanelProps {
  t: (key: TranslationKey, vars?: Record<string, string | number>) => string;
  publicCfg: PublicConfig | null;
  busyState: ReportBusyState;
  dashboards: ExistingDashboardSummary[];
  selectedDashboardUid: string;
  onSelectDashboard: (uid: string) => void;
  allowedDatasources: AllowedDatasourceOption[];
  selectedDsUid: string;
  onSelectDatasource: (uid: string) => void;
  windowVal: ReportWindow;
  onSelectWindow: (val: ReportWindow) => void;
  baselineMode: BaselineMode;
  onSelectBaselineMode: (val: BaselineMode) => void;
  scopeLabelName: string;
  onSelectScopeLabelName: (val: string) => void;
  scopeLabelValue: string;
  onChangeScopeLabelValue: (val: string) => void;
  reportLanguage: ReportLanguage;
  onSelectReportLanguage: (val: ReportLanguage) => void;
  userFocus: string;
  onChangeUserFocus: (val: string) => void;
  includeOrgCritical: boolean;
  onChangeIncludeOrgCritical: (checked: boolean) => void;
  previewBeforeSend: boolean;
  onChangePreviewBeforeSend: (checked: boolean) => void;
  onCollectAndMaybeAnalyze: () => void;
  onCancel: () => void;
}

export const ReportControlsPanel: React.FC<ReportControlsPanelProps> = ({
  t,
  publicCfg,
  busyState,
  dashboards,
  selectedDashboardUid,
  onSelectDashboard,
  allowedDatasources,
  selectedDsUid,
  onSelectDatasource,
  windowVal,
  onSelectWindow,
  baselineMode,
  onSelectBaselineMode,
  scopeLabelName,
  onSelectScopeLabelName,
  scopeLabelValue,
  onChangeScopeLabelValue,
  reportLanguage,
  onSelectReportLanguage,
  userFocus,
  onChangeUserFocus,
  includeOrgCritical,
  onChangeIncludeOrgCritical,
  previewBeforeSend,
  onChangePreviewBeforeSend,
  onCollectAndMaybeAnalyze,
  onCancel,
}) => {
  const isBusy = busyState !== 'idle';

  const dashboardOptions = useMemo(
    () => [
      { label: t('report.controls.dashboardAlertsOnly'), value: '' },
      ...dashboards.map((d) => ({
        label: d.folderTitle ? `${d.title} (${d.folderTitle})` : d.title,
        value: d.uid,
      })),
    ],
    [dashboards, t]
  );

  const dsOptions = useMemo(
    () => allowedDatasources.map((d) => ({ label: `${d.name} (${d.uid})`, value: d.uid })),
    [allowedDatasources]
  );

  return (
    <>
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
          gap: '12px',
          padding: '16px',
          border: '1px solid rgba(204, 204, 220, 0.2)',
          borderRadius: '4px',
        }}
      >
        <Field label={t('report.controls.dashboardLabel')}>
          <Select
            inputId="report-dashboard-select"
            options={dashboardOptions}
            value={selectedDashboardUid}
            onChange={(v) => onSelectDashboard(v.value ?? '')}
            disabled={isBusy}
          />
        </Field>

        <Field label={t('report.controls.datasourceLabel')}>
          <Select
            inputId="report-datasource-select"
            options={dsOptions}
            value={selectedDsUid}
            onChange={(v) => onSelectDatasource(v.value ?? '')}
            disabled={isBusy || dsOptions.length === 0}
          />
        </Field>

        <Field label={t('report.controls.windowLabel')}>
          <Select
            inputId="report-window-select"
            options={[
              { label: '15m', value: '15m' as ReportWindow },
              { label: '1h', value: '1h' as ReportWindow },
              { label: '6h', value: '6h' as ReportWindow },
              { label: '24h', value: '24h' as ReportWindow },
            ]}
            value={windowVal}
            onChange={(v) => {
              if (v.value) {
                onSelectWindow(v.value);
              }
            }}
            disabled={isBusy}
          />
        </Field>

        <Field label={t('report.controls.baselineLabel')}>
          <Select
            inputId="report-baseline-select"
            options={[
              { label: t('report.controls.baselinePreceding'), value: 'preceding' as BaselineMode },
              { label: t('report.controls.baseline24h'), value: '24h_ago' as BaselineMode },
              { label: t('report.controls.baselineNone'), value: 'none' as BaselineMode },
            ]}
            value={baselineMode}
            onChange={(v) => {
              if (v.value) {
                onSelectBaselineMode(v.value);
              }
            }}
            disabled={isBusy}
          />
        </Field>

        <Field label={t('report.controls.scopeLabel')}>
          <Select
            inputId="report-scope-label-select"
            options={SCOPE_LABELS.map((l) => ({ label: l, value: l }))}
            value={scopeLabelName}
            onChange={(v) => {
              if (v.value) {
                onSelectScopeLabelName(v.value);
              }
            }}
            disabled={isBusy}
          />
        </Field>

        <Field label={t('report.controls.scopeValueLabel')}>
          <Input
            id="report-scope-value-input"
            value={scopeLabelValue}
            placeholder={t('report.controls.scopeValuePlaceholder')}
            onChange={(e) => onChangeScopeLabelValue(e.currentTarget.value)}
            disabled={isBusy}
          />
        </Field>

        <Field label={t('report.controls.languageLabel')}>
          <Select
            inputId="report-language-select"
            options={[
              { label: t('report.controls.languageJa'), value: 'ja' as ReportLanguage },
              { label: t('report.controls.languageEn'), value: 'en' as ReportLanguage },
            ]}
            value={reportLanguage}
            onChange={(v) => {
              if (v.value) {
                onSelectReportLanguage(v.value);
              }
            }}
            disabled={isBusy}
          />
        </Field>

        <Field label={t('report.controls.focusLabel')}>
          <TextArea
            id="report-focus-input"
            rows={2}
            value={userFocus}
            placeholder={t('report.controls.focusPlaceholder')}
            onChange={(e) => onChangeUserFocus(e.currentTarget.value)}
            disabled={isBusy}
          />
        </Field>
      </div>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '16px', alignItems: 'center' }}>
        <Checkbox
          label={t('report.controls.includeOrgCritical')}
          value={includeOrgCritical}
          onChange={(e) => onChangeIncludeOrgCritical(e.currentTarget.checked)}
          disabled={isBusy}
        />
        <Checkbox
          label={t('report.controls.previewBeforeSend')}
          value={previewBeforeSend}
          onChange={(e) => onChangePreviewBeforeSend(e.currentTarget.checked)}
          disabled={isBusy}
        />
        <div style={{ display: 'flex', gap: '8px', marginLeft: 'auto' }}>
          <Button
            variant="primary"
            onClick={onCollectAndMaybeAnalyze}
            disabled={isBusy || !publicCfg}
            data-testid="report-analyze-button"
          >
            {busyState === 'collecting'
              ? t('report.controls.collecting')
              : busyState === 'analyzing'
                ? t('report.controls.analyzing')
                : previewBeforeSend
                  ? t('report.controls.collectPreviewButton')
                  : t('report.controls.analyzeButton')}
          </Button>
          {isBusy && (
            <Button variant="destructive" onClick={onCancel} disabled={busyState === 'cancelling'}>
              {busyState === 'cancelling' ? t('report.controls.cancelling') : t('report.controls.cancelButton')}
            </Button>
          )}
        </div>
      </div>
    </>
  );
};
