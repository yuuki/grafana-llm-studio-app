import { ThresholdsMode } from '@grafana/data';
import {
  EmbeddedScene,
  PanelBuilders,
  SceneGridItem,
  SceneGridLayout,
  SceneQueryRunner,
  SceneTimeRange,
} from '@grafana/scenes';
import { LegendDisplayMode } from '@grafana/schema';
import { NormalizedDashboardSpec, NormalizedPanelSpec } from './normalize';

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
 * - Mirrors timeseries legend and stat reduceOptions identically to toFoundation.ts (H-12).
 */
export function buildPreviewScene(
  spec: NormalizedDashboardSpec,
  options: SceneAdapterOptions = {}
): EmbeddedScene {
  const maxDataPoints = options.maxDataPoints ?? 200;

  const gridItems = spec.panels.map((panel) => buildGridItem(panel, maxDataPoints));

  return new EmbeddedScene({
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
  });
}

function buildGridItem(panel: NormalizedPanelSpec, maxDataPoints: number): SceneGridItem {
  const queryRunner = new SceneQueryRunner({
    datasource: panel.datasource,
    maxDataPoints,
    queries: [
      {
        refId: panel.target.refId,
        expr: panel.target.expr,
        legendFormat: panel.target.legendFormat,
        datasource: panel.target.datasource,
      },
    ],
  });

  const thresholdSteps = panel.thresholds.map((step) => ({
    value: step.value === null ? -Infinity : step.value,
    color: step.color,
  }));

  let body;
  if (panel.type === 'stat') {
    const statBuilder = PanelBuilders.stat()
      .setTitle(panel.title)
      .setData(queryRunner)
      .setUnit(panel.unit)
      .setThresholds({
        mode: ThresholdsMode.Absolute,
        steps: thresholdSteps,
      })
      .setOption('reduceOptions', {
        calcs: ['lastNotNull'],
        values: false,
      });
    if (panel.description) {
      statBuilder.setDescription(panel.description);
    }
    body = statBuilder.build();
  } else {
    const tsBuilder = PanelBuilders.timeseries()
      .setTitle(panel.title)
      .setData(queryRunner)
      .setUnit(panel.unit)
      .setThresholds({
        mode: ThresholdsMode.Absolute,
        steps: thresholdSteps,
      })
      .setOption('legend', {
        showLegend: true,
        displayMode: LegendDisplayMode.List,
        placement: 'bottom',
        calcs: [],
      });
    if (panel.description) {
      tsBuilder.setDescription(panel.description);
    }
    body = tsBuilder.build();
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
