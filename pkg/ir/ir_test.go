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

func TestValidateIR_SecurityBoundsAndUTF8(t *testing.T) {
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

	t.Run("missing required numeric layout.row rejected by DecodeStrictIR", func(t *testing.T) {
		missingRow := []byte(`{"schemaVersion":"1.0","revision":1,"title":"T","timeRange":{"from":"now-15m","to":"now"},"panels":[{"id":"panel-a","type":"timeseries","title":"A","datasourceRef":"selected_prometheus","query":{"expr":"up"},"unit":"short","layout":{"col":0,"width":"full"}}]}`)
		if _, err := ir.DecodeStrictIR(missingRow); err == nil {
			t.Fatal("expected missing layout.row to be rejected")
		}
	})

	t.Run("Japanese 60-rune panel title passes rune limit (H-7)", func(t *testing.T) {
		ja := base.Clone()
		ja.Panels[0].Title = strings.Repeat("あ", 60) // 180 UTF-8 bytes, 60 runes <= 80
		if err := ir.ValidateIR(ja, ir.ValidateOptions{MaxPanels: 6}); err != nil {
			t.Fatalf("expected 60-rune Japanese title to pass, got %v", err)
		}
	})

	t.Run("unauthorized datasourceRef rejected", func(t *testing.T) {
		bad := base.Clone()
		bad.Panels[0].DatasourceRef = "prod-prom-secret-uid"
		if err := ir.ValidateIR(bad, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected arbitrary datasource UID to be rejected")
		}
	})

	t.Run("HTML tag, event handler, and Bidi override rejected (L-2)", func(t *testing.T) {
		for _, badTitle := range []string{
			"Dashboard <script>alert(1)</script>",
			"Dashboard <svg/onload=alert(1)>",
			"Dashboard onmouseover = alert(1)",
			"Safe\u202ETitle",
		} {
			bad := base.Clone()
			bad.Title = badTitle
			if err := ir.ValidateIR(bad, ir.ValidateOptions{MaxPanels: 6}); err == nil {
				t.Fatalf("expected unsafe title %q to be rejected", badTitle)
			}
		}
	})

	t.Run("PromQL comparison < and > still allowed in expr", func(t *testing.T) {
		ok := base.Clone()
		ok.Panels[0].Query.Expr = `rate(http_requests_total[5m]) > 0`
		if err := ir.ValidateIR(ok, ir.ValidateOptions{MaxPanels: 6}); err != nil {
			t.Fatalf("expected PromQL > operator to pass, got %v", err)
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

func TestApplyPatch_RevisionConflictPreservesLegendAndRejectsNoOp(t *testing.T) {
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
				Query:         ir.QueryIR{Expr: `sum(rate(http_requests_total{service="checkout-api"}[5m]))`, LegendFormat: "{{status}}"},
				Unit:          "reqps",
				Layout:        ir.LayoutHint{Row: 0, Col: 0, Width: "half"},
			},
			{
				ID:            "panel-err",
				Type:          "stat",
				Title:         "Error Ratio",
				DatasourceRef: ir.SelectedDatasourceRef,
				Query:         ir.QueryIR{Expr: `sum(rate(http_requests_total{service="checkout-api",status=~"5.."}[5m]))`, LegendFormat: "5xx"},
				Unit:          "percentunit",
				Layout:        ir.LayoutHint{Row: 0, Col: 1, Width: "half"},
			},
		},
	}

	// Stale baseRevision fails with ErrRevisionConflict.
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

	// No-op patch fails with ErrNoOpPatch (M-3).
	noopPatch := ir.DashboardPatch{
		SchemaVersion: ir.SchemaVersion,
		BaseRevision:  2,
		Summary:       "no changes",
		Operations:    []ir.PatchOperation{{Op: "update_panel", PanelID: "panel-err", Panel: &ir.PanelUpdate{}}},
	}
	if _, _, err := ir.ApplyPatch(current, noopPatch, ir.ValidateOptions{MaxPanels: 6}); !errors.Is(err, ir.ErrNoOpPatch) {
		t.Fatalf("expected ErrNoOpPatch, got %v", err)
	}

	// Updating query.expr without legendFormat preserves existing legendFormat (M-3).
	queryOnlyPatch := ir.DashboardPatch{
		SchemaVersion: ir.SchemaVersion,
		BaseRevision:  2,
		Summary:       "Update expr only",
		Operations: []ir.PatchOperation{
			{
				Op:      "update_panel",
				PanelID: "panel-err",
				Panel:   &ir.PanelUpdate{Query: &ir.QueryUpdate{Expr: `sum(rate(http_requests_total{service="checkout-api",status=~"4..|5.."}[5m]))`}},
			},
		},
	}
	next, diffs, err := ir.ApplyPatch(current, queryOnlyPatch, ir.ValidateOptions{MaxPanels: 6})
	if err != nil {
		t.Fatalf("ApplyPatch failed: %v", err)
	}
	if next.Panels[1].Query.LegendFormat != "5xx" {
		t.Fatalf("omitted legendFormat unexpectedly wiped existing value: got %q", next.Panels[1].Query.LegendFormat)
	}
	if next.Panels[0].Query.Expr != current.Panels[0].Query.Expr {
		t.Fatalf("untouched panel-req was modified")
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff entry, got %+v", diffs)
	}
}
