import React from 'react';
import { Badge, Button, Field, Input, Select } from '@grafana/ui';
import { AllowedDatasourceOption, SCOPE_LABELS } from '../../datasource/allowlist';
import { QueryKind } from '../../ir/types';
import { StudioTranslateFn } from './types';

export interface DatasourceScopeSelectorProps {
  t: StudioTranslateFn;
  allowedDatasources: AllowedDatasourceOption[];
  selectedDsUid: string;
  onSelectDatasource: (uid: string) => void;
  selectedDatasource: AllowedDatasourceOption | null;
  activeDialect: { kind: QueryKind; pinned: boolean };
  scopeLabelName: string;
  onSelectScopeLabelName: (val: string) => void;
  scopeLabelValue: string;
  onChangeScopeLabelValue: (val: string) => void;
  scopeLabelValues: string[];
  loadingCatalog: boolean;
  catalogCount: number;
  catalogTruncated: boolean;
  isCatalogFresh: boolean;
  onLoadScopeValues: () => void;
  onLoadScopedCatalog: () => void;
}

export const DatasourceScopeSelector: React.FC<DatasourceScopeSelectorProps> = ({
  t,
  allowedDatasources,
  selectedDsUid,
  onSelectDatasource,
  selectedDatasource,
  activeDialect,
  scopeLabelName,
  onSelectScopeLabelName,
  scopeLabelValue,
  onChangeScopeLabelValue,
  scopeLabelValues,
  loadingCatalog,
  catalogCount,
  catalogTruncated,
  isCatalogFresh,
  onLoadScopeValues,
  onLoadScopedCatalog,
}) => (
  <>
    <Field label={t('studio.datasource.label')}>
      <Select
        options={allowedDatasources.map((d) => ({ label: `${d.name} (${d.uid})`, value: d.uid }))}
        value={selectedDsUid}
        onChange={(v) => onSelectDatasource(v.value ?? '')}
        placeholder={t('studio.datasource.placeholder')}
      />
    </Field>

    {selectedDatasource && (
      <div
        data-testid="studio-active-dialect"
        style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, color: '#9fa7b3' }}
      >
        <span>{t('studio.datasource.dialectLabel')}</span>
        <Badge
          text={activeDialect.kind === 'metricsql' ? 'MetricsQL' : 'PromQL'}
          color={activeDialect.kind === 'metricsql' ? 'purple' : 'blue'}
        />
        <span>
          {activeDialect.pinned ? t('studio.datasource.dialectPinned') : t('studio.datasource.dialectAuto')}
        </span>
      </div>
    )}

    <div style={{ display: 'grid', gridTemplateColumns: '120px 1fr', gap: 8 }}>
      <Field label={t('studio.scope.labelName')}>
        <Select
          options={SCOPE_LABELS.map((l) => ({ label: l, value: l }))}
          value={scopeLabelName}
          onChange={(v) => onSelectScopeLabelName(v.value ?? 'service')}
        />
      </Field>
      <Field label={t('studio.scope.labelValue')}>
        <Input
          value={scopeLabelValue}
          onChange={(e) => onChangeScopeLabelValue(e.currentTarget.value)}
          placeholder={t('studio.scope.placeholder')}
        />
      </Field>
    </div>

    <div style={{ display: 'flex', gap: 8 }}>
      <Button size="sm" variant="secondary" onClick={onLoadScopeValues} disabled={!selectedDsUid}>
        {t('studio.scope.fetchValues')}
      </Button>
      <Button
        size="sm"
        variant="secondary"
        onClick={onLoadScopedCatalog}
        disabled={!selectedDsUid || !scopeLabelValue.trim() || loadingCatalog}
      >
        {loadingCatalog ? t('studio.catalog.fetching') : t('studio.catalog.fetch')}
      </Button>
    </div>

    {scopeLabelValues.length > 0 && (
      <Select
        options={scopeLabelValues.map((v) => ({ label: v, value: v }))}
        value={scopeLabelValue}
        onChange={(v) => onChangeScopeLabelValue(v.value ?? '')}
        placeholder={t('studio.scope.selectValuePlaceholder')}
      />
    )}

    <div style={{ fontSize: 12, color: '#9fa7b3' }}>
      {t('studio.catalog.loadedPrefix')}{' '}
      <strong data-testid="studio-catalog-count">{catalogCount}</strong>{' '}
      {t('studio.catalog.loadedUnit')}
      {catalogTruncated ? ` ${t('studio.catalog.truncatedSuffix')}` : ''}
      {catalogCount > 0 && !isCatalogFresh ? ` ${t('studio.catalog.staleSuffix')}` : ''}
    </div>
  </>
);
