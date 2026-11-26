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
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var (
	_ JobStore           = (*RedisStore)(nil)
	_ CancelSubscription = (*RedisStore)(nil)
)

const (
	DefaultRedisKeyPrefix = "llmstudio:jobs"
	redisIndexTTLBufferMs = int64(60_000)
	redisDefaultTimeout   = 5 * time.Second
)

var (
	validRedisKeyPrefixRe = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,64}$`)
	validRedisPortRe      = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	validRedisHostnameRe  = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)
)

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

// ValidateRedisAddr validates that addr is a bare host:port (or [ipv6]:port) with port in 1..65535,
// disallowing URL schemes, embedded credentials, whitespace, control characters, and malformed hosts.
func ValidateRedisAddr(addr string) error {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return errors.New("redisAddr is required when jobStoreBackend=redis")
	}
	if strings.Contains(trimmed, "://") || strings.Contains(trimmed, "@") {
		return errors.New("redisAddr must be host:port without URL scheme or embedded credentials")
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c <= 0x20 || c == 0x7f || c == '/' || c == '?' || c == '#' {
			return fmt.Errorf("invalid redisAddr %q: contains forbidden character", trimmed)
		}
	}
	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		return fmt.Errorf("invalid redisAddr %q (expected host:port): %w", trimmed, err)
	}
	if host == "" {
		return fmt.Errorf("invalid redisAddr %q: host is empty", trimmed)
	}
	if strings.HasPrefix(trimmed, "[") {
		ip := net.ParseIP(host)
		if ip == nil || !strings.Contains(host, ":") {
			return fmt.Errorf("invalid redisAddr %q: bracketed host must be a valid IPv6 literal", trimmed)
		}
	} else if net.ParseIP(host) == nil {
		if !validRedisHostnameRe.MatchString(host) || strings.Contains(host, "..") {
			return fmt.Errorf("invalid redisAddr %q: invalid hostname %q", trimmed, host)
		}
	}
	if !validRedisPortRe.MatchString(portStr) {
		return fmt.Errorf("invalid redisAddr %q: port must be a decimal integer between 1 and 65535", trimmed)
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

// RedisStore is an atomic, org-isolated, owner-isolated, TTL-backed JobStore for HA Grafana.
// All Lua scripts access ONLY keys explicitly declared in KEYS[] under a hash-tagged prefix
// ({<keyPrefix>}), use Redis TIME as the authoritative clock when Now is zero, disable
// non-idempotent auto-retries (MaxRetries=-1), and broadcast Cancel/Undo fencing events via Pub/Sub.
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
		// Disable automatic retries on non-idempotent Lua mutations (BeginRepair, Undo, CommitRevision, etc.)
		// so a lost reply after server-side execution never double-executes state transitions.
		MaxRetries: -1,
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

func (s *RedisStore) orgIdxKey(orgID int64) string {
	return s.prefix + ":idx:org:" + strconv.FormatInt(orgID, 10)
}

func (s *RedisStore) orgMetaKey(orgID int64) string {
	return s.prefix + ":meta:org:" + strconv.FormatInt(orgID, 10)
}

func (s *RedisStore) userIdxKey(owner Owner) string {
	return s.prefix + ":idx:user:" + ownerKey(owner)
}

func (s *RedisStore) userIdxKeyByOwnerKey(oKey string) string {
	return s.prefix + ":idx:user:" + oKey
}

func (s *RedisStore) orgSlotsKey(orgID int64) string {
	return s.prefix + ":slots:org:" + strconv.FormatInt(orgID, 10)
}

func (s *RedisStore) userSlotsKey(owner Owner) string {
	return s.prefix + ":slots:user:" + ownerKey(owner)
}

func (s *RedisStore) orgsSetKey() string {
	return s.prefix + ":orgs"
}

func (s *RedisStore) cancelChannel() string {
	return s.prefix + ":cancel"
}

func redisNowArgMs(now time.Time) string {
	if now.IsZero() {
		return "0"
	}
	return strconv.FormatInt(now.UTC().UnixMilli(), 10)
}

// SubscribeCancels subscribes to Redis Pub/Sub fencing notifications emitted by Cancel and Undo
// so remote replicas cancel in-flight LLM contexts and unwind concurrency slots immediately.
func (s *RedisStore) SubscribeCancels(ctx context.Context, onCancel func(jobID string, fencedToken uint64)) (func(), error) {
	pubsub := s.client.Subscribe(ctx, s.cancelChannel())
	// Wait for SUBSCRIBE confirmation before returning so immediate Cancel calls are never missed.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, wrapRedisErr("SubscribeCancels", err)
	}

	ch := pubsub.Channel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for msg := range ch {
			if msg == nil {
				continue
			}
			sep := strings.LastIndexByte(msg.Payload, ':')
			if sep <= 0 || sep >= len(msg.Payload)-1 {
				continue
			}
			jobID := msg.Payload[:sep]
			token, err := strconv.ParseUint(msg.Payload[sep+1:], 10, 64)
			if err != nil || token == 0 || ValidateJobID(jobID) != nil {
				continue
			}
			onCancel(jobID, token)
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = pubsub.Close()
			wg.Wait()
		})
	}
	return stop, nil
}

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
	case "REVISION_LIMIT":
		return fmt.Errorf("revision limit %d reached", ir.MaxRevision)
	case "DUPLICATE_JOB":
		return errors.New("job id already exists")
	default:
		return wrapRedisErr("script", fmt.Errorf("store error code %s", asString(args[0])))
	}
}

// snapshotFromHMGetReply decodes the fixed 17-element HMGET sequence returned by Lua read_snapshot():
// [0]=id, [1]=datasource_uid, [2]=query_kind, [3]=scope_json, [4]=import_source_json,
// [5]=status, [6]=current_ir_json, [7]=current_revision, [8]=hist_len, [9]=last_diff_json,
// [10]=summary, [11]=warnings_json, [12]=error_message, [13]=repair_attempts_used,
// [14]=max_repair_attempts, [15]=llm_meta_json, [16]=updated_at_ms.
func snapshotFromHMGetReply(v any, opName string) (JobSnapshot, error) {
	vals, ok := v.([]any)
	if !ok || len(vals) < 17 {
		return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unexpected HMGET reply %T", v))
	}
	field := func(idx int) string {
		if vals[idx] == nil {
			return ""
		}
		return asString(vals[idx])
	}

	var scope *ir.ScopeFilter
	if raw := field(3); raw != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(raw), &sf); err != nil {
			return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unmarshal scope: %w", err))
		}
		scope = &sf
	}
	var importSrc *ir.ImportSource
	if raw := field(4); raw != "" {
		var is ir.ImportSource
		if err := json.Unmarshal([]byte(raw), &is); err != nil {
			return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unmarshal importSource: %w", err))
		}
		importSrc = &is
	}
	var currentIR *ir.DashboardIR
	if raw := field(6); raw != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unmarshal currentIR: %w", err))
		}
		if rev, err := strconv.Atoi(field(7)); err == nil && rev > 0 {
			doc.Revision = rev
		}
		currentIR = &doc
	}
	var diffs []ir.DiffEntry
	if raw := field(9); raw != "" {
		if err := json.Unmarshal([]byte(raw), &diffs); err != nil {
			return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unmarshal lastDiff: %w", err))
		}
	}
	var warns []string
	if raw := field(11); raw != "" {
		if err := json.Unmarshal([]byte(raw), &warns); err != nil {
			return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unmarshal warnings: %w", err))
		}
	}
	var meta *llm.CompletionMeta
	if raw := field(15); raw != "" {
		var m llm.CompletionMeta
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return JobSnapshot{}, wrapRedisErr(opName, fmt.Errorf("unmarshal llmMeta: %w", err))
		}
		meta = &m
	}
	histLen, _ := strconv.Atoi(field(8))
	used, _ := strconv.Atoi(field(13))
	maxRep, _ := strconv.Atoi(field(14))
	updatedMs, _ := strconv.ParseInt(field(16), 10, 64)
	qKind := query.Kind(field(2))
	if qKind == "" {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:              field(0),
		Status:             JobStatus(field(5)),
		DatasourceUID:      field(1),
		QueryKind:          string(qKind),
		Scope:              scope,
		ImportSource:       importSrc,
		CurrentIR:          currentIR,
		LastDiff:           diffs,
		Summary:            field(10),
		Warnings:           warns,
		ErrorMessage:       field(12),
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

// --- Atomic Redis Lua scripts (100% declared KEYS[], zero dynamic key access, Redis TIME authoritative) ---

const luaCommonHelpers = `
local function resolve_now_ms(argNowMs)
	if argNowMs and argNowMs > 0 then
		return argNowMs
	end
	local t = redis.call('TIME')
	return (tonumber(t[1]) or 0) * 1000 + math.floor((tonumber(t[2]) or 0) / 1000)
