package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/prompt"
)

// AnalyzeStatus validates an operational evidence bundle, executes a single grounded LLM
// completion via grafana-llm-app (or explicit dev mock), verifies fail-closed StatusReportIR
// invariants and evidence citation grounding, redacts any echoed secrets, and persists ONLY
// the validated StatusReportIR (never raw input evidence) under owner isolation with TTL.
func (m *Manager) AnalyzeStatus(ctx context.Context, owner Owner, in ir.AnalyzeStatusPayload) (JobSnapshot, error) {
	if err := validateOwner(owner); err != nil {
		return JobSnapshot{}, err
	}
	dsUID, err := m.validateAnalyzeDatasource(in)
	if err != nil {
		return JobSnapshot{}, err
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
		return m.handleAnalyzeLLMError(ctx, opCtx, owner, jobID, opToken, &meta, err)
	}

	return m.decodeAndPersistStatusReport(ctx, owner, jobID, opToken, raw, &meta, allowedEvidenceIDs)
}

func (m *Manager) validateAnalyzeDatasource(in ir.AnalyzeStatusPayload) (string, error) {
	dsUID := strings.TrimSpace(in.DatasourceUID)
	if len(in.Evidence.Metrics) > 0 || in.DashboardContext != nil {
		if dsUID == "" || !m.isDatasourceAllowed(dsUID) {
			return "", fmt.Errorf("%w: %q", ErrDatasourceDenied, dsUID)
		}
		return dsUID, nil
	}
	if dsUID != "" && !m.isDatasourceAllowed(dsUID) {
		return "", fmt.Errorf("%w: %q", ErrDatasourceDenied, dsUID)
	}
	return dsUID, nil
}

func (m *Manager) handleAnalyzeLLMError(
	ctx, opCtx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	meta *llm.CompletionMeta,
	err error,
) (JobSnapshot, error) {
	if errors.Is(err, context.Canceled) || opCtx.Err() == context.Canceled {
		snap, _ := m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusCancelled, nil, "Status analysis was cancelled.", "Status analysis was cancelled.", meta)
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
	snap, finishErr := m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusFailed, nil, "", errMsg, meta)
	if errors.Is(finishErr, ErrJobCancelled) {
		return snap, ErrJobCancelled
	}
	return snap, retErr
}

func (m *Manager) decodeAndPersistStatusReport(
	ctx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	raw []byte,
	meta *llm.CompletionMeta,
	allowedEvidenceIDs map[string]bool,
) (JobSnapshot, error) {
	m.setStatusIfToken(ctx, owner, jobID, opToken, StatusValidatingReport)
	m.emitStage(ctx, jobID, OperationAnalyze, StatusValidatingReport)

	report, err := ir.DecodeStrictStatusReport(raw)
	if err != nil {
		errMsg := fmt.Sprintf("Status report JSON schema validation failed: %s", prompt.RedactSecretsAndURLs(err.Error()))
		return m.failReportJob(ctx, owner, jobID, opToken, errMsg, meta, err)
	}

	// Defense-in-depth: redact any secret-like strings before final validation & persistence
	// so redaction never silently causes post-validation length overshoot.
	prompt.RedactStatusReport(&report)

	if err := ir.ValidateStatusReport(report, allowedEvidenceIDs); err != nil {
		errMsg := fmt.Sprintf("Status report grounding or hygiene validation failed: %s", prompt.RedactSecretsAndURLs(err.Error()))
		return m.failReportJob(ctx, owner, jobID, opToken, errMsg, meta, err)
	}

	return m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusReady, &report, report.ExecutiveSummary, "", meta)
}

func (m *Manager) failReportJob(
	ctx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	errMsg string,
	meta *llm.CompletionMeta,
	cause error,
) (JobSnapshot, error) {
	snap, finishErr := m.finishReportJobWithStage(ctx, owner, jobID, opToken, StatusFailed, nil, "", errMsg, meta)
	if errors.Is(finishErr, ErrJobCancelled) {
		return snap, ErrJobCancelled
	}
	return snap, cause
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
