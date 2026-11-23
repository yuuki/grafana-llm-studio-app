import { ThresholdsMode, VariableRefresh, VariableSort } from '@grafana/data';
import {
  CustomVariable,
  EmbeddedScene,
  PanelBuilders,
  QueryVariable,
  SceneGridItem,
  SceneGridLayout,
  SceneQueryRunner,
  SceneTimeRange,
  SceneVariable,
  SceneVariableSet,
  VariableValueSelectors,
} from '@grafana/scenes';
import {
  BarGaugeDisplayMode,
  LegendDisplayMode,
  TableCellHeight,
  VariableHide,
  VisibilityMode,
  VizOrientation,
} from '@grafana/schema';
import {
  NormalizedDashboardSpec,
  NormalizedPanelSpec,
  NormalizedVariableSpec,
} from './normalize';

export interface SceneAdapterOptions {
  /**
   * Optional maxDataPoints cap for preview queries (default 200).
   */
  maxDataPoints?: number;
}

/**
 * Thin adapter converting NormalizedDashboardSpec into a public-API @grafana/scenes EmbeddedScene.
 * - Uses ONLY public @grafana/scenes APIs (never internal DashboardScene).
 * - Disables auto-refresh AND disables refreshOnActivate ({}) so re-activation never
 *   silently re-evaluates relative timeRange or re-issues Prometheus queries (H-8).
 * - Mirrors panel options, multi-query targets, and template variables identically to toFoundation.ts (H-12).
 */
export function buildPreviewScene(
  spec: NormalizedDashboardSpec,
  options: SceneAdapterOptions = {}
): EmbeddedScene {
  const maxDataPoints = options.maxDataPoints ?? 200;

  const gridItems = spec.panels.map((panel) => buildGridItem(panel, maxDataPoints));
  const sceneVariables = spec.variables.map(buildSceneVariable);

  const sceneState: ConstructorParameters<typeof EmbeddedScene>[0] = {
    $timeRange: new SceneTimeRange({
      from: spec.timeRange.from,
      to: spec.timeRange.to,
      refreshOnActivate: {},
    }),
    body: new SceneGridLayout({
      isDraggable: false,
      isResizable: false,
      children: gridItems,
    }),
  };

  if (sceneVariables.length > 0) {
    sceneState.$variables = new SceneVariableSet({
      variables: sceneVariables,
    });
    sceneState.controls = [new VariableValueSelectors({})];
  }

  return new EmbeddedScene(sceneState);
}

type PieChartPieType = NonNullable<
  ReturnType<ReturnType<typeof PanelBuilders.piechart>['build']>['state']['options']['pieType']
>;

function buildSceneVariable(v: NormalizedVariableSpec): SceneVariable {
  const initialVal = v.multi ? (v.defaultValue ? [v.defaultValue] : []) : v.defaultValue;
  const initialText = v.multi ? (v.defaultValue ? [v.defaultValue] : []) : v.defaultValue;

  if (v.type === 'query') {
    return new QueryVariable({
      name: v.name,
      label: v.label,
      datasource: v.datasource ?? null,
      query: v.query,
      value: initialVal,
      text: initialText,
      isMulti: v.multi,
      includeAll: v.includeAll,
      refresh: VariableRefresh.onDashboardLoad,
      sort: VariableSort.alphabeticalAsc,
      hide: VariableHide.dontHide,
      skipUrlSync: true,
    });
  }

  return new CustomVariable({
    name: v.name,
    label: v.label,
    query: v.query,
    value: initialVal,
    text: initialText,
    isMulti: v.multi,
    includeAll: v.includeAll,
    hide: VariableHide.dontHide,
    skipUrlSync: true,
  });
}

