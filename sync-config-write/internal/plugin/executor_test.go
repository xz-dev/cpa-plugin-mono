package plugin

import (
	"context"
	"sync"
	"testing"
)

type fakePlanner struct {
	mu        sync.Mutex
	calls     int
	versions  []string
	proposals [][]byte
}

func (p *fakePlanner) PlanWithProgress(ctx context.Context, operation Operation, snapshot ConfigSnapshot, settings Settings, progress func(RunState)) (CommitProposal, ErrorCode) {
	if progress != nil {
		progress(StateFetching)
	}
	return p.Plan(ctx, operation, snapshot, settings)
}

func (p *fakePlanner) Plan(_ context.Context, _ Operation, snapshot ConfigSnapshot, _ Settings) (CommitProposal, ErrorCode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.versions = append(p.versions, snapshot.Version)
	raw, _ := snapshot.Decode()
	proposal := membershipProposal(raw)
	if len(p.proposals) >= p.calls {
		proposal = p.proposals[p.calls-1]
	}
	return ProposalFromBytes(snapshot.Version, proposal), ""
}

func TestWriterExecutorRetriesVersionConflictFromFreshSnapshot(t *testing.T) {
	base := commitTestConfig("old")
	engine, core, workers, closeFn := newCommitHarness(t, base)
	defer closeFn()
	for id, status := range statusTuplesFor(base, 1) {
		workers.set(id, status)
	}
	planner := &fakePlanner{}
	executor := NewWriterExecutor(planner, engine)
	originalGet := engine.getConfig
	_ = originalGet
	core.mu.Lock()
	core.raw = base
	core.mu.Unlock()
	// First plan races with an external exact-byte change; second plan receives fresh bytes.
	first := true
	engine.beforeCommitForTest = func() {
		if !first {
			return
		}
		first = false
		core.mu.Lock()
		core.raw = append(core.raw, []byte("# external\n")...)
		core.mu.Unlock()
		for id, status := range statusTuplesFor(core.raw, 1) {
			workers.set(id, status)
		}
	}
	engine.afterVerified = func(expected []byte) {
		for id, status := range statusTuplesFor(expected, 2) {
			workers.set(id, status)
		}
	}
	settings := Settings{CoreOrigin: engine.settings().CoreOrigin, ManagementKey: "mgmt-secret", WorkerToken: "worker-secret", MaxVersionRetries: 1}
	type progressEvent struct {
		attempt int
		state   RunState
	}
	var progress []progressEvent
	outcome := executor.ExecuteWithProgress(context.Background(), OperationAutoPull, settings, func(attempt int, state RunState) {
		progress = append(progress, progressEvent{attempt: attempt, state: state})
	})
	if outcome.Code != "" || !outcome.Changed || planner.calls != 2 || len(planner.versions) != 2 || planner.versions[0] == planner.versions[1] {
		t.Fatalf("outcome=%+v calls=%d versions=%v", outcome, planner.calls, planner.versions)
	}
	want := []progressEvent{
		{attempt: 1, state: StatePlanning},
		{attempt: 1, state: StateFetching},
		{attempt: 1, state: StateCommitting},
		{attempt: 2, state: StatePlanning},
		{attempt: 2, state: StateFetching},
		{attempt: 2, state: StateCommitting},
		{attempt: 2, state: StateWaiting},
	}
	if len(progress) != len(want) {
		t.Fatalf("progress=%+v want=%+v", progress, want)
	}
	for i := range want {
		if progress[i] != want[i] {
			t.Fatalf("progress[%d]=%+v want=%+v", i, progress[i], want[i])
		}
	}
}
