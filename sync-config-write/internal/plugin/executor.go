package plugin

import (
	"context"
	"encoding/base64"
	"fmt"
)

type Planner interface {
	Plan(context.Context, Operation, ConfigSnapshot, Settings) (CommitProposal, ErrorCode)
}

type WriterExecutor struct {
	planner Planner
	engine  *CommitEngine
}

func NewWriterExecutor(planner Planner, engine *CommitEngine) *WriterExecutor {
	return &WriterExecutor{planner: planner, engine: engine}
}

func (e *WriterExecutor) Execute(ctx context.Context, operation Operation, settings Settings) Outcome {
	return e.ExecuteWithProgress(ctx, operation, settings, nil)
}

func (e *WriterExecutor) ExecuteWithProgress(ctx context.Context, operation Operation, settings Settings, progress func(int, RunState)) Outcome {
	if operation != OperationAutoPull && operation != OperationMetadataSync {
		return Outcome{State: StateFailed, Code: CodeNotImplemented}
	}
	if e == nil || e.planner == nil || e.engine == nil {
		return Outcome{State: StateFailed, Code: CodeNotImplemented}
	}
	for attempt := 0; attempt <= settings.MaxVersionRetries; attempt++ {
		attemptNumber := attempt + 1
		if progress != nil {
			progress(attemptNumber, StatePlanning)
		}
		raw, code := e.engine.getConfig(ctx, settings)
		if code != "" {
			return Outcome{State: StateFailed, Code: code}
		}
		snapshot := NewConfigSnapshot(raw)
		proposal, code := e.planner.Plan(ctx, operation, snapshot, settings)
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
	return CommitProposal{BaseVersion: version, ConfigBase64: base64.StdEncoding.EncodeToString(raw)}
}

func validatePlannerProposal(snapshot ConfigSnapshot, proposal CommitProposal) error {
	if _, err := proposal.Decode(snapshot.Version); err != nil {
		return fmt.Errorf("invalid planner proposal")
	}
	return nil
}
