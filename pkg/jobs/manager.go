package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/promql"
	"github.com/yuuki/grafana-llm-studio-app/pkg/prompt"
)

var (
	ErrJobNotFound         = errors.New("job not found")
	ErrForbidden           = errors.New("forbidden: job belongs to another user or organization")
	ErrBudgetExhausted     = errors.New("job repair budget exhausted")
	ErrConcurrencyLimited  = errors.New("too many concurrent LLM jobs")
	ErrOperationInProgress = errors.New("another operation is already in progress for this job")
	ErrNotRepairable       = errors.New("validation findings are not eligible for automatic repair")
	ErrNoUndoHistory       = errors.New("no previous revision available to undo")
	ErrDatasourceDenied    = errors.New("selected datasource UID is not in admin allowlist")
	ErrJobCancelled        = errors.New("job was cancelled")
)

// Owner identifies the authenticated Grafana caller from trusted PluginContext only.
type Owner struct {
	OrgID     int64
	UserLogin string
}

// JobStatus represents the current workflow phase of a studio draft job.
type JobStatus string

const (
	StatusGenerating          JobStatus = "generating"
	StatusValidatingIR        JobStatus = "validating_ir"
	StatusAwaitingPreview     JobStatus = "awaiting_preview"
	StatusRepairing           JobStatus = "repairing"
	StatusReady               JobStatus = "ready"
	StatusInsufficientMetrics JobStatus = "insufficient_metrics"
	StatusCancelled           JobStatus = "cancelled"
	StatusFailed              JobStatus = "failed"
)

// Config configures single-backend bounded in-memory job management.
// Multi-replica continuation across Grafana instances is explicitly out of MVP scope.
type Config struct {
	MaxJobs                  int
	MaxJobsPerUser           int
	JobTTL                   time.Duration
	MaxConcurrentJobs        int
	MaxConcurrentJobsPerUser int
	MaxPanels                int
	// MaxRepairAttempts defaults to 2 when MaxRepairAttemptsSet is false.
	// Set MaxRepairAttemptsSet=true and MaxRepairAttempts=0 to disable auto-repair explicitly.
	MaxRepairAttempts    int
	MaxRepairAttemptsSet bool
	JobDeadline          time.Duration
	AllowedDSUIDs        []string
}

func (c *Config) normalize() {
	if c.MaxJobs <= 0 {
		c.MaxJobs = 200
	}
	if c.MaxJobsPerUser <= 0 {
		c.MaxJobsPerUser = 20
	}
	if c.JobTTL <= 0 {
		c.JobTTL = 30 * time.Minute
	}
	if c.MaxConcurrentJobs <= 0 {
		c.MaxConcurrentJobs = 4
	}
	if c.MaxConcurrentJobsPerUser <= 0 {
		c.MaxConcurrentJobsPerUser = 1
	}
	if c.MaxPanels <= 0 || c.MaxPanels > ir.MaxPanelsHardCap {
		c.MaxPanels = ir.DefaultMaxPanels
	}
	if !c.MaxRepairAttemptsSet {
		if c.MaxRepairAttempts <= 0 {
			c.MaxRepairAttempts = 2
		}
	}
	if c.MaxRepairAttempts < 0 {
		c.MaxRepairAttempts = 0
	}
	if c.MaxRepairAttempts > 2 {
		c.MaxRepairAttempts = 2
	}
	if c.JobDeadline <= 0 {
		c.JobDeadline = 90 * time.Second
	}
}

// JobSnapshot is the safe API view of a job returned to its owner.
type JobSnapshot struct {
	JobID              string              `json:"jobId"`
	Status             JobStatus           `json:"status"`
	DatasourceUID      string              `json:"datasourceUid"`
	Scope              *ir.ScopeFilter     `json:"scope,omitempty"`
	CurrentIR          *ir.DashboardIR     `json:"currentIR,omitempty"`
	LastDiff           []ir.DiffEntry      `json:"lastDiff,omitempty"`
	Summary            string              `json:"summary,omitempty"`
	Warnings           []string            `json:"warnings,omitempty"`
	ErrorMessage       string              `json:"errorMessage,omitempty"`
	RepairAttemptsUsed int                 `json:"repairAttemptsUsed"`
	MaxRepairAttempts  int                 `json:"maxRepairAttempts"`
	CanUndo            bool                `json:"canUndo"`
	LLM                *llm.CompletionMeta `json:"llm,omitempty"`
	UpdatedAt          time.Time           `json:"updatedAt"`
}

