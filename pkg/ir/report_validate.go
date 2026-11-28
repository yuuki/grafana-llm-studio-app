package ir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrInvalidReportIR indicates a StatusReportIR failed strict decoding or deterministic validation.
	ErrInvalidReportIR = errors.New("invalid status report IR")
	// ErrInvalidAnalyzeRequest indicates an AnalyzeStatusPayload failed pre-flight validation.
	ErrInvalidAnalyzeRequest = errors.New("invalid analyze status request")
	// ErrInsufficientEvidence indicates no evidence source succeeded during collection.
	ErrInsufficientEvidence = errors.New("insufficient evidence collected for status analysis")

	findingIDPattern       = regexp.MustCompile(`^f-[1-9][0-9]?$`)
	evidenceIDPattern      = regexp.MustCompile(`^ev-(alert|rule|metric|warn|status)-[1-9][0-9]{0,2}$`)
	alertEvidenceIDPattern = regexp.MustCompile(`^ev-alert-[1-9][0-9]{0,2}$`)
	ruleEvidenceIDPattern  = regexp.MustCompile(`^ev-rule-[1-9][0-9]{0,2}$`)
	metricEvidenceIDPat    = regexp.MustCompile(`^ev-metric-[1-9][0-9]{0,2}$`)
	warnEvidenceIDPattern  = regexp.MustCompile(`^ev-warn-[1-9][0-9]{0,2}$`)
	statusEvidenceIDPat    = regexp.MustCompile(`^ev-status-[1-9][0-9]{0,2}$`)
	// proseHTMLTagPattern matches actual HTML/XML tag syntax (<div, </script>, <img src=...>)
	// without falsely rejecting operational comparisons like "p95 < 50ms -> > 500ms".
	proseHTMLTagPattern = regexp.MustCompile(`(?i)<\s*/?\s*[a-z][a-z0-9-]*[\s/>]`)

	allowedReportSchemaVersions = map[string]bool{
		"1.0": true,
	}

	allowedReportOverallStatuses = map[string]bool{
		"healthy":           true,
		"degraded":          true,
		"critical":          true,
		"insufficient_data": true,
	}

	allowedReportConfidences = map[string]bool{
		"high":   true,
		"medium": true,
		"low":    true,
	}

	allowedReportFindingCategories = map[string]bool{
		"alert":          true,
		"performance":    true,
		"incident":       true,
		"monitoring_gap": true,
	}

	allowedReportFindingSeverities = map[string]bool{
		"critical": true,
		"warning":  true,
		"info":     true,
	}

	allowedReportLikelihoods = map[string]bool{
		"high":   true,
		"medium": true,
		"low":    true,
	}

	allowedReportActionPriorities = map[string]bool{
		"immediate":  true,
		"short_term": true,
		"follow_up":  true,
	}

	allowedReportWindows = map[string]bool{
		"15m": true,
		"1h":  true,
		"6h":  true,
		"24h": true,
	}

	allowedBaselineModes = map[string]bool{
		"preceding": true,
		"24h_ago":   true,
		"none":      true,
	}

	allowedReportLanguages = map[string]bool{
		"en": true,
		"ja": true,
	}

	allowedAlertStates = map[string]bool{
		"active":      true,
		"suppressed":  true,
		"unprocessed": true,
	}

	allowedRuleStates = map[string]bool{
		"firing":     true,
		"recovering": true,
		"pending":    true,
		"inactive":   true,
	}

	allowedRuleHealths = map[string]bool{
		"ok":      true,
		"error":   true,
		"nodata":  true,
		"unknown": true,
	}

	allowedMetricQueryStatuses = map[string]bool{
		"ok":      true,
		"empty":   true,
		"error":   true,
		"timeout": true,
	}

	allowedMetricTrends = map[string]bool{
		"rising":   true,
		"falling":  true,
		"stable":   true,
		"spiking":  true,
		"volatile": true,
		"no_data":  true,
	}

	allowedThresholdBreaches = map[string]bool{
		"none":     true,
		"warning":  true,
		"critical": true,
	}

	allowedAnomalyHints = map[string]bool{
		"normal":       true,
		"elevated":     true,
		"degraded":     true,
		"missing_data": true,
		"query_error":  true,
	}

	allowedCollectionWarningCodes = map[string]bool{
		"alerting_forbidden":   true,
		"alerting_unavailable": true,
		"dashboard_partial":    true,
		"dashboard_no_panels":  true,
		"collection_timeout":   true,
		"evidence_truncated":   true,
		"baseline_skipped":     true,
	}

	forbiddenReportProseSchemes = []string{
		"<script",
		"</script",
		"<iframe",
		"<svg",
		"<object",
		"javascript:",
		"vbscript:",
		"data:",
		"http://",
		"https://",
	}
)

