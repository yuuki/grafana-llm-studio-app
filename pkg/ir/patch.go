package ir

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrRevisionConflict indicates a patch targeted a stale draft revision.
	ErrRevisionConflict = errors.New("revision conflict")
	// ErrPanelNotFound indicates a patch referenced an unknown panelId.
	ErrPanelNotFound = errors.New("panel not found")
	// ErrNoOpPatch indicates a patch produced zero effective changes.
	ErrNoOpPatch = errors.New("patch produced no changes")
)

// ApplyPatch applies typed partial edit operations to current DashboardIR,
// enforcing baseRevision matching, summary validation, no-op rejection,
// preserving untouched panels, and re-validating the entire resulting IR.
func ApplyPatch(current DashboardIR, patch DashboardPatch, opts ValidateOptions) (DashboardIR, []DiffEntry, error) {
	if patch.SchemaVersion != SchemaVersion {
		return DashboardIR{}, nil, fmt.Errorf("patch schemaVersion must be %q, got %q", SchemaVersion, patch.SchemaVersion)
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
				if upd.Query.Expr != target.Query.Expr {
					diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "query.expr", Before: target.Query.Expr, After: upd.Query.Expr})
					target.Query.Expr = upd.Query.Expr
				}
				if upd.Query.LegendFormat != nil && *upd.Query.LegendFormat != target.Query.LegendFormat {
					diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "query.legendFormat", Before: target.Query.LegendFormat, After: *upd.Query.LegendFormat})
					target.Query.LegendFormat = *upd.Query.LegendFormat
				}
			}
			if upd.Unit != "" && upd.Unit != target.Unit {
				diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "unit", Before: target.Unit, After: upd.Unit})
				target.Unit = upd.Unit
			}
			if upd.Thresholds != nil {
				before := formatThresholds(target.Thresholds)
				after := formatThresholds(*upd.Thresholds)
				if before != after {
					diffs = append(diffs, DiffEntry{Op: "update_panel", PanelID: target.ID, Field: "thresholds", Before: before, After: after})
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
			if op.Panel == nil || op.Panel.Query == nil || op.Panel.Layout == nil {
				return DashboardIR{}, nil, fmt.Errorf("operations[%d]: add_panel requires complete panel definition", idx)
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
			newQuery := QueryIR{Expr: op.Panel.Query.Expr}
			if op.Panel.Query.LegendFormat != nil {
				newQuery.LegendFormat = *op.Panel.Query.LegendFormat
			}
			newPanel := PanelIR{
				ID:            panelID,
				Type:          op.Panel.Type,
				Title:         op.Panel.Title,
				DatasourceRef: op.Panel.DatasourceRef,
				Query:         newQuery,
				Unit:          op.Panel.Unit,
				Layout:        *op.Panel.Layout,
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

		default:
			return DashboardIR{}, nil, fmt.Errorf("operations[%d]: unsupported op %q", idx, op.Op)
		}
	}

	if len(diffs) == 0 {
		return DashboardIR{}, nil, ErrNoOpPatch
	}

	next.Revision = current.Revision + 1

	if err := ValidateIR(next, opts); err != nil {
		return DashboardIR{}, nil, fmt.Errorf("post-patch invariant violation: %w", err)
	}
	return next, diffs, nil
}

func findPanelIndex(panels []PanelIR, id string) int {
	for i := range panels {
		if panels[i].ID == id {
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
