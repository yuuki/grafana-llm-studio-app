package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

func (s *RedisStore) CreateGenerating(ctx context.Context, params CreateGeneratingParams) (OpLease, error) {
	if err := validateOwner(params.Owner); err != nil {
		return OpLease{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return OpLease{}, err
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

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)
	oKey := ownerKey(params.Owner)

	initStatus := StatusGenerating
	if params.JobKind == JobKindReport {
		initStatus = StatusAnalyzing
	}

	res, err := redisScriptCreateGenerating.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.orgIdxKey(params.Owner.OrgID),
			s.userIdxKey(params.Owner),
			s.orgMetaKey(params.Owner.OrgID),
			s.orgSlotsKey(params.Owner.OrgID),
			s.userSlotsKey(params.Owner),
			s.orgsSetKey(),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		oKey,
		params.DatasourceUID,
		string(params.QueryKind),
		scopeJSON,
		strconv.Itoa(params.MaxRepairAttempts),
		strings.Join(params.KnownGauges, "\n"),
		slotID,
		redisNowArgMs(params.Now),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.FormatInt(leaseTTLMs, 10),
		strconv.Itoa(limits.MaxJobs),
		strconv.Itoa(limits.MaxJobsPerUser),
		strconv.Itoa(limits.MaxConcurrentJobs),
		strconv.Itoa(limits.MaxConcurrentJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
		strconv.Itoa(maxKnownGaugesPerJob),
		params.JobKind,
		string(initStatus),
	).Result()
	if err != nil {
		return OpLease{}, wrapRedisErr("CreateGenerating", err)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return OpLease{}, wrapRedisErr("CreateGenerating", fmt.Errorf("unexpected reply %T", res))
	}
	if asString(arr[0]) == "ERR" {
		return OpLease{}, mapRedisScriptError(arr[1:], "", "")
	}
	token, err := strconv.ParseUint(asString(arr[1]), 10, 64)
	if err != nil {
		return OpLease{}, wrapRedisErr("CreateGenerating", fmt.Errorf("invalid opToken: %w", err))
	}
	if len(arr) >= 3 {
		s.cleanupEvictedRecords(ctx, arr[2], oKey)
	}
	return OpLease{OpToken: token, SlotID: slotID}, nil
}

func (s *RedisStore) CreateImported(ctx context.Context, params CreateImportedParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
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

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	oKey := ownerKey(params.Owner)

	res, err := redisScriptCreateImported.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.orgIdxKey(params.Owner.OrgID),
			s.userIdxKey(params.Owner),
			s.orgMetaKey(params.Owner.OrgID),
			s.orgsSetKey(),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		oKey,
		params.DatasourceUID,
		string(params.QueryKind),
		scopeJSON,
		importSrcJSON,
		string(docBytes),
		strconv.Itoa(params.Draft.Revision),
		strings.Join(extractPanelIDs(params.Draft.Panels), "\n"),
		params.Summary,
		warnsJSON,
		strconv.Itoa(params.MaxRepairAttempts),
		strings.Join(params.KnownGauges, "\n"),
		redisNowArgMs(params.Now),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.Itoa(limits.MaxJobs),
		strconv.Itoa(limits.MaxJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
		strconv.Itoa(maxKnownGaugesPerJob),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("CreateImported", err)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return JobSnapshot{}, wrapRedisErr("CreateImported", fmt.Errorf("unexpected reply %T", res))
	}
	if asString(arr[0]) == "ERR" {
		return JobSnapshot{}, mapRedisScriptError(arr[1:], "", "")
	}
	if len(arr) >= 3 {
		s.cleanupEvictedRecords(ctx, arr[2], oKey)
	}
	return snapshotFromHMGetReply(arr[1], "CreateImported")
}

func (s *RedisStore) Get(ctx context.Context, owner Owner, jobID string, now time.Time) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	res, err := redisScriptGetJob.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.orgIdxKey(owner.OrgID),
			s.userIdxKey(owner),
			s.orgMetaKey(owner.OrgID),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		redisNowArgMs(now),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("Get", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return JobSnapshot{}, wrapRedisErr("Get", fmt.Errorf("unexpected reply %T", res))
	}
	if asString(arr[0]) == "ERR" {
		return JobSnapshot{}, mapRedisScriptError(arr[1:], "", "")
	}
	return snapshotFromHMGetReply(arr[1], "Get")
}

