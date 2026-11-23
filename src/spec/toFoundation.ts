import * as bargauge from '@grafana/grafana-foundation-sdk/bargauge';
import * as common from '@grafana/grafana-foundation-sdk/common';
import * as dashboard from '@grafana/grafana-foundation-sdk/dashboard';
import * as gauge from '@grafana/grafana-foundation-sdk/gauge';
import * as piechart from '@grafana/grafana-foundation-sdk/piechart';
import * as prometheus from '@grafana/grafana-foundation-sdk/prometheus';
import * as stat from '@grafana/grafana-foundation-sdk/stat';
import * as statetimeline from '@grafana/grafana-foundation-sdk/statetimeline';
import * as table from '@grafana/grafana-foundation-sdk/table';
import * as timeseries from '@grafana/grafana-foundation-sdk/timeseries';
import {
  NormalizedDashboardSpec,
  NormalizedPanelSpec,
  NormalizedVariableSpec,
} from './normalize';

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
 * - Guarantees identical queries, units, thresholds, panel IDs, options, template variables,
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

  for (const variable of spec.variables) {
    builder.withVariable(buildFoundationVariable(variable));
  }

  for (const panel of spec.panels) {
    builder.withPanel(buildFoundationPanel(panel));
  }

  return builder.build();
}

function buildFoundationVariable(v: NormalizedVariableSpec) {
  if (v.type === 'query') {
    const qb = new dashboard.QueryVariableBuilder(v.name)
      .label(v.label)
      .query(v.query)
      .multi(v.multi)
      .includeAll(v.includeAll)
      .refresh(dashboard.VariableRefresh.OnDashboardLoad)
      .sort(dashboard.VariableSort.AlphabeticalAsc)
      .hide(dashboard.VariableHide.DontHide);
    if (v.datasource) {
      qb.datasource({
        type: v.datasource.type,
        uid: v.datasource.uid,
      });
    }
    if (v.defaultValue) {
      qb.current({
        text: v.defaultValue,
        value: v.defaultValue,
      });
    }
    return qb;
  }

  const cb = new dashboard.CustomVariableBuilder(v.name)
    .label(v.label)
    .values(v.query)
    .options(
      v.options.map((opt) => ({
        text: opt,
        value: opt,
        selected: opt === v.defaultValue,
      }))
    )
    .multi(v.multi)
    .includeAll(v.includeAll)
    .hide(dashboard.VariableHide.DontHide);

  if (v.defaultValue) {
    cb.current({
      text: v.defaultValue,
      value: v.defaultValue,
    });
  }
  return cb;
}

