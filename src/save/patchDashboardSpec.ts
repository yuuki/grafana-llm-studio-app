import {
  ImportPatchContext,
  ImportedPanelLocator,
  safeDeepClone,
} from '../import/importDashboard';
import {
  DashboardIR,
  getPanelQueries,
  LayoutHintIR,
  PanelIR,
  ThresholdIR,
  VariableIR,
} from '../ir/types';
import {
  NormalizedDashboardSpec,
  NormalizedPanelSpec,
  NormalizedVariableSpec,
} from '../spec/normalize';
import {
  buildFoundationPanelObject,
  buildFoundationTargetObject,
  buildFoundationVariableObject,
} from '../spec/toFoundation';

export interface BuildPatchedDashboardSpecParams {
  targetUid: string;
  currentIR: DashboardIR;
  currentSpec: NormalizedDashboardSpec;
  patchContext: ImportPatchContext;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return Boolean(v) && typeof v === 'object' && !Array.isArray(v);
}

function thresholdsEqual(a?: ThresholdIR[], b?: ThresholdIR[]): boolean {
  const listA = a ?? [];
  const listB = b ?? [];
  if (listA.length !== listB.length) {
    return false;
  }
  for (let i = 0; i < listA.length; i++) {
    if (listA[i].value !== listB[i].value || listA[i].color !== listB[i].color) {
      return false;
    }
  }
  return true;
}

function layoutEqual(a: LayoutHintIR, b: LayoutHintIR): boolean {
  return a.row === b.row && a.col === b.col && a.width === b.width;
}

function queriesEqual(a: PanelIR, b: PanelIR): boolean {
  const qa = getPanelQueries(a);
  const qb = getPanelQueries(b);
  if (qa.length !== qb.length) {
    return false;
  }
  for (let i = 0; i < qa.length; i++) {
    if (
      (qa[i].expr ?? '').trim() !== (qb[i].expr ?? '').trim() ||
      (qa[i].legendFormat ?? '') !== (qb[i].legendFormat ?? '') ||
      (qa[i].kind ?? '') !== (qb[i].kind ?? '')
    ) {
      return false;
    }
  }
  return true;
}

function panelIREqual(a: PanelIR, b: PanelIR): boolean {
  return (
    a.type === b.type &&
    a.title === b.title &&
    (a.description ?? '') === (b.description ?? '') &&
    a.unit === b.unit &&
    thresholdsEqual(a.thresholds, b.thresholds) &&
    layoutEqual(a.layout, b.layout) &&
    queriesEqual(a, b)
  );
}

function stringArraysEqual(a?: string[], b?: string[]): boolean {
  const la = a ?? [];
  const lb = b ?? [];
  if (la.length !== lb.length) {
    return false;
  }
  for (let i = 0; i < la.length; i++) {
    if (la[i] !== lb[i]) {
      return false;
    }
  }
  return true;
}

function variableIREqual(a: VariableIR, b: VariableIR): boolean {
  return (
    a.type === b.type &&
    (a.label ?? '') === (b.label ?? '') &&
    (a.labelName ?? '') === (b.labelName ?? '') &&
    (a.metricName ?? '') === (b.metricName ?? '') &&
    (a.defaultValue ?? '') === (b.defaultValue ?? '') &&
    Boolean(a.multi) === Boolean(b.multi) &&
    Boolean(a.includeAll) === Boolean(b.includeAll) &&
    stringArraysEqual(a.options, b.options)
  );
}

function nextAvailableRefId(used: Set<string>): string {
  for (let code = 65; code <= 90; code++) {
    const candidate = String.fromCharCode(code);
    if (!used.has(candidate)) {
      used.add(candidate);
      return candidate;
    }
  }
  let n = 1;
  while (used.has(`Q${n}`)) {
    n++;
  }
  const fallback = `Q${n}`;
  used.add(fallback);
  return fallback;
}