type jobRecord struct {
	id                 string
	owner              Owner
	datasourceUID      string
	scope              *ir.ScopeFilter
	status             JobStatus
	currentIR          *ir.DashboardIR
	history            []ir.DashboardIR
	lastDiff           []ir.DiffEntry
	summary            string
	warnings           []string
	errorMessage       string
	repairAttemptsUsed int
	maxRepairAttempts  int
	llmMeta            *llm.CompletionMeta
	knownGauges        map[string]bool
	createdAt          time.Time
	updatedAt          time.Time
	activeOpToken      uint64
	activeCancel       context.CancelFunc
}

// Manager coordinates bounded in-memory draft jobs with strict per-user/org isolation.
// Notice: metric catalogs are NEVER stored in a cross-user shared index; only job-private
// metric type guards (knownGauges) stay scoped to the single job owner and expire with the job.
type Manager struct {
	mu           sync.Mutex
	cfg          Config
	client       llm.Client
	jobs         map[string]*jobRecord
	activeGlobal int
	activeByUser map[Owner]int
	nowFunc      func() time.Time
}

// NewManager creates a new bounded job manager.
func NewManager(client llm.Client, cfg Config) *Manager {
	cfg.normalize()
	return &Manager{
		cfg:          cfg,
		client:       client,
		jobs:         make(map[string]*jobRecord),
		activeByUser: make(map[Owner]int),
		nowFunc:      time.Now,
	}
}

// Close cancels all active in-flight job contexts during plugin shutdown/reload.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.jobs {
		rec.activeOpToken++
		if rec.activeCancel != nil {
			rec.activeCancel()
			rec.activeCancel = nil
		}
	}
}

// GenerateInput is the request payload for starting a new draft generation job.
type GenerateInput struct {
	DatasourceUID string                 `json:"datasourceUid"`
	Scope         *ir.ScopeFilter        `json:"scope,omitempty"`
	UserIntent    string                 `json:"userIntent"`
	Catalog       []ir.MetricCatalogItem `json:"catalog"`
}

// PatchInput is the request payload for applying a partial edit to an existing job.
type PatchInput struct {
	BaseRevision  int                    `json:"baseRevision"`
	TargetPanelID string                 `json:"targetPanelId,omitempty"`
	EditIntent    string                 `json:"editIntent"`
	Catalog       []ir.MetricCatalogItem `json:"catalog"`
}

// RepairInput is the request payload for repairing syntax_error panels reported by Frontend.
type RepairInput struct {
	BaseRevision int                        `json:"baseRevision"`
	Results      []ir.PanelValidationResult `json:"results"`
	Catalog      []ir.MetricCatalogItem     `json:"catalog"`
}

