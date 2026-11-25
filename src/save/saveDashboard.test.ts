import { Observable, of, throwError } from 'rxjs';
import { DEFAULT_LOCALE, setStudioLocale, SUPPORTED_LOCALES, t } from '../i18n';
import { DashboardIR } from '../ir/types';
import { normalizeDashboardIR } from '../spec/normalize';
import { convertGrafanaDashboardToIR } from '../import/importDashboard';
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

    test('opt-in updateExistingDashboard requires resourceVersion and originalSpec patchContext, and rejects 409 conflict without force-overwriting', async () => {
      const saver = new DashboardSaver();
      const imported = convertGrafanaDashboardToIR(
        {
          title: 'checkout-api Dashboard',
          time: { from: 'now-15m', to: 'now' },
          tags: ['prod', 'payments'],
          annotations: { list: [{ name: 'Deploys', enable: true }] },
          panels: [
            {
              id: 7,
              type: 'timeseries',
              title: 'Request Rate',
              datasource: { type: 'prometheus', uid: 'prom-main' },
              gridPos: { x: 0, y: 0, w: 24, h: 9 },
              transformations: [{ id: 'organize', options: { excludeByName: { Time: false } } }],
              fieldConfig: {
                defaults: { unit: 'reqps', decimals: 2 },
                overrides: [{ matcher: { id: 'byName', options: '500' }, properties: [{ id: 'color', value: { mode: 'fixed', fixedColor: 'red' } }] }],
              },
              targets: [
                {
                  refId: 'A',
                  expr: 'sum(rate(http_requests_total{service="checkout-api"}[5m]))',
                  exemplar: true,
                },
              ],
            },
          ],
        },
        { selectedDatasourceUid: 'prom-main' }
      );
      const editedIR: DashboardIR = {
        ...imported.draft,
        revision: 3,
        title: 'checkout-api Dashboard (Renamed)',
      };
      const editedSpec = normalizeDashboardIR(editedIR, { uid: 'prom-main', type: 'prometheus' });

      // Missing resourceVersion MUST be rejected before sending any HTTP request.
      await expect(
        saver.updateExistingDashboard({
          spec: editedSpec,
          currentIR: editedIR,
          patchContext: imported.patchContext,
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

      // Missing patchContext/originalSpec MUST be rejected fail-closed before sending any HTTP request.
      await expect(
        saver.updateExistingDashboard({
          spec: editedSpec,
          currentIR: editedIR,
          patchContext: undefined as never,
          previewedRevision: 3,
          targetUid: 'existing-dash',
          expectedResourceVersion: '17',
          folderUid: 'team-folder',
        })
      ).rejects.toMatchObject({
        code: 'MISSING_ORIGINAL_SPEC',
        message: t('save.errors.missingOriginalSpec'),
      });
      expect(mockFetch).not.toHaveBeenCalled();

      // Valid revision-checked PUT succeeds, preserves non-folder annotations/labels AND untouched panel settings (transformations, overrides, decimals, tags, annotations).
      mockFetch.mockReturnValueOnce(
        of({ data: { metadata: { name: 'existing-dash', resourceVersion: '18' } } })
      );
      const updated = await saver.updateExistingDashboard({
        spec: editedSpec,
        currentIR: editedIR,
        patchContext: imported.patchContext,
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

      // Title changed, while untouched panel transformations, fieldConfig.overrides, decimals, gridPos, tags, and dashboard annotations are preserved verbatim.
      expect(putCall.data.spec.title).toBe('checkout-api Dashboard (Renamed)');
      expect(putCall.data.spec.tags).toEqual(['prod', 'payments']);
      expect(putCall.data.spec.annotations).toEqual({ list: [{ name: 'Deploys', enable: true }] });
      expect(putCall.data.spec.panels[0].transformations).toEqual([
        { id: 'organize', options: { excludeByName: { Time: false } } },
      ]);
      expect(putCall.data.spec.panels[0].fieldConfig.overrides).toHaveLength(1);
      expect(putCall.data.spec.panels[0].fieldConfig.defaults.decimals).toBe(2);
      expect(putCall.data.spec.panels[0].gridPos).toEqual({ x: 0, y: 0, w: 24, h: 9 });
      expect(putCall.data.spec.panels[0].targets[0].exemplar).toBe(true);

      // Concurrent external modification (HTTP 409/412) fails with REVISION_CONFLICT and NEVER retries without resourceVersion.
      mockFetch.mockReturnValueOnce(
        throwError(() => ({ status: 409, data: { message: 'resourceVersion conflict' } }))
      );
      await expect(
        saver.updateExistingDashboard({
          spec: editedSpec,
          currentIR: editedIR,
          patchContext: imported.patchContext,
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

    test('surgical update preserves unsupported panels, panels beyond maxPanels, collapsed rows, skipped variables, and hidden targets when patching edited panels', async () => {
      const saver = new DashboardSaver();
      const rawDashboard = {
        uid: 'complex-prod-dash',
        title: 'Production Overview',
        refresh: '30s',
        time: { from: 'now-24h', to: 'now' },
        templating: {
          list: [
            { name: 'region', type: 'constant', query: 'ap-northeast-1', hide: 2 },
            {
              name: 'status_class',
              type: 'custom',
              query: '5..,4..',
              options: [
                { text: '5..', value: '5..', selected: true },
                { text: '4..', value: '4..', selected: false },
              ],
              current: { text: '5..', value: '5..' },
            },
          ],
        },
        panels: [
          {
            id: 1,
            type: 'geomap',
            title: 'Geo Traffic Map',
            gridPos: { x: 0, y: 0, w: 12, h: 8 },
            options: { view: { id: 'zero' } },
          },
          {
            id: 2,
            type: 'timeseries',
            title: 'Request Rate',
            datasource: { type: 'prometheus', uid: 'prom-main' },
            gridPos: { x: 12, y: 0, w: 12, h: 8 },
            transformations: [{ id: 'calculateField', options: { mode: 'binary' } }],
            fieldConfig: {
              defaults: { unit: 'reqps', min: 0 },
              overrides: [{ matcher: { id: 'byName', options: 'err' }, properties: [] }],
            },
            targets: [
              { refId: 'A', expr: 'sum(rate(http_requests_total{service="checkout-api"}[5m]))', interval: '30s' },
              { refId: 'H', hide: true, expr: 'hidden_debug_metric' },
              { refId: 'L', datasource: { type: 'loki', uid: 'loki-main' }, expr: '{app="checkout"}' },
            ],
          },
          {
            id: 3,
            type: 'row',
            title: 'Collapsed Latency Row',
            collapsed: true,
            gridPos: { x: 0, y: 8, w: 24, h: 1 },
            panels: [
              {
                id: 4,
                type: 'stat',
                title: 'P95 Latency',
                datasource: { type: 'prometheus', uid: 'prom-main' },
                gridPos: { x: 0, y: 9, w: 12, h: 6 },
                fieldConfig: { defaults: { unit: 'currencyUSD' } },
                targets: [{ refId: 'A', expr: 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="checkout-api"}[5m])))' }],
              },
            ],
          },
          {
            id: 5,
            type: 'stat',
            title: 'Overflow Panel Beyond Max',
            datasource: { type: 'prometheus', uid: 'prom-main' },
            gridPos: { x: 0, y: 16, w: 24, h: 6 },
            targets: [{ refId: 'A', expr: 'up{service="checkout-api"}' }],
          },
        ],
      };

      // Import with maxPanels=2 so panel id=5 (Overflow) and panel id=1 (geomap) are skipped,
      // and variable `region` (hide=2 constant) is skipped.
      const imported = convertGrafanaDashboardToIR(rawDashboard, {
        selectedDatasourceUid: 'prom-main',
        maxPanels: 2,
      });
      expect(imported.report.importedPanelCount).toBe(2);
      expect(imported.report.skippedPanels.map((p) => p.title)).toEqual([
        'Geo Traffic Map',
        'Overflow Panel Beyond Max',
      ]);

      // Edit ONLY panel 2's PromQL query, add a brand new panel, and leave panel 4 (collapsed row) and timeRange untouched.
      const editedIR: DashboardIR = {
        ...imported.draft,
        revision: 2,
        panels: [
          {
            ...imported.draft.panels[0],
            query: {
              expr: 'sum(rate(http_requests_total{service="checkout-api"}[10m]))',
            },
          },
          imported.draft.panels[1],
          {
            id: 'panel-new-cpu',
            type: 'gauge',
            title: 'CPU Saturation',
            datasourceRef: 'selected_prometheus',
            query: { expr: 'avg(rate(process_cpu_seconds_total{service="checkout-api"}[5m]))' },
            unit: 'percentunit',
            layout: { row: 2, col: 0, width: 'half' },
          },
        ],
      };
      const editedSpec = normalizeDashboardIR(editedIR, { uid: 'prom-main', type: 'prometheus' }, 6);

      mockFetch.mockReturnValueOnce(
        of({ data: { metadata: { name: 'complex-prod-dash', resourceVersion: '44' } } })
      );
      await saver.updateExistingDashboard({
        spec: editedSpec,
        currentIR: editedIR,
        patchContext: imported.patchContext,
        previewedRevision: 2,
        targetUid: 'complex-prod-dash',
        expectedResourceVersion: '43',
        folderUid: '',
      });

      const sentSpec = mockFetch.mock.calls[0][0].data.spec;

      // Untouched dashboard refresh and original now-24h timeRange preserved!
      expect(sentSpec.refresh).toBe('30s');
      expect(sentSpec.time).toEqual({ from: 'now-24h', to: 'now' });
      // Hidden constant variable preserved alongside custom variable!
      expect(sentSpec.templating.list.map((v: { name: string }) => v.name)).toEqual([
        'region',
        'status_class',
      ]);
      // Unsupported geomap panel, overflow panel, and collapsed row all preserved!
      expect(sentSpec.panels).toHaveLength(5);
      expect(sentSpec.panels[0]).toMatchObject({ id: 1, type: 'geomap', title: 'Geo Traffic Map' });
      // Edited panel 2 updated its PromQL expr while keeping transformations, overrides, min:0, interval:'30s', hidden target H, and Loki target L!
      expect(sentSpec.panels[1].transformations).toEqual([
        { id: 'calculateField', options: { mode: 'binary' } },
      ]);
      expect(sentSpec.panels[1].fieldConfig.overrides).toHaveLength(1);
      expect(sentSpec.panels[1].fieldConfig.defaults.min).toBe(0);
      expect(sentSpec.panels[1].targets).toHaveLength(3);
      expect(sentSpec.panels[1].targets[0]).toMatchObject({
        refId: 'A',
        expr: 'sum(rate(http_requests_total{service="checkout-api"}[10m]))',
        interval: '30s',
      });
      expect(sentSpec.panels[1].targets[1]).toMatchObject({ refId: 'H', hide: true });
      expect(sentSpec.panels[1].targets[2]).toMatchObject({ refId: 'L', datasource: { type: 'loki', uid: 'loki-main' } });
      // Untouched panel 4 inside collapsed row kept its original custom unit `currencyUSD`!
      expect(sentSpec.panels[2].panels[0].fieldConfig.defaults.unit).toBe('currencyUSD');
      // Skipped overflow panel id=5 preserved verbatim!
      expect(sentSpec.panels[3]).toMatchObject({ id: 5, title: 'Overflow Panel Beyond Max' });
      // Newly added panel received non-colliding numeric id=6 and non-overlapping gridPos.y >= 22 (below overflow panel y=16+h=6)!
      expect(sentSpec.panels[4]).toMatchObject({
        id: 6,
        type: 'gauge',
        title: 'CPU Saturation',
        gridPos: { x: 0, y: 22, w: 12, h: 6 },
      });
    });
  }
);
