package ir

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	// ErrRevisionConflict indicates a patch targeted a stale draft revision.
	ErrRevisionConflict = errors.New("revision conflict")
	// ErrPanelNotFound indicates a patch referenced an unknown panelId.
	ErrPanelNotFound = errors.New("panel not found")
	// ErrVariableNotFound indicates a patch referenced an unknown variableName.
	ErrVariableNotFound = errors.New("variable not found")
	// ErrNoOpPatch indicates a patch produced zero effective changes.
	ErrNoOpPatch = errors.New("patch produced no changes")
)

// ApplyPatch applies typed partial edit operations to current DashboardIR,
// enforcing baseRevision matching, summary validation, no-op rejection,
// preserving untouched panels and variables, and re-validating the entire resulting IR.
func ApplyPatch(current DashboardIR, patch DashboardPatch, opts ValidateOptions) (DashboardIR, []DiffEntry, error) {
	if !allowedSchemaVersions[patch.SchemaVersion] {
		return DashboardIR{}, nil, fmt.Errorf("patch schemaVersion %q is not allowed (allowed: 1.0, 1.1)", patch.SchemaVersion)
	}
	if patch.BaseRevision != current.Revision {
		return DashboardIR{}, nil, fmt.Errorf("%w: patch baseRevision=%d does not match current revision=%d", ErrRevisionConflict, patch.BaseRevision, current.Revision)
	}
	if err := validatePlainText("summary", patch.Summary, 1, MaxSummaryLen); err != nil {
		return DashboardIR{}, nil, err
	}
	if len(patch.Operations) == 0 {
		return DashboardIR{}, nil, errors.New("patch must contain at least 1 operation")
	}
	if len(patch.Operations) > 6 {
		return DashboardIR{}, nil, fmt.Errorf("patch operation count %d exceeds limit 6", len(patch.Operations))
	}

	next := current.Clone()
	var diffs []DiffEntry

	for idx, op := range patch.Operations {
		if !allowedPatchOps[op.Op] {
			return DashboardIR{}, nil, fmt.Errorf("operations[%d]: unsupported op %q", idx, op.Op)
		}

		switch op.Op {
		case "update_meta":
			if op.Title != "" && op.Title != next.Title {
				diffs = append(diffs, DiffEntry{Op: "update_meta", Field: "title", Before: next.Title, After: op.Title})
				next.Title = op.Title
			}
			if op.Description != nil && *op.Description != next.Description {
				diffs = append(diffs, DiffEntry{Op: "update_meta", Field: "description", Before: next.Description, After: *op.Description})
				next.Description = *op.Description
			}
			if op.TimeRange != nil {
				before := fmt.Sprintf("%s..%s", next.TimeRange.From, next.TimeRange.To)
				after := fmt.Sprintf("%s..%s", op.TimeRange.From, op.TimeRange.To)
				if before != after {
					diffs = append(diffs, DiffEntry{Op: "update_meta", Field: "timeRange", Before: before, After: after})
					next.TimeRange = *op.TimeRange
				}
			}

		case "update_panel":
			if op.PanelID == "" || op.Panel == nil {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: update_panel requires panelId and panel", idx)
			}
			if op.Panel.ID != "" && op.Panel.ID != op.PanelID {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: cannot rename panel id from %q to %q", idx, op.PanelID, op.Panel.ID)
			}
			if op.Panel.Query != nil && op.Panel.Queries != nil {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: update_panel cannot specify both query and queries", idx)
			}
			pos := findPanelIndex(next.Panels, op.PanelID)
			if pos < 0 {
				return DashboardIR{}, nil, fmt.Errorf("%w: %q", ErrPanelNotFound, op.PanelID)
			}
			target := &next.Panels[pos]
			upd := op.Panel
			if upd.Type != "" && upd.Type != target.Type {
				diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "type", Before: target.Type, After: upd.Type})
				target.Type = upd.Type
			}
			if upd.Title != "" && upd.Title != target.Title {
				diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "title", Before: target.Title, After: upd.Title})
				target.Title = upd.Title
			}
			if upd.Description != nil && *upd.Description != target.Description {
				diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "description", Before: target.Description, After: *upd.Description})
				target.Description = *upd.Description
			}
			if upd.DatasourceRef != "" && upd.DatasourceRef != target.DatasourceRef {
				diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "datasourceRef", Before: target.DatasourceRef, After: upd.DatasourceRef})
				target.DatasourceRef = upd.DatasourceRef
			}
			if upd.Query != nil {
				if len(target.Queries) > 0 {
					newQ := QueryIR{Expr: upd.Query.Expr}
					if upd.Query.RefID != nil {
						newQ.RefID = *upd.Query.RefID
					}
					if upd.Query.LegendFormat != nil {
						newQ.LegendFormat = *upd.Query.LegendFormat
					} else if len(target.Queries) == 1 {
						newQ.LegendFormat = target.Queries[0].LegendFormat
					}
					if !queriesEqual(target.Queries, []QueryIR{newQ}) {
						diffs = append(diffs, DiffEntry{
							Op:      "update_panel",
							PanelID: target.ID,
							Field:   "query",
							Before:  formatQueries(target.Queries),
							After:   formatQueries([]QueryIR{newQ}),
						})
						target.Queries = nil
						target.Query = newQ
					}
				} else {
					if upd.Query.Expr != target.Query.Expr {
						diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "query.expr", Before: target.Query.Expr, After: upd.Query.Expr})
						target.Query.Expr = upd.Query.Expr
					}
					if upd.Query.LegendFormat != nil && *upd.Query.LegendFormat != target.Query.LegendFormat {
						diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "query.legendFormat", Before: target.Query.LegendFormat, After: *upd.Query.LegendFormat})
						target.Query.LegendFormat = *upd.Query.LegendFormat
					}
					if upd.Query.RefID != nil && effectiveRefID(*upd.Query.RefID, 0) != effectiveRefID(target.Query.RefID, 0) {
						diffs = append(diffs, DiffEntry{
							Op:      "update_panel",
							PanelID: target.ID,
							Field:   "query.refId",
							Before:  effectiveRefID(target.Query.RefID, 0),
							After:   effectiveRefID(*upd.Query.RefID, 0),
						})
						target.Query.RefID = *upd.Query.RefID
					}
				}
			}
			if upd.Queries != nil {
				if !queriesEqual(target.EffectiveQueries(), *upd.Queries) {
					diffs = append(diffs, DiffEntry{
						Op:      "update_panel",
						PanelID: target.ID,
						Field:   "queries",
						Before:  formatQueries(target.EffectiveQueries()),
						After:   formatQueries(*upd.Queries),
					})
					cp := make([]QueryIR, len(*upd.Queries))
					copy(cp, *upd.Queries)
					target.Query = QueryIR{}
					target.Queries = cp
				}
			}
			if upd.Unit != "" && upd.Unit != target.Unit {
				diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "unit", Before: target.Unit, After: upd.Unit})
				target.Unit = upd.Unit
			}
			if upd.Thresholds != nil {
				if !slices.Equal(target.Thresholds, *upd.Thresholds) {
					diffs = append(diffs, DiffEntry{
						Op:      "update_panel",
						PanelID: target.ID,
						Field:   "thresholds",
						Before:  formatThresholds(target.Thresholds),
						After:   formatThresholds(*upd.Thresholds),
					})
					cp := make([]ThresholdIR, len(*upd.Thresholds))
					copy(cp, *upd.Thresholds)
					target.Thresholds = cp
				}
			}
			if upd.Layout != nil && *upd.Layout != target.Layout {
				diffs = append(diffs, DiffEntry{
					Op:      "update_panel",
					PanelID: target.ID,
					Field:   "layout",
					Before:  fmt.Sprintf("r%d/c%d/%s", target.Layout.Row, target.Layout.Col, target.Layout.Width),
					After:   fmt.Sprintf("r%d/c%d/%s", upd.Layout.Row, upd.Layout.Col, upd.Layout.Width),
				})
				target.Layout = *upd.Layout
			}

		case "add_panel":
			if op.Panel == nil || (op.Panel.Query == nil && op.Panel.Queries == nil) || op.Panel.Layout == nil {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: add_panel requires complete panel definition", idx)
			}
			if op.Panel.Query != nil && op.Panel.Queries != nil {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: add_panel cannot specify both query and queries", idx)
			}
			if op.PanelID != "" && op.Panel.ID != "" && op.PanelID != op.Panel.ID {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: add_panel panelId %q conflicts with panel.id %q", idx, op.PanelID, op.Panel.ID)
			}
			panelID := op.Panel.ID
			if panelID == "" {
				panelID = op.PanelID
			}
			if findPanelIndex(next.Panels, panelID) >= 0 {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: panel %q already exists", idx, panelID)
			}
			newPanel := PanelIR{
				ID:            panelID,
				Type:          op.Panel.Type,
				Title:         op.Panel.Title,
				DatasourceRef: op.Panel.DatasourceRef,
				Unit:          op.Panel.Unit,
				Layout:        *op.Panel.Layout,
			}
			if op.Panel.Query != nil {
				newQuery := QueryIR{Expr: op.Panel.Query.Expr}
				if op.Panel.Query.RefID != nil {
					newQuery.RefID = *op.Panel.Query.RefID
				}
				if op.Panel.Query.LegendFormat != nil {
					newQuery.LegendFormat = *op.Panel.Query.LegendFormat
				}
				newPanel.Query = newQuery
			}
			if op.Panel.Queries != nil {
				newPanel.Queries = make([]QueryIR, len(*op.Panel.Queries))
				copy(newPanel.Queries, *op.Panel.Queries)
			}
			if op.Panel.Description != nil {
				newPanel.Description = *op.Panel.Description
			}
			if op.Panel.Thresholds != nil {
				newPanel.Thresholds = make([]ThresholdIR, len(*op.Panel.Thresholds))
				copy(newPanel.Thresholds, *op.Panel.Thresholds)
			}
			next.Panels = append(next.Panels, newPanel)
			diffs = append(diffs, DiffEntry{Op: "add_panel", PanelID: newPanel.ID, Field: "panel", After: newPanel.Title})

		case "remove_panel":
			if op.PanelID == "" {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: remove_panel requires panelId", idx)
			}
			pos := findPanelIndex(next.Panels, op.PanelID)
			if pos < 0 {
				return DashboardIR{}, nil, fmt.Errorf("%w: %q", ErrPanelNotFound, op.PanelID)
			}
			removed := next.Panels[pos]
			next.Panels = append(next.Panels[:pos], next.Panels[pos+1:]...)
			diffs = append(diffs, DiffEntry{Op: "remove_panel", PanelID: removed.ID, Field: "panel", Before: removed.Title})

		case "add_variable":
			if op.Variable == nil {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: add_variable requires variable definition", idx)
			}
			if op.VariableName != "" && op.Variable.Name != "" && op.VariableName != op.Variable.Name {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: add_variable variableName %q conflicts with variable.name %q", idx, op.VariableName, op.Variable.Name)
			}
			name := op.Variable.Name
			if name == "" {
				name = op.VariableName
			}
			if findVariableIndex(next.Variables, name) >= 0 {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: variable %q already exists", idx, name)
			}
			newVar := VariableIR{
				Name: name,
				Type: op.Variable.Type,
			}
			if op.Variable.Label != nil {
				newVar.Label = *op.Variable.Label
			}
			if op.Variable.DatasourceRef != nil {
				newVar.DatasourceRef = *op.Variable.DatasourceRef
			}
			if op.Variable.LabelName != nil {
				newVar.LabelName = *op.Variable.LabelName
			}
			if op.Variable.MetricName != nil {
				newVar.MetricName = *op.Variable.MetricName
			}
			if op.Variable.Options != nil {
				newVar.Options = make([]string, len(*op.Variable.Options))
				copy(newVar.Options, *op.Variable.Options)
			}
			if op.Variable.DefaultValue != nil {
				newVar.DefaultValue = *op.Variable.DefaultValue
			}
			if op.Variable.Multi != nil {
				newVar.Multi = *op.Variable.Multi
			}
			if op.Variable.IncludeAll != nil {
				newVar.IncludeAll = *op.Variable.IncludeAll
			}
			next.Variables = append(next.Variables, newVar)
			diffs = append(diffs, DiffEntry{Op: "add_variable", Field: "variables." + newVar.Name, After: newVar.Type})

		case "update_variable":
			name := op.VariableName
			if name == "" && op.Variable != nil {
				name = op.Variable.Name
			}
			if name == "" || op.Variable == nil {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: update_variable requires variableName and variable", idx)
			}
			if op.Variable.Name != "" && op.Variable.Name != name {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: cannot rename variable from %q to %q", idx, name, op.Variable.Name)
			}
			pos := findVariableIndex(next.Variables, name)
			if pos < 0 {
				return DashboardIR{}, nil, fmt.Errorf("%w: %q", ErrVariableNotFound, name)
			}
			target := &next.Variables[pos]
			upd := op.Variable
			if upd.Type != "" && upd.Type != target.Type {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".type", Before: target.Type, After: upd.Type})
				target.Type = upd.Type
				// Clear fields exclusive to the previous variable type so schema-compliant type switches validate cleanly.
				if upd.Type == "custom" {
					target.DatasourceRef = ""
					target.LabelName = ""
					target.MetricName = ""
				} else if upd.Type == "query" {
					target.Options = nil
				}
			}
			if upd.Label != nil && *upd.Label != target.Label {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".label", Before: target.Label, After: *upd.Label})
				target.Label = *upd.Label
			}
			if upd.DatasourceRef != nil && *upd.DatasourceRef != target.DatasourceRef {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".datasourceRef", Before: target.DatasourceRef, After: *upd.DatasourceRef})
				target.DatasourceRef = *upd.DatasourceRef
			}
			if upd.LabelName != nil && *upd.LabelName != target.LabelName {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".labelName", Before: target.LabelName, After: *upd.LabelName})
				target.LabelName = *upd.LabelName
			}
			if upd.MetricName != nil && *upd.MetricName != target.MetricName {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".metricName", Before: target.MetricName, After: *upd.MetricName})
				target.MetricName = *upd.MetricName
			}
			if upd.Options != nil {
				if !slices.Equal(target.Options, *upd.Options) {
					diffs = append(diffs, DiffEntry{
						Op:     "update_variable",
						Field:  "variables." + name + ".options",
						Before: strings.Join(target.Options, ","),
						After:  strings.Join(*upd.Options, ","),
					})
					cp := make([]string, len(*upd.Options))
					copy(cp, *upd.Options)
					target.Options = cp
				}
			}
			if upd.DefaultValue != nil && *upd.DefaultValue != target.DefaultValue {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".defaultValue", Before: target.DefaultValue, After: *upd.DefaultValue})
				target.DefaultValue = *upd.DefaultValue
			}
			if upd.Multi != nil && *upd.Multi != target.Multi {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".multi", Before: fmt.Sprintf("%t", target.Multi), After: fmt.Sprintf("%t", *upd.Multi)})
				target.Multi = *upd.Multi
			}
			if upd.IncludeAll != nil && *upd.IncludeAll != target.IncludeAll {
				diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".includeAll", Before: fmt.Sprintf("%t", target.IncludeAll), After: fmt.Sprintf("%t", *upd.IncludeAll)})
				target.IncludeAll = *upd.IncludeAll
			}

		case "remove_variable":
			if op.VariableName == "" {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: remove_variable requires variableName", idx)
			}
			pos := findVariableIndex(next.Variables, op.VariableName)
			if pos < 0 {
				return DashboardIR{}, nil, fmt.Errorf("%w: %q", ErrVariableNotFound, op.VariableName)
			}
			removed := next.Variables[pos]
			next.Variables = append(next.Variables[:pos], next.Variables[pos+1:]...)
			diffs = append(diffs, DiffEntry{Op: "remove_variable", Field: "variables." + removed.Name, Before: removed.Type})
		}
	}

	if len(diffs) == 0 {
		return DashboardIR{}, nil, ErrNoOpPatch
	}

	// Upgrade v1.0 draft to v1.1 if patch introduced v1.1 features or used v1.1 patch schema.
	if next.SchemaVersion == SchemaVersionV10 && (patch.SchemaVersion == SchemaVersion || usesV11Features(next)) {
		next.SchemaVersion = SchemaVersion
	}

	next.Revision = current.Revision + 1

	if err := ValidateIR(next, opts); err != nil {
		return DashboardIR{}, nil, fmt.Errorf("post-patch invariant violation: %w", err)
	}
	return next, diffs, nil
}