func (s *RedisStore) GetRepairCandidate(ctx context.Context, owner Owner, jobID string, now time.Time) (RepairCandidate, error) {
	if err := validateOwner(owner); err != nil {
		return RepairCandidate{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return RepairCandidate{}, err
	}
	res, err := redisScriptGetRepairCandidate.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.orgIdxKey(owner.OrgID),
			s.userIdxKey(owner),
			s.orgMetaKey(owner.OrgID),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		redisNowArgMs(now),
	).Result()
	if err != nil {
		return RepairCandidate{}, wrapRedisErr("GetRepairCandidate", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return RepairCandidate{}, wrapRedisErr("GetRepairCandidate", fmt.Errorf("unexpected reply %T", res))
	}
	if asString(arr[0]) == "ERR" {
		return RepairCandidate{}, mapRedisScriptError(arr[1:], "", "")
	}
	if len(arr) < 8 {
		return RepairCandidate{}, wrapRedisErr("GetRepairCandidate", fmt.Errorf("short reply len=%d", len(arr)))
	}
	var docPtr *ir.DashboardIR
	if raw := asString(arr[1]); raw != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return RepairCandidate{}, wrapRedisErr("GetRepairCandidate", fmt.Errorf("unmarshal currentIR: %w", err))
		}
		if rev, err := strconv.Atoi(asString(arr[2])); err == nil && rev > 0 {
			doc.Revision = rev
		}
		docPtr = &doc
	}
	used, _ := strconv.Atoi(asString(arr[3]))
	maxRep, _ := strconv.Atoi(asString(arr[4]))
	untilMs, _ := strconv.ParseInt(asString(arr[5]), 10, 64)
	qKind := query.Kind(asString(arr[6]))
	if qKind == "" {
		qKind = query.KindPromQL
	}
	serverNowMs, _ := strconv.ParseInt(asString(arr[7]), 10, 64)
	return RepairCandidate{
		CurrentIR:          docPtr,
		RepairAttemptsUsed: used,
		MaxRepairAttempts:  maxRep,
		HasActiveOp:        untilMs > serverNowMs,
		QueryKind:          qKind,
	}, nil
}

func (s *RedisStore) BeginPatch(ctx context.Context, params BeginPatchParams) (BeginOpResult, error) {
	if err := validateOwner(params.Owner); err != nil {
		return BeginOpResult{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return BeginOpResult{}, err
	}
	if strings.ContainsAny(params.TargetPanelID, "\r\n") {
		return BeginOpResult{}, fmt.Errorf("%w: %q", ir.ErrPanelNotFound, params.TargetPanelID)
	}
	limits := params.Limits.normalized()
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)

	res, err := redisScriptBeginPatch.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.orgIdxKey(params.Owner.OrgID),
			s.userIdxKey(params.Owner),
			s.orgMetaKey(params.Owner.OrgID),
			s.orgSlotsKey(params.Owner.OrgID),
			s.userSlotsKey(params.Owner),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		ownerKey(params.Owner),
		strconv.Itoa(params.BaseRevision),
		params.TargetPanelID,
		strings.Join(params.CatalogGauges, "\n"),
		slotID,
		redisNowArgMs(params.Now),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.FormatInt(leaseTTLMs, 10),
		strconv.Itoa(limits.MaxConcurrentJobs),
		strconv.Itoa(limits.MaxConcurrentJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
		strconv.Itoa(maxKnownGaugesPerJob),
	).Result()
	if err != nil {
		return BeginOpResult{}, wrapRedisErr("BeginPatch", err)
	}
	return decodeBeginOpReply(res, slotID, "", params.BaseRevision, params.TargetPanelID, "job has no active IR draft to patch")
}

