import React from 'react';
import { config, PluginPage } from '@grafana/runtime';
import { Alert, Spinner } from '@grafana/ui';
import { getKnownStageLabel } from '../components/shared/stageLabels';
import { EvidenceInspectorPane } from '../components/StatusReport/EvidenceInspectorPane';
import { ReportControlsPanel } from '../components/StatusReport/ReportControlsPanel';
import { StatusReportResultPane } from '../components/StatusReport/StatusReportResultPane';
import { useStatusReportWorkflow } from '../hooks/useStatusReportWorkflow';
import { useStudioTranslation } from '../i18n';

export const StatusReportPage: React.FC = () => {
  const { t } = useStudioTranslation();
  const currentOrgId = config?.bootData?.user?.orgId ?? 0;
  const wf = useStatusReportWorkflow(currentOrgId, t);

  const isBusy = wf.busyState !== 'idle';
  const stageText = getKnownStageLabel(wf.liveStage?.stage, t);

  return (
    <PluginPage>
      <div style={{ display: 'flex', flexDirection: 'column', gap: '16px', paddingBottom: '24px' }}>
        <div>
          <h2 style={{ margin: 0 }}>{t('report.pageTitle')}</h2>
          <p style={{ margin: '4px 0 0', opacity: 0.8 }}>{t('report.pageSubtitle')}</p>
        </div>

        {wf.publicCfg?.mockBanner && (
          <Alert title={t('studio.mockBanner.title')} severity="warning">
            {t('studio.mockBanner.body')}
          </Alert>
        )}

        {wf.uiError && (
          <Alert title={t('studio.alert.errorTitle')} severity="error" onRemove={() => wf.setUiError(null)}>
            {wf.uiError}
          </Alert>
        )}

        {wf.copyNotice && (
          <Alert title={wf.copyNotice} severity="info" onRemove={() => wf.setCopyNotice(null)} />
        )}

        <ReportControlsPanel
          t={t}
          publicCfg={wf.publicCfg}
          busyState={wf.busyState}
          dashboards={wf.dashboards}
          selectedDashboardUid={wf.selectedDashboardUid}
          onSelectDashboard={wf.setSelectedDashboardUid}
          allowedDatasources={wf.allowedDatasources}
          selectedDsUid={wf.selectedDsUid}
          onSelectDatasource={wf.setSelectedDsUid}
          windowVal={wf.windowVal}
          onSelectWindow={wf.setWindowVal}
          baselineMode={wf.baselineMode}
          onSelectBaselineMode={wf.setBaselineMode}
          scopeLabelName={wf.scopeLabelName}
          onSelectScopeLabelName={wf.setScopeLabelName}
          scopeLabelValue={wf.scopeLabelValue}
          onChangeScopeLabelValue={wf.setScopeLabelValue}
          reportLanguage={wf.reportLanguage}
          onSelectReportLanguage={wf.setReportLanguage}
          userFocus={wf.userFocus}
          onChangeUserFocus={wf.setUserFocus}
          includeOrgCritical={wf.includeOrgCritical}
          onChangeIncludeOrgCritical={wf.setIncludeOrgCritical}
          previewBeforeSend={wf.previewBeforeSend}
          onChangePreviewBeforeSend={wf.setPreviewBeforeSend}
          onCollectAndMaybeAnalyze={wf.handleCollectAndMaybeAnalyze}
          onCancel={wf.handleCancel}
        />

        {isBusy && (
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }} data-testid="report-progress">
            <Spinner />
            <span>
              {stageText ??
                (wf.busyState === 'collecting' ? t('report.controls.collecting') : t('report.controls.analyzing'))}
            </span>
          </div>
        )}

        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))',
            gap: '16px',
            alignItems: 'start',
          }}
        >
          <EvidenceInspectorPane
            t={t}
            collectedPayload={wf.collectedPayload}
            pendingPayload={wf.pendingPayload}
            redactionCount={wf.redactionCount}
            highlightedEvidenceId={wf.highlightedEvidenceId}
            evidenceCardRefs={wf.evidenceCardRefs}
            isBusy={isBusy}
            onConfirmPreviewSend={wf.handleConfirmPreviewSend}
            onDiscardPreview={wf.handleDiscardPreview}
          />

          <StatusReportResultPane
            t={t}
            report={wf.report}
            onJumpToEvidence={wf.handleJumpToEvidence}
            onCopyMarkdown={wf.handleCopyMarkdown}
            onDownloadMarkdown={wf.handleDownloadMarkdown}
            onExportJson={wf.handleExportJson}
            onInvestigateInStudio={wf.handleInvestigateInStudio}
          />
        </div>
      </div>
    </PluginPage>
  );
};