// Generate creates a new owner-bound job, calls the configured LLM, and validates the IR + PromQL.
func (m *Manager) Generate(ctx context.Context, owner Owner, in GenerateInput) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if !m.isDatasourceAllowed(in.DatasourceUID) {
		return JobSnapshot{}, fmt.Errorf("%w: %q", ErrDatasourceDenied, in.DatasourceUID)
	}
	if strings.TrimSpace(in.UserIntent) == "" {
		return JobSnapshot{}, errors.New("userIntent is required")
	}
	if err := promql.ValidateScopeFilter(in.Scope); err != nil {
		return JobSnapshot{}, err
	}
	safeScope := prompt.SanitizeScope(in.Scope)
	boundedCatalog := prompt.BoundCatalog(in.Catalog)

	// Do NOT allocate a job or trigger map eviction for empty catalogs (prevents eviction DoS).
	if len(boundedCatalog) == 0 {
		return JobSnapshot{
			Status:            StatusInsufficientMetrics,
			DatasourceUID:     in.DatasourceUID,
			Scope:             safeScope,
			MaxRepairAttempts: m.cfg.MaxRepairAttempts,
			ErrorMessage:      "選択スコープに利用可能なメトリクス候補がありません。メトリクス名を捏造せず中断しました。",
			UpdatedAt:         m.nowFunc(),
		}, nil
	}

	now := m.nowFunc()
	jobID := newJobID()
	opCtx, cancel := context.WithTimeout(ctx, m.cfg.JobDeadline)
	defer cancel()

	knownGauges := make(map[string]bool)
	for _, item := range boundedCatalog {
		if strings.EqualFold(item.Type, "gauge") {
			knownGauges[item.Name] = true
		}
	}

	m.mu.Lock()
	m.evictExpiredLocked(now)
	if m.activeGlobal >= m.cfg.MaxConcurrentJobs || m.activeByUser[owner] >= m.cfg.MaxConcurrentJobsPerUser {
		m.mu.Unlock()
		return JobSnapshot{}, ErrConcurrencyLimited
	}
	if err := m.ensureCapacityLocked(owner); err != nil {
		m.mu.Unlock()
		return JobSnapshot{}, err
	}
	rec := &jobRecord{
		id:                jobID,
		owner:             owner,
		datasourceUID:     in.DatasourceUID,
		scope:             cloneScope(safeScope),
		status:            StatusGenerating,
		maxRepairAttempts: m.cfg.MaxRepairAttempts,
		knownGauges:       knownGauges,
		createdAt:         now,
		updatedAt:         now,
		activeOpToken:     1,
		activeCancel:      cancel,
	}
	m.jobs[jobID] = rec
	m.activeGlobal++
	m.activeByUser[owner]++
	opToken := rec.activeOpToken
	m.mu.Unlock()
	defer m.releaseSlot(owner)

	raw, meta, err := m.client.GenerateIR(opCtx, llm.GenerateRequest{
		UserIntent: in.UserIntent,
		Scope:      rec.scope,
		Catalog:    boundedCatalog,
		MaxPanels:  m.cfg.MaxPanels,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
			return m.finishJob(jobID, opToken, StatusCancelled, nil, nil, "", nil, "生成はキャンセルされました。", &meta)
		}
		if errors.Is(err, llm.ErrInsufficientMetrics) {
			return m.finishJob(jobID, opToken, StatusInsufficientMetrics, nil, nil, "", nil, "要求に合致するメトリクスが選択スコープ内に見つかりませんでした。", &meta)
		}
		return m.finishJob(jobID, opToken, StatusFailed, nil, nil, "", nil, err.Error(), &meta)
	}

	m.setStatusIfToken(jobID, opToken, StatusValidatingIR)

	doc, err := ir.DecodeStrictIR(raw)
	if err != nil {
		return m.finishJob(jobID, opToken, StatusFailed, nil, nil, "", nil, fmt.Sprintf("IR JSON Schema 検査に失敗しました: %v", err), &meta)
	}
	doc.Revision = 1

	warnings, err := promql.ValidateDashboardIR(
		doc,
		boundedCatalog,
		rec.scope,
		ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels},
		promql.DefaultPolicy(),
	)
	if err != nil {
		if errors.Is(err, promql.ErrMetricNotInCatalog) {
			return m.finishJob(jobID, opToken, StatusInsufficientMetrics, nil, nil, "", nil, fmt.Sprintf("カタログ外のメトリクス参照を検出し拒否しました: %v", err), &meta)
		}
		return m.finishJob(jobID, opToken, StatusFailed, nil, nil, "", nil, fmt.Sprintf("IR/PromQL 検査に失敗しました: %v", err), &meta)
	}

	return m.finishJob(jobID, opToken, StatusAwaitingPreview, &doc, nil, "初期ドラフトを生成しました。検証・プレビューを実行して内容を確認してください。", warnings, "", &meta)
}