type rawStatusReportRequiredCheck struct {
	Timeline           *json.RawMessage `json:"timeline"`
	Findings           *json.RawMessage `json:"findings"`
	Hypotheses         *json.RawMessage `json:"hypotheses"`
	RecommendedActions *json.RawMessage `json:"recommendedActions"`
	DataGaps           *json.RawMessage `json:"dataGaps"`
}

// DecodeStrictStatusReport decodes a StatusReportIR payload strictly, rejecting unknown fields,
// trailing tokens, oversized payloads, and omitted required arrays.
func DecodeStrictStatusReport(data []byte) (StatusReportIR, error) {
	if len(data) == 0 {
		return StatusReportIR{}, fmt.Errorf("%w: empty status report JSON payload", ErrInvalidReportIR)
	}
	if len(data) > MaxReportPayloadBytes {
		return StatusReportIR{}, fmt.Errorf("%w: status report payload exceeds %d bytes", ErrInvalidReportIR, MaxReportPayloadBytes)
	}

	var report StatusReportIR
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return StatusReportIR{}, fmt.Errorf("%w: strict JSON decode failed: %v", ErrInvalidReportIR, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return StatusReportIR{}, fmt.Errorf("%w: unexpected trailing tokens after JSON object", ErrInvalidReportIR)
	}

	var reqCheck rawStatusReportRequiredCheck
	if err := json.Unmarshal(data, &reqCheck); err != nil {
		return StatusReportIR{}, fmt.Errorf("%w: required field check failed: %v", ErrInvalidReportIR, err)
	}
	if reqCheck.Timeline == nil || bytes.Equal(bytes.TrimSpace(*reqCheck.Timeline), []byte("null")) {
		return StatusReportIR{}, fmt.Errorf("%w: timeline array is required", ErrInvalidReportIR)
	}
	if reqCheck.Findings == nil || bytes.Equal(bytes.TrimSpace(*reqCheck.Findings), []byte("null")) {
		return StatusReportIR{}, fmt.Errorf("%w: findings array is required", ErrInvalidReportIR)
	}
	if reqCheck.Hypotheses == nil || bytes.Equal(bytes.TrimSpace(*reqCheck.Hypotheses), []byte("null")) {
		return StatusReportIR{}, fmt.Errorf("%w: hypotheses array is required", ErrInvalidReportIR)
	}
	if reqCheck.RecommendedActions == nil || bytes.Equal(bytes.TrimSpace(*reqCheck.RecommendedActions), []byte("null")) {
		return StatusReportIR{}, fmt.Errorf("%w: recommendedActions array is required", ErrInvalidReportIR)
	}
	if reqCheck.DataGaps == nil || bytes.Equal(bytes.TrimSpace(*reqCheck.DataGaps), []byte("null")) {
		return StatusReportIR{}, fmt.Errorf("%w: dataGaps array is required", ErrInvalidReportIR)
	}

	return report, nil
}

