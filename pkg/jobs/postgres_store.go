package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var (
	_ JobStore           = (*PostgresStore)(nil)
	_ CancelSubscription = (*PostgresStore)(nil)
)

// PostgresStore is a transactional, org-isolated, owner-isolated, TTL-backed JobStore for HA Grafana.
// Per-org advisory transaction locks serialize concurrency slots and bounded job eviction cleanly
// across replicas without cross-org contention, single-row FOR UPDATE transactions enforce fencing
// tokens and optimistic revisions, and PostgreSQL LISTEN/NOTIFY broadcasts Cancel/Undo immediately.
type PostgresStore struct {
	pool          *pgxpool.Pool
	ownsPool      bool
	listenConnCfg *pgx.ConnConfig
	schema        string
	tablePrefix   string
	jobsTable     string
	slotsTable    string
	cancelChannel string

	schemaReady           atomic.Bool
	supportsReportStorage atomic.Bool
	lastReportProbeNs     atomic.Int64
	schemaSem             chan struct{}

	closeMu     sync.Mutex
	closed      bool
	closeCtx    context.Context
	closeCancel context.CancelFunc
	subWG       sync.WaitGroup
	closeOnce   sync.Once
}

type pgQueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func resolvePostgresNowMs(ctx context.Context, q pgQueryRower, now time.Time) (int64, error) {
	if !now.IsZero() {
		return normalizeTimestamp(now).UnixMilli(), nil
	}
	var ms int64
	if err := q.QueryRow(ctx, `SELECT FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT`).Scan(&ms); err != nil {
		return 0, err
	}
	return ms, nil
}

func (s *PostgresStore) CreateGenerating(ctx context.Context, params CreateGeneratingParams) (OpLease, error) {
	if err := validateOwner(params.Owner); err != nil {
		return OpLease{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return OpLease{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return OpLease{}, err
	}
	if params.JobKind == JobKindReport {
		if err := s.ensureReportStorageReady(ctx); err != nil {
			return OpLease{}, err
		}
	}

	limits := params.Limits.normalized()
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	scopeJSON, err := marshalOptionalJSON(params.Scope)
	if err != nil {
		return OpLease{}, fmt.Errorf("marshal scope: %w", err)
	}
	gauges := make(map[string]bool)
	mergeGaugeMapBounded(gauges, params.KnownGauges)
	gaugesJSON, err := marshalGaugeMap(gauges)
	if err != nil {
		return OpLease{}, fmt.Errorf("marshal knownGauges: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.orgLockKey(params.Owner.OrgID)); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	if err := s.evictExpiredOrgTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	if err := s.reapExpiredOrgSlotsTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	var exists bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE job_id = $1)`, s.jobsTable), params.JobID).Scan(&exists); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	if exists {
		return OpLease{}, fmt.Errorf("job id %q already exists", params.JobID)
	}

	oKey := ownerKey(params.Owner)
	canAcquire, err := s.canAcquireSlotTx(ctx, tx, params.Owner.OrgID, oKey, nowMs, limits)
	if err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	if !canAcquire {
		return OpLease{}, ErrConcurrencyLimited
	}

	if err := s.ensureCapacityTx(ctx, tx, params.Owner, oKey, nowMs, limits); err != nil {
		return OpLease{}, err
	}

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)
	expiresAtMs := nowMs + jobTTLMs
	leaseUntilMs := nowMs + leaseTTLMs

	initStatus := StatusGenerating
	if params.JobKind == JobKindReport {
		initStatus = StatusAnalyzing
	}

	if s.supportsReportStorage.Load() {
		_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (
			job_id, org_id, user_login, owner_key, datasource_uid, query_kind,
			scope_json, status, max_repair_attempts, known_gauges_json,
			created_at_ms, updated_at_ms, expires_at_ms, active_op_token, active_op_until_ms,
			job_kind, report_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,1,$13,$14,'')`, s.jobsTable),
			params.JobID,
			params.Owner.OrgID,
			params.Owner.UserLogin,
			oKey,
			params.DatasourceUID,
			string(params.QueryKind),
			scopeJSON,
			string(initStatus),
			params.MaxRepairAttempts,
			gaugesJSON,
			nowMs,
			expiresAtMs,
			leaseUntilMs,
			params.JobKind,
		)
	} else {
		_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (
			job_id, org_id, user_login, owner_key, datasource_uid, query_kind,
			scope_json, status, max_repair_attempts, known_gauges_json,
			created_at_ms, updated_at_ms, expires_at_ms, active_op_token, active_op_until_ms
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,1,$13)`, s.jobsTable),
			params.JobID,
			params.Owner.OrgID,
			params.Owner.UserLogin,
			oKey,
			params.DatasourceUID,
			string(params.QueryKind),
			scopeJSON,
			string(initStatus),
			params.MaxRepairAttempts,
			gaugesJSON,
			nowMs,
			expiresAtMs,
			leaseUntilMs,
		)
	}
	if err != nil {
		if isPgUniqueViolation(err) {
			return OpLease{}, fmt.Errorf("job id %q already exists", params.JobID)
		}
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	if err := s.upsertSlotTx(ctx, tx, params.Owner.OrgID, slotID, oKey, leaseUntilMs); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return OpLease{}, wrapPostgresErr("CreateGenerating", err)
	}
	return OpLease{OpToken: 1, SlotID: slotID}, nil
}

