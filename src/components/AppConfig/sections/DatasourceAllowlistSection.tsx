import React from 'react';
import { Field, FieldSet, Input, Select } from '@grafana/ui';
import { QueryKind } from '../../../ir/types';
import { TranslateFn } from '../validation';

export interface DatasourceAllowlistSectionProps {
  t: TranslateFn;
  allowedDsText: string;
  onAllowedDsTextChange: (value: string) => void;
  parsedAllowedUids: string[];
  datasourceDialects: Record<string, QueryKind | ''>;
  onDialectChange: (uid: string, dialect: QueryKind | '') => void;
}

export const DatasourceAllowlistSection: React.FC<DatasourceAllowlistSectionProps> = ({
  t,
  allowedDsText,
  onAllowedDsTextChange,
  parsedAllowedUids,
  datasourceDialects,
  onDialectChange,
}) => (
  <FieldSet label={t('appConfig.datasource.fieldset')}>
    <Field
      label={t('appConfig.datasource.allowedUidsLabel')}
      description={t('appConfig.datasource.allowedUidsDescription')}
    >
      <Input
        value={allowedDsText}
        onChange={(e) => onAllowedDsTextChange(e.currentTarget.value)}
        placeholder="prom-main"
      />
    </Field>

    {parsedAllowedUids.length > 0 && (
      <Field
        label={t('appConfig.datasource.dialectsLabel')}
        description={t('appConfig.datasource.dialectsDescription')}
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          {parsedAllowedUids.map((uid) => (
            <div
              key={uid}
              style={{ display: 'grid', gridTemplateColumns: '180px 1fr', gap: 8, alignItems: 'center' }}
            >
              <code style={{ fontSize: 12 }}>{uid}</code>
              <Select
                aria-label={`Query dialect for ${uid}`}
                options={[
                  { label: t('appConfig.datasource.dialectAuto'), value: '' },
                  { label: t('appConfig.datasource.dialectPromql'), value: 'promql' },
                  { label: t('appConfig.datasource.dialectMetricsql'), value: 'metricsql' },
                ]}
                value={datasourceDialects[uid] ?? ''}
                onChange={(v) => {
                  const nextVal = (v.value ?? '') as QueryKind | '';
                  onDialectChange(uid, nextVal);
                }}
              />
            </div>
          ))}
        </div>
      </Field>
    )}
  </FieldSet>
);