// ValidateStatusReport deterministically validates a StatusReportIR against schema bounds,
// prose-safe hygiene rules, finding cross-references, and exact allowedEvidenceIDs grounding.
func ValidateStatusReport(report StatusReportIR, allowedEvidenceIDs map[string]bool) error {
	var issues []string

	if !allowedReportSchemaVersions[report.SchemaVersion] {
		issues = append(issues, fmt.Sprintf("unsupported schemaVersion %q", report.SchemaVersion))
	}
	if !allowedReportOverallStatuses[report.OverallStatus] {
		issues = append(issues, fmt.Sprintf("unsupported overallStatus %q", report.OverallStatus))
	}
	if !allowedReportConfidences[report.Confidence] {
		issues = append(issues, fmt.Sprintf("unsupported confidence %q", report.Confidence))
	}
	if err := validateReportProse("executiveSummary", report.ExecutiveSummary, 1, MaxReportSummaryRunes); err != nil {
		issues = append(issues, err.Error())
	}
	if report.SuggestedInvestigationIntent != "" {
		if err := validateReportProse("suggestedInvestigationIntent", report.SuggestedInvestigationIntent, 1, MaxReportIntentRunes); err != nil {
			issues = append(issues, err.Error())
		}
	}

	if len(report.Timeline) > MaxReportTimelineItems {
		issues = append(issues, fmt.Sprintf("timeline has %d items (max %d)", len(report.Timeline), MaxReportTimelineItems))
	}
	for i, item := range report.Timeline {
		prefix := fmt.Sprintf("timeline[%d]", i)
		if err := validateReportProse(prefix+".timestamp", item.Timestamp, 1, MaxReportTimestampRunes); err != nil {
			issues = append(issues, err.Error())
		}
		if err := validateReportProse(prefix+".event", item.Event, 1, MaxReportEventRunes); err != nil {
			issues = append(issues, err.Error())
		}
		issues = append(issues, validateEvidenceIDRefs(prefix+".evidenceIds", item.EvidenceIDs, allowedEvidenceIDs)...)
	}

	if len(report.Findings) > MaxReportFindings {
		issues = append(issues, fmt.Sprintf("findings has %d items (max %d)", len(report.Findings), MaxReportFindings))
	}
	if (report.OverallStatus == "degraded" || report.OverallStatus == "critical") && len(report.Findings) == 0 {
		issues = append(issues, fmt.Sprintf("overallStatus %q requires at least 1 grounded finding", report.OverallStatus))
	}

	declaredFindingIDs := make(map[string]bool, len(report.Findings))
	for i, f := range report.Findings {
		prefix := fmt.Sprintf("findings[%d]", i)
		if !findingIDPattern.MatchString(f.ID) {
			issues = append(issues, fmt.Sprintf("%s.id %q does not match ^f-[1-9][0-9]?$", prefix, f.ID))
		} else if declaredFindingIDs[f.ID] {
			issues = append(issues, fmt.Sprintf("duplicate finding id %q", f.ID))
		} else {
			declaredFindingIDs[f.ID] = true
		}
		if !allowedReportFindingCategories[f.Category] {
			issues = append(issues, fmt.Sprintf("%s.category %q not allowed", prefix, f.Category))
		}
		if !allowedReportFindingSeverities[f.Severity] {
			issues = append(issues, fmt.Sprintf("%s.severity %q not allowed", prefix, f.Severity))
		}
		if err := validateReportProse(prefix+".title", f.Title, 1, MaxReportFindingTitleRunes); err != nil {
			issues = append(issues, err.Error())
		}
		if err := validateReportProse(prefix+".detail", f.Detail, 1, MaxReportFindingDetailRunes); err != nil {
			issues = append(issues, err.Error())
		}
		issues = append(issues, validateEvidenceIDRefs(prefix+".evidenceIds", f.EvidenceIDs, allowedEvidenceIDs)...)
	}

	if len(report.Hypotheses) > MaxReportHypotheses {
		issues = append(issues, fmt.Sprintf("hypotheses has %d items (max %d)", len(report.Hypotheses), MaxReportHypotheses))
	}
	for i, h := range report.Hypotheses {
		prefix := fmt.Sprintf("hypotheses[%d]", i)
		if err := validateReportProse(prefix+".title", h.Title, 1, MaxReportHypTitleRunes); err != nil {
			issues = append(issues, err.Error())
		}
		if !allowedReportLikelihoods[h.Likelihood] {
			issues = append(issues, fmt.Sprintf("%s.likelihood %q not allowed", prefix, h.Likelihood))
		}
		if err := validateReportProse(prefix+".rationale", h.Rationale, 1, MaxReportHypRationaleRunes); err != nil {
			issues = append(issues, err.Error())
		}
		issues = append(issues, validateEvidenceIDRefs(prefix+".evidenceIds", h.EvidenceIDs, allowedEvidenceIDs)...)
		if len(h.VerificationSteps) > MaxReportHypSteps {
			issues = append(issues, fmt.Sprintf("%s.verificationSteps has %d items (max %d)", prefix, len(h.VerificationSteps), MaxReportHypSteps))
		}
		for j, step := range h.VerificationSteps {
			if err := validateReportProse(fmt.Sprintf("%s.verificationSteps[%d]", prefix, j), step, 1, MaxReportHypStepRunes); err != nil {
				issues = append(issues, err.Error())
			}
		}
	}

	if len(report.RecommendedActions) > MaxReportActions {
		issues = append(issues, fmt.Sprintf("recommendedActions has %d items (max %d)", len(report.RecommendedActions), MaxReportActions))
	}
	for i, act := range report.RecommendedActions {
		prefix := fmt.Sprintf("recommendedActions[%d]", i)
		if !allowedReportActionPriorities[act.Priority] {
			issues = append(issues, fmt.Sprintf("%s.priority %q not allowed", prefix, act.Priority))
		}
		if err := validateReportProse(prefix+".action", act.Action, 1, MaxReportActionRunes); err != nil {
			issues = append(issues, err.Error())
		}
		if len(act.RelatedFindingIds) > MaxReportRelatedFindings {
			issues = append(issues, fmt.Sprintf("%s.relatedFindingIds has %d items (max %d)", prefix, len(act.RelatedFindingIds), MaxReportRelatedFindings))
		}
		seenRel := make(map[string]bool, len(act.RelatedFindingIds))
		for j, fid := range act.RelatedFindingIds {
			if !findingIDPattern.MatchString(fid) {
				issues = append(issues, fmt.Sprintf("%s.relatedFindingIds[%d] %q invalid format", prefix, j, fid))
				continue
			}
			if seenRel[fid] {
				issues = append(issues, fmt.Sprintf("%s.relatedFindingIds contains duplicate %q", prefix, fid))
				continue
			}
			seenRel[fid] = true
			if !declaredFindingIDs[fid] {
				issues = append(issues, fmt.Sprintf("%s.relatedFindingIds[%d] references unknown finding %q", prefix, j, fid))
			}
		}
	}

	if len(report.DataGaps) > MaxReportDataGaps {
		issues = append(issues, fmt.Sprintf("dataGaps has %d items (max %d)", len(report.DataGaps), MaxReportDataGaps))
	}
	for i, gap := range report.DataGaps {
		if err := validateReportProse(fmt.Sprintf("dataGaps[%d]", i), gap, 1, MaxReportDataGapRunes); err != nil {
			issues = append(issues, err.Error())
		}
	}

	if len(issues) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidReportIR, strings.Join(issues, "; "))
	}
	return nil
}