func usesV11Features(doc DashboardIR) bool {
	if len(doc.Variables) > 0 {
		return true
	}
	for _, p := range doc.Panels {
		if len(p.Queries) > 0 || p.Query.RefID != "" || (p.Type != "timeseries" && p.Type != "stat") {
			return true
		}
	}
	return false
}

func effectiveRefID(refID string, idx int) string {
	if refID != "" {
		return refID
	}
	if idx >= 0 && idx < len(defaultQueryRefIDs) {
		return defaultQueryRefIDs[idx]
	}
	return ""
}

func queriesEqual(a, b []QueryIR) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if effectiveRefID(a[i].RefID, i) != effectiveRefID(b[i].RefID, i) ||
			a[i].Expr != b[i].Expr ||
			a[i].LegendFormat != b[i].LegendFormat {
			return false
		}
	}
	return true
}

func findPanelIndex(panels []PanelIR, id string) int {
	for i := range panels {
		if panels[i].ID == id {
			return i
		}
	}
	return -1
}

func findVariableIndex(vars []VariableIR, name string) int {
	for i := range vars {
		if vars[i].Name == name {
			return i
		}
	}
	return -1
}

func formatThresholds(ts []ThresholdIR) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = fmt.Sprintf("%g:%s", t.Value, t.Color)
	}
	return strings.Join(parts, ",")
}

func formatQueries(qs []QueryIR) string {
	parts := make([]string, len(qs))
	for i, q := range qs {
		ref := effectiveRefID(q.RefID, i)
		if q.LegendFormat != "" {
			parts[i] = fmt.Sprintf("%s:%s[%s]", ref, q.Expr, q.LegendFormat)
		} else {
			parts[i] = fmt.Sprintf("%s:%s", ref, q.Expr)
		}
	}
	return strings.Join(parts, " | ")
}