end

local function hget_num(key, field)
	return tonumber(redis.call('HGET', key, field)) or 0
end

local function hget_str(key, field)
	local v = redis.call('HGET', key, field)
	if not v then return '' end
	return v
end

local function safe_pexpire(key, ms)
	if ms < 1 then ms = 1 end
	redis.call('PEXPIRE', key, ms)
end

-- Monotonically extends TTL on shared index/slot keys so shorter-TTL callers never truncate longer-lived keys.
local function extend_pexpire(key, ms)
	if ms < 1 then ms = 1 end
	local cur = redis.call('PTTL', key)
	if cur < ms then
		redis.call('PEXPIRE', key, ms)
	end
end

local function is_active(jobKey, nowMs)
	return hget_num(jobKey, 'active_op_until_ms') > nowMs
end

local function is_expired(jobKey, nowMs)
	if redis.call('EXISTS', jobKey) == 0 then
		return true
	end
	if is_active(jobKey, nowMs) then
		return false
	end
	local expMs = hget_num(jobKey, 'expires_at_ms')
	return expMs > 0 and nowMs > expMs
end

-- Metadata format in orgMeta[jobID]: "<ownerKey>|<expiresAtMs>|<activeOpUntilMs>"
local function encode_meta(oKey, expMs, activeUntilMs)
	return oKey .. '|' .. tostring(expMs) .. '|' .. tostring(activeUntilMs)