// Patch performs a partial edit anchored to baseRevision without regenerating untouched panels.
func (m *Manager) Patch(ctx context.Context, owner Owner, jobID string, in PatchInput) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if strings.TrimSpace(in.EditIntent) == "" {
		return JobSnapshot{}, errors.New("editIntent is required")
	}

	boundedCatalog := prompt.BoundCatalog(in.Catalog)
	if len(boundedCatalog) == 0 {
		return JobSnapshot{}, errors.New("non-empty scoped metric catalog is required")
	}

	m.mu.Lock()
	rec, err := m.getOwnedLocked(owner, jobID)
	if err != nil {
		m.mu.Unlock()
		return JobSnapshot{}, err
	}
	if rec.currentIR == nil {
		m.mu.Unlock()
		return JobSnapshot{}, errors.New("job has no active IR draft to patch")
	}
	if in.BaseRevision != rec.currentIR.Revision {
		curRev := rec.currentIR.Revision
		m.mu.Unlock()
		return JobSnapshot{}, fmt.Errorf("%w: request baseRevision=%d != current revision=%d", ir.ErrRevisionConflict, in.BaseRevision, curRev)
	}
	if rec.activeCancel != nil {
		m.mu.Unlock()
		return JobSnapshot{}, ErrOperationInProgress
	}
	if m.activeGlobal >= m.cfg.MaxConcurrentJobs || m.activeByUser[owner] >= m.cfg.MaxConcurrentJobsPerUser {
		m.mu.Unlock()
		return JobSnapshot{}, ErrConcurrencyLimited
	}

	// Preserve known gauge types from earlier revisions so clients cannot spoof gauge->counter.
	boundedCatalog = enforceKnownGaugeTypes(boundedCatalog, rec.knownGauges)

	opCtx, cancel := context.WithTimeout(ctx, m.cfg.JobDeadline)
	defer cancel()
	rec.activeOpToken++
	opToken := rec.activeOpToken
	rec.activeCancel = cancel
	rec.status = StatusGenerating
	rec.updatedAt = m.nowFunc()
	m.activeGlobal++
	m.activeByUser[owner]++
	currentCopy := rec.currentIR.Clone()
	scopeCopy := cloneScope(rec.scope)
	m.mu.Unlock()
	defer m.releaseSlot(owner)

	raw, meta, err := m.client.GeneratePatch(opCtx, llm.PatchRequest{
		CurrentIR:     currentCopy,
		TargetPanelID: in.TargetPanelID,
		EditIntent:    in.EditIntent,
		Scope:         scopeCopy,
		Catalog:       boundedCatalog,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
			return m.revertToPreviousOnCancel(jobID, opToken)
		}
		return m.recordEditFailure(jobID, opToken, err.Error(), &meta)
	}

	patch, err := ir.DecodeStrictPatch(raw)
	if err != nil {
		return m.recordEditFailure(jobID, opToken, fmt.Sprintf("パッチ JSON 検査に失敗しました: %v", err), &meta)
	}

	// When user explicitly targeted a single panel, enforce that the LLM only updates that panel.
	targetPanelID := strings.TrimSpace(in.TargetPanelID)
	if targetPanelID != "" {
		for _, op := range patch.Operations {
			if op.Op != "update_panel" || op.PanelID != targetPanelID {
				return m.recordEditFailure(
					jobID,
					opToken,
					fmt.Sprintf("対象パネル指定 (%s) に反するパッチ操作 (%s:%s) を拒否しました", targetPanelID, op.Op, op.PanelID),
					&meta,
				)
			}
		}
	}

	nextIR, diffs, err := ir.ApplyPatch(currentCopy, patch, ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels})
	if err != nil {
		return m.recordEditFailure(jobID, opToken, fmt.Sprintf("パッチ適用に失敗しました: %v", err), &meta)
	}

	warnings, err := promql.ValidateDashboardIR(
		nextIR,
		boundedCatalog,
		scopeCopy,
		ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels},
		promql.DefaultPolicy(),
	)
	if err != nil {
		return m.recordEditFailure(jobID, opToken, fmt.Sprintf("パッチ後の PromQL 検査に失敗しました: %v", err), &meta)
	}

	return m.commitRevision(jobID, opToken, currentCopy.Revision, nextIR, diffs, patch.Summary, warnings, &meta)
}