func (s *RedisStore) BeginRepair(ctx context.Context, params BeginRepairParams) (BeginOpResult, error) {
	if err := validateOwner(params.Owner); err != nil {
		return BeginOpResult{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return BeginOpResult{}, err
	}
	limits := params.Limits.normalized()
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)

	res, err := redisScriptBeginRepair.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.orgIdxKey(params.Owner.OrgID),
			s.userIdxKey(params.Owner),
			s.orgMetaKey(params.Owner.OrgID),
			s.orgSlotsKey(params.Owner.OrgID),
			s.userSlotsKey(params.Owner),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		ownerKey(params.Owner),
		strconv.Itoa(params.BaseRevision),
		strings.Join(params.CatalogGauges, "\n"),
		slotID,
		redisNowArgMs(params.Now),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.FormatInt(leaseTTLMs, 10),
		strconv.Itoa(limits.MaxConcurrentJobs),
		strconv.Itoa(limits.MaxConcurrentJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
		strconv.Itoa(maxKnownGaugesPerJob),
	).Result()
	if err != nil {
		return BeginOpResult{}, wrapRedisErr("BeginRepair", err)
	}
	return decodeBeginOpReply(res, slotID, "repair", params.BaseRevision, "", "job has no active IR draft to repair")
}

func (s *RedisStore) SetStatus(ctx context.Context, owner Owner, jobID string, opToken uint64, status JobStatus, now time.Time, jobTTL time.Duration) error {
	if err := validateOwner(owner); err != nil {
		return err
	}
	if err := ValidateJobID(jobID); err != nil {
		return err
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptSetStatus.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.orgIdxKey(owner.OrgID),
			s.userIdxKey(owner),
			s.orgMetaKey(owner.OrgID),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		ownerKey(owner),
		strconv.FormatUint(opToken, 10),
		string(status),
		redisNowArgMs(now),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return wrapRedisErr("SetStatus", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 1 {
		return wrapRedisErr("SetStatus", fmt.Errorf("unexpected reply %T", res))
	}
	if asString(arr[0]) == "ERR" {
		return mapRedisScriptError(arr[1:], "", "")
	}
	return nil
}

func (s *RedisStore) FinishJob(ctx context.Context, params FinishJobParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)

	hasDoc := "0"
	docJSON := ""
	docRev := "0"
	panelIDs := ""
	if params.Doc != nil {
		b, err := json.Marshal(params.Doc)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal doc: %w", err)
		}
		hasDoc = "1"
		docJSON = string(b)
		docRev = strconv.Itoa(params.Doc.Revision)
		panelIDs = strings.Join(extractPanelIDs(params.Doc.Panels), "\n")
	}
	hasReport := "0"
	reportJSON := ""
	if params.Report != nil {
		b, err := json.Marshal(params.Report)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("%w: marshal report: %v", ErrStoreUnavailable, err)
		}
		hasReport = "1"
		reportJSON = string(b)
	}
	diffsJSON, err := marshalOptionalDiffs(params.Diffs)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal diffs: %w", err)
	}
	warnsJSON, err := marshalOptionalStringSlice(params.Warnings)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("marshal warnings: %w", err)
	}
	hasMeta := "0"
	metaJSON := ""
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		hasMeta = "1"
		metaJSON = string(b)
	}

	res, err := redisScriptFinishJob.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.orgIdxKey(params.Owner.OrgID),
			s.userIdxKey(params.Owner),
			s.orgMetaKey(params.Owner.OrgID),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		ownerKey(params.Owner),
		strconv.FormatUint(params.OpToken, 10),
		string(params.Status),
		hasDoc,
		docJSON,
		docRev,
		panelIDs,
		diffsJSON,
		params.Summary,
		warnsJSON,
		params.ErrorMessage,
		hasMeta,
		metaJSON,
		redisNowArgMs(params.Now),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
		hasReport,
		reportJSON,
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("FinishJob", err)
	}
	return decodeSnapshotStatusReply(res, "FinishJob", nil)
}

