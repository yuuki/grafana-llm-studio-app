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
	"github.com/yuuki/grafana-llm-studio-app/pkg/metricsql"
	"github.com/yuuki/grafana-llm-studio-app/pkg/promql"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var (
	ErrJobNotFound                    = errors.New("job not found")
	ErrForbidden                      = errors.New("forbidden: job belongs to another user or organization")
	ErrBudgetExhausted                = errors.New("job repair budget exhausted")
	ErrConcurrencyLimited             = errors.New("too many concurrent LLM jobs")
	ErrOperationInProgress            = errors.New("another operation is already in progress for this job")
	ErrNotRepairable                  = errors.New("validation findings are not eligible for automatic repair")
	ErrNoUndoHistory                  = errors.New("no previous revision available to undo")
	ErrDatasourceDenied               = errors.New("selected datasource UID is not in admin allowlist")
	ErrJobCancelled                   = errors.New("job was cancelled")
	ErrStoreUnavailable               = errors.New("job store unavailable")
	ErrWrongJobKind                   = errors.New("operation is not supported for this job kind")
	ErrReportStorageMigrationRequired = errors.New("postgresql job store requires migration to store status reports")
)

const (
	cancelSubscribeTimeout = 3 * time.Second
	maxFencedTokenEntries  = 1024
)

// Owner identifies the authenticated Grafana caller from trusted PluginContext only.
type Owner struct {
	OrgID     int64
	UserLogin string
}

// JobStatus represents the current workflow phase of a studio draft or status report job.
type JobStatus string

const (
	StatusGenerating          JobStatus = "generating"
	StatusValidatingIR        JobStatus = "validating_ir"
	StatusAwaitingPreview     JobStatus = "awaiting_preview"
	StatusRepairing           JobStatus = "repairing"
	StatusAnalyzing           JobStatus = "analyzing"
	StatusValidatingReport    JobStatus = "validating_report"
	StatusReady               JobStatus = "ready"
	StatusInsufficientMetrics JobStatus = "insufficient_metrics"
	StatusCancelled           JobStatus = "cancelled"
	StatusFailed              JobStatus = "failed"
)

// Config configures bounded job management backed by a pluggable JobStore.
// When Store is nil, NewManager defaults to the zero-dependency in-memory MemoryStore.
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
	// OpLeaseTTL overrides the per-operation distributed lock/slot lease TTL.
	// Defaults to JobDeadline + 15s so crashed replicas cannot permanently hold slots.
	OpLeaseTTL         time.Duration
	AllowedDSUIDs      []string
	DatasourceDialects map[string]string
	QueryRegistry      *query.Registry
	// Store optionally supplies a persistent / multi-replica JobStore (e.g. RedisStore or PostgresStore).
	// When nil, NewManager creates a process-local MemoryStore.
	Store JobStore
	// NowFunc optionally overrides wall-clock time for deterministic tests.
	// When nil, stores use their authoritative clock (time.Now for MemoryStore, Redis TIME for RedisStore,
	// and PostgreSQL clock_timestamp() for PostgresStore).
	NowFunc func() time.Time
}

