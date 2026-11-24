package jobs

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var _ JobStore = (*RedisStore)(nil)

const (
	DefaultRedisKeyPrefix = "llmstudio:jobs"
	redisIndexTTLBufferMs = int64(60_000)
	redisDefaultTimeout   = 5 * time.Second
)

var validRedisKeyPrefixRe = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,64}$`)

// RedisConfig configures the optional persistent Redis JobStore for HA Grafana deployments.
type RedisConfig struct {
	Addr         string
	Username     string
	Password     string
	DB           int
	KeyPrefix    string
	TLS          bool
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// Client optionally supplies a pre-configured redis.UniversalClient (e.g. for custom TLS,
	// Sentinel, Cluster, or testing). When set, Addr/Username/Password/DB/TLS are ignored and
	// Close() does not close the externally owned client.
	Client redis.UniversalClient
}

// ValidateRedisAddr validates that addr is a bare host:port with port in 1..65535
// (disallowing URL schemes and embedded credentials in jsonData).
func ValidateRedisAddr(addr string) error {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return errors.New("redisAddr is required when jobStoreBackend=redis")
	}
	if strings.Contains(trimmed, "://") || strings.Contains(trimmed, "@") {
		return errors.New("redisAddr must be host:port without URL scheme or embedded credentials")
	}
	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		return fmt.Errorf("invalid redisAddr %q (expected host:port): %w", trimmed, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("invalid redisAddr %q: host is empty", trimmed)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid redisAddr %q: port must be between 1 and 65535", trimmed)
	}
	return nil
}

// ValidateRedisKeyPrefix validates the Redis key prefix charset and length.
func ValidateRedisKeyPrefix(prefix string) error {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return nil
	}
	if !validRedisKeyPrefixRe.MatchString(trimmed) {
		return fmt.Errorf("invalid redisKeyPrefix %q (allowed: 1..64 chars of [a-zA-Z0-9:_-])", trimmed)
	}
	return nil
}

// RedisStore is an atomic, owner-isolated, TTL-backed JobStore implementation for HA Grafana.
// All multi-key state transitions run inside Redis Lua scripts using a hash-tagged prefix
// ({<keyPrefix>}) so standalone, Sentinel, and Redis Cluster deployments all execute atomically.
type RedisStore struct {
	client     redis.UniversalClient
	ownsClient bool
	prefix     string // hash-tagged prefix, e.g. "{llmstudio:jobs}"
}

// NewRedisStore creates a Redis-backed JobStore from cfg.
func NewRedisStore(cfg RedisConfig) (*RedisStore, error) {
	if err := ValidateRedisKeyPrefix(cfg.KeyPrefix); err != nil {
		return nil, err
	}
	keyPrefix := strings.TrimSpace(cfg.KeyPrefix)
	if keyPrefix == "" {
		keyPrefix = DefaultRedisKeyPrefix
	}
	tagPrefix := "{" + keyPrefix + "}"

	if cfg.Client != nil {
		return &RedisStore{
			client:     cfg.Client,
			ownsClient: false,
			prefix:     tagPrefix,
		}, nil
	}

	if err := ValidateRedisAddr(cfg.Addr); err != nil {
		return nil, err
	}
	if cfg.DB < 0 || cfg.DB > 15 {
		return nil, fmt.Errorf("redisDb must be between 0 and 15, got %d", cfg.DB)
	}

	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = redisDefaultTimeout
	}
	readTimeout := cfg.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = redisDefaultTimeout
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = redisDefaultTimeout
	}

	opts := &redis.Options{
		Addr:         strings.TrimSpace(cfg.Addr),
		Username:     strings.TrimSpace(cfg.Username),
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  dialTimeout,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	}
	if cfg.TLS {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	client := redis.NewClient(opts)
	return &RedisStore{
		client:     client,
		ownsClient: true,
		prefix:     tagPrefix,
	}, nil
}

func (s *RedisStore) jobKey(jobID string) string {
	return s.prefix + ":job:" + jobID
}

func (s *RedisStore) globalIdxKey() string {
	return s.prefix + ":idx:global"
}

func (s *RedisStore) userIdxKey(owner Owner) string {
	return s.prefix + ":idx:user:" + ownerKey(owner)
}

func (s *RedisStore) globalSlotsKey() string {
	return s.prefix + ":slots:global"
}

func (s *RedisStore) userSlotsKey(owner Owner) string {
	return s.prefix + ":slots:user:" + ownerKey(owner)
}

func (s *RedisStore) CreateGenerating(ctx context.Context, params CreateGeneratingParams) (OpLease, error) {
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

	scopeJSON, err := marshalOptionalJSON(params.Scope)
	if err != nil {
		return OpLease{}, fmt.Errorf("marshal scope: %w", err)
	}

	nowMs := now.UnixMilli()
	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)
	oKey := ownerKey(params.Owner)

	res, err := redisScriptCreateGenerating.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.globalIdxKey(),
			s.userIdxKey(params.Owner),
			s.globalSlotsKey(),
			s.userSlotsKey(params.Owner),
		},
		s.prefix,
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
		strconv.FormatInt(nowMs, 10),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.FormatInt(leaseTTLMs, 10),
		strconv.Itoa(limits.MaxJobs),
		strconv.Itoa(limits.MaxJobsPerUser),
		strconv.Itoa(limits.MaxConcurrentJobs),
		strconv.Itoa(limits.MaxConcurrentJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return OpLease{}, wrapRedisErr("CreateGenerating", err)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return OpLease{}, fmt.Errorf("redis CreateGenerating: unexpected reply %T", res)
	}
	tag := asString(arr[0])
	if tag == "ERR" {
		return OpLease{}, mapRedisScriptError(arr[1:], "", "")
	}
	token, err := strconv.ParseUint(asString(arr[1]), 10, 64)
	if err != nil {
		return OpLease{}, fmt.Errorf("redis CreateGenerating: invalid opToken: %w", err)
	}
	return OpLease{OpToken: token, SlotID: slotID}, nil
}

func (s *RedisStore) CreateImported(ctx context.Context, params CreateImportedParams) (JobSnapshot, error) {
	if err := validateOwner(params.Owner); err != nil {
		return JobSnapshot{}, err
	}
	limits := params.Limits.normalized()
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}

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

	nowMs := now.UnixMilli()
	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	oKey := ownerKey(params.Owner)

	res, err := redisScriptCreateImported.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.globalIdxKey(),
			s.userIdxKey(params.Owner),
		},
		s.prefix,
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
		strconv.FormatInt(nowMs, 10),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.Itoa(limits.MaxJobs),
		strconv.Itoa(limits.MaxJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("CreateImported", err)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return JobSnapshot{}, fmt.Errorf("redis CreateImported: unexpected reply %T", res)
	}
	if asString(arr[0]) == "ERR" {
		return JobSnapshot{}, mapRedisScriptError(arr[1:], "", "")
	}
	fields, err := decodeHashResult(arr[1])
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("redis CreateImported decode: %w", err)
	}
	return snapshotFromHash(fields)
}

func (s *RedisStore) Get(ctx context.Context, owner Owner, jobID string, now time.Time) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	fields, err := s.getOwnedHash(ctx, owner, jobID, now)
	if err != nil {
		return JobSnapshot{}, err
	}
	return snapshotFromHash(fields)
}

func (s *RedisStore) GetRepairCandidate(ctx context.Context, owner Owner, jobID string, now time.Time) (RepairCandidate, error) {
	if err := validateOwner(owner); err != nil {
		return RepairCandidate{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	fields, err := s.getOwnedHash(ctx, owner, jobID, now)
	if err != nil {
		return RepairCandidate{}, err
	}
	var docPtr *ir.DashboardIR
	if raw := fields["current_ir_json"]; raw != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return RepairCandidate{}, fmt.Errorf("unmarshal currentIR: %w", err)
		}
		if rev, err := strconv.Atoi(fields["current_revision"]); err == nil && rev > 0 {
			doc.Revision = rev
		}
		docPtr = &doc
	}
	used, _ := strconv.Atoi(fields["repair_attempts_used"])
	maxRep, _ := strconv.Atoi(fields["max_repair_attempts"])
	untilMs, _ := strconv.ParseInt(fields["active_op_until_ms"], 10, 64)
	qKind := query.Kind(fields["query_kind"])
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return RepairCandidate{
		CurrentIR:          docPtr,
		RepairAttemptsUsed: used,
		MaxRepairAttempts:  maxRep,
		HasActiveOp:        untilMs > now.UnixMilli(),
		QueryKind:          qKind,
	}, nil
}

func (s *RedisStore) getOwnedHash(ctx context.Context, owner Owner, jobID string, now time.Time) (map[string]string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	res, err := redisScriptGetJob.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.globalIdxKey(),
			s.userIdxKey(owner),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		strconv.FormatInt(now.UnixMilli(), 10),
	).Result()
	if err != nil {
		return nil, wrapRedisErr("Get", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return nil, fmt.Errorf("redis Get: unexpected reply %T", res)
	}
	if asString(arr[0]) == "ERR" {
		return nil, mapRedisScriptError(arr[1:], "", "")
	}
	return decodeHashResult(arr[1])
}

func (s *RedisStore) BeginPatch(ctx context.Context, params BeginPatchParams) (BeginOpResult, error) {
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

	nowMs := now.UnixMilli()
	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)

	res, err := redisScriptBeginPatch.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.globalIdxKey(),
			s.userIdxKey(params.Owner),
			s.globalSlotsKey(),
			s.userSlotsKey(params.Owner),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		strconv.Itoa(params.BaseRevision),
		params.TargetPanelID,
		strings.Join(params.CatalogGauges, "\n"),
		slotID,
		strconv.FormatInt(nowMs, 10),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.FormatInt(leaseTTLMs, 10),
		strconv.Itoa(limits.MaxConcurrentJobs),
		strconv.Itoa(limits.MaxConcurrentJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
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
	limits := params.Limits.normalized()
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
	slotID := params.SlotID
	if slotID == "" {
		slotID = newSlotID()
	}

	nowMs := now.UnixMilli()
	jobTTLMs := maxInt64(limits.JobTTL.Milliseconds(), 1)
	leaseTTLMs := maxInt64(limits.OpLeaseTTL.Milliseconds(), 1)

	res, err := redisScriptBeginRepair.Run(ctx, s.client,
		[]string{
			s.jobKey(params.JobID),
			s.globalIdxKey(),
			s.userIdxKey(params.Owner),
			s.globalSlotsKey(),
			s.userSlotsKey(params.Owner),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		strconv.Itoa(params.BaseRevision),
		strings.Join(params.CatalogGauges, "\n"),
		slotID,
		strconv.FormatInt(nowMs, 10),
		strconv.FormatInt(jobTTLMs, 10),
		strconv.FormatInt(leaseTTLMs, 10),
		strconv.Itoa(limits.MaxConcurrentJobs),
		strconv.Itoa(limits.MaxConcurrentJobsPerUser),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
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
	if now.IsZero() {
		now = time.Now()
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptSetStatus.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.globalIdxKey(),
			s.userIdxKey(owner),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		strconv.FormatUint(opToken, 10),
		string(status),
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return wrapRedisErr("SetStatus", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 1 {
		return fmt.Errorf("redis SetStatus: unexpected reply %T", res)
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
	now := params.Now
	if now.IsZero() {
		now = time.Now()
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
			s.globalIdxKey(),
			s.userIdxKey(params.Owner),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
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
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
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
	now := params.Now
	if now.IsZero() {
		now = time.Now()
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
			s.globalIdxKey(),
			s.userIdxKey(params.Owner),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
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
		strconv.FormatInt(now.UnixMilli(), 10),
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
	now := params.Now
	if now.IsZero() {
		now = time.Now()
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
			s.globalIdxKey(),
			s.userIdxKey(params.Owner),
		},
		params.JobID,
		strconv.FormatInt(params.Owner.OrgID, 10),
		params.Owner.UserLogin,
		strconv.FormatUint(params.OpToken, 10),
		params.ErrorMessage,
		hasMeta,
		metaJSON,
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("RecordEditFailure", err)
	}
	return decodeSnapshotStatusReply(res, "RecordEditFailure", errors.New(params.ErrorMessage))
}

func (s *RedisStore) RevertOnCancel(ctx context.Context, owner Owner, jobID string, opToken uint64, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptRevertOnCancel.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.globalIdxKey(),
			s.userIdxKey(owner),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		strconv.FormatUint(opToken, 10),
		"処理がキャンセルされたためドラフトは変更されていません。",
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("RevertOnCancel", err)
	}
	return decodeSnapshotStatusReply(res, "RevertOnCancel", ErrJobCancelled)
}

func (s *RedisStore) Undo(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptUndo.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.globalIdxKey(),
			s.userIdxKey(owner),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		strconv.Itoa(ir.MaxRevision),
		"直前のドラフト状態を取り消して復元しました。",
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("Undo", err)
	}
	return decodeSnapshotStatusReply(res, "Undo", nil)
}

func (s *RedisStore) Cancel(ctx context.Context, owner Owner, jobID string, now time.Time, jobTTL time.Duration) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	ttlMs := maxInt64(normalizeJobTTL(jobTTL).Milliseconds(), 1)

	res, err := redisScriptCancel.Run(ctx, s.client,
		[]string{
			s.jobKey(jobID),
			s.globalIdxKey(),
			s.userIdxKey(owner),
		},
		jobID,
		strconv.FormatInt(owner.OrgID, 10),
		owner.UserLogin,
		"実行中の処理をキャンセルしました。",
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(ttlMs, 10),
		strconv.FormatInt(redisIndexTTLBufferMs, 10),
	).Result()
	if err != nil {
		return JobSnapshot{}, wrapRedisErr("Cancel", err)
	}
	return decodeSnapshotStatusReply(res, "Cancel", nil)
}

func (s *RedisStore) ReleaseSlot(ctx context.Context, owner Owner, slotID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	_, err := redisScriptReleaseSlot.Run(ctx, s.client,
		[]string{
			s.globalSlotsKey(),
			s.userSlotsKey(owner),
		},
		slotID,
		strconv.FormatInt(now.UnixMilli(), 10),
	).Result()
	if err != nil {
		return wrapRedisErr("ReleaseSlot", err)
	}
	return nil
}

func (s *RedisStore) CleanupExpired(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	_, err := redisScriptCleanupExpired.Run(ctx, s.client,
		[]string{
			s.globalIdxKey(),
			s.globalSlotsKey(),
		},
		s.prefix,
		strconv.FormatInt(now.UnixMilli(), 10),
	).Result()
	if err != nil {
		return wrapRedisErr("CleanupExpired", err)
	}
	return nil
}

func (s *RedisStore) Ping(ctx context.Context) error {
	if err := s.client.Ping(ctx).Err(); err != nil {
		return wrapRedisErr("Ping", err)
	}
	return nil
}

func (s *RedisStore) Close() error {
	if s.ownsClient && s.client != nil {
		return s.client.Close()
	}
	return nil
}

// --- Reply decoding helpers ---

func decodeBeginOpReply(res any, slotID, revPrefix string, requestedRev int, targetPanelID, noDraftMsg string) (BeginOpResult, error) {
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return BeginOpResult{}, fmt.Errorf("redis begin op: unexpected reply %T", res)
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
		return BeginOpResult{}, fmt.Errorf("redis begin op: short OK reply len=%d", len(arr))
	}
	opToken, err := strconv.ParseUint(asString(arr[1]), 10, 64)
	if err != nil {
		return BeginOpResult{}, fmt.Errorf("redis begin op: invalid opToken: %w", err)
	}
	var currentIR ir.DashboardIR
	if err := json.Unmarshal([]byte(asString(arr[2])), &currentIR); err != nil {
		return BeginOpResult{}, fmt.Errorf("redis begin op: unmarshal currentIR: %w", err)
	}
	if rev, err := strconv.Atoi(asString(arr[3])); err == nil && rev > 0 {
		currentIR.Revision = rev
	}
	var scope *ir.ScopeFilter
	if rawScope := asString(arr[4]); rawScope != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(rawScope), &sf); err != nil {
			return BeginOpResult{}, fmt.Errorf("redis begin op: unmarshal scope: %w", err)
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
		return JobSnapshot{}, fmt.Errorf("redis %s: unexpected reply %T", opName, res)
	}
	tag := asString(arr[0])
	if tag == "ERR" {
		return JobSnapshot{}, mapRedisScriptError(arr[1:], "", "")
	}
	fields, err := decodeHashResult(arr[1])
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("redis %s decode: %w", opName, err)
	}
	snap, err := snapshotFromHash(fields)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("redis %s snapshot: %w", opName, err)
	}
	switch tag {
	case "OK":
		return snap, okErr
	case "CANCELLED":
		return snap, ErrJobCancelled
	case "CONFLICT":
		return snap, ir.ErrRevisionConflict
	default:
		return JobSnapshot{}, fmt.Errorf("redis %s: unknown status tag %q", opName, tag)
	}
}

func mapRedisScriptError(args []any, _ string, _ string) error {
	if len(args) == 0 {
		return errors.New("unknown redis store error")
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
	case "REVISION_LIMIT":
		return fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	default:
		return fmt.Errorf("redis store error: %s", asString(args[0]))
	}
}

func decodeHashResult(v any) (map[string]string, error) {
	switch raw := v.(type) {
	case []any:
		if len(raw)%2 != 0 {
			return nil, fmt.Errorf("odd HGETALL reply length %d", len(raw))
		}
		out := make(map[string]string, len(raw)/2)
		for i := 0; i < len(raw); i += 2 {
			out[asString(raw[i])] = asString(raw[i+1])
		}
		return out, nil
	case map[any]any:
		out := make(map[string]string, len(raw))
		for k, val := range raw {
			out[asString(k)] = asString(val)
		}
		return out, nil
	case map[string]string:
		return raw, nil
	default:
		return nil, fmt.Errorf("unexpected HGETALL reply type %T", v)
	}
}

func snapshotFromHash(fields map[string]string) (JobSnapshot, error) {
	var scope *ir.ScopeFilter
	if raw := fields["scope_json"]; raw != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(raw), &sf); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal scope: %w", err)
		}
		scope = &sf
	}
	var importSrc *ir.ImportSource
	if raw := fields["import_source_json"]; raw != "" {
		var is ir.ImportSource
		if err := json.Unmarshal([]byte(raw), &is); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal importSource: %w", err)
		}
		importSrc = &is
	}
	var currentIR *ir.DashboardIR
	if raw := fields["current_ir_json"]; raw != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal currentIR: %w", err)
		}
		if rev, err := strconv.Atoi(fields["current_revision"]); err == nil && rev > 0 {
			doc.Revision = rev
		}
		currentIR = &doc
	}
	var diffs []ir.DiffEntry
	if raw := fields["last_diff_json"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &diffs); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal lastDiff: %w", err)
		}
	}
	var warns []string
	if raw := fields["warnings_json"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &warns); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal warnings: %w", err)
		}
	}
	var meta *llm.CompletionMeta
	if raw := fields["llm_meta_json"]; raw != "" {
		var m llm.CompletionMeta
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal llmMeta: %w", err)
		}
		meta = &m
	}
	used, _ := strconv.Atoi(fields["repair_attempts_used"])
	maxRep, _ := strconv.Atoi(fields["max_repair_attempts"])
	histLen, _ := strconv.Atoi(fields["hist_len"])
	updatedMs, _ := strconv.ParseInt(fields["updated_at_ms"], 10, 64)
	qKind := query.Kind(fields["query_kind"])
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:              fields["id"],
		Status:             JobStatus(fields["status"]),
		DatasourceUID:      fields["datasource_uid"],
		QueryKind:          string(qKind),
		Scope:              scope,
		ImportSource:       importSrc,
		CurrentIR:          currentIR,
		LastDiff:           diffs,
		Summary:            fields["summary"],
		Warnings:           warns,
		ErrorMessage:       fields["error_message"],
		RepairAttemptsUsed: used,
		MaxRepairAttempts:  maxRep,
		CanUndo:            histLen > 0,
		LLM:                meta,
		UpdatedAt:          time.UnixMilli(updatedMs).UTC(),
	}, nil
}

func marshalOptionalJSON(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	switch x := v.(type) {
	case *ir.ScopeFilter:
		if x == nil {
			return "", nil
		}
	case *ir.ImportSource:
		if x == nil {
			return "", nil
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func marshalOptionalStringSlice(s []string) (string, error) {
	if len(s) == 0 {
		return "", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func marshalOptionalDiffs(d []ir.DiffEntry) (string, error) {
	if len(d) == 0 {
		return "", nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(b), nil
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

func maxInt64(a, b int64) int64 {
	if a >= b {
		return a
	}
	return b
}

func wrapRedisErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: redis %s: %v", ErrStoreUnavailable, op, err)
}

// --- Atomic Redis Lua scripts ---

const luaCommonHelpers = `
local function safe_pexpire(key, ms)
	if ms < 1 then ms = 1 end
	redis.call('PEXPIRE', key, ms)