end

local function parse_meta(raw)
	if not raw or raw == '' then
		return nil, 0, 0
	end
	local oKey, expStr, actStr = string.match(raw, '^([^|]+)|([^|]+)|([^|]+)$')
	if not oKey then
		return nil, 0, 0
	end
	return oKey, tonumber(expStr) or 0, tonumber(actStr) or 0
end

local function meta_is_active(activeUntilMs, nowMs)
	return activeUntilMs > nowMs
end

local function meta_is_expired(expMs, activeUntilMs, nowMs)
	if meta_is_active(activeUntilMs, nowMs) then
		return false
	end
	return expMs > 0 and nowMs > expMs
end

-- Reaps expired entries using ONLY declared orgIdx, orgMeta, and (optionally) userIdx keys.
-- Returns array of "<jobID>|<ownerKey>" entries for Go-side job hash deletion and cross-user ZREM.
local function reap_org_meta(orgIdx, orgMeta, userIdx, callerOKey, nowMs)
	local evicted = {}
	local members = redis.call('ZRANGE', orgIdx, 0, -1)
	for _, id in ipairs(members) do
		local raw = redis.call('HGET', orgMeta, id)
		local oKey, expMs, actMs = parse_meta(raw)
		if (not oKey) or meta_is_expired(expMs, actMs, nowMs) then
			redis.call('ZREM', orgIdx, id)
			redis.call('HDEL', orgMeta, id)
			if userIdx and oKey and oKey == callerOKey then
				redis.call('ZREM', userIdx, id)
			end
			if oKey then
				table.insert(evicted, id .. '|' .. oKey)
			else
				table.insert(evicted, id)
			end
		end
	end
	-- Reconcile ghost entries in caller's userIdx that are no longer in orgMeta.
	if userIdx then
		local uMembers = redis.call('ZRANGE', userIdx, 0, -1)
		for _, id in ipairs(uMembers) do
			if redis.call('HEXISTS', orgMeta, id) == 0 then
				redis.call('ZREM', userIdx, id)
			end
		end
	end
	return evicted
end

local function read_snapshot(jobKey)
	return redis.call('HMGET', jobKey,
		'id',
		'datasource_uid',
		'query_kind',
		'scope_json',
		'import_source_json',
		'status',
		'current_ir_json',
		'current_revision',
		'hist_len',
		'last_diff_json',
		'summary',
		'warnings_json',
		'error_message',
		'repair_attempts_used',
		'max_repair_attempts',
		'llm_meta_json',
		'updated_at_ms'
	)
end

local function merge_lines_bounded(existing, incoming, maxCount)
	local seen = {}
	local out = {}
	local function add(src)
		if not src or src == '' then return end
		for line in string.gmatch(src, '[^\r\n]+') do
			if line ~= '' and not seen[line] then
				if #out >= maxCount then
					return
				end
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
	if string.find(needle, '\n', 1, true) or string.find(needle, '\r', 1, true) then
		return false
	end
	return string.find('\n' .. haystack .. '\n', '\n' .. needle .. '\n', 1, true) ~= nil
end

-- Verifies job existence, expiration (only owner may delete expired hash), exact (org_id, user_login) match,
-- and non-evicted membership in orgMeta. Returns nil on success, or an ERR reply table.
local function check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
	if redis.call('EXISTS', jobKey) == 0 then
		return {'ERR', 'NOT_FOUND'}
	end
	local storedOrg = hget_str(jobKey, 'org_id')
	local storedUser = hget_str(jobKey, 'user_login')
	local isOwner = (storedOrg == orgID and storedUser == userLogin)
	if is_expired(jobKey, nowMs) then
		if isOwner then
			redis.call('DEL', jobKey)
			redis.call('ZREM', orgIdx, jobID)
			redis.call('ZREM', userIdx, jobID)
			redis.call('HDEL', orgMeta, jobID)
		end
		return {'ERR', 'NOT_FOUND'}
	end
	if not isOwner then
		return {'ERR', 'FORBIDDEN'}
	end
	if redis.call('HEXISTS', orgMeta, jobID) == 0 then
		redis.call('DEL', jobKey)
		redis.call('ZREM', orgIdx, jobID)
		redis.call('ZREM', userIdx, jobID)
		return {'ERR', 'NOT_FOUND'}
	end
	return nil
