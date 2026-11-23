import { BackendSrvRequest, getBackendSrv } from '@grafana/runtime';
import {
  JobSnapshot,
  MetricCatalogItem,
  PanelValidationResult,
  PublicConfig,
  ScopeFilter,
} from '../ir/types';

const PLUGIN_RESOURCE_BASE = '/api/plugins/yuuki-llmstudio-app/resources';

export interface GenerateJobRequest {
  datasourceUid: string;
  scope?: ScopeFilter;
  userIntent: string;
  catalog: MetricCatalogItem[];
}

export interface PatchJobRequest {
  baseRevision: number;
  targetPanelId?: string;
  editIntent: string;
  catalog: MetricCatalogItem[];
}

export interface RepairJobRequest {
  baseRevision: number;
  results: PanelValidationResult[];
  catalog: MetricCatalogItem[];
}

/**
 * Formats Error objects and Grafana FetchError plain objects without producing "[object Object]" (M-13).
 */
export function extractApiError(err: unknown): string {
  if (err instanceof Error && err.message) {
    return err.message;
  }
  if (typeof err === 'object' && err !== null) {
    const e = err as {
      data?: { error?: string; message?: string };
      message?: string;
      statusText?: string;
      status?: number;
    };
    const msg = e.data?.error || e.data?.message || e.message || e.statusText;
    if (msg) {
      return e.status ? `HTTP ${e.status}: ${msg}` : String(msg);
    }
  }
  return String(err);
}

function fetchWithAbort<T>(req: BackendSrvRequest, signal?: AbortSignal): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Request aborted', 'AbortError'));
      return;
    }
    let settled = false;
    let sub: { unsubscribe: () => void } | undefined;
    const cleanup = () => {
      signal?.removeEventListener('abort', onAbort);
    };
    const onAbort = () => {
      if (settled) {
        return;
      }
      settled = true;
      sub?.unsubscribe();
      cleanup();
      reject(new DOMException('Request aborted', 'AbortError'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });

    sub = getBackendSrv()
      .fetch<T>(req)
      .subscribe({
        next: (res) => {
          if (settled) {
            return;
          }
          settled = true;
          cleanup();
          resolve(res.data);
        },
        error: (err) => {
          if (settled) {
            return;
          }
          settled = true;
          cleanup();
          reject(err);
        },
        complete: () => {
          if (!settled) {
            settled = true;
            cleanup();
            reject(new Error('Empty response from plugin backend'));
          }
        },
      });
  });
}

export async function fetchPublicConfig(signal?: AbortSignal): Promise<PublicConfig> {
  return fetchWithAbort<PublicConfig>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/config/public`,
      method: 'GET',
      showErrorAlert: false,
    },
    signal
  );
}

export async function generateDraft(
  req: GenerateJobRequest,
  signal?: AbortSignal
): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/jobs/generate`,
      method: 'POST',
      data: req,
      showErrorAlert: false,
    },
    signal
  );
}

export async function patchDraft(
  jobId: string,
  req: PatchJobRequest,
  signal?: AbortSignal
): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}/patch`,
      method: 'POST',
      data: req,
      showErrorAlert: false,
    },
    signal
  );
}

export async function repairDraft(
  jobId: string,
  req: RepairJobRequest,
  signal?: AbortSignal
): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}/repair`,
      method: 'POST',
      data: req,
      showErrorAlert: false,
    },
    signal
  );
}

export async function undoDraft(jobId: string, signal?: AbortSignal): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}/undo`,
      method: 'POST',
      showErrorAlert: false,
    },
    signal
  );
}

export async function cancelDraftJob(jobId: string): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>({
    url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}/cancel`,
    method: 'POST',
    showErrorAlert: false,
  });
}