// Repair attempts a bounded repair ONLY for syntax_error panels matching the current IR revision and queryHash.
// Repair NEVER runs for empty_result, permission_error, or timeout, NEVER resets the per-job repair budget,
// and ONLY allows mutating the failing panels' Query field.
func (m *Manager) Repair(ctx context.Context, owner Owner, jobID string, in RepairInput) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}

	boundedCatalog := prompt.BoundCatalog(in.Catalog)

	m.mu.Lock()
	rec, err := m.getOwnedLocked(owner, jobID)
	if err != nil {
		m.mu.Unlock()
		return JobSnapshot{}, err
	}
	if rec.currentIR == nil {
		m.mu.Unlock()
		return JobSnapshot{}, errors.New("job has no active IR draft to repair")
	}
	if in.BaseRevision != rec.currentIR.Revision {
		curRev := rec.currentIR.Revision
		m.mu.Unlock()
		return JobSnapshot{}, fmt.Errorf("%w: repair baseRevision=%d != current revision=%d", ir.ErrRevisionConflict, in.BaseRevision, curRev)
	}
	if rec.activeCancel != nil {
		m.mu.Unlock()
		return JobSnapshot{}, ErrOperationInProgress
	}
	if rec.repairAttemptsUsed >= rec.maxRepairAttempts {
		m.mu.Unlock()
		return JobSnapshot{}, fmt.Errorf("%w: used %d of %d repair attempts", ErrBudgetExhausted, rec.repairAttemptsUsed, rec.maxRepairAttempts)
	}
	if len(in.Results) > len(rec.currentIR.Panels) {
		m.mu.Unlock()
		return JobSnapshot{}, fmt.Errorf("results count %d exceeds panel count %d", len(in.Results), len(rec.currentIR.Panels))
	}

	panelByID := make(map[string]ir.PanelIR, len(rec.currentIR.Panels))
	for _, p := range rec.currentIR.Panels {
		panelByID[p.ID] = p
	}

	seenPanels := make(map[string]bool, len(in.Results))
	var targets []prompt.RepairTarget
	var rejectedReasons []string
	for _, r := range in.Results {
		if seenPanels[r.PanelID] {
			continue
		}
		seenPanels[r.PanelID] = true

		p, exists := panelByID[r.PanelID]
		if !exists {
			continue
		}
		if r.QueryHash != ir.QueryHash(p.Query.Expr) {
			rejectedReasons = append(rejectedReasons, fmt.Sprintf("%s: queryHash mismatch", r.PanelID))
			continue
		}
		switch r.Category {
		case ir.ValidationSyntaxError:
			targets = append(targets, prompt.RepairTarget{
				PanelID:   p.ID,
				Expr:      p.Query.Expr,
				ErrorText: r.ErrorText,
			})
		case ir.ValidationEmptyResult, ir.ValidationPermissionError, ir.ValidationTimeout:
			rejectedReasons = append(rejectedReasons, fmt.Sprintf("%s: %s is not eligible for auto-repair", r.PanelID, r.Category))
		}
	}

	if len(targets) == 0 {
		m.mu.Unlock()
		if len(rejectedReasons) > 0 {
			return JobSnapshot{}, fmt.Errorf("%w (%s)", ErrNotRepairable, strings.Join(rejectedReasons, "; "))
		}
		return JobSnapshot{}, fmt.Errorf("%w: no syntax_error panel matched current revision", ErrNotRepairable)
	}

	if m.activeGlobal >= m.cfg.MaxConcurrentJobs || m.activeByUser[owner] >= m.cfg.MaxConcurrentJobsPerUser {
		m.mu.Unlock()
		return JobSnapshot{}, ErrConcurrencyLimited
	}

	boundedCatalog = enforceKnownGaugeTypes(boundedCatalog, rec.knownGauges)

	// Consume repair budget BEFORE calling LLM so repeated requests cannot bypass budget.
	rec.repairAttemptsUsed++
	opCtx, cancel := context.WithTimeout(ctx, m.cfg.JobDeadline)
	defer cancel()
	rec.activeOpToken++
	opToken := rec.activeOpToken
	rec.activeCancel = cancel
	rec.status = StatusRepairing
	rec.updatedAt = m.nowFunc()
	m.activeGlobal++
	m.activeByUser[owner]++
	currentCopy := rec.currentIR.Clone()
	scopeCopy := cloneScope(rec.scope)
	m.mu.Unlock()
	defer m.releaseSlot(owner)

	raw, meta, err := m.client.RepairPanels(opCtx, llm.RepairRequest{
		CurrentIR: currentCopy,
		Targets:   targets,
		Scope:     scopeCopy,
		Catalog:   boundedCatalog,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
			return m.revertToPreviousOnCancel(jobID, opToken)
		}
		return m.recordEditFailure(jobID, opToken, fmt.Sprintf("自動修復に失敗しました: %v", err), &meta)
	}

	patch, err := ir.DecodeStrictPatch(raw)
	if err != nil {
		return m.recordEditFailure(jobID, opToken, fmt.Sprintf("修復パッチ JSON 検査に失敗しました: %v", err), &meta)
	}

	// Enforce that repair ONLY touches Query on the failing syntax_error panels.
	allowedTargetIDs := make(map[string]bool, len(targets))
	for _, t := range targets {
		allowedTargetIDs[t.PanelID] = true
	}
	for _, op := range patch.Operations {
		if op.Op != "update_panel" || !allowedTargetIDs[op.PanelID] || op.Panel == nil || op.Panel.Query == nil {
			return m.recordEditFailure(jobID, opToken, fmt.Sprintf("修復パッチが対象外パネル/操作 (%s:%s) を含んでいたため拒否しました", op.Op, op.PanelID), &meta)
		}
		p := op.Panel
		if p.DatasourceRef != "" && p.DatasourceRef != ir.SelectedDatasourceRef {
			return m.recordEditFailure(jobID, opToken, "修復パッチによるデータソース変更は禁止されています", &meta)
		}
		if p.Type != "" || p.Title != "" || p.Description != nil || p.Unit != "" || p.Thresholds != nil || p.Layout != nil {
			return m.recordEditFailure(jobID, opToken, "修復パッチは失敗パネルの PromQL クエリのみ変更可能です", &meta)
		}
	}

	nextIR, diffs, err := ir.ApplyPatch(currentCopy, patch, ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels})
	if err != nil {
		return m.recordEditFailure(jobID, opToken, fmt.Sprintf("修復パッチ適用に失敗しました: %v", err), &meta)
	}

	warnings, err := promql.ValidateDashboardIR(
		nextIR,
		boundedCatalog,
		scopeCopy,
		ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels},
		promql.DefaultPolicy(),
	)
	if err != nil {
		return m.recordEditFailure(jobID, opToken, fmt.Sprintf("修復後の PromQL/スコープ検査に失敗しました: %v", err), &meta)
	}

	return m.commitRevision(jobID, opToken, currentCopy.Revision, nextIR, diffs, "構文エラーのパネルを修復しました。変更パネルを再検証してください。", warnings, &meta)
}