func (s *PostgresStore) CreateImported(ctx context.Context, params CreateImportedParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	limits := params.Limits.normalized()
	scopeJSON, err := marshalOptionalJSON(params.Scope)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal scope: %w", err)
	}
	importSrcJSON, err := marshalOptionalJSON(params.ImportSource)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal importSource: %w", err)
	}
	docBytes, err := json.Marshal(params.Draft)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal draft: %w", err)
	}
	warnsJSON, err := marshalOptionalStringSlice(params.Warnings)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal warnings: %w", err)
	}
	gauges := make(map[string]bool)
	mergeGaugeMapBounded(gauges, params.KnownGauges)
	gaugesJSON, err := marshalGaugeMap(gauges)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal knownGauges: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.orgLockKey(params.Owner.OrgID)); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	if err := s.evictExpiredOrgTx(ctx, tx, params.Owner.OrgID, nowMs); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	var exists bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE job_id = $1)`, s.jobsTable), params.JobID).Scan(&exists); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}
	if exists {
		return JobSnapshot{}, fmt.Errorf("job id %q already exists", params.JobID)
	}

	oKey := ownerKey(params.Owner)
	if err := s.ensureImportCapacityTx(ctx, tx, params.Owner, oKey, nowMs, limits); err != nil {
		return JobSnapshot{}, err
	}

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	expiresAtMs := nowMs + jobTTLMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (
		job_id, org_id, user_login, owner_key, datasource_uid, query_kind,
		scope_json, import_source_json, status, current_ir_json, current_rev,
		summary, warnings_json, max_repair_attempts, known_gauges_json,
		created_at_ms, updated_at_ms, expires_at_ms, active_op_token, active_op_until_ms
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16,$17,0,0)`, s.jobsTable),
		params.JobID,
		params.Owner.OrgID,
		params.Owner.UserLogin,
		oKey,
		params.DatasourceUID,
		string(params.QueryKind),
		scopeJSON,
		importSrcJSON,
		string(StatusAwaitingPreview),
		string(docBytes),
		params.Draft.Revision,
		params.Summary,
		warnsJSON,
		params.MaxRepairAttempts,
		gaugesJSON,
		nowMs,
		expiresAtMs,
	)
	if err != nil {
		if isPgUniqueViolation(err) {
			return JobSnapshot{}, fmt.Errorf("job id %q already exists", params.JobID)
		}
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CreateImported", err)
	}

	docCopy := params.Draft.Clone()
	var warns []string
	if len(params.Warnings) > 0 {
		warns = make([]string, len(params.Warnings))
		copy(warns, params.Warnings)
	}
	qKind := params.QueryKind
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:             params.JobID,
		Status:            StatusAwaitingPreview,
		DatasourceUID:     params.DatasourceUID,
		QueryKind:         string(qKind),
		Scope:             cloneScope(params.Scope),
		ImportSource:      cloneImportSource(params.ImportSource),
		CurrentIR:         &docCopy,
		Summary:           params.Summary,
		Warnings:          warns,
		MaxRepairAttempts: params.MaxRepairAttempts,
		CanUndo:           false,
		UpdatedAt:         time.UnixMilli(nowMs).UTC(),
	}, nil
}

type pgJobRow struct {
	id                 string
	orgID              int64
	userLogin          string
	datasourceUID      string
	queryKind          string
	scopeJSON          string
	importSourceJSON   string
	status             string
	currentIRJSON      string
	currentRev         int
	historyJSON        string
	historyLen         int
	lastDiffJSON       string
	summary            string
	warningsJSON       string
	errorMessage       string
	repairAttemptsUsed int
	maxRepairAttempts  int
	llmMetaJSON        string
	knownGaugesJSON    string
	updatedAtMs        int64
	expiresAtMs        int64
	activeOpToken      uint64
	activeOpUntilMs    int64
	jobKind            string
	reportJSON         string
}

func (r *pgJobRow) isActive(nowMs int64) bool {
	return r.activeOpUntilMs > 0 && nowMs < r.activeOpUntilMs
}

func (r *pgJobRow) isExpired(nowMs int64) bool {
	if r.isActive(nowMs) {
		return false
	}
	return r.expiresAtMs > 0 && nowMs > r.expiresAtMs
}

func nextOpToken(cur uint64) (uint64, error) {
	if cur >= math.MaxInt64 {
		return 0, errors.New("operation token sequence exhausted")
	}
	return cur + 1, nil
}

func opTokenToInt64(token uint64) int64 {
	if token > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(token)
}

func (r *pgJobRow) toSnapshot(opName string) (JobSnapshot, error) {
	snap, err := serializedJobFields{
		JobID:              r.id,
		JobKind:            r.jobKind,
		Status:             r.status,
		DatasourceUID:      r.datasourceUID,
		QueryKind:          r.queryKind,
		ScopeJSON:          r.scopeJSON,
		ImportSourceJSON:   r.importSourceJSON,
		CurrentIRJSON:      r.currentIRJSON,
		CurrentRev:         r.currentRev,
		HistoryLen:         r.historyLen,
		LastDiffJSON:       r.lastDiffJSON,
		Summary:            r.summary,
		WarningsJSON:       r.warningsJSON,
		ErrorMessage:       r.errorMessage,
		RepairAttemptsUsed: r.repairAttemptsUsed,
		MaxRepairAttempts:  r.maxRepairAttempts,
		LLMMetaJSON:        r.llmMetaJSON,
		ReportJSON:         r.reportJSON,
		UpdatedAtMs:        r.updatedAtMs,
	}.decodeSnapshot()
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr(opName, err)
	}
	return snap, nil
}