func (s *RedisStore) CommitRevision(ctx context.Context, params CommitRevisionParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	maxHist := params.MaxUndoHistory
	if maxHist <= 0 {
		maxHist = defaultMaxUndoHistory
	}

	docBytes, err := json.Marshal(params.NextIR)
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
	hasMeta := "0"
	metaJSON := ""
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		hasMeta = "1"
		metaJSON = string(b)
	}

	res, err := redisScriptCommitRevision.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.orgIdxKey(params.Owner.OrgID),
			s.userIdxKey(params.Owner),
			s.orgMetaKey(params.Owner.OrgID),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		ownerKey(params.Owner),
		strconv.FormatUint(params.OpToken, 10),
		strconv.Itoa(params.ExpectedRev),
		string(docBytes),
		strconv.Itoa(params.NextIR.Revision),
		strings.Join(extractPanelIDs(params.NextIR.Panels), "\n"),
		diffsJSON,
		params.Summary,
		warnsJSON,
		hasMeta,
		metaJSON,
		strconv.Itoa(maxHist),
		redisNowArgMs(params.Now),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("CommitRevision", err)
	}
	return decodeSnapshotStatusReply(res, "CommitRevision", nil)
}

func (s *RedisStore) RecordEditFailure(ctx context.Context, params RecordEditFailureParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(params.JobID); err != nil {
		return JobSnapshot{}, err
	}
	ttlMs := maxInt64(normalizeJobTTL(params.JobTTL).Milliseconds(), 1)
	hasMeta := "0"
	metaJSON := ""
	if params.LLMMeta != nil {
		b, err := json.Marshal(params.LLMMeta)
		if err != nil {
			return JobSnapshot{}, fmt.Errorf("marshal llmMeta: %w", err)
		}
		hasMeta = "1"
		metaJSON = string(b)
	}

	res, err := redisScriptRecordEditFailure.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.orgIdxKey(params.Owner.OrgID),
			s.userIdxKey(params.Owner),
			s.orgMetaKey(params.Owner.OrgID),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		ownerKey(params.Owner),
		strconv.FormatUint(params.OpToken, 10),
		params.ErrorMessage,
		hasMeta,
		metaJSON,
		redisNowArgMs(params.Now),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("RecordEditFailure", err)
	}
	return decodeSnapshotStatusReply(res, "RecordEditFailure", nil)
}

func (s *RedisStore) RevertOnCancel(ctx context.Context, owner Owner, jobID string, opToken uint64, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return JobSnapshot{}, err
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptRevertOnCancel.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.orgIdxKey(owner.OrgID),
			s.userIdxKey(owner),
			s.orgMetaKey(owner.OrgID),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		ownerKey(owner),
		strconv.FormatUint(opToken, 10),
		"処理がキャンセルされたためドラフトは変更されていません。",
		redisNowArgMs(now),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("RevertOnCancel", err)
	}
	return decodeSnapshotStatusReply(res, "RevertOnCancel", ErrJobCancelled)
}

func (s *RedisStore) Undo(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptUndo.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.orgIdxKey(owner.OrgID),
			s.userIdxKey(owner),
			s.orgMetaKey(owner.OrgID),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		ownerKey(owner),
		strconv.Itoa(ir.MaxRevision),
		"直前のドラフト状態を取り消して復元しました。",
		redisNowArgMs(now),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
		s.cancelChannel(),
	).Result()
	if err != nil {
		return MutationResult{}, wrapRedisErr("Undo", err)
	}
	return decodeMutationReply(res, "Undo")
}

