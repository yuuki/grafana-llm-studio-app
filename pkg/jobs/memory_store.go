package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var _ JobStore = (*MemoryStore)(nil)

type memJobRecord struct {
	id                 string
	owner              Owner
	datasourceUID      string
	queryKind          query.Kind
	scope              *ir.ScopeFilter
	importSource       *ir.ImportSource
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
	expiresAt          time.Time
	activeOpToken      uint64
	activeOpUntil      time.Time
	activeSlotID       string
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
type MemoryStore struct {
	mu          sync.Mutex
	jobs        map[string]*memJobRecord
	globalSlots map[string]time.Time
	userSlots   map[Owner]map[string]time.Time
}

// NewMemoryStore creates a new bounded in-memory JobStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		jobs:        make(map[string]*memJobRecord),
		globalSlots: make(map[string]time.Time),
		userSlots:   make(map[Owner]map[string]time.Time),
	}
}

func (s *MemoryStore) CreateGenerating(_ context.Context, params CreateGeneratingParams) (OpLease, error) {
	if err := validateOwner(params.Owner); err != nil {
		return OpLease{}, err
	}
	limits := params.Limits.normalized()
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictExpiredLocked(now)
	s.reapExpiredSlotsLocked(now)

	if !s.canAcquireSlotLocked(params.Owner, limits) {
		return OpLease{}, ErrConcurrencyLimited
	}
	if err := s.ensureCapacityLocked(params.Owner, now, limits); err != nil {
		return OpLease{}, err
	}

	gauges := make(map[string]bool, len(params.KnownGauges))
	for _, g := range params.KnownGauges {
		if g != "" {
			gauges[g] = true
		}
	}

	leaseUntil := now.Add(limits.OpLeaseTTL)
	rec := &memJobRecord{
		id:                params.JobID,
		owner:             params.Owner,
		datasourceUID:     params.DatasourceUID,
		queryKind:         params.QueryKind,
		scope:             cloneScope(params.Scope),
		status:            StatusGenerating,
		maxRepairAttempts: params.MaxRepairAttempts,
		knownGauges:       gauges,
		createdAt:         now,
		updatedAt:         now,
		expiresAt:         now.Add(limits.JobTTL),
		activeOpToken:     1,
		activeOpUntil:     leaseUntil,
		activeSlotID:      slotID,
	}
	s.jobs[params.JobID] = rec
	s.acquireSlotLocked(params.Owner, slotID, leaseUntil)
	return OpLease{OpToken: rec.activeOpToken, SlotID: slotID}, nil
}

func (s *MemoryStore) CreateImported(_ context.Context, params CreateImportedParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	limits := params.Limits.normalized()
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictExpiredLocked(now)
	if err := s.ensureImportCapacityLocked(params.Owner, now, limits); err != nil {
		return JobSnapshot{}, err
	}

	gauges := make(map[string]bool, len(params.KnownGauges))
	for _, g := range params.KnownGauges {
		if g != "" {
			gauges[g] = true
		}
	}
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
		expiresAt:         now.Add(limits.JobTTL),
		activeOpToken:     1,
	}
	s.jobs[params.JobID] = rec
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) Get(_ context.Context, owner Owner, jobID string, now time.Time) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
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
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return RepairCandidate{}, err
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
	limits := params.Limits.normalized()
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
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
	if rec.currentIR == nil {
		return BeginOpResult{}, errors.New("job has no active IR draft to patch")
	}
	if params.BaseRevision != rec.currentIR.Revision {
		return BeginOpResult{}, formatRevisionConflict("", params.BaseRevision, rec.currentIR.Revision)
	}
	if params.TargetPanelID != "" && !panelExists(rec.currentIR.Panels, params.TargetPanelID) {
		return BeginOpResult{}, fmt.Errorf("%w: %q", ir.ErrPanelNotFound, params.TargetPanelID)
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
	for _, g := range params.CatalogGauges {
		if g != "" {
			rec.knownGauges[g] = true
		}
	}

	leaseUntil := now.Add(limits.OpLeaseTTL)
	rec.activeOpToken++
	rec.activeOpUntil = leaseUntil
	rec.activeSlotID = slotID
	rec.status = StatusGenerating
	rec.updatedAt = now
	rec.expiresAt = now.Add(limits.JobTTL)
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
	limits := params.Limits.normalized()
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
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
	for _, g := range params.CatalogGauges {
		if g != "" {
			rec.knownGauges[g] = true
		}
	}

	// Atomically consume repair budget BEFORE calling LLM so concurrent requests across replicas cannot bypass budget.
	leaseUntil := now.Add(limits.OpLeaseTTL)
	rec.repairAttemptsUsed++
	rec.activeOpToken++
	rec.activeOpUntil = leaseUntil
	rec.activeSlotID = slotID
	rec.status = StatusRepairing
	rec.updatedAt = now
	rec.expiresAt = now.Add(limits.JobTTL)
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
	if now.IsZero() {
		now = time.Now()
	}
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if rec.owner != owner {
		return ErrForbidden
	}
	if rec.activeOpToken == opToken {
		rec.status = status
		rec.updatedAt = now
		rec.expiresAt = now.Add(ttl)
	}
	return nil
}

