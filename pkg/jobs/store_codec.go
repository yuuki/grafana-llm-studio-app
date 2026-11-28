package jobs

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

var (
	validHostPortPortRe     = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	validHostPortHostnameRe = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)
)

// validateHostPortAddr validates that addr is a bare host:port (or [ipv6]:port) with port in 1..65535,
// disallowing URL schemes, embedded credentials, whitespace, control characters, bracketed non-IPv6 hosts,
// and malformed hosts.
func validateHostPortAddr(fieldName, backendName, addr string) error {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return fmt.Errorf("%s is required when jobStoreBackend=%s", fieldName, backendName)
	}
	if strings.Contains(trimmed, "://") || strings.Contains(trimmed, "@") {
		return fmt.Errorf("%s must be host:port without URL scheme or embedded credentials", fieldName)
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c <= 0x20 || c == 0x7f || c == '/' || c == '?' || c == '#' {
			return fmt.Errorf("invalid %s %q: contains forbidden character", fieldName, trimmed)
		}
	}
	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		return fmt.Errorf("invalid %s %q (expected host:port): %w", fieldName, trimmed, err)
	}
	if host == "" {
		return fmt.Errorf("invalid %s %q: host is empty", fieldName, trimmed)
	}
	if strings.HasPrefix(trimmed, "[") {
		ip := net.ParseIP(host)
		if ip == nil || !strings.Contains(host, ":") {
			return fmt.Errorf("invalid %s %q: bracketed host must be a valid IPv6 literal", fieldName, trimmed)
		}
	} else if net.ParseIP(host) == nil {
		if !validHostPortHostnameRe.MatchString(host) || strings.Contains(host, "..") {
			return fmt.Errorf("invalid %s %q: invalid hostname %q", fieldName, trimmed, host)
		}
	}
	if !validHostPortPortRe.MatchString(portStr) {
		return fmt.Errorf("invalid %s %q: port must be a decimal integer between 1 and 65535", fieldName, trimmed)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid %s %q: port must be between 1 and 65535", fieldName, trimmed)
	}
	return nil
}

// parseCancelNotification parses "<jobID>:<opToken>" fencing notifications delivered over
// Redis Pub/Sub or PostgreSQL LISTEN/NOTIFY.
func parseCancelNotification(payload string) (jobID string, token uint64, ok bool) {
	sep := strings.LastIndexByte(payload, ':')
	if sep <= 0 || sep >= len(payload)-1 {
		return "", 0, false
	}
	jobID = payload[:sep]
	token, err := strconv.ParseUint(payload[sep+1:], 10, 64)
	if err != nil || token == 0 || ValidateJobID(jobID) != nil {
		return "", 0, false
	}
	return jobID, token, true
}

// serializedJobFields is the backend-agnostic serialized representation of a stored job record
// shared by RedisStore and PostgresStore when decoding into a JobSnapshot.
type serializedJobFields struct {
	JobID              string
	JobKind            string
	Status             string
	DatasourceUID      string
	QueryKind          string
	ScopeJSON          string
	ImportSourceJSON   string
	CurrentIRJSON      string
	CurrentRev         int
	HistoryLen         int
	LastDiffJSON       string
	Summary            string
	WarningsJSON       string
	ErrorMessage       string
	RepairAttemptsUsed int
	MaxRepairAttempts  int
	LLMMetaJSON        string
	ReportJSON         string
	UpdatedAtMs        int64
}