end

local function is_active(jobKey, nowMs)
	local untilMs = tonumber(redis.call('HGET', jobKey, 'active_op_until_ms') or '0')
	return untilMs > nowMs
end

local function is_expired(jobKey, nowMs)
	if redis.call('EXISTS', jobKey) == 0 then
		return true
	end
	if is_active(jobKey, nowMs) then
		return false
	end
	local expMs = tonumber(redis.call('HGET', jobKey, 'expires_at_ms') or '0')
	return expMs > 0 and nowMs > expMs
end

local function evict_job(prefix, globalIdx, jobID)
	local jk = prefix .. ':job:' .. jobID
	local ok = redis.call('HGET', jk, 'owner_key')
	redis.call('DEL', jk)
	redis.call('ZREM', globalIdx, jobID)
	if ok and ok ~= '' then
		redis.call('ZREM', prefix .. ':idx:user:' .. ok, jobID)
	end
end

local function reap_index(prefix, globalIdx, idxKey, nowMs)
	local members = redis.call('ZRANGE', idxKey, 0, -1)
	for _, id in ipairs(members) do
		local jk = prefix .. ':job:' .. id
		if is_expired(jk, nowMs) then
			evict_job(prefix, globalIdx, id)
			redis.call('ZREM', idxKey, id)
		end
	end