func (c *Config) normalize() {
	if c.MaxJobs <= 0 {
		c.MaxJobs = defaultMaxJobs
	}
	if c.MaxJobsPerUser <= 0 {
		c.MaxJobsPerUser = defaultMaxJobsPerUser
	}
	if c.JobTTL <= 0 {
		c.JobTTL = defaultJobTTL
	}
	if c.MaxConcurrentJobs <= 0 {
		c.MaxConcurrentJobs = defaultMaxConcurrentJobs
	}
	if c.MaxConcurrentJobsPerUser <= 0 {
		c.MaxConcurrentJobsPerUser = defaultMaxConcurrentJobsPerUser
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
	if c.OpLeaseTTL <= 0 {
		c.OpLeaseTTL = c.JobDeadline + defaultOpLeaseGrace
	}
}

func (c Config) storeLimits() StoreLimits {
	return StoreLimits{
		MaxJobs:                  c.MaxJobs,
		MaxJobsPerUser:           c.MaxJobsPerUser,
		JobTTL:                   c.JobTTL,
		MaxConcurrentJobs:        c.MaxConcurrentJobs,
		MaxConcurrentJobsPerUser: c.MaxConcurrentJobsPerUser,
		MaxUndoHistory:           defaultMaxUndoHistory,
		OpLeaseTTL:               c.OpLeaseTTL,
	}
}

// JobSnapshot is the safe API view of a job returned to its owner.
type JobSnapshot struct {
	JobID              string              `json:"jobId"`
	JobKind            string              `json:"jobKind,omitempty"`
	Status             JobStatus           `json:"status"`
	DatasourceUID      string              `json:"datasourceUid"`
	QueryKind          string              `json:"queryKind,omitempty"`
	Scope              *ir.ScopeFilter     `json:"scope,omitempty"`
	ImportSource       *ir.ImportSource    `json:"importSource,omitempty"`
	CurrentIR          *ir.DashboardIR     `json:"currentIR,omitempty"`
	StatusReport       *ir.StatusReportIR  `json:"statusReport,omitempty"`
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

type localCancelEntry struct {
	opToken uint64
	cancel  context.CancelFunc
}

// Manager coordinates bounded draft jobs with strict per-user/org isolation across
// either a process-local MemoryStore (default) or a shared persistent JobStore for HA Grafana.
// Notice: metric catalogs are NEVER stored in a cross-user shared index; only job-private
// metric type guards (knownGauges) stay scoped to the single job owner and expire with the job.
type Manager struct {
	cfg           Config
	client        llm.Client
	registry      *query.Registry
	store         JobStore
	ownsStore     bool
	customNow     func() time.Time
	localMu       sync.Mutex
	localCancels  map[string]localCancelEntry
	latestFenced  map[string]uint64
	opWG          sync.WaitGroup
	bgWG          sync.WaitGroup
	stopCh        chan struct{}
	stopCancelSub func()
	closeOnce     sync.Once
}

// NewManager creates a new bounded job manager.
func NewManager(client llm.Client, cfg Config) *Manager {
	cfg.normalize()
	reg := cfg.QueryRegistry
	if reg == nil {
		reg = query.NewRegistry(
			promql.NewAdapter(promql.DefaultPolicy()),
			metricsql.NewAdapter(metricsql.DefaultPolicy()),
		)
	}
	store := cfg.Store
	ownsStore := false
	if store == nil {
		store = NewMemoryStore()
		ownsStore = true
	}
	m := &Manager{
		cfg:          cfg,
		client:       client,
		registry:     reg,
		store:        store,
		ownsStore:    ownsStore,
		customNow:    cfg.NowFunc,
		localCancels: make(map[string]localCancelEntry),
		latestFenced: make(map[string]uint64),
		stopCh:       make(chan struct{}),
	}

	// Subscribe to cross-replica Cancel/Undo fencing events when supported by the JobStore
	// (e.g. RedisStore or PostgresStore). Try synchronously first so healthy stores are armed
	// immediately, and fall back to bounded background retry if the store is transiently unreachable.
	if sub, ok := store.(CancelSubscription); ok {
		onCancel := func(jobID string, fencedToken uint64) {
			m.cancelLocalOp(jobID, fencedToken)
		}
		subCtx, cancel := context.WithTimeout(context.Background(), cancelSubscribeTimeout)
		stopFn, err := sub.SubscribeCancels(subCtx, onCancel)
		cancel()
		if err == nil && stopFn != nil {
			m.localMu.Lock()
			m.stopCancelSub = stopFn
			m.localMu.Unlock()
		} else {
			m.bgWG.Add(1)
			go func() {
				defer m.bgWG.Done()
				retryCtx, retryCancel := context.WithCancel(context.Background())
				retryDone := make(chan struct{})
				go func() {
					select {
					case <-m.stopCh:
						retryCancel()
					case <-retryDone:
					}
				}()
				defer func() {
					close(retryDone)
					retryCancel()
				}()

				backoff := 500 * time.Millisecond
				const maxBackoff = 5 * time.Second
				for {
					timer := time.NewTimer(jitterDuration(backoff))
					select {
					case <-m.stopCh:
						timer.Stop()
						return
					case <-timer.C:
					}
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					attemptCtx, attemptCancel := context.WithTimeout(retryCtx, cancelSubscribeTimeout)
					fn, subErr := sub.SubscribeCancels(attemptCtx, onCancel)
					attemptCancel()
					if subErr == nil && fn != nil {
						m.localMu.Lock()
						select {
						case <-m.stopCh:
							m.localMu.Unlock()
							fn()
							return
						default:
							m.stopCancelSub = fn
							m.localMu.Unlock()
							return
						}
					}
				}
			}()
		}
	}

	// Start periodic background cleanup for idle MemoryStore/RedisStore/PostgresStore expiration when using real wall clock.
	if m.customNow == nil && cfg.JobTTL > 0 {
		interval := cfg.JobTTL / 2
		if interval < 10*time.Second {
			interval = 10 * time.Second
		}
		if interval > 5*time.Minute {
			interval = 5 * time.Minute
		}
		m.bgWG.Add(1)
		go func() {
			defer m.bgWG.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-m.stopCh:
					return
				case <-ticker.C:
					cleanCtx, cancel := context.WithTimeout(context.Background(), defaultStoreOpTimeout)
					_ = m.store.CleanupExpired(cleanCtx, time.Time{})
					cancel()
				}
			}
		}()
	}

	return m
}

