package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartupAndPersistedUncertaintyReconcileRequireAuthoritativeHashesAndInactivePlanners(t *testing.T) {
	for _, kind := range []ErrorCode{CodeStartupReconcileRequired, CodePersistedRuntimeUncertain, CodeCommitVerificationFailed} {
		t.Run(string(kind), func(t *testing.T) {
			raw := commitTestConfig("persisted")
			engine, _, workers, closeFn := newCommitHarness(t, raw)
			defer closeFn()
			statuses := statusTuplesFor(raw, 3)
			for id, status := range statuses {
				workers.set(id, status)
			}
			block := BlockRecord{ID: mustOpaqueID(t), Code: kind, Version: configVersion(raw), PreStatus: statusTuplesFor(commitTestConfig("old"), 2)}
			result := engine.Reconcile(context.Background(), block)
			if !result.Cleared || result.Version != configVersion(raw) || result.Code != "" {
				t.Fatalf("result=%+v", result)
			}
			bad := statuses["model-info"]
			bad.ConfigSHA256 = configVersion([]byte("wrong"))
			workers.set("model-info", bad)
			result = engine.Reconcile(context.Background(), block)
			if result.Cleared || result.Code != CodeReconcileFailed {
				t.Fatalf("wrong hash result=%+v", result)
			}
		})
	}
}

func TestPersistedUncertaintyReconcileUsesSequenceWhenEvidenceSurvivesAndFreshInstanceAfterRestart(t *testing.T) {
	raw := commitTestConfig("persisted")
	engine, _, workers, closeFn := newCommitHarness(t, raw)
	defer closeFn()
	current := statusTuplesFor(raw, 2)
	pre := statusTuplesFor(raw, 2)
	for id, status := range current {
		workers.set(id, status)
	}
	block := BlockRecord{ID: mustOpaqueID(t), Code: CodePersistedRuntimeUncertain, Version: configVersion(raw), PreStatus: pre}
	if result := engine.Reconcile(context.Background(), block); result.Cleared {
		t.Fatalf("same sequence cleared: %+v", result)
	}
	for id, status := range statusTuplesFor(raw, 3) {
		workers.set(id, status)
	}
	if result := engine.Reconcile(context.Background(), block); !result.Cleared {
		t.Fatalf("advanced sequence did not clear: %+v", result)
	}
	fresh := statusTuplesFor(raw, 1)
	for id, status := range fresh {
		status.InstanceID = mustOpaqueID(t)
		workers.set(id, status)
	}
	block.Restarted = true
	if result := engine.Reconcile(context.Background(), block); !result.Cleared {
		t.Fatalf("fresh restart hashes did not clear: %+v", result)
	}
}

