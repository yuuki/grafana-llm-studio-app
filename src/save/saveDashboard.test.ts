import { Observable, of, throwError } from 'rxjs';
import { DEFAULT_LOCALE, setStudioLocale, SUPPORTED_LOCALES, t } from '../i18n';
import { DashboardIR } from '../ir/types';
import { normalizeDashboardIR } from '../spec/normalize';
import { DashboardSaver, listWritableFolders, SaveDashboardError } from './saveDashboard';

const mockFetch = jest.fn();

const mockConfig: {
  namespace?: string;
  appSubUrl?: string;
  bootData: { user: { orgId: number } };
} = {
  namespace: undefined,
  appSubUrl: '',
  bootData: {
    user: { orgId: 1 },
  },
};

jest.mock('@grafana/runtime', () => ({
  get config() {
    return mockConfig;
  },
  getBackendSrv: () => ({
    fetch: mockFetch,
  }),
}));

const sampleIR: DashboardIR = {
  schemaVersion: '1.0',
  revision: 3,
  title: 'checkout-api Dashboard',
  timeRange: { from: 'now-15m', to: 'now' },
  panels: [
    {
      id: 'panel-req',
      type: 'timeseries',
      title: 'Request Rate',
      datasourceRef: 'selected_prometheus',
      query: { expr: 'sum(rate(http_requests_total{service="checkout-api"}[5m]))' },
      unit: 'reqps',
      layout: { row: 0, col: 0, width: 'full' },
    },
  ],
};

