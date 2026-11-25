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

  const updatedByOrigIdx = new Map<number, Record<string, unknown>>();
  const removeOrigIndices = new Set<number>();
  const appendedTargets: Array<Record<string, unknown>> = [];

  for (let qi = 0; qi < normPanel.targets.length; qi++) {
    const newTarget = normPanel.targets[qi];
    if (qi < importedTargetIndices.length) {
      const origIdx = importedTargetIndices[qi];
      const existing = origTargets[origIdx];
      const cloned: Record<string, unknown> = isRecord(existing)
        ? safeDeepClone(existing)
        : buildFoundationTargetObject(newTarget);
      cloned.expr = newTarget.expr;
      cloned.datasource = { ...newTarget.datasource };
      const origRefId = typeof cloned.refId === 'string' ? cloned.refId.trim() : '';
      if (origRefId && !usedRefIds.has(origRefId)) {
        usedRefIds.add(origRefId);
      } else {
        cloned.refId = !usedRefIds.has(newTarget.refId)
          ? (usedRefIds.add(newTarget.refId), newTarget.refId)
          : nextAvailableRefId(usedRefIds);
      }
      const prevLegend = initialQueries[qi]?.legendFormat ?? '';
      const nextLegend = newTarget.legendFormat ?? '';
      if (nextLegend !== prevLegend) {
        if (nextLegend) {
          cloned.legendFormat = nextLegend;
        } else {
          delete cloned.legendFormat;
        }
      }
      updatedByOrigIdx.set(origIdx, cloned);
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
    if (updatedByOrigIdx.has(i)) {
      nextTargets.push(updatedByOrigIdx.get(i));
    } else {
      nextTargets.push(origTargets[i]);
    }
  }
  for (const extra of appendedTargets) {
    nextTargets.push(extra);
  }
  rawPanel.targets = nextTargets;
}

