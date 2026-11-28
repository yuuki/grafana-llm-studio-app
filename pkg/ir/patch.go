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
	if err := validatePatchHeader(current, patch); err != nil {
		return DashboardIR{}, nil, err
	}

	next := current.Clone()
	var diffs []DiffEntry

	for idx, op := range patch.Operations {
		opDiffs, err := applyPatchOperation(&next, idx, op)
		if err != nil {
			return DashboardIR{}, nil, err
		}
		diffs = append(diffs, opDiffs...)
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

func validatePatchHeader(current DashboardIR, patch DashboardPatch) error {
	if !allowedSchemaVersions[patch.SchemaVersion] {
		return fmt.Errorf("patch schemaVersion %q is not allowed (allowed: 1.0, 1.1)", patch.SchemaVersion)
	}
	if patch.BaseRevision != current.Revision {
		return fmt.Errorf("%w: patch baseRevision=%d does not match current revision=%d", ErrRevisionConflict, patch.BaseRevision, current.Revision)
	}
	if err := validatePlainText("summary", patch.Summary, 1, MaxSummaryLen); err != nil {
		return err
	}
	if len(patch.Operations) == 0 {
		return errors.New("patch must contain at least 1 operation")
	}
	if len(patch.Operations) > 6 {
		return fmt.Errorf("patch operation count %d exceeds limit 6", len(patch.Operations))
	}
	return nil
}

func applyPatchOperation(next *DashboardIR, idx int, op PatchOperation) ([]DiffEntry, error) {
	if !allowedPatchOps[op.Op] {
		return nil, fmt.Errorf("operations[%d]: unsupported op %q", idx, op.Op)
	}
	switch op.Op {
	case "update_meta":
		return applyUpdateMeta(next, op), nil
	case "update_panel":
		return applyUpdatePanel(next, idx, op)
	case "add_panel":
		return applyAddPanel(next, idx, op)
	case "remove_panel":
		return applyRemovePanel(next, idx, op)
	case "add_variable":
		return applyAddVariable(next, idx, op)
	case "update_variable":
		return applyUpdateVariable(next, idx, op)
	case "remove_variable":
		return applyRemoveVariable(next, idx, op)
	default:
		return nil, fmt.Errorf("operations[%d]: unsupported op %q", idx, op.Op)
	}
}

func applyUpdateMeta(next *DashboardIR, op PatchOperation) []DiffEntry {
	var diffs []DiffEntry
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
	return diffs
}

func applyUpdatePanel(next *DashboardIR, idx int, op PatchOperation) ([]DiffEntry, error) {
	if op.PanelID == "" || op.Panel == nil {
		return nil, fmt.Errorf("operations[%d]: update_panel requires panelId and panel", idx)
	}
	if op.Panel.ID != "" && op.Panel.ID != op.PanelID {
		return nil, fmt.Errorf("operations[%d]: cannot rename panel id from %q to %q", idx, op.PanelID, op.Panel.ID)
	}
	if op.Panel.Query != nil && op.Panel.Queries != nil {
		return nil, fmt.Errorf("operations[%d]: update_panel cannot specify both query and queries", idx)
	}
	pos := findPanelIndex(next.Panels, op.PanelID)
	if pos < 0 {
		return nil, fmt.Errorf("%w: %q", ErrPanelNotFound, op.PanelID)
	}
	target := &next.Panels[pos]
	upd := op.Panel

	var diffs []DiffEntry
	diffs = append(diffs, applyPanelScalarUpdates(target, upd)...)
	if upd.Query != nil {
		diffs = append(diffs, applyPanelSingleQueryUpdate(target, upd.Query)...)
	}
	if upd.Queries != nil {
		diffs = append(diffs, applyPanelMultiQueriesUpdate(target, *upd.Queries)...)
	}
	diffs = append(diffs, applyPanelDisplayUpdates(target, upd)...)
	return diffs, nil
}

func applyPanelScalarUpdates(target *PanelIR, upd *PanelUpdate) []DiffEntry {
	var diffs []DiffEntry
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
	return diffs
}

func applyPanelSingleQueryUpdate(target *PanelIR, q *QueryUpdate) []DiffEntry {
	if len(target.Queries) > 0 {
		newQ := buildQueryFromPatch(q, target.Queries)
		if queriesEqual(target.Queries, []QueryIR{newQ}) {
			return nil
		}
		diff := DiffEntry{
			Op:      "update_panel",
			PanelID: target.ID,
			Field:   "query",
			Before:  formatQueries(target.Queries),
			After:   formatQueries([]QueryIR{newQ}),
		}
		target.Queries = nil
		target.Query = newQ
		return []DiffEntry{diff}
	}

	var diffs []DiffEntry
	if q.Expr != target.Query.Expr {
		diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "query.expr", Before: target.Query.Expr, After: q.Expr})
		target.Query.Expr = q.Expr
	}
	if q.Kind != nil && *q.Kind != "" && *q.Kind != target.Query.Kind {
		diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "query.kind", Before: target.Query.Kind, After: *q.Kind})
		target.Query.Kind = *q.Kind
	}
	if q.LegendFormat != nil && *q.LegendFormat != target.Query.LegendFormat {
		diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "query.legendFormat", Before: target.Query.LegendFormat, After: *q.LegendFormat})
		target.Query.LegendFormat = *q.LegendFormat
	}
	if q.RefID != nil && effectiveRefID(*q.RefID, 0) != effectiveRefID(target.Query.RefID, 0) {
		diffs = append(diffs, DiffEntry{
			Op:      "update_panel",
			PanelID: target.ID,
			Field:   "query.refId",
			Before:  effectiveRefID(target.Query.RefID, 0),
			After:   effectiveRefID(*q.RefID, 0),
		})
		target.Query.RefID = *q.RefID
	}
	return diffs
}