end

local function find_oldest_inactive(prefix, idxKey, nowMs)
	local members = redis.call('ZRANGE', idxKey, 0, -1)
	for _, id in ipairs(members) do
		local jk = prefix .. ':job:' .. id
		if redis.call('EXISTS', jk) == 1 and not is_active(jk, nowMs) then
			return id
		end
	end
	return nil
end

local function merge_lines(existing, incoming)
	local seen = {}
	local out = {}
	local function add(src)
		if not src or src == '' then return end
		for line in string.gmatch(src, '[^\n]+') do
			if line ~= '' and not seen[line] then
				seen[line] = true
				table.insert(out, line)
			end
		end
	end
	add(existing)
	add(incoming)
	table.sort(out)
	return table.concat(out, '\n')
end

local function contains_line(haystack, needle)
	if not haystack or haystack == '' or not needle or needle == '' then
		return false
	end
	return string.find('\n' .. haystack .. '\n', '\n' .. needle .. '\n', 1, true) ~= nil
end
`

var redisScriptCreateGenerating = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]
local globalSlots = KEYS[4]
local userSlots = KEYS[5]

local prefix = ARGV[1]
local jobID = ARGV[2]
local orgID = ARGV[3]
local userLogin = ARGV[4]
local oKey = ARGV[5]
local dsUID = ARGV[6]
local qKind = ARGV[7]
local scopeJSON = ARGV[8]
local maxRepairs = ARGV[9]
local knownGauges = ARGV[10]
local slotID = ARGV[11]
local nowMs = tonumber(ARGV[12])
local jobTTLMs = tonumber(ARGV[13])
local leaseTTLMs = tonumber(ARGV[14])
local maxJobs = tonumber(ARGV[15])
local maxJobsPerUser = tonumber(ARGV[16])
local maxConcurrent = tonumber(ARGV[17])
local maxConcurrentPerUser = tonumber(ARGV[18])
local idxBufMs = tonumber(ARGV[19])

-- 1. Reap expired concurrency slot leases and check limits.
redis.call('ZREMRANGEBYSCORE', globalSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', globalSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

-- 2. Reap expired jobs from user and global indices.
reap_index(prefix, globalIdx, userIdx, nowMs)
reap_index(prefix, globalIdx, globalIdx, nowMs)

-- 3. Enforce per-user retained job cap (evict caller's oldest inactive job).
if redis.call('ZCARD', userIdx) >= maxJobsPerUser then
	local victim = find_oldest_inactive(prefix, userIdx, nowMs)
	if not victim then
		return {'ERR', 'CONCURRENCY_LIMITED'}
	end
	evict_job(prefix, globalIdx, victim)
	redis.call('ZREM', userIdx, victim)
end

-- 4. Enforce global job cap (evict caller's oldest inactive -> any oldest inactive).
if redis.call('ZCARD', globalIdx) >= maxJobs then
	local victim = find_oldest_inactive(prefix, userIdx, nowMs)
	if not victim then
		victim = find_oldest_inactive(prefix, globalIdx, nowMs)
	end
	if not victim then
		return {'ERR', 'CONCURRENCY_LIMITED'}
	end
	evict_job(prefix, globalIdx, victim)
end

local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'id', jobID,
	'org_id', orgID,
	'user_login', userLogin,
	'owner_key', oKey,
	'datasource_uid', dsUID,
	'query_kind', qKind,
	'scope_json', scopeJSON,
	'import_source_json', '',
	'status', 'generating',
	'current_ir_json', '',
	'current_revision', '0',
	'panel_ids', '',
	'hist_len', '0',
	'last_diff_json', '',
	'summary', '',
	'warnings_json', '',
	'error_message', '',
	'repair_attempts_used', '0',
	'max_repair_attempts', maxRepairs,
	'llm_meta_json', '',
	'known_gauges', merge_lines('', knownGauges),
	'created_at_ms', tostring(nowMs),
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs),
	'active_op_token', '1',
	'active_op_until_ms', tostring(leaseUntilMs),
	'active_slot_id', slotID
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', globalSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
safe_pexpire(globalSlots, leaseTTLMs + idxBufMs)
safe_pexpire(userSlots, leaseTTLMs + idxBufMs)

return {'OK', '1'}
`)

