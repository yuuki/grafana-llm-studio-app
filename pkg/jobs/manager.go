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
	"github.com/yuuki/grafana-llm-studio-app/pkg/prompt"
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

// Generate creates a new owner-bound job, calls the configured LLM, and validates the IR + queries.
func (m *Manager) Generate(ctx context.Context, owner Owner, in GenerateInput) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	dsUID := strings.TrimSpace(in.DatasourceUID)
	if !m.isDatasourceAllowed(dsUID) {
		return JobSnapshot{}, fmt.Errorf("%w: %q", ErrDatasourceDenied, dsUID)
	}
	qKind, adapter, err := m.resolveAdapter(dsUID, in.QueryKind)
	if err != nil {
		return JobSnapshot{}, err
	}
	if strings.TrimSpace(in.UserIntent) == "" {
		return JobSnapshot{}, errors.New("userIntent is required")
	}
	if err := adapter.ValidateScopeFilter(in.Scope); err != nil {
		return JobSnapshot{}, err
	}
	safeScope := prompt.SanitizeScope(in.Scope)
	boundedCatalog := prompt.BoundCatalog(in.Catalog)

	// Do NOT allocate a job or trigger map eviction for empty catalogs (prevents eviction DoS).
	if len(boundedCatalog) == 0 {
		return JobSnapshot{
			Status:            StatusInsufficientMetrics,
			DatasourceUID:     dsUID,
			QueryKind:         string(qKind),
			Scope:             safeScope,
			MaxRepairAttempts: m.cfg.MaxRepairAttempts,
			ErrorMessage:      "選択スコープに利用可能なメトリクス候補がありません。メトリクス名を捏造せず中断しました。",
			UpdatedAt:         m.now().UTC(),
		}, nil
	}

	jobID := newJobID()
	slotID := newSlotID()
	opCtx, cancel := context.WithTimeout(ctx, m.cfg.JobDeadline)
	defer cancel()

	m.opWG.Add(1)
	defer m.opWG.Done()

	lease, err := m.store.CreateGenerating(ctx, CreateGeneratingParams{
		JobID:             jobID,
		Owner:             owner,
		DatasourceUID:     dsUID,
		QueryKind:         qKind,
		Scope:             cloneScope(safeScope),
		MaxRepairAttempts: m.cfg.MaxRepairAttempts,
		KnownGauges:       extractGaugeNames(boundedCatalog),
		SlotID:            slotID,
		Now:               m.storeNow(),
		Limits:            m.cfg.storeLimits(),
	})
	if err != nil {
		if errors.Is(err, ErrStoreUnavailable) {
			m.releaseSlot(ctx, owner, slotID)
		}
		return JobSnapshot{}, err
	}
	opToken := lease.OpToken
	m.registerLocalCancel(jobID, opToken, cancel)
	defer m.unregisterLocalCancel(jobID, opToken)
	defer m.releaseSlot(ctx, owner, lease.SlotID)

	m.emitStage(ctx, jobID, OperationGenerate, StatusGenerating)

	raw, meta, err := m.client.GenerateIR(opCtx, llm.GenerateRequest{
		UserIntent:   in.UserIntent,
		Scope:        cloneScope(safeScope),
		Catalog:      boundedCatalog,
		MaxPanels:    m.cfg.MaxPanels,
		QueryKind:    string(qKind),
		DialectRules: adapter.PromptRules(),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
			return m.finishJobWithStage(ctx, owner, jobID, opToken, OperationGenerate, StatusCancelled, nil, nil, "", nil, "生成はキャンセルされました。", &meta)
		}
		if errors.Is(err, llm.ErrInsufficientMetrics) {
			return m.finishJobWithStage(ctx, owner, jobID, opToken, OperationGenerate, StatusInsufficientMetrics, nil, nil, "", nil, "要求に合致するメトリクスが選択スコープ内に見つかりませんでした。", &meta)
		}
		return m.finishJobWithStage(ctx, owner, jobID, opToken, OperationGenerate, StatusFailed, nil, nil, "", nil, err.Error(), &meta)
	}

	m.setStatusIfToken(ctx, owner, jobID, opToken, StatusValidatingIR)
	m.emitStage(ctx, jobID, OperationGenerate, StatusValidatingIR)

	doc, err := ir.DecodeStrictIR(raw)
	if err != nil {
		return m.finishJobWithStage(ctx, owner, jobID, opToken, OperationGenerate, StatusFailed, nil, nil, "", nil, fmt.Sprintf("IR JSON Schema 検査に失敗しました: %v", err), &meta)
	}
	doc.Revision = 1
	if qKind != query.KindPromQL {
		doc.StampQueryKind(string(qKind))
	}

	warnings, err := query.ValidateDashboardIR(
		adapter,
		doc,
		boundedCatalog,
		safeScope,
		ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels},
		query.ValidateOptions{RequireCatalogMatch: true},
	)
	if err != nil {
		if errors.Is(err, query.ErrMetricNotInCatalog) {
			return m.finishJobWithStage(ctx, owner, jobID, opToken, OperationGenerate, StatusInsufficientMetrics, nil, nil, "", nil, fmt.Sprintf("カタログ外のメトリクス参照を検出し拒否しました: %v", err), &meta)
		}
		return m.finishJobWithStage(ctx, owner, jobID, opToken, OperationGenerate, StatusFailed, nil, nil, "", nil, fmt.Sprintf("IR/%s 検査に失敗しました: %v", adapter.DisplayName(), err), &meta)
	}

	return m.finishJobWithStage(ctx, owner, jobID, opToken, OperationGenerate, StatusAwaitingPreview, &doc, nil, "初期ドラフトを生成しました。検証・プレビューを実行して内容を確認してください。", warnings, "", &meta)
}