func (s *PostgresStore) selectJobRowWithClock(ctx context.Context, q pgQueryRower, jobID string, includeHistory bool, forUpdate bool, explicitNow time.Time) (*pgJobRow, int64, error) {
	if !s.supportsReportStorage.Load() {
		last := s.lastReportProbeNs.Load()
		if last == 0 || time.Since(time.Unix(0, last)) >= postgresReportProbeInterval {
			_, _ = s.probeReportColumnsWith(ctx, q)
		}
	}

	histExpr := "''"
	if includeHistory {
		histExpr = "history_json"
	}
	lockClause := ""
	if forUpdate {
		lockClause = " FOR UPDATE"
	}
	clockExpr := "FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT"
	if !explicitNow.IsZero() {
		clockExpr = strconv.FormatInt(normalizeTimestamp(explicitNow).UnixMilli(), 10) + "::BIGINT"
	}

	runSelect := func(withReportCols bool) (*pgJobRow, int64, error) {
		reportColsExpr := "'', ''"
		if withReportCols {
			reportColsExpr = "job_kind, report_json"
		}
		querySQL := fmt.Sprintf(`SELECT
			job_id, org_id, user_login, datasource_uid, query_kind,
			scope_json, import_source_json, status, current_ir_json, current_rev,
			%s, history_len, last_diff_json, summary, warnings_json,
			error_message, repair_attempts_used, max_repair_attempts, llm_meta_json,
			known_gauges_json, updated_at_ms, expires_at_ms, active_op_token, active_op_until_ms,
			%s, %s
		FROM %s WHERE job_id = $1%s`, histExpr, reportColsExpr, clockExpr, s.jobsTable, lockClause)

		var row pgJobRow
		var opTokenSigned int64
		var nowMs int64
		err := q.QueryRow(ctx, querySQL, jobID).Scan(
			&row.id, &row.orgID, &row.userLogin, &row.datasourceUID, &row.queryKind,
			&row.scopeJSON, &row.importSourceJSON, &row.status, &row.currentIRJSON, &row.currentRev,
			&row.historyJSON, &row.historyLen, &row.lastDiffJSON, &row.summary, &row.warningsJSON,
			&row.errorMessage, &row.repairAttemptsUsed, &row.maxRepairAttempts, &row.llmMetaJSON,
			&row.knownGaugesJSON, &row.updatedAtMs, &row.expiresAtMs, &opTokenSigned, &row.activeOpUntilMs,
			&row.jobKind, &row.reportJSON,
			&nowMs,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, 0, ErrJobNotFound
			}
			return nil, 0, err
		}
		if opTokenSigned > 0 {
			row.activeOpToken = uint64(opTokenSigned)
		}
		return &row, nowMs, nil
	}

	withReportCols := s.supportsReportStorage.Load()
	row, nowMs, err := runSelect(withReportCols)
	if err != nil {
		return nil, 0, err
	}
	// Multi-replica zero-restart DML-only migration guard: if another replica already created a report
	// row (empty query_kind or report-only status) after DBA added columns, immediately re-probe on q
	// and re-select job_kind + report_json so GET/Cancel/Patch/Repair/Undo never see a split-brain row.
	if !withReportCols && (row.queryKind == "" || JobStatus(row.status) == StatusAnalyzing || JobStatus(row.status) == StatusValidatingReport || JobStatus(row.status) == StatusReady) {
		if supported, probeErr := s.probeReportColumnsWith(ctx, q); probeErr == nil && supported {
			return runSelect(true)
		}
	}
	return row, nowMs, nil
}

func (s *PostgresStore) verifyOwnedRowTx(ctx context.Context, tx pgx.Tx, owner Owner, jobID string, nowMs int64, includeHistory bool) (*pgJobRow, error) {
	row, _, err := s.selectJobRowWithClock(ctx, tx, jobID, includeHistory, true, time.UnixMilli(nowMs).UTC())
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if row.isExpired(nowMs) {
		if row.orgID == owner.OrgID && row.userLogin == owner.UserLogin {
			_, _ = tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE job_id = $1 AND org_id = $2 AND user_login = $3`, s.jobsTable),
				jobID, owner.OrgID, owner.UserLogin)
			_ = tx.Commit(ctx)
		}
		return nil, ErrJobNotFound
	}
	if row.orgID != owner.OrgID || row.userLogin != owner.UserLogin {
		return nil, ErrForbidden
	}
	return row, nil
}

func (s *PostgresStore) Get(ctx context.Context, owner Owner, jobID string, now time.Time) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	row, nowMs, err := s.selectJobRowWithClock(ctx, s.pool, jobID, false, false, now)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return JobSnapshot{}, ErrJobNotFound
		}
		return JobSnapshot{}, wrapPostgresErr("Get", err)
	}
	if row.isExpired(nowMs) {
		if row.orgID == owner.OrgID && row.userLogin == owner.UserLogin {
			_, _ = s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE job_id = $1 AND org_id = $2 AND user_login = $3 AND active_op_until_ms <= $4 AND expires_at_ms > 0 AND expires_at_ms < $4`, s.jobsTable),
				jobID, owner.OrgID, owner.UserLogin, nowMs)
		}
		return JobSnapshot{}, ErrJobNotFound
	}
	if row.orgID != owner.OrgID || row.userLogin != owner.UserLogin {
		return JobSnapshot{}, ErrForbidden
	}
	return row.toSnapshot("Get")
}

