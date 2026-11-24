package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	SchemaVersionV10      = "1.0"
	SchemaVersion         = "1.1"
	SelectedDatasourceRef = "selected_prometheus"

	DefaultMaxPanels      = 6
	MaxPanelsHardCap      = 6
	MaxQueriesPerPanel    = 3
	MaxVariables          = 4
	MaxVariableOptions    = 20
	MaxVariableNameLen    = 32
	MaxVariableLabelLen   = 64
	MaxVariableDefaultLen = 128
	MaxVariableOptionLen  = 64
	MaxTitleLen           = 120
	MaxDescLen            = 400
	MaxPanelTitleLen      = 80
	MaxPanelDescLen       = 240
	MaxExprLen            = 512
	MaxLegendLen          = 80
	MaxSummaryLen         = 240
	MaxThresholds         = 4
	MaxRevision           = 10000
	MaxIRPayloadBytes     = 64 * 1024
)

var defaultQueryRefIDs = []string{"A", "B", "C"}

// DashboardIR is the restricted intermediate representation produced by the LLM
// or created via safe import from an existing Grafana dashboard.
type DashboardIR struct {
	SchemaVersion string       `json:"schemaVersion"`
	Revision      int          `json:"revision"`
	Title         string       `json:"title"`
	Description   string       `json:"description,omitempty"`
	TimeRange     TimeRange    `json:"timeRange"`
	Variables     []VariableIR `json:"variables,omitempty"`
	Panels        []PanelIR    `json:"panels"`
}

// TimeRange defines a bounded relative dashboard time window.
type TimeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// VariableIR defines a scoped dashboard template variable (query or bounded custom).
type VariableIR struct {
	Name          string   `json:"name"`
	Label         string   `json:"label,omitempty"`
	Type          string   `json:"type"`
	DatasourceRef string   `json:"datasourceRef,omitempty"`
	LabelName     string   `json:"labelName,omitempty"`
	MetricName    string   `json:"metricName,omitempty"`
	Options       []string `json:"options,omitempty"`
	DefaultValue  string   `json:"defaultValue,omitempty"`
	Multi         bool     `json:"multi,omitempty"`
	IncludeAll    bool     `json:"includeAll,omitempty"`
}

// VariableUpdate carries partial or full variable fields for add_variable / update_variable.
type VariableUpdate struct {
	Name          string    `json:"name,omitempty"`
	Label         *string   `json:"label,omitempty"`
	Type          string    `json:"type,omitempty"`
	DatasourceRef *string   `json:"datasourceRef,omitempty"`
	LabelName     *string   `json:"labelName,omitempty"`
	MetricName    *string   `json:"metricName,omitempty"`
	Options       *[]string `json:"options,omitempty"`
	DefaultValue  *string   `json:"defaultValue,omitempty"`
	Multi         *bool     `json:"multi,omitempty"`
	IncludeAll    *bool     `json:"includeAll,omitempty"`
}

// PanelIR defines one visualization panel in the IR.
// Exactly one of Query (single-query v1.0 compatible) or Queries (1..3 queries) must be populated.
type PanelIR struct {
	ID            string        `json:"id"`
	Type          string        `json:"type"`
	Title         string        `json:"title"`
	Description   string        `json:"description,omitempty"`
	DatasourceRef string        `json:"datasourceRef"`
	Query         QueryIR       `json:"query,omitzero"`
	Queries       []QueryIR     `json:"queries,omitempty"`
	Unit          string        `json:"unit"`
	Thresholds    []ThresholdIR `json:"thresholds,omitempty"`
	Layout        LayoutHint    `json:"layout"`
}

// EffectiveQueries returns the ordered list of PromQL queries defined on this panel.
func (p PanelIR) EffectiveQueries() []QueryIR {
	if len(p.Queries) > 0 {
		return p.Queries
	}
	if p.Query != (QueryIR{}) {
		return []QueryIR{p.Query}
	}
	return nil
}

// QueryIR defines one query for a panel.
type QueryIR struct {
	RefID        string `json:"refId,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat,omitempty"`
}

// QueryUpdate carries patch query updates; pointer RefID/Kind/LegendFormat distinguish omitted from explicit value.
type QueryUpdate struct {
	RefID        *string `json:"refId,omitempty"`
	Kind         *string `json:"kind,omitempty"`
	Expr         string  `json:"expr"`
	LegendFormat *string `json:"legendFormat,omitempty"`
}