func buildQueryFromPatch(q *QueryUpdate, existing []QueryIR) QueryIR {
	newQ := QueryIR{Expr: q.Expr}
	if q.RefID != nil {
		newQ.RefID = *q.RefID
	}
	if q.Kind != nil {
		newQ.Kind = *q.Kind
	} else if len(existing) > 0 {
		newQ.Kind = existing[0].Kind
	}
	if q.LegendFormat != nil {
		newQ.LegendFormat = *q.LegendFormat
	} else if len(existing) == 1 {
		newQ.LegendFormat = existing[0].LegendFormat
	}
	return newQ
}

func applyPanelMultiQueriesUpdate(target *PanelIR, queries []QueryIR) []DiffEntry {
	// Inherit existing panel query kind when omitted in patch queries so edits never downgrade dialect.
	existingKind := ""
	if prev := target.EffectiveQueries(); len(prev) > 0 {
		existingKind = prev[0].Kind
	}
	cp := make([]QueryIR, len(queries))
	copy(cp, queries)
	if existingKind != "" {
		for i := range cp {
			if cp[i].Kind == "" {
				cp[i].Kind = existingKind
			}
		}
	}
	if queriesEqual(target.EffectiveQueries(), cp) {
		return nil
	}
	diff := DiffEntry{
		Op:      "update_panel",
		PanelID: target.ID,
		Field:   "queries",
		Before:  formatQueries(target.EffectiveQueries()),
		After:   formatQueries(cp),
	}
	target.Query = QueryIR{}
	target.Queries = cp
	return []DiffEntry{diff}
}

func applyPanelDisplayUpdates(target *PanelIR, upd *PanelUpdate) []DiffEntry {
	var diffs []DiffEntry
	if upd.Unit != "" && upd.Unit != target.Unit {
		diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "unit", Before: target.Unit, After: upd.Unit})
		target.Unit = upd.Unit
	}
	if upd.Thresholds != nil && !slices.Equal(target.Thresholds, *upd.Thresholds) {
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
	return diffs
}

func applyAddPanel(next *DashboardIR, idx int, op PatchOperation) ([]DiffEntry, error) {
	if op.Panel == nil || (op.Panel.Query == nil && op.Panel.Queries == nil) || op.Panel.Layout == nil {
		return nil, fmt.Errorf("operations[%d]: add_panel requires complete panel definition", idx)
	}
	if op.Panel.Query != nil && op.Panel.Queries != nil {
		return nil, fmt.Errorf("operations[%d]: add_panel cannot specify both query and queries", idx)
	}
	if op.PanelID != "" && op.Panel.ID != "" && op.PanelID != op.Panel.ID {
		return nil, fmt.Errorf("operations[%d]: add_panel panelId %q conflicts with panel.id %q", idx, op.PanelID, op.Panel.ID)
	}
	panelID := op.Panel.ID
	if panelID == "" {
		panelID = op.PanelID
	}
	if findPanelIndex(next.Panels, panelID) >= 0 {
		return nil, fmt.Errorf("operations[%d]: panel %q already exists", idx, panelID)
	}
	newPanel := buildPanelFromPatch(panelID, op.Panel)
	next.Panels = append(next.Panels, newPanel)
	return []DiffEntry{{Op: "add_panel", PanelID: newPanel.ID, Field: "panel", After: newPanel.Title}}, nil
}