func (m *Manager) now() time.Time {
	if m.customNow != nil {
		return m.customNow()
	}
	return time.Now()
}

// storeNow returns the explicit test clock if configured, or zero time.Time{} so RedisStore/PostgresStore
// use authoritative server time in production across HA replicas.
func (m *Manager) storeNow() time.Time {
	if m.customNow != nil {
		return m.customNow()
	}
	return time.Time{}
}

// Ping verifies connectivity to the underlying JobStore.
func (m *Manager) Ping(ctx context.Context) error {
	return m.store.Ping(ctx)
}

// CleanupExpired triggers explicit cleanup of expired non-active jobs and slot leases.
func (m *Manager) CleanupExpired(ctx context.Context) error {
	return m.store.CleanupExpired(ctx, m.storeNow())
}

// Close cancels all active in-flight local job contexts during plugin shutdown/reload,
// waits up to defaultStoreOpTimeout for in-flight goroutines to finish releasing slots
// and reverting state, and stops background helpers.
// If the Manager created its own default MemoryStore, Close also closes it; externally
// supplied stores remain owned by the caller.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.stopCh)
		m.bgWG.Wait()

		m.localMu.Lock()
		stopSub := m.stopCancelSub
		m.stopCancelSub = nil
		m.localMu.Unlock()
		if stopSub != nil {
			stopSub()
		}

		m.localMu.Lock()
		cancels := make([]context.CancelFunc, 0, len(m.localCancels))
		for id, entry := range m.localCancels {
			if entry.cancel != nil {
				cancels = append(cancels, entry.cancel)
			}
			delete(m.localCancels, id)
		}
		m.localMu.Unlock()

		for _, cancel := range cancels {
			cancel()
		}

		// Wait with bounded timeout for cancelled in-flight operations to complete ReleaseSlot / RevertOnCancel
		// before App.Dispose() closes the shared Redis client.
		done := make(chan struct{})
		go func() {
			m.opWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(defaultStoreOpTimeout):
		}

		if m.ownsStore {
			_ = m.store.Close()
		}
	})
}

// GenerateInput is the request payload for starting a new draft generation job.
type GenerateInput struct {
	DatasourceUID string                 `json:"datasourceUid"`
	QueryKind     string                 `json:"queryKind,omitempty"`
	Scope         *ir.ScopeFilter        `json:"scope,omitempty"`
	UserIntent    string                 `json:"userIntent"`
	Catalog       []ir.MetricCatalogItem `json:"catalog"`
}

// ImportInput is the request payload for creating a constrained draft job from an existing dashboard.
type ImportInput struct {
	DatasourceUID string                 `json:"datasourceUid"`
	QueryKind     string                 `json:"queryKind,omitempty"`
	Scope         *ir.ScopeFilter        `json:"scope,omitempty"`
	Draft         ir.DashboardIR         `json:"draft"`
	ImportSource  *ir.ImportSource       `json:"importSource,omitempty"`
	Catalog       []ir.MetricCatalogItem `json:"catalog,omitempty"`
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

// Undo restores the immediately preceding IR revision as a new monotonic revision,
// cancelling any in-flight operation so late failures cannot overwrite the restored draft.
func (m *Manager) Undo(owner Owner, jobID string) (JobSnapshot, error) {
	return m.UndoContext(context.Background(), owner, jobID)
}

// UndoContext is the context-aware variant of Undo.
func (m *Manager) UndoContext(ctx context.Context, owner Owner, jobID string) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	res, err := m.store.Undo(ctx, owner, jobID, m.storeNow(), m.cfg.JobTTL)
	if err != nil {
		return JobSnapshot{}, err
	}
	// Cancel only local in-flight LLM goroutines older than res.OpToken AFTER ownership check succeeded.
	m.cancelLocalOp(jobID, res.OpToken)
	return res.Snapshot, nil
}

