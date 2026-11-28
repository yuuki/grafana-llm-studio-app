package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var _ JobStore = (*MemoryStore)(nil)

type memJobRecord struct {
	id                 string
	jobKind            string
	owner              Owner
	datasourceUID      string
	queryKind          query.Kind
	scope              *ir.ScopeFilter
	importSource       *ir.ImportSource
	status             JobStatus
	currentIR          *ir.DashboardIR
	statusReport       *ir.StatusReportIR
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
	expiresAt          time.Time
	activeOpToken      uint64
	activeOpUntil      time.Time
}

func (r *memJobRecord) isActive(now time.Time) bool {
	return !r.activeOpUntil.IsZero() && now.Before(r.activeOpUntil)
}

func (r *memJobRecord) isExpired(now time.Time) bool {
	if r.isActive(now) {
		return false
	}
	return !r.expiresAt.IsZero() && now.After(r.expiresAt)
}

// MemoryStore is the zero-dependency in-memory JobStore implementation used by default
// for single-process Grafana deployments and local development.
// Concurrency slots and job capacity limits are scoped per OrgID to guarantee multi-org isolation.
type MemoryStore struct {
	mu        sync.Mutex
	jobs      map[string]*memJobRecord
	orgSlots  map[int64]map[string]time.Time
	userSlots map[Owner]map[string]time.Time
}

// NewMemoryStore creates a new bounded in-memory JobStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		jobs:      make(map[string]*memJobRecord),
		orgSlots:  make(map[int64]map[string]time.Time),
		userSlots: make(map[Owner]map[string]time.Time),
	}
}

func (s *MemoryStore) CreateGenerating(_ context.Context, params CreateGeneratingParams) (OpLease, error) {
	if err := validateOwner(params.Owner); err != nil {
		return OpLease{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return OpLease{}, err
	}
	limits := params.Limits.normalized()
	now := normalizeTimestamp(params.Now)
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictExpiredOrgLocked(params.Owner.OrgID, now)
	s.reapExpiredSlotsLocked(now)

	if _, exists := s.jobs[params.JobID]; exists {
		return OpLease{}, fmt.Errorf("job id %q already exists", params.JobID)
	}
	if !s.canAcquireSlotLocked(params.Owner, limits) {
		return OpLease{}, ErrConcurrencyLimited
	}
	if err := s.ensureCapacityLocked(params.Owner, now, limits); err != nil {
		return OpLease{}, err
	}

	gauges := make(map[string]bool)
	mergeGaugeMapBounded(gauges, params.KnownGauges)

	leaseUntil := now.Add(limits.OpLeaseTTL).Truncate(time.Millisecond)
	initStatus := StatusGenerating
	jobKind := params.JobKind
	if jobKind == JobKindReport {
		initStatus = StatusAnalyzing
	}
	rec := &memJobRecord{
		id:                params.JobID,
		jobKind:           jobKind,
		owner:             params.Owner,
		datasourceUID:     params.DatasourceUID,
		queryKind:         params.QueryKind,
		scope:             cloneScope(params.Scope),
		status:            initStatus,
		maxRepairAttempts: params.MaxRepairAttempts,
		knownGauges:       gauges,
		createdAt:         now,
		updatedAt:         now,
		expiresAt:         now.Add(limits.JobTTL).Truncate(time.Millisecond),
		activeOpToken:     1,
		activeOpUntil:     leaseUntil,
	}
	s.jobs[params.JobID] = rec
	s.acquireSlotLocked(params.Owner, slotID, leaseUntil)
	return OpLease{OpToken: rec.activeOpToken, SlotID: slotID}, nil
}

func (s *MemoryStore) CreateImported(_ context.Context, params CreateImportedParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	limits := params.Limits.normalized()
	now := normalizeTimestamp(params.Now)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictExpiredOrgLocked(params.Owner.OrgID, now)
	if _, exists := s.jobs[params.JobID]; exists {
		return JobSnapshot{}, fmt.Errorf("job id %q already exists", params.JobID)
	}
	if err := s.ensureImportCapacityLocked(params.Owner, now, limits); err != nil {
		return JobSnapshot{}, err
	}

	gauges := make(map[string]bool)
	mergeGaugeMapBounded(gauges, params.KnownGauges)

	docCopy := params.Draft.Clone()
	var warns []string
	if len(params.Warnings) > 0 {
		warns = make([]string, len(params.Warnings))
		copy(warns, params.Warnings)
	}
	rec := &memJobRecord{
		id:                params.JobID,
		owner:             params.Owner,
		datasourceUID:     params.DatasourceUID,
		queryKind:         params.QueryKind,
		scope:             cloneScope(params.Scope),
		importSource:      cloneImportSource(params.ImportSource),
		status:            StatusAwaitingPreview,
		currentIR:         &docCopy,
		summary:           params.Summary,
		warnings:          warns,
		maxRepairAttempts: params.MaxRepairAttempts,
		knownGauges:       gauges,
		createdAt:         now,
		updatedAt:         now,
		expiresAt:         now.Add(limits.JobTTL).Truncate(time.Millisecond),
		activeOpToken:     0,
	}
	s.jobs[params.JobID] = rec
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) Get(_ context.Context, owner Owner, jobID string, now time.Time) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	now = normalizeTimestamp(now)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) GetRepairCandidate(_ context.Context, owner Owner, jobID string, now time.Time) (RepairCandidate, error) {
	if err := validateOwner(owner); err != nil {
		return RepairCandidate{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return RepairCandidate{}, err
	}
	now = normalizeTimestamp(now)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return RepairCandidate{}, err
	}
	if rec.jobKind == JobKindReport {
		return RepairCandidate{}, ErrWrongJobKind
	}
	var irCopy *ir.DashboardIR
	if rec.currentIR != nil {
		c := rec.currentIR.Clone()
		irCopy = &c
	}
	qKind := rec.queryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return RepairCandidate{
		CurrentIR:          irCopy,
		RepairAttemptsUsed: rec.repairAttemptsUsed,
		MaxRepairAttempts:  rec.maxRepairAttempts,
		HasActiveOp:        rec.isActive(now),
		QueryKind:          qKind,
	}, nil
}