func (f serializedJobFields) decodeSnapshot() (JobSnapshot, error) {
	var scope *ir.ScopeFilter
	if f.ScopeJSON != "" {
		var sf ir.ScopeFilter
		if err := json.Unmarshal([]byte(f.ScopeJSON), &sf); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal scope: %w", err)
		}
		scope = &sf
	}
	var importSrc *ir.ImportSource
	if f.ImportSourceJSON != "" {
		var is ir.ImportSource
		if err := json.Unmarshal([]byte(f.ImportSourceJSON), &is); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal importSource: %w", err)
		}
		importSrc = &is
	}
	var currentIR *ir.DashboardIR
	if f.CurrentIRJSON != "" {
		var doc ir.DashboardIR
		if err := json.Unmarshal([]byte(f.CurrentIRJSON), &doc); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal currentIR: %w", err)
		}
		if f.CurrentRev > 0 {
			doc.Revision = f.CurrentRev
		}
		currentIR = &doc
	}
	var statusReport *ir.StatusReportIR
	if f.ReportJSON != "" {
		var rep ir.StatusReportIR
		if err := json.Unmarshal([]byte(f.ReportJSON), &rep); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal statusReport: %w", err)
		}
		statusReport = &rep
	}
	var diffs []ir.DiffEntry
	if f.LastDiffJSON != "" {
		if err := json.Unmarshal([]byte(f.LastDiffJSON), &diffs); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal lastDiff: %w", err)
		}
	}
	var warns []string
	if f.WarningsJSON != "" {
		if err := json.Unmarshal([]byte(f.WarningsJSON), &warns); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal warnings: %w", err)
		}
	}
	var meta *llm.CompletionMeta
	if f.LLMMetaJSON != "" {
		var m llm.CompletionMeta
		if err := json.Unmarshal([]byte(f.LLMMetaJSON), &m); err != nil {
			return JobSnapshot{}, fmt.Errorf("unmarshal llmMeta: %w", err)
		}
		meta = &m
	}
	qKind := query.Kind(f.QueryKind)
	if qKind == "" && f.JobKind != JobKindReport {
		qKind = query.KindPromQL
	}
	return JobSnapshot{
		JobID:              f.JobID,
		JobKind:            f.JobKind,
		Status:             JobStatus(f.Status),
		DatasourceUID:      f.DatasourceUID,
		QueryKind:          string(qKind),
		Scope:              scope,
		ImportSource:       importSrc,
		CurrentIR:          currentIR,
		StatusReport:       statusReport,
		LastDiff:           diffs,
		Summary:            f.Summary,
		Warnings:           warns,
		ErrorMessage:       f.ErrorMessage,
		RepairAttemptsUsed: f.RepairAttemptsUsed,
		MaxRepairAttempts:  f.MaxRepairAttempts,
		CanUndo:            f.HistoryLen > 0,
		LLM:                meta,
		UpdatedAt:          time.UnixMilli(f.UpdatedAtMs).UTC(),
	}, nil
}

// selectEvictionVictims computes the ordered slice of inactive job IDs that must be evicted
// to make room for one new job under limits. Both callerInactive and otherOrgInactive must be
// ordered oldest-updated first.
//
// When allowOtherOrgEviction is false (Import jobs), only callerInactive jobs may be evicted
// so unthrottled imports can never evict another user's non-expired job.
func selectEvictionVictims(
	orgCount int,
	userCount int,
	callerInactive []string,
	otherOrgInactive []string,
	limits StoreLimits,
	allowOtherOrgEviction bool,
) ([]string, error) {
	needUserEvict := 0
	if userCount >= limits.MaxJobsPerUser {
		needUserEvict = userCount - limits.MaxJobsPerUser + 1
	}

	if !allowOtherOrgEviction {
		needOrgEvict := 0
		if orgCount >= limits.MaxJobs {
			needOrgEvict = orgCount - limits.MaxJobs + 1
		}
		needTotal := needUserEvict
		if needOrgEvict > needTotal {
			needTotal = needOrgEvict
		}
		if needTotal > len(callerInactive) {
			return nil, ErrConcurrencyLimited
		}
		if needTotal == 0 {
			return nil, nil
		}
		victims := make([]string, needTotal)
		copy(victims, callerInactive[:needTotal])
		return victims, nil
	}

	if needUserEvict > len(callerInactive) {
		return nil, ErrConcurrencyLimited
	}
	remainingCaller := callerInactive[needUserEvict:]
	remainingOrgCount := orgCount - needUserEvict

	needOrgEvict := 0
	if remainingOrgCount >= limits.MaxJobs {
		needOrgEvict = remainingOrgCount - limits.MaxJobs + 1
	}
	if needOrgEvict > len(remainingCaller)+len(otherOrgInactive) {
		return nil, ErrConcurrencyLimited
	}

	total := needUserEvict + needOrgEvict
	if total == 0 {
		return nil, nil
	}
	victims := make([]string, 0, total)
	victims = append(victims, callerInactive[:needUserEvict]...)
	// Evict caller's own inactive jobs first, then oldest inactive jobs within the same OrgID.
	for i := 0; i < len(remainingCaller) && needOrgEvict > 0; i++ {
		victims = append(victims, remainingCaller[i])
		needOrgEvict--
	}
	for i := 0; i < len(otherOrgInactive) && needOrgEvict > 0; i++ {
		victims = append(victims, otherOrgInactive[i])
		needOrgEvict--
	}
	return victims, nil
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

func marshalGaugeMap(gauges map[string]bool) (string, error) {
	if len(gauges) == 0 {
		return "[]", nil
	}
	list := make([]string, 0, len(gauges))
	for k, v := range gauges {
		if v {
			list = append(list, k)
		}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalGaugeMap(raw string) (map[string]bool, error) {
	out := make(map[string]bool)
	if raw == "" || raw == "[]" {
		return out, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	mergeGaugeMapBounded(out, list)
	return out, nil
}

func maxInt64(a, b int64) int64 {
	if a >= b {
		return a
	}
	return b
}
