package ir_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
)

func TestValidAndInvalidSampleFixtures(t *testing.T) {
	validBytes, err := os.ReadFile(filepath.Join("..", "..", "schemas", "samples", "valid-red-dashboard.json"))
	if err != nil {
		t.Fatalf("read valid fixture: %v", err)
	}
	doc, err := ir.DecodeStrictIR(validBytes)
	if err != nil {
		t.Fatalf("DecodeStrictIR(valid) failed: %v", err)
	}
	if err := ir.ValidateIR(doc, ir.ValidateOptions{MaxPanels: 6}); err != nil {
		t.Fatalf("ValidateIR(valid) failed: %v", err)
	}

	invalidBytes, err := os.ReadFile(filepath.Join("..", "..", "schemas", "samples", "invalid-dashboard.json"))
	if err != nil {
		t.Fatalf("read invalid fixture: %v", err)
	}
	if _, err := ir.DecodeStrictIR(invalidBytes); err == nil {
		t.Fatalf("expected DecodeStrictIR to reject unknownField in invalid fixture")
	}
}

func TestValidateIR_RejectsSecurityAndBoundsViolations(t *testing.T) {
	base := ir.DashboardIR{
		SchemaVersion: ir.SchemaVersion,
		Revision:      1,
		Title:         "Valid Title",
		TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
		Panels: []ir.PanelIR{
			{
				ID:            "panel-req",
				Type:          "timeseries",
				Title:         "Requests",
				DatasourceRef: ir.SelectedDatasourceRef,
				Query:         ir.QueryIR{Expr: "rate(http_requests_total[5m])"},
				Unit:          "reqps",
				Layout:        ir.LayoutHint{Row: 0, Col: 0, Width: "full"},
			},
		},
	}

	t.Run("oversized payload rejected", func(t *testing.T) {
		huge := []byte(`{"title":"` + strings.Repeat("a", ir.MaxIRPayloadBytes+10) + `"}`)
		if _, err := ir.DecodeStrictIR(huge); err == nil {
			t.Fatal("expected oversized IR payload to fail")
		}
	})

	t.Run("unauthorized datasourceRef rejected", func(t *testing.T) {
		bad := base.Clone()
		bad.Panels[0].DatasourceRef = "prod-prom-secret-uid"
		if err := ir.ValidateIR(bad, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected arbitrary datasource UID to be rejected")
		}
	})

	t.Run("active HTML/JS content rejected", func(t *testing.T) {
		bad := base.Clone()
		bad.Title = "Dashboard <script>alert(1)</script>"
		if err := ir.ValidateIR(bad, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected script tag in title to be rejected")
		}
	})

	t.Run("disallowed panel type rejected", func(t *testing.T) {
		bad := base.Clone()
		bad.Panels[0].Type = "text"
		if err := ir.ValidateIR(bad, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected text panel type to be rejected in MVP")
		}
	})

	t.Run("overlapping layout rejected", func(t *testing.T) {
		bad := base.Clone()
		bad.Panels = append(bad.Panels, ir.PanelIR{
			ID:            "panel-overlap",
			Type:          "stat",
			Title:         "Overlap",
			DatasourceRef: ir.SelectedDatasourceRef,
			Query:         ir.QueryIR{Expr: "up"},
			Unit:          "short",
			Layout:        ir.LayoutHint{Row: 0, Col: 0, Width: "half"},
		})
		if err := ir.ValidateIR(bad, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected overlapping panel grid cells to be rejected")
		}
	})
}

func TestApplyPatch_RevisionConflictAndPartialEdit(t *testing.T) {
	current := ir.DashboardIR{
		SchemaVersion: ir.SchemaVersion,
		Revision:      2,
		Title:         "checkout-api",
		TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
		Panels: []ir.PanelIR{
			{
				ID:            "panel-req",
				Type:          "timeseries",
				Title:         "Request Rate",
				DatasourceRef: ir.SelectedDatasourceRef,
				Query:         ir.QueryIR{Expr: `sum(rate(http_requests_total{service="checkout-api"}[5m]))`},
				Unit:          "reqps",
				Layout:        ir.LayoutHint{Row: 0, Col: 0, Width: "half"},
			},
			{
				ID:            "panel-err",
				Type:          "stat",
				Title:         "Error Ratio",
				DatasourceRef: ir.SelectedDatasourceRef,
				Query:         ir.QueryIR{Expr: `sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m]))`},
				Unit:          "percentunit",
				Layout:        ir.LayoutHint{Row: 0, Col: 1, Width: "half"},
			},
		},
	}

	// Stale baseRevision must fail with ErrRevisionConflict.
	stalePatch := ir.DashboardPatch{
		SchemaVersion: ir.SchemaVersion,
		BaseRevision:  1,
		Summary:       "stale",
		Operations: []ir.PatchOperation{
			{Op: "update_panel", PanelID: "panel-err", Panel: &ir.PanelUpdate{Type: "timeseries"}},
		},
	}
	if _, _, err := ir.ApplyPatch(current, stalePatch, ir.ValidateOptions{MaxPanels: 6}); !errors.Is(err, ir.ErrRevisionConflict) {
		t.Fatalf("expected ErrRevisionConflict, got %v", err)
	}

	// Unknown panelId must fail with ErrPanelNotFound.
	badPanelPatch := ir.DashboardPatch{
		SchemaVersion: ir.SchemaVersion,
		BaseRevision:  2,
		Summary:       "bad panel",
		Operations: []ir.PatchOperation{
			{Op: "update_panel", PanelID: "panel-missing", Panel: &ir.PanelUpdate{Type: "timeseries"}},
		},
	}
	if _, _, err := ir.ApplyPatch(current, badPanelPatch, ir.ValidateOptions{MaxPanels: 6}); !errors.Is(err, ir.ErrPanelNotFound) {
		t.Fatalf("expected ErrPanelNotFound, got %v", err)
	}

	// Valid partial edit modifies only panel-err and preserves panel-req verbatim.
	validPatch := ir.DashboardPatch{
		SchemaVersion: ir.SchemaVersion,
		BaseRevision:  2,
		Summary:       "Switch error panel to timeseries",
		Operations: []ir.PatchOperation{
			{Op: "update_panel", PanelID: "panel-err", Panel: &ir.PanelUpdate{Type: "timeseries", Title: "5xx Error Trend"}},
		},
	}
	next, diffs, err := ir.ApplyPatch(current, validPatch, ir.ValidateOptions{MaxPanels: 6})
	if err != nil {
		t.Fatalf("ApplyPatch failed: %v", err)
	}
	if next.Revision != 3 {
		t.Fatalf("expected revision 3, got %d", next.Revision)
	}
	if next.Panels[0].Query.Expr != current.Panels[0].Query.Expr || next.Panels[0].Title != current.Panels[0].Title {
		t.Fatalf("untouched panel-req was unexpectedly modified")
	}
	if next.Panels[1].Type != "timeseries" || next.Panels[1].Title != "5xx Error Trend" {
		t.Fatalf("panel-err was not updated properly: %+v", next.Panels[1])
	}
	if len(diffs) != 2 {
		t.Fatalf("expected 2 diff entries, got %d: %+v", len(diffs), diffs)
	}
}
