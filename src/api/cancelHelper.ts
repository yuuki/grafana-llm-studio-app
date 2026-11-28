import { JobSnapshot } from '../ir/types';
import { cancelDraftJob } from './studioClient';

export interface CancelJobBoundedOptions {
  timeoutMs?: number;
  controller?: AbortController;
}

/**
 * Best-effort bounded backend job cancellation. Always clears its timeout timer and returns null
 * if cancellation races with job completion or times out.
 */
export async function cancelJobBounded(
  jobId: string,
  options?: CancelJobBoundedOptions
): Promise<JobSnapshot | null> {
  const controller = options?.controller ?? new AbortController();
  const timeoutMs = options?.timeoutMs ?? 5000;
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    return await cancelDraftJob(jobId, controller.signal);
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
}