describe.each(SUPPORTED_LOCALES)(
  'Create-only DashboardSaver (Grafana 12.4.15 v1beta1, locale: %s)',
  (locale) => {
    const spec = normalizeDashboardIR(sampleIR, { uid: 'prom-main', type: 'prometheus' });

    beforeEach(async () => {
      await setStudioLocale(locale);
      mockFetch.mockReset();
      mockConfig.namespace = undefined;
      mockConfig.appSubUrl = '';
      mockConfig.bootData.user.orgId = 1;
    });

    afterAll(async () => {
      await setStudioLocale(DEFAULT_LOCALE);
    });

    test('rejects saving when revision does not match previewedRevision', async () => {
      const saver = new DashboardSaver();
      await expect(
        saver.createNewDashboard({ spec, previewedRevision: 2, folderUid: 'folder-1' })
      ).rejects.toMatchObject({
        code: 'REVISION_MISMATCH',
        message: t('save.errors.revisionMismatch', { targetRevision: 3, previewedRevision: 2 }),
      });
      expect(mockFetch).not.toHaveBeenCalled();
    });

    test('creates a new dashboard via POST v1beta1 and never sends PUT', async () => {
      mockFetch.mockReturnValueOnce(of({ data: { metadata: { name: 'llmstudio-fresh123' } } }));
      const saver = new DashboardSaver();
      const res = await saver.createNewDashboard({
        spec,
        previewedRevision: 3,
        folderUid: 'team-folder',
        uidOverride: 'llmstudio-fresh123',
      });

      expect(res.uid).toBe('llmstudio-fresh123');
      expect(res.url).toBe('/d/llmstudio-fresh123');
      expect(mockFetch).toHaveBeenCalledTimes(1);

      const callArg = mockFetch.mock.calls[0][0];
      expect(callArg.method).toBe('POST');
      expect(callArg.url).toBe('/apis/dashboard.grafana.app/v1beta1/namespaces/default/dashboards');
      expect(callArg.data.metadata.name).toBe('llmstudio-fresh123');
      expect(callArg.data.metadata.annotations['grafana.app/folder']).toBe('team-folder');
      expect(callArg.data.spec.id).toBeNull();
      expect(callArg.data.spec.version).toBe(0);
      expect(callArg.data.spec.refresh).toBe('');
    });

    test('prefers config.namespace and prefixes returned dashboard URL with config.appSubUrl (M-10)', async () => {
      mockConfig.namespace = 'org-42';
      mockConfig.appSubUrl = '/grafana';
      mockFetch.mockReturnValueOnce(of({ data: { metadata: { name: 'llmstudio-sub1' } } }));

      const saver = new DashboardSaver();
      const res = await saver.createNewDashboard({
        spec,
        previewedRevision: 3,
        folderUid: '',
        uidOverride: 'llmstudio-sub1',
      });

      const callArg = mockFetch.mock.calls[0][0];
      expect(callArg.url).toBe('/apis/dashboard.grafana.app/v1beta1/namespaces/org-42/dashboards');
      expect(res.url).toBe('/grafana/d/llmstudio-sub1');
    });

    test('on 409 UID conflict, fails safely and NEVER retries with PUT/overwrite', async () => {
      mockFetch.mockReturnValueOnce(
        throwError(() => ({ status: 409, data: { message: 'dashboards.dashboard.grafana.app already exists' } }))
      );
      const saver = new DashboardSaver();
      await expect(
        saver.createNewDashboard({ spec, previewedRevision: 3, folderUid: '' })
      ).rejects.toMatchObject({
        code: 'UID_CONFLICT',
        message: t('save.errors.uidConflict', {
          status: 409,
          message: 'dashboards.dashboard.grafana.app already exists',
        }),
      });
      expect(mockFetch).toHaveBeenCalledTimes(1);
    });

    test('on 403 permission denied, surfaces PERMISSION_DENIED', async () => {
      mockFetch.mockReturnValueOnce(throwError(() => ({ status: 403, data: { message: 'Forbidden' } })));
      const saver = new DashboardSaver();
      await expect(
        saver.createNewDashboard({ spec, previewedRevision: 3, folderUid: 'readonly-folder' })
      ).rejects.toMatchObject({
        code: 'PERMISSION_DENIED',
        message: t('save.errors.permissionDenied', { status: 403, message: 'Forbidden' }),
      });
    });

    test('blocks double-click concurrent save attempts', async () => {
      let resolveFirst!: () => void;
      mockFetch.mockImplementationOnce(
        () =>
          new Observable((sub) => {
            resolveFirst = () => {
              sub.next({ data: { metadata: { name: 'llmstudio-once' } } });
              sub.complete();
            };
          })
      );

      const saver = new DashboardSaver();
      const first = saver.createNewDashboard({ spec, previewedRevision: 3, folderUid: '' });
      await expect(
        saver.createNewDashboard({ spec, previewedRevision: 3, folderUid: '' })
      ).rejects.toMatchObject({
        code: 'IN_FLIGHT',
        message: t('save.errors.inFlight'),
      });
      await expect(
        saver.createNewDashboard({ spec, previewedRevision: 3, folderUid: '' })
      ).rejects.toBeInstanceOf(SaveDashboardError);

      resolveFirst();
      await first;
      expect(mockFetch).toHaveBeenCalledTimes(1);
    });

    test('listWritableFolders falls back to folder.grafana.app/v1beta1 when legacy /api/folders returns 404 (Grafana 13+)', async () => {
      mockConfig.namespace = 'org-7';
      mockFetch
        .mockReturnValueOnce(throwError(() => ({ status: 404, data: { message: 'Not Found' } })))
        .mockReturnValueOnce(
          of({
            data: {
              items: [
                { metadata: { name: 'folder-sre' }, spec: { title: 'SRE Team' } },
                { metadata: { name: 'folder-app' }, spec: { title: 'App Team' } },
              ],
            },
          })
        );

      const folders = await listWritableFolders();
      expect(folders).toEqual([
        { uid: '', title: t('save.folder.generalRoot') },
        { uid: 'folder-sre', title: 'SRE Team' },
        { uid: 'folder-app', title: 'App Team' },
      ]);
      expect(mockFetch).toHaveBeenCalledTimes(2);
      expect(mockFetch.mock.calls[0][0].url).toBe('/api/folders');
      expect(mockFetch.mock.calls[1][0].url).toBe(
        '/apis/folder.grafana.app/v1beta1/namespaces/org-7/folders'
      );
    });

    test('opt-in updateExistingDashboard sends PUT with metadata.resourceVersion and rejects missing resourceVersion or 409 conflict without force-overwriting', async () => {
      const saver = new DashboardSaver();

      // Missing resourceVersion MUST be rejected before sending any HTTP request.
      await expect(
        saver.updateExistingDashboard({
          spec,
          previewedRevision: 3,
          targetUid: 'existing-dash',
          expectedResourceVersion: '',
          folderUid: 'team-folder',
        })
      ).rejects.toMatchObject({
        code: 'MISSING_RESOURCE_VERSION',
        message: t('save.errors.missingResourceVersion'),
      });
      expect(mockFetch).not.toHaveBeenCalled();

      // Valid revision-checked PUT succeeds, preserves non-folder annotations/labels, and returns updated resourceVersion.
      mockFetch.mockReturnValueOnce(
        of({ data: { metadata: { name: 'existing-dash', resourceVersion: '18' } } })
      );
      const updated = await saver.updateExistingDashboard({
        spec,
        previewedRevision: 3,
        targetUid: 'existing-dash',
        expectedResourceVersion: '17',
        folderUid: 'team-folder',
        preservedAnnotations: {
          'grafana.app/managedBy': 'gitops',
          'grafana.app/folder': 'should-not-overwrite',
        },
        preservedLabels: { team: 'payments' },
      });
      expect(updated.uid).toBe('existing-dash');
      expect(updated.mode).toBe('updated');
      expect(updated.resourceVersion).toBe('18');

      const putCall = mockFetch.mock.calls[0][0];
      expect(putCall.method).toBe('PUT');
      expect(putCall.url).toBe(
        '/apis/dashboard.grafana.app/v1beta1/namespaces/default/dashboards/existing-dash'
      );
      expect(putCall.data.metadata.name).toBe('existing-dash');
      expect(putCall.data.metadata.resourceVersion).toBe('17');
      expect(putCall.data.metadata.annotations).toEqual({
        'grafana.app/managedBy': 'gitops',
        'grafana.app/folder': 'team-folder',
      });
      expect(putCall.data.metadata.labels).toEqual({ team: 'payments' });

      // Concurrent external modification (HTTP 409/412) fails with REVISION_CONFLICT and NEVER retries without resourceVersion.
      mockFetch.mockReturnValueOnce(
        throwError(() => ({ status: 409, data: { message: 'resourceVersion conflict' } }))
      );
      await expect(
        saver.updateExistingDashboard({
          spec,
          previewedRevision: 3,
          targetUid: 'existing-dash',
          expectedResourceVersion: '17',
          folderUid: 'team-folder',
        })
      ).rejects.toMatchObject({
        code: 'REVISION_CONFLICT',
        message: t('save.errors.revisionConflict', {
          status: 409,
          message: 'resourceVersion conflict',
        }),
      });
      expect(mockFetch).toHaveBeenCalledTimes(2);
    });
  }
);
