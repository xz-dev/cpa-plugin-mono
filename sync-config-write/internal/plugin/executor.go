package plugin

import (
	"context"
	"encoding/base64"
)

type Planner interface {
	Plan(context.Context, Operation, ConfigSnapshot, Settings) (CommitProposal, ErrorCode)
}

type ProgressPlanner interface {
	PlanWithProgress(context.Context, Operation, ConfigSnapshot, Settings, func(RunState)) (CommitProposal, ErrorCode)
}

type WriterExecutor struct {
	planner   Planner
	engine    *CommitEngine
	modelInfo *ModelInfoRefresher
}

func NewWriterExecutor(planner Planner, engine *CommitEngine, modelInfo ...*ModelInfoRefresher) *WriterExecutor {
	executor := &WriterExecutor{planner: planner, engine: engine}
	if len(modelInfo) != 0 {
		executor.modelInfo = modelInfo[0]
	} else if engine != nil {
		executor.modelInfo = NewModelInfoRefresher(engine.client, engine)
	}
	return executor
}

func (e *WriterExecutor) Execute(ctx context.Context, operation Operation, settings Settings) Outcome {
	return e.ExecuteWithProgress(ctx, operation, settings, nil)
}

func (e *WriterExecutor) ExecuteWithProgress(ctx context.Context, operation Operation, settings Settings, progress func(int, RunState)) Outcome {
	if e == nil || e.engine == nil {
		return Outcome{State: StateFailed, Code: CodeNotImplemented}
	}
	if operation == OperationModelInfo {
		if e.modelInfo == nil {
			return Outcome{State: StateFailed, Code: CodeNotImplemented}
		}
		if progress != nil {
			progress(1, StatePlanning)
		}
		return e.modelInfo.Refresh(ctx, settings, func(state RunState) {
			if progress != nil {
				progress(1, state)
			}
		})
	}
	if operation != OperationAutoPull && operation != OperationMetadataSync || e.planner == nil {
		return Outcome{State: StateFailed, Code: CodeNotImplemented}
	}
	for attempt := 0; attempt <= settings.MaxVersionRetries; attempt++ {
		attemptNumber := attempt + 1
		if progress != nil {
			progress(attemptNumber, StatePlanning)
		}
		raw, getCode := e.engine.getConfig(ctx, settings)
		if getCode != "" {
			return Outcome{State: StateFailed, Code: getCode}
		}
		snapshot := NewConfigSnapshot(raw)
		var proposal CommitProposal
		var code ErrorCode
		if planner, ok := e.planner.(ProgressPlanner); ok {
			proposal, code = planner.PlanWithProgress(ctx, operation, snapshot, settings, func(state RunState) {
				if progress != nil {
					progress(attemptNumber, state)
				}
			})
		} else {
			proposal, code = e.planner.Plan(ctx, operation, snapshot, settings)
		}
		if code != "" {
			return Outcome{State: StateFailed, Code: code}
		}
		if _, err := proposal.Decode(snapshot.Version); err != nil {
			return Outcome{State: StateFailed, Code: CodeInvalidRequest}
		}
		result := e.engine.commitWithSettings(ctx, operation, CommitRequest{Proposal: proposal}, settings, func(state RunState) {
			if progress != nil {
				progress(attemptNumber, state)
			}
		})
		if result.Code == CodeVersionConflict && attempt < settings.MaxVersionRetries {
			continue
		}
		outcome := Outcome{State: result.State, Code: result.Code, Version: result.Version, Changed: result.Changed}
		if result.Code == CodePersistedRuntimeUncertain || result.Code == CodeCommitVerificationFailed {
			outcome.Block = BlockRecord{Code: result.Code, Version: result.PersistedVersion, PreStatus: result.PreStatus, ExpectedHashes: result.ExpectedHashes}
		}
		return outcome
	}
	return Outcome{State: StateFailed, Code: CodeVersionConflict}
}

func (e *WriterExecutor) Reconcile(ctx context.Context, block BlockRecord, settings Settings) ReconcileResult {
	if e == nil || e.engine == nil {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	return e.engine.ReconcileWithSettings(ctx, block, settings)
}

func (e *WriterExecutor) ReconcileAndClear(ctx context.Context, block BlockRecord, settings Settings, clear func(BlockRecord, string) bool) ReconcileResult {
	if e == nil || e.engine == nil {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	e.engine.mu.Lock()
	defer e.engine.mu.Unlock()
	result := e.engine.reconcileLocked(ctx, block, settings)
	if !result.Cleared || !clear(block, result.Version) {
		return ReconcileResult{Code: CodeReconcileFailed, Version: result.Version}
	}
	return result
}

func ProposalFromBytes(version string, raw []byte) CommitProposal {
	return CommitProposal{BaseVersion: version, ConfigBase64: base64.StdEncoding.EncodeToString(raw), Report: []byte(`{"changed":true}`)}
}
