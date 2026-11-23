import { config, getBackendSrv } from '@grafana/runtime';
import { lastValueFrom } from 'rxjs';
import { NormalizedDashboardSpec } from '../spec/normalize';
import { buildFoundationDashboard } from '../spec/toFoundation';

export interface GrafanaFolderOption {
  uid: string;
  title: string;
}

export interface SaveDashboardRequest {
  spec: NormalizedDashboardSpec;
  previewedRevision: number;
  folderUid: string;
  /**
   * Optional caller-provided UID (used mainly in unit tests); otherwise the app generates a fresh UID.
   */
  uidOverride?: string;
}

export interface UpdateDashboardRequest {
  spec: NormalizedDashboardSpec;
  previewedRevision: number;
  targetUid: string;
  /**
   * Required Kubernetes resourceVersion captured when importing the existing dashboard.
   * Ensures optimistic-concurrency revision checking; missing resourceVersion is rejected.
   */
  expectedResourceVersion: string;
  folderUid: string;
}

export interface SaveDashboardResult {
  uid: string;
  url: string;
  folderUid: string;
  revision: number;
  mode?: 'created' | 'updated';
  resourceVersion?: string;
}

export class SaveDashboardError extends Error {
  constructor(
    message: string,
    public readonly code:
      | 'REVISION_MISMATCH'
      | 'MISSING_RESOURCE_VERSION'
      | 'REVISION_CONFLICT'
      | 'IN_FLIGHT'
      | 'PERMISSION_DENIED'
      | 'UID_CONFLICT'
      | 'API_ERROR'
  ) {
    super(message);
    this.name = 'SaveDashboardError';
  }
}

/**
 * Generates a fresh Grafana-compliant Dashboard UID (<= 40 chars, [a-z0-9_-]).
 * Dashboard UIDs are strictly app-managed and NEVER supplied by the LLM.
 */
export function generateDashboardUid(): string {
  const bytes = new Uint8Array(9);
  if (typeof globalThis.crypto !== 'undefined' && globalThis.crypto.getRandomValues) {
    globalThis.crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < bytes.length; i++) {
      bytes[i] = Math.floor(Math.random() * 256);
    }
  }
  const hex = Array.from(bytes)
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('');
  return `llmstudio-${hex}`;
}

/**
 * Derives the Grafana v12.4.15 / v13.3.1 API namespace for the active organization.
 * Prefers Grafana's config.namespace when present (M-10).
 */
export function getOrgNamespace(orgId?: number): string {
  if (orgId === undefined && typeof config.namespace === 'string' && config.namespace.trim() !== '') {
    return config.namespace.trim();
  }
  const id = orgId ?? config.bootData?.user?.orgId ?? 1;
  return id === 1 ? 'default' : `org-${id}`;
}

interface FolderV1Beta1ListResponse {
  items?: Array<{
    metadata?: { name?: string };
    spec?: { title?: string };
  }>;
}

/**
 * Lists folders visible to the logged-in user via Grafana's Folder API.
 * Uses `/api/folders?permission=Edit` (active in Grafana 12.4.x and 13.x) for write-permission filtering,
 * and falls back to `/apis/folder.grafana.app/v1beta1/namespaces/{namespace}/folders` if legacy `/api`
 * endpoints are disabled (404/410). Save authorization is always enforced server-side on POST/PUT.
 * Uses showErrorAlert: false so Viewer accounts without folders:read do not see spurious error toasts (M-10).
 */
export async function listWritableFolders(): Promise<GrafanaFolderOption[]> {
  const list: GrafanaFolderOption[] = [{ uid: '', title: 'General (Root)' }];
  try {
    const res = await lastValueFrom(
      getBackendSrv().fetch<Array<{ uid: string; title: string }>>({
        url: '/api/folders',
        method: 'GET',
        params: {
          limit: 100,
          permission: 'Edit',
        },
        showErrorAlert: false,
      })
    );
    const folders = res.data;
    if (Array.isArray(folders)) {
      for (const f of folders) {
        if (f.uid && f.title) {
          list.push({ uid: f.uid, title: f.title });
        }
      }
    }
    return list;
  } catch (err: unknown) {
    const status = (err as { status?: number })?.status;
    if (status !== 404 && status !== 410) {
      throw err;
    }
    const namespace = getOrgNamespace();
    const fallback = await lastValueFrom(
      getBackendSrv().fetch<FolderV1Beta1ListResponse>({
        url: `/apis/folder.grafana.app/v1beta1/namespaces/${encodeURIComponent(namespace)}/folders`,
        method: 'GET',
        params: { limit: 100 },
        showErrorAlert: false,
      })
    );
    const items = fallback.data?.items;
    if (Array.isArray(items)) {
      for (const item of items) {
        const uid = item.metadata?.name;
        const title = item.spec?.title;
        if (uid && title) {
          list.push({ uid, title });
        }
      }
    }
    return list;
  }
}

