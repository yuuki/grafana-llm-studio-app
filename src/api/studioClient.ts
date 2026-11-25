import { BackendSrvRequest, config, getBackendSrv } from '@grafana/runtime';
import {
  DashboardIR,
  ImportSource,
  JobSnapshot,
  JobStageEvent,
  MetricCatalogItem,
  PanelValidationResult,
  PublicConfig,
  QueryKind,
  ScopeFilter,
} from '../ir/types';

const PLUGIN_RESOURCE_BASE = '/api/plugins/yuuki-llmstudio-app/resources';
const MAX_SSE_BUFFER_CHARS = 2 * 1024 * 1024;
const STREAM_DROP_RECOVERY_MAX_ATTEMPTS = 5;
const STREAM_DROP_RECOVERY_DELAY_MS = 1000;

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

export interface JobStreamOptions {
  /**
   * When true, requests optional decoupled SSE stage-progress streaming
   * (`Accept: text/event-stream, application/json`) and falls back gracefully
   * to non-streaming JSON requests if SSE is unavailable or disabled on the server.
   */
  enableSse?: boolean;
  /** Invoked for application-level lifecycle stage updates (`generating`, `validating_ir`, etc.). */
  onStage?: (event: JobStageEvent) => void;
  /** Invoked when the client falls back from SSE to a standard non-streaming JSON request. */
  onFallback?: (reason: string) => void;
}

export interface SseFrame {
  event: string;
  data: string;
}

/**
 * Incremental W3C Server-Sent Events frame parser resilient to arbitrary chunk boundaries,
 * CRLF line endings, and `: keepalive` comment heartbeats.
 */
export class SseEventParser {
  private buffer = '';
  private currentEvent = '';
  private currentData: string[] = [];

  push(chunk: string): SseFrame[] {
    if (!chunk) {
      return [];
    }
    this.buffer += chunk;
    if (this.buffer.length > MAX_SSE_BUFFER_CHARS) {
      throw new Error('SSE stream buffer limit exceeded');
    }

    const frames: SseFrame[] = [];
    let nlIdx = this.buffer.indexOf('\n');
    while (nlIdx !== -1) {
      let line = this.buffer.slice(0, nlIdx);
      this.buffer = this.buffer.slice(nlIdx + 1);
      if (line.endsWith('\r')) {
        line = line.slice(0, -1);
      }
      this.consumeLine(line, frames);
      nlIdx = this.buffer.indexOf('\n');
    }
    return frames;
  }

  flush(): SseFrame[] {
    const frames: SseFrame[] = [];
    if (this.buffer.length > 0) {
      const trailing = this.buffer.endsWith('\r') ? this.buffer.slice(0, -1) : this.buffer;
      this.buffer = '';
      this.consumeLine(trailing, frames);
    }
    this.dispatchCurrent(frames);
    return frames;
  }

  private consumeLine(line: string, out: SseFrame[]): void {
    if (line === '') {
      this.dispatchCurrent(out);
      return;
    }
    if (line.startsWith(':')) {
      return;
    }
    const colonIdx = line.indexOf(':');
    const field = colonIdx === -1 ? line : line.slice(0, colonIdx);
    let value = colonIdx === -1 ? '' : line.slice(colonIdx + 1);
    if (value.startsWith(' ')) {
      value = value.slice(1);
    }
    if (field === 'event') {
      this.currentEvent = value;
    } else if (field === 'data') {
      this.currentData.push(value);
    }
  }

  private dispatchCurrent(out: SseFrame[]): void {
    if (this.currentData.length === 0 && this.currentEvent === '') {
      return;
    }
    out.push({
      event: this.currentEvent || 'message',
      data: this.currentData.join('\n'),
    });
    this.currentEvent = '';
    this.currentData = [];
  }
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

function buildResourceUrl(path: string): string {
  const subUrl = (config?.appSubUrl ?? '').replace(/\/+$/, '');
  return `${subUrl}${PLUGIN_RESOURCE_BASE}${path}`;
}

function isAbortError(err: unknown, signal?: AbortSignal): boolean {
  if (signal?.aborted) {
    return true;
  }
  if (err instanceof DOMException && err.name === 'AbortError') {
    return true;
  }
  if (err instanceof Error && err.name === 'AbortError') {
    return true;
  }
  return false;
}

function sleepWithAbort(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Request aborted', 'AbortError'));
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(new DOMException('Request aborted', 'AbortError'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });
  });
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