func validateEvidenceIDRefs(field string, ids []string, allowed map[string]bool) []string {
	var issues []string
	if len(ids) == 0 {
		return []string{fmt.Sprintf("%s must cite at least 1 evidence ID", field)}
	}
	if len(ids) > MaxReportEvidenceIDsPerItem {
		issues = append(issues, fmt.Sprintf("%s has %d IDs (max %d)", field, len(ids), MaxReportEvidenceIDsPerItem))
	}
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if !evidenceIDPattern.MatchString(id) {
			issues = append(issues, fmt.Sprintf("%s[%d] %q invalid evidence ID format", field, i, id))
			continue
		}
		if seen[id] {
			issues = append(issues, fmt.Sprintf("%s contains duplicate evidence ID %q", field, id))
			continue
		}
		seen[id] = true
		if !allowed[id] {
			issues = append(issues, fmt.Sprintf("%s[%d] references unknown evidence ID %q", field, i, id))
		}
	}
	return issues
}

// validateReportProse enforces UTF-8 validity, rune bounds, absence of control/Bidi chars,
// HTML tags, event handlers, and active/URL schemes while permitting benign inequality
// comparisons such as "p95 < 50ms -> > 500ms".
func validateReportProse(field, val string, minRunes, maxRunes int) error {
	if !utf8.ValidString(val) {
		return fmt.Errorf("%s contains invalid UTF-8", field)
	}
	trimmed := strings.TrimSpace(val)
	if utf8.RuneCountInString(trimmed) < minRunes {
		return fmt.Errorf("%s must not be empty", field)
	}
	if utf8.RuneCountInString(val) > maxRunes {
		return fmt.Errorf("%s length %d runes exceeds max %d", field, utf8.RuneCountInString(val), maxRunes)
	}
	if err := checkForbiddenRunes(field, val); err != nil {
		return err
	}
	if proseHTMLTagPattern.MatchString(val) || eventAttrPattern.MatchString(val) {
		return fmt.Errorf("%s must not contain HTML markup or event handlers", field)
	}
	lower := strings.ToLower(val)
	for _, frag := range forbiddenReportProseSchemes {
		if strings.Contains(lower, frag) {
			return fmt.Errorf("%s contains forbidden markup or URL scheme %q", field, frag)
		}
	}
	return nil
}