function collectPreservedTargetRefIds(
  origTargets: unknown[],
  importedIndexSet: Set<number>
): Set<string> {
  const usedRefIds = new Set<string>();
  for (let i = 0; i < origTargets.length; i++) {
    if (importedIndexSet.has(i)) {
      continue;
    }
    const t = origTargets[i];
    if (isRecord(t) && typeof t.refId === 'string' && t.refId.trim() !== '') {
      usedRefIds.add(t.refId.trim());
    }
  }
  return usedRefIds;
}

function patchSingleExistingTarget(
  existing: unknown,
  newTarget: NormalizedPanelSpec['targets'][number],
  prevLegend: string,
  usedRefIds: Set<string>
): Record<string, unknown> {
  const cloned: Record<string, unknown> = isRecord(existing)
    ? safeDeepClone(existing)
    : buildFoundationTargetObject(newTarget);
  cloned.expr = newTarget.expr;
  cloned.datasource = { ...newTarget.datasource };

  const origRefId = typeof cloned.refId === 'string' ? cloned.refId.trim() : '';
  if (origRefId && !usedRefIds.has(origRefId)) {
    usedRefIds.add(origRefId);
  } else if (!usedRefIds.has(newTarget.refId)) {
    usedRefIds.add(newTarget.refId);
    cloned.refId = newTarget.refId;
  } else {
    cloned.refId = nextAvailableRefId(usedRefIds);
  }

  const nextLegend = newTarget.legendFormat ?? '';
  if (nextLegend !== prevLegend) {
    if (nextLegend) {
      cloned.legendFormat = nextLegend;
    } else {
      delete cloned.legendFormat;
    }
  }
  return cloned;
}

function patchPanelTargets(
  rawPanel: Record<string, unknown>,
  normPanel: NormalizedPanelSpec,
  initialPanel: PanelIR,
  importedTargetIndices: number[]
): void {
  const origTargets = Array.isArray(rawPanel.targets) ? rawPanel.targets : [];
  const importedIndexSet = new Set<number>(importedTargetIndices);
  const initialQueries = getPanelQueries(initialPanel);

  // Collect refIds owned by preserved non-imported targets (e.g. hidden or other-datasource targets)
  // so appended targets never collide on refId.
  const usedRefIds = collectPreservedTargetRefIds(origTargets, importedIndexSet);

  const updatedByOrigIdx = new Map<number, Record<string, unknown>>();
  const removeOrigIndices = new Set<number>();
  const appendedTargets: Array<Record<string, unknown>> = [];

  for (let qi = 0; qi < normPanel.targets.length; qi++) {
    const newTarget = normPanel.targets[qi];
    if (qi < importedTargetIndices.length) {
      const origIdx = importedTargetIndices[qi];
      updatedByOrigIdx.set(
        origIdx,
        patchSingleExistingTarget(
          origTargets[origIdx],
          newTarget,
          initialQueries[qi]?.legendFormat ?? '',
          usedRefIds
        )
      );
    } else {
      const built = buildFoundationTargetObject(newTarget);
      if (typeof built.refId !== 'string' || usedRefIds.has(built.refId)) {
        built.refId = nextAvailableRefId(usedRefIds);
      } else {
        usedRefIds.add(built.refId);
      }
      appendedTargets.push(built);
    }
  }

  for (let qi = normPanel.targets.length; qi < importedTargetIndices.length; qi++) {
    removeOrigIndices.add(importedTargetIndices[qi]);
  }

  const nextTargets: unknown[] = [];
  for (let i = 0; i < origTargets.length; i++) {
    if (removeOrigIndices.has(i)) {
      continue;
    }
    nextTargets.push(updatedByOrigIdx.has(i) ? updatedByOrigIdx.get(i) : origTargets[i]);
  }
  rawPanel.targets = [...nextTargets, ...appendedTargets];
}

interface PanelChangeFlags {
  typeChanged: boolean;
  unitChanged: boolean;
  thresholdsChanged: boolean;
  queriesChanged: boolean;
  layoutChanged: boolean;
  titleChanged: boolean;
  descChanged: boolean;
}

