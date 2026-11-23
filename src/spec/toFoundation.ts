import * as common from '@grafana/grafana-foundation-sdk/common';
import * as dashboard from '@grafana/grafana-foundation-sdk/dashboard';
import * as prometheus from '@grafana/grafana-foundation-sdk/prometheus';
import * as stat from '@grafana/grafana-foundation-sdk/stat';
import * as timeseries from '@grafana/grafana-foundation-sdk/timeseries';
import { NormalizedDashboardSpec, NormalizedPanelSpec } from './normalize';

export interface FoundationBuildOptions {
  /**
   * Fresh dashboard UID managed by the application (never LLM-generated).
   * Leave empty string for clean JSON export where Grafana assigns UID on import.
   */
  uid?: string;
}

/**
 * Thin adapter converting NormalizedDashboardSpec into a Grafana Classic JSON Dashboard
 * via the official @grafana/grafana-foundation-sdk (v0.0.22, schemaVersion 42).
 * - Guarantees identical queries, units, thresholds, panel IDs, legend/reduce options,
 *   datasource refs, and gridPos as Scenes preview (H-12).
 * - Never embeds chat history, credentials, unnecessary label values, or raw query data.
 * - Forces id=null, version=0, refresh="" so saved/exported JSON is always create-only and static.
 */
export function buildFoundationDashboard(
  spec: NormalizedDashboardSpec,
  options: FoundationBuildOptions = {}
): dashboard.Dashboard {
  const builder = new dashboard.DashboardBuilder(spec.title)
    .id(null)
    .version(0)
    .editable()
    .timezone('browser')
    .refresh(spec.refresh)
    .tags([...spec.tags])
    .time({
      from: spec.timeRange.from,
      to: spec.timeRange.to,
    });

  if (options.uid && options.uid.trim() !== '') {
    builder.uid(options.uid.trim());
  }
  if (spec.description) {
    builder.description(spec.description);
  }

  for (const panel of spec.panels) {
    builder.withPanel(buildFoundationPanel(panel));
  }

  return builder.build();
}

function buildFoundationPanel(panel: NormalizedPanelSpec) {
  const queryBuilder = new prometheus.DataqueryBuilder()
    .refId(panel.target.refId)
    .expr(panel.target.expr)
    .datasource({
      type: panel.target.datasource.type,
      uid: panel.target.datasource.uid,
    });

  if (panel.target.legendFormat) {
    queryBuilder.legendFormat(panel.target.legendFormat);
  }

  const thresholdsBuilder = new dashboard.ThresholdsConfigBuilder()
    .mode(dashboard.ThresholdsMode.Absolute)
    .steps(
      panel.thresholds.map((step) => ({
        value: step.value,
        color: step.color,
      }))
    );

  if (panel.type === 'stat') {
    const statBuilder = new stat.PanelBuilder()
      .id(panel.numericId)
      .title(panel.title)
      .datasource({
        type: panel.datasource.type,
        uid: panel.datasource.uid,
      })
      .unit(panel.unit)
      .thresholds(thresholdsBuilder)
      .reduceOptions(
        new common.ReduceDataOptionsBuilder().calcs(['lastNotNull']).values(false)
      )
      .gridPos({
        x: panel.gridPos.x,
        y: panel.gridPos.y,
        w: panel.gridPos.w,
        h: panel.gridPos.h,
      })
      .withTarget(queryBuilder);

    if (panel.description) {
      statBuilder.description(panel.description);
    }
    return statBuilder;
  }

  const tsBuilder = new timeseries.PanelBuilder()
    .id(panel.numericId)
    .title(panel.title)
    .datasource({
      type: panel.datasource.type,
      uid: panel.datasource.uid,
    })
    .unit(panel.unit)
    .thresholds(thresholdsBuilder)
    .legend(
      new common.VizLegendOptionsBuilder()
        .showLegend(true)
        .displayMode(common.LegendDisplayMode.List)
        .placement(common.LegendPlacement.Bottom)
        .calcs([])
    )
    .gridPos({
      x: panel.gridPos.x,
      y: panel.gridPos.y,
      w: panel.gridPos.w,
      h: panel.gridPos.h,
    })
    .withTarget(queryBuilder);

  if (panel.description) {
    tsBuilder.description(panel.description);
  }
  return tsBuilder;
}