func (s *MemoryStore) FinishJob(_ context.Context, params FinishJobParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
	ttl := normalizeJobTTL(params.JobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.jobs[params.JobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.owner != params.Owner {
		return JobSnapshot{}, ErrForbidden
	}
	if rec.activeOpToken != params.OpToken {
		return memSnapshotLocked(rec), ErrJobCancelled
	}
	rec.activeOpUntil = time.Time{}
	rec.activeSlotID = ""
	rec.status = params.Status
	if params.Doc != nil {
		c := params.Doc.Clone()
		rec.currentIR = &c
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
	rec.expiresAt = now.Add(ttl)
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) CommitRevision(_ context.Context, params CommitRevisionParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
	ttl := normalizeJobTTL(params.JobTTL)
	maxHist := params.MaxUndoHistory
	if maxHist <= 0 {
		maxHist = defaultMaxUndoHistory
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.jobs[params.JobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.owner != params.Owner {
		return JobSnapshot{}, ErrForbidden
	}
	if rec.activeOpToken != params.OpToken {
		return memSnapshotLocked(rec), ErrJobCancelled
	}
	if rec.currentIR == nil || rec.currentIR.Revision != params.ExpectedRev {
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
	rec.activeOpUntil = time.Time{}
	rec.activeSlotID = ""
	if params.LLMMeta != nil {
		mc := *params.LLMMeta
		rec.llmMeta = &mc
	}
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl)
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) RecordEditFailure(_ context.Context, params RecordEditFailureParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
	ttl := normalizeJobTTL(params.JobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.jobs[params.JobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.owner != params.Owner {
		return JobSnapshot{}, ErrForbidden
	}
	if rec.activeOpToken != params.OpToken {
		return memSnapshotLocked(rec), ErrJobCancelled
	}
	rec.activeOpUntil = time.Time{}
	rec.activeSlotID = ""
	rec.status = StatusFailed
	rec.errorMessage = params.ErrorMessage
	if params.LLMMeta != nil {
		mc := *params.LLMMeta
		rec.llmMeta = &mc
	}
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl)
	return memSnapshotLocked(rec), errors.New(params.ErrorMessage)
}

func (s *MemoryStore) RevertOnCancel(_ context.Context, owner Owner, jobID string, opToken uint64, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.jobs[jobID]
	if !ok {
		return JobSnapshot{}, ErrJobNotFound
	}
	if rec.owner != owner {
		return JobSnapshot{}, ErrForbidden
	}
	if rec.activeOpToken == opToken {
		rec.activeOpUntil = time.Time{}
		rec.activeSlotID = ""
		if rec.currentIR != nil {
			rec.status = StatusAwaitingPreview
		} else {
			rec.status = StatusCancelled
		}
		rec.summary = "処理がキャンセルされたためドラフトは変更されていません。"
		rec.updatedAt = now
		rec.expiresAt = now.Add(ttl)
	}
	return memSnapshotLocked(rec), ErrJobCancelled
}

func (s *MemoryStore) Undo(_ context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	if len(rec.history) == 0 || rec.currentIR == nil {
		return JobSnapshot{}, ErrNoUndoHistory
	}
	if rec.currentIR.Revision >= ir.MaxRevision {
		return JobSnapshot{}, fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	}

	// Bump fencing token and clear active op lease so any stale in-flight Patch/Repair on any replica is invalidated.
	rec.activeOpToken++
	rec.activeOpUntil = time.Time{}
	rec.activeSlotID = ""

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
	rec.expiresAt = now.Add(ttl)
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) Cancel(_ context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	ttl := normalizeJobTTL(jobTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getOwnedLocked(owner, jobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	rec.activeOpToken++
	rec.activeOpUntil = time.Time{}
	rec.activeSlotID = ""
	if rec.currentIR == nil {
		rec.status = StatusCancelled
	} else {
		rec.status = StatusAwaitingPreview
	}
	rec.summary = "実行中の処理をキャンセルしました。"
	rec.updatedAt = now
	rec.expiresAt = now.Add(ttl)
	return memSnapshotLocked(rec), nil
}

func (s *MemoryStore) ReleaseSlot(_ context.Context, owner Owner, slotID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.globalSlots, slotID)
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
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(now)
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
	s.evictExpiredLocked(now)
	rec, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrJobNotFound
	}
	if rec.owner.OrgID != owner.OrgID || rec.owner.UserLogin != owner.UserLogin {
		return nil, ErrForbidden
	}
	return rec, nil
}

func (s *MemoryStore) evictExpiredLocked(now time.Time) {
	for id, rec := range s.jobs {
		if rec.isExpired(now) {
			delete(s.jobs, id)
		}
	}
}

func (s *MemoryStore) reapExpiredSlotsLocked(now time.Time) {
	for id, exp := range s.globalSlots {
		if !now.Before(exp) {
			delete(s.globalSlots, id)
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
	if len(s.globalSlots) >= limits.MaxConcurrentJobs {
		return false
	}
	if len(s.userSlots[owner]) >= limits.MaxConcurrentJobsPerUser {
		return false
	}
	return true
}

func (s *MemoryStore) acquireSlotLocked(owner Owner, slotID string, leaseUntil time.Time) {
	s.globalSlots[slotID] = leaseUntil
	if s.userSlots[owner] == nil {
		s.userSlots[owner] = make(map[string]time.Time)
	}
	s.userSlots[owner][slotID] = leaseUntil
}

// ensureCapacityLocked enforces per-user and global job limits while NEVER evicting
// any currently active (in-flight) job.
func (s *MemoryStore) ensureCapacityLocked(caller Owner, now time.Time, limits StoreLimits) error {
	userCount := 0
	var callerOldestInactive string
	var callerOldestTime time.Time
	for id, rec := range s.jobs {
		if rec.owner == caller {
			userCount++
			if !rec.isActive(now) && (callerOldestInactive == "" || rec.updatedAt.Before(callerOldestTime)) {
				callerOldestInactive = id
				callerOldestTime = rec.updatedAt
			}
		}
	}
	if userCount >= limits.MaxJobsPerUser {
		if callerOldestInactive == "" {
			return ErrConcurrencyLimited
		}
		delete(s.jobs, callerOldestInactive)
	}

	if len(s.jobs) < limits.MaxJobs {
		return nil
	}
	// Try evicting caller's own oldest inactive job first.
	var callerNextOldest string
	var callerNextTime time.Time
	for id, rec := range s.jobs {
		if rec.owner == caller && !rec.isActive(now) && (callerNextOldest == "" || rec.updatedAt.Before(callerNextTime)) {
			callerNextOldest = id
			callerNextTime = rec.updatedAt
		}
	}
	if callerNextOldest != "" {
		delete(s.jobs, callerNextOldest)
		return nil
	}
	var oldestInactive string
	var oldestTime time.Time
	for id, rec := range s.jobs {
		if !rec.isActive(now) && (oldestInactive == "" || rec.updatedAt.Before(oldestTime)) {
			oldestInactive = id
			oldestTime = rec.updatedAt
		}
	}
	if oldestInactive == "" {
		return ErrConcurrencyLimited
	}
	delete(s.jobs, oldestInactive)
	return nil
}

// ensureImportCapacityLocked enforces per-user and global caps for synchronous Import jobs.
// Unlike Generate, Import does not consume an LLM concurrency slot, so it MUST NEVER evict
// another user's non-expired job (preventing cross-user job-eviction DoS).
func (s *MemoryStore) ensureImportCapacityLocked(caller Owner, now time.Time, limits StoreLimits) error {
	userCount := 0
	var callerOldestInactive string
	var callerOldestTime time.Time
	for id, rec := range s.jobs {
		if rec.owner == caller {
			userCount++
			if !rec.isActive(now) && (callerOldestInactive == "" || rec.updatedAt.Before(callerOldestTime)) {
				callerOldestInactive = id
				callerOldestTime = rec.updatedAt
			}
		}
	}
	if userCount >= limits.MaxJobsPerUser {
		if callerOldestInactive == "" {
			return ErrConcurrencyLimited
		}
		delete(s.jobs, callerOldestInactive)
	}
	if len(s.jobs) < limits.MaxJobs {
		return nil
	}
	var callerNextOldest string
	var callerNextTime time.Time
	for id, rec := range s.jobs {
		if rec.owner == caller && !rec.isActive(now) && (callerNextOldest == "" || rec.updatedAt.Before(callerNextTime)) {
			callerNextOldest = id
			callerNextTime = rec.updatedAt
		}
	}
	if callerNextOldest == "" {
		return ErrConcurrencyLimited
	}
	delete(s.jobs, callerNextOldest)
	return nil
}

func memSnapshotLocked(rec *memJobRecord) JobSnapshot {
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
	qKind := rec.queryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:              rec.id,
		Status:             rec.status,
		DatasourceUID:      rec.datasourceUID,
		QueryKind:          string(qKind),
		Scope:              cloneScope(rec.scope),
		ImportSource:       cloneImportSource(rec.importSource),
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