function patchPanelWithTypeChange(
  rawPanel: Record<string, unknown>,
  fresh: Record<string, unknown>,
  initialPanel: PanelIR,
  normPanel: NormalizedPanelSpec,
  locator: ImportedPanelLocator,
  flags: PanelChangeFlags
): Record<string, unknown> {
  const nextPanel = safeDeepClone(fresh);
  if (typeof rawPanel.id === 'number') {
    nextPanel.id = rawPanel.id;
  }
  if (!flags.titleChanged && typeof rawPanel.title === 'string') {
    nextPanel.title = rawPanel.title;
  }
  if (!flags.descChanged) {
    if (rawPanel.description !== undefined) {
      nextPanel.description = rawPanel.description;
    } else {
      delete nextPanel.description;
    }
  }
  if (!flags.layoutChanged && isRecord(rawPanel.gridPos)) {
    nextPanel.gridPos = safeDeepClone(rawPanel.gridPos);
  }
  if (rawPanel.transformations !== undefined) {
    nextPanel.transformations = safeDeepClone(rawPanel.transformations);
  }
  for (const extraKey of ['links', 'transparent', 'repeat', 'repeatDirection', 'maxPerRow']) {
    if (rawPanel[extraKey] !== undefined) {
      nextPanel[extraKey] = safeDeepClone(rawPanel[extraKey]);
    }
  }
  mergeFieldConfigOnTypeChange(rawPanel, nextPanel, flags.unitChanged, flags.thresholdsChanged);
  if (!flags.queriesChanged && Array.isArray(rawPanel.targets)) {
    nextPanel.targets = safeDeepClone(rawPanel.targets);
  } else if (flags.queriesChanged) {
    nextPanel.targets = Array.isArray(rawPanel.targets) ? safeDeepClone(rawPanel.targets) : [];
    patchPanelTargets(nextPanel, normPanel, initialPanel, locator.importedTargetIndices);
  }
  return nextPanel;
}

function mergeFieldConfigOnTypeChange(
  rawPanel: Record<string, unknown>,
  nextPanel: Record<string, unknown>,
  unitChanged: boolean,
  thresholdsChanged: boolean
): void {
  // Preserve untouched fieldConfig.overrides and extra fieldConfig.defaults fields.
  if (!isRecord(rawPanel.fieldConfig)) {
    return;
  }
  const nextFc: Record<string, unknown> = isRecord(nextPanel.fieldConfig)
    ? safeDeepClone(nextPanel.fieldConfig)
    : {};
  if (rawPanel.fieldConfig.overrides !== undefined) {
    nextFc.overrides = safeDeepClone(rawPanel.fieldConfig.overrides);
  }
  if (isRecord(rawPanel.fieldConfig.defaults)) {
    const mergedDefaults: Record<string, unknown> = safeDeepClone(rawPanel.fieldConfig.defaults);
    const freshDefaults = isRecord(nextFc.defaults) ? nextFc.defaults : {};
    if (unitChanged) {
      mergedDefaults.unit = freshDefaults.unit;
    }
    if (thresholdsChanged) {
      mergedDefaults.thresholds = safeDeepClone(freshDefaults.thresholds);
    }
    nextFc.defaults = mergedDefaults;
  }
  nextPanel.fieldConfig = nextFc;
}

function patchSameTypePanelFieldConfig(
  patched: Record<string, unknown>,
  fresh: Record<string, unknown>,
  normPanel: NormalizedPanelSpec,
  unitChanged: boolean,
  thresholdsChanged: boolean
): void {
  const fc: Record<string, unknown> = isRecord(patched.fieldConfig)
    ? safeDeepClone(patched.fieldConfig)
    : {};
  const defaults: Record<string, unknown> = isRecord(fc.defaults)
    ? safeDeepClone(fc.defaults)
    : {};
  const freshDefaults =
    isRecord(fresh.fieldConfig) && isRecord(fresh.fieldConfig.defaults)
      ? fresh.fieldConfig.defaults
      : {};
  if (unitChanged) {
    defaults.unit = normPanel.unit;
  }
  if (thresholdsChanged) {
    defaults.thresholds = safeDeepClone(freshDefaults.thresholds);
  }
  fc.defaults = defaults;
  patched.fieldConfig = fc;
}

