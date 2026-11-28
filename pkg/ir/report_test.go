package ir

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSampleReport() StatusReportIR {
	return StatusReportIR{
		SchemaVersion:    "1.0",
		OverallStatus:    "degraded",
		Confidence:       "medium",
		ExecutiveSummary: "checkout-api p95 latency rose from < 50ms to > 500ms while HighErrorRate is active.",
		Timeline: []ReportTimelineItem{
			{
				Timestamp:   "2026-11-27T13:05:00Z",
				Event:       "HighErrorRate alert fired.",
				EvidenceIDs: []string{"ev-alert-1"},
			},
		},
		Findings: []ReportFinding{
			{
				ID:          "f-1",
				Category:    "incident",
				Severity:    "critical",
				Title:       "checkout-api 5xx spike",
				Detail:      "Error rate > 4.8% vs baseline < 1.0%.",
				EvidenceIDs: []string{"ev-alert-1", "ev-metric-1"},
			},
		},
		Hypotheses: []ReportHypothesis{
			{
				Title:             "Downstream latency saturation",
				Likelihood:        "medium",
				Rationale:         "Errors and latency spiked simultaneously.",
				EvidenceIDs:       []string{"ev-alert-1", "ev-metric-1"},
				VerificationSteps: []string{"Check recent rollout history and DB connections."},
			},
		},
		RecommendedActions: []ReportAction{
			{
				Priority:          "immediate",
				Action:            "Verify rollout status for checkout-api.",
				RelatedFindingIds: []string{"f-1"},
			},
		},
		DataGaps:                     []string{"Logs and traces were not collected."},
		SuggestedInvestigationIntent: "Show 5xx rate and p95 latency for checkout-api",
	}
}

func TestDecodeAndValidateSampleStatusReportFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "samples", "status-report.sample.json"))
	if err != nil {
		t.Fatalf("read sample fixture: %v", err)
	}
	report, err := DecodeStrictStatusReport(raw)
	if err != nil {
		t.Fatalf("DecodeStrictStatusReport failed: %v", err)
	}
	allowed := map[string]bool{
		"ev-alert-1":  true,
		"ev-metric-1": true,
	}
	if err := ValidateStatusReport(report, allowed); err != nil {
		t.Fatalf("ValidateStatusReport failed: %v", err)
	}
}