// ValidateAnalyzePayload validates pre-flight invariants on AnalyzeStatusPayload and returns
// the authoritative set of citable evidence IDs present in the request.
func ValidateAnalyzePayload(p AnalyzeStatusPayload) (map[string]bool, error) {
	if !allowedReportWindows[p.Window] {
		return nil, fmt.Errorf("%w: unsupported window %q", ErrInvalidAnalyzeRequest, p.Window)
	}
	if !allowedBaselineModes[p.BaselineMode] {
		return nil, fmt.Errorf("%w: unsupported baselineMode %q", ErrInvalidAnalyzeRequest, p.BaselineMode)
	}
	if !allowedReportLanguages[p.Language] {
		return nil, fmt.Errorf("%w: unsupported language %q", ErrInvalidAnalyzeRequest, p.Language)
	}
	if p.Scope != nil {
		if err := validateScopeFilter(p.Scope); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
	}
	if !utf8.ValidString(p.UserFocus) {
		return nil, fmt.Errorf("%w: userFocus contains invalid UTF-8", ErrInvalidAnalyzeRequest)
	}
	if utf8.RuneCountInString(p.UserFocus) > MaxAnalyzeFocusRunes {
		return nil, fmt.Errorf("%w: userFocus exceeds max %d runes", ErrInvalidAnalyzeRequest, MaxAnalyzeFocusRunes)
	}
	if err := validateRFC3339("evidence.collectedAt", p.Evidence.CollectedAt, true); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
	}

	if p.CollectionStatus.TotalRulesEvaluated < 0 || p.CollectionStatus.ActiveAlertsTotal < 0 || p.CollectionStatus.AbnormalRulesTotal < 0 {
		return nil, fmt.Errorf("%w: collectionStatus counters must be non-negative", ErrInvalidAnalyzeRequest)
	}
	if !p.CollectionStatus.AlertsChecked && !p.CollectionStatus.RulesChecked && len(p.Evidence.Metrics) == 0 {
		return nil, fmt.Errorf("%w: alerting and dashboard metric collection all failed or returned no sources", ErrInsufficientEvidence)
	}

	if p.DashboardContext != nil {
		if utf8.RuneCountInString(strings.TrimSpace(p.DashboardContext.Title)) == 0 || utf8.RuneCountInString(p.DashboardContext.Title) > MaxTitleLen {
			return nil, fmt.Errorf("%w: dashboardContext.title must be 1..%d runes", ErrInvalidAnalyzeRequest, MaxTitleLen)
		}
		if p.DashboardContext.UID != "" && utf8.RuneCountInString(p.DashboardContext.UID) > 64 {
			return nil, fmt.Errorf("%w: dashboardContext.uid exceeds 64 runes", ErrInvalidAnalyzeRequest)
		}
		if p.DashboardContext.PanelCount < 0 || p.DashboardContext.AnalyzedPanelCount < 0 {
			return nil, fmt.Errorf("%w: dashboardContext panel counts must be non-negative", ErrInvalidAnalyzeRequest)
		}
	}

	if len(p.Evidence.Alerts) > MaxAlertEvidenceItems {
		return nil, fmt.Errorf("%w: evidence.alerts has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.Alerts), MaxAlertEvidenceItems)
	}
	if len(p.Evidence.Rules) > MaxRuleEvidenceItems {
		return nil, fmt.Errorf("%w: evidence.rules has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.Rules), MaxRuleEvidenceItems)
	}
	if len(p.Evidence.Metrics) > MaxMetricEvidenceItems {
		return nil, fmt.Errorf("%w: evidence.metrics has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.Metrics), MaxMetricEvidenceItems)
	}
	if len(p.Evidence.StatusNotes) > MaxStatusNoteItems {
		return nil, fmt.Errorf("%w: evidence.statusNotes has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.StatusNotes), MaxStatusNoteItems)
	}
	if len(p.CollectionWarnings) > MaxCollectionWarnings {
		return nil, fmt.Errorf("%w: collectionWarnings has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.CollectionWarnings), MaxCollectionWarnings)
	}

	allowedIDs := make(map[string]bool, len(p.Evidence.Alerts)+len(p.Evidence.Rules)+len(p.Evidence.Metrics)+len(p.Evidence.StatusNotes)+len(p.CollectionWarnings))
	registerID := func(field, id string, pat *regexp.Regexp) error {
		if !pat.MatchString(id) {
			return fmt.Errorf("%s %q has invalid ID format", field, id)
		}
		if allowedIDs[id] {
			return fmt.Errorf("duplicate evidence ID %q", id)
		}
		allowedIDs[id] = true
		return nil
	}

	for i, a := range p.Evidence.Alerts {
		prefix := fmt.Sprintf("evidence.alerts[%d]", i)
		if err := registerID(prefix+".id", a.ID, alertEvidenceIDPattern); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if strings.TrimSpace(a.AlertName) == "" || utf8.RuneCountInString(a.AlertName) > 128 {
			return nil, fmt.Errorf("%w: %s.alertName must be 1..128 runes", ErrInvalidAnalyzeRequest, prefix)
		}
		if !allowedAlertStates[a.State] {
			return nil, fmt.Errorf("%w: %s.state %q not allowed", ErrInvalidAnalyzeRequest, prefix, a.State)
		}
		if err := validateRFC3339(prefix+".startsAt", a.StartsAt, false); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if len(a.Labels) > MaxEvidenceLabels {
			return nil, fmt.Errorf("%w: %s.labels has %d entries (max %d)", ErrInvalidAnalyzeRequest, prefix, len(a.Labels), MaxEvidenceLabels)
		}
		for k, v := range a.Labels {
			if !labelNamePattern.MatchString(k) {
				return nil, fmt.Errorf("%w: %s.labels key %q invalid", ErrInvalidAnalyzeRequest, prefix, k)
			}
			if utf8.RuneCountInString(v) > MaxEvidenceLabelValRune {
				return nil, fmt.Errorf("%w: %s.labels[%q] exceeds %d runes", ErrInvalidAnalyzeRequest, prefix, k, MaxEvidenceLabelValRune)
			}
		}
	}

	for i, r := range p.Evidence.Rules {
		prefix := fmt.Sprintf("evidence.rules[%d]", i)
		if err := registerID(prefix+".id", r.ID, ruleEvidenceIDPattern); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if strings.TrimSpace(r.RuleName) == "" || utf8.RuneCountInString(r.RuleName) > 128 {
			return nil, fmt.Errorf("%w: %s.ruleName must be 1..128 runes", ErrInvalidAnalyzeRequest, prefix)
		}
		if !allowedRuleStates[r.State] {
			return nil, fmt.Errorf("%w: %s.state %q not allowed", ErrInvalidAnalyzeRequest, prefix, r.State)
		}
		if !allowedRuleHealths[r.Health] {
			return nil, fmt.Errorf("%w: %s.health %q not allowed", ErrInvalidAnalyzeRequest, prefix, r.Health)
		}
		if r.ActiveAlertsCount < 0 {
			return nil, fmt.Errorf("%w: %s.activeAlertsCount must be non-negative", ErrInvalidAnalyzeRequest, prefix)
		}
	}

	for i, m := range p.Evidence.Metrics {
		prefix := fmt.Sprintf("evidence.metrics[%d]", i)
		if err := registerID(prefix+".id", m.ID, metricEvidenceIDPat); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if strings.TrimSpace(m.PanelTitle) == "" || utf8.RuneCountInString(m.PanelTitle) > MaxTitleLen {
			return nil, fmt.Errorf("%w: %s.panelTitle must be 1..%d runes", ErrInvalidAnalyzeRequest, prefix, MaxTitleLen)
		}
		if !allowedMetricQueryStatuses[m.QueryStatus] {
			return nil, fmt.Errorf("%w: %s.queryStatus %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.QueryStatus)
		}
		if !allowedMetricTrends[m.Trend] {
			return nil, fmt.Errorf("%w: %s.trend %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.Trend)
		}
		if !allowedThresholdBreaches[m.ThresholdBreached] {
			return nil, fmt.Errorf("%w: %s.thresholdBreached %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.ThresholdBreached)
		}
		if !allowedAnomalyHints[m.AnomalyHint] {
			return nil, fmt.Errorf("%w: %s.anomalyHint %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.AnomalyHint)
		}
		if m.Current != nil {
			if err := validateWindowStats(prefix+".current", m.Current); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
			}
		}
		if m.Baseline != nil {
			if err := checkFiniteFloat(prefix+".baseline.avg", m.Baseline.Avg); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
			}
			if err := checkFiniteFloat(prefix+".baseline.p95", m.Baseline.P95); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
			}
			if err := checkFiniteFloat(prefix+".baseline.max", m.Baseline.Max); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
			}
		}
		if m.DeltaPctAvg != nil {
			if err := checkFiniteFloat(prefix+".deltaPctAvg", *m.DeltaPctAvg); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
			}
		}
		if m.DeltaPctP95 != nil {
			if err := checkFiniteFloat(prefix+".deltaPctP95", *m.DeltaPctP95); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
			}
		}
	}

	for i, s := range p.Evidence.StatusNotes {
		prefix := fmt.Sprintf("evidence.statusNotes[%d]", i)
		if err := registerID(prefix+".id", s.ID, statusEvidenceIDPat); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if strings.TrimSpace(s.Message) == "" || utf8.RuneCountInString(s.Message) > MaxEvidenceSummaryRunes {
			return nil, fmt.Errorf("%w: %s.message must be 1..%d runes", ErrInvalidAnalyzeRequest, prefix, MaxEvidenceSummaryRunes)
		}
	}

	for i, w := range p.CollectionWarnings {
		prefix := fmt.Sprintf("collectionWarnings[%d]", i)
		if err := registerID(prefix+".id", w.ID, warnEvidenceIDPattern); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if !allowedCollectionWarningCodes[w.Code] {
			return nil, fmt.Errorf("%w: %s.code %q not allowed", ErrInvalidAnalyzeRequest, prefix, w.Code)
		}
		if strings.TrimSpace(w.Message) == "" || utf8.RuneCountInString(w.Message) > MaxEvidenceSummaryRunes {
			return nil, fmt.Errorf("%w: %s.message must be 1..%d runes", ErrInvalidAnalyzeRequest, prefix, MaxEvidenceSummaryRunes)
		}
	}

	if len(allowedIDs) == 0 {
		return nil, fmt.Errorf("%w: at least one evidence item or status note is required", ErrInsufficientEvidence)
	}
	return allowedIDs, nil
}

func validateScopeFilter(scope *ScopeFilter) error {
	name := strings.TrimSpace(scope.LabelName)
	val := strings.TrimSpace(scope.LabelValue)
	if !labelNamePattern.MatchString(name) || name == "__name__" {
		return fmt.Errorf("scope.labelName %q is invalid", scope.LabelName)
	}
	if !utf8.ValidString(val) || val == "" || utf8.RuneCountInString(val) > 128 {
		return fmt.Errorf("scope.labelValue must be 1..128 valid UTF-8 runes")
	}
	for _, r := range val {
		if unicode.IsControl(r) || isBidiControl(r) {
			return fmt.Errorf("scope.labelValue contains forbidden control character")
		}
	}
	return nil
}

func validateRFC3339(field, val string, required bool) error {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	if _, err := time.Parse(time.RFC3339, trimmed); err != nil {
		if _, errNano := time.Parse(time.RFC3339Nano, trimmed); errNano != nil {
			return fmt.Errorf("%s must be RFC3339 timestamp", field)
		}
	}
	return nil
}

func validateWindowStats(prefix string, s *MetricWindowStats) error {
	if s.Latest != nil {
		if err := checkFiniteFloat(prefix+".latest", *s.Latest); err != nil {
			return err
		}
	}
	for name, val := range map[string]float64{
		"avg":          s.Avg,
		"p95":          s.P95,
		"min":          s.Min,
		"max":          s.Max,
		"missingRatio": s.MissingRatio,
	} {
		if err := checkFiniteFloat(prefix+"."+name, val); err != nil {
			return err
		}
	}
	if s.SampleCount < 0 {
		return fmt.Errorf("%s.sampleCount must be non-negative", prefix)
	}
	if s.MissingRatio < 0 || s.MissingRatio > 1 {
		return fmt.Errorf("%s.missingRatio must be in [0, 1]", prefix)
	}
	if err := validateRFC3339(prefix+".peakAt", s.PeakAt, false); err != nil {
		return err
	}
	return nil
}

func checkFiniteFloat(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number", field)
	}
	return nil
}