function patchSameTypePanelGridPos(
  patched: Record<string, unknown>,
  initialPanel: PanelIR,
  currentPanel: PanelIR,
  normPanel: NormalizedPanelSpec
): void {
  const prevGrid = isRecord(patched.gridPos) ? safeDeepClone(patched.gridPos) : {};
  const widthChanged = currentPanel.layout.width !== initialPanel.layout.width;
  const posChanged =
    currentPanel.layout.row !== initialPanel.layout.row ||
    currentPanel.layout.col !== initialPanel.layout.col;
  if (posChanged) {
    patched.gridPos = {
      ...prevGrid,
      x: normPanel.gridPos.x,
      y: normPanel.gridPos.y,
      w: normPanel.gridPos.w,
      h: typeof prevGrid.h === 'number' ? prevGrid.h : normPanel.gridPos.h,
    };
  } else if (widthChanged) {
    patched.gridPos = {
      ...prevGrid,
      w: normPanel.gridPos.w,
      ...(normPanel.gridPos.w === 24 ? { x: 0 } : { x: normPanel.gridPos.x }),
    };
  }
}

function patchSingleRawPanel(
  rawPanel: Record<string, unknown>,
  initialPanel: PanelIR,
  currentPanel: PanelIR,
  normPanel: NormalizedPanelSpec,
  locator: ImportedPanelLocator
): Record<string, unknown> {
  const fresh = buildFoundationPanelObject(normPanel);
  const flags: PanelChangeFlags = {
    typeChanged: currentPanel.type !== initialPanel.type,
    unitChanged: currentPanel.unit !== initialPanel.unit,
    thresholdsChanged: !thresholdsEqual(initialPanel.thresholds, currentPanel.thresholds),
    queriesChanged: !queriesEqual(initialPanel, currentPanel),
    layoutChanged: !layoutEqual(initialPanel.layout, currentPanel.layout),
    titleChanged: currentPanel.title !== initialPanel.title,
    descChanged: (currentPanel.description ?? '') !== (initialPanel.description ?? ''),
  };

  if (flags.typeChanged) {
    return patchPanelWithTypeChange(rawPanel, fresh, initialPanel, normPanel, locator, flags);
  }

  // Same panel type: mutate ONLY the changed fields on the cloned original panel,
  // preserving transformations, fieldConfig.overrides, options, untouched defaults, gridPos, etc.
  const patched = safeDeepClone(rawPanel);
  if (flags.titleChanged) {
    patched.title = normPanel.title;
  }
  if (flags.descChanged) {
    if (normPanel.description) {
      patched.description = normPanel.description;
    } else {
      delete patched.description;
    }
  }
  if (flags.unitChanged || flags.thresholdsChanged) {
    patchSameTypePanelFieldConfig(patched, fresh, normPanel, flags.unitChanged, flags.thresholdsChanged);
  }
  if (flags.queriesChanged) {
    patchPanelTargets(patched, normPanel, initialPanel, locator.importedTargetIndices);
  }
  if (flags.layoutChanged) {
    patchSameTypePanelGridPos(patched, initialPanel, currentPanel, normPanel);
  }
  return patched;
}

function patchVariableTypeSpecificFields(
  patched: Record<string, unknown>,
  fresh: Record<string, unknown>,
  initialVar: VariableIR,
  currentVar: VariableIR,
  normVar: NormalizedVariableSpec
): void {
  if (currentVar.type === 'query') {
    const queryChanged =
      (currentVar.labelName ?? '') !== (initialVar.labelName ?? '') ||
      (currentVar.metricName ?? '') !== (initialVar.metricName ?? '');
    if (queryChanged) {
      patched.query = normVar.query;
      if (normVar.datasource) {
        patched.datasource = { ...normVar.datasource };
      }
    }
    return;
  }
  if (currentVar.type === 'custom') {
    const customChanged =
      !stringArraysEqual(currentVar.options, initialVar.options) ||
      Boolean(currentVar.includeAll) !== Boolean(initialVar.includeAll) ||
      (currentVar.defaultValue ?? '') !== (initialVar.defaultValue ?? '');
    if (customChanged) {
      patched.query = normVar.query;
      if (fresh.options !== undefined) {
        patched.options = safeDeepClone(fresh.options);
      }
    }
  }
}