/**
 * Dashboard saver compatible with Grafana OSS 12.4.15 and 13.3.1 Kubernetes-style Dashboard API:
 * - Default `createNewDashboard`: Create-only `POST /apis/dashboard.grafana.app/v1beta1/namespaces/{namespace}/dashboards`
 *   (on 409 Conflict, NEVER switches to PUT/overwrite).
 * - Opt-in `updateExistingDashboard`: Explicit revision-checked `PUT .../dashboards/{uid}` requiring
 *   `metadata.resourceVersion` captured at import time (on 409/412 conflict, fails safely and NEVER force-overwrites).
 *
 * Guarantees:
 * - Uses the logged-in user's Grafana session (never an app backend service account).
 * - Verifies saved revision === previewedRevision before sending.
 * - Guards against double-clicks via per-instance in-flight lock.
 * - Prepends config.appSubUrl for subpath Grafana deployments (M-10).
 */
export class DashboardSaver {
  private inFlight = false;

  async createNewDashboard(req: SaveDashboardRequest): Promise<SaveDashboardResult> {
    if (this.inFlight) {
      throw new SaveDashboardError(
        '保存処理がすでに進行中です。完了までお待ちください。',
        'IN_FLIGHT'
      );
    }
    if (req.spec.revision !== req.previewedRevision) {
      throw new SaveDashboardError(
        `保存対象リビジョン (rev ${req.spec.revision}) が確認済みプレビュー (rev ${req.previewedRevision}) と一致しません。先に検証・プレビューを実行してください。`,
        'REVISION_MISMATCH'
      );
    }

    this.inFlight = true;
    try {
      const uid = req.uidOverride?.trim() || generateDashboardUid();
      const foundationSpec = buildFoundationDashboard(req.spec, { uid });
      const namespace = getOrgNamespace();

      const annotations: Record<string, string> = {};
      if (req.folderUid && req.folderUid.trim() !== '') {
        annotations['grafana.app/folder'] = req.folderUid.trim();
      }

      const payload = {
        apiVersion: 'dashboard.grafana.app/v1beta1',
        kind: 'Dashboard',
        metadata: {
          name: uid,
          annotations,
        },
        spec: foundationSpec,
      };

      const url = `/apis/dashboard.grafana.app/v1beta1/namespaces/${encodeURIComponent(namespace)}/dashboards`;

      const response = await lastValueFrom(
        getBackendSrv().fetch<{ metadata?: { name?: string; resourceVersion?: string } }>({
          url,
          method: 'POST',
          showErrorAlert: false,
          headers: {
            'Content-Type': 'application/json',
          },
          data: payload,
        })
      );

      const savedUid = response.data?.metadata?.name || uid;
      const subUrl = (config.appSubUrl ?? '').replace(/\/+$/, '');
      return {
        uid: savedUid,
        url: `${subUrl}/d/${encodeURIComponent(savedUid)}`,
        folderUid: req.folderUid,
        revision: req.spec.revision,
        mode: 'created',
        resourceVersion: response.data?.metadata?.resourceVersion,
      };
    } catch (err: unknown) {
      const status = (err as { status?: number })?.status;
      const msg =
        (err as { data?: { message?: string }; message?: string })?.data?.message ||
        (err as { message?: string })?.message ||
        'ダッシュボードの新規保存に失敗しました';

      if (status === 401 || status === 403) {
        throw new SaveDashboardError(
          `選択したフォルダへの保存権限がありません (HTTP ${status}): ${msg}`,
          'PERMISSION_DENIED'
        );
      }
      if (status === 409 || status === 412) {
        throw new SaveDashboardError(
          `UID 衝突のため新規作成を中止しました。既存ダッシュボードは上書きされていません (HTTP ${status}): ${msg}`,
          'UID_CONFLICT'
        );
      }
      throw new SaveDashboardError(
        `ダッシュボード保存 API エラー (HTTP ${status ?? 'unknown'}): ${msg}`,
        'API_ERROR'
      );
    } finally {
      this.inFlight = false;
    }
  }

