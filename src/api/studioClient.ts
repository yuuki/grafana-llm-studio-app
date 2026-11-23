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

function fetchWithAbort<T>(req: BackendSrvRequest, signal?: AbortSignal): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new Error('Request aborted'));
      return;
    }
    let sub: { unsubscribe: () => void } | undefined;
    const onAbort = () => {
      sub?.unsubscribe();
      signal?.removeEventListener('abort', onAbort);
      reject(new Error('Request aborted'));
    };
    const cleanup = () => {
      signal?.removeEventListener('abort', onAbort);
    };
    signal?.addEventListener('abort', onAbort, { once: true });

    sub = getBackendSrv()
      .fetch<T>(req)
      .subscribe({
        next: (res) => {
          cleanup();
          resolve(res.data);
        },
        error: (err) => {
          cleanup();
          reject(err);
        },
      });
  });
}

export async function fetchPublicConfig(): Promise<PublicConfig> {
  return getBackendSrv().get<PublicConfig>(`${PLUGIN_RESOURCE_BASE}/config/public`);
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

export async function undoDraft(jobId: string): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>({
    url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}/undo`,
    method: 'POST',
    showErrorAlert: false,
  });
}

export async function cancelDraftJob(jobId: string): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>({
    url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}/cancel`,
    method: 'POST',
    showErrorAlert: false,
  });
}