function isActiveJobStatus(status: JobSnapshot['status']): boolean {
  return status === 'generating' || status === 'validating_ir' || status === 'repairing';
}

async function recoverJobAfterStreamDrop(
  jobId: string,
  signal?: AbortSignal
): Promise<JobSnapshot> {
  let latest: JobSnapshot | null = null;
  for (let attempt = 0; attempt < STREAM_DROP_RECOVERY_MAX_ATTEMPTS; attempt++) {
    if (signal?.aborted) {
      throw new DOMException('Request aborted', 'AbortError');
    }
    latest = await getJob(jobId, signal);
    if (!isActiveJobStatus(latest.status)) {
      if (latest.status === 'failed') {
        throw {
          status: 500,
          data: {
            error: latest.errorMessage || 'Job failed after SSE stream disconnect',
            job: latest,
          },
          message: latest.errorMessage || 'Job failed after SSE stream disconnect',
        };
      }
      if (latest.status === 'cancelled') {
        throw {
          status: 409,
          data: {
            error: latest.errorMessage || 'SSE stream disconnected before completion',
            job: latest,
          },
          message: latest.errorMessage || 'SSE stream disconnected before completion',
        };
      }
      return latest;
    }
    if (attempt + 1 < STREAM_DROP_RECOVERY_MAX_ATTEMPTS) {
      await sleepWithAbort(STREAM_DROP_RECOVERY_DELAY_MS, signal);
    }
  }
  throw {
    status: 502,
    data: {
      error: 'SSE stream disconnected before job completion',
      ...(latest ? { job: latest } : {}),
    },
    message: 'SSE stream disconnected before job completion',
  };
}

interface SseErrorEventPayload {
  status?: number;
  error?: string;
  job?: JobSnapshot;
}

