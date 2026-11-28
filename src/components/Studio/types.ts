import { TranslationKey } from '../../i18n';
import { ImportPatchContext } from '../../import/importDashboard';
import { PanelValidationResult, ValidationCategory } from '../../ir/types';
import { NormalizedDashboardSpec } from '../../spec/normalize';

export type StudioBusyState =
  | 'idle'
  | 'generating'
  | 'importing'
  | 'patching'
  | 'validating'
  | 'repairing'
  | 'undoing'
  | 'saving'
  | 'cancelling';

export type StudioRightTab = 'preview' | 'ir' | 'foundation';

export type StudioTranslateFn = (key: TranslationKey, vars?: Record<string, string | number>) => string;

export interface PreviewMeta {
  jobId: string;
  revision: number;
}

export interface ValidatedPanelCacheEntry {
  rawQueryHash: string;
  varsFingerprint: string;
  category: ValidationCategory;
}

export interface PreservedImportMeta {
  resourceVersion?: string;
  folderUid?: string;
  preservedAnnotations?: Record<string, string>;
  preservedLabels?: Record<string, string>;
  patchContext?: ImportPatchContext;
}

export const VALIDATION_BADGE_COLOR: Record<ValidationCategory, 'green' | 'orange' | 'red' | 'blue'> = {
  ok: 'green',
  empty_result: 'blue',
  syntax_error: 'red',
  permission_error: 'red',
  timeout: 'orange',
  datasource_error: 'red',
};

export const VALIDATION_LABEL_KEYS: Record<ValidationCategory, TranslationKey> = {
  ok: 'studio.validation.category.ok',
  empty_result: 'studio.validation.category.empty_result',
  syntax_error: 'studio.validation.category.syntax_error',
  permission_error: 'studio.validation.category.permission_error',
  timeout: 'studio.validation.category.timeout',
  datasource_error: 'studio.validation.category.datasource_error',
};

export const BLOCKING_CATEGORIES = new Set<ValidationCategory>([
  'syntax_error',
  'permission_error',
  'timeout',
  'datasource_error',
]);

export function makeScopeKey(dsUid: string, labelName: string, labelValue: string): string {
  return `${dsUid}::${labelName}::${labelValue.trim()}`;
}

export function computeVariablesFingerprint(spec: NormalizedDashboardSpec): string {
  return JSON.stringify(
    spec.variables.map((v) => ({
      name: v.name,
      type: v.type,
      query: v.query,
      options: v.options,
      defaultValue: v.defaultValue,
      multi: v.multi,
      includeAll: v.includeAll,
    }))
  );
}

export function mergeValidationResults(
  prev: PanelValidationResult[],
  updated: PanelValidationResult[],
  spec: NormalizedDashboardSpec
): PanelValidationResult[] {
  const byId = new Map<string, PanelValidationResult>();
  for (const r of prev) {
    byId.set(r.panelId, r);
  }
  for (const r of updated) {
    byId.set(r.panelId, r);
  }
  return spec.panels
    .map((p) => byId.get(p.id))
    .filter((r): r is PanelValidationResult => Boolean(r));
}
