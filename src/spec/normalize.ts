import {
  ALLOWED_QUERY_REF_IDS,
  AllowedUnit,
  DashboardIR,
  getPanelQueries,
  PanelType,
  QueryRefId,
  SELECTED_DATASOURCE_REF,
  ThresholdColor,
  validateIRClient,
  VariableIR,
  VariableType,
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
  refId: QueryRefId;
  expr: string;
  legendFormat?: string;
  datasource: {
    type: string;
    uid: string;
  };
}

export interface NormalizedVariableSpec {
  name: string;
  label: string;
  type: VariableType;
  datasource?: {
    type: string;
    uid: string;
  };
  /**
   * For query variables: Prometheus label_values(...) query string.
   * For custom variables: comma-separated options string.
   */
  query: string;
  labelName?: string;
  metricName?: string;
  options: string[];
  defaultValue: string;
  multi: boolean;
  includeAll: boolean;
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
  /**
   * Primary query target (targets[0]), retained for convenience and backward compatibility.
   */
  target: NormalizedQueryTarget;
  /**
   * Ordered list of 1..3 query targets (refIds A, B, C).
   */
  targets: NormalizedQueryTarget[];
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
  variables: NormalizedVariableSpec[];
  panels: NormalizedPanelSpec[];
}

const PANEL_ROW_HEIGHT: Record<PanelType, number> = {
  timeseries: 8,
  stat: 6,
  table: 8,
  gauge: 6,
  bargauge: 6,
  piechart: 8,
  'state-timeline': 8,
};

/**
 * Single normalization pipeline shared by both the Scenes preview adapter
 * and the Foundation SDK dashboard export/save adapter.
 * Layout, units, thresholds, datasource references, multi-query targets, and
 * scoped template variables are computed ONCE here to guarantee parity between
 * live preview and saved/exported dashboard.
 */
export function normalizeDashboardIR(
  doc: DashboardIR,
  datasource: ResolvedDatasource,
  maxPanels = 6
): NormalizedDashboardSpec {
  if (!datasource.uid || datasource.uid.trim() === '') {
    throw new Error('Resolved datasource UID is required');
  }
  const issues = validateIRClient(doc, maxPanels);
  if (issues.length > 0) {
    throw new Error(`Invalid DashboardIR cannot be normalized: ${issues.join('; ')}`);
  }

  const dsRef = {
    type: datasource.type || 'prometheus',
    uid: datasource.uid.trim(),
  };

  const normalizedVariables: NormalizedVariableSpec[] = (doc.variables ?? []).map((v) =>
    normalizeVariable(v, dsRef)
  );

  const sorted = [...doc.panels].sort((a, b) => {
    if (a.layout.row !== b.layout.row) {
      return a.layout.row - b.layout.row;
    }
    if (a.layout.col !== b.layout.col) {
      return a.layout.col - b.layout.col;
    }
    return a.id.localeCompare(b.id);
  });

  const rowHeights = new Map<number, number>();
  for (const p of sorted) {
    const h = PANEL_ROW_HEIGHT[p.type] ?? 8;
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
    cursorY += rowHeights.get(r) ?? 8;
  }

  const normalizedPanels: NormalizedPanelSpec[] = sorted.map((p, index) => {
    if (p.datasourceRef !== SELECTED_DATASOURCE_REF) {
      throw new Error(`Panel ${p.id} has unauthorized datasourceRef: ${p.datasourceRef}`);
    }
    const w = p.layout.width === 'full' ? 24 : 12;
    const x = p.layout.width === 'full' ? 0 : p.layout.col * 12;
    const h = rowHeights.get(p.layout.row) ?? 8;
    const y = rowStartY.get(p.layout.row) ?? index * 8;

    const rawQueries = getPanelQueries(p);
    const targets: NormalizedQueryTarget[] = rawQueries.map((q, qi) => ({
      refId: q.refId ?? ALLOWED_QUERY_REF_IDS[qi],
      expr: q.expr.trim(),
      legendFormat: q.legendFormat,
      datasource: { ...dsRef },
    }));

    return {
      id: p.id,
      numericId: index + 1,
      type: p.type,
      title: p.title,
      description: p.description,
      datasource: { ...dsRef },
      target: targets[0],
      targets,
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
    refresh: '',
    tags: ['llm-dashboard-studio'],
    variables: normalizedVariables,
    panels: normalizedPanels,
  };
}

function normalizeVariable(
  v: VariableIR,
  dsRef: { type: string; uid: string }
): NormalizedVariableSpec {
  const label = (v.label && v.label.trim()) || v.name;
  const multi = Boolean(v.multi);
  const includeAll = Boolean(v.includeAll);

  if (v.type === 'query') {
    if (v.datasourceRef !== SELECTED_DATASOURCE_REF) {
      throw new Error(`Variable ${v.name} has unauthorized datasourceRef: ${v.datasourceRef}`);
    }
    const query = v.metricName
      ? `label_values(${v.metricName}, ${v.labelName})`
      : `label_values(${v.labelName})`;
    return {
      name: v.name,
      label,
      type: 'query',
      datasource: { ...dsRef },
      query,
      labelName: v.labelName,
      metricName: v.metricName,
      options: [],
      defaultValue: (v.defaultValue ?? '').trim(),
      multi,
      includeAll,
    };
  }

  const options = [...(v.options ?? [])];
  const defaultValue = (v.defaultValue ?? '').trim() || options[0] || '';
  return {
    name: v.name,
    label,
    type: 'custom',
    query: options.join(','),
    options,
    defaultValue,
    multi,
    includeAll,
  };
}

/**
 * Interpolates declared dashboard template variable placeholders ($var, ${var}, ${var:regex}, ${var:pipe})
 * using their default values for non-Grafana dry-run validation queries (/api/ds/query).
 */
export function interpolatePromQLVariables(
  expr: string,
  variables: NormalizedVariableSpec[]
): string {
  if (!variables.length || !expr.includes('$')) {
    return expr;
  }
  let out = expr;
  for (const v of variables) {
    const val = v.defaultValue || v.options[0] || '';
    if (!val) {
      continue;
    }
    const regexEscaped = val.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    out = out.split(`\${${v.name}:regex}`).join(regexEscaped);
    out = out.split(`\${${v.name}:pipe}`).join(val);
    out = out.split(`\${${v.name}}`).join(val);
    const bareRe = new RegExp(`\\$${v.name}(?![a-zA-Z0-9_])`, 'g');
    out = out.replace(bareRe, () => val);
  }
  return out;
}

/**
 * Normalizes ascending IR thresholds into Grafana absolute threshold steps.
 * Preserves non-zero starting thresholds (e.g. [{value: 0.5, color: 'yellow'}]) by
 * prepending a green base step {value: null, color: 'green'} instead of overwriting 0.5.
 */
function normalizeThresholds(
  raw?: Array<{ value: number; color: ThresholdColor }>
): NormalizedThresholdStep[] {
  if (!raw || raw.length === 0) {
    return [{ value: null, color: 'green' }];
  }
  const sorted = [...raw].sort((a, b) => a.value - b.value);
  if (sorted[0].value === 0 || sorted[0].color === 'green') {
    const steps: NormalizedThresholdStep[] = [{ value: null, color: sorted[0].color }];
    for (let i = 1; i < sorted.length; i++) {
      steps.push({ value: sorted[i].value, color: sorted[i].color });
    }
    return steps;
  }
  return [
    { value: null, color: 'green' },
    ...sorted.map((s) => ({ value: s.value, color: s.color })),
  ];
}