// AnalyzeStatus validates an operational evidence bundle, executes a single grounded LLM
// completion via grafana-llm-app (or explicit dev mock), verifies fail-closed StatusReportIR
// invariants and evidence citation grounding, redacts any echoed secrets, and persists ONLY
// the validated StatusReportIR (never raw input evidence) under owner isolation with TTL.
func (m *Manager) AnalyzeStatus(ctx context.Context, owner Owner, in ir.AnalyzeStatusPayload) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}

	dsUID := strings.TrimSpace(in.DatasourceUID)
	if len(in.Evidence.Metrics) > 0 || in.DashboardContext != nil {
		if dsUID == "" || !m.isDatasourceAllowed(dsUID) {
			return JobSnapshot{}, fmt.Errorf("%w: %q", ErrDatasourceDenied, dsUID)
		}
	} else if dsUID != "" && !m.isDatasourceAllowed(dsUID) {
		return JobSnapshot{}, fmt.Errorf("%w: %q", ErrDatasourceDenied, dsUID)
	}

	allowedEvidenceIDs, err := ir.ValidateAnalyzePayload(in)
	if err != nil {
		return JobSnapshot{}, err
	}
	sanitized := prompt.SanitizeAnalyzePayload(in)
	sanitized.DatasourceUID = dsUID
	// Verify serialized payload fits within MaxAnalyzePayloadBytes before allocating a job slot.
	if _, err := prompt.BuildAnalyzeStatusMessages(sanitized); err != nil {
		return JobSnapshot{}, fmt.Errorf("%w: %v", ir.ErrInvalidAnalyzeRequest, err)
	}

	jobID := newJobID()
	slotID := newSlotID()
	opCtx, cancel := context.WithTimeout(ctx, m.cfg.JobDeadline)
	defer cancel()

	m.opWG.Add(1)
	defer m.opWG.Done()

	lease, err := m.store.CreateGenerating(ctx, CreateGeneratingParams{
		JobID:             jobID,
		JobKind:           JobKindReport,
		Owner:             owner,
		DatasourceUID:     dsUID,
		Scope:             cloneScope(sanitized.Scope),
		MaxRepairAttempts: 0,
		SlotID:            slotID,
		Now:               m.storeNow(),
		Limits:            m.cfg.storeLimits(),
	})
	if err != nil {
		if errors.Is(err, ErrStoreUnavailable) {
			m.releaseSlot(ctx, owner, slotID)
		}
		return JobSnapshot{}, err
	}
	opToken := lease.OpToken
	m.registerLocalCancel(jobID, opToken, cancel)
	defer m.unregisterLocalCancel(jobID, opToken)
	defer m.releaseSlot(ctx, owner, lease.SlotID)

	m.emitStage(ctx, jobID, OperationAnalyze, StatusAnalyzing)

	raw, meta, err := m.client.AnalyzeStatus(opCtx, llm.AnalyzeStatusRequest{
		Payload: sanitized,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
			snap, _ := m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusCancelled, nil, "Status analysis was cancelled.", "Status analysis was cancelled.", &meta)
			return snap, ErrJobCancelled
		}
		redactedErr := prompt.RedactSecretsAndURLs(err.Error())
		if redactedErr == "" {
			redactedErr = "upstream LLM error"
		}
		errMsg := fmt.Sprintf("LLM status analysis failed: %s", redactedErr)
		retErr := err
		if !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, llm.ErrOutputTruncated) &&
			!errors.Is(err, llm.ErrOutputRefused) &&
			!errors.Is(err, llm.ErrLLMUnavailable) {
			retErr = fmt.Errorf("%w: %s", llm.ErrLLMUnavailable, redactedErr)
		}
		snap, finishErr := m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusFailed, nil, "", errMsg, &meta)
		if errors.Is(finishErr, ErrJobCancelled) {
			return snap, ErrJobCancelled
		}
		return snap, retErr
	}

	m.setStatusIfToken(ctx, owner, jobID, opToken, StatusValidatingReport)
	m.emitStage(ctx, jobID, OperationAnalyze, StatusValidatingReport)

	report, err := ir.DecodeStrictStatusReport(raw)
	if err != nil {
		errMsg := fmt.Sprintf("Status report JSON schema validation failed: %s", prompt.RedactSecretsAndURLs(err.Error()))
		snap, finishErr := m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusFailed, nil, "", errMsg, &meta)
		if errors.Is(finishErr, ErrJobCancelled) {
			return snap, ErrJobCancelled
		}
		return snap, err
	}

	// Defense-in-depth: redact any secret-like strings before final validation & persistence
	// so redaction never silently causes post-validation length overshoot.
	prompt.RedactStatusReport(&report)

	if err := ir.ValidateStatusReport(report, allowedEvidenceIDs); err != nil {
		errMsg := fmt.Sprintf("Status report grounding or hygiene validation failed: %s", prompt.RedactSecretsAndURLs(err.Error()))
		snap, finishErr := m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusFailed, nil, "", errMsg, &meta)
		if errors.Is(finishErr, ErrJobCancelled) {
			return snap, ErrJobCancelled
		}
		return snap, err
	}

	return m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusReady, &report, report.ExecutiveSummary, "", &meta)
}