func (s *PostgresStore) GetRepairCandidate(ctx context.Context, owner Owner, jobID string, now time.Time) (RepairCandidate, error) {
	if err := validateOwner(owner); err != nil {
		return RepairCandidate{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return RepairCandidate{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return RepairCandidate{}, err
	}

	row, nowMs, err := s.selectJobRowWithClock(ctx, s.pool, jobID, false, false, now)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return RepairCandidate{}, ErrJobNotFound
		}
		return RepairCandidate{}, wrapPostgresErr("GetRepairCandidate", err)
	}
	if row.isExpired(nowMs) {
		if row.orgID == owner.OrgID && row.userLogin == owner.UserLogin {
			_, _ = s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE job_id = $1 AND org_id = $2 AND user_login = $3 AND active_op_until_ms <= $4 AND expires_at_ms > 0 AND expires_at_ms < $4`, s.jobsTable),
				jobID, owner.OrgID, owner.UserLogin, nowMs)
		}
		return RepairCandidate{}, ErrJobNotFound
	}
	if row.orgID != owner.OrgID || row.userLogin != owner.UserLogin {
		return RepairCandidate{}, ErrForbidden
	}
	if row.jobKind == JobKindReport {
		return RepairCandidate{}, ErrWrongJobKind
	}

	var docPtr *ir.DashboardIR
	if row.currentIRJSON != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(row.currentIRJSON), &doc); err != nil {
			return RepairCandidate{}, wrapPostgresErr("GetRepairCandidate", fmt.Errorf("unmarshal currentIR: %w", err))
		}
		if row.currentRev > 0 {
			doc.Revision = row.currentRev
		}
		docPtr = &doc
	}
	qKind := query.Kind(row.queryKind)
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return RepairCandidate{
		CurrentIR:          docPtr,
		RepairAttemptsUsed: row.repairAttemptsUsed,
		MaxRepairAttempts:  row.maxRepairAttempts,
		HasActiveOp:        row.isActive(nowMs),
		QueryKind:          qKind,
	}, nil
}

func (s *PostgresStore) BeginPatch(ctx context.Context, params BeginPatchParams) (BeginOpResult, error) {
	return s.beginEditOpTx(ctx, "BeginPatch", beginEditSpec{
		owner:         params.Owner,
		jobID:         params.JobID,
		baseRevision:  params.BaseRevision,
		revPrefix:     "",
		noDraftMsg:    "job has no active IR draft to patch",
		targetPanelID: params.TargetPanelID,
		nextStatus:    StatusGenerating,
		catalogGauges: params.CatalogGauges,
		slotID:        params.SlotID,
		now:           params.Now,
		limits:        params.Limits,
	})
}

func (s *PostgresStore) BeginRepair(ctx context.Context, params BeginRepairParams) (BeginOpResult, error) {
	return s.beginEditOpTx(ctx, "BeginRepair", beginEditSpec{
		owner:         params.Owner,
		jobID:         params.JobID,
		baseRevision:  params.BaseRevision,
		revPrefix:     "repair",
		noDraftMsg:    "job has no active IR draft to repair",
		consumeRepair: true,
		nextStatus:    StatusRepairing,
		catalogGauges: params.CatalogGauges,
		slotID:        params.SlotID,
		now:           params.Now,
		limits:        params.Limits,
	})
}

func (s *PostgresStore) beginEditOpTx(ctx context.Context, opName string, spec beginEditSpec) (BeginOpResult, error) {
	if err := validateOwner(spec.owner); err != nil {
		return BeginOpResult{}, err
	}
	if err := ValidateJobID(spec.jobID); err != nil {
		return BeginOpResult{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return BeginOpResult{}, err
	}

	limits := spec.limits.normalized()
	slotID := spec.slotID
	if slotID == "" {
		slotID = newSlotID()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.orgLockKey(spec.owner.OrgID)); err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	nowMs, err := resolvePostgresNowMs(ctx, tx, spec.now)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}

	row, err := s.verifyOwnedRowTx(ctx, tx, spec.owner, spec.jobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return BeginOpResult{}, err
		}
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	if row.jobKind == JobKindReport {
		return BeginOpResult{}, ErrWrongJobKind
	}
	if row.currentIRJSON == "" {
		return BeginOpResult{}, errors.New(spec.noDraftMsg)
	}
	var currentIR ir.DashboardIR
	if err := json.Unmarshal([]byte(row.currentIRJSON), &currentIR); err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal currentIR: %w", err))
	}
	if row.currentRev > 0 {
		currentIR.Revision = row.currentRev
	}
	if spec.baseRevision != currentIR.Revision {
		return BeginOpResult{}, formatRevisionConflict(spec.revPrefix, spec.baseRevision, currentIR.Revision)
	}
	if spec.targetPanelID != "" && (strings.ContainsAny(spec.targetPanelID, "\r\n") || !panelExists(currentIR.Panels, spec.targetPanelID)) {
		return BeginOpResult{}, fmt.Errorf("%w: %q", ir.ErrPanelNotFound, spec.targetPanelID)
	}
	if row.isActive(nowMs) {
		return BeginOpResult{}, ErrOperationInProgress
	}
	if spec.consumeRepair && row.repairAttemptsUsed >= row.maxRepairAttempts {
		return BeginOpResult{}, fmt.Errorf("%w: used %d of %d repair attempts", ErrBudgetExhausted, row.repairAttemptsUsed, row.maxRepairAttempts)
	}

	// Parse scope and knownGauges BEFORE committing any budget decrement, lease, or slot.
	var scope *ir.ScopeFilter
	if row.scopeJSON != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(row.scopeJSON), &sf); err != nil {
			return BeginOpResult{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal scope: %w", err))
		}
		scope = &sf
	}
	knownGauges, err := unmarshalGaugeMap(row.knownGaugesJSON)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, fmt.Errorf("unmarshal knownGauges: %w", err))
	}
	mergeGaugeMapBounded(knownGauges, spec.catalogGauges)
	gaugesJSON, err := marshalGaugeMap(knownGauges)
	if err != nil {
		return BeginOpResult{}, fmt.Errorf("marshal knownGauges: %w", err)
	}

	if err := s.reapExpiredOrgSlotsTx(ctx, tx, spec.owner.OrgID, nowMs); err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	oKey := ownerKey(spec.owner)
	canAcquire, err := s.canAcquireSlotTx(ctx, tx, spec.owner.OrgID, oKey, nowMs, limits)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	if !canAcquire {
		return BeginOpResult{}, ErrConcurrencyLimited
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)
	leaseUntilMs := nowMs + leaseTTLMs
	expiresAtMs := nowMs + jobTTLMs

	updateSQL := fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = $2, status = $3,
		    known_gauges_json = $4, updated_at_ms = $5, expires_at_ms = $6
		WHERE job_id = $7`, s.jobsTable)
	if spec.consumeRepair {
		// Atomically consume repair budget BEFORE calling the LLM.
		updateSQL = fmt.Sprintf(`UPDATE %s
		SET repair_attempts_used = repair_attempts_used + 1,
		    active_op_token = $1, active_op_until_ms = $2, status = $3,
		    known_gauges_json = $4, updated_at_ms = $5, expires_at_ms = $6
		WHERE job_id = $7`, s.jobsTable)
	}
	if _, err = tx.Exec(ctx, updateSQL,
		opTokenToInt64(newToken), leaseUntilMs, string(spec.nextStatus), gaugesJSON, nowMs, expiresAtMs, spec.jobID); err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	if err := s.upsertSlotTx(ctx, tx, spec.owner.OrgID, slotID, oKey, leaseUntilMs); err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BeginOpResult{}, wrapPostgresErr(opName, err)
	}

	qKind := query.Kind(row.queryKind)
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return BeginOpResult{
		OpToken:     newToken,
		SlotID:      slotID,
		CurrentIR:   currentIR,
		Scope:       scope,
		QueryKind:   qKind,
		KnownGauges: knownGauges,
	}, nil
}