function patchSingleRawPanel(
  rawPanel: Record<string, unknown>,
  initialPanel: PanelIR,
  currentPanel: PanelIR,
  normPanel: NormalizedPanelSpec,
  locator: ImportedPanelLocator
): Record<string, unknown> {
  const fresh = buildFoundationPanelObject(normPanel);
  const typeChanged = currentPanel.type !== initialPanel.type;
  const unitChanged = currentPanel.unit !== initialPanel.unit;
  const thresholdsChanged = !thresholdsEqual(initialPanel.thresholds, currentPanel.thresholds);
  const queriesChanged = !queriesEqual(initialPanel, currentPanel);
  const layoutChanged = !layoutEqual(initialPanel.layout, currentPanel.layout);
  const titleChanged = currentPanel.title !== initialPanel.title;
  const descChanged = (currentPanel.description ?? '') !== (initialPanel.description ?? '');

  if (typeChanged) {
    const nextPanel = safeDeepClone(fresh);
    if (typeof rawPanel.id === 'number') {
      nextPanel.id = rawPanel.id;
    }
    if (!titleChanged && typeof rawPanel.title === 'string') {
      nextPanel.title = rawPanel.title;
    }
    if (!descChanged) {
      if (rawPanel.description !== undefined) {
        nextPanel.description = rawPanel.description;
      } else {
        delete nextPanel.description;
      }
    }
    if (!layoutChanged && isRecord(rawPanel.gridPos)) {
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
    // Preserve untouched fieldConfig.overrides and extra fieldConfig.defaults fields.
    if (isRecord(rawPanel.fieldConfig)) {
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
    if (!queriesChanged && Array.isArray(rawPanel.targets)) {
      nextPanel.targets = safeDeepClone(rawPanel.targets);
    } else if (queriesChanged) {
      nextPanel.targets = Array.isArray(rawPanel.targets) ? safeDeepClone(rawPanel.targets) : [];
      patchPanelTargets(nextPanel, normPanel, initialPanel, locator.importedTargetIndices);
    }
    return nextPanel;
  }

  // Same panel type: mutate ONLY the changed fields on the cloned original panel,
  // preserving transformations, fieldConfig.overrides, options, untouched defaults, gridPos, etc.
  const patched = safeDeepClone(rawPanel);
  if (titleChanged) {
    patched.title = normPanel.title;
  }
  if (descChanged) {
    if (normPanel.description) {
      patched.description = normPanel.description;
    } else {
      delete patched.description;
    }
  }
  if (unitChanged || thresholdsChanged) {
    const fc: Record<string, unknown> = isRecord(patched.fieldConfig)
      ? safeDeepClone(patched.fieldConfig)
      : {};
    const defaults: Record<string, unknown> = isRecord(fc.defaults)
      ? safeDeepClone(fc.defaults)
      : {};
    const freshDefaults = isRecord(fresh.fieldConfig) && isRecord(fresh.fieldConfig.defaults)
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
  if (queriesChanged) {
    patchPanelTargets(patched, normPanel, initialPanel, locator.importedTargetIndices);
  }
  if (layoutChanged) {
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
  return patched;
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
  if (
    (currentVar.defaultValue ?? '') !== (initialVar.defaultValue ?? '') ||
    Boolean(currentVar.multi) !== Boolean(initialVar.multi)
  ) {
    if (fresh.current !== undefined) {
      patched.current = safeDeepClone(fresh.current);
    }
  }
  if (currentVar.type === 'query') {
    if (
      (currentVar.labelName ?? '') !== (initialVar.labelName ?? '') ||
      (currentVar.metricName ?? '') !== (initialVar.metricName ?? '')
    ) {
      patched.query = normVar.query;
      if (normVar.datasource) {
        patched.datasource = { ...normVar.datasource };
      }
    }
  } else if (currentVar.type === 'custom') {
    if (
      !stringArraysEqual(currentVar.options, initialVar.options) ||
      Boolean(currentVar.includeAll) !== Boolean(initialVar.includeAll) ||
      (currentVar.defaultValue ?? '') !== (initialVar.defaultValue ?? '')
    ) {
      patched.query = normVar.query;
      if (fresh.options !== undefined) {
        patched.options = safeDeepClone(fresh.options);
      }
    }
  }
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

  // 2. Patch panels in place while keeping skipped/unsupported/untouched panels intact.
  const rawPanels: unknown[] = Array.isArray(patched.panels) ? [...patched.panels] : [];
  const { maxNumericId: initialMaxId, maxBottomY } = collectPanelMetrics(rawPanels);
  let nextNumericId = initialMaxId;

  const initialPanelById = new Map<string, PanelIR>(
    initialDraft.panels.map((p) => [p.id, p])
  );
  const currentPanelById = new Map<string, PanelIR>(
    currentIR.panels.map((p) => [p.id, p])
  );
  const normPanelById = new Map<string, NormalizedPanelSpec>(
    currentSpec.panels.map((p) => [p.id, p])
  );

  const topRemovals: number[] = [];
  const rowChildRemovals = new Map<number, number[]>();

  for (const [panelId, locator] of Object.entries(panelBindings)) {
    const initialPanel = initialPanelById.get(panelId);
    const currentPanel = currentPanelById.get(panelId);

    const containerItem = rawPanels[locator.topIndex];
    if (!isRecord(containerItem)) {
      continue;
    }

    if (locator.rowChildIndex !== undefined) {
      const rowPanels = Array.isArray(containerItem.panels) ? containerItem.panels : null;
      if (!rowPanels || locator.rowChildIndex >= rowPanels.length) {
        continue;
      }
      if (!currentPanel) {
        const list = rowChildRemovals.get(locator.topIndex) ?? [];
        list.push(locator.rowChildIndex);
        rowChildRemovals.set(locator.topIndex, list);
        continue;
      }
      if (!initialPanel || panelIREqual(initialPanel, currentPanel)) {
        continue;
      }
      const targetRaw = rowPanels[locator.rowChildIndex];
      const normPanel = normPanelById.get(panelId);
      if (isRecord(targetRaw) && normPanel) {
        rowPanels[locator.rowChildIndex] = patchSingleRawPanel(
          targetRaw,
          initialPanel,
          currentPanel,
          normPanel,
          locator
        );
      }
    } else {
      if (!currentPanel) {
        topRemovals.push(locator.topIndex);
        continue;
      }
      if (!initialPanel || panelIREqual(initialPanel, currentPanel)) {
        continue;
      }
      const normPanel = normPanelById.get(panelId);
      if (normPanel) {
        rawPanels[locator.topIndex] = patchSingleRawPanel(
          containerItem,
          initialPanel,
          currentPanel,
          normPanel,
          locator
        );
      }
    }
  }

  // Apply row child removals in descending index order.
  for (const [topIdx, childIndices] of rowChildRemovals.entries()) {
    const rowObj = rawPanels[topIdx];
    if (isRecord(rowObj) && Array.isArray(rowObj.panels)) {
      const sorted = [...childIndices].sort((a, b) => b - a);
      for (const idx of sorted) {
        rowObj.panels.splice(idx, 1);
      }
    }
  }

  // Apply top-level panel removals in descending index order.
  for (const topIdx of [...topRemovals].sort((a, b) => b - a)) {
    rawPanels.splice(topIdx, 1);
  }

  // Append newly added panels below all existing panels with non-colliding numeric IDs.
  const addedNormPanels = currentSpec.panels.filter((p) => !(p.id in panelBindings));
  if (addedNormPanels.length > 0) {
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
  patched.panels = rawPanels;

  // 3. Patch template variables while keeping skipped/hidden/untouched variables intact.
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

  if (hasVariableChanges) {
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

  return patched;
}