end
`

var redisScriptCreateGenerating = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgSlots = KEYS[5]
local userSlots = KEYS[6]
local orgsSet = KEYS[7]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local dsUID = ARGV[5]
local qKind = ARGV[6]
local scopeJSON = ARGV[7]
local maxRepairs = ARGV[8]
local knownGauges = ARGV[9]
local slotID = ARGV[10]
local nowMs = resolve_now_ms(tonumber(ARGV[11]) or 0)
local jobTTLMs = tonumber(ARGV[12]) or 1
local leaseTTLMs = tonumber(ARGV[13]) or 1
local maxJobs = tonumber(ARGV[14]) or 1
local maxJobsPerUser = tonumber(ARGV[15]) or 1
local maxConcurrent = tonumber(ARGV[16]) or 1
local maxConcurrentPerUser = tonumber(ARGV[17]) or 1
local idxBufMs = tonumber(ARGV[18]) or 60000
local maxGauges = tonumber(ARGV[19]) or 512

-- 1. Reap expired concurrency slot leases and check per-org & per-user limits.
redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', orgSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

-- 2. Reap expired org entries from orgMeta/orgIdx/userIdx (declared keys only).
local evicted = reap_org_meta(orgIdx, orgMeta, userIdx, oKey, nowMs)

if redis.call('EXISTS', jobKey) == 1 then
	return {'ERR', 'DUPLICATE_JOB'}
end

-- 3. Transactional capacity planning: choose all victims BEFORE deleting anything.
local orgMembers = redis.call('ZRANGE', orgIdx, 0, -1)
local callerInactive = {}
local otherOrgInactive = {}
local userCount = 0
local orgCount = #orgMembers

for _, id in ipairs(orgMembers) do
	local mOKey, _, actMs = parse_meta(redis.call('HGET', orgMeta, id))
	local active = meta_is_active(actMs, nowMs)
	if mOKey == oKey then
		userCount = userCount + 1
		if not active then
			table.insert(callerInactive, {id = id, oKey = mOKey})
		end
	elseif not active then
		table.insert(otherOrgInactive, {id = id, oKey = mOKey or ''})
	end
end

local needUserEvict = 0
if userCount >= maxJobsPerUser then
	needUserEvict = userCount - maxJobsPerUser + 1
end
if needUserEvict > #callerInactive then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local victims = {}
for i = 1, needUserEvict do
	table.insert(victims, callerInactive[i])
end

local remainingCaller = #callerInactive - needUserEvict
local remainingOrgCount = orgCount - needUserEvict
local needOrgEvict = 0
if remainingOrgCount >= maxJobs then
	needOrgEvict = remainingOrgCount - maxJobs + 1
end
if needOrgEvict > remainingCaller + #otherOrgInactive then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local cIdx = needUserEvict + 1
while needOrgEvict > 0 and cIdx <= #callerInactive do
	table.insert(victims, callerInactive[cIdx])
	cIdx = cIdx + 1
	needOrgEvict = needOrgEvict - 1
end
local oIdx = 1
while needOrgEvict > 0 and oIdx <= #otherOrgInactive do
	table.insert(victims, otherOrgInactive[oIdx])
	oIdx = oIdx + 1
	needOrgEvict = needOrgEvict - 1
end

-- Apply planned evictions atomically on declared keys.
for _, v in ipairs(victims) do
	redis.call('ZREM', orgIdx, v.id)
	redis.call('HDEL', orgMeta, v.id)
	if v.oKey == oKey then
		redis.call('ZREM', userIdx, v.id)
	end
	table.insert(evicted, v.id .. '|' .. v.oKey)
end

local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'id', jobID,
	'org_id', orgID,
	'user_login', userLogin,
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
	'known_gauges', merge_lines_bounded('', knownGauges, maxGauges),
	'created_at_ms', tostring(nowMs),
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs),
	'active_op_token', '1',
	'active_op_until_ms', tostring(leaseUntilMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('SADD', orgsSet, orgID)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, leaseUntilMs))
extend_pexpire(orgsSet, keyTTLMs + idxBufMs)
extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
extend_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', orgSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
extend_pexpire(orgSlots, leaseTTLMs + idxBufMs)
extend_pexpire(userSlots, leaseTTLMs + idxBufMs)

return {'OK', '1', evicted}
`)

