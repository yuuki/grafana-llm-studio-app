package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/prompt"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

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
	if errMsg := validateTargetedPatch(patch, targetPanelID); errMsg != "" {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationPatch, nil, errMsg, &meta)
	}

	return m.applyValidateAndCommitPatch(ctx, owner, jobID, opToken, OperationPatch, op, qKind, adapter, boundedCatalog, patch, patch.Summary, &meta,
		"パッチ適用に失敗しました: %v",
		fmt.Sprintf("パッチ後の %s 検査に失敗しました: %%v", adapter.DisplayName()),
	)
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

	targets, err := collectRepairTargets(cand.CurrentIR.Panels, in.Results)
	if err != nil {
		return JobSnapshot{}, err
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
	if errMsg := validateRepairPatch(patch, targets, qKind, adapter.DisplayName()); errMsg != "" {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, OperationRepair, nil, errMsg, &meta)
	}

	return m.applyValidateAndCommitPatch(ctx, owner, jobID, opToken, OperationRepair, op, qKind, adapter, boundedCatalog, patch,
		"構文エラーのパネルを修復しました。変更パネルを再検証してください。", &meta,
		"修復パッチ適用に失敗しました: %v",
		fmt.Sprintf("修復後の %s/スコープ検査に失敗しました: %%v", adapter.DisplayName()),
	)
}

func (m *Manager) applyValidateAndCommitPatch(
	ctx context.Context,
	owner Owner,
	jobID string,
	opToken uint64,
	operation string,
	op BeginOpResult,
	qKind query.Kind,
	adapter query.Adapter,
	boundedCatalog []ir.MetricCatalogItem,
	patch ir.DashboardPatch,
	commitSummary string,
	meta *llm.CompletionMeta,
	applyErrFmt string,
	validateErrFmt string,
) (JobSnapshot, error) {
	nextIR, diffs, err := ir.ApplyPatch(op.CurrentIR, patch, ir.ValidateOptions{MaxPanels: m.cfg.MaxPanels})
	if err != nil {
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, operation, err, fmt.Sprintf(applyErrFmt, err), meta)
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
		return m.recordEditFailureWithStage(ctx, owner, jobID, opToken, operation, err, fmt.Sprintf(validateErrFmt, err), meta)
	}

	return m.commitRevisionWithStage(ctx, owner, jobID, opToken, operation, op.CurrentIR.Revision, nextIR, diffs, commitSummary, warnings, meta)
}

// validateTargetedPatch enforces that when the user explicitly targeted a single panel,
// the LLM only updates that panel.
func validateTargetedPatch(patch ir.DashboardPatch, targetPanelID string) string {
	if targetPanelID == "" {
		return ""
	}
	for _, pOp := range patch.Operations {
		if pOp.Op != "update_panel" || pOp.PanelID != targetPanelID {
			return fmt.Sprintf("対象パネル指定 (%s) に反するパッチ操作 (%s:%s) を拒否しました", targetPanelID, pOp.Op, pOp.PanelID)
		}
	}
	return ""
}

func collectRepairTargets(panels []ir.PanelIR, results []ir.PanelValidationResult) ([]prompt.RepairTarget, error) {
	panelByID := make(map[string]ir.PanelIR, len(panels))
	for _, p := range panels {
		panelByID[p.ID] = p
	}

	seenPanels := make(map[string]bool, len(results))
	var targets []prompt.RepairTarget
	var rejectedReasons []string
	for _, r := range results {
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
			return nil, fmt.Errorf("%w (%s)", ErrNotRepairable, strings.Join(rejectedReasons, "; "))
		}
		return nil, fmt.Errorf("%w: no syntax_error panel matched current revision", ErrNotRepairable)
	}
	return targets, nil
}

// validateRepairPatch enforces that repair ONLY touches Query or Queries on the failing syntax_error
// panels and NEVER mutates query.kind away from the job's active dialect.
func validateRepairPatch(patch ir.DashboardPatch, targets []prompt.RepairTarget, qKind query.Kind, dialectName string) string {
	allowedTargetIDs := make(map[string]bool, len(targets))
	for _, t := range targets {
		allowedTargetIDs[t.PanelID] = true
	}
	for _, pOp := range patch.Operations {
		if pOp.Op != "update_panel" || !allowedTargetIDs[pOp.PanelID] || pOp.Panel == nil || (pOp.Panel.Query == nil && pOp.Panel.Queries == nil) {
			return fmt.Sprintf("修復パッチが対象外パネル/操作 (%s:%s) を含んでいたため拒否しました", pOp.Op, pOp.PanelID)
		}
		p := pOp.Panel
		if p.DatasourceRef != "" && p.DatasourceRef != ir.SelectedDatasourceRef {
			return "修復パッチによるデータソース変更は禁止されています"
		}
		if p.Type != "" || p.Title != "" || p.Description != nil || p.Unit != "" || p.Thresholds != nil || p.Layout != nil {
			return fmt.Sprintf("修復パッチは失敗パネルの %s クエリのみ変更可能です", dialectName)
		}
		if p.Query != nil && p.Query.Kind != nil && *p.Query.Kind != "" && *p.Query.Kind != string(qKind) {
			return fmt.Sprintf("修復パッチによるクエリ方言変更 (%q -> %q) は禁止されています", qKind, *p.Query.Kind)
		}
		if p.Queries != nil {
			for _, q := range *p.Queries {
				if q.Kind != "" && q.Kind != string(qKind) {
					return fmt.Sprintf("修復パッチによるクエリ方言変更 (%q -> %q) は禁止されています", qKind, q.Kind)
				}
			}
		}
	}
	return ""
}
