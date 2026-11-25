package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

const (
	defaultMaxJobs                  = 200
	defaultMaxJobsPerUser           = 20
	defaultJobTTL                   = 30 * time.Minute
	defaultMaxConcurrentJobs        = 4
	defaultMaxConcurrentJobsPerUser = 1
	defaultMaxUndoHistory           = 10
	defaultOpLeaseGrace             = 15 * time.Second
	defaultStoreOpTimeout           = 5 * time.Second
	maxKnownGaugesPerJob            = 512
)

var validJobIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ValidateJobID checks that jobID is a safe bounded identifier before hitting any JobStore.
func ValidateJobID(jobID string) error {
	if !validJobIDRe.MatchString(jobID) {
		return ErrJobNotFound
	}
	return nil
}

// StoreLimits defines bounded capacity, TTL, undo depth, and concurrency parameters
// enforced atomically per Grafana organization by a JobStore implementation.
type StoreLimits struct {
	MaxJobs                  int
	MaxJobsPerUser           int
	JobTTL                   time.Duration
	MaxConcurrentJobs        int
	MaxConcurrentJobsPerUser int
	MaxUndoHistory           int
	OpLeaseTTL               time.Duration
}

func (l StoreLimits) normalized() StoreLimits {
	if l.MaxJobs <= 0 {
		l.MaxJobs = defaultMaxJobs
	}
	if l.MaxJobsPerUser <= 0 {
		l.MaxJobsPerUser = defaultMaxJobsPerUser
	}
	if l.JobTTL <= 0 {
		l.JobTTL = defaultJobTTL
	}
	if l.MaxConcurrentJobs <= 0 {
		l.MaxConcurrentJobs = defaultMaxConcurrentJobs
	}
	if l.MaxConcurrentJobsPerUser <= 0 {
		l.MaxConcurrentJobsPerUser = defaultMaxConcurrentJobsPerUser
	}
	if l.MaxUndoHistory <= 0 {
		l.MaxUndoHistory = defaultMaxUndoHistory
	}
	if l.OpLeaseTTL <= 0 {
		l.OpLeaseTTL = 90*time.Second + defaultOpLeaseGrace
	}
	return l
}

// OpLease represents an atomically acquired per-job fencing token and concurrency slot lease.
type OpLease struct {
	OpToken uint64
	SlotID  string
}

// MutationResult pairs the resulting JobSnapshot with the updated monotonic fencing token
// produced by Undo or Cancel so callers only cancel local contexts older than OpToken.
type MutationResult struct {
	Snapshot JobSnapshot
	OpToken  uint64
}

// BeginOpResult contains the fencing token, slot lease, and authoritative job state
// returned atomically when starting a Patch or Repair operation.
type BeginOpResult struct {
	OpToken     uint64
	SlotID      string
	CurrentIR   ir.DashboardIR
	Scope       *ir.ScopeFilter
	QueryKind   query.Kind
	KnownGauges map[string]bool
}

// RepairCandidate holds the job state read prior to validating syntax_error repair targets.
// BeginRepair re-verifies all invariants atomically against the store before burning budget.
type RepairCandidate struct {
	CurrentIR          *ir.DashboardIR
	RepairAttemptsUsed int
	MaxRepairAttempts  int
	HasActiveOp        bool
	QueryKind          query.Kind
}

// CreateGeneratingParams specifies parameters for creating a new generating job.
type CreateGeneratingParams struct {
	JobID             string
	Owner             Owner
	DatasourceUID     string
	QueryKind         query.Kind
	Scope             *ir.ScopeFilter
	MaxRepairAttempts int
	KnownGauges       []string
	SlotID            string
	Now               time.Time
	Limits            StoreLimits
}

// CreateImportedParams specifies parameters for persisting a validated imported draft job.
type CreateImportedParams struct {
	JobID             string
	Owner             Owner
	DatasourceUID     string
	QueryKind         query.Kind
	Scope             *ir.ScopeFilter
	ImportSource      *ir.ImportSource
	Draft             ir.DashboardIR
	Summary           string
	Warnings          []string
	MaxRepairAttempts int
	KnownGauges       []string
	Now               time.Time
	Limits            StoreLimits
}

// BeginPatchParams specifies parameters for atomically starting a Patch operation.
type BeginPatchParams struct {
	Owner         Owner
	JobID         string
	BaseRevision  int
	TargetPanelID string
	CatalogGauges []string
	SlotID        string
	Now           time.Time
	Limits        StoreLimits
}

// BeginRepairParams specifies parameters for atomically starting a Repair operation
// and consuming one unit of repair budget before calling the LLM.
type BeginRepairParams struct {
	Owner         Owner
	JobID         string
	BaseRevision  int
	CatalogGauges []string
	SlotID        string
	Now           time.Time
	Limits        StoreLimits
}

// FinishJobParams specifies parameters for completing an initial Generate job.
type FinishJobParams struct {
	Owner        Owner
	JobID        string
	OpToken      uint64
	Status       JobStatus
	Doc          *ir.DashboardIR
	Diffs        []ir.DiffEntry
	Summary      string
	Warnings     []string
	ErrorMessage string
	LLMMeta      *llm.CompletionMeta
	Now          time.Time
	JobTTL       time.Duration
}

// CommitRevisionParams specifies parameters for atomically committing a new IR revision.
type CommitRevisionParams struct {
	Owner          Owner
	JobID          string
	OpToken        uint64
	ExpectedRev    int
	NextIR         ir.DashboardIR
	Diffs          []ir.DiffEntry
	Summary        string
	Warnings       []string
	LLMMeta        *llm.CompletionMeta
	Now            time.Time
	JobTTL         time.Duration
	MaxUndoHistory int
}