var redisScriptCreateImported = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgsSet = KEYS[5]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local dsUID = ARGV[5]
local qKind = ARGV[6]
local scopeJSON = ARGV[7]
local importSrcJSON = ARGV[8]
local draftJSON = ARGV[9]
local draftRev = ARGV[10]
local panelIDs = ARGV[11]
local summary = ARGV[12]
local warnsJSON = ARGV[13]
local maxRepairs = ARGV[14]
local knownGauges = ARGV[15]
local nowMs = resolve_now_ms(tonumber(ARGV[16]) or 0)
local jobTTLMs = tonumber(ARGV[17]) or 1
local maxJobs = tonumber(ARGV[18]) or 1
local maxJobsPerUser = tonumber(ARGV[19]) or 1
local idxBufMs = tonumber(ARGV[20]) or 60000
local maxGauges = tonumber(ARGV[21]) or 512

local evicted = reap_org_meta(orgIdx, orgMeta, userIdx, oKey, nowMs)

if redis.call('EXISTS', jobKey) == 1 then
	return {'ERR', 'DUPLICATE_JOB'}
end

-- Import MUST NEVER evict another user's non-expired job (prevents cross-user eviction DoS).
local orgMembers = redis.call('ZRANGE', orgIdx, 0, -1)
local callerInactive = {}
local userCount = 0
local orgCount = #orgMembers

for _, id in ipairs(orgMembers) do
	local mOKey, _, actMs = parse_meta(redis.call('HGET', orgMeta, id))
	if mOKey == oKey then
		userCount = userCount + 1
		if not meta_is_active(actMs, nowMs) then
			table.insert(callerInactive, id)
		end
	end
end

local needUserEvict = 0
if userCount >= maxJobsPerUser then
	needUserEvict = userCount - maxJobsPerUser + 1
end
local needOrgEvict = 0
if orgCount >= maxJobs then
	needOrgEvict = orgCount - maxJobs + 1
end
local needTotal = math.max(needUserEvict, needOrgEvict)
if needTotal > #callerInactive then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

for i = 1, needTotal do
	local victimID = callerInactive[i]
	redis.call('ZREM', orgIdx, victimID)
	redis.call('ZREM', userIdx, victimID)
	redis.call('HDEL', orgMeta, victimID)
	table.insert(evicted, victimID .. '|' .. oKey)
end

local expiresAtMs = nowMs + jobTTLMs

redis.call('HSET', jobKey,
	'id', jobID,
	'org_id', orgID,
	'user_login', userLogin,
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
	'known_gauges', merge_lines_bounded('', knownGauges, maxGauges),
	'created_at_ms', tostring(nowMs),
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs),
	'active_op_token', '0',
	'active_op_until_ms', '0'
)
safe_pexpire(jobKey, jobTTLMs)

redis.call('SADD', orgsSet, orgID)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgsSet, jobTTLMs + idxBufMs)
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey), evicted}
`)

var redisScriptGetJob = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local nowMs = resolve_now_ms(tonumber(ARGV[4]) or 0)

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptGetRepairCandidate = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local nowMs = resolve_now_ms(tonumber(ARGV[4]) or 0)

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local vals = redis.call('HMGET', jobKey,
	'current_ir_json',
	'current_revision',
	'repair_attempts_used',
	'max_repair_attempts',
	'active_op_until_ms',
	'query_kind'
)
return {
	'OK',
	vals[1] or '',
	vals[2] or '0',
	vals[3] or '0',
	vals[4] or '0',
	vals[5] or '0',
	vals[6] or '',
	tostring(nowMs)
}
`)

var redisScriptBeginPatch = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgSlots = KEYS[5]
local userSlots = KEYS[6]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local baseRev = tonumber(ARGV[5]) or 0
local targetPanelID = ARGV[6]
local catalogGauges = ARGV[7]
local slotID = ARGV[8]
local nowMs = resolve_now_ms(tonumber(ARGV[9]) or 0)
local jobTTLMs = tonumber(ARGV[10]) or 1
local leaseTTLMs = tonumber(ARGV[11]) or 1
local maxConcurrent = tonumber(ARGV[12]) or 1
local maxConcurrentPerUser = tonumber(ARGV[13]) or 1
local idxBufMs = tonumber(ARGV[14]) or 60000
local maxGauges = tonumber(ARGV[15]) or 512

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local currentIR = hget_str(jobKey, 'current_ir_json')
if currentIR == '' then
	return {'ERR', 'NO_DRAFT'}