var redisScriptCreateImported = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local prefix = ARGV[1]
local jobID = ARGV[2]
local orgID = ARGV[3]
local userLogin = ARGV[4]
local oKey = ARGV[5]
local dsUID = ARGV[6]
local qKind = ARGV[7]
local scopeJSON = ARGV[8]
local importSrcJSON = ARGV[9]
local draftJSON = ARGV[10]
local draftRev = ARGV[11]
local panelIDs = ARGV[12]
local summary = ARGV[13]
local warnsJSON = ARGV[14]
local maxRepairs = ARGV[15]
local knownGauges = ARGV[16]
local nowMs = tonumber(ARGV[17])
local jobTTLMs = tonumber(ARGV[18])
local maxJobs = tonumber(ARGV[19])
local maxJobsPerUser = tonumber(ARGV[20])
local idxBufMs = tonumber(ARGV[21])

reap_index(prefix, globalIdx, userIdx, nowMs)
reap_index(prefix, globalIdx, globalIdx, nowMs)

if redis.call('ZCARD', userIdx) >= maxJobsPerUser then
	local victim = find_oldest_inactive(prefix, userIdx, nowMs)
	if not victim then
		return {'ERR', 'CONCURRENCY_LIMITED'}
	end
	evict_job(prefix, globalIdx, victim)
	redis.call('ZREM', userIdx, victim)
