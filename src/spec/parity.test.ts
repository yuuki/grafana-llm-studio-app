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

import {
  CustomVariable,
  QueryVariable,
  SceneGridItem,
  SceneGridLayout,
  SceneQueryRunner,
  SceneTimeRange,
  SceneVariableSet,
  VariableValueSelectors,
  VizPanel,
} from '@grafana/scenes';
import { DashboardIR, validateIRClient } from '../ir/types';
import { interpolatePromQLVariables, normalizeDashboardIR } from './normalize';
import { buildFoundationDashboard } from './toFoundation';
import { buildPreviewScene } from './toScene';

describe('Scenes and Foundation SDK adapter parity', () => {
  const samplePath = path.resolve(__dirname, '../../schemas/samples/valid-red-dashboard.json');
  const sampleIR = JSON.parse(fs.readFileSync(samplePath, 'utf8')) as DashboardIR;
  const resolvedDs = { uid: 'prom-main', type: 'prometheus', name: 'Prometheus Main' };

  test('preview Scene and saved Foundation SDK Dashboard match on id, query, options, unit, thresholds, datasource, and gridPos', () => {
    const spec = normalizeDashboardIR(sampleIR, resolvedDs);
    const scene = buildPreviewScene(spec);
    const foundationDash = buildFoundationDashboard(spec, { uid: 'llmstudio-abc123456' });

    expect(foundationDash.id).toBeNull();
    expect(foundationDash.uid).toBe('llmstudio-abc123456');
    expect(foundationDash.version).toBe(0);
    expect(foundationDash.refresh).toBe('');
    expect(foundationDash.schemaVersion).toBe(42);
    expect(foundationDash.time).toEqual({ from: 'now-15m', to: 'now' });

    // H-8: SceneTimeRange must disable implicit refreshOnActivate re-querying.
    const timeRange = scene.state.$timeRange as SceneTimeRange;
    expect(timeRange.state.refreshOnActivate).toEqual({});

    const gridLayout = scene.state.body as SceneGridLayout;
    const sceneItems = gridLayout.state.children as SceneGridItem[];
    const foundationPanels = foundationDash.panels ?? [];

    expect(sceneItems).toHaveLength(3);
    expect(foundationPanels).toHaveLength(3);

    for (let i = 0; i < spec.panels.length; i++) {
      const sItem = sceneItems[i];
      const fPanel = foundationPanels[i] as {
        id?: number;
        title?: string;
        type?: string;
        datasource?: { uid?: string; type?: string };
        gridPos?: { x: number; y: number; w: number; h: number };
        options?: {
          legend?: { showLegend?: boolean; displayMode?: string; placement?: string };
          reduceOptions?: { calcs?: string[]; values?: boolean };
        };
        fieldConfig?: {
          defaults?: {
            unit?: string;
            thresholds?: { mode?: string; steps?: Array<{ value: number | null; color: string }> };
          };
        };
        targets?: Array<{ refId?: string; expr?: string; legendFormat?: string; datasource?: { uid?: string } }>;
      };

      // H-12: Explicit numeric panel id
      expect(fPanel.id).toBe(spec.panels[i].numericId);

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

      // H-12: Explicit panel options parity (timeseries legend / stat reduceOptions)
      const sceneOptions = viz.state.options as {
        legend?: { showLegend?: boolean; displayMode?: string; placement?: string };
        reduceOptions?: { calcs?: string[]; values?: boolean };
      };
      if (fPanel.type === 'timeseries') {
        expect(fPanel.options?.legend?.showLegend).toBe(true);
        expect(fPanel.options?.legend?.displayMode).toBe('list');
        expect(fPanel.options?.legend?.placement).toBe('bottom');
        expect(sceneOptions?.legend).toEqual(fPanel.options?.legend);
      } else if (fPanel.type === 'stat') {
        expect(fPanel.options?.reduceOptions?.calcs).toEqual(['lastNotNull']);
        expect(fPanel.options?.reduceOptions?.values).toBe(false);
        expect(sceneOptions?.reduceOptions?.calcs).toEqual(['lastNotNull']);
        expect(sceneOptions?.reduceOptions?.values).toBe(false);
      }

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

  test('preserves non-zero first threshold step by prepending green base step (H-12)', () => {
    const irWithNonZeroThresholds: DashboardIR = {
      schemaVersion: '1.0',
      revision: 1,
      title: 'Threshold Test',
      timeRange: { from: 'now-15m', to: 'now' },
      panels: [
        {
          id: 'panel-lat',
          type: 'stat',
          title: 'Latency',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))' },
          unit: 's',
          thresholds: [
            { value: 0.5, color: 'yellow' },
            { value: 1.0, color: 'red' },
          ],
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };

    const spec = normalizeDashboardIR(irWithNonZeroThresholds, resolvedDs);
    expect(spec.panels[0].thresholds).toEqual([
      { value: null, color: 'green' },
      { value: 0.5, color: 'yellow' },
      { value: 1.0, color: 'red' },
    ]);

    const foundationDash = buildFoundationDashboard(spec, { uid: 'llmstudio-thresh' });
    const fSteps = (
      foundationDash.panels?.[0] as {
        fieldConfig?: { defaults?: { thresholds?: { steps?: Array<{ value: number | null; color: string }> } } };
      }
    )?.fieldConfig?.defaults?.thresholds?.steps;
    expect(fSteps).toEqual([
      { value: null, color: 'green' },
      { value: 0.5, color: 'yellow' },
      { value: 1.0, color: 'red' },
    ]);
  });

  test('rejects invalid IR with unauthorized datasourceRef or overlapping layout before reaching adapters', () => {
    const tampered = JSON.parse(JSON.stringify(sampleIR)) as DashboardIR;
    (tampered.panels[0] as unknown as { datasourceRef: string }).datasourceRef = 'other-prom-uid';
    expect(() => normalizeDashboardIR(tampered, resolvedDs)).toThrow(/Forbidden datasourceRef/);

    const overlapping = JSON.parse(JSON.stringify(sampleIR)) as DashboardIR;
    overlapping.panels[1].layout = { ...overlapping.panels[0].layout };
    expect(() => normalizeDashboardIR(overlapping, resolvedDs)).toThrow(/Layout overlap/);
  });

  test('IR v1.1 full parity across timeseries, gauge, bargauge, piechart, table, state-timeline, multi-query (3 targets), and variables', () => {
    const v11Path = path.resolve(__dirname, '../../schemas/samples/valid-v11-dashboard.json');
    const v11IR = JSON.parse(fs.readFileSync(v11Path, 'utf8')) as DashboardIR;
    const spec = normalizeDashboardIR(v11IR, resolvedDs);
    const scene = buildPreviewScene(spec);
    const foundationDash = buildFoundationDashboard(spec, { uid: 'llmstudio-v11-parity' });

    // Variable parity between Scenes SceneVariableSet and Foundation SDK templating.list
    const sceneVarSet = scene.state.$variables as SceneVariableSet;
    expect(sceneVarSet).toBeInstanceOf(SceneVariableSet);
    expect(scene.state.controls?.[0]).toBeInstanceOf(VariableValueSelectors);

    const sceneVars = sceneVarSet.state.variables;
    const foundationVars = foundationDash.templating?.list ?? [];
    expect(sceneVars).toHaveLength(2);
    expect(foundationVars).toHaveLength(2);

    const sQueryVar = sceneVars[0] as QueryVariable;
    const fQueryVar = foundationVars[0];
    expect(sQueryVar.state.name).toBe('service');
    expect(fQueryVar.name).toBe('service');
    expect(fQueryVar.type).toBe('query');
    expect(sQueryVar.state.label).toBe(fQueryVar.label);
    expect(sQueryVar.state.query).toBe('label_values(http_requests_total, service)');
    expect(fQueryVar.query).toBe(sQueryVar.state.query);
    expect(sQueryVar.state.datasource?.uid).toBe('prom-main');
    expect(fQueryVar.datasource?.uid).toBe('prom-main');
    expect(sQueryVar.state.value).toBe('checkout-api');
    expect(fQueryVar.current?.value).toBe('checkout-api');

    const sCustomVar = sceneVars[1] as CustomVariable;
    const fCustomVar = foundationVars[1];
    expect(sCustomVar.state.name).toBe('status_class');
    expect(fCustomVar.name).toBe('status_class');
    expect(fCustomVar.type).toBe('custom');
    expect(sCustomVar.state.query).toBe('5..,4..|5..');
    expect(fCustomVar.query).toBe(sCustomVar.state.query);
    expect(fCustomVar.current?.value).toBe('5..');

    // Panel parity across all 6 panels in v1.1 fixture (including 3-query timeseries + 5 new panel types)
    const gridLayout = scene.state.body as SceneGridLayout;
    const sceneItems = gridLayout.state.children as SceneGridItem[];
    const foundationPanels = foundationDash.panels ?? [];

    expect(sceneItems).toHaveLength(6);
    expect(foundationPanels).toHaveLength(6);

    for (let i = 0; i < spec.panels.length; i++) {
      const sItem = sceneItems[i];
      const viz = sItem.state.body as VizPanel;
      const qr = viz.state.$data as SceneQueryRunner;
      const fPanel = foundationPanels[i] as {
        id?: number;
        title?: string;
        type?: string;
        gridPos?: { x: number; y: number; w: number; h: number };
        options?: Record<string, unknown>;
        targets?: Array<{ refId?: string; expr?: string; legendFormat?: string; datasource?: { uid?: string } }>;
        fieldConfig?: { defaults?: { unit?: string } };
      };

      expect(fPanel.id).toBe(spec.panels[i].numericId);
      expect(fPanel.type).toBe(viz.state.pluginId);
      expect(fPanel.title).toBe(viz.state.title);
      expect(fPanel.fieldConfig?.defaults?.unit).toBe(viz.state.fieldConfig.defaults.unit);
      expect(fPanel.gridPos).toEqual({
        x: sItem.state.x,
        y: sItem.state.y,
        w: sItem.state.width,
        h: sItem.state.height,
      });

      // Multi-query target parity (panel 0 has 3 queries A/B/C)
      const sQueries = qr.state.queries as Array<{ refId: string; expr: string; legendFormat?: string }>;
      expect(fPanel.targets).toHaveLength(sQueries.length);
      for (let q = 0; q < sQueries.length; q++) {
        expect(fPanel.targets?.[q]?.refId).toBe(sQueries[q].refId);
        expect(fPanel.targets?.[q]?.expr).toBe(sQueries[q].expr);
        expect(fPanel.targets?.[q]?.legendFormat).toBe(sQueries[q].legendFormat);
        expect(fPanel.targets?.[q]?.datasource?.uid).toBe('prom-main');
      }

      // Panel-type-specific options parity
      const sOpts = viz.state.options as Record<string, unknown>;
      const fOpts = fPanel.options ?? {};
      switch (fPanel.type) {
        case 'timeseries':
          expect(sQueries).toHaveLength(3);
          expect(sOpts.legend).toEqual(fOpts.legend);
          break;
        case 'gauge':
          expect(sOpts.reduceOptions).toMatchObject({ calcs: ['lastNotNull'], values: false });
          expect(fOpts.reduceOptions).toMatchObject({ calcs: ['lastNotNull'], values: false });
          expect(sOpts.showThresholdLabels).toBe(fOpts.showThresholdLabels);
          expect(sOpts.showThresholdMarkers).toBe(fOpts.showThresholdMarkers);
          expect(sOpts.orientation).toBe(fOpts.orientation);
          break;
        case 'bargauge':
          expect(sOpts.displayMode).toBe('gradient');
          expect(fOpts.displayMode).toBe('gradient');
          expect(sOpts.orientation).toBe('horizontal');
          expect(fOpts.orientation).toBe('horizontal');
          expect(sOpts.showUnfilled).toBe(fOpts.showUnfilled);
          expect(sOpts.reduceOptions).toMatchObject({ calcs: ['lastNotNull'], values: false });
          expect(fOpts.reduceOptions).toMatchObject({ calcs: ['lastNotNull'], values: false });
          break;
        case 'piechart':
          expect(sOpts.pieType).toBe('pie');
          expect(fOpts.pieType).toBe('pie');
          expect(sOpts.legend).toMatchObject({
            showLegend: true,
            displayMode: 'list',
            placement: 'bottom',
          });
          expect(fOpts.legend).toMatchObject({
            showLegend: true,
            displayMode: 'list',
            placement: 'bottom',
          });
          break;
        case 'table':
          expect(sOpts.frameIndex).toBe(0);
          expect(fOpts.frameIndex).toBe(0);
          expect(sOpts.showHeader).toBe(true);
          expect(fOpts.showHeader).toBe(true);
          expect(sOpts.showTypeIcons).toBe(false);
          expect(fOpts.showTypeIcons).toBe(false);
          expect(sOpts.cellHeight).toBe('sm');
          expect(fOpts.cellHeight).toBe('sm');
          break;
        case 'state-timeline':
          expect(sOpts.mergeValues).toBe(true);
          expect(fOpts.mergeValues).toBe(true);
          expect(sOpts.showValue).toBe('auto');
          expect(fOpts.showValue).toBe('auto');
          expect(sOpts.rowHeight).toBe(0.9);
          expect(fOpts.rowHeight).toBe(0.9);
          expect(sOpts.legend).toEqual(fOpts.legend);
          break;
        default:
          throw new Error(`Unexpected panel type in v1.1 fixture: ${fPanel.type}`);
      }
    }
  });

  test('validateIRClient enforces IR v1.1 multi-query and variable bounds', () => {
    const badDoc: DashboardIR = {
      schemaVersion: '1.1',
      revision: 1,
      title: 'Bad v1.1',
      timeRange: { from: 'now-15m', to: 'now' },
      variables: [
        { name: '__from', type: 'custom', options: ['a'] },
      ],
      panels: [
        {
          id: 'panel-a',
          type: 'timeseries',
          title: 'Too many queries',
          datasourceRef: 'selected_prometheus',
          queries: [
            { refId: 'A', expr: 'up' },
            { refId: 'B', expr: 'up' },
            { refId: 'C', expr: 'up' },
            { expr: 'up' },
          ],
          unit: 'short',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };
    const issues = validateIRClient(badDoc);
    expect(issues.some((m) => m.includes('reserved'))).toBe(true);
    expect(issues.some((m) => m.includes('max 3 queries'))).toBe(true);

    // Adversarial client validation checks: HTML/Bidi/quotes/legend var token/empty queries
    const adversarialDoc: DashboardIR = {
      schemaVersion: '1.1',
      revision: 1,
      title: 'Safe <img src=x>',
      timeRange: { from: 'now-15m', to: 'now' },
      variables: [
        {
          name: '__custom_var',
          type: 'query',
          datasourceRef: 'selected_prometheus',
          labelName: 'service',
          defaultValue: '',
        },
        {
          name: 'env',
          type: 'custom',
          options: ['prod,staging'],
          defaultValue: 'prod,staging',
        },
      ],
      panels: [
        {
          id: 'panel-a',
          type: 'timeseries',
          title: 'Panel A',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'up', legendFormat: '{{instance}} $env' },
          queries: [],
          unit: 'short',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };
    const advIssues = validateIRClient(adversarialDoc);
    expect(advIssues.some((m) => m.includes('angle brackets'))).toBe(true);
    expect(advIssues.some((m) => m.includes('__custom_var'))).toBe(true);
    expect(advIssues.some((m) => m.includes('comma'))).toBe(true);
    expect(advIssues.some((m) => m.includes('both query and queries'))).toBe(true);
  });

  test('multi + includeAll variable parity and single-pass PromQL interpolation escaping (H-1, M-2)', () => {
    const multiVarDoc: DashboardIR = {
      schemaVersion: '1.1',
      revision: 1,
      title: 'Multi Var Parity',
      timeRange: { from: 'now-15m', to: 'now' },
      variables: [
        {
          name: 'host',
          type: 'query',
          datasourceRef: 'selected_prometheus',
          labelName: 'instance',
          metricName: 'up',
          defaultValue: 'api.v1',
          multi: true,
          includeAll: true,
        },
        {
          name: 'tier',
          type: 'custom',
          options: ['web', 'worker'],
          defaultValue: 'web',
          multi: true,
          includeAll: true,
        },
      ],
      panels: [
        {
          id: 'panel-up',
          type: 'stat',
          title: 'Up',
          datasourceRef: 'selected_prometheus',
          query: { expr: 'avg(up{instance=~"${host:regex}",tier=~"${tier:pipe}"})' },
          unit: 'short',
          layout: { row: 0, col: 0, width: 'full' },
        },
      ],
    };

    const spec = normalizeDashboardIR(multiVarDoc, { uid: 'prom-main', type: 'prometheus' });
    const scene = buildPreviewScene(spec);
    const foundationDash = buildFoundationDashboard(spec, { uid: 'llmstudio-multi-var' });

    const sceneVars = (scene.state.$variables as SceneVariableSet).state.variables;
    const foundationVars = foundationDash.templating?.list ?? [];

    // Multi variables must use array initial values in both Scenes and Foundation SDK
    expect((sceneVars[0] as QueryVariable).state.value).toEqual(['api.v1']);
    expect(foundationVars[0].current?.value).toEqual(['api.v1']);
    expect(foundationVars[0].allowCustomValue).toBe(false);

    expect((sceneVars[1] as CustomVariable).state.value).toEqual(['web']);
    expect(foundationVars[1].current?.value).toEqual(['web']);
    expect(foundationVars[1].allowCustomValue).toBe(false);
    expect(foundationVars[1].options?.[0]).toEqual({ text: 'All', value: '$__all', selected: false });

    // Single-pass interpolation escapes RE2 dots + PromQL backslashes (\\. not \.)
    expect(
      interpolatePromQLVariables('up{instance=~"${host:regex}",tier=~"${tier:pipe}"}', spec.variables)
    ).toBe('up{instance=~"api\\\\.v1",tier=~"web"}');

    // Single-pass prevents secondary expansion ($a -> "$b" must NOT expand $b)
    expect(
      interpolatePromQLVariables('up{a="$a",b="$b"}', [
        { name: 'a', label: 'a', type: 'custom', query: '$b', options: ['$b'], defaultValue: '$b', multi: false, includeAll: false },
        { name: 'b', label: 'b', type: 'custom', query: 'pwned', options: ['pwned'], defaultValue: 'pwned', multi: false, includeAll: false },
      ])
    ).toBe('up{a="$b",b="pwned"}');
  });
});
