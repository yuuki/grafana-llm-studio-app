package ir

import "testing"

func FuzzDecodeAndValidateStatusReport(f *testing.F) {
	f.Add([]byte(`{"schemaVersion":"1.0","overallStatus":"healthy","confidence":"high","executiveSummary":"All clear.","timeline":[],"findings":[],"hypotheses":[],"recommendedActions":[],"dataGaps":[]}`))
	f.Add([]byte(`{"schemaVersion":"1.0","overallStatus":"critical","confidence":"medium","executiveSummary":"Latency > 500ms","timeline":[{"timestamp":"2026-11-27T13:05:00Z","event":"Spike","evidenceIds":["ev-alert-1"]}],"findings":[{"id":"f-1","category":"alert","severity":"critical","title":"HighErrorRate","detail":"5xx > 5%","evidenceIds":["ev-alert-1"]}],"hypotheses":[],"recommendedActions":[{"priority":"immediate","action":"Check logs","relatedFindingIds":["f-1"]}],"dataGaps":[]}`))
	f.Add([]byte(`{"schemaVersion":"1.0","overallStatus":"degraded","confidence":"low","executiveSummary":"<script>alert(1)</script>","timeline":[],"findings":[],"hypotheses":[],"recommendedActions":[],"dataGaps":[]}`))

	allowed := map[string]bool{
		"ev-alert-1":  true,
		"ev-rule-1":   true,
		"ev-metric-1": true,
		"ev-warn-1":   true,
		"ev-status-1": true,
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		report, err := DecodeStrictStatusReport(data)
		if err != nil {
			return
		}
		_ = ValidateStatusReport(report, allowed)
	})
}