func (s *PostgresStore) SetStatus(ctx context.Context, owner Owner, jobID string, opToken uint64, status JobStatus, now time.Time, jobTTL time.Duration) error {
	if err := validateOwner(owner); err != nil {
		return err
	}
	if err := ValidateJobID(jobID); err != nil {
		return err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wrapPostgresErr("SetStatus", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return wrapPostgresErr("SetStatus", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return err
		}
		return wrapPostgresErr("SetStatus", err)
	}
	if opToken > 0 && row.activeOpToken == opToken && row.isActive(nowMs) {
		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET status = $1, updated_at_ms = $2, expires_at_ms = $3 WHERE job_id = $4`, s.jobsTable),
			string(status), nowMs, nowMs+ttlMs, jobID)
		if err != nil {
			return wrapPostgresErr("SetStatus", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return wrapPostgresErr("SetStatus", err)
		}
	}
	return nil
}

func (s *PostgresStore) FinishJob(ctx context.Context, params FinishJobParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	var docJSON string
	var docRev int
	hasDoc := false
	if params.Doc != nil {
		b, err := json.Marshal(params.Doc)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal doc: %w", err)
		}
		docJSON = string(b)
		docRev = params.Doc.Revision
		hasDoc = true
	}
	diffsJSON, err := marshalOptionalDiffs(params.Diffs)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal diffs: %w", err)
	}
	warnsJSON, err := marshalOptionalStringSlice(params.Warnings)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal warnings: %w", err)
	}
	var reportJSON string
	hasReport := false
	if params.Report != nil {
		b, err := json.Marshal(params.Report)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("%w: marshal report: %v", ErrStoreUnavailable, err)
		}
		reportJSON = string(b)
		hasReport = true
	}
	var metaJSON string
	hasMeta := false
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		metaJSON = string(b)
		hasMeta = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	if params.OpToken == 0 || row.activeOpToken != params.OpToken || !row.isActive(nowMs) {
		snap, snapErr := row.toSnapshot("FinishJob")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ErrJobCancelled
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.status = string(params.Status)
	if hasDoc {
		row.currentIRJSON = docJSON
		row.currentRev = docRev
	}
	if hasReport {
		row.reportJSON = reportJSON
	}
	row.lastDiffJSON = diffsJSON
	row.summary = params.Summary
	row.warningsJSON = warnsJSON
	row.errorMessage = params.ErrorMessage
	if hasMeta {
		row.llmMetaJSON = metaJSON
	}
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	if s.supportsReportStorage.Load() {
		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
			SET active_op_token = $1, active_op_until_ms = 0, status = $2,
			    current_ir_json = $3, current_rev = $4, last_diff_json = $5,
			    summary = $6, warnings_json = $7, error_message = $8,
			    llm_meta_json = $9, updated_at_ms = $10, expires_at_ms = $11,
			    report_json = $12
			WHERE job_id = $13`, s.jobsTable),
			opTokenToInt64(newToken), row.status,
			row.currentIRJSON, row.currentRev, row.lastDiffJSON,
			row.summary, row.warningsJSON, row.errorMessage,
			row.llmMetaJSON, row.updatedAtMs, row.expiresAtMs, row.reportJSON, params.JobID)
	} else {
		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
			SET active_op_token = $1, active_op_until_ms = 0, status = $2,
			    current_ir_json = $3, current_rev = $4, last_diff_json = $5,
			    summary = $6, warnings_json = $7, error_message = $8,
			    llm_meta_json = $9, updated_at_ms = $10, expires_at_ms = $11
			WHERE job_id = $12`, s.jobsTable),
			opTokenToInt64(newToken), row.status,
			row.currentIRJSON, row.currentRev, row.lastDiffJSON,
			row.summary, row.warningsJSON, row.errorMessage,
			row.llmMetaJSON, row.updatedAtMs, row.expiresAtMs, params.JobID)
	}
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("FinishJob", err)
	}
	return row.toSnapshot("FinishJob")
}

func (s *PostgresStore) CommitRevision(ctx context.Context, params CommitRevisionParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	maxHist := params.MaxUndoHistory
	if maxHist <= 0 {
		maxHist = defaultMaxUndoHistory
	}

	nextDocBytes, err := json.Marshal(params.NextIR)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal nextIR: %w", err)
	}
	diffsJSON, err := marshalOptionalDiffs(params.Diffs)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal diffs: %w", err)
	}
	warnsJSON, err := marshalOptionalStringSlice(params.Warnings)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal warnings: %w", err)
	}
	var metaJSON string
	hasMeta := false
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		metaJSON = string(b)
		hasMeta = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, true)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	if params.OpToken == 0 || row.activeOpToken != params.OpToken || !row.isActive(nowMs) {
		snap, snapErr := row.toSnapshot("CommitRevision")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ErrJobCancelled
	}
	if row.currentIRJSON == "" || row.currentRev != params.ExpectedRev {
		newToken, tokErr := nextOpToken(row.activeOpToken)
		if tokErr != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", tokErr)
		}
		_, updErr := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET active_op_token = $1, active_op_until_ms = 0 WHERE job_id = $2`, s.jobsTable),
			opTokenToInt64(newToken), params.JobID)
		if updErr != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", updErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", commitErr)
		}
		snap, snapErr := row.toSnapshot("CommitRevision")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ir.ErrRevisionConflict
	}

	var prevIR ir.DashboardIR
	if err := json.Unmarshal([]byte(row.currentIRJSON), &prevIR); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", fmt.Errorf("unmarshal currentIR: %w", err))
	}
	if row.currentRev > 0 {
		prevIR.Revision = row.currentRev
	}

	var history []ir.DashboardIR
	if row.historyJSON != "" && row.historyJSON != "[]" {
		if err := json.Unmarshal([]byte(row.historyJSON), &history); err != nil {
			return JobSnapshot{}, wrapPostgresErr("CommitRevision", fmt.Errorf("unmarshal history: %w", err))
		}
	}
	history = append(history, prevIR)
	if len(history) > maxHist {
		history = history[len(history)-maxHist:]
	}
	histBytes, err := json.Marshal(history)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal history: %w", err)
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.currentIRJSON = string(nextDocBytes)
	row.currentRev = params.NextIR.Revision
	row.historyJSON = string(histBytes)
	row.historyLen = len(history)
	row.lastDiffJSON = diffsJSON
	row.summary = params.Summary
	row.warningsJSON = warnsJSON
	row.errorMessage = ""
	row.status = string(StatusAwaitingPreview)
	if hasMeta {
		row.llmMetaJSON = metaJSON
	}
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    current_ir_json = $3, current_rev = $4, history_json = $5, history_len = $6,
		    last_diff_json = $7, summary = $8, warnings_json = $9, error_message = '',
		    llm_meta_json = $10, updated_at_ms = $11, expires_at_ms = $12
		WHERE job_id = $13`, s.jobsTable),
		opTokenToInt64(newToken), row.status,
		row.currentIRJSON, row.currentRev, row.historyJSON, row.historyLen,
		row.lastDiffJSON, row.summary, row.warningsJSON,
		row.llmMetaJSON, row.updatedAtMs, row.expiresAtMs, params.JobID)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("CommitRevision", err)
	}
	return row.toSnapshot("CommitRevision")
}

func (s *PostgresStore) RecordEditFailure(ctx context.Context, params RecordEditFailureParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	var metaJSON string
	hasMeta := false
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		metaJSON = string(b)
		hasMeta = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, params.Now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, params.Owner, params.JobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	if params.OpToken == 0 || row.activeOpToken != params.OpToken || !row.isActive(nowMs) {
		snap, snapErr := row.toSnapshot("RecordEditFailure")
		if snapErr != nil {
			return JobSnapshot{}, snapErr
		}
		return snap, ErrJobCancelled
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.status = string(StatusFailed)
	row.errorMessage = params.ErrorMessage
	if hasMeta {
		row.llmMetaJSON = metaJSON
	}
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    error_message = $3, llm_meta_json = $4, updated_at_ms = $5, expires_at_ms = $6
		WHERE job_id = $7`, s.jobsTable),
		opTokenToInt64(newToken), row.status, row.errorMessage, row.llmMetaJSON, row.updatedAtMs, row.expiresAtMs, params.JobID)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobSnapshot{}, wrapPostgresErr("RecordEditFailure", err)
	}
	return row.toSnapshot("RecordEditFailure")
}

