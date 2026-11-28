package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

type stubDialectLLM struct {
	rawGenerate []byte
	rawRepair   []byte
}

func (s *stubDialectLLM) Enabled(_ context.Context) (bool, error) { return true, nil }
func (s *stubDialectLLM) GenerateIR(_ context.Context, _ llm.GenerateRequest) ([]byte, llm.CompletionMeta, error) {
	return s.rawGenerate, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
}
func (s *stubDialectLLM) GeneratePatch(_ context.Context, _ llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
	return nil, llm.CompletionMeta{ProviderMode: "stub"}, nil
}
func (s *stubDialectLLM) RepairPanels(_ context.Context, _ llm.RepairRequest) ([]byte, llm.CompletionMeta, error) {
	return s.rawRepair, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
}
func (s *stubDialectLLM) AnalyzeStatus(_ context.Context, _ llm.AnalyzeStatusRequest) ([]byte, llm.CompletionMeta, error) {
	return nil, llm.CompletionMeta{ProviderMode: "stub"}, nil
}

func TestManager_MetricsQLGeneratePatchImportAndAdminPinning(t *testing.T) {
	owner := Owner{OrgID: 1, UserLogin: "alice"}
	catalog := []ir.MetricCatalogItem{
		{Name: "http_requests_total", Type: "counter", Labels: []string{"service", "status"}},
		{Name: "http_request_duration_seconds_bucket", Type: "histogram", Labels: []string{"service", "le"}},
	}
	scope := &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"}

	mgr := NewManager(&llm.MockClient{}, Config{
		AllowedDSUIDs: []string{"prom-main", "vm-prom"},
		DatasourceDialects: map[string]string{
			"vm-prom":   "metricsql",
			"prom-main": "promql",
		},
	})

	// 1. Admin-pinned vm-prom uses MetricsQL even if client omits or sends "promql".
	snap, err := mgr.Generate(context.Background(), owner, GenerateInput{
		DatasourceUID: "vm-prom",
		QueryKind:     "promql",
		Scope:         scope,
		UserIntent:    "Show request rate and 5xx error ratio",
		Catalog:       catalog,
	})
	if err != nil {
		t.Fatalf("MetricsQL generate failed: %v", err)
	}
	if snap.Status != StatusAwaitingPreview {
		t.Fatalf("expected awaiting_preview, got %s (%s)", snap.Status, snap.ErrorMessage)
	}
	if snap.QueryKind != "metricsql" {
		t.Fatalf("expected admin-pinned queryKind=metricsql, got %q", snap.QueryKind)
	}
	if snap.CurrentIR == nil || len(snap.CurrentIR.Panels) == 0 {
		t.Fatalf("expected generated panels")
	}
	for _, p := range snap.CurrentIR.Panels {
		for _, q := range p.EffectiveQueries() {
			if q.Kind != "metricsql" {
				t.Fatalf("expected stamped query.kind=metricsql on panel %s, got %q", p.ID, q.Kind)
			}
		}
	}
	if !strings.Contains(snap.CurrentIR.Panels[0].Query.Expr, "default 0") {
		t.Fatalf("expected mock MetricsQL expression using default operator, got: %s", snap.CurrentIR.Panels[0].Query.Expr)
	}

	// 2. Patch preserves MetricsQL dialect across revisions.
	patched, err := mgr.Patch(context.Background(), owner, snap.JobID, PatchInput{
		BaseRevision: snap.CurrentIR.Revision,
		EditIntent:   "Change error panel to 4xx and 5xx",
		Catalog:      catalog,
	})
	if err != nil {
		t.Fatalf("MetricsQL patch failed: %v", err)
	}
	if patched.QueryKind != "metricsql" {
		t.Fatalf("expected patched queryKind=metricsql, got %q", patched.QueryKind)
	}

	// 3. Admin-pinned prom-main ignores client "metricsql" hint and rejects MetricsQL-only syntax on Import.
	vmImportDraft := ir.DashboardIR{
		SchemaVersion: ir.SchemaVersion,
		Revision:      1,
		Title:         "Imported VM Dashboard",
		TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
		Panels: []ir.PanelIR{
			{
				ID:            "panel-vm",
				Type:          "timeseries",
				Title:         "VM Rate",
				DatasourceRef: ir.SelectedDatasourceRef,
				Query: ir.QueryIR{
					Expr: `sum(increase_pure(http_requests_total{service="checkout-api"}[5m])) default 0`,
				},
				Unit:   "reqps",
				Layout: ir.LayoutHint{Row: 0, Col: 0, Width: "full"},
			},
		},
	}
	if _, err := mgr.Import(owner, ImportInput{
		DatasourceUID: "prom-main",
		QueryKind:     "metricsql", // must be overridden by admin pin "promql"
		Scope:         scope,
		Draft:         vmImportDraft,
		Catalog:       catalog,
	}); err == nil {
		t.Fatalf("expected MetricsQL syntax import on admin-pinned promql datasource to fail")
	}

	// Same import succeeds on vm-prom (admin-pinned metricsql).
	imported, err := mgr.Import(owner, ImportInput{
		DatasourceUID: "vm-prom",
		Scope:         scope,
		Draft:         vmImportDraft,
		Catalog:       catalog,
	})
	if err != nil {
		t.Fatalf("expected MetricsQL import on vm-prom to succeed, got: %v", err)
	}
	if imported.QueryKind != "metricsql" || imported.CurrentIR.Panels[0].Query.Kind != "metricsql" {
		t.Fatalf("expected imported MetricsQL job and stamped query kind, got job=%q query=%q", imported.QueryKind, imported.CurrentIR.Panels[0].Query.Kind)
	}

	// 4. WITH bomb is rejected on Import immediately before Parse.
	withBombDraft := vmImportDraft.Clone()
	withBombDraft.Panels[0].Query.Expr = `WITH (a=http_requests_total{service="checkout-api"}, b=a+a) b`
	if _, err := mgr.Import(owner, ImportInput{
		DatasourceUID: "vm-prom",
		Scope:         scope,
		Draft:         withBombDraft,
		Catalog:       catalog,
	}); err == nil || !strings.Contains(err.Error(), query.ErrPolicyViolation.Error()) {
		t.Fatalf("expected policy violation for WITH template on Import, got: %v", err)
	}
}