// RecordEditFailureParams specifies parameters for recording a Patch or Repair failure.
// RecordEditFailure returns (snapshot, nil) when the failure state is persisted cleanly,
// or (snapshot, ErrJobCancelled) when the opToken/lease is no longer active.
type RecordEditFailureParams struct {
	Owner        Owner
	JobID        string
	OpToken      uint64
	ErrorMessage string
	LLMMeta      *llm.CompletionMeta
	Now          time.Time
	JobTTL       time.Duration
}

// JobStore abstracts job persistence, strict (OrgID, UserLogin) owner isolation,
// per-org/per-user concurrency locking, atomic repair-budget accounting, and
// TTL-based expiration across single-process or multi-replica HA deployments.
//
// Timestamps and TTLs have millisecond resolution normalized to UTC. When Now is zero,
// implementations use their authoritative clock (time.Now for MemoryStore, Redis TIME for RedisStore).
//
// Implementations MUST verify exact Owner (OrgID and UserLogin) equality on every
// read and write operation, returning ErrForbidden if a job exists for another owner
// and ErrJobNotFound if the job does not exist or has expired.
type JobStore interface {
	CreateGenerating(ctx context.Context, params CreateGeneratingParams) (OpLease, error)
	CreateImported(ctx context.Context, params CreateImportedParams) (JobSnapshot, error)
	Get(ctx context.Context, owner Owner, jobID string, now time.Time) (JobSnapshot, error)
	GetRepairCandidate(ctx context.Context, owner Owner, jobID string, now time.Time) (RepairCandidate, error)
	BeginPatch(ctx context.Context, params BeginPatchParams) (BeginOpResult, error)
	BeginRepair(ctx context.Context, params BeginRepairParams) (BeginOpResult, error)
	SetStatus(ctx context.Context, owner Owner, jobID string, opToken uint64, status JobStatus, now time.Time, jobTTL time.Duration) error
	FinishJob(ctx context.Context, params FinishJobParams) (JobSnapshot, error)
	CommitRevision(ctx context.Context, params CommitRevisionParams) (JobSnapshot, error)
	RecordEditFailure(ctx context.Context, params RecordEditFailureParams) (JobSnapshot, error)
	RevertOnCancel(ctx context.Context, owner Owner, jobID string, opToken uint64, now time.Time, jobTTL time.Duration) (JobSnapshot, error)
	Undo(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error)
	Cancel(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error)
	ReleaseSlot(ctx context.Context, owner Owner, slotID string, now time.Time) error
	CleanupExpired(ctx context.Context, now time.Time) error
	Ping(ctx context.Context) error
	Close() error
}

// CancelSubscription is an optional extension implemented by distributed JobStores
// (such as RedisStore) to broadcast Cancel/Undo fencing events across replicas so
// in-flight LLM requests and concurrency slots unwind immediately on remote replicas.
type CancelSubscription interface {
	SubscribeCancels(ctx context.Context, onCancel func(jobID string, fencedToken uint64)) (stop func(), err error)
}

// ownerKey produces a deterministic, collision-free, PII-free key component for an Owner.
// Using a NUL delimiter inside SHA-256 prevents any collision regardless of characters in UserLogin.
func ownerKey(owner Owner) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(owner.OrgID, 10) + "\x00" + owner.UserLogin))
	return strconv.FormatInt(owner.OrgID, 10) + ":" + hex.EncodeToString(sum[:])
}

func newSlotID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "slot-" + hex.EncodeToString(b)
}

func extractPanelIDs(panels []ir.PanelIR) []string {
	if len(panels) == 0 {
		return nil
	}
	ids := make([]string, 0, len(panels))
	for _, p := range panels {
		if id := strings.TrimSpace(p.ID); id != "" && !strings.ContainsAny(id, "\r\n") {
			ids = append(ids, id)
		}
	}
	return ids
}

func extractGaugeNames(catalog []ir.MetricCatalogItem) []string {
	if len(catalog) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, item := range catalog {
		if strings.EqualFold(strings.TrimSpace(item.Type), "gauge") {
			name := strings.TrimSpace(item.Name)
			if name != "" && !strings.ContainsAny(name, "\r\n") {
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					out = append(out, name)
				}
			}
		}
	}
	sort.Strings(out)
	if len(out) > maxKnownGaugesPerJob {
		out = out[:maxKnownGaugesPerJob]
	}
	return out
}

func mergeGaugeMapBounded(dst map[string]bool, incoming []string) {
	if dst == nil {
		return
	}
	for _, g := range incoming {
		g = strings.TrimSpace(g)
		if g == "" || strings.ContainsAny(g, "\r\n") {
			continue
		}
		if dst[g] {
			continue
		}
		if len(dst) >= maxKnownGaugesPerJob {
			break
		}
		dst[g] = true
	}
}

func cloneStringBoolMap(src map[string]bool) map[string]bool {
	if len(src) == 0 {
		return make(map[string]bool)
	}
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		if v {
			dst[k] = true
		}
	}
	return dst
}

func normalizeJobTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultJobTTL
	}
	return d
}

func formatRevisionConflict(prefix string, requested, current int) error {
	if prefix == "" {
		return fmt.Errorf("%w: request baseRevision=%d != current revision=%d", ir.ErrRevisionConflict, requested, current)
	}
	return fmt.Errorf("%w: %s baseRevision=%d != current revision=%d", ir.ErrRevisionConflict, prefix, requested, current)
}

func normalizeTimestamp(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Truncate(time.Millisecond)
}
