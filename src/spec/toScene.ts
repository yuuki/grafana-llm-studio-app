import { ThresholdsMode } from '@grafana/data';
import {
  EmbeddedScene,
  PanelBuilders,
  SceneGridItem,
  SceneGridLayout,
  SceneQueryRunner,
  SceneTimeRange,
} from '@grafana/scenes';
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
 * - Disables auto-refresh (no SceneRefreshPicker) and fixes grid geometry.
 * - Callers MUST invoke this ONLY after explicit user "検証・プレビュー" action and memoize
 *   by (jobId, revision, previewToken) so React re-renders never trigger unintended re-queries.
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

  const vizBuilder =
    panel.type === 'stat' ? PanelBuilders.stat() : PanelBuilders.timeseries();

  vizBuilder
    .setTitle(panel.title)
    .setData(queryRunner)
    .setUnit(panel.unit)
    .setThresholds({
      mode: ThresholdsMode.Absolute,
      steps: thresholdSteps,
    });

  if (panel.description) {
    vizBuilder.setDescription(panel.description);
  }

  return new SceneGridItem({
    key: panel.id,
    x: panel.gridPos.x,
    y: panel.gridPos.y,
    width: panel.gridPos.w,
    height: panel.gridPos.h,
    isDraggable: false,
    isResizable: false,
    body: vizBuilder.build(),
  });
}