func buildPanelFromPatch(panelID string, p *PanelUpdate) PanelIR {
	newPanel := PanelIR{
		ID:            panelID,
		Type:          p.Type,
		Title:         p.Title,
		DatasourceRef: p.DatasourceRef,
		Unit:          p.Unit,
		Layout:        *p.Layout,
	}
	if p.Query != nil {
		newPanel.Query = buildQueryFromPatch(p.Query, nil)
	}
	if p.Queries != nil {
		newPanel.Queries = make([]QueryIR, len(*p.Queries))
		copy(newPanel.Queries, *p.Queries)
	}
	if p.Description != nil {
		newPanel.Description = *p.Description
	}
	if p.Thresholds != nil {
		newPanel.Thresholds = make([]ThresholdIR, len(*p.Thresholds))
		copy(newPanel.Thresholds, *p.Thresholds)
	}
	return newPanel
}

func applyRemovePanel(next *DashboardIR, idx int, op PatchOperation) ([]DiffEntry, error) {
	if op.PanelID == "" {
		return nil, fmt.Errorf("operations[%d]: remove_panel requires panelId", idx)
	}
	pos := findPanelIndex(next.Panels, op.PanelID)
	if pos < 0 {
		return nil, fmt.Errorf("%w: %q", ErrPanelNotFound, op.PanelID)
	}
	removed := next.Panels[pos]
	next.Panels = append(next.Panels[:pos], next.Panels[pos+1:]...)
	return []DiffEntry{{Op: "remove_panel", PanelID: removed.ID, Field: "panel", Before: removed.Title}}, nil
}

func applyAddVariable(next *DashboardIR, idx int, op PatchOperation) ([]DiffEntry, error) {
	if op.Variable == nil {
		return nil, fmt.Errorf("operations[%d]: add_variable requires variable definition", idx)
	}
	if op.VariableName != "" && op.Variable.Name != "" && op.VariableName != op.Variable.Name {
		return nil, fmt.Errorf("operations[%d]: add_variable variableName %q conflicts with variable.name %q", idx, op.VariableName, op.Variable.Name)
	}
	name := op.Variable.Name
	if name == "" {
		name = op.VariableName
	}
	if findVariableIndex(next.Variables, name) >= 0 {
		return nil, fmt.Errorf("operations[%d]: variable %q already exists", idx, name)
	}
	newVar := buildVariableFromPatch(name, op.Variable)
	next.Variables = append(next.Variables, newVar)
	return []DiffEntry{{Op: "add_variable", Field: "variables." + newVar.Name, After: newVar.Type}}, nil
}

func buildVariableFromPatch(name string, v *VariableUpdate) VariableIR {
	newVar := VariableIR{
		Name: name,
		Type: v.Type,
	}
	if v.Label != nil {
		newVar.Label = *v.Label
	}
	if v.DatasourceRef != nil {
		newVar.DatasourceRef = *v.DatasourceRef
	}
	if v.LabelName != nil {
		newVar.LabelName = *v.LabelName
	}
	if v.MetricName != nil {
		newVar.MetricName = *v.MetricName
	}
	if v.Options != nil {
		newVar.Options = make([]string, len(*v.Options))
		copy(newVar.Options, *v.Options)
	}
	if v.DefaultValue != nil {
		newVar.DefaultValue = *v.DefaultValue
	}
	if v.Multi != nil {
		newVar.Multi = *v.Multi
	}
	if v.IncludeAll != nil {
		newVar.IncludeAll = *v.IncludeAll
	}
	return newVar
}

func applyUpdateVariable(next *DashboardIR, idx int, op PatchOperation) ([]DiffEntry, error) {
	name := op.VariableName
	if name == "" && op.Variable != nil {
		name = op.Variable.Name
	}
	if name == "" || op.Variable == nil {
		return nil, fmt.Errorf("operations[%d]: update_variable requires variableName and variable", idx)
	}
	if op.Variable.Name != "" && op.Variable.Name != name {
		return nil, fmt.Errorf("operations[%d]: cannot rename variable from %q to %q", idx, name, op.Variable.Name)
	}
	pos := findVariableIndex(next.Variables, name)
	if pos < 0 {
		return nil, fmt.Errorf("%w: %q", ErrVariableNotFound, name)
	}
	target := &next.Variables[pos]
	upd := op.Variable
	var diffs []DiffEntry

	if upd.Type != "" && upd.Type != target.Type {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: "variables." + name + ".type", Before: target.Type, After: upd.Type})
		target.Type = upd.Type
		// Clear fields exclusive to the previous variable type so schema-compliant type switches validate cleanly.
		switch upd.Type {
		case "custom":
			target.DatasourceRef = ""
			target.LabelName = ""
			target.MetricName = ""
		case "query":
			target.Options = nil
		}
	}
	diffs = append(diffs, applyVariableFieldUpdates(name, target, upd)...)
	return diffs, nil
}