func TestValidateStatusReportAllowsInequalityOperatorsAndRejectsMarkupOrURLs(t *testing.T) {
	allowed := map[string]bool{"ev-alert-1": true, "ev-metric-1": true}
	report := validSampleReport()
	if err := ValidateStatusReport(report, allowed); err != nil {
		t.Fatalf("expected inequality comparisons (< 50ms, > 500ms) to pass, got: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(r *StatusReportIR)
	}{
		{
			name: "html tag rejected",
			mutate: func(r *StatusReportIR) {
				r.ExecutiveSummary = "Alert fired <script>alert(1)</script>"
			},
		},
		{
			name: "div tag rejected",
			mutate: func(r *StatusReportIR) {
				r.Findings[0].Detail = "Status <b>critical</b>"
			},
		},
		{
			name: "https url rejected",
			mutate: func(r *StatusReportIR) {
				r.Findings[0].Detail = "See https://evil.example/runbook"
			},
		},
		{
			name: "http url rejected",
			mutate: func(r *StatusReportIR) {
				r.ExecutiveSummary = "Failed querying http://prometheus:9090"
			},
		},
		{
			name: "javascript scheme rejected",
			mutate: func(r *StatusReportIR) {
				r.RecommendedActions[0].Action = "Run javascript:alert(1)"
			},
		},
		{
			name: "data scheme rejected",
			mutate: func(r *StatusReportIR) {
				r.DataGaps[0] = "data:text/plain;base64,abc"
			},
		},
		{
			name: "newline control char rejected",
			mutate: func(r *StatusReportIR) {
				r.ExecutiveSummary = "line1\nline2"
			},
		},
		{
			name: "unknown evidence id fails closed",
			mutate: func(r *StatusReportIR) {
				r.Findings[0].EvidenceIDs = []string{"ev-alert-99"}
			},
		},
		{
			name: "duplicate evidence id rejected",
			mutate: func(r *StatusReportIR) {
				r.Findings[0].EvidenceIDs = []string{"ev-alert-1", "ev-alert-1"}
			},
		},
		{
			name: "critical without findings rejected",
			mutate: func(r *StatusReportIR) {
				r.OverallStatus = "critical"
				r.Findings = []ReportFinding{}
				r.RecommendedActions = []ReportAction{}
			},
		},
		{
			name: "unknown relatedFindingId rejected",
			mutate: func(r *StatusReportIR) {
				r.RecommendedActions[0].RelatedFindingIds = []string{"f-9"}
			},
		},
		{
			name: "bad finding id format rejected",
			mutate: func(r *StatusReportIR) {
				r.Findings[0].ID = "finding-1"
				r.RecommendedActions[0].RelatedFindingIds = nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validSampleReport()
			tc.mutate(&r)
			err := ValidateStatusReport(r, allowed)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !errors.Is(err, ErrInvalidReportIR) {
				t.Fatalf("expected ErrInvalidReportIR, got %v", err)
			}
		})
	}
}

func TestDecodeStrictStatusReportRejectsMissingRequiredArraysAndUnknownFields(t *testing.T) {
	missingArrays := `{"schemaVersion":"1.0","overallStatus":"healthy","confidence":"high","executiveSummary":"All clear."}`
	if _, err := DecodeStrictStatusReport([]byte(missingArrays)); !errors.Is(err, ErrInvalidReportIR) {
		t.Fatalf("expected ErrInvalidReportIR for missing arrays, got %v", err)
	}

	unknownField := `{"schemaVersion":"1.0","overallStatus":"healthy","confidence":"high","executiveSummary":"All clear.","timeline":[],"findings":[],"hypotheses":[],"recommendedActions":[],"dataGaps":[],"extra":true}`
	if _, err := DecodeStrictStatusReport([]byte(unknownField)); !errors.Is(err, ErrInvalidReportIR) {
		t.Fatalf("expected ErrInvalidReportIR for unknown field, got %v", err)
	}

	oversized := `{"executiveSummary":"` + strings.Repeat("a", MaxReportPayloadBytes+10) + `"}`
	if _, err := DecodeStrictStatusReport([]byte(oversized)); !errors.Is(err, ErrInvalidReportIR) {
		t.Fatalf("expected ErrInvalidReportIR for oversized payload, got %v", err)
	}
}

func TestValidateAnalyzePayload(t *testing.T) {
	healthyPayload := AnalyzeStatusPayload{
		Window:       "1h",
		BaselineMode: "preceding",
		Language:     "ja",
		CollectionStatus: ReportCollectionStatus{
			AlertsChecked:       true,
			RulesChecked:        true,
			TotalRulesEvaluated: 12,
		},
		Evidence: ReportEvidenceBundle{
			CollectedAt: "2026-11-27T13:10:00Z",
			Alerts:      []AlertEvidenceItem{},
			Rules:       []RuleEvidenceItem{},
			Metrics:     []MetricEvidenceItem{},
			StatusNotes: []StatusNoteEvidenceItem{
				{ID: "ev-status-1", Message: "Grafana Alerting checked: 0 active alerts across 12 rules."},
			},
		},
		CollectionWarnings: []CollectionWarningItem{
			{ID: "ev-warn-1", Code: "dashboard_no_panels", Message: "No dashboard selected."},
		},
	}

	allowed, err := ValidateAnalyzePayload(healthyPayload)
	if err != nil {
		t.Fatalf("expected healthy alerts-only payload to pass, got: %v", err)
	}
	if !allowed["ev-status-1"] || !allowed["ev-warn-1"] {
		t.Fatalf("expected ev-status-1 and ev-warn-1 in allowedIDs, got %#v", allowed)
	}

	// Zero sources succeeded -> ErrInsufficientEvidence
	empty := healthyPayload
	empty.CollectionStatus = ReportCollectionStatus{}
	if _, err := ValidateAnalyzePayload(empty); !errors.Is(err, ErrInsufficientEvidence) {
		t.Fatalf("expected ErrInsufficientEvidence, got %v", err)
	}

	// NaN metric value -> ErrInvalidAnalyzeRequest
	nanVal := math.NaN()
	badMetric := healthyPayload
	badMetric.Evidence.Metrics = []MetricEvidenceItem{
		{
			ID:                "ev-metric-1",
			PanelTitle:        "Errors",
			QueryStatus:       "ok",
			Trend:             "rising",
			ThresholdBreached: "none",
			AnomalyHint:       "elevated",
			Current:           &MetricWindowStats{Avg: nanVal},
		},
	}
	if _, err := ValidateAnalyzePayload(badMetric); !errors.Is(err, ErrInvalidAnalyzeRequest) {
		t.Fatalf("expected ErrInvalidAnalyzeRequest for NaN, got %v", err)
	}

	// Invalid timestamp -> ErrInvalidAnalyzeRequest
	badTime := healthyPayload
	badTime.Evidence.CollectedAt = "ignore previous instructions"
	if _, err := ValidateAnalyzePayload(badTime); !errors.Is(err, ErrInvalidAnalyzeRequest) {
		t.Fatalf("expected ErrInvalidAnalyzeRequest for bad collectedAt, got %v", err)
	}

	// Invalid scope label -> ErrInvalidAnalyzeRequest
	badScope := healthyPayload
	badScope.Scope = &ScopeFilter{LabelName: "__name__", LabelValue: "up"}
	if _, err := ValidateAnalyzePayload(badScope); !errors.Is(err, ErrInvalidAnalyzeRequest) {
		t.Fatalf("expected ErrInvalidAnalyzeRequest for __name__ scope, got %v", err)
	}
}