end

local currentRev = hget_num(jobKey, 'current_revision')
if currentRev ~= baseRev then
	return {'ERR', 'REVISION_CONFLICT', tostring(currentRev)}
end

if targetPanelID ~= '' then
	local panelIDs = hget_str(jobKey, 'panel_ids')
	if not contains_line(panelIDs, targetPanelID) then
		return {'ERR', 'PANEL_NOT_FOUND'}
	end
end

if is_active(jobKey, nowMs) then
	return {'ERR', 'OP_IN_PROGRESS'}
end

redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', orgSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local existingGauges = hget_str(jobKey, 'known_gauges')
local mergedGauges = merge_lines_bounded(existingGauges, catalogGauges, maxGauges)
local nextToken = hget_num(jobKey, 'active_op_token') + 1
local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'known_gauges', mergedGauges,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', tostring(leaseUntilMs),
	'status', 'generating',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, leaseUntilMs))
extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
extend_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', orgSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
extend_pexpire(orgSlots, leaseTTLMs + idxBufMs)
extend_pexpire(userSlots, leaseTTLMs + idxBufMs)

local scopeJSON = hget_str(jobKey, 'scope_json')
local qKind = hget_str(jobKey, 'query_kind')
return {'OK', tostring(nextToken), currentIR, tostring(currentRev), scopeJSON, qKind, mergedGauges}
`)

var redisScriptBeginRepair = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgSlots = KEYS[5]
local userSlots = KEYS[6]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local baseRev = tonumber(ARGV[5]) or 0
local catalogGauges = ARGV[6]
local slotID = ARGV[7]
local nowMs = resolve_now_ms(tonumber(ARGV[8]) or 0)
local jobTTLMs = tonumber(ARGV[9]) or 1
local leaseTTLMs = tonumber(ARGV[10]) or 1
local maxConcurrent = tonumber(ARGV[11]) or 1
local maxConcurrentPerUser = tonumber(ARGV[12]) or 1
local idxBufMs = tonumber(ARGV[13]) or 60000
local maxGauges = tonumber(ARGV[14]) or 512

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local currentIR = hget_str(jobKey, 'current_ir_json')
if currentIR == '' then
	return {'ERR', 'NO_DRAFT'}
end

local currentRev = hget_num(jobKey, 'current_revision')
if currentRev ~= baseRev then
	return {'ERR', 'REVISION_CONFLICT', tostring(currentRev)}
end

if is_active(jobKey, nowMs) then
	return {'ERR', 'OP_IN_PROGRESS'}
end

local used = hget_num(jobKey, 'repair_attempts_used')
local maxRepairs = hget_num(jobKey, 'max_repair_attempts')
if used >= maxRepairs then
	return {'ERR', 'BUDGET_EXHAUSTED', tostring(used), tostring(maxRepairs)}
end

redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', orgSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local existingGauges = hget_str(jobKey, 'known_gauges')
local mergedGauges = merge_lines_bounded(existingGauges, catalogGauges, maxGauges)
local nextToken = hget_num(jobKey, 'active_op_token') + 1
local nextUsed = used + 1
local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'known_gauges', mergedGauges,
	'repair_attempts_used', tostring(nextUsed),
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', tostring(leaseUntilMs),
	'status', 'repairing',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, leaseUntilMs))
extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
extend_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', orgSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
extend_pexpire(orgSlots, leaseTTLMs + idxBufMs)
extend_pexpire(userSlots, leaseTTLMs + idxBufMs)

local scopeJSON = hget_str(jobKey, 'scope_json')
local qKind = hget_str(jobKey, 'query_kind')
return {'OK', tostring(nextToken), currentIR, tostring(currentRev), scopeJSON, qKind, mergedGauges}
`)

var redisScriptSetStatus = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local status = ARGV[6]
local nowMs = resolve_now_ms(tonumber(ARGV[7]) or 0)
local jobTTLMs = tonumber(ARGV[8]) or 1
local idxBufMs = tonumber(ARGV[9]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
local untilMs = hget_num(jobKey, 'active_op_until_ms')
if opToken > 0 and storedToken == opToken and untilMs > nowMs then
	local remLeaseMs = math.max(0, untilMs - nowMs)
	local keyTTLMs = math.max(jobTTLMs, remLeaseMs)
	local expiresAtMs = nowMs + jobTTLMs
	redis.call('HSET', jobKey,
		'status', status,
		'updated_at_ms', tostring(nowMs),
		'expires_at_ms', tostring(expiresAtMs)
	)
	safe_pexpire(jobKey, keyTTLMs)
	redis.call('ZADD', orgIdx, nowMs, jobID)
	redis.call('ZADD', userIdx, nowMs, jobID)
	redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, untilMs))
	extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
	extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
	extend_pexpire(userIdx, keyTTLMs + idxBufMs)
