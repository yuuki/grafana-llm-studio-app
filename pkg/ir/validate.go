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
	"unicode"
	"unicode/utf8"
)

var (
	panelIDPattern        = regexp.MustCompile(`^panel-[a-z0-9][a-z0-9-]{0,31}$`)
	varNamePattern        = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,31}$`)
	labelNamePattern      = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)
	metricPattern         = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]{0,127}$`)
	htmlTagPattern        = regexp.MustCompile(`<[^>]*>`)
	eventAttrPattern      = regexp.MustCompile(`(?i)\bon[a-z]+\s*=`)
	legendVarTokenPattern = regexp.MustCompile(`(?:\$(?:\{|[a-zA-Z_])|\[\[[a-zA-Z_])`)

	allowedSchemaVersions = map[string]bool{
		"1.0": true,
		"1.1": true,
	}

	allowedTimeFrom = map[string]bool{
		"now-5m":  true,
		"now-15m": true,
		"now-30m": true,
		"now-1h":  true,
		"now-3h":  true,
		"now-6h":  true,
	}

	allowedPanelTypes = map[string]bool{
		"timeseries":     true,
		"stat":           true,
		"table":          true,
		"gauge":          true,
		"bargauge":       true,
		"piechart":       true,
		"state-timeline": true,
	}

	allowedUnits = map[string]bool{
		"short":       true,
		"none":        true,
		"percent":     true,
		"percentunit": true,
		"reqps":       true,
		"ops":         true,
		"s":           true,
		"ms":          true,
		"bytes":       true,
		"decbytes":    true,
		"Bps":         true,
	}

	allowedThresholdColors = map[string]bool{
		"green":  true,
		"yellow": true,
		"orange": true,
		"red":    true,
		"blue":   true,
	}

	allowedWidths = map[string]bool{
		"half": true,
		"full": true,
	}

	allowedQueryRefIDs = map[string]bool{
		"A": true,
		"B": true,
		"C": true,
	}

	allowedQueryKinds = map[string]bool{
		"promql":    true,
		"metricsql": true,
	}

	allowedVariableTypes = map[string]bool{
		"query":  true,
		"custom": true,
	}

	allowedPatchOps = map[string]bool{
		"update_panel":    true,
		"add_panel":       true,
		"remove_panel":    true,
		"update_meta":     true,
		"add_variable":    true,
		"update_variable": true,
		"remove_variable": true,
	}

	reservedVariableNames = map[string]bool{
		"timeFilter": true,
		"timeFrom":   true,
		"timeTo":     true,
	}

	forbiddenMarkupFragments = []string{
		"<script",
		"</script",
		"<iframe",
		"<svg",
		"<object",
		"javascript:",
		"vbscript:",
		"data:text/html",
	}
)

// IsReservedVariableName returns true for Grafana global/built-in variable identifiers
// (all "__*" names plus timeFilter/timeFrom/timeTo).
func IsReservedVariableName(name string) bool {
	return strings.HasPrefix(name, "__") || reservedVariableNames[name]
}

// ValidationError aggregates deterministic IR validation issues.
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("IR validation failed (%d issue(s)): %s", len(e.Issues), strings.Join(e.Issues, "; "))
}

type rawThresholdCheck struct {
	Value *float64 `json:"value"`
}

type rawLayoutCheck struct {
	Row *int `json:"row"`
	Col *int `json:"col"`
}

// rawIRRequiredCheck ensures required numeric fields (layout.row, layout.col, thresholds[].value)
// are explicitly present in JSON rather than silently defaulted to Go zero values, and detects
// mutually exclusive query + queries (including empty queries: []) on the same panel.
type rawIRRequiredCheck struct {
	Revision *int `json:"revision"`
	Panels   []struct {
		ID         string              `json:"id"`
		Query      *json.RawMessage    `json:"query"`
		Queries    *json.RawMessage    `json:"queries"`
		Thresholds []rawThresholdCheck `json:"thresholds"`
		Layout     *rawLayoutCheck     `json:"layout"`
	} `json:"panels"`
}

type rawPatchRequiredCheck struct {
	BaseRevision *int `json:"baseRevision"`
	Operations   []struct {
		Panel *struct {
			Query      *json.RawMessage    `json:"query"`
			Queries    *json.RawMessage    `json:"queries"`
			Thresholds []rawThresholdCheck `json:"thresholds"`
			Layout     *rawLayoutCheck     `json:"layout"`
		} `json:"panel"`
	} `json:"operations"`
}