// StampQueryKind fills empty query.Kind fields across all panels with kind.
// Explicit non-empty kinds are preserved so downstream validation can reject dialect mismatches.
func (d *DashboardIR) StampQueryKind(kind string) {
	if d == nil || strings.TrimSpace(kind) == "" {
		return
	}
	k := strings.TrimSpace(kind)
	for i := range d.Panels {
		p := &d.Panels[i]
		if len(p.Queries) > 0 {
			for qi := range p.Queries {
				if strings.TrimSpace(p.Queries[qi].Kind) == "" {
					p.Queries[qi].Kind = k
				}
			}
		} else if p.Query != (QueryIR{}) {
			if strings.TrimSpace(p.Query.Kind) == "" {
				p.Query.Kind = k
			}
		}
	}
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
	Op           string          `json:"op"`
	PanelID      string          `json:"panelId,omitempty"`
	VariableName string          `json:"variableName,omitempty"`
	Title        string          `json:"title,omitempty"`
	Description  *string         `json:"description,omitempty"`
	TimeRange    *TimeRange      `json:"timeRange,omitempty"`
	Panel        *PanelUpdate    `json:"panel,omitempty"`
	Variable     *VariableUpdate `json:"variable,omitempty"`
}

// PanelUpdate holds partial or full panel fields for update_panel / add_panel.
type PanelUpdate struct {
	ID            string         `json:"id,omitempty"`
	Type          string         `json:"type,omitempty"`
	Title         string         `json:"title,omitempty"`
	Description   *string        `json:"description,omitempty"`
	DatasourceRef string         `json:"datasourceRef,omitempty"`
	Query         *QueryUpdate   `json:"query,omitempty"`
	Queries       *[]QueryIR     `json:"queries,omitempty"`
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

// ImportSource records provenance when a draft was safely imported from an existing Grafana dashboard.
// ResourceVersion enables opt-in optimistic-concurrency updates without silent overwrites.
type ImportSource struct {
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	FolderUID       string `json:"folderUid,omitempty"`
	Title           string `json:"title,omitempty"`
	Version         int    `json:"version,omitempty"`
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
	PanelID     string             `json:"panelId"`
	QueryHash   string             `json:"queryHash"`
	Category    ValidationCategory `json:"category"`
	ErrorText   string             `json:"errorText,omitempty"`
	SeriesCount int                `json:"seriesCount,omitempty"`
	DurationMs  int64              `json:"durationMs,omitempty"`
}

// QueryHash returns a deterministic SHA-256 hex digest (first 16 bytes) of a normalized PromQL expression.
func QueryHash(expr string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(expr)))
	return hex.EncodeToString(sum[:16])
}

// PanelQueryHash returns a deterministic hash over all effective queries on a panel.
// For single-query panels this is identical to QueryHash(expr) for full backward compatibility.
func PanelQueryHash(p PanelIR) string {
	qs := p.EffectiveQueries()
	if len(qs) == 0 {
		return QueryHash("")
	}
	if len(qs) == 1 {
		return QueryHash(qs[0].Expr)
	}
	parts := make([]string, len(qs))
	for i, q := range qs {
		refID := q.RefID
		if refID == "" && i < len(defaultQueryRefIDs) {
			refID = defaultQueryRefIDs[i]
		}
		parts[i] = refID + ":" + strings.TrimSpace(q.Expr)
	}
	return QueryHash(strings.Join(parts, "\n"))
}

// Clone returns a deep copy of DashboardIR so mutations never alias previous revisions.
func (d DashboardIR) Clone() DashboardIR {
	out := d
	if d.Variables != nil {
		out.Variables = make([]VariableIR, len(d.Variables))
		for i, v := range d.Variables {
			vc := v
			if v.Options != nil {
				vc.Options = make([]string, len(v.Options))
				copy(vc.Options, v.Options)
			}
			out.Variables[i] = vc
		}
	}
	if d.Panels != nil {
		out.Panels = make([]PanelIR, len(d.Panels))
		for i, p := range d.Panels {
			pc := p
			if p.Queries != nil {
				pc.Queries = make([]QueryIR, len(p.Queries))
				copy(pc.Queries, p.Queries)
			}
			if p.Thresholds != nil {
				pc.Thresholds = make([]ThresholdIR, len(p.Thresholds))
				copy(pc.Thresholds, p.Thresholds)
			}
			out.Panels[i] = pc
		}
	}
	return out
}