// Cancel aborts any in-flight LLM operation for an owner's job and prevents stale results from applying.
func (m *Manager) Cancel(owner Owner, jobID string) (JobSnapshot, error) {
	return m.CancelContext(context.Background(), owner, jobID)
}

// CancelContext is the context-aware variant of Cancel.
func (m *Manager) CancelContext(ctx context.Context, owner Owner, jobID string) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	res, err := m.store.Cancel(ctx, owner, jobID, m.storeNow(), m.cfg.JobTTL)
	if err != nil {
		return JobSnapshot{}, err
	}
	// Cancel only local in-flight LLM goroutines older than res.OpToken AFTER ownership check succeeded.
	m.cancelLocalOp(jobID, res.OpToken)
	return res.Snapshot, nil
}

// Get returns the current state of an owner's job.
func (m *Manager) Get(owner Owner, jobID string) (JobSnapshot, error) {
	return m.GetContext(context.Background(), owner, jobID)
}

// GetContext is the context-aware variant of Get.
func (m *Manager) GetContext(ctx context.Context, owner Owner, jobID string) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	return m.store.Get(ctx, owner, jobID, m.storeNow())
}

func (m *Manager) finishJobWithStage(
	parentCtx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	operation string,
	status JobStatus,
	doc *ir.DashboardIR,
	diffs []ir.DiffEntry,
	summary string,
	warnings []string,
	errMsg string,
	meta *llm.CompletionMeta,
) (JobSnapshot, error) {
	storeCtx, cancel := detachedStoreContext(parentCtx)
	defer cancel()
	snap, err := m.store.FinishJob(storeCtx, FinishJobParams{
		Owner:        owner,
		JobID:        jobID,
		OpToken:      opToken,
		Status:       status,
		Doc:          doc,
		Diffs:        diffs,
		Summary:      summary,
		Warnings:     warnings,
		ErrorMessage: errMsg,
		LLMMeta:      meta,
		Now:          m.storeNow(),
		JobTTL:       m.cfg.JobTTL,
	})
	switch {
	case err == nil && snap.Status != "":
		m.emitStage(parentCtx, jobID, operation, snap.Status)
	case errors.Is(err, ErrJobCancelled):
		m.emitStage(parentCtx, jobID, operation, StatusCancelled)
	case err != nil:
		m.emitStage(parentCtx, jobID, operation, StatusFailed)
	}
	return snap, err
}

func (m *Manager) commitRevisionWithStage(
	parentCtx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	operation string,
	expectedRev int,
	nextIR ir.DashboardIR,
	diffs []ir.DiffEntry,
	summary string,
	warnings []string,
	meta *llm.CompletionMeta,
) (JobSnapshot, error) {
	storeCtx, cancel := detachedStoreContext(parentCtx)
	defer cancel()
	snap, err := m.store.CommitRevision(storeCtx, CommitRevisionParams{
		Owner:          owner,
		JobID:          jobID,
		OpToken:        opToken,
		ExpectedRev:    expectedRev,
		NextIR:         nextIR,
		Diffs:          diffs,
		Summary:        summary,
		Warnings:       warnings,
		LLMMeta:        meta,
		Now:            m.storeNow(),
		JobTTL:         m.cfg.JobTTL,
		MaxUndoHistory: defaultMaxUndoHistory,
	})
	switch {
	case err == nil && snap.Status != "":
		m.emitStage(parentCtx, jobID, operation, snap.Status)
	case errors.Is(err, ErrJobCancelled):
		m.emitStage(parentCtx, jobID, operation, StatusCancelled)
	case err != nil:
		m.emitStage(parentCtx, jobID, operation, StatusFailed)
	}
	return snap, err
}

func (m *Manager) revertToPreviousOnCancelWithStage(parentCtx context.Context, owner Owner, jobID string, opToken uint64, operation string) (JobSnapshot, error) {
	storeCtx, cancel := detachedStoreContext(parentCtx)
	defer cancel()
	snap, err := m.store.RevertOnCancel(storeCtx, owner, jobID, opToken, m.storeNow(), m.cfg.JobTTL)
	m.emitStage(parentCtx, jobID, operation, StatusCancelled)
	return snap, err
}