end

-- Import MUST NEVER evict another user's non-expired job (prevents cross-user eviction DoS).
if redis.call('ZCARD', globalIdx) >= maxJobs then
	local victim = find_oldest_inactive(prefix, userIdx, nowMs)
	if not victim then
		return {'ERR', 'CONCURRENCY_LIMITED'}
	end
	evict_job(prefix, globalIdx, victim)
	redis.call('ZREM', userIdx, victim)
end

local expiresAtMs = nowMs + jobTTLMs

redis.call('HSET', jobKey,
	'id', jobID,
	'org_id', orgID,
	'user_login', userLogin,
	'owner_key', oKey,
	'datasource_uid', dsUID,
	'query_kind', qKind,
	'scope_json', scopeJSON,
	'import_source_json', importSrcJSON,
	'status', 'awaiting_preview',
	'current_ir_json', draftJSON,
	'current_revision', draftRev,
	'panel_ids', panelIDs,
	'hist_len', '0',
	'last_diff_json', '',
	'summary', summary,
	'warnings_json', warnsJSON,
	'error_message', '',
	'repair_attempts_used', '0',
	'max_repair_attempts', maxRepairs,
	'llm_meta_json', '',
	'known_gauges', merge_lines('', knownGauges),
	'created_at_ms', tostring(nowMs),
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs),
	'active_op_token', '1',
	'active_op_until_ms', '0',
	'active_slot_id', ''
)
safe_pexpire(jobKey, jobTTLMs)

redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', redis.call('HGETALL', jobKey)}
`)

var redisScriptGetJob = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local nowMs = tonumber(ARGV[4])

if redis.call('EXISTS', jobKey) == 0 then
	redis.call('ZREM', globalIdx, jobID)
	redis.call('ZREM', userIdx, jobID)
	return {'ERR', 'NOT_FOUND'}
end

if is_expired(jobKey, nowMs) then
	redis.call('DEL', jobKey)
	redis.call('ZREM', globalIdx, jobID)
	redis.call('ZREM', userIdx, jobID)
	return {'ERR', 'NOT_FOUND'}
end

local storedOrg = redis.call('HGET', jobKey, 'org_id')
local storedUser = redis.call('HGET', jobKey, 'user_login')
if storedOrg ~= orgID or storedUser ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end

return {'OK', redis.call('HGETALL', jobKey)}
`)

var redisScriptBeginPatch = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]
local globalSlots = KEYS[4]
local userSlots = KEYS[5]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local baseRev = tonumber(ARGV[4])
local targetPanelID = ARGV[5]
local catalogGauges = ARGV[6]
local slotID = ARGV[7]
local nowMs = tonumber(ARGV[8])
local jobTTLMs = tonumber(ARGV[9])
local leaseTTLMs = tonumber(ARGV[10])
local maxConcurrent = tonumber(ARGV[11])
local maxConcurrentPerUser = tonumber(ARGV[12])
local idxBufMs = tonumber(ARGV[13])

if redis.call('EXISTS', jobKey) == 0 or is_expired(jobKey, nowMs) then
	redis.call('DEL', jobKey)
	redis.call('ZREM', globalIdx, jobID)
	redis.call('ZREM', userIdx, jobID)
	return {'ERR', 'NOT_FOUND'}
end

if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end

local currentIR = redis.call('HGET', jobKey, 'current_ir_json') or ''
if currentIR == '' then
	return {'ERR', 'NO_DRAFT'}
end

local currentRev = tonumber(redis.call('HGET', jobKey, 'current_revision') or '0')
if currentRev ~= baseRev then
	return {'ERR', 'REVISION_CONFLICT', tostring(currentRev)}
end

if targetPanelID ~= '' then
	local panelIDs = redis.call('HGET', jobKey, 'panel_ids') or ''
	if not contains_line(panelIDs, targetPanelID) then
		return {'ERR', 'PANEL_NOT_FOUND'}
	end
end

if is_active(jobKey, nowMs) then
	return {'ERR', 'OP_IN_PROGRESS'}
end

redis.call('ZREMRANGEBYSCORE', globalSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', globalSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local existingGauges = redis.call('HGET', jobKey, 'known_gauges') or ''
local mergedGauges = merge_lines(existingGauges, catalogGauges)
local nextToken = tonumber(redis.call('HGET', jobKey, 'active_op_token') or '0') + 1
local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'known_gauges', mergedGauges,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', tostring(leaseUntilMs),
	'active_slot_id', slotID,
	'status', 'generating',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', globalSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
safe_pexpire(globalSlots, leaseTTLMs + idxBufMs)
safe_pexpire(userSlots, leaseTTLMs + idxBufMs)

local scopeJSON = redis.call('HGET', jobKey, 'scope_json') or ''
local qKind = redis.call('HGET', jobKey, 'query_kind') or ''
return {'OK', tostring(nextToken), currentIR, tostring(currentRev), scopeJSON, qKind, mergedGauges}
`)

var redisScriptBeginRepair = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]
local globalSlots = KEYS[4]
local userSlots = KEYS[5]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local baseRev = tonumber(ARGV[4])
local catalogGauges = ARGV[5]
local slotID = ARGV[6]
local nowMs = tonumber(ARGV[7])
local jobTTLMs = tonumber(ARGV[8])
local leaseTTLMs = tonumber(ARGV[9])
local maxConcurrent = tonumber(ARGV[10])
local maxConcurrentPerUser = tonumber(ARGV[11])
local idxBufMs = tonumber(ARGV[12])

if redis.call('EXISTS', jobKey) == 0 or is_expired(jobKey, nowMs) then
	redis.call('DEL', jobKey)
	redis.call('ZREM', globalIdx, jobID)
	redis.call('ZREM', userIdx, jobID)
	return {'ERR', 'NOT_FOUND'}
end

if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end

local currentIR = redis.call('HGET', jobKey, 'current_ir_json') or ''
if currentIR == '' then
	return {'ERR', 'NO_DRAFT'}
end

local currentRev = tonumber(redis.call('HGET', jobKey, 'current_revision') or '0')
if currentRev ~= baseRev then
	return {'ERR', 'REVISION_CONFLICT', tostring(currentRev)}
end

if is_active(jobKey, nowMs) then
	return {'ERR', 'OP_IN_PROGRESS'}
end

local used = tonumber(redis.call('HGET', jobKey, 'repair_attempts_used') or '0')
local maxRepairs = tonumber(redis.call('HGET', jobKey, 'max_repair_attempts') or '0')
if used >= maxRepairs then
	return {'ERR', 'BUDGET_EXHAUSTED', tostring(used), tostring(maxRepairs)}
end

redis.call('ZREMRANGEBYSCORE', globalSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', globalSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local existingGauges = redis.call('HGET', jobKey, 'known_gauges') or ''
local mergedGauges = merge_lines(existingGauges, catalogGauges)
local nextToken = tonumber(redis.call('HGET', jobKey, 'active_op_token') or '0') + 1
local nextUsed = used + 1
local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'known_gauges', mergedGauges,
	'repair_attempts_used', tostring(nextUsed),
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', tostring(leaseUntilMs),
	'active_slot_id', slotID,
	'status', 'repairing',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', globalSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
safe_pexpire(globalSlots, leaseTTLMs + idxBufMs)
safe_pexpire(userSlots, leaseTTLMs + idxBufMs)

local scopeJSON = redis.call('HGET', jobKey, 'scope_json') or ''
local qKind = redis.call('HGET', jobKey, 'query_kind') or ''
return {'OK', tostring(nextToken), currentIR, tostring(currentRev), scopeJSON, qKind, mergedGauges}
`)

var redisScriptSetStatus = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local opToken = ARGV[4]
local status = ARGV[5]
local nowMs = tonumber(ARGV[6])
local jobTTLMs = tonumber(ARGV[7])
local idxBufMs = tonumber(ARGV[8])