func (s *RedisStore) Cancel(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (MutationResult, error) {
	if err := validateOwner(owner); err != nil {
		return MutationResult{}, err
	}
	if err := ValidateJobID(jobID); err != nil {
		return MutationResult{}, err
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptCancel.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.orgIdxKey(owner.OrgID),
			s.userIdxKey(owner),
			s.orgMetaKey(owner.OrgID),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		ownerKey(owner),
		"実行中の処理をキャンセルしました。",
		redisNowArgMs(now),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
		s.cancelChannel(),
	).Result()
	if err != nil {
		return MutationResult{}, wrapRedisErr("Cancel", err)
	}
	return decodeMutationReply(res, "Cancel")
}

func (s *RedisStore) ReleaseSlot(ctx context.Context, owner Owner, slotID string, now time.Time) error {
	_, err := redisScriptReleaseSlot.Run(ctx, s.client,
		[]string{
			s.orgSlotsKey(owner.OrgID),
			s.userSlotsKey(owner),
		},
		slotID,
		redisNowArgMs(now),
	).Result()
	if err != nil {
		return wrapRedisErr("ReleaseSlot", err)
	}
	return nil
}

func (s *RedisStore) CleanupExpired(ctx context.Context, now time.Time) error {
	orgIDs, err := s.client.SMembers(ctx, s.orgsSetKey()).Result()
	if err != nil {
		return wrapRedisErr("CleanupExpired", err)
	}
	nowArg := redisNowArgMs(now)
	for _, rawOrg := range orgIDs {
		orgID, err := strconv.ParseInt(rawOrg, 10, 64)
		if err != nil || orgID <= 0 {
			continue
		}
		res, err := redisScriptCleanupOrg.Run(ctx, s.client,
			[]string{
				s.orgIdxKey(orgID),
				s.orgMetaKey(orgID),
				s.orgSlotsKey(orgID),
			},
			nowArg,
		).Result()
		if err != nil {
			return wrapRedisErr("CleanupExpired", err)
		}
		s.cleanupEvictedRecords(ctx, res, "")
	}
	return nil
}

// cleanupEvictedRecords deletes evicted job hashes and removes ghost entries from other owners'
// user index ZSETs outside Lua so every Lua script strictly accesses only its declared KEYS[].
func (s *RedisStore) cleanupEvictedRecords(ctx context.Context, raw any, alreadyCleanedOwnerKey string) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return
	}
	jobKeys := make([]string, 0, len(items))
	otherUserRemovals := make(map[string][]any)
	for _, item := range items {
		entry := asString(item)
		if entry == "" {
			continue
		}
		jobID := entry
		oKey := ""
		if sep := strings.IndexByte(entry, '|'); sep > 0 {
			jobID = entry[:sep]
			oKey = entry[sep+1:]
		}
		if ValidateJobID(jobID) != nil {
			continue
		}
		jobKeys = append(jobKeys, s.jobKey(jobID))
		if oKey != "" && oKey != alreadyCleanedOwnerKey {
			otherUserRemovals[oKey] = append(otherUserRemovals[oKey], jobID)
		}
	}
	if len(jobKeys) == 0 && len(otherUserRemovals) == 0 {
		return
	}
	pipe := s.client.Pipeline()
	if len(jobKeys) > 0 {
		pipe.Del(ctx, jobKeys...)
	}
	for oKey, ids := range otherUserRemovals {
		pipe.ZRem(ctx, s.userIdxKeyByOwnerKey(oKey), ids...)
	}
	_, _ = pipe.Exec(ctx)
}

// --- Reply decoding helpers ---

