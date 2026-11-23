import {
  AllowedUnit,
  DashboardIR,
  PanelType,
  SELECTED_DATASOURCE_REF,
  ThresholdColor,
  validateIRClient,
} from '../ir/types';

export interface ResolvedDatasource {
  uid: string;
  type: string;
  name?: string;
}

export interface NormalizedGridPos {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface NormalizedThresholdStep {
  value: number | null;
  color: ThresholdColor;
}

export interface NormalizedQueryTarget {
  refId: 'A';
  expr: string;
  legendFormat?: string;
  datasource: {
    type: string;
    uid: string;
  };
}

export interface NormalizedPanelSpec {
  id: string;
  numericId: number;
  type: PanelType;
  title: string;
  description?: string;
  datasource: {
    type: string;
    uid: string;
  };
  target: NormalizedQueryTarget;
  unit: AllowedUnit;
  thresholds: NormalizedThresholdStep[];
  gridPos: NormalizedGridPos;
}

export interface NormalizedDashboardSpec {
  revision: number;
  title: string;
  description?: string;
  timeRange: {
    from: string;
    to: string;
  };
  refresh: '';
  tags: string[];
  panels: NormalizedPanelSpec[];
}

const ROW_HEIGHT_TIMESERIES = 8;
const ROW_HEIGHT_STAT = 6;

/**
 * Single normalization pipeline shared by both the Scenes preview adapter
 * and the Foundation SDK dashboard export/save adapter.
 * Layout, units, thresholds, datasource reference, and query targets are
 * computed ONCE here to guarantee parity between preview and saved dashboard.
 */
export function normalizeDashboardIR(
  doc: DashboardIR,
  datasource: ResolvedDatasource
): NormalizedDashboardSpec {
  if (!datasource.uid || datasource.uid.trim() === '') {
    throw new Error('Resolved datasource UID is required');
  }
  const issues = validateIRClient(doc);
  if (issues.length > 0) {
    throw new Error(`Invalid DashboardIR cannot be normalized: ${issues.join('; ')}`);
  }

  const dsRef = {
    type: datasource.type || 'prometheus',
    uid: datasource.uid.trim(),
  };

  // Sort panels deterministically by (row, col, id) before computing y coordinates.
  const sorted = [...doc.panels].sort((a, b) => {
    if (a.layout.row !== b.layout.row) {
      return a.layout.row - b.layout.row;
    }
    if (a.layout.col !== b.layout.col) {
      return a.layout.col - b.layout.col;
    }
    return a.id.localeCompare(b.id);
  });

  // Compute row heights so every row stacks without overlap.
  const rowHeights = new Map<number, number>();
  for (const p of sorted) {
    const h = p.type === 'stat' ? ROW_HEIGHT_STAT : ROW_HEIGHT_TIMESERIES;
    const prev = rowHeights.get(p.layout.row) ?? 0;
    if (h > prev) {
      rowHeights.set(p.layout.row, h);
    }
  }
  const orderedRows = Array.from(rowHeights.keys()).sort((a, b) => a - b);
  const rowStartY = new Map<number, number>();
  let cursorY = 0;
  for (const r of orderedRows) {
    rowStartY.set(r, cursorY);
    cursorY += rowHeights.get(r) ?? ROW_HEIGHT_TIMESERIES;
  }

  const normalizedPanels: NormalizedPanelSpec[] = sorted.map((p, index) => {
    if (p.datasourceRef !== SELECTED_DATASOURCE_REF) {
      throw new Error(`Panel ${p.id} has unauthorized datasourceRef: ${p.datasourceRef}`);
    }
    const w = p.layout.width === 'full' ? 24 : 12;
    const x = p.layout.width === 'full' ? 0 : p.layout.col * 12;
    const h = rowHeights.get(p.layout.row) ?? ROW_HEIGHT_TIMESERIES;
    const y = rowStartY.get(p.layout.row) ?? index * ROW_HEIGHT_TIMESERIES;

    return {
      id: p.id,
      numericId: index + 1,
      type: p.type,
      title: p.title,
      description: p.description,
      datasource: { ...dsRef },
      target: {
        refId: 'A',
        expr: p.query.expr.trim(),
        legendFormat: p.query.legendFormat,
        datasource: { ...dsRef },
      },
      unit: p.unit,
      thresholds: normalizeThresholds(p.thresholds),
      gridPos: { x, y, w, h },
    };
  });

  return {
    revision: doc.revision,
    title: doc.title,
    description: doc.description,
    timeRange: {
      from: doc.timeRange.from,
      to: doc.timeRange.to,
    },
    refresh: '', // Auto-refresh strictly disabled in MVP
    tags: ['llm-dashboard-studio'],
    panels: normalizedPanels,
  };
}

function normalizeThresholds(
  raw?: Array<{ value: number; color: ThresholdColor }>
): NormalizedThresholdStep[] {
  if (!raw || raw.length === 0) {
    return [{ value: null, color: 'green' }];
  }
  const sorted = [...raw].sort((a, b) => a.value - b.value);
  // Grafana thresholds require an initial base step with value: null (or -Infinity).
  const steps: NormalizedThresholdStep[] = [{ value: null, color: sorted[0].color }];
  for (let i = 1; i < sorted.length; i++) {
    steps.push({ value: sorted[i].value, color: sorted[i].color });
  }
  return steps;
}
