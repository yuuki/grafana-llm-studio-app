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
)

var (
	panelIDPattern = regexp.MustCompile(`^panel-[a-z0-9][a-z0-9-]{0,31}$`)

	allowedTimeFrom = map[string]bool{
		"now-5m":  true,
		"now-15m": true,
		"now-30m": true,
		"now-1h":  true,
		"now-3h":  true,
		"now-6h":  true,
	}

	allowedPanelTypes = map[string]bool{
		"timeseries": true,
		"stat":       true,
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

	forbiddenMarkupFragments = []string{
		"<script",
		"</script",
		"<iframe",
		"javascript:",
		"vbscript:",
		"onerror=",
		"onload=",
		"data:text/html",
	}
)

// ValidationError aggregates deterministic IR validation issues.
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("IR validation failed (%d issue(s)): %s", len(e.Issues), strings.Join(e.Issues, "; "))
}

// DecodeStrictIR parses JSON bytes into DashboardIR, rejecting unknown fields,
// trailing garbage, or oversized payloads.
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
	return doc, nil
}

// DecodeStrictPatch parses JSON bytes into DashboardPatch with DisallowUnknownFields.
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
	return patch, nil
}

// ValidateOptions controls deterministic IR validation constraints.
type ValidateOptions struct {
	MaxPanels int
}

// ValidateIR performs deterministic schema and invariant validation on DashboardIR.
// PromQL AST/catalog validation is layered on top via pkg/promql.
func ValidateIR(doc DashboardIR, opts ValidateOptions) error {
	maxPanels := opts.MaxPanels
	if maxPanels <= 0 || maxPanels > MaxPanelsHardCap {
		maxPanels = DefaultMaxPanels
	}

	var issues []string

	if doc.SchemaVersion != SchemaVersion {
		issues = append(issues, fmt.Sprintf("schemaVersion must be %q, got %q", SchemaVersion, doc.SchemaVersion))
	}
	if doc.Revision < 1 || doc.Revision > MaxRevision {
		issues = append(issues, fmt.Sprintf("revision must be between 1 and %d, got %d", MaxRevision, doc.Revision))
	}
	if err := validateSafeText("title", doc.Title, 1, MaxTitleLen); err != nil {
		issues = append(issues, err.Error())
	}
	if doc.Description != "" {
		if err := validateSafeText("description", doc.Description, 0, MaxDescLen); err != nil {
			issues = append(issues, err.Error())
		}
	}
	if !allowedTimeFrom[doc.TimeRange.From] {
		issues = append(issues, fmt.Sprintf("timeRange.from %q is not in allowlist", doc.TimeRange.From))
	}
	if doc.TimeRange.To != "now" {
		issues = append(issues, fmt.Sprintf("timeRange.to must be %q, got %q", "now", doc.TimeRange.To))
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
		prefix := fmt.Sprintf("panels[%d]", i)
		if !panelIDPattern.MatchString(p.ID) {
			issues = append(issues, fmt.Sprintf("%s.id %q violates pattern", prefix, p.ID))
		} else if seenIDs[p.ID] {
			issues = append(issues, fmt.Sprintf("%s.id %q is duplicated", prefix, p.ID))
		} else {
			seenIDs[p.ID] = true
		}

		if !allowedPanelTypes[p.Type] {
			issues = append(issues, fmt.Sprintf("%s.type %q is not allowed (allowed: timeseries, stat)", prefix, p.Type))
		}
		if err := validateSafeText(prefix+".title", p.Title, 1, MaxPanelTitleLen); err != nil {
			issues = append(issues, err.Error())
		}
		if p.Description != "" {
			if err := validateSafeText(prefix+".description", p.Description, 0, MaxPanelDescLen); err != nil {
				issues = append(issues, err.Error())
			}
		}
		if p.DatasourceRef != SelectedDatasourceRef {
			issues = append(issues, fmt.Sprintf("%s.datasourceRef must be %q, got %q", prefix, SelectedDatasourceRef, p.DatasourceRef))
		}
		if err := validateSafeText(prefix+".query.expr", p.Query.Expr, 1, MaxExprLen); err != nil {
			issues = append(issues, err.Error())
		}
		if p.Query.LegendFormat != "" {
			if err := validateSafeText(prefix+".query.legendFormat", p.Query.LegendFormat, 0, MaxLegendLen); err != nil {
				issues = append(issues, err.Error())
			}
		}
		if !allowedUnits[p.Unit] {
			issues = append(issues, fmt.Sprintf("%s.unit %q is not in allowlist", prefix, p.Unit))
		}
		if len(p.Thresholds) > MaxThresholds {
			issues = append(issues, fmt.Sprintf("%s.thresholds count %d exceeds limit %d", prefix, len(p.Thresholds), MaxThresholds))
		}
		prevVal := math.Inf(-1)
		for ti, th := range p.Thresholds {
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
		if p.Layout.Row < 0 || p.Layout.Row > 5 {
			issues = append(issues, fmt.Sprintf("%s.layout.row %d out of range 0..5", prefix, p.Layout.Row))
		}
		if p.Layout.Col != 0 && p.Layout.Col != 1 {
			issues = append(issues, fmt.Sprintf("%s.layout.col %d must be 0 or 1", prefix, p.Layout.Col))
		}
		if !allowedWidths[p.Layout.Width] {
			issues = append(issues, fmt.Sprintf("%s.layout.width %q must be half or full", prefix, p.Layout.Width))
		}
		if p.Layout.Width == "full" && p.Layout.Col != 0 {
			issues = append(issues, fmt.Sprintf("%s.layout.col must be 0 when width is full", prefix))
		}
		if p.Layout.Row >= 0 && p.Layout.Row <= 5 && (p.Layout.Col == 0 || p.Layout.Col == 1) && allowedWidths[p.Layout.Width] {
			cols := []int{p.Layout.Col}
			if p.Layout.Width == "full" {
				cols = []int{0, 1}
			}
			for _, c := range cols {
				key := fmt.Sprintf("%d:%d", p.Layout.Row, c)
				if other, exists := occupiedCells[key]; exists {
					issues = append(issues, fmt.Sprintf("%s.layout overlaps with panel %q at row=%d col=%d", prefix, other, p.Layout.Row, c))
				} else {
					occupiedCells[key] = p.ID
				}
			}
		}
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

func validateSafeText(field, val string, minLen, maxLen int) error {
	trimmed := strings.TrimSpace(val)
	if len(trimmed) < minLen {
		return fmt.Errorf("%s must not be empty", field)
	}
	if len(val) > maxLen {
		return fmt.Errorf("%s length %d exceeds max %d", field, len(val), maxLen)
	}
	lower := strings.ToLower(val)
	for _, frag := range forbiddenMarkupFragments {
		if strings.Contains(lower, frag) {
			return fmt.Errorf("%s contains forbidden active content %q", field, frag)
		}
	}
	return nil
}