// Undo restores the immediately preceding IR revision as a new monotonic revision,
// cancelling any in-flight operation so late failures cannot overwrite the restored draft.
func (m *Manager) Undo(owner Owner, jobID string) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.getOwnedLocked(owner, jobID)
	if err != nil {
		return JobSnapshot{}, err
	}
	if len(rec.history) == 0 || rec.currentIR == nil {
		return JobSnapshot{}, ErrNoUndoHistory
	}
	if rec.currentIR.Revision >= ir.MaxRevision {
		return JobSnapshot{}, fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	}

	// Cancel and invalidate any in-flight Patch/Repair so stale results cannot corrupt Undo state.
	rec.activeOpToken++
	if rec.activeCancel != nil {
		rec.activeCancel()
		rec.activeCancel = nil
	}

	prev := rec.history[len(rec.history)-1]
	rec.history = rec.history[:len(rec.history)-1]
	restored := prev.Clone()
	restored.Revision = rec.currentIR.Revision + 1
	rec.currentIR = &restored
	rec.lastDiff = []ir.DiffEntry{{
		Op:     "undo",
		Field:  "revision",
		Before: fmt.Sprintf("rev %d", restored.Revision-1),
		After:  fmt.Sprintf("restored from rev %d as rev %d", prev.Revision, restored.Revision),
	}}
	rec.summary = "直前のドラフト状態を取り消して復元しました。"
	rec.status = StatusAwaitingPreview
	rec.errorMessage = ""
	rec.updatedAt = m.nowFunc()
	return snapshotLocked(rec), nil
}

// Cancel aborts any in-flight LLM operation for an owner's job and prevents stale results from applying.
func (m *Manager) Cancel(owner Owner, jobID string) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.getOwnedLocked(owner, jobID)
	if err != nil {
		return JobSnapshot{}, err
	}
	rec.activeOpToken++
	if rec.activeCancel != nil {
		rec.activeCancel()
		rec.activeCancel = nil
	}
	if rec.currentIR == nil {
		rec.status = StatusCancelled
	} else {
		rec.status = StatusAwaitingPreview
	}
	rec.summary = "実行中の処理をキャンセルしました。"
	rec.updatedAt = m.nowFunc()
	return snapshotLocked(rec), nil
}

// Get returns the current state of an owner's job.
func (m *Manager) Get(owner Owner, jobID string) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.getOwnedLocked(owner, jobID)
	if err != nil {
		return JobSnapshot{}, err
	}
	return snapshotLocked(rec), nil
}