func (s *PostgresStore) RevertOnCancel(ctx context.Context, owner Owner, jobID string, opToken uint64, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return JobSnapshot{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return JobSnapshot{}, err
		}
		return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
	}
	if opToken > 0 && row.activeOpToken == opToken && row.isActive(nowMs) {
		newToken, tokErr := nextOpToken(row.activeOpToken)
		if tokErr != nil {
			return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", tokErr)
		}
		row.activeOpToken = newToken
		row.activeOpUntilMs = 0
		if row.jobKind == JobKindReport {
			row.status = string(StatusCancelled)
			row.summary = "Status analysis was cancelled."
		} else if row.currentIRJSON != "" {
			row.status = string(StatusAwaitingPreview)
			row.summary = "処理がキャンセルされたためドラフトは変更されていません。"
		} else {
			row.status = string(StatusCancelled)
			row.summary = "処理がキャンセルされたためドラフトは変更されていません。"
		}
		row.updatedAtMs = nowMs
		row.expiresAtMs = nowMs + ttlMs

		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
			SET active_op_token = $1, active_op_until_ms = 0, status = $2,
			    summary = $3, updated_at_ms = $4, expires_at_ms = $5
			WHERE job_id = $6`, s.jobsTable),
			opTokenToInt64(newToken), row.status, row.summary, row.updatedAtMs, row.expiresAtMs, jobID)
		if err != nil {
			return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return JobSnapshot{}, wrapPostgresErr("RevertOnCancel", err)
		}
	}
	snap, snapErr := row.toSnapshot("RevertOnCancel")
	if snapErr != nil {
		return JobSnapshot{}, snapErr
	}
	return snap, ErrJobCancelled
}

func (s *PostgresStore) Undo(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return MutationResult{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, true)
	if err != nil {
		if isDomainJobErr(err) {
			return MutationResult{}, err
		}
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	if row.jobKind == JobKindReport {
		return MutationResult{}, ErrWrongJobKind
	}
	if row.historyLen == 0 || row.currentIRJSON == "" {
		return MutationResult{}, ErrNoUndoHistory
	}
	if row.currentRev >= ir.MaxRevision {
		return MutationResult{}, fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	}

	var history []ir.DashboardIR
	if row.historyJSON != "" {
		if err := json.Unmarshal([]byte(row.historyJSON), &history); err != nil {
			return MutationResult{}, wrapPostgresErr("Undo", fmt.Errorf("unmarshal history: %w", err))
		}
	}
	if len(history) == 0 {
		return MutationResult{}, ErrNoUndoHistory
	}

	prev := history[len(history)-1]
	history = history[:len(history)-1]
	restored := prev.Clone()
	restored.Revision = row.currentRev + 1

	restoredBytes, err := json.Marshal(restored)
	if err != nil {
		return MutationResult{}, fmt.Errorf("marshal restored IR: %w", err)
	}
	histBytes, err := json.Marshal(history)
	if err != nil {
		return MutationResult{}, fmt.Errorf("marshal history: %w", err)
	}
	diffs := []ir.DiffEntry{{
		Op:     "undo",
		Field:  "revision",
		Before: fmt.Sprintf("rev %d", restored.Revision-1),
		After:  fmt.Sprintf("restored from rev %d as rev %d", prev.Revision, restored.Revision),
	}}
	diffsJSON, err := marshalOptionalDiffs(diffs)
	if err != nil {
		return MutationResult{}, fmt.Errorf("marshal undo diff: %w", err)
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	row.currentIRJSON = string(restoredBytes)
	row.currentRev = restored.Revision
	row.historyJSON = string(histBytes)
	row.historyLen = len(history)
	row.lastDiffJSON = diffsJSON
	row.summary = "直前のドラフト状態を取り消して復元しました。"
	row.status = string(StatusAwaitingPreview)
	row.errorMessage = ""
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    current_ir_json = $3, current_rev = $4, history_json = $5, history_len = $6,
		    last_diff_json = $7, summary = $8, error_message = '',
		    updated_at_ms = $9, expires_at_ms = $10
		WHERE job_id = $11`, s.jobsTable),
		opTokenToInt64(newToken), row.status,
		row.currentIRJSON, row.currentRev, row.historyJSON, row.historyLen,
		row.lastDiffJSON, row.summary,
		row.updatedAtMs, row.expiresAtMs, jobID)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, s.cancelChannel, jobID+":"+strconv.FormatUint(newToken, 10)); err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationResult{}, wrapPostgresErr("Undo", err)
	}
	snap, err := row.toSnapshot("Undo")
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Snapshot: snap, OpToken: newToken}, nil
}

