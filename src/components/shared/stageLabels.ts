import { TranslationKey } from '../../i18n';
import { JobStatus } from '../../ir/types';

export const STAGE_LABEL_KEYS: Record<JobStatus, TranslationKey> = {
  generating: 'studio.progress.stage.generating',
  repairing: 'studio.progress.stage.repairing',
  validating_ir: 'studio.progress.stage.validating_ir',
  analyzing: 'studio.progress.stage.analyzing',
  validating_report: 'studio.progress.stage.validating_report',
  awaiting_preview: 'studio.progress.stage.awaiting_preview',
  ready: 'studio.progress.stage.ready',
  insufficient_metrics: 'studio.progress.stage.insufficient_metrics',
  cancelled: 'studio.progress.stage.cancelled',
  failed: 'studio.progress.stage.failed',
};

/**
 * Prototype-safe stage label lookup so unexpected backend stage names (e.g. "__proto__",
 * "toString", "constructor") never resolve Object.prototype properties.
 * Returns null if stage is empty or unrecognized.
 */
export function getKnownStageLabel(
  stage: JobStatus | string | undefined,
  t: (key: TranslationKey) => string
): string | null {
  if (!stage) {
    return null;
  }
  if (Object.prototype.hasOwnProperty.call(STAGE_LABEL_KEYS, stage)) {
    return t(STAGE_LABEL_KEYS[stage as JobStatus]);
  }
  return null;
}
