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