func (s *PostgresStore) Cancel(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return MutationResult{}, err
	}

	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	defer rollbackTx(ctx, tx)

	nowMs, err := resolvePostgresNowMs(ctx, tx, now)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	row, err := s.verifyOwnedRowTx(ctx, tx, owner, jobID, nowMs, false)
	if err != nil {
		if isDomainJobErr(err) {
			return MutationResult{}, err
		}
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}

	if row.jobKind == JobKindReport && !row.isActive(nowMs) && isTerminalReportStatus(JobStatus(row.status)) {
		snap, snapErr := row.toSnapshot("Cancel")
		if snapErr != nil {
			return MutationResult{}, snapErr
		}
		return MutationResult{Snapshot: snap, OpToken: row.activeOpToken}, nil
	}

	newToken, err := nextOpToken(row.activeOpToken)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	row.activeOpToken = newToken
	row.activeOpUntilMs = 0
	if row.jobKind == JobKindReport {
		row.status = string(StatusCancelled)
		row.summary = "Status analysis was cancelled."
	} else if row.currentIRJSON == "" {
		row.status = string(StatusCancelled)
		row.summary = "実行中の処理をキャンセルしました。"
	} else {
		row.status = string(StatusAwaitingPreview)
		row.summary = "実行中の処理をキャンセルしました。"
	}
	row.updatedAtMs = nowMs
	row.expiresAtMs = nowMs + ttlMs

	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
		SET active_op_token = $1, active_op_until_ms = 0, status = $2,
		    summary = $3, updated_at_ms = $4, expires_at_ms = $5
		WHERE job_id = $6`, s.jobsTable),
		opTokenToInt64(newToken), row.status, row.summary, row.updatedAtMs, row.expiresAtMs, jobID)
	if err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, s.cancelChannel, jobID+":"+strconv.FormatUint(newToken, 10)); err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationResult{}, wrapPostgresErr("Cancel", err)
	}
	snap, err := row.toSnapshot("Cancel")
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Snapshot: snap, OpToken: newToken}, nil
}

// ReleaseSlot deletes only the caller's exact slot row (single-row PK lookup) so concurrent
// slot releases can never deadlock (40P01) against org reaping or background CleanupExpired.
func (s *PostgresStore) ReleaseSlot(ctx context.Context, owner Owner, slotID string, _ time.Time) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND slot_id = $2 AND owner_key = $3`, s.slotsTable),
		owner.OrgID, slotID, ownerKey(owner))
	if err != nil {
		return wrapPostgresErr("ReleaseSlot", err)
	}
	return nil
}

// CleanupExpired removes expired inactive jobs and expired slots in bounded SKIP LOCKED batches
// so background cleanup never deadlocks concurrent org transactions or rolls back on large backlogs.
func (s *PostgresStore) CleanupExpired(ctx context.Context, now time.Time) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	nowMs, err := resolvePostgresNowMs(ctx, s.pool, now)
	if err != nil {
		return wrapPostgresErr("CleanupExpired", err)
	}

	deleteJobsSQL := fmt.Sprintf(`DELETE FROM %s WHERE job_id IN (
		SELECT job_id FROM %s
		WHERE active_op_until_ms <= $1 AND expires_at_ms > 0 AND expires_at_ms < $1
		ORDER BY job_id
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	)`, s.jobsTable, s.jobsTable)
	for ctx.Err() == nil {
		tag, err := s.pool.Exec(ctx, deleteJobsSQL, nowMs, postgresCleanupBatchSize)
		if err != nil {
			return wrapPostgresErr("CleanupExpired", err)
		}
		if tag.RowsAffected() < postgresCleanupBatchSize {
			break
		}
	}

	deleteSlotsSQL := fmt.Sprintf(`DELETE FROM %s WHERE (org_id, slot_id) IN (
		SELECT org_id, slot_id FROM %s
		WHERE expires_at_ms <= $1
		ORDER BY org_id, slot_id
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	)`, s.slotsTable, s.slotsTable)
	for ctx.Err() == nil {
		tag, err := s.pool.Exec(ctx, deleteSlotsSQL, nowMs, postgresCleanupBatchSize)
		if err != nil {
			return wrapPostgresErr("CleanupExpired", err)
		}
		if tag.RowsAffected() < postgresCleanupBatchSize {
			break
		}
	}
	return nil
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return wrapPostgresErr("Ping", err)
	}
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	return nil
}