end
return {'OK'}
`)

var redisScriptFinishJob = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local status = ARGV[6]
local hasDoc = ARGV[7]
local docJSON = ARGV[8]
local docRev = ARGV[9]
local panelIDs = ARGV[10]
local diffsJSON = ARGV[11]
local summary = ARGV[12]
local warnsJSON = ARGV[13]
local errMsg = ARGV[14]
local hasMeta = ARGV[15]
local metaJSON = ARGV[16]
local nowMs = resolve_now_ms(tonumber(ARGV[17]) or 0)
local jobTTLMs = tonumber(ARGV[18]) or 1
local idxBufMs = tonumber(ARGV[19]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken <= 0 or storedToken ~= opToken or not is_active(jobKey, nowMs) then
	return {'CANCELLED', read_snapshot(jobKey)}
end

local expiresAtMs = nowMs + jobTTLMs
local nextToken = storedToken + 1
redis.call('HSET', jobKey,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'status', status,
	'last_diff_json', diffsJSON,
	'summary', summary,
	'warnings_json', warnsJSON,
	'error_message', errMsg,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
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
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptCommitRevision = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local expectedRev = tonumber(ARGV[6]) or 0
local nextIRJSON = ARGV[7]
local nextRev = ARGV[8]
local nextPanelIDs = ARGV[9]
local diffsJSON = ARGV[10]
local summary = ARGV[11]
local warnsJSON = ARGV[12]
local hasMeta = ARGV[13]
local metaJSON = ARGV[14]
local maxHist = tonumber(ARGV[15]) or 10
local nowMs = resolve_now_ms(tonumber(ARGV[16]) or 0)
local jobTTLMs = tonumber(ARGV[17]) or 1
local idxBufMs = tonumber(ARGV[18]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken <= 0 or storedToken ~= opToken or not is_active(jobKey, nowMs) then
	return {'CANCELLED', read_snapshot(jobKey)}
end

local prevIR = hget_str(jobKey, 'current_ir_json')
local curRev = hget_num(jobKey, 'current_revision')
if prevIR == '' or curRev ~= expectedRev then
	redis.call('HSET', jobKey,
		'active_op_token', tostring(storedToken + 1),
		'active_op_until_ms', '0'
	)
	local expMs = hget_num(jobKey, 'expires_at_ms')
	redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expMs, 0))
	return {'CONFLICT', read_snapshot(jobKey)}
end

local prevPanels = hget_str(jobKey, 'panel_ids')
local histLen = hget_num(jobKey, 'hist_len')
redis.call('HSET', jobKey,
	'hist_' .. tostring(histLen), prevIR,
	'hist_rev_' .. tostring(histLen), tostring(curRev),
	'hist_panels_' .. tostring(histLen), prevPanels
)
histLen = histLen + 1
if histLen > maxHist then
	local shift = histLen - maxHist
	for i = 0, maxHist - 1 do
		local src = tostring(i + shift)
		local dst = tostring(i)
		redis.call('HSET', jobKey,
			'hist_' .. dst, hget_str(jobKey, 'hist_' .. src),
			'hist_rev_' .. dst, tostring(hget_num(jobKey, 'hist_rev_' .. src)),
			'hist_panels_' .. dst, hget_str(jobKey, 'hist_panels_' .. src)
		)
	end
	for d = maxHist, histLen - 1 do
		local drop = tostring(d)
		redis.call('HDEL', jobKey, 'hist_' .. drop, 'hist_rev_' .. drop, 'hist_panels_' .. drop)
	end
	histLen = maxHist
end

local expiresAtMs = nowMs + jobTTLMs
local nextToken = storedToken + 1
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
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptRecordEditFailure = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local errMsg = ARGV[6]
local hasMeta = ARGV[7]
local metaJSON = ARGV[8]
local nowMs = resolve_now_ms(tonumber(ARGV[9]) or 0)
local jobTTLMs = tonumber(ARGV[10]) or 1
local idxBufMs = tonumber(ARGV[11]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken <= 0 or storedToken ~= opToken or not is_active(jobKey, nowMs) then
	return {'CANCELLED', read_snapshot(jobKey)}
end

local expiresAtMs = nowMs + jobTTLMs
local nextToken = storedToken + 1
redis.call('HSET', jobKey,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'status', 'failed',
	'error_message', errMsg,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptRevertOnCancel = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local cancelSummary = ARGV[6]
local nowMs = resolve_now_ms(tonumber(ARGV[7]) or 0)
local jobTTLMs = tonumber(ARGV[8]) or 1
local idxBufMs = tonumber(ARGV[9]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken > 0 and storedToken == opToken and is_active(jobKey, nowMs) then
	local currentIR = hget_str(jobKey, 'current_ir_json')
	local nextStatus = 'cancelled'
	if currentIR ~= '' then
		nextStatus = 'awaiting_preview'
	end
	local expiresAtMs = nowMs + jobTTLMs
	local nextToken = storedToken + 1
	redis.call('HSET', jobKey,
		'active_op_token', tostring(nextToken),
		'active_op_until_ms', '0',
		'status', nextStatus,
		'summary', cancelSummary,
		'updated_at_ms', tostring(nowMs),
		'expires_at_ms', tostring(expiresAtMs)
	)
	safe_pexpire(jobKey, jobTTLMs)
	redis.call('ZADD', orgIdx, nowMs, jobID)
	redis.call('ZADD', userIdx, nowMs, jobID)
	redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
	extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
	extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
	extend_pexpire(userIdx, jobTTLMs + idxBufMs)
end

return {'CANCELLED', read_snapshot(jobKey)}
`)