async function postJobWithOptionalSse<TReq>(
  path: string,
  data: TReq,
  signal?: AbortSignal,
  options?: JobStreamOptions
): Promise<JobSnapshot> {
  const nonStreaming = (fallbackReason?: string): Promise<JobSnapshot> => {
    if (fallbackReason) {
      try {
        options?.onFallback?.(fallbackReason);
      } catch {
        // Ignore observer callback errors during fallback.
      }
    }
    return fetchWithAbort<JobSnapshot>(
      {
        url: `${PLUGIN_RESOURCE_BASE}${path}`,
        method: 'POST',
        data,
        showErrorAlert: false,
      },
      signal
    );
  };

  if (!options?.enableSse) {
    return nonStreaming();
  }
  if (signal?.aborted) {
    throw new DOMException('Request aborted', 'AbortError');
  }
  if (typeof globalThis.fetch !== 'function' || typeof TextDecoder === 'undefined') {
    return nonStreaming('streams_unavailable');
  }

  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Accept: 'text/event-stream, application/json',
  };
  const orgId = config?.bootData?.user?.orgId;
  if (typeof orgId === 'number' && orgId > 0) {
    headers['X-Grafana-Org-Id'] = String(orgId);
  }

  let res: Response;
  try {
    res = await globalThis.fetch(buildResourceUrl(path), {
      method: 'POST',
      credentials: 'same-origin',
      headers,
      body: JSON.stringify(data),
      signal,
    });
  } catch (err: unknown) {
    if (isAbortError(err, signal)) {
      throw new DOMException('Request aborted', 'AbortError');
    }
    // Network/transport failure before any response was received: fall back gracefully to BackendSrv non-streaming.
    return nonStreaming('sse_transport_error');
  }

  const contentType = (res.headers?.get('content-type') || '').toLowerCase();

  // Server returned JSON directly (e.g., enableSseProgress=false, pre-flight 4xx/5xx error, or empty catalog).
  if (contentType.includes('application/json')) {
    let parsed: unknown;
    try {
      parsed = await res.json();
    } catch {
      parsed = undefined;
    }
    if (!res.ok) {
      throw {
        status: res.status,
        statusText: res.statusText,
        data: parsed,
      };
    }
    return parsed as JobSnapshot;
  }

  // Proxy or server explicitly rejected text/event-stream negotiation -> fall back to JSON request.
  if (res.status === 406 || res.status === 501) {
    return nonStreaming('sse_not_acceptable');
  }

  if (!res.ok) {
    let errText = '';
    try {
      errText = (await res.text()).trim();
    } catch {
      // Ignore body read error.
    }
    throw {
      status: res.status,
      statusText: res.statusText,
      message: errText || res.statusText || `HTTP ${res.status}`,
    };
  }

  if (!contentType.includes('text/event-stream')) {
    // Check if an intermediary stripped Content-Type on a valid JSON JobSnapshot before falling back.
    try {
      const text = await res.text();
      const maybeSnap = JSON.parse(text) as Partial<JobSnapshot>;
      if (maybeSnap && typeof maybeSnap.status === 'string') {
        return maybeSnap as JobSnapshot;
      }
    } catch {
      // Not valid JSON; fall back cleanly.
    }
    return nonStreaming('unexpected_content_type');
  }

  const parser = new SseEventParser();
  let observedJobId: string | undefined;
  let receivedAnyFrame = false;
  let finalResult: JobSnapshot | null = null;
  let finalError: { status?: number; data?: { error?: string; job?: JobSnapshot }; message?: string } | null =
    null;

  const handleFrames = (frames: SseFrame[]) => {
    for (const frame of frames) {
      receivedAnyFrame = true;
      if (frame.event === 'stage') {
        try {
          const ev = JSON.parse(frame.data) as JobStageEvent;
          if (ev.jobId) {
            observedJobId = ev.jobId;
          }
          options.onStage?.(ev);
        } catch {
          // Ignore malformed non-terminal stage frame; never pass partial JSON to UI state.
        }
      } else if (frame.event === 'result') {
        finalResult = JSON.parse(frame.data) as JobSnapshot;
      } else if (frame.event === 'error') {
        const payload = JSON.parse(frame.data) as SseErrorEventPayload;
        const errMsg = payload.error || 'Job operation failed';
        finalError = {
          status: payload.status,
          data: {
            error: errMsg,
            ...(payload.job ? { job: payload.job } : {}),
          },
          message: errMsg,
        };
      }
    }
  };

  try {
    if (res.body && typeof res.body.getReader === 'function') {
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      try {
        while (true) {
          const { done, value } = await reader.read();
          if (done) {
            const tail = decoder.decode();
            if (tail) {
              handleFrames(parser.push(tail));
            }
            handleFrames(parser.flush());
            break;
          }
          if (value) {
            handleFrames(parser.push(decoder.decode(value, { stream: true })));
          }
        }
      } finally {
        reader.releaseLock();
      }
    } else {
      // Buffered fallback if ReadableStream is unavailable (still extracts complete SSE frames without re-POSTing).
      const fullText = await res.text();
      handleFrames(parser.push(fullText));
      handleFrames(parser.flush());
    }
  } catch (err: unknown) {
    if (isAbortError(err, signal)) {
      throw new DOMException('Request aborted', 'AbortError');
    }
    if (finalResult) {
      return finalResult;
    }
    if (finalError) {
      throw finalError;
    }
    if (observedJobId) {
      return recoverJobAfterStreamDrop(observedJobId, signal);
    }
    if (!receivedAnyFrame) {
      return nonStreaming('sse_stream_error');
    }
    throw err;
  }

  if (finalResult) {
    return finalResult;
  }
  if (finalError) {
    throw finalError;
  }
  if (signal?.aborted) {
    throw new DOMException('Request aborted', 'AbortError');
  }
  if (observedJobId) {
    return recoverJobAfterStreamDrop(observedJobId, signal);
  }
  return nonStreaming('sse_empty_stream');
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

export async function getJob(jobId: string, signal?: AbortSignal): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}`,
      method: 'GET',
      showErrorAlert: false,
    },
    signal
  );
}

export async function generateDraft(
  req: GenerateJobRequest,
  signal?: AbortSignal,
  options?: JobStreamOptions
): Promise<JobSnapshot> {
  return postJobWithOptionalSse('/jobs/generate', req, signal, options);
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
  signal?: AbortSignal,
  options?: JobStreamOptions
): Promise<JobSnapshot> {
  return postJobWithOptionalSse(
    `/jobs/${encodeURIComponent(jobId)}/patch`,
    req,
    signal,
    options
  );
}

export async function repairDraft(
  jobId: string,
  req: RepairJobRequest,
  signal?: AbortSignal,
  options?: JobStreamOptions
): Promise<JobSnapshot> {
  return postJobWithOptionalSse(
    `/jobs/${encodeURIComponent(jobId)}/repair`,
    req,
    signal,
    options
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
