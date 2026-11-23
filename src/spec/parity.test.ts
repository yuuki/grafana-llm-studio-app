import fs from 'node:fs';
import path from 'node:path';

// Keep @grafana/scenes and @grafana/grafana-foundation-sdk 100% real;
// stub unrelated Grafana browser UI/runtime modules so Jest runs cleanly in jsdom.
jest.mock('@grafana/ui', () => ({
  useStyles2: () => ({}),
  useTheme2: () => ({}),
}));
jest.mock('@grafana/runtime', () => ({
  config: { featureToggles: {}, panels: {}, buildInfo: { version: '12.4.15' } },
  getDataSourceSrv: () => ({ getInstanceSettings: () => undefined }),
  locationService: { getLocation: () => ({ search: '', pathname: '/' }), partial: jest.fn() },
}));

import { SceneGridItem, SceneGridLayout, SceneQueryRunner, VizPanel } from '@grafana/scenes';
import { DashboardIR } from '../ir/types';
import { normalizeDashboardIR } from './normalize';
import { buildFoundationDashboard } from './toFoundation';
import { buildPreviewScene } from './toScene';

describe('Scenes and Foundation SDK adapter parity', () => {
  const samplePath = path.resolve(__dirname, '../../schemas/samples/valid-red-dashboard.json');
  const sampleIR = JSON.parse(fs.readFileSync(samplePath, 'utf8')) as DashboardIR;
  const resolvedDs = { uid: 'prom-main', type: 'prometheus', name: 'Prometheus Main' };

  test('preview Scene and saved Foundation SDK Dashboard match on query, unit, thresholds, datasource, and gridPos', () => {
    const spec = normalizeDashboardIR(sampleIR, resolvedDs);
    const scene = buildPreviewScene(spec);
    const foundationDash = buildFoundationDashboard(spec, { uid: 'llmstudio-abc123456' });

    expect(foundationDash.id).toBeNull();
    expect(foundationDash.uid).toBe('llmstudio-abc123456');
    expect(foundationDash.version).toBe(0);
    expect(foundationDash.refresh).toBe('');
    expect(foundationDash.schemaVersion).toBe(42);
    expect(foundationDash.time).toEqual({ from: 'now-15m', to: 'now' });

    const gridLayout = scene.state.body as SceneGridLayout;
    const sceneItems = gridLayout.state.children as SceneGridItem[];
    const foundationPanels = foundationDash.panels ?? [];

    expect(sceneItems).toHaveLength(3);
    expect(foundationPanels).toHaveLength(3);

    for (let i = 0; i < spec.panels.length; i++) {
      const sItem = sceneItems[i];
      const fPanel = foundationPanels[i] as {
        title?: string;
        type?: string;
        datasource?: { uid?: string; type?: string };
        gridPos?: { x: number; y: number; w: number; h: number };
        fieldConfig?: {
          defaults?: {
            unit?: string;
            thresholds?: { mode?: string; steps?: Array<{ value: number | null; color: string }> };
          };
        };
        targets?: Array<{ refId?: string; expr?: string; legendFormat?: string; datasource?: { uid?: string } }>;
      };

      // Grid layout parity
      expect(fPanel.gridPos).toEqual({
        x: sItem.state.x,
        y: sItem.state.y,
        w: sItem.state.width,
        h: sItem.state.height,
      });

      const viz = sItem.state.body as VizPanel;
      const qr = viz.state.$data as SceneQueryRunner;
      const sceneQuery = qr.state.queries[0] as { refId: string; expr: string; legendFormat?: string };

      // Title & type parity
      expect(fPanel.title).toBe(viz.state.title);
      expect(fPanel.type).toBe(viz.state.pluginId);

      // Datasource & query target parity
      expect(fPanel.datasource?.uid).toBe(qr.state.datasource?.uid);
      expect(fPanel.datasource?.uid).toBe('prom-main');
      expect(fPanel.targets?.[0]?.refId).toBe(sceneQuery.refId);
      expect(fPanel.targets?.[0]?.expr).toBe(sceneQuery.expr);
      expect(fPanel.targets?.[0]?.legendFormat).toBe(sceneQuery.legendFormat);

      // Unit parity
      expect(fPanel.fieldConfig?.defaults?.unit).toBe(viz.state.fieldConfig.defaults.unit);

      // Threshold step count & color/value parity
      const sceneSteps = viz.state.fieldConfig.defaults.thresholds?.steps ?? [];
      const foundSteps = fPanel.fieldConfig?.defaults?.thresholds?.steps ?? [];
      expect(foundSteps).toHaveLength(sceneSteps.length);
      for (let t = 0; t < foundSteps.length; t++) {
        expect(foundSteps[t].color).toBe(sceneSteps[t].color);
        if (t === 0) {
          expect(foundSteps[t].value).toBeNull();
          expect(sceneSteps[t].value).toBe(-Infinity);
        } else {
          expect(foundSteps[t].value).toBe(sceneSteps[t].value);
        }
      }
    }
  });

  test('rejects invalid IR with unauthorized datasourceRef before reaching adapters', () => {
    const tampered = JSON.parse(JSON.stringify(sampleIR)) as DashboardIR;
    (tampered.panels[0] as unknown as { datasourceRef: string }).datasourceRef = 'other-prom-uid';
    expect(() => normalizeDashboardIR(tampered, resolvedDs)).toThrow(/Forbidden datasourceRef/);
  });
});