if redis.call('EXISTS', jobKey) == 0 then
	return {'ERR', 'NOT_FOUND'}
end
if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end
if redis.call('HGET', jobKey, 'active_op_token') == opToken then
	local untilMs = tonumber(redis.call('HGET', jobKey, 'active_op_until_ms') or '0')
	local remLeaseMs = math.max(0, untilMs - nowMs)
	local keyTTLMs = math.max(jobTTLMs, remLeaseMs)
	redis.call('HSET', jobKey,
		'status', status,
		'updated_at_ms', tostring(nowMs),
		'expires_at_ms', tostring(nowMs + jobTTLMs)
	)
	safe_pexpire(jobKey, keyTTLMs)
	redis.call('ZADD', globalIdx, nowMs, jobID)
	redis.call('ZADD', userIdx, nowMs, jobID)
	safe_pexpire(userIdx, keyTTLMs + idxBufMs)
end
return {'OK'}
`)

var redisScriptFinishJob = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local opToken = ARGV[4]
local status = ARGV[5]
local hasDoc = ARGV[6]
local docJSON = ARGV[7]
local docRev = ARGV[8]
local panelIDs = ARGV[9]
local diffsJSON = ARGV[10]
local summary = ARGV[11]
local warnsJSON = ARGV[12]
local errMsg = ARGV[13]
local hasMeta = ARGV[14]
local metaJSON = ARGV[15]
local nowMs = tonumber(ARGV[16])
local jobTTLMs = tonumber(ARGV[17])
local idxBufMs = tonumber(ARGV[18])

if redis.call('EXISTS', jobKey) == 0 then
	return {'ERR', 'NOT_FOUND'}
end
if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end
if redis.call('HGET', jobKey, 'active_op_token') ~= opToken then
	return {'CANCELLED', redis.call('HGETALL', jobKey)}
end

redis.call('HSET', jobKey,
	'active_op_until_ms', '0',
	'active_slot_id', '',
	'status', status,
	'last_diff_json', diffsJSON,
	'summary', summary,
	'warnings_json', warnsJSON,
	'error_message', errMsg,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(nowMs + jobTTLMs)
)
if hasDoc == '1' then
	redis.call('HSET', jobKey,
		'current_ir_json', docJSON,
		'current_revision', docRev,
		'panel_ids', panelIDs
	)
end
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', redis.call('HGETALL', jobKey)}
`)

var redisScriptCommitRevision = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local opToken = ARGV[4]
local expectedRev = tonumber(ARGV[5])
local nextIRJSON = ARGV[6]
local nextRev = ARGV[7]
local nextPanelIDs = ARGV[8]
local diffsJSON = ARGV[9]
local summary = ARGV[10]
local warnsJSON = ARGV[11]
local hasMeta = ARGV[12]
local metaJSON = ARGV[13]
local maxHist = tonumber(ARGV[14])
local nowMs = tonumber(ARGV[15])
local jobTTLMs = tonumber(ARGV[16])
local idxBufMs = tonumber(ARGV[17])

if redis.call('EXISTS', jobKey) == 0 then
	return {'ERR', 'NOT_FOUND'}
end
if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end
if redis.call('HGET', jobKey, 'active_op_token') ~= opToken then
	return {'CANCELLED', redis.call('HGETALL', jobKey)}
end

local prevIR = redis.call('HGET', jobKey, 'current_ir_json') or ''
local curRev = tonumber(redis.call('HGET', jobKey, 'current_revision') or '0')
if prevIR == '' or curRev ~= expectedRev then
	return {'CONFLICT', redis.call('HGETALL', jobKey)}
end

local prevPanels = redis.call('HGET', jobKey, 'panel_ids') or ''
local histLen = tonumber(redis.call('HGET', jobKey, 'hist_len') or '0')
redis.call('HSET', jobKey,
	'hist_' .. tostring(histLen), prevIR,
	'hist_rev_' .. tostring(histLen), tostring(curRev),
	'hist_panels_' .. tostring(histLen), prevPanels
)
histLen = histLen + 1
if histLen > maxHist then
	for i = 0, maxHist - 1 do
		local src = tostring(i + 1)
		local dst = tostring(i)
		redis.call('HSET', jobKey,
			'hist_' .. dst, redis.call('HGET', jobKey, 'hist_' .. src) or '',
			'hist_rev_' .. dst, redis.call('HGET', jobKey, 'hist_rev_' .. src) or '0',
			'hist_panels_' .. dst, redis.call('HGET', jobKey, 'hist_panels_' .. src) or ''
		)
	end
	local drop = tostring(maxHist)
	redis.call('HDEL', jobKey, 'hist_' .. drop, 'hist_rev_' .. drop, 'hist_panels_' .. drop)
	histLen = maxHist
end

redis.call('HSET', jobKey,
	'hist_len', tostring(histLen),
	'current_ir_json', nextIRJSON,
	'current_revision', nextRev,
	'panel_ids', nextPanelIDs,
	'last_diff_json', diffsJSON,
	'summary', summary,
	'warnings_json', warnsJSON,
	'error_message', '',
	'status', 'awaiting_preview',
	'active_op_until_ms', '0',
	'active_slot_id', '',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(nowMs + jobTTLMs)
)
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', redis.call('HGETALL', jobKey)}
`)

var redisScriptRecordEditFailure = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local opToken = ARGV[4]
local errMsg = ARGV[5]
local hasMeta = ARGV[6]
local metaJSON = ARGV[7]
local nowMs = tonumber(ARGV[8])
local jobTTLMs = tonumber(ARGV[9])
local idxBufMs = tonumber(ARGV[10])

if redis.call('EXISTS', jobKey) == 0 then
	return {'ERR', 'NOT_FOUND'}
end
if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end
if redis.call('HGET', jobKey, 'active_op_token') ~= opToken then
	return {'CANCELLED', redis.call('HGETALL', jobKey)}
end

redis.call('HSET', jobKey,
	'active_op_until_ms', '0',
	'active_slot_id', '',
	'status', 'failed',
	'error_message', errMsg,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(nowMs + jobTTLMs)
)
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', redis.call('HGETALL', jobKey)}
`)

