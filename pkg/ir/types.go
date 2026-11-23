package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	SchemaVersion         = "1.0"
	SelectedDatasourceRef = "selected_prometheus"

	DefaultMaxPanels = 6
	MaxPanelsHardCap = 6
	MaxQueriesPerPanel = 1
	MaxTitleLen      = 120
	MaxDescLen       = 400
	MaxPanelTitleLen = 80
	MaxPanelDescLen  = 240
	MaxExprLen       = 512
	MaxLegendLen     = 80
	MaxThresholds    = 4
	MaxRevision      = 10000
	MaxIRPayloadBytes = 64 * 1024
)

// DashboardIR is the restricted intermediate representation produced by the LLM.
type DashboardIR struct {
	SchemaVersion string    `json:"schemaVersion"`
	Revision      int       `json:"revision"`
	Title         string    `json:"title"`
	Description   string    `json:"description,omitempty"`
	TimeRange     TimeRange `json:"timeRange"`
	Panels        []PanelIR `json:"panels"`
}

// TimeRange defines a bounded relative dashboard time window.
type TimeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// PanelIR defines one visualization panel in the IR.
type PanelIR struct {
	ID            string        `json:"id"`
	Type          string        `json:"type"`
	Title         string        `json:"title"`
	Description   string        `json:"description,omitempty"`
	DatasourceRef string        `json:"datasourceRef"`
	Query         QueryIR       `json:"query"`
	Unit          string        `json:"unit"`
	Thresholds    []ThresholdIR `json:"thresholds,omitempty"`
	Layout        LayoutHint    `json:"layout"`
}

// QueryIR defines the single PromQL query for a panel.
type QueryIR struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat,omitempty"`
}

// ThresholdIR defines an absolute threshold step.
type ThresholdIR struct {
	Value float64 `json:"value"`
	Color string  `json:"color"`
}

// LayoutHint defines a deterministic 2-column grid hint.
type LayoutHint struct {
	Row   int    `json:"row"`
	Col   int    `json:"col"`
	Width string `json:"width"`
}

// DashboardPatch defines typed partial edit operations anchored to a baseRevision.
type DashboardPatch struct {
	SchemaVersion string           `json:"schemaVersion"`
	BaseRevision  int              `json:"baseRevision"`
	Summary       string           `json:"summary"`
	Operations    []PatchOperation `json:"operations"`
}

// PatchOperation is one atomic operation inside DashboardPatch.
type PatchOperation struct {
	Op          string       `json:"op"`
	PanelID     string       `json:"panelId,omitempty"`
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	TimeRange   *TimeRange   `json:"timeRange,omitempty"`
	Panel       *PanelUpdate `json:"panel,omitempty"`
}

// PanelUpdate holds partial or full panel fields for update_panel / add_panel.
type PanelUpdate struct {
	ID            string         `json:"id,omitempty"`
	Type          string         `json:"type,omitempty"`
	Title         string         `json:"title,omitempty"`
	Description   *string        `json:"description,omitempty"`
	DatasourceRef string         `json:"datasourceRef,omitempty"`
	Query         *QueryIR       `json:"query,omitempty"`
	Unit          string         `json:"unit,omitempty"`
	Thresholds    *[]ThresholdIR `json:"thresholds,omitempty"`
	Layout        *LayoutHint    `json:"layout,omitempty"`
}

// DiffEntry describes one human-readable change produced by applying a patch.
type DiffEntry struct {
	Op      string `json:"op"`
	PanelID string `json:"panelId,omitempty"`
	Field   string `json:"field,omitempty"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
}

// ScopeFilter constrains metadata grounding and PromQL selectors.
type ScopeFilter struct {
	LabelName  string `json:"labelName"`
	LabelValue string `json:"labelValue"`
}

// MetricCatalogItem is a bounded metadata item provided by the Frontend for the current request only.
type MetricCatalogItem struct {
	Name   string   `json:"name"`
	Type   string   `json:"type,omitempty"`
	Help   string   `json:"help,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// ValidationCategory classifies live datasource dry-run outcomes.
type ValidationCategory string

const (
	ValidationOK              ValidationCategory = "ok"
	ValidationEmptyResult     ValidationCategory = "empty_result"
	ValidationSyntaxError     ValidationCategory = "syntax_error"
	ValidationPermissionError ValidationCategory = "permission_error"
	ValidationTimeout         ValidationCategory = "timeout"
	ValidationDatasourceError ValidationCategory = "datasource_error"
)

// PanelValidationResult is submitted by the Frontend after running a bounded dry-run query.
// Backend treats this as untrusted input and binds it to jobId, revision, panelId, and queryHash.
type PanelValidationResult struct {
	PanelID    string             `json:"panelId"`
	QueryHash  string             `json:"queryHash"`
	Category   ValidationCategory `json:"category"`
	ErrorText  string             `json:"errorText,omitempty"`
	SeriesCount int               `json:"seriesCount,omitempty"`
	DurationMs int64              `json:"durationMs,omitempty"`
}

// QueryHash returns a deterministic SHA-256 hex digest of a normalized PromQL expression.
func QueryHash(expr string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(expr)))
	return hex.EncodeToString(sum[:16])
}

// Clone returns a deep copy of DashboardIR so mutations never alias previous revisions.
func (d DashboardIR) Clone() DashboardIR {
	out := d
	if d.Panels != nil {
		out.Panels = make([]PanelIR, len(d.Panels))
		for i, p := range d.Panels {
			pc := p
			if p.Thresholds != nil {
				pc.Thresholds = make([]ThresholdIR, len(p.Thresholds))
				copy(pc.Thresholds, p.Thresholds)
			}
			out.Panels[i] = pc
		}
	}
	return out
}