func (m *Manager) finishReportJobWithStage(
	ctx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	status JobStatus,
	report *ir.StatusReportIR,
	summary string,
	errMsg string,
	meta *llm.CompletionMeta,
) (JobSnapshot, error) {
	finishCtx, finishCancel := detachedStoreContext(ctx)
	defer finishCancel()

	snap, err := m.store.FinishJob(finishCtx, FinishJobParams{
		Owner:        owner,
		JobID:        jobID,
		OpToken:      opToken,
		Status:       status,
		Report:       report,
		Summary:      summary,
		ErrorMessage: errMsg,
		LLMMeta:      meta,
		Now:          m.storeNow(),
		JobTTL:       m.cfg.JobTTL,
	})
	if err != nil {
		if errors.Is(err, ErrJobCancelled) {
			m.emitStage(ctx, jobID, OperationAnalyze, snap.Status)
			return snap, ErrJobCancelled
		}
		return JobSnapshot{}, err
	}
	m.emitStage(ctx, jobID, OperationAnalyze, snap.Status)
	return snap, nil
}

// Import validates a constrained DashboardIR converted from an existing Grafana dashboard
// and registers it as an owner-bound draft job ready for dry-run validation, preview, and patching.
// Static IR schema + dialect AST/regex/range safety policies are ALWAYS enforced; catalog membership
// is additionally enforced whenever a non-empty scoped catalog is supplied.
func (m *Manager) Import(owner Owner, in ImportInput) (JobSnapshot, error) {
	return m.ImportContext(context.Background(), owner, in)
}

