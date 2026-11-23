#!/usr/bin/env node
/**
 * Verifies that schemas/dashboard-ir.schema.json (Single Source of Truth) stays
 * synchronized with Go (pkg/ir/validate.go) and TypeScript (src/ir/types.ts),
 * and that the JSON Schema remains compatible with grafana-llm-app's go-openai
 * jsonschema.Definition (string-only enums, no unsupported numeric enums).
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

assertStringOnlyEnums(irSchema, 'dashboard-ir');
assertStringOnlyEnums(patchSchema, 'dashboard-patch');

const units = irSchema.properties.panels.items.properties.unit.enum;
const panelTypes = irSchema.properties.panels.items.properties.type.enum;
const timeFrom = irSchema.properties.timeRange.properties.from.enum;

for (const u of units) {
  if (!tsTypes.includes(`'${u}'`) || !goValidate.includes(`"${u}"`)) {
    throw new Error(`Unit "${u}" missing in TS or Go validator`);
  }
}
for (const pt of panelTypes) {
  if (!tsTypes.includes(`'${pt}'`) || !goValidate.includes(`"${pt}"`)) {
    throw new Error(`Panel type "${pt}" missing in TS or Go validator`);
  }
}
for (const tf of timeFrom) {
  if (!tsTypes.includes(`'${tf}'`) || !goValidate.includes(`"${tf}"`)) {
    throw new Error(`timeRange.from "${tf}" missing in TS or Go validator`);
  }
}

console.log(
  `Schema sync OK: ${units.length} units, ${panelTypes.length} panel types, ${timeFrom.length} timeRange options, string-only enums verified.`
);