func TestPostPUTUncertaintyWithoutSurvivingEvidenceCannotClear(t *testing.T) {
	raw := commitTestConfig("persisted")
	for _, code := range []ErrorCode{CodePersistedRuntimeUncertain, CodeCommitVerificationFailed} {
		t.Run(string(code), func(t *testing.T) {
			engine, _, workers, closeFn := newCommitHarness(t, raw)
			defer closeFn()
			for id, status := range statusTuplesFor(raw, 3) {
				workers.set(id, status)
			}
			result := engine.Reconcile(context.Background(), BlockRecord{ID: mustOpaqueID(t), Code: code, Version: configVersion(raw)})
			if result.Cleared || result.Code != CodeReconcileFailed {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestWriterExecutorReconcileUsesQueuedSettingsSnapshot(t *testing.T) {
	raw := commitTestConfig("persisted")
	core := &coreHarness{raw: append([]byte(nil), raw...)}
	server := httptest.NewServer(http.HandlerFunc(core.handler))
	defer server.Close()
	workers := &fakeWorkerStatuses{statuses: statusTuplesFor(raw, 3)}
	current := Settings{CoreOrigin: "http://127.0.0.1:1", ManagementKey: "new-management", WorkerToken: "new-worker"}
	engine := NewCommitEngine(NewLoopbackClient(), workers, func() Settings { return current }, nil)
	executor := NewWriterExecutor(nil, engine)
	queued := Settings{CoreOrigin: server.URL, ManagementKey: "mgmt-secret", WorkerToken: "queued-worker"}
	result := executor.Reconcile(context.Background(), BlockRecord{ID: mustOpaqueID(t), Code: CodeStartupReconcileRequired}, queued)
	if !result.Cleared || result.Code != "" || result.Version != configVersion(raw) {
		t.Fatalf("queued settings were not used: %+v", result)
	}
}

func TestReconcileRejectsStaleBlockedVersionButAllowsUnavailableVersionWithEvidence(t *testing.T) {
	currentRaw := commitTestConfig("current")
	oldRaw := commitTestConfig("old")
	engine, _, workers, closeFn := newCommitHarness(t, currentRaw)
	defer closeFn()
	for id, status := range statusTuplesFor(currentRaw, 3) {
		workers.set(id, status)
	}
	pre := statusTuplesFor(oldRaw, 2)
	stale := BlockRecord{ID: mustOpaqueID(t), Code: CodeCommitVerificationFailed, Version: configVersion(oldRaw), PreStatus: pre}
	if result := engine.Reconcile(context.Background(), stale); result.Cleared || result.Code != CodeReconcileFailed {
		t.Fatalf("stale blocker cleared: %+v", result)
	}
	stale.Version = ""
	if result := engine.Reconcile(context.Background(), stale); !result.Cleared || result.Version != configVersion(currentRaw) {
		t.Fatalf("unavailable version with complete evidence did not clear: %+v", result)
	}
}

func TestServiceClearMatchingBlockBindsReconciledVersion(t *testing.T) {
	s := New(nil)
	defer s.Shutdown()
	block := s.blockRecordForTest()
	block.Version = configVersion([]byte("blocked"))
	s.replaceBlockForTest(block.Code, block.Version)
	block = s.blockRecordForTest()
	if s.clearMatchingBlock(block, configVersion([]byte("different"))) {
		t.Fatal("mismatched reconciled version cleared blocker")
	}
	if !s.Status().WriterBlocked {
		t.Fatal("mismatched version removed blocker")
	}
}

func TestPlannerStalledReconcileNeedsBothInactiveAndLive(t *testing.T) {
	raw := commitTestConfig("old")
	engine, _, workers, closeFn := newCommitHarness(t, raw)
	defer closeFn()
	statuses := statusTuplesFor(raw, 1)
	for id, status := range statuses {
		workers.set(id, status)
	}
	active := statuses["auto-pull-models"]
	active.ActivePlan = true
	workers.set("auto-pull-models", active)
	block := BlockRecord{ID: mustOpaqueID(t), Code: CodePlannerStalled}
	if result := engine.Reconcile(context.Background(), block); result.Cleared {
		t.Fatalf("active planner cleared: %+v", result)
	}
	active.ActivePlan = false
	workers.set("auto-pull-models", active)
	if result := engine.Reconcile(context.Background(), block); !result.Cleared {
		t.Fatalf("inactive planners did not clear: %+v", result)
	}
	workers.err = context.DeadlineExceeded
	if result := engine.Reconcile(context.Background(), block); result.Cleared {
		t.Fatalf("unavailable planners cleared: %+v", result)
	}
}

func TestReconcileRejectsConcurrentAuthoritativeConfigChange(t *testing.T) {
	raw := commitTestConfig("persisted")
	engine, core, workers, closeFn := newCommitHarness(t, raw)
	defer closeFn()
	for id, status := range statusTuplesFor(raw, 2) {
		workers.set(id, status)
	}
	engine.beforeReconcileFinalGetForTest = func() {
		core.mu.Lock()
		core.raw = append(core.raw, []byte("# concurrent\n")...)
		core.mu.Unlock()
	}
	result := engine.Reconcile(context.Background(), BlockRecord{ID: mustOpaqueID(t), Code: CodeStartupReconcileRequired})
	if result.Cleared || result.Code != CodeReconcileFailed {
		t.Fatalf("result=%+v", result)
	}
}

func TestServiceReconcileClearsOnlyMatchingBlockAndVersionUnderCommitMutex(t *testing.T) {
	setValidSecrets(t)
	executor := &blockingAtomicReconcileExecutor{started: make(chan BlockRecord, 1), release: make(chan struct{})}
	s := New(executor)
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	initial := s.blockRecordForTest()
	id := acceptedRunID(t, s.HandleManagement(managementRequest("POST", reconcilePath, nil)))
	got := <-executor.started
	if got.ID != initial.ID {
		t.Fatalf("got=%+v initial=%+v", got, initial)
	}
	s.replaceBlockForTest(CodePersistedRuntimeUncertain, configVersion([]byte("new")))
	close(executor.release)
	status := waitForState(t, s, id, StateFailed)
	if status.ErrorCode != CodeReconcileFailed || !s.Status().WriterBlocked {
		t.Fatalf("status=%+v writer=%+v", status, s.Status())
	}
}

type blockingAtomicReconcileExecutor struct {
	started chan BlockRecord
	release chan struct{}
}

func (e *blockingAtomicReconcileExecutor) Execute(context.Context, Operation, Settings) Outcome {
	return Outcome{Code: CodeNotImplemented}
}
func (e *blockingAtomicReconcileExecutor) ReconcileAndClear(_ context.Context, block BlockRecord, _ Settings, clear func(BlockRecord, string) bool) ReconcileResult {
	e.started <- block
	<-e.release
	if !clear(block, block.Version) {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	return ReconcileResult{Cleared: true, Version: block.Version}
}