// ImportContext is the context-aware variant of Import.
func (m *Manager) ImportContext(ctx context.Context, owner Owner, in ImportInput) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	dsUID := strings.TrimSpace(in.DatasourceUID)
	if !m.isDatasourceAllowed(dsUID) {
		return JobSnapshot{}, fmt.Errorf("%w: %q", ErrDatasourceDenied, dsUID)
	}
	qKind, adapter, err := m.resolveAdapter(dsUID, in.QueryKind)
	if err != nil {
		return JobSnapshot{}, err
	}
	if err := adapter.ValidateScopeFilter(in.Scope); err != nil {
		return JobSnapshot{}, err
	}
	if err := ir.ValidateImportSource(in.ImportSource); err != nil {
		return JobSnapshot{}, err
	}
	safeScope := prompt.SanitizeScope(in.Scope)
	boundedCatalog := prompt.BoundCatalog(in.Catalog)

	doc := in.Draft.Clone()
	if strings.TrimSpace(doc.SchemaVersion) == "" {
		doc.SchemaVersion = ir.SchemaVersion
	}
	if doc.Revision < 1 || doc.Revision > ir.MaxRevision {
		return JobSnapshot{}, fmt.Errorf("imported IR validation failed: revision must be between 1 and %d, got %d", ir.MaxRevision, doc.Revision)
	}
	doc.Revision = 1
	if qKind != query.KindPromQL {
		doc.StampQueryKind(string(qKind))
	}

	warnings, err := query.ValidateDashboardIR(
		adapter,
		doc,
		boundedCatalog,
		safeScope,
		ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels},
		query.ValidateOptions{RequireCatalogMatch: len(boundedCatalog) > 0},
	)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("imported IR validation failed: %w", err)
	}

	jobID := newJobID()
	summary := "既存ダッシュボードを安全な IR ドラフトとしてインポートしました。検証・プレビューを実行して内容を確認してください。"
	if in.ImportSource != nil && in.ImportSource.UID != "" {
		summary = fmt.Sprintf("既存ダッシュボード (%s) を安全な IR ドラフトとしてインポートしました。検証・プレビューを実行して内容を確認してください。", in.ImportSource.UID)
	}

	return m.store.CreateImported(ctx, CreateImportedParams{
		JobID:             jobID,
		Owner:             owner,
		DatasourceUID:     dsUID,
		QueryKind:         qKind,
		Scope:             cloneScope(safeScope),
		ImportSource:      cloneImportSource(in.ImportSource),
		Draft:             doc,
		Summary:           summary,
		Warnings:          warnings,
		MaxRepairAttempts: m.cfg.MaxRepairAttempts,
		KnownGauges:       extractGaugeNames(boundedCatalog),
		Now:               m.storeNow(),
		Limits:            m.cfg.storeLimits(),
	})
}