function buildGridItem(panel: NormalizedPanelSpec, maxDataPoints: number): SceneGridItem {
  const queryRunner = new SceneQueryRunner({
    datasource: panel.datasource,
    maxDataPoints,
    queries: panel.targets.map((target) => ({
      refId: target.refId,
      expr: target.expr,
      legendFormat: target.legendFormat,
      datasource: target.datasource,
    })),
  });

  const thresholdSteps = panel.thresholds.map((step) => ({
    value: step.value === null ? -Infinity : step.value,
    color: step.color,
  }));

  const thresholdsConfig = {
    mode: ThresholdsMode.Absolute,
    steps: thresholdSteps,
  };

  let body;
  switch (panel.type) {
    case 'stat': {
      const builder = PanelBuilders.stat()
        .setTitle(panel.title)
        .setData(queryRunner)
        .setUnit(panel.unit)
        .setThresholds(thresholdsConfig)
        .setOption('reduceOptions', {
          calcs: ['lastNotNull'],
          values: false,
        });
      if (panel.description) {
        builder.setDescription(panel.description);
      }
      body = builder.build();
      break;
    }

    case 'table': {
      const builder = PanelBuilders.table()
        .setTitle(panel.title)
        .setData(queryRunner)
        .setUnit(panel.unit)
        .setThresholds(thresholdsConfig)
        .setOption('frameIndex', 0)
        .setOption('showHeader', true)
        .setOption('showTypeIcons', false)
        .setOption('cellHeight', TableCellHeight.Sm);
      if (panel.description) {
        builder.setDescription(panel.description);
      }
      body = builder.build();
      break;
    }

    case 'gauge': {
      const builder = PanelBuilders.gauge()
        .setTitle(panel.title)
        .setData(queryRunner)
        .setUnit(panel.unit)
        .setThresholds(thresholdsConfig)
        .setOption('reduceOptions', {
          calcs: ['lastNotNull'],
          values: false,
        })
        .setOption('showThresholdLabels', false)
        .setOption('showThresholdMarkers', true)
        .setOption('orientation', VizOrientation.Auto);
      if (panel.description) {
        builder.setDescription(panel.description);
      }
      body = builder.build();
      break;
    }

    case 'bargauge': {
      const builder = PanelBuilders.bargauge()
        .setTitle(panel.title)
        .setData(queryRunner)
        .setUnit(panel.unit)
        .setThresholds(thresholdsConfig)
        .setOption('reduceOptions', {
          calcs: ['lastNotNull'],
          values: false,
        })
        .setOption('displayMode', BarGaugeDisplayMode.Gradient)
        .setOption('orientation', VizOrientation.Horizontal)
        .setOption('showUnfilled', true);
      if (panel.description) {
        builder.setDescription(panel.description);
      }
      body = builder.build();
      break;
    }

    case 'piechart': {
      const builder = PanelBuilders.piechart()
        .setTitle(panel.title)
        .setData(queryRunner)
        .setUnit(panel.unit)
        .setThresholds(thresholdsConfig)
        .setOption('pieType', 'pie' as PieChartPieType)
        .setOption('reduceOptions', {
          calcs: ['lastNotNull'],
          values: false,
        })
        .setOption('legend', {
          showLegend: true,
          displayMode: LegendDisplayMode.List,
          placement: 'bottom',
          values: [],
          calcs: [],
        });
      if (panel.description) {
        builder.setDescription(panel.description);
      }
      body = builder.build();
      break;
    }

    case 'state-timeline': {
      const builder = PanelBuilders.statetimeline()
        .setTitle(panel.title)
        .setData(queryRunner)
        .setUnit(panel.unit)
        .setThresholds(thresholdsConfig)
        .setOption('mergeValues', true)
        .setOption('showValue', VisibilityMode.Auto)
        .setOption('rowHeight', 0.9)
        .setOption('legend', {
          showLegend: true,
          displayMode: LegendDisplayMode.List,
          placement: 'bottom',
          calcs: [],
        });
      if (panel.description) {
        builder.setDescription(panel.description);
      }
      body = builder.build();
      break;
    }

    case 'timeseries':
    default: {
      const builder = PanelBuilders.timeseries()
        .setTitle(panel.title)
        .setData(queryRunner)
        .setUnit(panel.unit)
        .setThresholds(thresholdsConfig)
        .setOption('legend', {
          showLegend: true,
          displayMode: LegendDisplayMode.List,
          placement: 'bottom',
          calcs: [],
        });
      if (panel.description) {
        builder.setDescription(panel.description);
      }
      body = builder.build();
      break;
    }
  }

  return new SceneGridItem({
    key: panel.id,
    x: panel.gridPos.x,
    y: panel.gridPos.y,
    width: panel.gridPos.w,
    height: panel.gridPos.h,
    isDraggable: false,
    isResizable: false,
    body,
  });
}