function patchSingleRawVariable(
  rawVar: Record<string, unknown>,
  initialVar: VariableIR,
  currentVar: VariableIR,
  normVar: NormalizedVariableSpec
): Record<string, unknown> {
  const fresh = buildFoundationVariableObject(normVar);
  if (currentVar.type !== initialVar.type) {
    return fresh;
  }
  const patched = safeDeepClone(rawVar);
  if ((currentVar.label ?? '') !== (initialVar.label ?? '')) {
    patched.label = normVar.label;
  }
  if (Boolean(currentVar.multi) !== Boolean(initialVar.multi)) {
    patched.multi = normVar.multi;
  }
  if (Boolean(currentVar.includeAll) !== Boolean(initialVar.includeAll)) {
    patched.includeAll = normVar.includeAll;
  }
  const defaultOrMultiChanged =
    (currentVar.defaultValue ?? '') !== (initialVar.defaultValue ?? '') ||
    Boolean(currentVar.multi) !== Boolean(initialVar.multi);
  if (defaultOrMultiChanged && fresh.current !== undefined) {
    patched.current = safeDeepClone(fresh.current);
  }
  patchVariableTypeSpecificFields(patched, fresh, initialVar, currentVar, normVar);
  return patched;
}

function collectPanelMetrics(panels: unknown[]): { maxNumericId: number; maxBottomY: number } {
  let maxNumericId = 0;
  let maxBottomY = 0;
  for (const item of panels) {
    if (!isRecord(item)) {
      continue;
    }
    if (typeof item.id === 'number' && Number.isInteger(item.id) && item.id > maxNumericId) {
      maxNumericId = item.id;
    }
    if (isRecord(item.gridPos)) {
      const y = typeof item.gridPos.y === 'number' ? item.gridPos.y : 0;
      const h = typeof item.gridPos.h === 'number' ? item.gridPos.h : 0;
      if (y + h > maxBottomY) {
        maxBottomY = y + h;
      }
    }
    if (item.type === 'row' && Array.isArray(item.panels)) {
      for (const child of item.panels) {
        if (isRecord(child) && typeof child.id === 'number' && Number.isInteger(child.id) && child.id > maxNumericId) {
          maxNumericId = child.id;
        }
      }
    }
  }
  return { maxNumericId, maxBottomY };
}

/**
 * Non-destructively patches the original fetched Grafana dashboard `spec` using only the
 * diff between `patchContext.initialDraft` (import baseline) and `currentIR` / `currentSpec`.
 *
 * Guarantees:
 * - Unsupported panels, panels beyond maxPanels, row containers, and skipped variables are preserved verbatim.
 * - Unmodified imported panels retain their exact original JSON (transformations, fieldConfig.overrides,
 *   options, gridPos, hidden/other-datasource targets, links, etc.).
 * - Modified panels mutate only the fields that actually changed in the IR.
 * - Newly added panels receive non-colliding numeric IDs and non-overlapping gridPos below existing panels.
 */
export function buildPatchedDashboardSpec({
  targetUid,
  currentIR,
  currentSpec,
  patchContext,
}: BuildPatchedDashboardSpecParams): Record<string, unknown> {
  const patched = safeDeepClone(patchContext.originalSpec);
  const { initialDraft, panelBindings, variableBindings } = patchContext;

  patched.uid = targetUid;
  patchDashboardMeta(patched, initialDraft, currentIR, currentSpec);
  patched.panels = patchDashboardPanels(
    Array.isArray(patched.panels) ? [...patched.panels] : [],
    initialDraft,
    currentIR,
    currentSpec,
    panelBindings
  );
  patchDashboardVariables(patched, initialDraft, currentIR, currentSpec, variableBindings);
  return patched;
}