func (s *PostgresStore) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closed = true
		s.closeCancel()
		s.closeMu.Unlock()
		s.subWG.Wait()
		if s.ownsPool && s.pool != nil {
			s.pool.Close()
		}
	})
	return nil
}

// --- Transactional capacity & slot helpers ---

func (s *PostgresStore) evictExpiredOrgTx(ctx context.Context, tx pgx.Tx, orgID int64, nowMs int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND active_op_until_ms <= $2 AND expires_at_ms > 0 AND expires_at_ms < $2`, s.jobsTable),
		orgID, nowMs)
	return err
}

func (s *PostgresStore) reapExpiredOrgSlotsTx(ctx context.Context, tx pgx.Tx, orgID int64, nowMs int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND expires_at_ms <= $2`, s.slotsTable),
		orgID, nowMs)
	return err
}

func (s *PostgresStore) canAcquireSlotTx(ctx context.Context, tx pgx.Tx, orgID int64, oKey string, nowMs int64, limits StoreLimits) (bool, error) {
	var orgSlots int
	var userSlots int
	err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT
		COUNT(*) FILTER (WHERE expires_at_ms > $2),
		COUNT(*) FILTER (WHERE owner_key = $3 AND expires_at_ms > $2)
	FROM %s WHERE org_id = $1`, s.slotsTable), orgID, nowMs, oKey).Scan(&orgSlots, &userSlots)
	if err != nil {
		return false, err
	}
	if orgSlots >= limits.MaxConcurrentJobs || userSlots >= limits.MaxConcurrentJobsPerUser {
		return false, nil
	}
	return true, nil
}

func (s *PostgresStore) upsertSlotTx(ctx context.Context, tx pgx.Tx, orgID int64, slotID string, oKey string, leaseUntilMs int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (org_id, slot_id, owner_key, expires_at_ms)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (org_id, slot_id) DO UPDATE
		SET owner_key = EXCLUDED.owner_key, expires_at_ms = EXCLUDED.expires_at_ms`, s.slotsTable),
		orgID, slotID, oKey, leaseUntilMs)
	return err
}

func (s *PostgresStore) ensureCapacityTx(ctx context.Context, tx pgx.Tx, caller Owner, callerKey string, nowMs int64, limits StoreLimits) error {
	return s.ensureOrgCapacityTx(ctx, tx, "ensureCapacity", caller, callerKey, nowMs, limits, true)
}

// ensureImportCapacityTx transactionally enforces per-user and per-org caps for Import jobs
// without ever evicting another user's non-expired job or any active job.
func (s *PostgresStore) ensureImportCapacityTx(ctx context.Context, tx pgx.Tx, caller Owner, callerKey string, nowMs int64, limits StoreLimits) error {
	return s.ensureOrgCapacityTx(ctx, tx, "ensureImportCapacity", caller, callerKey, nowMs, limits, false)
}

func (s *PostgresStore) ensureOrgCapacityTx(
	ctx context.Context,
	tx pgx.Tx,
	opName string,
	caller Owner,
	callerKey string,
	nowMs int64,
	limits StoreLimits,
	allowOtherOrgEviction bool,
) error {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT job_id, owner_key, active_op_until_ms FROM %s WHERE org_id = $1 ORDER BY updated_at_ms ASC, job_id ASC`, s.jobsTable), caller.OrgID)
	if err != nil {
		return wrapPostgresErr(opName, err)
	}
	defer rows.Close()

	var callerInactive []string
	var otherOrgInactive []string
	userCount := 0
	orgCount := 0

	for rows.Next() {
		var jID, oKey string
		var activeUntil int64
		if scanErr := rows.Scan(&jID, &oKey, &activeUntil); scanErr != nil {
			return wrapPostgresErr(opName, scanErr)
		}
		orgCount++
		isActive := activeUntil > 0 && nowMs < activeUntil
		if oKey == callerKey {
			userCount++
			if !isActive {
				callerInactive = append(callerInactive, jID)
			}
		} else if allowOtherOrgEviction && !isActive {
			otherOrgInactive = append(otherOrgInactive, jID)
		}
	}
	if err := rows.Err(); err != nil {
		return wrapPostgresErr(opName, err)
	}

	victims, err := selectEvictionVictims(orgCount, userCount, callerInactive, otherOrgInactive, limits, allowOtherOrgEviction)
	if err != nil {
		return err
	}
	if len(victims) > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE org_id = $1 AND job_id = ANY($2) AND active_op_until_ms <= $3`, s.jobsTable),
			caller.OrgID, victims, nowMs); err != nil {
			return wrapPostgresErr(opName, err)
		}
	}
	return nil
}

// --- Error helpers ---

func isPgUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isDomainJobErr(err error) bool {
	return errors.Is(err, ErrJobNotFound) ||
		errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrOperationInProgress) ||
		errors.Is(err, ErrConcurrencyLimited) ||
		errors.Is(err, ErrBudgetExhausted) ||
		errors.Is(err, ErrNoUndoHistory) ||
		errors.Is(err, ErrJobCancelled) ||
		errors.Is(err, ErrWrongJobKind) ||
		errors.Is(err, ErrReportStorageMigrationRequired) ||
		errors.Is(err, ir.ErrRevisionConflict) ||
		errors.Is(err, ir.ErrPanelNotFound)
}

func wrapPostgresErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrStoreUnavailable) {
		return err
	}
	return fmt.Errorf("%w: postgres %s: %v", ErrStoreUnavailable, op, err)
}
