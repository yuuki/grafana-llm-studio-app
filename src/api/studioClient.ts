import { BackendSrvRequest, getBackendSrv } from '@grafana/runtime';
import {
  DashboardIR,
  ImportSource,
  JobSnapshot,
  MetricCatalogItem,
  PanelValidationResult,
  PublicConfig,
  QueryKind,
  ScopeFilter,
} from '../ir/types';

const PLUGIN_RESOURCE_BASE = '/api/plugins/yuuki-llmstudio-app/resources';

const GENERIC_HTTP_ERROR_LABELS = new Set([
  'bad request',
  'unauthorized',
  'forbidden',
  'not found',
  'method not allowed',
  'request timeout',
  'conflict',
  'unprocessable entity',
  'unprocessable content',
  'too many requests',
  'internal server error',
  'bad gateway',
  'service unavailable',
  'gateway timeout',
]);

export interface GenerateJobRequest {
  datasourceUid: string;
  queryKind?: QueryKind;
  scope?: ScopeFilter;
  userIntent: string;
  catalog: MetricCatalogItem[];
}

export interface ImportJobRequest {
  datasourceUid: string;
  queryKind?: QueryKind;
  scope?: ScopeFilter;
  draft: DashboardIR;
  importSource?: ImportSource;
  catalog?: MetricCatalogItem[];
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
 * Prefers specific `data.message` when `data.error` is only a generic HTTP status phrase (e.g., VM plugin VMAPIQuery).
 */
export function extractApiError(err: unknown): string {
  if (err instanceof Error && err.message) {
    return err.message;
  }
  if (typeof err === 'object' && err !== null) {
    const e = err as {
      data?: { error?: unknown; message?: unknown };
      message?: unknown;
      statusText?: unknown;
      status?: number;
    };
    const dataErr = typeof e.data?.error === 'string' ? e.data.error.trim() : '';
    const dataMsg = typeof e.data?.message === 'string' ? e.data.message.trim() : '';
    const primary =
      dataErr && GENERIC_HTTP_ERROR_LABELS.has(dataErr.toLowerCase()) && dataMsg
        ? dataMsg
        : dataErr || dataMsg;
    const candidates = [primary, e.message, e.statusText];
    const msg = candidates.find(
      (c): c is string => typeof c === 'string' && c.trim().length > 0
    );
    if (msg) {
      return e.status ? `HTTP ${e.status}: ${msg}` : msg;
    }
    if (typeof e.status === 'number') {
      return `HTTP ${e.status}`;
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

export async function importDraft(
  req: ImportJobRequest,
  signal?: AbortSignal
): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/jobs/import`,
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
