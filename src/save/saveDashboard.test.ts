import { Observable, of, throwError } from 'rxjs';
import { DashboardIR } from '../ir/types';
import { normalizeDashboardIR } from '../spec/normalize';
import { DashboardSaver, SaveDashboardError } from './saveDashboard';

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

describe('Create-only DashboardSaver (Grafana 12.4.15 v1beta1)', () => {
  const spec = normalizeDashboardIR(sampleIR, { uid: 'prom-main', type: 'prometheus' });

  beforeEach(() => {
    mockFetch.mockReset();
    mockConfig.namespace = undefined;
    mockConfig.appSubUrl = '';
    mockConfig.bootData.user.orgId = 1;
  });

  test('rejects saving when revision does not match previewedRevision', async () => {
    const saver = new DashboardSaver();
    await expect(
      saver.createNewDashboard({ spec, previewedRevision: 2, folderUid: 'folder-1' })
    ).rejects.toMatchObject({ code: 'REVISION_MISMATCH' });
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
    ).rejects.toMatchObject({ code: 'UID_CONFLICT' });
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  test('on 403 permission denied, surfaces PERMISSION_DENIED', async () => {
    mockFetch.mockReturnValueOnce(throwError(() => ({ status: 403, data: { message: 'Forbidden' } })));
    const saver = new DashboardSaver();
    await expect(
      saver.createNewDashboard({ spec, previewedRevision: 3, folderUid: 'readonly-folder' })
    ).rejects.toMatchObject({ code: 'PERMISSION_DENIED' });
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
    ).rejects.toBeInstanceOf(SaveDashboardError);

    resolveFirst();
    await first;
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });
});
