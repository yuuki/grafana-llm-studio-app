import { BackendSrvRequest, config, getBackendSrv } from '@grafana/runtime';
import {
  AnalyzeStatusPayload,
  DashboardIR,
  ImportSource,
  JobOperation,
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
const STREAM_DROP_RECOVERY_MAX_ATTEMPTS = 15;
const STREAM_DROP_RECOVERY_DELAY_MS = 2000;

const VALID_JOB_OPERATIONS = new Set<JobOperation>(['generate', 'patch', 'repair', 'analyze']);

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
   * to non-streaming JSON requests when SSE is unavailable or rejected before execution.
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
 * Incremental WHATWG Server-Sent Events frame parser resilient to arbitrary chunk boundaries,
 * LF/CRLF/CR line endings, `: keepalive` comment heartbeats, and total buffered-event memory caps.
 */
export class SseEventParser {
  private buffer = '';
  private currentEvent = '';
  private currentData: string[] = [];
  private currentDataChars = 0;

  push(chunk: string): SseFrame[] {
    if (!chunk) {
      return [];
    }
    this.buffer += chunk;
    if (this.buffer.length + this.currentDataChars > MAX_SSE_BUFFER_CHARS) {
      throw new Error('SSE stream buffer limit exceeded');
    }

    const frames: SseFrame[] = [];
    let pos = 0;
    const len = this.buffer.length;
    while (pos < len) {
      const crIdx = this.buffer.indexOf('\r', pos);
      const lfIdx = this.buffer.indexOf('\n', pos);
      let breakIdx = -1;
      if (crIdx !== -1 && lfIdx !== -1) {
        breakIdx = Math.min(crIdx, lfIdx);
      } else if (crIdx !== -1) {
        breakIdx = crIdx;
      } else if (lfIdx !== -1) {
        breakIdx = lfIdx;
      }

      if (breakIdx === -1) {
        break;
      }
      // Hold a trailing CR at the end of a chunk in case LF arrives at the start of the next chunk.
      if (this.buffer[breakIdx] === '\r' && breakIdx === len - 1) {
        break;
      }

      const line = this.buffer.slice(pos, breakIdx);
      if (this.buffer[breakIdx] === '\r' && this.buffer[breakIdx + 1] === '\n') {
        pos = breakIdx + 2;
      } else {
        pos = breakIdx + 1;
      }
      this.consumeLine(line, frames);
    }

    if (pos > 0) {
      this.buffer = this.buffer.slice(pos);
    }
    return frames;
  }

  flush(): SseFrame[] {
    const frames: SseFrame[] = [];
    if (this.buffer.length > 0) {
      const hadTrailingCr = this.buffer.endsWith('\r');
      const trailing = hadTrailingCr ? this.buffer.slice(0, -1) : this.buffer;
      this.buffer = '';
      this.consumeLine(trailing, frames);
      if (hadTrailingCr) {
        this.consumeLine('', frames);
      }
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
      this.currentDataChars += value.length + 1;
      if (this.buffer.length + this.currentDataChars > MAX_SSE_BUFFER_CHARS) {
        throw new Error('SSE stream buffer limit exceeded');
      }
      this.currentData.push(value);
    }
  }

  private dispatchCurrent(out: SseFrame[]): void {
    if (this.currentData.length === 0) {
      this.currentEvent = '';
      this.currentDataChars = 0;
      return;
    }
    out.push({
      event: this.currentEvent || 'message',
      data: this.currentData.join('\n'),
    });
    this.currentEvent = '';
    this.currentData = [];
    this.currentDataChars = 0;
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

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function isValidJobSnapshot(v: unknown): v is JobSnapshot {
  return isPlainObject(v) && typeof v.status === 'string';
}

function parseValidatedStageEvent(raw: string): JobStageEvent | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isPlainObject(parsed) || typeof parsed.stage !== 'string' || parsed.stage.trim() === '') {
    return null;
  }
  const jobId = typeof parsed.jobId === 'string' ? parsed.jobId : undefined;
  const op: JobOperation =
    typeof parsed.operation === 'string' && VALID_JOB_OPERATIONS.has(parsed.operation as JobOperation)
      ? (parsed.operation as JobOperation)
      : 'generate';
  const timestamp = typeof parsed.timestamp === 'string' ? parsed.timestamp : '';
  return {
    ...(jobId ? { jobId } : {}),
    operation: op,
    stage: parsed.stage as JobStageEvent['stage'],
    timestamp,
  };
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
  return (
    status === 'generating' ||
    status === 'validating_ir' ||
    status === 'repairing' ||
    status === 'analyzing' ||
    status === 'validating_report'
  );
}

async function recoverJobAfterStreamDrop(
  jobId: string,
  signal?: AbortSignal,
  expectedMinRevision?: number
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
      // For patch/repair, RevertOnCancel returns status='awaiting_preview' at the UNCHANGED revision.
      // Never treat a reverted old revision as a successful edit/repair (H-1).
      if (
        expectedMinRevision !== undefined &&
        (latest.currentIR?.revision ?? 0) < expectedMinRevision
      ) {
        const msg =
          latest.errorMessage ||
          latest.summary ||
          'SSE stream disconnected before edit completed; draft was reverted to the previous revision';
        throw {
          status: 409,
          data: {
            error: msg,
            job: latest,
          },
          message: msg,
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

interface SseErrorObject {
  status?: number;
  data?: { error?: string; job?: JobSnapshot };
  message?: string;
}

interface JobOpContext {
  jobId?: string;
  expectedMinRevision?: number;
}

async function postJobWithOptionalSse<TReq>(
  path: string,
  data: TReq,
  signal?: AbortSignal,
  options?: JobStreamOptions,
  opContext?: JobOpContext
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

  let observedJobId: string | undefined = opContext?.jobId;

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
    // Never blindly re-POST on a post-send transport reset (prevents duplicate LLM calls / double repair budget burn, H-2).
    if (observedJobId) {
      return recoverJobAfterStreamDrop(observedJobId, signal, opContext?.expectedMinRevision);
    }
    throw {
      status: 502,
      message: err instanceof Error ? err.message : 'SSE request failed before receiving a response',
    };
  }

  const contentType = (res.headers?.get('content-type') || '').toLowerCase();

  // Server returned JSON directly (e.g., enableSseProgress=false, pre-flight 4xx/5xx error, or empty catalog).
  if (contentType.includes('application/json')) {
    let parsed: unknown;
    try {
      parsed = await res.json();
    } catch (err: unknown) {
      if (isAbortError(err, signal)) {
        throw new DOMException('Request aborted', 'AbortError');
      }
      parsed = undefined;
    }
    if (!res.ok) {
      throw {
        status: res.status,
        statusText: res.statusText,
        data: parsed,
      };
    }
    if (!isValidJobSnapshot(parsed)) {
      throw new Error('Invalid JSON response from plugin backend');
    }
    return parsed;
  }

  // Proxy or server explicitly rejected text/event-stream negotiation BEFORE executing the job -> safe non-streaming fallback.
  if (res.status === 406 || res.status === 501) {
    return nonStreaming('sse_not_acceptable');
  }

  if (!res.ok) {
    let errText = '';
    try {
      errText = (await res.text()).trim();
    } catch (err: unknown) {
      if (isAbortError(err, signal)) {
        throw new DOMException('Request aborted', 'AbortError');
      }
    }
    throw {
      status: res.status,
      statusText: res.statusText,
      message: errText || res.statusText || `HTTP ${res.status}`,
    };
  }

  const parser = new SseEventParser();
  let finalResult: JobSnapshot | null = null;
  let finalError: SseErrorObject | null = null;

  const handleFrames = (frames: SseFrame[]) => {
    for (const frame of frames) {
      if (frame.event === 'stage') {
        const ev = parseValidatedStageEvent(frame.data);
        if (!ev) {
          continue;
        }
        if (ev.jobId) {
          observedJobId = ev.jobId;
        }
        try {
          options.onStage?.(ev);
        } catch {
          // Ignore UI callback errors so stream consumption continues cleanly.
        }
      } else if (frame.event === 'result') {
        const parsed = JSON.parse(frame.data) as unknown;
        if (!isValidJobSnapshot(parsed)) {
          throw new Error('Invalid SSE result payload from plugin backend');
        }
        finalResult = parsed;
      } else if (frame.event === 'error') {
        const parsed = JSON.parse(frame.data) as unknown;
        const payload = isPlainObject(parsed) ? parsed : {};
        const errMsg =
          typeof payload.error === 'string' && payload.error.trim().length > 0
            ? payload.error
            : 'Job operation failed';
        const status = typeof payload.status === 'number' ? payload.status : undefined;
        const jobSnap = isValidJobSnapshot(payload.job) ? payload.job : undefined;
        finalError = {
          status,
          data: {
            error: errMsg,
            ...(jobSnap ? { job: jobSnap } : {}),
          },
          message: errMsg,
        };
      }
    }
  };

  if (!contentType.includes('text/event-stream')) {
    // HTTP 200 with unexpected Content-Type (e.g., proxy rewrote text/event-stream to text/plain):
    // Attempt to parse JSON or buffered SSE frames directly without blindly re-POSTing (H-2).
    let text = '';
    try {
      text = await res.text();
    } catch (err: unknown) {
      if (isAbortError(err, signal)) {
        throw new DOMException('Request aborted', 'AbortError');
      }
      if (observedJobId) {
        return recoverJobAfterStreamDrop(observedJobId, signal, opContext?.expectedMinRevision);
      }
      throw err;
    }
    try {
      const maybeSnap = JSON.parse(text) as unknown;
      if (isValidJobSnapshot(maybeSnap)) {
        return maybeSnap;
      }
    } catch {
      // Not JSON; check if proxy rewrote Content-Type on a buffered SSE payload.
    }
    if (text.includes('data:') || text.includes('event:')) {
      handleFrames(parser.push(text));
      handleFrames(parser.flush());
      if (finalResult) {
        return finalResult;
      }
      if (finalError) {
        throw finalError;
      }
    }
    if (observedJobId) {
      return recoverJobAfterStreamDrop(observedJobId, signal, opContext?.expectedMinRevision);
    }
    throw {
      status: 502,
      message: 'Unexpected response format from plugin backend during SSE request',
    };
  }

  try {
    if (res.body && typeof res.body.getReader === 'function') {
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let streamDone = false;
      try {
        while (true) {
          const { done, value } = await reader.read();
          if (done) {
            streamDone = true;
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
        if (!streamDone) {
          void reader.cancel().catch(() => {});
        }
        reader.releaseLock();
      }
    } else {
      // Buffered fallback if ReadableStream is unavailable (extracts complete SSE frames without re-POSTing).
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
    // Once 200 text/event-stream was sent, the backend already started the job; NEVER re-POST (H-2).
    if (observedJobId) {
      return recoverJobAfterStreamDrop(observedJobId, signal, opContext?.expectedMinRevision);
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
    return recoverJobAfterStreamDrop(observedJobId, signal, opContext?.expectedMinRevision);
  }
  throw {
    status: 502,
    message: 'SSE stream closed before receiving a result from plugin backend',
  };
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

export async function analyzeStatus(
  req: AnalyzeStatusPayload,
  signal?: AbortSignal,
  options?: JobStreamOptions
): Promise<JobSnapshot> {
  return postJobWithOptionalSse('/jobs/analyze', req, signal, options);
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
    options,
    { jobId, expectedMinRevision: req.baseRevision + 1 }
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
    options,
    { jobId, expectedMinRevision: req.baseRevision + 1 }
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

export async function cancelDraftJob(
  jobId: string,
  signal?: AbortSignal
): Promise<JobSnapshot> {
  return fetchWithAbort<JobSnapshot>(
    {
      url: `${PLUGIN_RESOURCE_BASE}/jobs/${encodeURIComponent(jobId)}/cancel`,
      method: 'POST',
      showErrorAlert: false,
    },
    signal
  );
}