  /**
   * Opt-in revision-checked update of an imported Grafana dashboard via Kubernetes-style PUT.
   * Requires both `targetUid` and `expectedResourceVersion` so concurrent external edits
   * fail safely with HTTP 409/412 (`REVISION_CONFLICT`) rather than silently overwriting.
   */
  async updateExistingDashboard(req: UpdateDashboardRequest): Promise<SaveDashboardResult> {
    if (this.inFlight) {
      throw new SaveDashboardError(
        '保存処理がすでに進行中です。完了までお待ちください。',
        'IN_FLIGHT'
      );
    }
    if (req.spec.revision !== req.previewedRevision) {
      throw new SaveDashboardError(
        `保存対象リビジョン (rev ${req.spec.revision}) が確認済みプレビュー (rev ${req.previewedRevision}) と一致しません。先に検証・プレビューを実行してください。`,
        'REVISION_MISMATCH'
      );
    }
    const targetUid = req.targetUid.trim();
    const expectedRV = req.expectedResourceVersion.trim();
    if (!targetUid || !expectedRV) {
      throw new SaveDashboardError(
        '既存ダッシュボードの更新には対象 UID とインポート時の resourceVersion が必須です。コピーとして新規保存してください。',
        'MISSING_RESOURCE_VERSION'
      );
    }

    this.inFlight = true;
    try {
      const foundationSpec = buildFoundationDashboard(req.spec, { uid: targetUid });
      const namespace = getOrgNamespace();

      const annotations: Record<string, string> = {};
      if (req.folderUid && req.folderUid.trim() !== '') {
        annotations['grafana.app/folder'] = req.folderUid.trim();
      }

      const payload = {
        apiVersion: 'dashboard.grafana.app/v1beta1',
        kind: 'Dashboard',
        metadata: {
          name: targetUid,
          resourceVersion: expectedRV,
          annotations,
        },
        spec: foundationSpec,
      };

      const url = `/apis/dashboard.grafana.app/v1beta1/namespaces/${encodeURIComponent(namespace)}/dashboards/${encodeURIComponent(targetUid)}`;

      const response = await lastValueFrom(
        getBackendSrv().fetch<{ metadata?: { name?: string; resourceVersion?: string } }>({
          url,
          method: 'PUT',
          showErrorAlert: false,
          headers: {
            'Content-Type': 'application/json',
          },
          data: payload,
        })
      );

      const savedUid = response.data?.metadata?.name || targetUid;
      const subUrl = (config.appSubUrl ?? '').replace(/\/+$/, '');
      return {
        uid: savedUid,
        url: `${subUrl}/d/${encodeURIComponent(savedUid)}`,
        folderUid: req.folderUid,
        revision: req.spec.revision,
        mode: 'updated',
        resourceVersion: response.data?.metadata?.resourceVersion,
      };
    } catch (err: unknown) {
      const status = (err as { status?: number })?.status;
      const msg =
        (err as { data?: { message?: string }; message?: string })?.data?.message ||
        (err as { message?: string })?.message ||
        '既存ダッシュボードの更新に失敗しました';

      if (status === 401 || status === 403) {
        throw new SaveDashboardError(
          `対象ダッシュボードの更新権限がありません (HTTP ${status}): ${msg}`,
          'PERMISSION_DENIED'
        );
      }
      if (status === 409 || status === 412) {
        throw new SaveDashboardError(
          `インポート後に既存ダッシュボードが他で更新されたため上書きを中止しました (HTTP ${status})。再インポートするか新規コピーとして保存してください: ${msg}`,
          'REVISION_CONFLICT'
        );
      }
      throw new SaveDashboardError(
        `ダッシュボード更新 API エラー (HTTP ${status ?? 'unknown'}): ${msg}`,
        'API_ERROR'
      );
    } finally {
      this.inFlight = false;
    }
  }
}

/**
 * Exports a clean Foundation SDK Dashboard JSON (without UID, secrets, chat logs, or raw data).
 */
export function buildExportableDashboardJson(spec: NormalizedDashboardSpec): string {
  const dash = buildFoundationDashboard(spec, { uid: '' });
  return JSON.stringify(dash, null, 2);
}
