package jobs

import (
	"context"
	"time"
)

const (
	OperationGenerate = "generate"
	OperationPatch    = "patch"
	OperationRepair   = "repair"
)

// StageEvent represents an application-level workflow stage transition during a job operation.
// Per ADR-004, StageEvent strictly carries lifecycle metadata and NEVER contains raw LLM tokens
// or unvalidated partial JSON fragments.
type StageEvent struct {
	JobID     string    `json:"jobId,omitempty"`
	Operation string    `json:"operation"`
	Stage     JobStatus `json:"stage"`
	Timestamp time.Time `json:"timestamp"`
}

// StageObserver receives synchronous notifications as a job transitions between lifecycle stages.
type StageObserver func(StageEvent)

type stageObserverCtxKey struct{}

// WithStageObserver attaches an optional StageObserver to ctx so Generate, Patch, and Repair
// emit application-level stage progress notifications without altering method signatures.
func WithStageObserver(ctx context.Context, observer StageObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, stageObserverCtxKey{}, observer)
}

func stageObserverFromContext(ctx context.Context) StageObserver {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(stageObserverCtxKey{}).(StageObserver)
	return obs
}

func (m *Manager) emitStage(ctx context.Context, jobID, operation string, stage JobStatus) {
	obs := stageObserverFromContext(ctx)
	if obs == nil || stage == "" {
		return
	}
	obs(StageEvent{
		JobID:     jobID,
		Operation: operation,
		Stage:     stage,
		Timestamp: m.now().UTC().Truncate(time.Millisecond),
	})
}