var redisScriptUndo = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local maxRev = tonumber(ARGV[5]) or 1000
local undoSummary = ARGV[6]
local nowMs = resolve_now_ms(tonumber(ARGV[7]) or 0)
local jobTTLMs = tonumber(ARGV[8]) or 1
local idxBufMs = tonumber(ARGV[9]) or 60000
local cancelChannel = ARGV[10]

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local histLen = hget_num(jobKey, 'hist_len')
local currentIR = hget_str(jobKey, 'current_ir_json')
if histLen <= 0 or currentIR == '' then
	return {'ERR', 'NO_UNDO_HISTORY'}
end

local oldRev = hget_num(jobKey, 'current_revision')
if oldRev >= maxRev then
	return {'ERR', 'REVISION_LIMIT'}
end

local idx = histLen - 1
local idxStr = tostring(idx)
local prevIR = hget_str(jobKey, 'hist_' .. idxStr)
local prevRev = hget_num(jobKey, 'hist_rev_' .. idxStr)
local prevPanels = hget_str(jobKey, 'hist_panels_' .. idxStr)
redis.call('HDEL', jobKey, 'hist_' .. idxStr, 'hist_rev_' .. idxStr, 'hist_panels_' .. idxStr)

local newRev = oldRev + 1
local nextToken = hget_num(jobKey, 'active_op_token') + 1
local diffJSON = string.format(
	'[{"op":"undo","field":"revision","before":"rev %d","after":"restored from rev %d as rev %d"}]',
	oldRev, prevRev, newRev
)
local expiresAtMs = nowMs + jobTTLMs

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
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

if cancelChannel and cancelChannel ~= '' then
	redis.call('PUBLISH', cancelChannel, jobID .. ':' .. tostring(nextToken))
end

return {'OK', read_snapshot(jobKey), tostring(nextToken)}
`)

var redisScriptCancel = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local cancelSummary = ARGV[5]
local nowMs = resolve_now_ms(tonumber(ARGV[6]) or 0)
local jobTTLMs = tonumber(ARGV[7]) or 1
local idxBufMs = tonumber(ARGV[8]) or 60000
local cancelChannel = ARGV[9]

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local nextToken = hget_num(jobKey, 'active_op_token') + 1
local currentIR = hget_str(jobKey, 'current_ir_json')
local nextStatus = 'cancelled'
if currentIR ~= '' then
	nextStatus = 'awaiting_preview'
end
local expiresAtMs = nowMs + jobTTLMs

redis.call('HSET', jobKey,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'status', nextStatus,
	'summary', cancelSummary,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

if cancelChannel and cancelChannel ~= '' then
	redis.call('PUBLISH', cancelChannel, jobID .. ':' .. tostring(nextToken))
end

return {'OK', read_snapshot(jobKey), tostring(nextToken)}
`)

var redisScriptReleaseSlot = redis.NewScript(luaCommonHelpers + `
local orgSlots = KEYS[1]
local userSlots = KEYS[2]
local slotID = ARGV[1]
local nowMs = resolve_now_ms(tonumber(ARGV[2]) or 0)

redis.call('ZREM', orgSlots, slotID)
redis.call('ZREM', userSlots, slotID)
redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
return 1
`)

var redisScriptCleanupOrg = redis.NewScript(luaCommonHelpers + `
local orgIdx = KEYS[1]
local orgMeta = KEYS[2]
local orgSlots = KEYS[3]
local nowMs = resolve_now_ms(tonumber(ARGV[1]) or 0)

redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
return reap_org_meta(orgIdx, orgMeta, nil, '', nowMs)
`)
