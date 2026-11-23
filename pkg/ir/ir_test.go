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

func TestIRv11_FixturePanelsMultiQueryVariablesAndPatches(t *testing.T) {
	v11Bytes, err := os.ReadFile(filepath.Join("..", "..", "schemas", "samples", "valid-v11-dashboard.json"))
	if err != nil {
		t.Fatalf("read v1.1 fixture: %v", err)
	}
	doc, err := ir.DecodeStrictIR(v11Bytes)
	if err != nil {
		t.Fatalf("DecodeStrictIR(v1.1) failed: %v", err)
	}
	if err := ir.ValidateIR(doc, ir.ValidateOptions{MaxPanels: 6}); err != nil {
		t.Fatalf("ValidateIR(v1.1) failed: %v", err)
	}
	if len(doc.Variables) != 2 || len(doc.Panels) != 6 || len(doc.Panels[0].Queries) != 3 {
		t.Fatalf("unexpected v1.1 fixture structure: vars=%d panels=%d q0=%d", len(doc.Variables), len(doc.Panels), len(doc.Panels[0].Queries))
	}

	t.Run("rejects both query and queries on same panel", func(t *testing.T) {
		raw := []byte(`{"schemaVersion":"1.1","revision":1,"title":"Dup","timeRange":{"from":"now-15m","to":"now"},"panels":[{"id":"panel-a","type":"timeseries","title":"A","datasourceRef":"selected_prometheus","query":{"expr":"up"},"queries":[{"expr":"up"}],"unit":"short","layout":{"row":0,"col":0,"width":"full"}}]}`)
		if _, err := ir.DecodeStrictIR(raw); err == nil {
			t.Fatal("expected DecodeStrictIR to reject panel having both query and queries")
		}
	})

	t.Run("rejects >3 queries or duplicate refIds", func(t *testing.T) {
		bad := doc.Clone()
		bad.Panels[0].Queries = append(bad.Panels[0].Queries, ir.QueryIR{Expr: "up"})
		if err := ir.ValidateIR(bad, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected 4 queries per panel to be rejected")
		}

		dupRef := doc.Clone()
		dupRef.Panels[0].Queries[1].RefID = "A"
		if err := ir.ValidateIR(dupRef, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected duplicate refId to be rejected")
		}
	})

	t.Run("rejects reserved variable names, >4 vars, and invalid custom defaultValue", func(t *testing.T) {
		badVar := doc.Clone()
		badVar.Variables = append(badVar.Variables, ir.VariableIR{
			Name:          "__from",
			Type:          "query",
			DatasourceRef: ir.SelectedDatasourceRef,
			LabelName:     "service",
		})
		if err := ir.ValidateIR(badVar, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected reserved variable name __from to be rejected")
		}

		badDefault := doc.Clone()
		badDefault.Variables[1].DefaultValue = "not-in-options"
		if err := ir.ValidateIR(badDefault, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected custom variable defaultValue outside options to be rejected")
		}

		tooManyVars := doc.Clone()
		for i := 0; i < 4; i++ {
			tooManyVars.Variables = append(tooManyVars.Variables, ir.VariableIR{
				Name:    "extra_" + string(rune('a'+i)),
				Type:    "custom",
				Options: []string{"x"},
			})
		}
		if err := ir.ValidateIR(tooManyVars, ir.ValidateOptions{MaxPanels: 6}); err == nil {
			t.Fatal("expected >4 variables to be rejected")
		}
	})

	t.Run("PanelQueryHash matches QueryHash for single query and tracks multi-query changes", func(t *testing.T) {
		single := doc.Panels[1]
		if got, want := ir.PanelQueryHash(single), ir.QueryHash(single.Query.Expr); got != want {
			t.Fatalf("single-query PanelQueryHash=%q != QueryHash=%q", got, want)
		}
		multi := doc.Panels[0]
		h1 := ir.PanelQueryHash(multi)
		multi2 := multi
		multi2.Queries = append([]ir.QueryIR(nil), multi.Queries...)
		multi2.Queries[2].Expr = `histogram_quantile(0.999, sum by (le) (rate(http_request_duration_seconds_bucket{service="$service"}[5m])))`
		if h2 := ir.PanelQueryHash(multi2); h1 == h2 {
			t.Fatal("expected PanelQueryHash to change when query C changes")
		}
	})

	t.Run("patch supports add/update/remove variable and multi-query update", func(t *testing.T) {
		lbl := "Namespace"
		dsRef := ir.SelectedDatasourceRef
		lblName := "namespace"
		defVal := "prod"
		newQueries := []ir.QueryIR{
			{RefID: "A", Expr: `sum(rate(http_requests_total{service="$service",status=~"5.."}[5m]))`, LegendFormat: "5xx"},
			{RefID: "B", Expr: `sum(rate(http_requests_total{service="$service",status=~"4.."}[5m]))`, LegendFormat: "4xx"},
		}
		patch := ir.DashboardPatch{
			SchemaVersion: ir.SchemaVersion,
			BaseRevision:  doc.Revision,
			Summary:       "Add namespace variable and expand error panel to 2 queries",
			Operations: []ir.PatchOperation{
				{
					Op: "add_variable",
					Variable: &ir.VariableUpdate{
						Name:          "namespace",
						Label:         &lbl,
						Type:          "query",
						DatasourceRef: &dsRef,
						LabelName:     &lblName,
						DefaultValue:  &defVal,
					},
				},
				{
					Op:      "update_panel",
					PanelID: "panel-error-gauge",
					Panel:   &ir.PanelUpdate{Queries: &newQueries},
				},
			},
		}
		next, diffs, err := ir.ApplyPatch(doc, patch, ir.ValidateOptions{MaxPanels: 6})
		if err != nil {
			t.Fatalf("ApplyPatch v1.1 failed: %v", err)
		}
		if len(next.Variables) != 3 || next.Variables[2].Name != "namespace" {
			t.Fatalf("expected 3 variables with namespace added, got %+v", next.Variables)
		}
		if len(next.Panels[1].Queries) != 2 || next.Panels[1].Query != (ir.QueryIR{}) {
			t.Fatalf("expected panel-error-gauge switched to 2 queries, got %+v", next.Panels[1])
		}
		if len(diffs) != 2 {
			t.Fatalf("expected 2 diff entries, got %+v", diffs)
		}

		// Update and then remove the variable.
		newDef := "staging"
		updNext, _, err := ir.ApplyPatch(next, ir.DashboardPatch{
			SchemaVersion: ir.SchemaVersion,
			BaseRevision:  next.Revision,
			Summary:       "Update namespace default and remove status_class",
			Operations: []ir.PatchOperation{
				{Op: "update_variable", VariableName: "namespace", Variable: &ir.VariableUpdate{DefaultValue: &newDef}},
				{Op: "remove_variable", VariableName: "status_class"},
			},
		}, ir.ValidateOptions{MaxPanels: 6})
		if err != nil {
			t.Fatalf("update/remove variable patch failed: %v", err)
		}
		if len(updNext.Variables) != 2 || updNext.Variables[1].DefaultValue != "staging" {
			t.Fatalf("unexpected variables after update/remove: %+v", updNext.Variables)
		}
	})
}
