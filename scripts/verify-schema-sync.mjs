#!/usr/bin/env node
/**
 * Bidirectional contract verification ensuring schemas/dashboard-ir.schema.json and
 * schemas/dashboard-patch.schema.json stay strictly synchronized with BOTH
 * Go (pkg/ir/validate.go) and TypeScript (src/ir/types.ts), and remain compatible
 * with grafana-llm-app's go-openai jsonschema.Definition (string-only enums).
 */
import fs from 'node:fs';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
const irSchema = JSON.parse(
  fs.readFileSync(path.join(root, 'schemas/dashboard-ir.schema.json'), 'utf8')
);
const patchSchema = JSON.parse(
  fs.readFileSync(path.join(root, 'schemas/dashboard-patch.schema.json'), 'utf8')
);
const tsTypes = fs.readFileSync(path.join(root, 'src/ir/types.ts'), 'utf8');
const goValidate = fs.readFileSync(path.join(root, 'pkg/ir/validate.go'), 'utf8');

function assertStringOnlyEnums(node, trail = '$') {
  if (!node || typeof node !== 'object') return;
  if (Array.isArray(node.enum)) {
    for (const v of node.enum) {
      if (typeof v !== 'string') {
        throw new Error(
          `Non-string enum value ${JSON.stringify(v)} at ${trail} breaks grafana-llm-app (go-openai jsonschema.Definition.Enum is []string)`
        );
      }
    }
  }
  for (const [k, v] of Object.entries(node)) {
    if (v && typeof v === 'object') {
      assertStringOnlyEnums(v, `${trail}.${k}`);
    }
  }
}

function extractTsConstArray(source, constName) {
  const re = new RegExp(`export const ${constName}\\s*=\\s*\\[([\\s\\S]*?)\\]\\s*as const`);
  const match = source.match(re);
  if (!match) {
    throw new Error(`Could not find TS const array ${constName}`);
  }
  return Array.from(match[1].matchAll(/'([^']+)'/g), (m) => m[1]);
}

function extractGoMapKeys(source, mapName) {
  const re = new RegExp(`${mapName}\\s*=\\s*map\\[string\\]bool\\{([\\s\\S]*?)\\}`);
  const match = source.match(re);
  if (!match) {
    throw new Error(`Could not find Go map ${mapName}`);
  }
  return Array.from(match[1].matchAll(/"([^"]+)"\s*:\s*true/g), (m) => m[1]);
}

function assertExactSetEqual(label, expected, actual, targetName) {
  const exp = [...new Set(expected)].sort();
  const act = [...new Set(actual)].sort();
  if (JSON.stringify(exp) !== JSON.stringify(act)) {
    throw new Error(
      `Enum mismatch for ${label} in ${targetName}:\n  schema: ${JSON.stringify(exp)}\n  actual: ${JSON.stringify(act)}`
    );
  }
}

assertStringOnlyEnums(irSchema, 'dashboard-ir');
assertStringOnlyEnums(patchSchema, 'dashboard-patch');

const irPanelProps = irSchema.properties.panels.items.properties;
const patchPanelProps = patchSchema.properties.operations.items.properties.panel.properties;

const checks = [
  {
    label: 'timeRange.from',
    irEnum: irSchema.properties.timeRange.properties.from.enum,
    patchEnum: patchSchema.properties.operations.items.properties.timeRange.properties.from.enum,
    tsConst: 'ALLOWED_TIME_FROM',
    goMap: 'allowedTimeFrom',
  },
  {
    label: 'panel.type',
    irEnum: irPanelProps.type.enum,
    patchEnum: patchPanelProps.type.enum,
    tsConst: 'ALLOWED_PANEL_TYPES',
    goMap: 'allowedPanelTypes',
  },
  {
    label: 'panel.unit',
    irEnum: irPanelProps.unit.enum,
    patchEnum: patchPanelProps.unit.enum,
    tsConst: 'ALLOWED_UNITS',
    goMap: 'allowedUnits',
  },
  {
    label: 'panel.thresholds.color',
    irEnum: irPanelProps.thresholds.items.properties.color.enum,
    patchEnum: patchPanelProps.thresholds.items.properties.color.enum,
    tsConst: 'ALLOWED_THRESHOLD_COLORS',
    goMap: 'allowedThresholdColors',
  },
  {
    label: 'panel.layout.width',
    irEnum: irPanelProps.layout.properties.width.enum,
    patchEnum: patchPanelProps.layout.properties.width.enum,
    tsConst: 'ALLOWED_WIDTHS',
    goMap: 'allowedWidths',
  },
];

for (const c of checks) {
  assertExactSetEqual(c.label, c.irEnum, c.patchEnum, 'dashboard-patch.schema.json');
  assertExactSetEqual(c.label, c.irEnum, extractTsConstArray(tsTypes, c.tsConst), 'src/ir/types.ts');
  assertExactSetEqual(c.label, c.irEnum, extractGoMapKeys(goValidate, c.goMap), 'pkg/ir/validate.go');
}

console.log(
  `Bidirectional schema sync OK: ${checks.map((c) => `${c.label}(${c.irEnum.length})`).join(', ')}; string-only enums verified.`
);
