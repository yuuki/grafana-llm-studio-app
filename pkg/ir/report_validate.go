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
	// proseDataURIPattern matches data: URIs (data:text/html,..., data:;base64,..., data:,...)
	// without falsely rejecting benign SRE prose such as "Missing data: logs not collected".
	proseDataURIPattern = regexp.MustCompile(`(?i)\bdata:(?:[a-z0-9.+-]+/[a-z0-9.+-]+|[^ \t\r\n]*,)`)
	// proseEventAttrPattern matches actual DOM/HTML event handler attributes (onclick=, onerror=,
	// onload=, ononline=, etc.) without falsely rejecting benign SRE labels like "oncall=payments".
	proseEventAttrPattern = regexp.MustCompile(`(?i)\bon(?:abort|afterprint|animation\w*|auxclick|before\w+|blur|canplay\w*|change|click|close|contextmenu|copy|cuechange|cut|dblclick|drag\w*|drop|durationchange|emptied|ended|error|focus\w*|formdata|gotpointercapture|hashchange|input|invalid|key\w+|load\w*|lostpointercapture|message\w*|mouse\w*|offline|online|page\w*|paste|pause|play\w*|pointer\w*|popstate|progress|ratechange|reset|resize|scroll\w*|search|seek\w*|select\w*|slotchange|stalled|storage|submit|suspend|timeupdate|toggle|touch\w+|transition\w+|unload|volumechange|waiting|wheel)\s*=`)

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
		"<!--",
		"javascript:",
		"vbscript:",
		"http://",
		"https://",
		"ftp://",
		"file://",
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

	issues = append(issues, validateReportTimeline(report.Timeline, allowedEvidenceIDs)...)

	declaredFindingIDs, findingIssues := validateReportFindings(report.OverallStatus, report.Findings, allowedEvidenceIDs)
	issues = append(issues, findingIssues...)
	issues = append(issues, validateReportHypotheses(report.Hypotheses, allowedEvidenceIDs)...)
	issues = append(issues, validateReportActions(report.RecommendedActions, declaredFindingIDs)...)
	issues = append(issues, validateReportDataGaps(report.DataGaps)...)

	if len(issues) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidReportIR, strings.Join(issues, "; "))
	}
	return nil
}

func validateReportTimeline(timeline []ReportTimelineItem, allowedEvidenceIDs map[string]bool) []string {
	var issues []string
	if len(timeline) > MaxReportTimelineItems {
		issues = append(issues, fmt.Sprintf("timeline has %d items (max %d)", len(timeline), MaxReportTimelineItems))
	}
	for i, item := range timeline {
		prefix := fmt.Sprintf("timeline[%d]", i)
		if err := validateReportProse(prefix+".timestamp", item.Timestamp, 1, MaxReportTimestampRunes); err != nil {
			issues = append(issues, err.Error())
		}
		if err := validateReportProse(prefix+".event", item.Event, 1, MaxReportEventRunes); err != nil {
			issues = append(issues, err.Error())
		}
		issues = append(issues, validateEvidenceIDRefs(prefix+".evidenceIds", item.EvidenceIDs, allowedEvidenceIDs)...)
	}
	return issues
}