func TestManager_RejectsDialectSplitBrainInGenerateAndRepair(t *testing.T) {
	owner := Owner{OrgID: 1, UserLogin: "bob"}
	catalog := []ir.MetricCatalogItem{
		{Name: "http_requests_total", Type: "counter", Labels: []string{"service", "status"}},
	}
	scope := &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"}

	// LLM outputs query.kind="metricsql" during a PromQL job -> MUST fail validation.
	mismatchedDoc := ir.DashboardIR{
		SchemaVersion: ir.SchemaVersion,
		Revision:      1,
		Title:         "Spoofed Kind",
		TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
		Panels: []ir.PanelIR{
			{
				ID:            "panel-req",
				Type:          "timeseries",
				Title:         "Req",
				DatasourceRef: ir.SelectedDatasourceRef,
				Query: ir.QueryIR{
					Kind: "metricsql",
					Expr: `sum(rate(http_requests_total{service="checkout-api"}[5m]))`,
				},
				Unit:   "reqps",
				Layout: ir.LayoutHint{Row: 0, Col: 0, Width: "full"},
			},
		},
	}
	rawMismatch, _ := json.Marshal(mismatchedDoc)

	// Repair patch tries to switch query.kind from metricsql to promql -> MUST fail repair guard.
	badKind := "promql"
	badRepairPatch := ir.DashboardPatch{
		SchemaVersion: ir.SchemaVersion,
		BaseRevision:  1,
		Summary:       "Attempt dialect switch",
		Operations: []ir.PatchOperation{
			{
				Op:      "update_panel",
				PanelID: "panel-req",
				Panel: &ir.PanelUpdate{
					Query: &ir.QueryUpdate{
						Kind: &badKind,
						Expr: `sum(rate(http_requests_total{service="checkout-api"}[5m]))`,
					},
				},
			},
		},
	}
	rawBadRepair, _ := json.Marshal(badRepairPatch)

	stub := &stubDialectLLM{rawGenerate: rawMismatch, rawRepair: rawBadRepair}
	mgr := NewManager(stub, Config{
		AllowedDSUIDs: []string{"prom-main", "vm-main"},
		DatasourceDialects: map[string]string{
			"prom-main": "promql",
			"vm-main":   "metricsql",
		},
	})

	snap, err := mgr.Generate(context.Background(), owner, GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         scope,
		UserIntent:    "Show requests",
		Catalog:       catalog,
	})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if snap.Status != StatusFailed || !strings.Contains(snap.ErrorMessage, "does not match active dialect") {
		t.Fatalf("expected StatusFailed with dialect mismatch, got status=%s msg=%q", snap.Status, snap.ErrorMessage)
	}

	// Import a valid MetricsQL draft on vm-main, then attempt repair that tries to flip kind to promql.
	validVMDraft := mismatchedDoc.Clone()
	imported, err := mgr.Import(owner, ImportInput{
		DatasourceUID: "vm-main",
		Scope:         scope,
		Draft:         validVMDraft,
		Catalog:       catalog,
	})
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	_, repairErr := mgr.Repair(context.Background(), owner, imported.JobID, RepairInput{
		BaseRevision: imported.CurrentIR.Revision,
		Catalog:      catalog,
		Results: []ir.PanelValidationResult{
			{
				PanelID:   "panel-req",
				QueryHash: ir.PanelQueryHash(imported.CurrentIR.Panels[0]),
				Category:  ir.ValidationSyntaxError,
				ErrorText: "cannot parse query",
			},
		},
	})
	if repairErr == nil || !strings.Contains(repairErr.Error(), "クエリ方言変更") {
		t.Fatalf("expected repair guard to reject query.kind mutation, got: %v", repairErr)
	}
}