function buildFoundationPanel(panel: NormalizedPanelSpec) {
  const queryBuilders = panel.targets.map((target) => {
    const qb = new prometheus.DataqueryBuilder()
      .refId(target.refId)
      .expr(target.expr)
      .datasource({
        type: target.datasource.type,
        uid: target.datasource.uid,
      });
    if (target.legendFormat) {
      qb.legendFormat(target.legendFormat);
    }
    return qb;
  });

  const thresholdsBuilder = new dashboard.ThresholdsConfigBuilder()
    .mode(dashboard.ThresholdsMode.Absolute)
    .steps(
      panel.thresholds.map((step) => ({
        value: step.value,
        color: step.color,
      }))
    );

  const dsRef = {
    type: panel.datasource.type,
    uid: panel.datasource.uid,
  };
  const gridPos = {
    x: panel.gridPos.x,
    y: panel.gridPos.y,
    w: panel.gridPos.w,
    h: panel.gridPos.h,
  };

  switch (panel.type) {
    case 'stat': {
      const builder = new stat.PanelBuilder()
        .id(panel.numericId)
        .title(panel.title)
        .datasource(dsRef)
        .unit(panel.unit)
        .thresholds(thresholdsBuilder)
        .reduceOptions(
          new common.ReduceDataOptionsBuilder().calcs(['lastNotNull']).values(false)
        )
        .gridPos(gridPos);
      for (const qb of queryBuilders) {
        builder.withTarget(qb);
      }
      if (panel.description) {
        builder.description(panel.description);
      }
      return builder;
    }

    case 'table': {
      const builder = new table.PanelBuilder()
        .id(panel.numericId)
        .title(panel.title)
        .datasource(dsRef)
        .unit(panel.unit)
        .thresholds(thresholdsBuilder)
        .frameIndex(0)
        .showHeader(true)
        .showTypeIcons(false)
        .cellHeight(common.TableCellHeight.Sm)
        .gridPos(gridPos);
      for (const qb of queryBuilders) {
        builder.withTarget(qb);
      }
      if (panel.description) {
        builder.description(panel.description);
      }
      return builder;
    }

    case 'gauge': {
      const builder = new gauge.PanelBuilder()
        .id(panel.numericId)
        .title(panel.title)
        .datasource(dsRef)
        .unit(panel.unit)
        .thresholds(thresholdsBuilder)
        .reduceOptions(
          new common.ReduceDataOptionsBuilder().calcs(['lastNotNull']).values(false)
        )
        .showThresholdLabels(false)
        .showThresholdMarkers(true)
        .orientation(common.VizOrientation.Auto)
        .gridPos(gridPos);
      for (const qb of queryBuilders) {
        builder.withTarget(qb);
      }
      if (panel.description) {
        builder.description(panel.description);
      }
      return builder;
    }

    case 'bargauge': {
      const builder = new bargauge.PanelBuilder()
        .id(panel.numericId)
        .title(panel.title)
        .datasource(dsRef)
        .unit(panel.unit)
        .thresholds(thresholdsBuilder)
        .reduceOptions(
          new common.ReduceDataOptionsBuilder().calcs(['lastNotNull']).values(false)
        )
        .displayMode(common.BarGaugeDisplayMode.Gradient)
        .orientation(common.VizOrientation.Horizontal)
        .showUnfilled(true)
        .gridPos(gridPos);
      for (const qb of queryBuilders) {
        builder.withTarget(qb);
      }
      if (panel.description) {
        builder.description(panel.description);
      }
      return builder;
    }

    case 'piechart': {
      const builder = new piechart.PanelBuilder()
        .id(panel.numericId)
        .title(panel.title)
        .datasource(dsRef)
        .unit(panel.unit)
        .thresholds(thresholdsBuilder)
        .pieType(piechart.PieChartType.Pie)
        .reduceOptions(
          new common.ReduceDataOptionsBuilder().calcs(['lastNotNull']).values(false)
        )
        .legend(
          new piechart.PieChartLegendOptionsBuilder()
            .showLegend(true)
            .displayMode(common.LegendDisplayMode.List)
            .placement(common.LegendPlacement.Bottom)
            .values([])
            .calcs([])
        )
        .gridPos(gridPos);
      for (const qb of queryBuilders) {
        builder.withTarget(qb);
      }
      if (panel.description) {
        builder.description(panel.description);
      }
      return builder;
    }

    case 'state-timeline': {
      const builder = new statetimeline.PanelBuilder()
        .id(panel.numericId)
        .title(panel.title)
        .datasource(dsRef)
        .unit(panel.unit)
        .thresholds(thresholdsBuilder)
        .mergeValues(true)
        .showValue(common.VisibilityMode.Auto)
        .rowHeight(0.9)
        .legend(
          new common.VizLegendOptionsBuilder()
            .showLegend(true)
            .displayMode(common.LegendDisplayMode.List)
            .placement(common.LegendPlacement.Bottom)
            .calcs([])
        )
        .gridPos(gridPos);
      for (const qb of queryBuilders) {
        builder.withTarget(qb);
      }
      if (panel.description) {
        builder.description(panel.description);
      }
      return builder;
    }

    case 'timeseries':
    default: {
      const builder = new timeseries.PanelBuilder()
        .id(panel.numericId)
        .title(panel.title)
        .datasource(dsRef)
        .unit(panel.unit)
        .thresholds(thresholdsBuilder)
        .legend(
          new common.VizLegendOptionsBuilder()
            .showLegend(true)
            .displayMode(common.LegendDisplayMode.List)
            .placement(common.LegendPlacement.Bottom)
            .calcs([])
        )
        .gridPos(gridPos);
      for (const qb of queryBuilders) {
        builder.withTarget(qb);
      }
      if (panel.description) {
        builder.description(panel.description);
      }
      return builder;
    }
  }
}