// Patch performs a partial edit anchored to baseRevision without regenerating untouched panels.
func (m *Manager) Patch(ctx context.Context, owner Owner, jobID string, in PatchInput) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	if strings.TrimSpace(in.EditIntent) == "" {
		return JobSnapshot{}, errors.New("editIntent is required")
	}

	boundedCatalog := prompt.BoundCatalog(in.Catalog)
	if len(boundedCatalog) == 0 {
		return JobSnapshot{}, errors.New("non-empty scoped metric catalog is required")
	}
	targetPanelID := strings.TrimSpace(in.TargetPanelID)
	if strings.ContainsAny(targetPanelID, "\r\n") {
		return JobSnapshot{}, fmt.Errorf("%w: %q", ir.ErrPanelNotFound, targetPanelID)
	}

	opCtx, cancel := context.WithTimeout(ctx, m.cfg.JobDeadline)
	defer cancel()

	m.opWG.Add(1)
	defer m.opWG.Done()

	slotID := newSlotID()
	op, err := m.store.BeginPatch(ctx, BeginPatchParams{
		Owner:         owner,
		JobID:         jobID,
		BaseRevision:  in.BaseRevision,
		TargetPanelID: targetPanelID,
		CatalogGauges: extractGaugeNames(boundedCatalog),
		SlotID:        slotID,
		Now:           m.storeNow(),
		Limits:        m.cfg.storeLimits(),
	})
	if err != nil {
		if errors.Is(err, ErrStoreUnavailable) {
			m.releaseSlot(ctx, owner, slotID)
		}
		return JobSnapshot{}, err
	}
	opToken := op.OpToken
	m.registerLocalCancel(jobID, opToken, cancel)
	defer m.unregisterLocalCancel(jobID, opToken)
	defer m.releaseSlot(ctx, owner, op.SlotID)

	m.emitStage(ctx, jobID, OperationPatch, StatusGenerating)

	qKind := op.QueryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	adapter, err := m.registry.Get(qKind)
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationPatch, err, err.Error(), nil)
	}

	// Preserve and monotonically expand known gauge types across revisions/replicas so clients cannot spoof gauge->counter.
	boundedCatalog = enforceKnownGaugeTypes(boundedCatalog, op.KnownGauges)

	raw, meta, err := m.client.GeneratePatch(opCtx, llm.PatchRequest{
		CurrentIR:     op.CurrentIR,
		TargetPanelID: targetPanelID,
		EditIntent:    in.EditIntent,
		Scope:         op.Scope,
		Catalog:       boundedCatalog,
		QueryKind:     string(qKind),
		DialectRules:  adapter.PromptRules(),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
			return m.revertToPreviousOnCancelWithStage(ctx, owner, jobID, opToken, OperationPatch)
		}
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationPatch, err, err.Error(), &meta)
	}

	m.setStatusIfToken(ctx, owner, jobID, opToken, StatusValidatingIR)
	m.emitStage(ctx, jobID, OperationPatch, StatusValidatingIR)

	patch, err := ir.DecodeStrictPatch(raw)
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationPatch, err, fmt.Sprintf("パッチ JSON 検査に失敗しました: %v", err), &meta)
	}

	// When user explicitly targeted a single panel, enforce that the LLM only updates that panel.
	if targetPanelID != "" {
		for _, pOp := range patch.Operations {
			if pOp.Op != "update_panel" || pOp.PanelID != targetPanelID {
				return m.recordEditFailureWithStage(
					ctx,
					owner,
					jobID,
					opToken,
					OperationPatch,
					nil,
					fmt.Sprintf("対象パネル指定 (%s) に反するパッチ操作 (%s:%s) を拒否しました", targetPanelID, pOp.Op, pOp.PanelID),
					&meta,
				)
			}
		}
	}

	nextIR, diffs, err := ir.ApplyPatch(op.CurrentIR, patch, ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels})
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationPatch, err, fmt.Sprintf("パッチ適用に失敗しました: %v", err), &meta)
	}
	if qKind != query.KindPromQL {
		nextIR.StampQueryKind(string(qKind))
	}

	warnings, err := query.ValidateDashboardIR(
		adapter,
		nextIR,
		boundedCatalog,
		op.Scope,
		ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels},
		query.ValidateOptions{RequireCatalogMatch: true},
	)
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationPatch, err, fmt.Sprintf("パッチ後の %s 検査に失敗しました: %v", adapter.DisplayName(), err), &meta)
	}

	return m.commitRevisionWithStage(ctx, owner, jobID, opToken, OperationPatch, op.CurrentIR.Revision, nextIR, diffs, patch.Summary, warnings, &meta)
}