func decodeBeginOpReply(res any, slotID, revPrefix string, requestedRev int, targetPanelID, noDraftMsg string) (BeginOpResult, error) {
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return BeginOpResult{}, wrapRedisErr("BeginOp", fmt.Errorf("unexpected reply %T", res))
	}
	if asString(arr[0]) == "ERR" {
		code := asString(arr[1])
		switch code {
		case "NO_DRAFT":
			return BeginOpResult{}, errors.New(noDraftMsg)
		case "REVISION_CONFLICT":
			curRev := 0
			if len(arr) >= 3 {
				curRev, _ = strconv.Atoi(asString(arr[2]))
			}
			return BeginOpResult{}, formatRevisionConflict(revPrefix, requestedRev, curRev)
		case "PANEL_NOT_FOUND":
			return BeginOpResult{}, fmt.Errorf("%w: %q", ir.ErrPanelNotFound, targetPanelID)
		case "BUDGET_EXHAUSTED":
			used, maxRep := 0, 0
			if len(arr) >= 4 {
				used, _ = strconv.Atoi(asString(arr[2]))
				maxRep, _ = strconv.Atoi(asString(arr[3]))
			}
			return BeginOpResult{}, fmt.Errorf("%w: used %d of %d repair attempts", ErrBudgetExhausted, used, maxRep)
		default:
			return BeginOpResult{}, mapRedisScriptError(arr[1:], revPrefix, targetPanelID)
		}
	}
	if len(arr) < 7 {
		return BeginOpResult{}, wrapRedisErr("BeginOp", fmt.Errorf("short OK reply len=%d", len(arr)))
	}
	opToken, err := strconv.ParseUint(asString(arr[1]), 10, 64)
	if err != nil {
		return BeginOpResult{}, wrapRedisErr("BeginOp", fmt.Errorf("invalid opToken: %w", err))
	}
	var currentIR ir.DashboardIR
	if err := json.Unmarshal([]byte(asString(arr[2])), &currentIR); err != nil {
		return BeginOpResult{}, wrapRedisErr("BeginOp", fmt.Errorf("unmarshal currentIR: %w", err))
	}
	if rev, err := strconv.Atoi(asString(arr[3])); err == nil && rev > 0 {
		currentIR.Revision = rev
	}
	var scope *ir.ScopeFilter
	if rawScope := asString(arr[4]); rawScope != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(rawScope), &sf); err != nil {
			return BeginOpResult{}, wrapRedisErr("BeginOp", fmt.Errorf("unmarshal scope: %w", err))
		}
		scope = &sf
	}
	qKind := query.Kind(asString(arr[5]))
	if qKind == "" {
		qKind = query.KindPromQL
	}
	knownGauges := parseNewlineSet(asString(arr[6]))
	return BeginOpResult{
		OpToken:     opToken,
		SlotID:      slotID,
		CurrentIR:   currentIR,
		Scope:       scope,
		QueryKind:   qKind,
		KnownGauges: knownGauges,
	}, nil
}

func decodeSnapshotStatusReply(res any, opName string, okErr error) (JobSnapshot, error) {
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unexpected reply %T", res))
	}
	tag := asString(arr[0])
	if tag == "ERR" {
		return JobSnapshot{}, mapRedisScriptError(arr[1:], "", "")
	}
	snap, err := snapshotFromHMGetReply(arr[1], opName)
	if err != nil {
		return JobSnapshot{}, err
	}
	switch tag {
	case "OK":
		return snap, okErr
	case "CANCELLED":
		return snap, ErrJobCancelled
	case "CONFLICT":
		return snap, ir.ErrRevisionConflict
	default:
		return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unknown status tag %q", tag))
	}
}

func decodeMutationReply(res any, opName string) (MutationResult, error) {
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return MutationResult{}, wrapRedisErr(opName, fmt.Errorf("unexpected reply %T", res))
	}
	tag := asString(arr[0])
	if tag == "ERR" {
		return MutationResult{}, mapRedisScriptError(arr[1:], "", "")
	}
	if tag != "OK" || len(arr) < 3 {
		return MutationResult{}, wrapRedisErr(opName, fmt.Errorf("unexpected mutation reply tag=%q len=%d", tag, len(arr)))
	}
	snap, err := snapshotFromHMGetReply(arr[1], opName)
	if err != nil {
		return MutationResult{}, err
	}
	token, err := strconv.ParseUint(asString(arr[2]), 10, 64)
	if err != nil {
		return MutationResult{}, wrapRedisErr(opName, fmt.Errorf("invalid opToken: %w", err))
	}
	return MutationResult{Snapshot: snap, OpToken: token}, nil
}