func (m *Manager) recordEditFailureWithStage(
	parentCtx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	operation string,
	cause error,
	errMsg string,
	meta *llm.CompletionMeta,
) (JobSnapshot, error) {
	storeCtx, cancel := detachedStoreContext(parentCtx)
	defer cancel()
	snap, storeErr := m.store.RecordEditFailure(storeCtx, RecordEditFailureParams{
		Owner:        owner,
		JobID:        jobID,
		OpToken:      opToken,
		ErrorMessage: errMsg,
		LLMMeta:      meta,
		Now:          m.storeNow(),
		JobTTL:       m.cfg.JobTTL,
	})
	if storeErr != nil {
		if errors.Is(storeErr, ErrJobCancelled) {
			m.emitStage(parentCtx, jobID, operation, StatusCancelled)
		} else {
			m.emitStage(parentCtx, jobID, operation, StatusFailed)
		}
		return snap, storeErr
	}
	m.emitStage(parentCtx, jobID, operation, StatusFailed)
	if cause != nil {
		if errMsg == "" || errMsg == cause.Error() {
			return snap, cause
		}
		return snap, fmt.Errorf("%s: %w", errMsg, cause)
	}
	return snap, errors.New(errMsg)
}

func (m *Manager) setStatusIfToken(parentCtx context.Context, owner Owner, jobID string, opToken uint64, status JobStatus) {
	storeCtx, cancel := detachedStoreContext(parentCtx)
	defer cancel()
	_ = m.store.SetStatus(storeCtx, owner, jobID, opToken, status, m.storeNow(), m.cfg.JobTTL)
}

func (m *Manager) releaseSlot(parentCtx context.Context, owner Owner, slotID string) {
	if slotID == "" {
		return
	}
	storeCtx, cancel := detachedStoreContext(parentCtx)
	defer cancel()
	_ = m.store.ReleaseSlot(storeCtx, owner, slotID, m.storeNow())
}

func (m *Manager) registerLocalCancel(jobID string, opToken uint64, cancel context.CancelFunc) {
	m.localMu.Lock()
	alreadyFenced := m.latestFenced[jobID] >= opToken
	if !alreadyFenced {
		m.localCancels[jobID] = localCancelEntry{opToken: opToken, cancel: cancel}
	}
	m.localMu.Unlock()
	if alreadyFenced && cancel != nil {
		cancel()
	}
}

func (m *Manager) unregisterLocalCancel(jobID string, opToken uint64) {
	m.localMu.Lock()
	if entry, ok := m.localCancels[jobID]; ok && entry.opToken == opToken {
		delete(m.localCancels, jobID)
	}
	m.localMu.Unlock()
}

func (m *Manager) cancelLocalOp(jobID string, fencedToken uint64) {
	m.localMu.Lock()
	if fencedToken > m.latestFenced[jobID] {
		if len(m.latestFenced) >= maxFencedTokenEntries {
			// Clear stale fencing cache when bounded cap is reached.
			m.latestFenced = make(map[string]uint64, maxFencedTokenEntries/2)
		}
		m.latestFenced[jobID] = fencedToken
	}
	entry, ok := m.localCancels[jobID]
	shouldCancel := ok && entry.opToken < fencedToken
	if shouldCancel {
		delete(m.localCancels, jobID)
	}
	m.localMu.Unlock()
	if shouldCancel && entry.cancel != nil {
		entry.cancel()
	}
}

func detachedStoreContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), defaultStoreOpTimeout)
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

// resolveAdapter determines the authoritative query dialect for dsUID:
// admin-configured DatasourceDialects[dsUID] takes precedence over client-supplied queryKind.
func (m *Manager) resolveAdapter(dsUID string, clientKind string) (query.Kind, query.Adapter, error) {
	if pinned, ok := m.cfg.DatasourceDialects[dsUID]; ok && strings.TrimSpace(pinned) != "" {
		k, err := query.ParseKind(pinned)
		if err != nil {
			return "", nil, err
		}
		adapter, err := m.registry.Get(k)
		if err != nil {
			return "", nil, err
		}
		return k, adapter, nil
	}
	k, err := query.ParseKind(clientKind)
	if err != nil {
		return "", nil, err
	}
	adapter, err := m.registry.Get(k)
	if err != nil {
		return "", nil, err
	}
	return k, adapter, nil
}

func panelExists(panels []ir.PanelIR, panelID string) bool {
	for _, p := range panels {
		if p.ID == panelID {
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

func cloneImportSource(s *ir.ImportSource) *ir.ImportSource {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "job-" + hex.EncodeToString(b)
}
