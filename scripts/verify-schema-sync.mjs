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
const reportSchema = JSON.parse(
  fs.readFileSync(path.join(root, 'schemas/status-report.schema.json'), 'utf8')
);
const tsTypes = fs.readFileSync(path.join(root, 'src/ir/types.ts'), 'utf8');
const tsReportTypes = fs.readFileSync(path.join(root, 'src/ir/reportTypes.ts'), 'utf8');
const goValidate = fs.readFileSync(path.join(root, 'pkg/ir/validate.go'), 'utf8');
const goReportValidate = fs.readFileSync(path.join(root, 'pkg/ir/report_validate.go'), 'utf8');

function assertGoOpenAICompatible(node, trail = '$') {
  if (!node || typeof node !== 'object') {
    return;
  }
  if ('oneOf' in node || 'anyOf' in node) {
    throw new Error(
      `Unsupported keyword oneOf/anyOf at ${trail} breaks grafana-llm-app (go-openai jsonschema.Definition)`
    );
  }
  if ('const' in node || 'format' in node) {
    throw new Error(
      `Unsupported keyword const/format at ${trail} is silently dropped by grafana-llm-app (go-openai jsonschema.Definition)`
    );
  }
  if (Array.isArray(node.type)) {
    throw new Error(
      `Array type ${JSON.stringify(node.type)} at ${trail} breaks grafana-llm-app (go-openai jsonschema.Definition.Type is DataType string)`
    );
  }
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
      assertGoOpenAICompatible(v, `${trail}.${k}`);
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

assertGoOpenAICompatible(irSchema, 'dashboard-ir');
assertGoOpenAICompatible(patchSchema, 'dashboard-patch');

const irPanelProps = irSchema.properties.panels.items.properties;
const irVarProps = irSchema.properties.variables.items.properties;
const patchOpProps = patchSchema.properties.operations.items.properties;
const patchPanelProps = patchOpProps.panel.properties;
const patchVarProps = patchOpProps.variable.properties;

const checks = [
  {
    label: 'schemaVersion',
    irEnum: irSchema.properties.schemaVersion.enum,
    patchEnum: patchSchema.properties.schemaVersion.enum,
    tsConst: 'ALLOWED_SCHEMA_VERSIONS',
    goMap: 'allowedSchemaVersions',
  },
  {
    label: 'timeRange.from',
    irEnum: irSchema.properties.timeRange.properties.from.enum,
    patchEnum: patchOpProps.timeRange.properties.from.enum,
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
  {
    label: 'panel.query.refId',
    irEnum: irPanelProps.query.properties.refId.enum,
    patchEnum: patchPanelProps.query.properties.refId.enum,
    tsConst: 'ALLOWED_QUERY_REF_IDS',
    goMap: 'allowedQueryRefIDs',
  },
  {
    label: 'panel.queries.refId',
    irEnum: irPanelProps.queries.items.properties.refId.enum,
    patchEnum: patchPanelProps.queries.items.properties.refId.enum,
    tsConst: 'ALLOWED_QUERY_REF_IDS',
    goMap: 'allowedQueryRefIDs',
  },
  {
    label: 'panel.query.kind',
    irEnum: irPanelProps.query.properties.kind.enum,
    patchEnum: patchPanelProps.query.properties.kind.enum,
    tsConst: 'ALLOWED_QUERY_KINDS',
    goMap: 'allowedQueryKinds',
  },
  {
    label: 'panel.queries.kind',
    irEnum: irPanelProps.queries.items.properties.kind.enum,
    patchEnum: patchPanelProps.queries.items.properties.kind.enum,
    tsConst: 'ALLOWED_QUERY_KINDS',
    goMap: 'allowedQueryKinds',
  },
  {
    label: 'variable.type',
    irEnum: irVarProps.type.enum,
    patchEnum: patchVarProps.type.enum,
    tsConst: 'ALLOWED_VARIABLE_TYPES',
    goMap: 'allowedVariableTypes',
  },
];

for (const c of checks) {
  assertExactSetEqual(c.label, c.irEnum, c.patchEnum, 'dashboard-patch.schema.json');
  assertExactSetEqual(c.label, c.irEnum, extractTsConstArray(tsTypes, c.tsConst), 'src/ir/types.ts');
  assertExactSetEqual(c.label, c.irEnum, extractGoMapKeys(goValidate, c.goMap), 'pkg/ir/validate.go');
}

// Verify fixed enum anchors for datasourceRef and timeRange.to across IR and Patch schemas.
assertExactSetEqual('timeRange.to', ['now'], irSchema.properties.timeRange.properties.to.enum, 'dashboard-ir.schema.json');
assertExactSetEqual('timeRange.to', ['now'], patchOpProps.timeRange.properties.to.enum, 'dashboard-patch.schema.json');
assertExactSetEqual('panel.datasourceRef', ['selected_prometheus'], irPanelProps.datasourceRef.enum, 'dashboard-ir.schema.json');
assertExactSetEqual('panel.datasourceRef', ['selected_prometheus'], patchPanelProps.datasourceRef.enum, 'dashboard-patch.schema.json');
assertExactSetEqual('variable.datasourceRef', ['selected_prometheus'], irVarProps.datasourceRef.enum, 'dashboard-ir.schema.json');
assertExactSetEqual('variable.datasourceRef', ['selected_prometheus'], patchVarProps.datasourceRef.enum, 'dashboard-patch.schema.json');

// Verify patch.operations.op enum sync across dashboard-patch.schema.json, TS, and Go.
const patchOpEnum = patchOpProps.op.enum;
assertExactSetEqual('patch.operations.op', patchOpEnum, extractTsConstArray(tsTypes, 'ALLOWED_PATCH_OPS'), 'src/ir/types.ts');
assertExactSetEqual('patch.operations.op', patchOpEnum, extractGoMapKeys(goValidate, 'allowedPatchOps'), 'pkg/ir/validate.go');

// Verify status-report.schema.json go-openai compatibility and enum sync across TS and Go.
assertGoOpenAICompatible(reportSchema, 'status-report');
const reportChecks = [
  {
    label: 'report.schemaVersion',
    schemaEnum: reportSchema.properties.schemaVersion.enum,
    tsConst: 'ALLOWED_REPORT_SCHEMA_VERSIONS',
    goMap: 'allowedReportSchemaVersions',
  },
  {
    label: 'report.overallStatus',
    schemaEnum: reportSchema.properties.overallStatus.enum,
    tsConst: 'ALLOWED_REPORT_OVERALL_STATUSES',
    goMap: 'allowedReportOverallStatuses',
  },
  {
    label: 'report.confidence',
    schemaEnum: reportSchema.properties.confidence.enum,
    tsConst: 'ALLOWED_REPORT_CONFIDENCES',
    goMap: 'allowedReportConfidences',
  },
  {
    label: 'report.findings.category',
    schemaEnum: reportSchema.properties.findings.items.properties.category.enum,
    tsConst: 'ALLOWED_REPORT_FINDING_CATEGORIES',
    goMap: 'allowedReportFindingCategories',
  },
  {
    label: 'report.findings.severity',
    schemaEnum: reportSchema.properties.findings.items.properties.severity.enum,
    tsConst: 'ALLOWED_REPORT_FINDING_SEVERITIES',
    goMap: 'allowedReportFindingSeverities',
  },
  {
    label: 'report.hypotheses.likelihood',
    schemaEnum: reportSchema.properties.hypotheses.items.properties.likelihood.enum,
    tsConst: 'ALLOWED_REPORT_LIKELIHOODS',
    goMap: 'allowedReportLikelihoods',
  },
  {
    label: 'report.recommendedActions.priority',
    schemaEnum: reportSchema.properties.recommendedActions.items.properties.priority.enum,
    tsConst: 'ALLOWED_REPORT_ACTION_PRIORITIES',
    goMap: 'allowedReportActionPriorities',
  },
];

for (const c of reportChecks) {
  assertExactSetEqual(c.label, c.schemaEnum, extractTsConstArray(tsReportTypes, c.tsConst), 'src/ir/reportTypes.ts');
  assertExactSetEqual(c.label, c.schemaEnum, extractGoMapKeys(goReportValidate, c.goMap), 'pkg/ir/report_validate.go');
}

console.log(
  `Bidirectional schema sync OK: ${checks.map((c) => `${c.label}(${c.irEnum.length})`).join(', ')}, patch.operations.op(${patchOpEnum.length}), status-report(${reportChecks.length} enums), datasourceRef/timeRange.to; go-openai compatibility verified.`
);