func (s *MemoryStore) BeginPatch(_ context.Context, params BeginPatchParams) (BeginOpResult, error) {
	if err := validateOwner(params.Owner); err != nil {
		return BeginOpResult{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return BeginOpResult{}, err
	}
	limits := params.Limits.normalized()
	now := normalizeTimestamp(params.Now)
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(params.Owner, params.JobID, now)
	if err != nil {
		return BeginOpResult{}, err
	}
	if rec.jobKind == JobKindReport {
		return BeginOpResult{}, ErrWrongJobKind
	}
	if rec.currentIR == nil {
		return BeginOpResult{}, errors.New("job has no active IR draft to patch")
	}
	if params.BaseRevision != rec.currentIR.Revision {
		return BeginOpResult{}, formatRevisionConflict("", params.BaseRevision, rec.currentIR.Revision)
	}
	if params.TargetPanelID != "" {
		if strings.ContainsAny(params.TargetPanelID, "\r\n") || !panelExists(rec.currentIR.Panels, params.TargetPanelID) {
			return BeginOpResult{}, fmt.Errorf("%w: %q", ir.ErrPanelNotFound, params.TargetPanelID)
		}
	}
	if rec.isActive(now) {
		return BeginOpResult{}, ErrOperationInProgress
	}

	s.reapExpiredSlotsLocked(now)
	if !s.canAcquireSlotLocked(params.Owner, limits) {
		return BeginOpResult{}, ErrConcurrencyLimited
	}

	if rec.knownGauges == nil {
		rec.knownGauges = make(map[string]bool)
	}
	mergeGaugeMapBounded(rec.knownGauges, params.CatalogGauges)

	leaseUntil := now.Add(limits.OpLeaseTTL).Truncate(time.Millisecond)
	rec.activeOpToken++
	rec.activeOpUntil = leaseUntil
	rec.status = StatusGenerating
	rec.updatedAt = now
	rec.expiresAt = now.Add(limits.JobTTL).Truncate(time.Millisecond)
	s.acquireSlotLocked(params.Owner, slotID, leaseUntil)

	qKind := rec.queryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return BeginOpResult{
		OpToken:     rec.activeOpToken,
		SlotID:      slotID,
		CurrentIR:   rec.currentIR.Clone(),
		Scope:       cloneScope(rec.scope),
		QueryKind:   qKind,
		KnownGauges: cloneStringBoolMap(rec.knownGauges),
	}, nil
}

func (s *MemoryStore) BeginRepair(_ context.Context, params BeginRepairParams) (BeginOpResult, error) {
	if err := validateOwner(params.Owner); err != nil {
		return BeginOpResult{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return BeginOpResult{}, err
	}
	limits := params.Limits.normalized()
	now := normalizeTimestamp(params.Now)
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(params.Owner, params.JobID, now)
	if err != nil {
		return BeginOpResult{}, err
	}
	if rec.jobKind == JobKindReport {
		return BeginOpResult{}, ErrWrongJobKind
	}
	if rec.currentIR == nil {
		return BeginOpResult{}, errors.New("job has no active IR draft to repair")
	}
	if params.BaseRevision != rec.currentIR.Revision {
		return BeginOpResult{}, formatRevisionConflict("repair", params.BaseRevision, rec.currentIR.Revision)
	}
	if rec.isActive(now) {
		return BeginOpResult{}, ErrOperationInProgress
	}
	if rec.repairAttemptsUsed >= rec.maxRepairAttempts {
		return BeginOpResult{}, fmt.Errorf("%w: used %d of %d repair attempts", ErrBudgetExhausted, rec.repairAttemptsUsed, rec.maxRepairAttempts)
	}

	s.reapExpiredSlotsLocked(now)
	if !s.canAcquireSlotLocked(params.Owner, limits) {
		return BeginOpResult{}, ErrConcurrencyLimited
	}

	if rec.knownGauges == nil {
		rec.knownGauges = make(map[string]bool)
	}
	mergeGaugeMapBounded(rec.knownGauges, params.CatalogGauges)

	// Atomically consume repair budget BEFORE calling LLM so concurrent requests across replicas cannot bypass budget.
	leaseUntil := now.Add(limits.OpLeaseTTL).Truncate(time.Millisecond)
	rec.repairAttemptsUsed++
	rec.activeOpToken++
	rec.activeOpUntil = leaseUntil
	rec.status = StatusRepairing
	rec.updatedAt = now
	rec.expiresAt = now.Add(limits.JobTTL).Truncate(time.Millisecond)
	s.acquireSlotLocked(params.Owner, slotID, leaseUntil)

	qKind := rec.queryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return BeginOpResult{
		OpToken:     rec.activeOpToken,
		SlotID:      slotID,
		CurrentIR:   rec.currentIR.Clone(),
		Scope:       cloneScope(rec.scope),
		QueryKind:   qKind,
		KnownGauges: cloneStringBoolMap(rec.knownGauges),
	}, nil
}

func (s *MemoryStore) SetStatus(_ context.Context, owner Owner, jobID string, opToken uint64, status JobStatus, now time.Time, jobTTL time.Duration) error {
	if err := validateOwner(owner); err != nil {
		return err
	}
	if err := ValidateJobID(jobID); err != nil {
		return err
	}
	now = normalizeTimestamp(now)
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return err
	}
	if opToken > 0 && rec.activeOpToken == opToken && rec.isActive(now) {
		rec.status = status
		rec.updatedAt = now
		rec.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	}
	return nil
}

func (s *MemoryStore) FinishJob(_ context.Context, params FinishJobParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	now := normalizeTimestamp(params.Now)
	ttl := normalizeJobTTL(params.JobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(params.Owner, params.JobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	if params.OpToken == 0 || rec.activeOpToken != params.OpToken || !rec.isActive(now) {
		return memSnapshotLocked(rec), ErrJobCancelled
	}
	// Advance token on completion so stale/replayed callers with the old opToken cannot mutate the completed job.
	rec.activeOpToken++
	rec.activeOpUntil = time.Time{}
	rec.status = params.Status
	if params.Doc != nil {
		c := params.Doc.Clone()
		rec.currentIR = &c
	}
	if params.Report != nil {
		rc := params.Report.Clone()
		rec.statusReport = &rc
	}
	if len(params.Diffs) > 0 {
		rec.lastDiff = make([]ir.DiffEntry, len(params.Diffs))
		copy(rec.lastDiff, params.Diffs)
	} else {
		rec.lastDiff = nil
	}
	rec.summary = params.Summary
	if len(params.Warnings) > 0 {
		rec.warnings = make([]string, len(params.Warnings))
		copy(rec.warnings, params.Warnings)
	} else {
		rec.warnings = nil
	}
	rec.errorMessage = params.ErrorMessage
	if params.LLMMeta != nil {
		mc := *params.LLMMeta
		rec.llmMeta = &mc
	}
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) CommitRevision(_ context.Context, params CommitRevisionParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	now := normalizeTimestamp(params.Now)
	ttl := normalizeJobTTL(params.JobTTL)
	maxHist := params.MaxUndoHistory
	if maxHist <= 0 {
		maxHist = defaultMaxUndoHistory
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(params.Owner, params.JobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	if params.OpToken == 0 || rec.activeOpToken != params.OpToken || !rec.isActive(now) {
		return memSnapshotLocked(rec), ErrJobCancelled
	}
	if rec.currentIR == nil || rec.currentIR.Revision != params.ExpectedRev {
		rec.activeOpToken++
		rec.activeOpUntil = time.Time{}
		return memSnapshotLocked(rec), ir.ErrRevisionConflict
	}
	rec.history = append(rec.history, rec.currentIR.Clone())
	if len(rec.history) > maxHist {
		rec.history = rec.history[len(rec.history)-maxHist:]
	}
	c := params.NextIR.Clone()
	rec.currentIR = &c
	if len(params.Diffs) > 0 {
		rec.lastDiff = make([]ir.DiffEntry, len(params.Diffs))
		copy(rec.lastDiff, params.Diffs)
	} else {
		rec.lastDiff = nil
	}
	rec.summary = params.Summary
	if len(params.Warnings) > 0 {
		rec.warnings = make([]string, len(params.Warnings))
		copy(rec.warnings, params.Warnings)
	} else {
		rec.warnings = nil
	}
	rec.errorMessage = ""
	rec.status = StatusAwaitingPreview
	rec.activeOpToken++
	rec.activeOpUntil = time.Time{}
	if params.LLMMeta != nil {
		mc := *params.LLMMeta
		rec.llmMeta = &mc
	}
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) RecordEditFailure(_ context.Context, params RecordEditFailureParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	now := normalizeTimestamp(params.Now)
	ttl := normalizeJobTTL(params.JobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(params.Owner, params.JobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	if params.OpToken == 0 || rec.activeOpToken != params.OpToken || !rec.isActive(now) {
		return memSnapshotLocked(rec), ErrJobCancelled
	}
	rec.activeOpToken++
	rec.activeOpUntil = time.Time{}
	rec.status = StatusFailed
	rec.errorMessage = params.ErrorMessage
	if params.LLMMeta != nil {
		mc := *params.LLMMeta
		rec.llmMeta = &mc
	}
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) RevertOnCancel(_ context.Context, owner Owner, jobID string, opToken uint64, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	now = normalizeTimestamp(now)
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	if opToken > 0 && rec.activeOpToken == opToken && rec.isActive(now) {
		rec.activeOpToken++
		rec.activeOpUntil = time.Time{}
		if rec.jobKind == JobKindReport {
			rec.status = StatusCancelled
			rec.summary = "Status analysis was cancelled."
		} else if rec.currentIR != nil {
			rec.status = StatusAwaitingPreview
			rec.summary = "処理がキャンセルされたためドラフトは変更されていません。"
		} else {
			rec.status = StatusCancelled
			rec.summary = "処理がキャンセルされたためドラフトは変更されていません。"
		}
		rec.updatedAt = now
		rec.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	}
	return memSnapshotLocked(rec), ErrJobCancelled
}

func (s *MemoryStore) Undo(_ context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	now = normalizeTimestamp(now)
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return MutationResult{}, err
	}
	if rec.jobKind == JobKindReport {
		return MutationResult{}, ErrWrongJobKind
	}
	if len(rec.history) == 0 || rec.currentIR == nil {
		return MutationResult{}, ErrNoUndoHistory
	}
	if rec.currentIR.Revision >= ir.MaxRevision {
		return MutationResult{}, fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	}

	// Bump fencing token and clear active op lease so any stale in-flight Patch/Repair on any replica is invalidated.
	rec.activeOpToken++
	rec.activeOpUntil = time.Time{}

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
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	return MutationResult{Snapshot: memSnapshotLocked(rec), OpToken: rec.activeOpToken}, nil
}

func (s *MemoryStore) Cancel(_ context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	now = normalizeTimestamp(now)
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return MutationResult{}, err
	}
	if rec.jobKind == JobKindReport && !rec.isActive(now) && isTerminalReportStatus(rec.status) {
		return MutationResult{Snapshot: memSnapshotLocked(rec), OpToken: rec.activeOpToken}, nil
	}
	rec.activeOpToken++
	rec.activeOpUntil = time.Time{}
	if rec.jobKind == JobKindReport {
		rec.status = StatusCancelled
		rec.summary = "Status analysis was cancelled."
	} else if rec.currentIR == nil {
		rec.status = StatusCancelled
		rec.summary = "実行中の処理をキャンセルしました。"
	} else {
		rec.status = StatusAwaitingPreview
		rec.summary = "実行中の処理をキャンセルしました。"
	}
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	return MutationResult{Snapshot: memSnapshotLocked(rec), OpToken: rec.activeOpToken}, nil
}

func (s *MemoryStore) ReleaseSlot(_ context.Context, owner Owner, slotID string, now time.Time) error {
	now = normalizeTimestamp(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.orgSlots[owner.OrgID]; ok {
		delete(m, slotID)
		if len(m) == 0 {
			delete(s.orgSlots, owner.OrgID)
		}
	}
	if m, ok := s.userSlots[owner]; ok {
		delete(m, slotID)
		if len(m) == 0 {
			delete(s.userSlots, owner)
		}
	}
	s.reapExpiredSlotsLocked(now)
	return nil
}

func (s *MemoryStore) CleanupExpired(_ context.Context, now time.Time) error {
	now = normalizeTimestamp(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rec := range s.jobs {
		if rec.isExpired(now) {
			delete(s.jobs, id)
		}
	}
	s.reapExpiredSlotsLocked(now)
	return nil
}

func (s *MemoryStore) Ping(_ context.Context) error {
	return nil
}

func (s *MemoryStore) Close() error {
	return nil
}

func (s *MemoryStore) getOwnedLocked(owner Owner, jobID string, now time.Time) (*memJobRecord, error) {
	rec, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrJobNotFound
	}
	if rec.isExpired(now) {
		if rec.owner == owner {
			delete(s.jobs, jobID)
		}
		return nil, ErrJobNotFound
	}
	if rec.owner.OrgID != owner.OrgID || rec.owner.UserLogin != owner.UserLogin {
		return nil, ErrForbidden
	}
	return rec, nil
}

func (s *MemoryStore) evictExpiredOrgLocked(orgID int64, now time.Time) {
	for id, rec := range s.jobs {
		if rec.owner.OrgID == orgID && rec.isExpired(now) {
			delete(s.jobs, id)
		}
	}
}

func (s *MemoryStore) reapExpiredSlotsLocked(now time.Time) {
	for orgID, m := range s.orgSlots {
		for id, exp := range m {
			if !now.Before(exp) {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(s.orgSlots, orgID)
		}
	}
	for owner, m := range s.userSlots {
		for id, exp := range m {
			if !now.Before(exp) {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(s.userSlots, owner)
		}
	}
}

func (s *MemoryStore) canAcquireSlotLocked(owner Owner, limits StoreLimits) bool {
	if len(s.orgSlots[owner.OrgID]) >= limits.MaxConcurrentJobs {
		return false
	}
	if len(s.userSlots[owner]) >= limits.MaxConcurrentJobsPerUser {
		return false
	}
	return true
}

func (s *MemoryStore) acquireSlotLocked(owner Owner, slotID string, leaseUntil time.Time) {
	if s.orgSlots[owner.OrgID] == nil {
		s.orgSlots[owner.OrgID] = make(map[string]time.Time)
	}
	s.orgSlots[owner.OrgID][slotID] = leaseUntil
	if s.userSlots[owner] == nil {
		s.userSlots[owner] = make(map[string]time.Time)
	}
	s.userSlots[owner][slotID] = leaseUntil
}

// ensureCapacityLocked transactionally enforces per-user and per-org job limits while NEVER
// evicting active jobs, NEVER touching another organization's jobs, and NEVER deleting any job
// if the final capacity constraint cannot be satisfied.
func (s *MemoryStore) ensureCapacityLocked(caller Owner, now time.Time, limits StoreLimits) error {
	var callerInactive []*memJobRecord
	var otherOrgInactive []*memJobRecord
	userCount := 0
	orgCount := 0

	for _, rec := range s.jobs {
		if rec.owner.OrgID != caller.OrgID {
			continue
		}
		orgCount++
		if rec.owner == caller {
			userCount++
			if !rec.isActive(now) {
				callerInactive = append(callerInactive, rec)
			}
		} else if !rec.isActive(now) {
			otherOrgInactive = append(otherOrgInactive, rec)
		}
	}

	sortByOldestUpdated(callerInactive)
	sortByOldestUpdated(otherOrgInactive)

	needUserEvict := 0
	if userCount >= limits.MaxJobsPerUser {
		needUserEvict = userCount - limits.MaxJobsPerUser + 1
	}
	if needUserEvict > len(callerInactive) {
		return ErrConcurrencyLimited
	}

	var victims []string
	for i := 0; i < needUserEvict; i++ {
		victims = append(victims, callerInactive[i].id)
	}
	remainingCaller := callerInactive[needUserEvict:]
	remainingOrgCount := orgCount - needUserEvict

	needOrgEvict := 0
	if remainingOrgCount >= limits.MaxJobs {
		needOrgEvict = remainingOrgCount - limits.MaxJobs + 1
	}
	if needOrgEvict > len(remainingCaller)+len(otherOrgInactive) {
		return ErrConcurrencyLimited
	}

	// Evict caller's own inactive jobs first, then oldest inactive jobs within the same OrgID.
	for i := 0; i < len(remainingCaller) && needOrgEvict > 0; i++ {
		victims = append(victims, remainingCaller[i].id)
		needOrgEvict--
	}
	for i := 0; i < len(otherOrgInactive) && needOrgEvict > 0; i++ {
		victims = append(victims, otherOrgInactive[i].id)
		needOrgEvict--
	}

	for _, id := range victims {
		delete(s.jobs, id)
	}
	return nil
}

// ensureImportCapacityLocked transactionally enforces per-user and per-org caps for Import jobs.
// Unlike Generate, Import does not consume an LLM concurrency slot, so it MUST NEVER evict
// another user's non-expired job (preventing cross-user job-eviction DoS) and MUST NEVER
// delete any job if capacity cannot be satisfied.
func (s *MemoryStore) ensureImportCapacityLocked(caller Owner, now time.Time, limits StoreLimits) error {
	var callerInactive []*memJobRecord
	userCount := 0
	orgCount := 0

	for _, rec := range s.jobs {
		if rec.owner.OrgID != caller.OrgID {
			continue
		}
		orgCount++
		if rec.owner == caller {
			userCount++
			if !rec.isActive(now) {
				callerInactive = append(callerInactive, rec)
			}
		}
	}
	sortByOldestUpdated(callerInactive)

	needUserEvict := 0
	if userCount >= limits.MaxJobsPerUser {
		needUserEvict = userCount - limits.MaxJobsPerUser + 1
	}
	needOrgEvict := 0
	if orgCount >= limits.MaxJobs {
		needOrgEvict = orgCount - limits.MaxJobs + 1
	}
	needTotal := needUserEvict
	if needOrgEvict > needTotal {
		needTotal = needOrgEvict
	}
	if needTotal > len(callerInactive) {
		return ErrConcurrencyLimited
	}
	for i := 0; i < needTotal; i++ {
		delete(s.jobs, callerInactive[i].id)
	}
	return nil
}

func sortByOldestUpdated(recs []*memJobRecord) {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].updatedAt.Equal(recs[j].updatedAt) {
			return recs[i].id < recs[j].id
		}
		return recs[i].updatedAt.Before(recs[j].updatedAt)
	})
}

func isTerminalReportStatus(st JobStatus) bool {
	return st == StatusReady || st == StatusFailed || st == StatusCancelled
}

func memSnapshotLocked(rec *memJobRecord) JobSnapshot {
	var irCopy *ir.DashboardIR
	if rec.currentIR != nil {
		c := rec.currentIR.Clone()
		irCopy = &c
	}
	var reportCopy *ir.StatusReportIR
	if rec.statusReport != nil {
		rc := rec.statusReport.Clone()
		reportCopy = &rc
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
	qKind := rec.queryKind
	if qKind == "" && rec.jobKind != JobKindReport {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:              rec.id,
		JobKind:            rec.jobKind,
		Status:             rec.status,
		DatasourceUID:      rec.datasourceUID,
		QueryKind:          string(qKind),
		Scope:              cloneScope(rec.scope),
		ImportSource:       cloneImportSource(rec.importSource),
		CurrentIR:          irCopy,
		StatusReport:       reportCopy,
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