func applyVariableFieldUpdates(name string, target *VariableIR, upd *VariableUpdate) []DiffEntry {
	var diffs []DiffEntry
	prefix := "variables." + name + "."
	if upd.Label != nil && *upd.Label != target.Label {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: prefix + "label", Before: target.Label, After: *upd.Label})
		target.Label = *upd.Label
	}
	if upd.DatasourceRef != nil && *upd.DatasourceRef != target.DatasourceRef {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: prefix + "datasourceRef", Before: target.DatasourceRef, After: *upd.DatasourceRef})
		target.DatasourceRef = *upd.DatasourceRef
	}
	if upd.LabelName != nil && *upd.LabelName != target.LabelName {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: prefix + "labelName", Before: target.LabelName, After: *upd.LabelName})
		target.LabelName = *upd.LabelName
	}
	if upd.MetricName != nil && *upd.MetricName != target.MetricName {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: prefix + "metricName", Before: target.MetricName, After: *upd.MetricName})
		target.MetricName = *upd.MetricName
	}
	if upd.Options != nil && !slices.Equal(target.Options, *upd.Options) {
		diffs = append(diffs, DiffEntry{
			Op:     "update_variable",
			Field:  prefix + "options",
			Before: strings.Join(target.Options, ","),
			After:  strings.Join(*upd.Options, ","),
		})
		cp := make([]string, len(*upd.Options))
		copy(cp, *upd.Options)
		target.Options = cp
	}
	if upd.DefaultValue != nil && *upd.DefaultValue != target.DefaultValue {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: prefix + "defaultValue", Before: target.DefaultValue, After: *upd.DefaultValue})
		target.DefaultValue = *upd.DefaultValue
	}
	if upd.Multi != nil && *upd.Multi != target.Multi {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: prefix + "multi", Before: fmt.Sprintf("%t", target.Multi), After: fmt.Sprintf("%t", *upd.Multi)})
		target.Multi = *upd.Multi
	}
	if upd.IncludeAll != nil && *upd.IncludeAll != target.IncludeAll {
		diffs = append(diffs, DiffEntry{Op: "update_variable", Field: prefix + "includeAll", Before: fmt.Sprintf("%t", target.IncludeAll), After: fmt.Sprintf("%t", *upd.IncludeAll)})
		target.IncludeAll = *upd.IncludeAll
	}
	return diffs
}

func applyRemoveVariable(next *DashboardIR, idx int, op PatchOperation) ([]DiffEntry, error) {
	if op.VariableName == "" {
		return nil, fmt.Errorf("operations[%d]: remove_variable requires variableName", idx)
	}
	pos := findVariableIndex(next.Variables, op.VariableName)
	if pos < 0 {
		return nil, fmt.Errorf("%w: %q", ErrVariableNotFound, op.VariableName)
	}
	removed := next.Variables[pos]
	next.Variables = append(next.Variables[:pos], next.Variables[pos+1:]...)
	return []DiffEntry{{Op: "remove_variable", Field: "variables." + removed.Name, Before: removed.Type}}, nil
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
	for i, qb := range b {
		qa := a[i]
		if effectiveRefID(qa.RefID, i) != effectiveRefID(qb.RefID, i) ||
			qa.Kind != qb.Kind ||
			qa.Expr != qb.Expr ||
			qa.LegendFormat != qb.LegendFormat {
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
		prefix := ref
		if q.Kind != "" {
			prefix = fmt.Sprintf("%s(%s)", ref, q.Kind)
		}
		if q.LegendFormat != "" {
			parts[i] = fmt.Sprintf("%s:%s[%s]", prefix, q.Expr, q.LegendFormat)
		} else {
			parts[i] = fmt.Sprintf("%s:%s", prefix, q.Expr)
		}
	}
	return strings.Join(parts, " | ")
}