func mapRedisScriptError(args []any, _ string, _ string) error {
	if len(args) == 0 {
		return wrapRedisErr("script", errors.New("empty error code"))
	}
	switch asString(args[0]) {
	case "NOT_FOUND":
		return ErrJobNotFound
	case "FORBIDDEN":
		return ErrForbidden
	case "OP_IN_PROGRESS":
		return ErrOperationInProgress
	case "CONCURRENCY_LIMITED":
		return ErrConcurrencyLimited
	case "NO_UNDO_HISTORY":
		return ErrNoUndoHistory
	case "WRONG_JOB_KIND":
		return ErrWrongJobKind
	case "REVISION_LIMIT":
		return fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	case "DUPLICATE_JOB":
		return errors.New("job id already exists")
	default:
		return wrapRedisErr("script", fmt.Errorf("store error code %s", asString(args[0])))
	}
}

// snapshotFromHMGetReply decodes the HMGET sequence returned by Lua read_snapshot():
// [0]=id, [1]=datasource_uid, [2]=query_kind, [3]=scope_json, [4]=import_source_json,
// [5]=status, [6]=current_ir_json, [7]=current_revision, [8]=hist_len, [9]=last_diff_json,
// [10]=summary, [11]=warnings_json, [12]=error_message, [13]=repair_attempts_used,
// [14]=max_repair_attempts, [15]=llm_meta_json, [16]=updated_at_ms,
// optional additive fields: [17]=job_kind, [18]=report_json.
func snapshotFromHMGetReply(v any, opName string) (JobSnapshot, error) {
	vals, ok := v.([]any)
	if !ok || len(vals) < 17 {
		return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unexpected HMGET reply %T", v))
	}
	field := func(idx int) string {
		if idx >= len(vals) || vals[idx] == nil {
			return ""
		}
		return asString(vals[idx])
	}

	curRev, _ := strconv.Atoi(field(7))
	histLen, _ := strconv.Atoi(field(8))
	used, _ := strconv.Atoi(field(13))
	maxRep, _ := strconv.Atoi(field(14))
	updatedMs, _ := strconv.ParseInt(field(16), 10, 64)

	snap, err := serializedJobFields{
		JobID:              field(0),
		DatasourceUID:      field(1),
		QueryKind:          field(2),
		ScopeJSON:          field(3),
		ImportSourceJSON:   field(4),
		Status:             field(5),
		CurrentIRJSON:      field(6),
		CurrentRev:         curRev,
		HistoryLen:         histLen,
		LastDiffJSON:       field(9),
		Summary:            field(10),
		WarningsJSON:       field(11),
		ErrorMessage:       field(12),
		RepairAttemptsUsed: used,
		MaxRepairAttempts:  maxRep,
		LLMMetaJSON:        field(15),
		UpdatedAtMs:        updatedMs,
		JobKind:            field(17),
		ReportJSON:         field(18),
	}.decodeSnapshot()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr(opName, err)
	}
	return snap, nil
}

func parseNewlineSet(raw string) map[string]bool {
	out := make(map[string]bool)
	if raw == "" {
		return out
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out[line] = true
		}
	}
	return out
}

func asString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case []byte:
		return string(s)
	case int64:
		return strconv.FormatInt(s, 10)
	case uint64:
		return strconv.FormatUint(s, 10)
	case int:
		return strconv.Itoa(s)
	default:
		return fmt.Sprint(v)
	}
}

func wrapRedisErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: redis %s: %v", ErrStoreUnavailable, op, err)
}