// Repair attempts a bounded repair ONLY for syntax_error panels matching the current IR revision and queryHash.
// Repair NEVER runs for empty_result, permission_error, or timeout, NEVER resets the per-job repair budget,
// and ONLY allows mutating the failing panels' Query/Queries field.
func (m *Manager) Repair(ctx context.Context, owner Owner, jobID string, in RepairInput) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}

	boundedCatalog := prompt.BoundCatalog(in.Catalog)
	if len(boundedCatalog) == 0 {
		return JobSnapshot{}, errors.New("non-empty scoped metric catalog is required for repair")
	}

	// Pre-read job state to validate syntax_error targets before atomically consuming repair budget.
	// BeginRepair re-verifies ownership, revision, active op lock, repair budget, and concurrency slots atomically.
	cand, err := m.store.GetRepairCandidate(ctx, owner, jobID, m.storeNow())
	if err != nil {
		return JobSnapshot{}, err
	}
	if cand.CurrentIR == nil {
		return JobSnapshot{}, errors.New("job has no active IR draft to repair")
	}
	if in.BaseRevision != cand.CurrentIR.Revision {
		return JobSnapshot{}, formatRevisionConflict("repair", in.BaseRevision, cand.CurrentIR.Revision)
	}
	if cand.HasActiveOp {
		return JobSnapshot{}, ErrOperationInProgress
	}
	if cand.RepairAttemptsUsed >= cand.MaxRepairAttempts {
		return JobSnapshot{}, fmt.Errorf("%w: used %d of %d repair attempts", ErrBudgetExhausted, cand.RepairAttemptsUsed, cand.MaxRepairAttempts)
	}
	if len(in.Results) > len(cand.CurrentIR.Panels) {
		return JobSnapshot{}, fmt.Errorf("results count %d exceeds panel count %d", len(in.Results), len(cand.CurrentIR.Panels))
	}

	panelByID := make(map[string]ir.PanelIR, len(cand.CurrentIR.Panels))
	for _, p := range cand.CurrentIR.Panels {
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
		if r.QueryHash != ir.PanelQueryHash(p) {
			rejectedReasons = append(rejectedReasons, fmt.Sprintf("%s: queryHash mismatch", r.PanelID))
			continue
		}
		switch r.Category {
		case ir.ValidationSyntaxError:
			qs := p.EffectiveQueries()
			primaryExpr := ""
			if len(qs) > 0 {
				primaryExpr = qs[0].Expr
			}
			target := prompt.RepairTarget{
				PanelID:   p.ID,
				Expr:      primaryExpr,
				ErrorText: r.ErrorText,
			}
			if len(p.Queries) > 0 {
				target.Queries = append([]ir.QueryIR(nil), p.Queries...)
			}
			targets = append(targets, target)
		case ir.ValidationEmptyResult, ir.ValidationPermissionError, ir.ValidationTimeout:
			rejectedReasons = append(rejectedReasons, fmt.Sprintf("%s: %s is not eligible for auto-repair", r.PanelID, r.Category))
		}
	}

	if len(targets) == 0 {
		if len(rejectedReasons) > 0 {
			return JobSnapshot{}, fmt.Errorf("%w (%s)", ErrNotRepairable, strings.Join(rejectedReasons, "; "))
		}
		return JobSnapshot{}, fmt.Errorf("%w: no syntax_error panel matched current revision", ErrNotRepairable)
	}

	qKind := cand.QueryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	adapter, err := m.registry.Get(qKind)
	if err != nil {
		return JobSnapshot{}, err
	}

	opCtx, cancel := context.WithTimeout(ctx, m.cfg.JobDeadline)
	defer cancel()

	m.opWG.Add(1)
	defer m.opWG.Done()

	slotID := newSlotID()
	// Atomically verify revision/lock/budget/concurrency and consume repair budget BEFORE calling LLM.
	op, err := m.store.BeginRepair(ctx, BeginRepairParams{
		Owner:         owner,
		JobID:         jobID,
		BaseRevision:  in.BaseRevision,
		CatalogGauges: extractGaugeNames(boundedCatalog),
		SlotID:        slotID,
		Now:           m.storeNow(),
		Limits:        m.cfg.storeLimits(),
	})
	if err != nil {
		if errors.Is(err, ErrStoreUnavailable) {
			m.releaseSlot(ctx, owner, slotID)
		}
		return JobSnapshot{}, err
	}
	opToken := op.OpToken
	m.registerLocalCancel(jobID, opToken, cancel)
	defer m.unregisterLocalCancel(jobID, opToken)
	defer m.releaseSlot(ctx, owner, op.SlotID)

	m.emitStage(ctx, jobID, OperationRepair, StatusRepairing)

	boundedCatalog = enforceKnownGaugeTypes(boundedCatalog, op.KnownGauges)

	raw, meta, err := m.client.RepairPanels(opCtx, llm.RepairRequest{
		CurrentIR:    op.CurrentIR,
		Targets:      targets,
		Scope:        op.Scope,
		Catalog:      boundedCatalog,
		QueryKind:    string(qKind),
		DialectRules: adapter.PromptRules(),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
			return m.revertToPreviousOnCancelWithStage(ctx, owner, jobID, opToken, OperationRepair)
		}
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, err, fmt.Sprintf("自動修復に失敗しました: %v", err), &meta)
	}

	m.setStatusIfToken(ctx, owner, jobID, opToken, StatusValidatingIR)
	m.emitStage(ctx, jobID, OperationRepair, StatusValidatingIR)

	patch, err := ir.DecodeStrictPatch(raw)
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, err, fmt.Sprintf("修復パッチ JSON 検査に失敗しました: %v", err), &meta)
	}

	// Enforce that repair ONLY touches Query or Queries on the failing syntax_error panels
	// and NEVER mutates query.kind away from the job's active dialect.
	allowedTargetIDs := make(map[string]bool, len(targets))
	for _, t := range targets {
		allowedTargetIDs[t.PanelID] = true
	}
	for _, pOp := range patch.Operations {
		if pOp.Op != "update_panel" || !allowedTargetIDs[pOp.PanelID] || pOp.Panel == nil || (pOp.Panel.Query == nil && pOp.Panel.Queries == nil) {
			return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, nil, fmt.Sprintf("修復パッチが対象外パネル/操作 (%s:%s) を含んでいたため拒否しました", pOp.Op, pOp.PanelID), &meta)
		}
		p := pOp.Panel
		if p.DatasourceRef != "" && p.DatasourceRef != ir.SelectedDatasourceRef {
			return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, nil, "修復パッチによるデータソース変更は禁止されています", &meta)
		}
		if p.Type != "" || p.Title != "" || p.Description != nil || p.Unit != "" || p.Thresholds != nil || p.Layout != nil {
			return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, nil, fmt.Sprintf("修復パッチは失敗パネルの %s クエリのみ変更可能です", adapter.DisplayName()), &meta)
		}
		if p.Query != nil && p.Query.Kind != nil && *p.Query.Kind != "" && *p.Query.Kind != string(qKind) {
			return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, nil, fmt.Sprintf("修復パッチによるクエリ方言変更 (%q -> %q) は禁止されています", qKind, *p.Query.Kind), &meta)
		}
		if p.Queries != nil {
			for _, q := range *p.Queries {
				if q.Kind != "" && q.Kind != string(qKind) {
					return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, nil, fmt.Sprintf("修復パッチによるクエリ方言変更 (%q -> %q) は禁止されています", qKind, q.Kind), &meta)
				}
			}
		}
	}

	nextIR, diffs, err := ir.ApplyPatch(op.CurrentIR, patch, ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels})
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, err, fmt.Sprintf("修復パッチ適用に失敗しました: %v", err), &meta)
	}
	if qKind != query.KindPromQL {
		nextIR.StampQueryKind(string(qKind))
	}

	warnings, err := query.ValidateDashboardIR(
		adapter,
		nextIR,
		boundedCatalog,
		op.Scope,
		ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels},
		query.ValidateOptions{RequireCatalogMatch: true},
	)
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, err, fmt.Sprintf("修復後の %s/スコープ検査に失敗しました: %v", adapter.DisplayName(), err), &meta)
	}

	return m.commitRevisionWithStage(ctx, owner, jobID, opToken, OperationRepair, op.CurrentIR.Revision, nextIR, diffs, "構文エラーのパネルを修復しました。変更パネルを再検証してください。", warnings, &meta)
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