func isJSONNull(raw *json.RawMessage) bool {
	if raw == nil {
		return true
	}
	return bytes.Equal(bytes.TrimSpace(*raw), []byte("null"))
}

func isEmptyJSONArray(raw *json.RawMessage) bool {
	if isJSONNull(raw) {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(*raw, &arr); err != nil {
		return false
	}
	return len(arr) == 0
}

// DecodeStrictIR parses JSON bytes into DashboardIR, rejecting unknown fields,
// missing required numeric fields, trailing garbage, query/queries conflicts, or oversized payloads.
func DecodeStrictIR(raw []byte) (DashboardIR, error) {
	if len(raw) == 0 {
		return DashboardIR{}, errors.New("empty IR payload")
	}
	if len(raw) > MaxIRPayloadBytes {
		return DashboardIR{}, fmt.Errorf("IR payload exceeds limit (%d > %d bytes)", len(raw), MaxIRPayloadBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var doc DashboardIR
	if err := dec.Decode(&doc); err != nil {
		return DashboardIR{}, fmt.Errorf("strict IR JSON decode failed: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return DashboardIR{}, errors.New("unexpected trailing tokens after IR JSON object")
	}

	var req rawIRRequiredCheck
	if err := json.Unmarshal(raw, &req); err != nil {
		return DashboardIR{}, fmt.Errorf("IR required field inspection failed: %w", err)
	}
	if req.Revision == nil {
		return DashboardIR{}, errors.New("missing required field: revision")
	}
	for i, p := range req.Panels {
		hasRawQuery := !isJSONNull(p.Query)
		hasRawQueries := !isJSONNull(p.Queries)
		if hasRawQuery && hasRawQueries {
			return DashboardIR{}, fmt.Errorf("panels[%d]: cannot specify both query and queries", i)
		}
		if hasRawQueries && isEmptyJSONArray(p.Queries) {
			return DashboardIR{}, fmt.Errorf("panels[%d]: queries must contain 1..%d items", i, MaxQueriesPerPanel)
		}
		if p.Layout == nil || p.Layout.Row == nil || p.Layout.Col == nil {
			return DashboardIR{}, fmt.Errorf("panels[%d]: missing required layout.row or layout.col", i)
		}
		for ti, th := range p.Thresholds {
			if th.Value == nil {
				return DashboardIR{}, fmt.Errorf("panels[%d].thresholds[%d]: missing required value", i, ti)
			}
		}
	}
	return doc, nil
}

// DecodeStrictPatch parses JSON bytes into DashboardPatch with DisallowUnknownFields
// and strict required numeric field checks for baseRevision, layout, and thresholds.
func DecodeStrictPatch(raw []byte) (DashboardPatch, error) {
	if len(raw) == 0 {
		return DashboardPatch{}, errors.New("empty patch payload")
	}
	if len(raw) > MaxIRPayloadBytes {
		return DashboardPatch{}, fmt.Errorf("patch payload exceeds limit (%d > %d bytes)", len(raw), MaxIRPayloadBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var patch DashboardPatch
	if err := dec.Decode(&patch); err != nil {
		return DashboardPatch{}, fmt.Errorf("strict patch JSON decode failed: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return DashboardPatch{}, errors.New("unexpected trailing tokens after patch JSON object")
	}

	var req rawPatchRequiredCheck
	if err := json.Unmarshal(raw, &req); err != nil {
		return DashboardPatch{}, fmt.Errorf("patch required field inspection failed: %w", err)
	}
	if req.BaseRevision == nil {
		return DashboardPatch{}, errors.New("missing required field: baseRevision")
	}
	for i, op := range req.Operations {
		if op.Panel == nil {
			continue
		}
		hasRawQuery := !isJSONNull(op.Panel.Query)
		hasRawQueries := !isJSONNull(op.Panel.Queries)
		if hasRawQuery && hasRawQueries {
			return DashboardPatch{}, fmt.Errorf("operations[%d].panel: cannot specify both query and queries", i)
		}
		if hasRawQueries && isEmptyJSONArray(op.Panel.Queries) {
			return DashboardPatch{}, fmt.Errorf("operations[%d].panel: queries must contain 1..%d items", i, MaxQueriesPerPanel)
		}
		if op.Panel.Layout != nil && (op.Panel.Layout.Row == nil || op.Panel.Layout.Col == nil) {
			return DashboardPatch{}, fmt.Errorf("operations[%d].panel: missing required layout.row or layout.col", i)
		}
		for ti, th := range op.Panel.Thresholds {
			if th.Value == nil {
				return DashboardPatch{}, fmt.Errorf("operations[%d].panel.thresholds[%d]: missing required value", i, ti)
			}
		}
	}
	return patch, nil
}

// ValidateOptions controls deterministic IR validation constraints.
type ValidateOptions struct {
	MaxPanels int
}

// ValidateIR performs deterministic schema and invariant validation on DashboardIR.
// PromQL AST/catalog/variable validation is layered on top via pkg/promql.
func ValidateIR(doc DashboardIR, opts ValidateOptions) error {
	maxPanels := opts.MaxPanels
	if maxPanels <= 0 || maxPanels > MaxPanelsHardCap {
		maxPanels = DefaultMaxPanels
	}

	issues := validateDashboardMeta(doc)

	if len(doc.Variables) > MaxVariables {
		issues = append(issues, fmt.Sprintf("variables count %d exceeds max %d", len(doc.Variables), MaxVariables))
	}
	seenVars := make(map[string]bool, len(doc.Variables))
	for i, v := range doc.Variables {
		issues = append(issues, validateVariable(i, v, seenVars)...)
	}

	if len(doc.Panels) == 0 {
		issues = append(issues, "panels must contain at least 1 panel")
	}
	if len(doc.Panels) > maxPanels {
		issues = append(issues, fmt.Sprintf("panels count %d exceeds maxPanels %d", len(doc.Panels), maxPanels))
	}

	seenIDs := make(map[string]bool, len(doc.Panels))
	occupiedCells := make(map[string]string, len(doc.Panels)*2)
	for i, p := range doc.Panels {
		issues = append(issues, validatePanel(i, p, seenIDs, occupiedCells)...)
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

func validateDashboardMeta(doc DashboardIR) []string {
	var issues []string
	if !allowedSchemaVersions[doc.SchemaVersion] {
		issues = append(issues, fmt.Sprintf("schemaVersion %q is not allowed (allowed: 1.0, 1.1)", doc.SchemaVersion))
	}
	if doc.Revision < 1 || doc.Revision > MaxRevision {
		issues = append(issues, fmt.Sprintf("revision must be between 1 and %d, got %d", MaxRevision, doc.Revision))
	}
	if err := validatePlainText("title", doc.Title, 1, MaxTitleLen); err != nil {
		issues = append(issues, err.Error())
	}
	if doc.Description != "" {
		if err := validatePlainText("description", doc.Description, 0, MaxDescLen); err != nil {
			issues = append(issues, err.Error())
		}
	}
	if !allowedTimeFrom[doc.TimeRange.From] {
		issues = append(issues, fmt.Sprintf("timeRange.from %q is not in allowlist", doc.TimeRange.From))
	}
	if doc.TimeRange.To != "now" {
		issues = append(issues, fmt.Sprintf("timeRange.to must be %q, got %q", "now", doc.TimeRange.To))
	}
	return issues
}

func validatePanel(idx int, p PanelIR, seenIDs map[string]bool, occupiedCells map[string]string) []string {
	var issues []string
	prefix := fmt.Sprintf("panels[%d]", idx)
	if !panelIDPattern.MatchString(p.ID) {
		issues = append(issues, fmt.Sprintf("%s.id %q violates pattern", prefix, p.ID))
	} else if seenIDs[p.ID] {
		issues = append(issues, fmt.Sprintf("%s.id %q is duplicated", prefix, p.ID))
	} else {
		seenIDs[p.ID] = true
	}

	if !allowedPanelTypes[p.Type] {
		issues = append(issues, fmt.Sprintf("%s.type %q is not allowed", prefix, p.Type))
	}
	if err := validatePlainText(prefix+".title", p.Title, 1, MaxPanelTitleLen); err != nil {
		issues = append(issues, err.Error())
	}
	if p.Description != "" {
		if err := validatePlainText(prefix+".description", p.Description, 0, MaxPanelDescLen); err != nil {
			issues = append(issues, err.Error())
		}
	}
	if p.DatasourceRef != SelectedDatasourceRef {
		issues = append(issues, fmt.Sprintf("%s.datasourceRef must be %q, got %q", prefix, SelectedDatasourceRef, p.DatasourceRef))
	}

	issues = append(issues, validatePanelQueries(prefix, p)...)

	if !allowedUnits[p.Unit] {
		issues = append(issues, fmt.Sprintf("%s.unit %q is not in allowlist", prefix, p.Unit))
	}
	issues = append(issues, validatePanelThresholds(prefix, p.Thresholds)...)
	issues = append(issues, validatePanelLayout(prefix, p.ID, p.Layout, occupiedCells)...)
	return issues
}

func validatePanelThresholds(prefix string, thresholds []ThresholdIR) []string {
	var issues []string
	if len(thresholds) > MaxThresholds {
		issues = append(issues, fmt.Sprintf("%s.thresholds count %d exceeds limit %d", prefix, len(thresholds), MaxThresholds))
	}
	prevVal := math.Inf(-1)
	for ti, th := range thresholds {
		if math.IsNaN(th.Value) || math.IsInf(th.Value, 0) || th.Value < -1e9 || th.Value > 1e9 {
			issues = append(issues, fmt.Sprintf("%s.thresholds[%d].value out of range", prefix, ti))
		}
		if th.Value < prevVal {
			issues = append(issues, fmt.Sprintf("%s.thresholds must be sorted ascending", prefix))
		}
		prevVal = th.Value
		if !allowedThresholdColors[th.Color] {
			issues = append(issues, fmt.Sprintf("%s.thresholds[%d].color %q is not allowed", prefix, ti, th.Color))
		}
	}
	return issues
}

func validatePanelLayout(prefix, panelID string, layout LayoutHint, occupiedCells map[string]string) []string {
	var issues []string
	if layout.Row < 0 || layout.Row > 5 {
		issues = append(issues, fmt.Sprintf("%s.layout.row %d out of range 0..5", prefix, layout.Row))
	}
	if layout.Col != 0 && layout.Col != 1 {
		issues = append(issues, fmt.Sprintf("%s.layout.col %d must be 0 or 1", prefix, layout.Col))
	}
	if !allowedWidths[layout.Width] {
		issues = append(issues, fmt.Sprintf("%s.layout.width %q must be half or full", prefix, layout.Width))
	}
	if layout.Width == "full" && layout.Col != 0 {
		issues = append(issues, fmt.Sprintf("%s.layout.col must be 0 when width is full", prefix))
	}
	if layout.Row < 0 || layout.Row > 5 || (layout.Col != 0 && layout.Col != 1) || !allowedWidths[layout.Width] {
		return issues
	}
	cols := []int{layout.Col}
	if layout.Width == "full" {
		cols = []int{0, 1}
	}
	for _, c := range cols {
		key := fmt.Sprintf("%d:%d", layout.Row, c)
		if other, exists := occupiedCells[key]; exists {
			issues = append(issues, fmt.Sprintf("%s.layout overlaps with panel %q at row=%d col=%d", prefix, other, layout.Row, c))
		} else {
			occupiedCells[key] = panelID
		}
	}
	return issues
}

func validatePanelQueries(prefix string, p PanelIR) []string {
	var issues []string
	hasSingle := p.Query != (QueryIR{})
	hasMulti := len(p.Queries) > 0

	if hasSingle && hasMulti {
		return []string{fmt.Sprintf("%s cannot specify both query and queries", prefix)}
	}
	if !hasSingle && !hasMulti {
		return []string{fmt.Sprintf("%s must specify query or 1..%d queries", prefix, MaxQueriesPerPanel)}
	}

	qs := p.EffectiveQueries()
	if len(qs) > MaxQueriesPerPanel {
		issues = append(issues, fmt.Sprintf("%s.queries count %d exceeds limit %d", prefix, len(qs), MaxQueriesPerPanel))
	}

	seenRefIDs := make(map[string]bool, len(qs))
	for qi, q := range qs {
		qPrefix := fmt.Sprintf("%s.query", prefix)
		if hasMulti {
			qPrefix = fmt.Sprintf("%s.queries[%d]", prefix, qi)
		}
		refID := q.RefID
		if refID != "" {
			if !allowedQueryRefIDs[refID] {
				issues = append(issues, fmt.Sprintf("%s.refId %q is not allowed (allowed: A, B, C)", qPrefix, refID))
			}
		} else if qi < len(defaultQueryRefIDs) {
			refID = defaultQueryRefIDs[qi]
		}
		if refID != "" {
			if seenRefIDs[refID] {
				issues = append(issues, fmt.Sprintf("%s.refId %q is duplicated", qPrefix, refID))
			}
			seenRefIDs[refID] = true
		}
		if q.Kind != "" && !allowedQueryKinds[q.Kind] {
			issues = append(issues, fmt.Sprintf("%s.kind %q is not allowed (allowed: promql, metricsql)", qPrefix, q.Kind))
		}
		if err := validateExprText(qPrefix+".expr", q.Expr, 1, MaxExprLen); err != nil {
			issues = append(issues, err.Error())
		}
		if q.LegendFormat != "" {
			if err := validatePlainText(qPrefix+".legendFormat", q.LegendFormat, 0, MaxLegendLen); err != nil {
				issues = append(issues, err.Error())
			} else if legendVarTokenPattern.MatchString(q.LegendFormat) {
				issues = append(issues, fmt.Sprintf("%s.legendFormat must not reference template variables ($ or [[...]])", qPrefix))
			}
		}
	}
	return issues
}

func validateVariable(idx int, v VariableIR, seen map[string]bool) []string {
	var issues []string
	prefix := fmt.Sprintf("variables[%d]", idx)

	if !varNamePattern.MatchString(v.Name) {
		issues = append(issues, fmt.Sprintf("%s.name %q violates pattern", prefix, v.Name))
	} else if IsReservedVariableName(v.Name) {
		issues = append(issues, fmt.Sprintf("%s.name %q is reserved", prefix, v.Name))
	} else if seen[v.Name] {
		issues = append(issues, fmt.Sprintf("%s.name %q is duplicated", prefix, v.Name))
	} else {
		seen[v.Name] = true
	}

	if v.Label != "" {
		if err := validatePlainText(prefix+".label", v.Label, 0, MaxVariableLabelLen); err != nil {
			issues = append(issues, err.Error())
		}
	}

	if !allowedVariableTypes[v.Type] {
		issues = append(issues, fmt.Sprintf("%s.type %q is not allowed (allowed: query, custom)", prefix, v.Type))
		return issues
	}

	switch v.Type {
	case "query":
		issues = append(issues, validateQueryVariable(prefix, v)...)
	case "custom":
		issues = append(issues, validateCustomVariable(prefix, v)...)
	}

	return issues
}

func validateQueryVariable(prefix string, v VariableIR) []string {
	var issues []string
	if v.DatasourceRef != SelectedDatasourceRef {
		issues = append(issues, fmt.Sprintf("%s.datasourceRef must be %q for query variable, got %q", prefix, SelectedDatasourceRef, v.DatasourceRef))
	}
	if !labelNamePattern.MatchString(v.LabelName) || v.LabelName == "__name__" {
		issues = append(issues, fmt.Sprintf("%s.labelName %q is invalid for query variable", prefix, v.LabelName))
	}
	if v.MetricName != "" && !metricPattern.MatchString(v.MetricName) {
		issues = append(issues, fmt.Sprintf("%s.metricName %q violates Prometheus metric pattern", prefix, v.MetricName))
	}
	if len(v.Options) > 0 {
		issues = append(issues, fmt.Sprintf("%s.options must not be set on query variable", prefix))
	}
	if err := validateVariableValue(prefix+".defaultValue", v.DefaultValue, 1, MaxVariableDefaultLen, false); err != nil {
		issues = append(issues, err.Error())
	}
	return issues
}

func validateCustomVariable(prefix string, v VariableIR) []string {
	var issues []string
	if v.DatasourceRef != "" {
		issues = append(issues, fmt.Sprintf("%s.datasourceRef must be empty for custom variable", prefix))
	}
	if v.LabelName != "" || v.MetricName != "" {
		issues = append(issues, fmt.Sprintf("%s.labelName and metricName must be empty for custom variable", prefix))
	}
	if len(v.Options) == 0 || len(v.Options) > MaxVariableOptions {
		issues = append(issues, fmt.Sprintf("%s.options count %d must be between 1 and %d", prefix, len(v.Options), MaxVariableOptions))
	}
	optSeen := make(map[string]bool, len(v.Options))
	for oi, opt := range v.Options {
		if err := validateVariableValue(fmt.Sprintf("%s.options[%d]", prefix, oi), opt, 1, MaxVariableOptionLen, true); err != nil {
			issues = append(issues, err.Error())
		}
		if optSeen[opt] {
			issues = append(issues, fmt.Sprintf("%s.options[%d] %q is duplicated", prefix, oi, opt))
		}
		optSeen[opt] = true
	}
	if v.DefaultValue != "" {
		if err := validateVariableValue(prefix+".defaultValue", v.DefaultValue, 1, MaxVariableDefaultLen, true); err != nil {
			issues = append(issues, err.Error())
		} else if !optSeen[v.DefaultValue] {
			issues = append(issues, fmt.Sprintf("%s.defaultValue %q must be one of custom options", prefix, v.DefaultValue))
		}
	}
	return issues
}

// validateVariableValue ensures variable defaultValue and custom options cannot break out of
// PromQL string literals, trigger chained variable expansion, or corrupt Grafana CSV custom syntax.
func validateVariableValue(field, val string, minRunes, maxRunes int, isCustom bool) error {
	if err := validatePlainText(field, val, minRunes, maxRunes); err != nil {
		return err
	}
	if strings.ContainsAny(val, "\"'\\`$") || strings.Contains(val, "[[") {
		return fmt.Errorf("%s contains forbidden quote, backslash, backtick, or variable token", field)
	}
	if isCustom {
		if strings.Contains(val, ",") || strings.Contains(val, " : ") {
			return fmt.Errorf("%s must not contain ',' or ' : ' (reserved by Grafana custom variable syntax)", field)
		}
	}
	return nil
}

// ValidateImportSource checks provenance metadata for an imported Grafana dashboard.
func ValidateImportSource(src *ImportSource) error {
	if src == nil {
		return nil
	}
	if err := validatePlainText("importSource.uid", src.UID, 1, 64); err != nil {
		return err
	}
	if src.ResourceVersion != "" {
		if err := validatePlainText("importSource.resourceVersion", src.ResourceVersion, 1, 64); err != nil {
			return err
		}
	}
	if src.FolderUID != "" {
		if err := validatePlainText("importSource.folderUid", src.FolderUID, 1, 64); err != nil {
			return err
		}
	}
	if src.Title != "" {
		if err := validatePlainText("importSource.title", src.Title, 0, MaxTitleLen); err != nil {
			return err
		}
	}
	if src.Version < 0 || src.Version > 1000000 {
		return fmt.Errorf("importSource.version %d out of range", src.Version)
	}
	return nil
}

// validatePlainText validates Unicode rune length, valid UTF-8, absence of control/Bidi chars,
// and absence of HTML tags / event handler attributes / active schemes.
func validatePlainText(field, val string, minRunes, maxRunes int) error {
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
	if htmlTagPattern.MatchString(val) || eventAttrPattern.MatchString(val) {
		return fmt.Errorf("%s must not contain HTML markup or event handlers", field)
	}
	lower := strings.ToLower(val)
	for _, frag := range forbiddenMarkupFragments {
		if strings.Contains(lower, frag) {
			return fmt.Errorf("%s contains forbidden active content %q", field, frag)
		}
	}
	return nil
}

// validateExprText validates PromQL expression string length (runes) and forbids control/Bidi/active markup
// while allowing PromQL comparison operators (<, >).
func validateExprText(field, val string, minRunes, maxRunes int) error {
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
	lower := strings.ToLower(val)
	for _, frag := range forbiddenMarkupFragments {
		if strings.Contains(lower, frag) {
			return fmt.Errorf("%s contains forbidden active content %q", field, frag)
		}
	}
	return nil
}

func checkForbiddenRunes(field, val string) error {
	for _, r := range val {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains forbidden control character U+%04X", field, r)
		}
		if isBidiControl(r) {
			return fmt.Errorf("%s contains forbidden bidirectional control character U+%04X", field, r)
		}
	}
	return nil
}

func isBidiControl(r rune) bool {
	return r == 0x200E || r == 0x200F ||
		(r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2066 && r <= 0x2069)
}