function patchDashboardMeta(
  patched: Record<string, unknown>,
  initialDraft: DashboardIR,
  currentIR: DashboardIR,
  currentSpec: NormalizedDashboardSpec
): void {
  // 1. Dashboard-level metadata: update ONLY fields that actually changed from import baseline.
  if (currentIR.title !== initialDraft.title) {
    patched.title = currentSpec.title;
  }
  if ((currentIR.description ?? '') !== (initialDraft.description ?? '')) {
    if (currentSpec.description) {
      patched.description = currentSpec.description;
    } else {
      delete patched.description;
    }
  }
  if (
    currentIR.timeRange.from !== initialDraft.timeRange.from ||
    currentIR.timeRange.to !== initialDraft.timeRange.to
  ) {
    const prevTime = isRecord(patched.time) ? safeDeepClone(patched.time) : {};
    patched.time = {
      ...prevTime,
      from: currentSpec.timeRange.from,
      to: currentSpec.timeRange.to,
    };
  }
}

function patchDashboardPanels(
  rawPanels: unknown[],
  initialDraft: DashboardIR,
  currentIR: DashboardIR,
  currentSpec: NormalizedDashboardSpec,
  panelBindings: Record<string, ImportedPanelLocator>
): unknown[] {
  const { maxNumericId, maxBottomY } = collectPanelMetrics(rawPanels);
  const initialPanelById = new Map<string, PanelIR>(initialDraft.panels.map((p) => [p.id, p]));
  const currentPanelById = new Map<string, PanelIR>(currentIR.panels.map((p) => [p.id, p]));
  const normPanelById = new Map<string, NormalizedPanelSpec>(
    currentSpec.panels.map((p) => [p.id, p])
  );

  const topRemovals: number[] = [];
  const rowChildRemovals = new Map<number, number[]>();

  for (const [panelId, locator] of Object.entries(panelBindings)) {
    applySinglePanelBinding(
      rawPanels,
      locator,
      initialPanelById.get(panelId),
      currentPanelById.get(panelId),
      normPanelById.get(panelId),
      topRemovals,
      rowChildRemovals
    );
  }

  applyPanelRemovals(rawPanels, topRemovals, rowChildRemovals);
  appendAddedPanels(rawPanels, currentSpec.panels, panelBindings, maxNumericId, maxBottomY);
  return rawPanels;
}

function applySinglePanelBinding(
  rawPanels: unknown[],
  locator: ImportedPanelLocator,
  initialPanel: PanelIR | undefined,
  currentPanel: PanelIR | undefined,
  normPanel: NormalizedPanelSpec | undefined,
  topRemovals: number[],
  rowChildRemovals: Map<number, number[]>
): void {
  const containerItem = rawPanels[locator.topIndex];
  if (!isRecord(containerItem)) {
    return;
  }
  if (locator.rowChildIndex !== undefined) {
    const rowPanels = Array.isArray(containerItem.panels) ? containerItem.panels : null;
    if (!rowPanels || locator.rowChildIndex >= rowPanels.length) {
      return;
    }
    if (!currentPanel) {
      const list = rowChildRemovals.get(locator.topIndex) ?? [];
      list.push(locator.rowChildIndex);
      rowChildRemovals.set(locator.topIndex, list);
      return;
    }
    if (!initialPanel || panelIREqual(initialPanel, currentPanel) || !normPanel) {
      return;
    }
    const targetRaw = rowPanels[locator.rowChildIndex];
    if (isRecord(targetRaw)) {
      rowPanels[locator.rowChildIndex] = patchSingleRawPanel(
        targetRaw,
        initialPanel,
        currentPanel,
        normPanel,
        locator
      );
    }
    return;
  }

  if (!currentPanel) {
    topRemovals.push(locator.topIndex);
    return;
  }
  if (!initialPanel || panelIREqual(initialPanel, currentPanel) || !normPanel) {
    return;
  }
  rawPanels[locator.topIndex] = patchSingleRawPanel(
    containerItem,
    initialPanel,
    currentPanel,
    normPanel,
    locator
  );
}

