package schemas

import _ "embed"

// DashboardIRSchemaJSON is the canonical JSON Schema for DashboardIR.
//
//go:embed dashboard-ir.schema.json
var DashboardIRSchemaJSON []byte

// DashboardPatchSchemaJSON is the canonical JSON Schema for DashboardPatch.
//
//go:embed dashboard-patch.schema.json
var DashboardPatchSchemaJSON []byte

// StatusReportSchemaJSON is the canonical JSON Schema for StatusReportIR.
//
//go:embed status-report.schema.json
var StatusReportSchemaJSON []byte
