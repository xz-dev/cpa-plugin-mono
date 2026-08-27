package plugin

import (
	"context"
	"time"
)

type BlockRecord struct {
	ID               string
	Code             ErrorCode
	Version          string
	PreStatus        map[string]WorkerStatus
	ExpectedHashes   map[string]string
	Restarted        bool
	EvidenceCaptured time.Time
}

type ReconcileResult struct {
	Cleared bool
	Code    ErrorCode
	Version string
}

type ReconcileExecutor interface {
	Reconcile(context.Context, BlockRecord, Settings) ReconcileResult
}

type AtomicReconcileExecutor interface {
	ReconcileAndClear(context.Context, BlockRecord, Settings, func(BlockRecord, string) bool) ReconcileResult
}

func (e *CommitEngine) Reconcile(ctx context.Context, block BlockRecord) ReconcileResult {
	return e.ReconcileWithSettings(ctx, block, e.settings())
}

func (e *CommitEngine) ReconcileWithSettings(ctx context.Context, block BlockRecord, settings Settings) ReconcileResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reconcileLocked(ctx, block, settings)
}

func (e *CommitEngine) reconcileLocked(ctx context.Context, block BlockRecord, settings Settings) ReconcileResult {
	if block.Code == CodePlannerStalled {
		for _, id := range []string{"auto-pull-models", "model-metadata-sync"} {
			status, err := e.workers.Status(ctx, id, settings)
			if err != nil || validateWorkerStatus(status) != nil || status.ActivePlan {
				return ReconcileResult{Code: CodeReconcileFailed}
			}
		}
		return ReconcileResult{Cleared: true}
	}
	if block.Code != CodeStartupReconcileRequired && block.Code != CodePersistedRuntimeUncertain && block.Code != CodeCommitVerificationFailed {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	if (block.Code == CodePersistedRuntimeUncertain || block.Code == CodeCommitVerificationFailed) && len(block.PreStatus) == 0 {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	raw, code := e.getConfig(ctx, settings)
	if code != "" {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	version := configVersion(raw)
	if block.Version != "" && block.Version != version {
		return ReconcileResult{Code: CodeReconcileFailed, Version: version}
	}
	hashes, err := runtimeConfigHashes(raw)
	if err != nil {
		return ReconcileResult{Code: CodeReconcileFailed, Version: version}
	}
	statuses, err := e.captureStatus(ctx, settings, hashes)
	if err != nil {
		return ReconcileResult{Code: CodeReconcileFailed, Version: version}
	}
	for _, id := range []string{"auto-pull-models", "model-metadata-sync"} {
		if statuses[id].ActivePlan {
			return ReconcileResult{Code: CodeReconcileFailed, Version: version}
		}
	}
	if len(block.PreStatus) != 0 {
		for _, id := range pluginIDs {
			before, ok := block.PreStatus[id]
			if !ok {
				return ReconcileResult{Code: CodeReconcileFailed, Version: version}
			}
			current := statuses[id]
			if block.Restarted {
				if current.InstanceID == before.InstanceID {
					return ReconcileResult{Code: CodeReconcileFailed, Version: version}
				}
			} else if current.InstanceID != before.InstanceID || current.ReconfigureSeq <= before.ReconfigureSeq {
				return ReconcileResult{Code: CodeReconcileFailed, Version: version}
			}
		}
	}
	if e.beforeReconcileFinalGetForTest != nil {
		e.beforeReconcileFinalGetForTest()
	}
	finalRaw, finalCode := e.getConfig(ctx, settings)
	if finalCode != "" || configVersion(finalRaw) != version {
		return ReconcileResult{Code: CodeReconcileFailed, Version: version}
	}
	return ReconcileResult{Cleared: true, Version: version}
}