var redisScriptRevertOnCancel = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local opToken = ARGV[4]
local cancelSummary = ARGV[5]
local nowMs = tonumber(ARGV[6])
local jobTTLMs = tonumber(ARGV[7])
local idxBufMs = tonumber(ARGV[8])

if redis.call('EXISTS', jobKey) == 0 then
	return {'ERR', 'NOT_FOUND'}
end
if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end

if redis.call('HGET', jobKey, 'active_op_token') == opToken then
	local currentIR = redis.call('HGET', jobKey, 'current_ir_json') or ''
	local nextStatus = 'cancelled'
	if currentIR ~= '' then
		nextStatus = 'awaiting_preview'
	end
	redis.call('HSET', jobKey,
		'active_op_until_ms', '0',
		'active_slot_id', '',
		'status', nextStatus,
		'summary', cancelSummary,
		'updated_at_ms', tostring(nowMs),
		'expires_at_ms', tostring(nowMs + jobTTLMs)
	)
	safe_pexpire(jobKey, jobTTLMs)
	redis.call('ZADD', globalIdx, nowMs, jobID)
	redis.call('ZADD', userIdx, nowMs, jobID)
	safe_pexpire(userIdx, jobTTLMs + idxBufMs)
end

return {'CANCELLED', redis.call('HGETALL', jobKey)}
`)

var redisScriptUndo = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local maxRev = tonumber(ARGV[4])
local undoSummary = ARGV[5]
local nowMs = tonumber(ARGV[6])
local jobTTLMs = tonumber(ARGV[7])
local idxBufMs = tonumber(ARGV[8])

if redis.call('EXISTS', jobKey) == 0 or is_expired(jobKey, nowMs) then
	redis.call('DEL', jobKey)
	redis.call('ZREM', globalIdx, jobID)
	redis.call('ZREM', userIdx, jobID)
	return {'ERR', 'NOT_FOUND'}
end
if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end

local histLen = tonumber(redis.call('HGET', jobKey, 'hist_len') or '0')
local currentIR = redis.call('HGET', jobKey, 'current_ir_json') or ''
if histLen <= 0 or currentIR == '' then
	return {'ERR', 'NO_UNDO_HISTORY'}
end

local oldRev = tonumber(redis.call('HGET', jobKey, 'current_revision') or '0')
if oldRev >= maxRev then
	return {'ERR', 'REVISION_LIMIT'}
end

local idx = histLen - 1
local idxStr = tostring(idx)
local prevIR = redis.call('HGET', jobKey, 'hist_' .. idxStr) or ''
local prevRev = tonumber(redis.call('HGET', jobKey, 'hist_rev_' .. idxStr) or '0')
local prevPanels = redis.call('HGET', jobKey, 'hist_panels_' .. idxStr) or ''
redis.call('HDEL', jobKey, 'hist_' .. idxStr, 'hist_rev_' .. idxStr, 'hist_panels_' .. idxStr)

local newRev = oldRev + 1
local nextToken = tonumber(redis.call('HGET', jobKey, 'active_op_token') or '0') + 1
local diffJSON = string.format(
	'[{"op":"undo","field":"revision","before":"rev %d","after":"restored from rev %d as rev %d"}]',
	oldRev, prevRev, newRev
)

redis.call('HSET', jobKey,
	'hist_len', tostring(idx),
	'current_ir_json', prevIR,
	'current_revision', tostring(newRev),
	'panel_ids', prevPanels,
	'last_diff_json', diffJSON,
	'summary', undoSummary,
	'status', 'awaiting_preview',
	'error_message', '',
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'active_slot_id', '',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(nowMs + jobTTLMs)
)
safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', redis.call('HGETALL', jobKey)}
`)

var redisScriptCancel = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local globalIdx = KEYS[2]
local userIdx = KEYS[3]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local cancelSummary = ARGV[4]
local nowMs = tonumber(ARGV[5])
local jobTTLMs = tonumber(ARGV[6])
local idxBufMs = tonumber(ARGV[7])

if redis.call('EXISTS', jobKey) == 0 or is_expired(jobKey, nowMs) then
	redis.call('DEL', jobKey)
	redis.call('ZREM', globalIdx, jobID)
	redis.call('ZREM', userIdx, jobID)
	return {'ERR', 'NOT_FOUND'}
end
if redis.call('HGET', jobKey, 'org_id') ~= orgID or redis.call('HGET', jobKey, 'user_login') ~= userLogin then
	return {'ERR', 'FORBIDDEN'}
end

local nextToken = tonumber(redis.call('HGET', jobKey, 'active_op_token') or '0') + 1
local currentIR = redis.call('HGET', jobKey, 'current_ir_json') or ''
local nextStatus = 'cancelled'
if currentIR ~= '' then
	nextStatus = 'awaiting_preview'
end

redis.call('HSET', jobKey,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'active_slot_id', '',
	'status', nextStatus,
	'summary', cancelSummary,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(nowMs + jobTTLMs)
)
safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', globalIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
safe_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', redis.call('HGETALL', jobKey)}
`)

var redisScriptReleaseSlot = redis.NewScript(`
local globalSlots = KEYS[1]
local userSlots = KEYS[2]
local slotID = ARGV[1]
local nowMs = tonumber(ARGV[2])

redis.call('ZREM', globalSlots, slotID)
redis.call('ZREM', userSlots, slotID)
redis.call('ZREMRANGEBYSCORE', globalSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
return 1
`)

var redisScriptCleanupExpired = redis.NewScript(luaCommonHelpers + `
local globalIdx = KEYS[1]
local globalSlots = KEYS[2]
local prefix = ARGV[1]
local nowMs = tonumber(ARGV[2])

redis.call('ZREMRANGEBYSCORE', globalSlots, '-inf', nowMs)
reap_index(prefix, globalIdx, globalIdx, nowMs)
return 1
`)
