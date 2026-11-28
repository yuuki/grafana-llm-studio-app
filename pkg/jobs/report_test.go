package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

type errReportClient struct {
	llm.MockClient
	err error
}

func (c *errReportClient) AnalyzeStatus(_ context.Context, _ llm.AnalyzeStatusRequest) ([]byte, llm.CompletionMeta, error) {
	return nil, llm.CompletionMeta{ProviderMode: "stub"}, c.err
}

func sampleAnalyzePayload() ir.AnalyzeStatusPayload {
	return ir.AnalyzeStatusPayload{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		Window:        "1h",
		BaselineMode:  "preceding",
		Language:      "en",
		CollectionStatus: ir.ReportCollectionStatus{
			AlertsChecked:       true,
			RulesChecked:        true,
			TotalRulesEvaluated: 10,
			ActiveAlertsTotal:   1,
		},
		Evidence: ir.ReportEvidenceBundle{
			CollectedAt: "2026-11-27T13:10:00Z",
			Alerts: []ir.AlertEvidenceItem{
				{
					ID:           "ev-alert-1",
					AlertName:    "HighErrorRate",
					State:        "active",
					Severity:     "critical",
					StartsAt:     "2026-11-27T13:05:00Z",
					Summary:      "5xx rate > 5% on checkout-api",
					MatchesScope: true,
				},
			},
			Rules:   []ir.RuleEvidenceItem{},
			Metrics: []ir.MetricEvidenceItem{},
		},
	}
}