func validateReportFindings(overallStatus string, findings []ReportFinding, allowedEvidenceIDs map[string]bool) (map[string]bool, []string) {
	var issues []string
	if len(findings) > MaxReportFindings {
		issues = append(issues, fmt.Sprintf("findings has %d items (max %d)", len(findings), MaxReportFindings))
	}
	if (overallStatus == "degraded" || overallStatus == "critical") && len(findings) == 0 {
		issues = append(issues, fmt.Sprintf("overallStatus %q requires at least 1 grounded finding", overallStatus))
	}

	declaredFindingIDs := make(map[string]bool, len(findings))
	for i, f := range findings {
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
	return declaredFindingIDs, issues
}

func validateReportHypotheses(hypotheses []ReportHypothesis, allowedEvidenceIDs map[string]bool) []string {
	var issues []string
	if len(hypotheses) > MaxReportHypotheses {
		issues = append(issues, fmt.Sprintf("hypotheses has %d items (max %d)", len(hypotheses), MaxReportHypotheses))
	}
	for i, h := range hypotheses {
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
		if h.VerificationSteps == nil {
			issues = append(issues, fmt.Sprintf("%s.verificationSteps array is required", prefix))
		}
		if len(h.VerificationSteps) > MaxReportHypSteps {
			issues = append(issues, fmt.Sprintf("%s.verificationSteps has %d items (max %d)", prefix, len(h.VerificationSteps), MaxReportHypSteps))
		}
		for j, step := range h.VerificationSteps {
			if err := validateReportProse(fmt.Sprintf("%s.verificationSteps[%d]", prefix, j), step, 1, MaxReportHypStepRunes); err != nil {
				issues = append(issues, err.Error())
			}
		}
	}
	return issues
}

func validateReportActions(actions []ReportAction, declaredFindingIDs map[string]bool) []string {
	var issues []string
	if len(actions) > MaxReportActions {
		issues = append(issues, fmt.Sprintf("recommendedActions has %d items (max %d)", len(actions), MaxReportActions))
	}
	for i, act := range actions {
		prefix := fmt.Sprintf("recommendedActions[%d]", i)
		if !allowedReportActionPriorities[act.Priority] {
			issues = append(issues, fmt.Sprintf("%s.priority %q not allowed", prefix, act.Priority))
		}
		if err := validateReportProse(prefix+".action", act.Action, 1, MaxReportActionRunes); err != nil {
			issues = append(issues, err.Error())
		}
		issues = append(issues, validateRelatedFindingIDs(prefix, act.RelatedFindingIds, declaredFindingIDs)...)
	}
	return issues
}

func validateRelatedFindingIDs(prefix string, relatedIDs []string, declaredFindingIDs map[string]bool) []string {
	var issues []string
	if relatedIDs == nil {
		issues = append(issues, fmt.Sprintf("%s.relatedFindingIds array is required", prefix))
	}
	if len(relatedIDs) > MaxReportRelatedFindings {
		issues = append(issues, fmt.Sprintf("%s.relatedFindingIds has %d items (max %d)", prefix, len(relatedIDs), MaxReportRelatedFindings))
	}
	seenRel := make(map[string]bool, len(relatedIDs))
	for j, fid := range relatedIDs {
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
	return issues
}

func validateReportDataGaps(dataGaps []string) []string {
	var issues []string
	if len(dataGaps) > MaxReportDataGaps {
		issues = append(issues, fmt.Sprintf("dataGaps has %d items (max %d)", len(dataGaps), MaxReportDataGaps))
	}
	for i, gap := range dataGaps {
		if err := validateReportProse(fmt.Sprintf("dataGaps[%d]", i), gap, 1, MaxReportDataGapRunes); err != nil {
			issues = append(issues, err.Error())
		}
	}
	return issues
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
	if proseHTMLTagPattern.MatchString(val) || proseEventAttrPattern.MatchString(val) {
		return fmt.Errorf("%s must not contain HTML markup or event handlers", field)
	}
	if proseDataURIPattern.MatchString(val) {
		return fmt.Errorf("%s contains forbidden data: URI scheme", field)
	}
	lower := strings.ToLower(val)
	for _, frag := range forbiddenReportProseSchemes {
		if strings.Contains(lower, frag) {
			return fmt.Errorf("%s contains forbidden markup or URL scheme %q", field, frag)
		}
	}
	return nil
}

func hasVisibleText(s string) bool {
	for _, r := range s {
		if !unicode.IsControl(r) && !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// ValidateAnalyzePayload validates pre-flight invariants on AnalyzeStatusPayload and returns
// the authoritative set of citable evidence IDs present in the request.
func ValidateAnalyzePayload(p AnalyzeStatusPayload) (map[string]bool, error) {
	if err := validateAnalyzeHeader(p); err != nil {
		return nil, err
	}
	if err := validateAnalyzeDashboardContext(p.DashboardContext); err != nil {
		return nil, err
	}
	if err := validateAnalyzeEvidenceCounts(p); err != nil {
		return nil, err
	}

	allowedIDs := make(map[string]bool, len(p.Evidence.Alerts)+len(p.Evidence.Rules)+len(p.Evidence.Metrics)+len(p.Evidence.StatusNotes)+len(p.CollectionWarnings))
	if err := validateAlertEvidences(p.Evidence.Alerts, allowedIDs); err != nil {
		return nil, err
	}
	if err := validateRuleEvidences(p.Evidence.Rules, allowedIDs); err != nil {
		return nil, err
	}
	if err := validateMetricEvidences(p.Evidence.Metrics, allowedIDs); err != nil {
		return nil, err
	}
	if err := validateStatusNoteEvidences(p.Evidence.StatusNotes, allowedIDs); err != nil {
		return nil, err
	}
	if err := validateCollectionWarnings(p.CollectionWarnings, allowedIDs); err != nil {
		return nil, err
	}

	if len(allowedIDs) == 0 {
		return nil, fmt.Errorf("%w: at least one evidence item or status note is required", ErrInsufficientEvidence)
	}
	return allowedIDs, nil
}

func validateAnalyzeHeader(p AnalyzeStatusPayload) error {
	if !allowedReportWindows[p.Window] {
		return fmt.Errorf("%w: unsupported window %q", ErrInvalidAnalyzeRequest, p.Window)
	}
	if !allowedBaselineModes[p.BaselineMode] {
		return fmt.Errorf("%w: unsupported baselineMode %q", ErrInvalidAnalyzeRequest, p.BaselineMode)
	}
	if !allowedReportLanguages[p.Language] {
		return fmt.Errorf("%w: unsupported language %q", ErrInvalidAnalyzeRequest, p.Language)
	}
	if p.Scope != nil {
		if err := validateScopeFilter(p.Scope); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
	}
	if !utf8.ValidString(p.UserFocus) {
		return fmt.Errorf("%w: userFocus contains invalid UTF-8", ErrInvalidAnalyzeRequest)
	}
	if utf8.RuneCountInString(p.UserFocus) > MaxAnalyzeFocusRunes {
		return fmt.Errorf("%w: userFocus exceeds max %d runes", ErrInvalidAnalyzeRequest, MaxAnalyzeFocusRunes)
	}
	if err := validateRFC3339("evidence.collectedAt", p.Evidence.CollectedAt, true); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
	}
	if p.CollectionStatus.TotalRulesEvaluated < 0 || p.CollectionStatus.ActiveAlertsTotal < 0 || p.CollectionStatus.AbnormalRulesTotal < 0 {
		return fmt.Errorf("%w: collectionStatus counters must be non-negative", ErrInvalidAnalyzeRequest)
	}
	if !p.CollectionStatus.AlertsChecked && !p.CollectionStatus.RulesChecked && len(p.Evidence.Metrics) == 0 {
		return fmt.Errorf("%w: alerting and dashboard metric collection all failed or returned no sources", ErrInsufficientEvidence)
	}
	return nil
}

func validateAnalyzeDashboardContext(ctx *ReportDashboardContext) error {
	if ctx == nil {
		return nil
	}
	if !hasVisibleText(ctx.Title) || utf8.RuneCountInString(ctx.Title) > MaxTitleLen {
		return fmt.Errorf("%w: dashboardContext.title must be 1..%d runes", ErrInvalidAnalyzeRequest, MaxTitleLen)
	}
	if ctx.UID != "" && utf8.RuneCountInString(ctx.UID) > 64 {
		return fmt.Errorf("%w: dashboardContext.uid exceeds 64 runes", ErrInvalidAnalyzeRequest)
	}
	if ctx.PanelCount < 0 || ctx.AnalyzedPanelCount < 0 {
		return fmt.Errorf("%w: dashboardContext panel counts must be non-negative", ErrInvalidAnalyzeRequest)
	}
	if ctx.AnalyzedPanelCount > ctx.PanelCount {
		return fmt.Errorf("%w: dashboardContext.analyzedPanelCount (%d) exceeds panelCount (%d)", ErrInvalidAnalyzeRequest, ctx.AnalyzedPanelCount, ctx.PanelCount)
	}
	return nil
}

func validateAnalyzeEvidenceCounts(p AnalyzeStatusPayload) error {
	if len(p.Evidence.Alerts) > MaxAlertEvidenceItems {
		return fmt.Errorf("%w: evidence.alerts has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.Alerts), MaxAlertEvidenceItems)
	}
	if len(p.Evidence.Rules) > MaxRuleEvidenceItems {
		return fmt.Errorf("%w: evidence.rules has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.Rules), MaxRuleEvidenceItems)
	}
	if len(p.Evidence.Metrics) > MaxMetricEvidenceItems {
		return fmt.Errorf("%w: evidence.metrics has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.Metrics), MaxMetricEvidenceItems)
	}
	if len(p.Evidence.StatusNotes) > MaxStatusNoteItems {
		return fmt.Errorf("%w: evidence.statusNotes has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.Evidence.StatusNotes), MaxStatusNoteItems)
	}
	if len(p.CollectionWarnings) > MaxCollectionWarnings {
		return fmt.Errorf("%w: collectionWarnings has %d items (max %d)", ErrInvalidAnalyzeRequest, len(p.CollectionWarnings), MaxCollectionWarnings)
	}
	return nil
}

func registerEvidenceID(allowedIDs map[string]bool, field, id string, pat *regexp.Regexp) error {
	if !pat.MatchString(id) {
		return fmt.Errorf("%w: %s %q has invalid ID format", ErrInvalidAnalyzeRequest, field, id)
	}
	if allowedIDs[id] {
		return fmt.Errorf("%w: duplicate evidence ID %q", ErrInvalidAnalyzeRequest, id)
	}
	allowedIDs[id] = true
	return nil
}

func validateAlertEvidences(alerts []AlertEvidenceItem, allowedIDs map[string]bool) error {
	for i, a := range alerts {
		prefix := fmt.Sprintf("evidence.alerts[%d]", i)
		if err := registerEvidenceID(allowedIDs, prefix+".id", a.ID, alertEvidenceIDPattern); err != nil {
			return err
		}
		if !hasVisibleText(a.AlertName) || utf8.RuneCountInString(a.AlertName) > 128 {
			return fmt.Errorf("%w: %s.alertName must be 1..128 runes", ErrInvalidAnalyzeRequest, prefix)
		}
		if !allowedAlertStates[a.State] {
			return fmt.Errorf("%w: %s.state %q not allowed", ErrInvalidAnalyzeRequest, prefix, a.State)
		}
		if err := validateRFC3339(prefix+".startsAt", a.StartsAt, false); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if err := validateEvidenceLabels(prefix, a.Labels); err != nil {
			return err
		}
	}
	return nil
}

func validateEvidenceLabels(prefix string, labels map[string]string) error {
	if len(labels) > MaxEvidenceLabels {
		return fmt.Errorf("%w: %s.labels has %d entries (max %d)", ErrInvalidAnalyzeRequest, prefix, len(labels), MaxEvidenceLabels)
	}
	for k, v := range labels {
		if !labelNamePattern.MatchString(k) {
			return fmt.Errorf("%w: %s.labels key %q invalid", ErrInvalidAnalyzeRequest, prefix, k)
		}
		if utf8.RuneCountInString(v) > MaxEvidenceLabelValRune {
			return fmt.Errorf("%w: %s.labels[%q] exceeds %d runes", ErrInvalidAnalyzeRequest, prefix, k, MaxEvidenceLabelValRune)
		}
	}
	return nil
}

func validateRuleEvidences(rules []RuleEvidenceItem, allowedIDs map[string]bool) error {
	for i, r := range rules {
		prefix := fmt.Sprintf("evidence.rules[%d]", i)
		if err := registerEvidenceID(allowedIDs, prefix+".id", r.ID, ruleEvidenceIDPattern); err != nil {
			return err
		}
		if !hasVisibleText(r.RuleName) || utf8.RuneCountInString(r.RuleName) > 128 {
			return fmt.Errorf("%w: %s.ruleName must be 1..128 runes", ErrInvalidAnalyzeRequest, prefix)
		}
		if !allowedRuleStates[r.State] {
			return fmt.Errorf("%w: %s.state %q not allowed", ErrInvalidAnalyzeRequest, prefix, r.State)
		}
		if !allowedRuleHealths[r.Health] {
			return fmt.Errorf("%w: %s.health %q not allowed", ErrInvalidAnalyzeRequest, prefix, r.Health)
		}
		if r.ActiveAlertsCount < 0 {
			return fmt.Errorf("%w: %s.activeAlertsCount must be non-negative", ErrInvalidAnalyzeRequest, prefix)
		}
	}
	return nil
}

func validateMetricEvidences(metrics []MetricEvidenceItem, allowedIDs map[string]bool) error {
	for i, m := range metrics {
		prefix := fmt.Sprintf("evidence.metrics[%d]", i)
		if err := registerEvidenceID(allowedIDs, prefix+".id", m.ID, metricEvidenceIDPat); err != nil {
			return err
		}
		if !hasVisibleText(m.PanelTitle) || utf8.RuneCountInString(m.PanelTitle) > MaxTitleLen {
			return fmt.Errorf("%w: %s.panelTitle must be 1..%d runes", ErrInvalidAnalyzeRequest, prefix, MaxTitleLen)
		}
		if !allowedMetricQueryStatuses[m.QueryStatus] {
			return fmt.Errorf("%w: %s.queryStatus %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.QueryStatus)
		}
		if !allowedMetricTrends[m.Trend] {
			return fmt.Errorf("%w: %s.trend %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.Trend)
		}
		if !allowedThresholdBreaches[m.ThresholdBreached] {
			return fmt.Errorf("%w: %s.thresholdBreached %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.ThresholdBreached)
		}
		if !allowedAnomalyHints[m.AnomalyHint] {
			return fmt.Errorf("%w: %s.anomalyHint %q not allowed", ErrInvalidAnalyzeRequest, prefix, m.AnomalyHint)
		}
		if err := validateMetricStats(prefix, m); err != nil {
			return err
		}
	}
	return nil
}

func validateMetricStats(prefix string, m MetricEvidenceItem) error {
	if m.Current != nil {
		if err := validateWindowStats(prefix+".current", m.Current); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
	}
	if m.Baseline != nil {
		if err := checkFiniteFloat(prefix+".baseline.avg", m.Baseline.Avg); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if err := checkFiniteFloat(prefix+".baseline.p95", m.Baseline.P95); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
		if err := checkFiniteFloat(prefix+".baseline.max", m.Baseline.Max); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
	}
	if m.DeltaPctAvg != nil {
		if err := checkFiniteFloat(prefix+".deltaPctAvg", *m.DeltaPctAvg); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
	}
	if m.DeltaPctP95 != nil {
		if err := checkFiniteFloat(prefix+".deltaPctP95", *m.DeltaPctP95); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidAnalyzeRequest, err)
		}
	}
	return nil
}

func validateStatusNoteEvidences(notes []StatusNoteEvidenceItem, allowedIDs map[string]bool) error {
	for i, s := range notes {
		prefix := fmt.Sprintf("evidence.statusNotes[%d]", i)
		if err := registerEvidenceID(allowedIDs, prefix+".id", s.ID, statusEvidenceIDPat); err != nil {
			return err
		}
		if strings.TrimSpace(s.Message) == "" || utf8.RuneCountInString(s.Message) > MaxEvidenceSummaryRunes {
			return fmt.Errorf("%w: %s.message must be 1..%d runes", ErrInvalidAnalyzeRequest, prefix, MaxEvidenceSummaryRunes)
		}
	}
	return nil
}

func validateCollectionWarnings(warnings []CollectionWarningItem, allowedIDs map[string]bool) error {
	for i, w := range warnings {
		prefix := fmt.Sprintf("collectionWarnings[%d]", i)
		if err := registerEvidenceID(allowedIDs, prefix+".id", w.ID, warnEvidenceIDPattern); err != nil {
			return err
		}
		if !allowedCollectionWarningCodes[w.Code] {
			return fmt.Errorf("%w: %s.code %q not allowed", ErrInvalidAnalyzeRequest, prefix, w.Code)
		}
		if strings.TrimSpace(w.Message) == "" || utf8.RuneCountInString(w.Message) > MaxEvidenceSummaryRunes {
			return fmt.Errorf("%w: %s.message must be 1..%d runes", ErrInvalidAnalyzeRequest, prefix, MaxEvidenceSummaryRunes)
		}
	}
	return nil
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