func (m *Manager) finishJob(
	jobID string,
	opToken uint64,
	status JobStatus,
	doc *ir.DashboardIR,
	diffs []ir.DiffEntry,
	summary string,
	warnings []string,
	errMsg string,
	meta *llm.CompletionMeta,
) (JobSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.jobs[jobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.activeOpToken != opToken {
		return snapshotLocked(rec), ErrJobCancelled
	}
	rec.activeCancel = nil
	rec.status = status
	if doc != nil {
		c := doc.Clone()
		rec.currentIR = &c
	}
	rec.lastDiff = diffs
	rec.summary = summary
	rec.warnings = warnings
	rec.errorMessage = errMsg
	if meta != nil {
		mc := *meta
		rec.llmMeta = &mc
	}
	rec.updatedAt = m.nowFunc()
	return snapshotLocked(rec), nil
}

func (m *Manager) commitRevision(
	jobID string,
	opToken uint64,
	expectedRev int,
	nextIR ir.DashboardIR,
	diffs []ir.DiffEntry,
	summary string,
	warnings []string,
	meta *llm.CompletionMeta,
) (JobSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.jobs[jobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.activeOpToken != opToken {
		return snapshotLocked(rec), ErrJobCancelled
	}
	if rec.currentIR == nil || rec.currentIR.Revision != expectedRev {
		return snapshotLocked(rec), ir.ErrRevisionConflict
	}
	rec.history = append(rec.history, rec.currentIR.Clone())
	if len(rec.history) > 10 {
		rec.history = rec.history[len(rec.history)-10:]
	}
	c := nextIR.Clone()
	rec.currentIR = &c
	rec.lastDiff = diffs
	rec.summary = summary
	rec.warnings = warnings
	rec.errorMessage = ""
	rec.status = StatusAwaitingPreview
	rec.activeCancel = nil
	if meta != nil {
		mc := *meta
		rec.llmMeta = &mc
	}
	rec.updatedAt = m.nowFunc()
	return snapshotLocked(rec), nil
}

func (m *Manager) revertToPreviousOnCancel(jobID string, opToken uint64) (JobSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.jobs[jobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.activeOpToken == opToken {
		rec.activeCancel = nil
		if rec.currentIR != nil {
			rec.status = StatusAwaitingPreview
		} else {
			rec.status = StatusCancelled
		}
		rec.summary = "処理がキャンセルされたためドラフトは変更されていません。"
		rec.updatedAt = m.nowFunc()
	}
	return snapshotLocked(rec), ErrJobCancelled
}

func (m *Manager) recordEditFailure(jobID string, opToken uint64, errMsg string, meta *llm.CompletionMeta) (JobSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.jobs[jobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.activeOpToken != opToken {
		return snapshotLocked(rec), ErrJobCancelled
	}
	rec.activeCancel = nil
	rec.status = StatusFailed
	rec.errorMessage = errMsg
	if meta != nil {
		mc := *meta
		rec.llmMeta = &mc
	}
	rec.updatedAt = m.nowFunc()
	return snapshotLocked(rec), errors.New(errMsg)
}

func (m *Manager) setStatusIfToken(jobID string, opToken uint64, status JobStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.jobs[jobID]; ok && rec.activeOpToken == opToken {
		rec.status = status
		rec.updatedAt = m.nowFunc()
	}
}

func (m *Manager) getOwnedLocked(owner Owner, jobID string) (*jobRecord, error) {
	m.evictExpiredLocked(m.nowFunc())
	rec, ok := m.jobs[jobID]
	if !ok {
		return nil, ErrJobNotFound
	}
	if rec.owner.OrgID != owner.OrgID || rec.owner.UserLogin != owner.UserLogin {
		return nil, ErrForbidden
	}
	return rec, nil
}

func (m *Manager) evictExpiredLocked(now time.Time) {
	for id, rec := range m.jobs {
		if now.Sub(rec.updatedAt) > m.cfg.JobTTL && rec.activeCancel == nil {
			delete(m.jobs, id)
		}
	}
}

// ensureCapacityLocked enforces per-user and global job limits while NEVER evicting
// another user's currently active (in-flight) job.
func (m *Manager) ensureCapacityLocked(caller Owner) error {
	// 1. Enforce per-user retained job cap first (evict caller's own oldest non-active job).
	userCount := 0
	var callerOldestInactive string
	var callerOldestTime time.Time
	for id, rec := range m.jobs {
		if rec.owner == caller {
			userCount++
			if rec.activeCancel == nil && (callerOldestInactive == "" || rec.updatedAt.Before(callerOldestTime)) {
				callerOldestInactive = id
				callerOldestTime = rec.updatedAt
			}
		}
	}
	if userCount >= m.cfg.MaxJobsPerUser {
		if callerOldestInactive == "" {
			return ErrConcurrencyLimited
		}
		delete(m.jobs, callerOldestInactive)
	}

	// 2. Enforce global store cap: evict caller's oldest inactive -> any oldest inactive.
	if len(m.jobs) < m.cfg.MaxJobs {
		return nil
	}
	if callerOldestInactive != "" {
		if _, stillThere := m.jobs[callerOldestInactive]; stillThere {
			delete(m.jobs, callerOldestInactive)
			return nil
		}
	}
	var oldestInactive string
	var oldestTime time.Time
	for id, rec := range m.jobs {
		if rec.activeCancel == nil && (oldestInactive == "" || rec.updatedAt.Before(oldestTime)) {
			oldestInactive = id
			oldestTime = rec.updatedAt
		}
	}
	if oldestInactive == "" {
		// All stored jobs are actively executing for other users; refuse instead of killing them.
		return ErrConcurrencyLimited
	}
	delete(m.jobs, oldestInactive)
	return nil
}

func (m *Manager) releaseSlot(owner Owner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeGlobal > 0 {
		m.activeGlobal--
	}
	if m.activeByUser[owner] > 1 {
		m.activeByUser[owner]--
	} else {
		delete(m.activeByUser, owner)
	}
}

func (m *Manager) isDatasourceAllowed(uid string) bool {
	uid = strings.TrimSpace(uid)
	if uid == "" || len(m.cfg.AllowedDSUIDs) == 0 {
		return false
	}
	for _, allowed := range m.cfg.AllowedDSUIDs {
		if strings.TrimSpace(allowed) == uid {
			return true
		}
	}
	return false
}

func enforceKnownGaugeTypes(catalog []ir.MetricCatalogItem, knownGauges map[string]bool) []ir.MetricCatalogItem {
	if len(knownGauges) == 0 {
		return catalog
	}
	out := make([]ir.MetricCatalogItem, len(catalog))
	for i, item := range catalog {
		if knownGauges[item.Name] {
			item.Type = "gauge"
		}
		out[i] = item
	}
	return out
}

func validateOwner(owner Owner) error {
	if owner.OrgID <= 0 || strings.TrimSpace(owner.UserLogin) == "" {
		return errors.New("unauthenticated caller context: orgId and user login are required")
	}
	return nil
}

func cloneScope(s *ir.ScopeFilter) *ir.ScopeFilter {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

func snapshotLocked(rec *jobRecord) JobSnapshot {
	var irCopy *ir.DashboardIR
	if rec.currentIR != nil {
		c := rec.currentIR.Clone()
		irCopy = &c
	}
	var diffs []ir.DiffEntry
	if len(rec.lastDiff) > 0 {
		diffs = make([]ir.DiffEntry, len(rec.lastDiff))
		copy(diffs, rec.lastDiff)
	}
	var warns []string
	if len(rec.warnings) > 0 {
		warns = make([]string, len(rec.warnings))
		copy(warns, rec.warnings)
	}
	var metaCopy *llm.CompletionMeta
	if rec.llmMeta != nil {
		mc := *rec.llmMeta
		metaCopy = &mc
	}
	return JobSnapshot{
		JobID:              rec.id,
		Status:             rec.status,
		DatasourceUID:      rec.datasourceUID,
		Scope:              cloneScope(rec.scope),
		CurrentIR:          irCopy,
		LastDiff:           diffs,
		Summary:            rec.summary,
		Warnings:           warns,
		ErrorMessage:       rec.errorMessage,
		RepairAttemptsUsed: rec.repairAttemptsUsed,
		MaxRepairAttempts:  rec.maxRepairAttempts,
		CanUndo:            len(rec.history) > 0,
		LLM:                metaCopy,
		UpdatedAt:          rec.updatedAt,
	}
}

func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "job-" + hex.EncodeToString(b)
}