func runReportStoreSuite(t *testing.T, makeStore func(t *testing.T) jobs.JobStore) {
	t.Helper()

	alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
	bob := jobs.Owner{OrgID: 1, UserLogin: "bob"}

	t.Run("lifecycle_isolation_cancel_noop_and_wrong_kind", func(t *testing.T) {
		store := makeStore(t)
		defer func() { _ = store.Close() }()

		mgr := jobs.NewManager(&llm.MockClient{}, jobs.Config{
			AllowedDSUIDs: []string{"prom-main"},
			Store:         store,
			JobTTL:        10 * time.Minute,
		})
		defer mgr.Close()

		snap, err := mgr.AnalyzeStatus(context.Background(), alice, sampleAnalyzePayload())
		if err != nil {
			t.Fatalf("AnalyzeStatus failed: %v", err)
		}
		if snap.JobKind != jobs.JobKindReport {
			t.Fatalf("expected jobKind=report, got %q", snap.JobKind)
		}
		if snap.Status != jobs.StatusReady {
			t.Fatalf("expected status=ready, got %q", snap.Status)
		}
		if snap.QueryKind != "" {
			t.Fatalf("expected empty queryKind for report job, got %q", snap.QueryKind)
		}
		if snap.StatusReport == nil || snap.StatusReport.OverallStatus != "critical" {
			t.Fatalf("expected populated critical StatusReport, got %#v", snap.StatusReport)
		}

		// Owner isolation on GET
		if _, err := mgr.GetContext(context.Background(), bob, snap.JobID); !errors.Is(err, jobs.ErrForbidden) {
			t.Fatalf("expected ErrForbidden for cross-user GET, got %v", err)
		}
		got, err := mgr.GetContext(context.Background(), alice, snap.JobID)
		if err != nil {
			t.Fatalf("owner GET failed: %v", err)
		}
		if got.StatusReport == nil || got.StatusReport.ExecutiveSummary != snap.StatusReport.ExecutiveSummary {
			t.Fatalf("expected persisted report on GET, got %#v", got.StatusReport)
		}

		// Late Cancel on completed report MUST be a no-op (must NOT clobber status=ready -> cancelled)
		afterCancel, err := mgr.CancelContext(context.Background(), alice, snap.JobID)
		if err != nil {
			t.Fatalf("CancelContext failed: %v", err)
		}
		if afterCancel.Status != jobs.StatusReady {
			t.Fatalf("late Cancel clobbered completed report status to %q", afterCancel.Status)
		}

		// Patch, Repair, Undo MUST return ErrWrongJobKind
		miniCatalog := []ir.MetricCatalogItem{{Name: "http_requests_total", Type: "counter", Labels: []string{"service"}}}
		if _, err := mgr.Patch(context.Background(), alice, snap.JobID, jobs.PatchInput{
			BaseRevision: 1,
			EditIntent:   "add panel",
			Catalog:      miniCatalog,
		}); !errors.Is(err, jobs.ErrWrongJobKind) {
			t.Fatalf("expected ErrWrongJobKind on Patch, got %v", err)
		}
		if _, err := mgr.Repair(context.Background(), alice, snap.JobID, jobs.RepairInput{
			BaseRevision: 1,
			Results:      []ir.PanelValidationResult{{PanelID: "p-1", Category: "syntax_error", ErrorText: "bad"}},
			Catalog:      miniCatalog,
		}); !errors.Is(err, jobs.ErrWrongJobKind) {
			t.Fatalf("expected ErrWrongJobKind on Repair, got %v", err)
		}
		if _, err := mgr.UndoContext(context.Background(), alice, snap.JobID); !errors.Is(err, jobs.ErrWrongJobKind) {
			t.Fatalf("expected ErrWrongJobKind on Undo, got %v", err)
		}
	})

	t.Run("fail_closed_hallucinated_evidence_id_and_output_redaction", func(t *testing.T) {
		store := makeStore(t)
		defer func() { _ = store.Close() }()

		hallucinatedReport := ir.StatusReportIR{
			SchemaVersion:    "1.0",
			OverallStatus:    "critical",
			Confidence:       "high",
			ExecutiveSummary: "Alert active.",
			Timeline:         []ir.ReportTimelineItem{},
			Findings: []ir.ReportFinding{{
				ID:          "f-1",
				Category:    "alert",
				Severity:    "critical",
				Title:       "Fabricated",
				Detail:      "Cites unknown evidence.",
				EvidenceIDs: []string{"ev-alert-999"},
			}},
			Hypotheses:         []ir.ReportHypothesis{},
			RecommendedActions: []ir.ReportAction{},
			DataGaps:           []string{},
		}
		hallucinatedBytes, _ := json.Marshal(hallucinatedReport)

		mgr := jobs.NewManager(&stubLLM{
			onAnalyze: func(_ context.Context, _ llm.AnalyzeStatusRequest) ([]byte, llm.CompletionMeta, error) {
				return hallucinatedBytes, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
			},
		}, jobs.Config{
			AllowedDSUIDs: []string{"prom-main"},
			Store:         store,
		})
		defer mgr.Close()

		snap, err := mgr.AnalyzeStatus(context.Background(), alice, sampleAnalyzePayload())
		if !errors.Is(err, ir.ErrInvalidReportIR) {
			t.Fatalf("expected ErrInvalidReportIR for hallucinated evidenceId, got %v", err)
		}
		if snap.Status != jobs.StatusFailed || snap.StatusReport != nil {
			t.Fatalf("expected failed snapshot with nil report, got status=%q report=%#v", snap.Status, snap.StatusReport)
		}

		// Echoed secret in valid report must be redacted before storage
		secretReport := ir.StatusReportIR{
			SchemaVersion:    "1.0",
			OverallStatus:    "critical",
			Confidence:       "high",
			ExecutiveSummary: "Failure involving glsa_1234567890abcdef1234",
			Timeline:         []ir.ReportTimelineItem{},
			Findings: []ir.ReportFinding{{
				ID:          "f-1",
				Category:    "alert",
				Severity:    "critical",
				Title:       "Alert",
				Detail:      "Token glsa_1234567890abcdef1234 appeared.",
				EvidenceIDs: []string{"ev-alert-1"},
			}},
			Hypotheses:         []ir.ReportHypothesis{},
			RecommendedActions: []ir.ReportAction{},
			DataGaps:           []string{},
		}
		secretBytes, _ := json.Marshal(secretReport)
		mgr2 := jobs.NewManager(&stubLLM{
			onAnalyze: func(_ context.Context, _ llm.AnalyzeStatusRequest) ([]byte, llm.CompletionMeta, error) {
				return secretBytes, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
			},
		}, jobs.Config{
			AllowedDSUIDs: []string{"prom-main"},
			Store:         store,
		})
		defer mgr2.Close()

		redactedSnap, err := mgr2.AnalyzeStatus(context.Background(), alice, sampleAnalyzePayload())
		if err != nil {
			t.Fatalf("AnalyzeStatus failed: %v", err)
		}
		if strings.Contains(redactedSnap.StatusReport.ExecutiveSummary, "glsa_123456") ||
			strings.Contains(redactedSnap.StatusReport.Findings[0].Detail, "glsa_123456") {
			t.Fatalf("expected echoed secret to be redacted before storage, got %#v", redactedSnap.StatusReport)
		}
	})

	t.Run("datasource_allowlist_and_alerts_only", func(t *testing.T) {
		store := makeStore(t)
		defer func() { _ = store.Close() }()

		mgr := jobs.NewManager(&llm.MockClient{}, jobs.Config{
			AllowedDSUIDs: []string{"prom-main"},
			Store:         store,
		})
		defer mgr.Close()

		// Alerts-only with empty datasourceUid succeeds
		alertsOnly := sampleAnalyzePayload()
		alertsOnly.DatasourceUID = ""
		snap, err := mgr.AnalyzeStatus(context.Background(), alice, alertsOnly)
		if err != nil {
			t.Fatalf("expected alerts-only with empty datasourceUid to succeed, got %v", err)
		}
		if snap.Status != jobs.StatusReady {
			t.Fatalf("expected ready, got %q", snap.Status)
		}

		// Metrics present with empty datasourceUid MUST be denied (allowlist bypass prevention)
		withMetricsNoDS := sampleAnalyzePayload()
		withMetricsNoDS.DatasourceUID = ""
		withMetricsNoDS.Evidence.Metrics = []ir.MetricEvidenceItem{{
			ID:                "ev-metric-1",
			PanelTitle:        "Errors",
			QueryStatus:       "ok",
			Trend:             "rising",
			ThresholdBreached: "critical",
			AnomalyHint:       "degraded",
		}}
		if _, err := mgr.AnalyzeStatus(context.Background(), alice, withMetricsNoDS); !errors.Is(err, jobs.ErrDatasourceDenied) {
			t.Fatalf("expected ErrDatasourceDenied when metrics present without datasourceUid, got %v", err)
		}

		// Non-allowlisted datasourceUid denied even in alerts-only mode
		badDS := sampleAnalyzePayload()
		badDS.DatasourceUID = "untrusted-prom"
		if _, err := mgr.AnalyzeStatus(context.Background(), alice, badDS); !errors.Is(err, jobs.ErrDatasourceDenied) {
			t.Fatalf("expected ErrDatasourceDenied for untrusted datasourceUid, got %v", err)
		}
	})

	t.Run("upstream_llm_error_classified_502_and_redacted", func(t *testing.T) {
		store := makeStore(t)
		defer func() { _ = store.Close() }()

		failingClient := &errReportClient{
			err: errors.New("upstream https://llm.internal/v1 failed with glsa_1234567890abcdef1234"),
		}
		mgr := jobs.NewManager(failingClient, jobs.Config{
			AllowedDSUIDs: []string{"prom-main"},
			Store:         store,
		})
		defer mgr.Close()

		snap, err := mgr.AnalyzeStatus(context.Background(), alice, sampleAnalyzePayload())
		if !errors.Is(err, llm.ErrLLMUnavailable) {
			t.Fatalf("expected llm.ErrLLMUnavailable (502 classification), got %v", err)
		}
		if snap.Status != jobs.StatusFailed {
			t.Fatalf("expected failed status, got %q", snap.Status)
		}
		if strings.Contains(snap.ErrorMessage, "glsa_123456") || strings.Contains(snap.ErrorMessage, "https://llm.internal") {
			t.Fatalf("expected error message persisted in JobStore to be redacted, got %q", snap.ErrorMessage)
		}
	})

	t.Run("mock_client_clamps_long_alert_panel_warning_names", func(t *testing.T) {
		store := makeStore(t)
		defer func() { _ = store.Close() }()

		mgr := jobs.NewManager(&llm.MockClient{}, jobs.Config{
			AllowedDSUIDs: []string{"prom-main"},
			Store:         store,
		})
		defer mgr.Close()

		longPayload := sampleAnalyzePayload()
		longPayload.Evidence.Alerts[0].AlertName = strings.Repeat("AlertNameLong", 9) // 117 runes
		longPayload.Evidence.Metrics = []ir.MetricEvidenceItem{{
			ID:                "ev-metric-1",
			PanelTitle:        strings.Repeat("PanelTitleX", 10), // 110 runes
			QueryStatus:       "ok",
			Trend:             "rising",
			ThresholdBreached: "critical",
			AnomalyHint:       "degraded",
		}}
		longPayload.CollectionWarnings = []ir.CollectionWarningItem{{
			ID:      "ev-warn-1",
			Code:    "dashboard_partial",
			Message: strings.Repeat("W", 195), // > MaxReportDataGapRunes (160)
		}}

		snap, err := mgr.AnalyzeStatus(context.Background(), alice, longPayload)
		if err != nil {
			t.Fatalf("expected MockClient to clamp long names cleanly without 502, got %v", err)
		}
		if snap.Status != jobs.StatusReady || snap.StatusReport == nil {
			t.Fatalf("expected ready status report, got %#v", snap)
		}
	})
}

func TestManager_AnalyzeStatus_MemoryStore(t *testing.T) {
	runReportStoreSuite(t, func(t *testing.T) jobs.JobStore {
		t.Helper()
		return jobs.NewMemoryStore()
	})
}

func TestManager_AnalyzeStatus_RedisStore(t *testing.T) {
	runReportStoreSuite(t, func(t *testing.T) jobs.JobStore {
		t.Helper()
		mr := miniredis.RunT(t)
		store, err := jobs.NewRedisStore(jobs.RedisConfig{
			Addr:      mr.Addr(),
			KeyPrefix: "llmstudio:reporttest",
		})
		if err != nil {
			t.Fatalf("NewRedisStore failed: %v", err)
		}
		return store
	})
}