function applyPanelRemovals(
  rawPanels: unknown[],
  topRemovals: number[],
  rowChildRemovals: Map<number, number[]>
): void {
  // Apply row child removals in descending index order.
  for (const [topIdx, childIndices] of rowChildRemovals.entries()) {
    const rowObj = rawPanels[topIdx];
    if (isRecord(rowObj) && Array.isArray(rowObj.panels)) {
      for (const idx of [...childIndices].sort((a, b) => b - a)) {
        rowObj.panels.splice(idx, 1);
      }
    }
  }
  // Apply top-level panel removals in descending index order.
  for (const topIdx of [...topRemovals].sort((a, b) => b - a)) {
    rawPanels.splice(topIdx, 1);
  }
}

function appendAddedPanels(
  rawPanels: unknown[],
  specPanels: NormalizedPanelSpec[],
  panelBindings: Record<string, ImportedPanelLocator>,
  initialMaxId: number,
  maxBottomY: number
): void {
  const addedNormPanels = specPanels.filter((p) => !(p.id in panelBindings));
  if (addedNormPanels.length === 0) {
    return;
  }
  let nextNumericId = initialMaxId;
  const minAddedY = Math.min(...addedNormPanels.map((p) => p.gridPos.y));
  for (const added of addedNormPanels) {
    const built = buildFoundationPanelObject(added);
    built.id = ++nextNumericId;
    built.gridPos = {
      x: added.gridPos.x,
      y: maxBottomY + (added.gridPos.y - minAddedY),
      w: added.gridPos.w,
      h: added.gridPos.h,
    };
    rawPanels.push(built);
  }
}

function patchDashboardVariables(
  patched: Record<string, unknown>,
  initialDraft: DashboardIR,
  currentIR: DashboardIR,
  currentSpec: NormalizedDashboardSpec,
  variableBindings: Record<string, number>
): void {
  const initialVarByName = new Map<string, VariableIR>(
    (initialDraft.variables ?? []).map((v) => [v.name, v])
  );
  const currentVarByName = new Map<string, VariableIR>(
    (currentIR.variables ?? []).map((v) => [v.name, v])
  );
  const normVarByName = new Map<string, NormalizedVariableSpec>(
    currentSpec.variables.map((v) => [v.name, v])
  );

  const hasVariableChanges =
    Object.keys(variableBindings).some((name) => {
      const initV = initialVarByName.get(name);
      const curV = currentVarByName.get(name);
      return !curV || !initV || !variableIREqual(initV, curV);
    }) || currentSpec.variables.some((v) => !(v.name in variableBindings));

  if (!hasVariableChanges) {
    return;
  }

  const templatingObj: Record<string, unknown> = isRecord(patched.templating)
    ? safeDeepClone(patched.templating)
    : {};
  const rawVars: unknown[] = Array.isArray(templatingObj.list) ? [...templatingObj.list] : [];
  const varRemovals: number[] = [];

  for (const [varName, origIdx] of Object.entries(variableBindings)) {
    if (origIdx < 0 || origIdx >= rawVars.length) {
      continue;
    }
    const initialVar = initialVarByName.get(varName);
    const currentVar = currentVarByName.get(varName);
    if (!currentVar) {
      varRemovals.push(origIdx);
      continue;
    }
    if (!initialVar || variableIREqual(initialVar, currentVar)) {
      continue;
    }
    const normVar = normVarByName.get(varName);
    const existingRawVar = rawVars[origIdx];
    if (normVar && isRecord(existingRawVar)) {
      rawVars[origIdx] = patchSingleRawVariable(existingRawVar, initialVar, currentVar, normVar);
    }
  }

  for (const idx of [...varRemovals].sort((a, b) => b - a)) {
    rawVars.splice(idx, 1);
  }

  for (const addedVar of currentSpec.variables) {
    if (!(addedVar.name in variableBindings)) {
      rawVars.push(buildFoundationVariableObject(addedVar));
    }
  }

  templatingObj.list = rawVars;
  patched.templating = templatingObj;
}
