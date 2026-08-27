package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type blockingExecutor struct {
	started chan Settings
	release chan struct{}
}

func (e *blockingExecutor) Execute(_ context.Context, _ Operation, settings Settings) Outcome {
	e.started <- settings
	<-e.release
	return Outcome{Code: CodeNotImplemented}
}

type queuedBlockReconciler struct {
	started       chan Operation
	releaseFirst  chan struct{}
	blockCode     ErrorCode
	metadataCalls atomic.Int32
}

func (e *queuedBlockReconciler) Execute(_ context.Context, operation Operation, _ Settings) Outcome {
	e.started <- operation
	switch operation {
	case OperationAutoPull:
		<-e.releaseFirst
		version := configVersion([]byte("blocked"))
		return Outcome{State: StateUncertain, Code: e.blockCode, Version: version, Block: BlockRecord{Version: version}}
	case OperationMetadataSync:
		e.metadataCalls.Add(1)
	}
	return Outcome{State: StateSucceeded}
}

func (e *queuedBlockReconciler) ReconcileAndClear(_ context.Context, block BlockRecord, _ Settings, clear func(BlockRecord, string) bool) ReconcileResult {
	e.started <- OperationReconcile
	if !clear(block, block.Version) {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	return ReconcileResult{Cleared: true, Version: block.Version}
}

func TestQueuedWritesWaitForEvidenceBasedReconcileWithoutBlockingReadJobs(t *testing.T) {
	setValidSecrets(t)
	for _, code := range []ErrorCode{CodePlannerStalled, CodePersistedRuntimeUncertain, CodeCommitVerificationFailed} {
		t.Run(string(code), func(t *testing.T) {
			executor := &queuedBlockReconciler{started: make(chan Operation, 8), releaseFirst: make(chan struct{}), blockCode: code}
			s := New(executor)
			defer s.Shutdown()
			if err := s.Configure(validConfigYAML()); err != nil {
				t.Fatal(err)
			}
			s.ClearBlockForReconcileProof()

			autoID := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil)))
			if operation := <-executor.started; operation != OperationAutoPull {
				t.Fatalf("first operation=%s", operation)
			}
			metadataID := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runMetadataPath, nil)))
			modelInfoID := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, modelInfoPath, nil)))
			close(executor.releaseFirst)
			waitForState(t, s, autoID, StateUncertain)

			if operation := <-executor.started; operation != OperationModelInfo {
				t.Fatalf("operation past retained write=%s", operation)
			}
			waitForState(t, s, modelInfoID, StateSucceeded)
			if executor.metadataCalls.Load() != 0 {
				t.Fatal("queued metadata executed while Writer was blocked")
			}

			reconcileID := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, reconcilePath, nil)))
			if operation := <-executor.started; operation != OperationReconcile {
				t.Fatalf("operation past retained write=%s", operation)
			}
			waitForState(t, s, reconcileID, StateSucceeded)
			if operation := <-executor.started; operation != OperationMetadataSync {
				t.Fatalf("post-reconcile operation=%s", operation)
			}
			waitForState(t, s, metadataID, StateSucceeded)
			if executor.metadataCalls.Load() != 1 || s.Status().WriterBlocked {
				t.Fatalf("metadata calls=%d status=%+v", executor.metadataCalls.Load(), s.Status())
			}
		})
	}
}

func TestManagementRegistersExactFoundationRoutes(t *testing.T) {
	s := New(nil)
	defer s.Shutdown()
	got := s.ManagementRoutes().Routes
	want := map[string]bool{
		http.MethodPost + " /v0/management/plugins/sync-config-write/run/auto-pull-models":    true,
		http.MethodPost + " /v0/management/plugins/sync-config-write/run/model-metadata-sync": true,
		http.MethodPost + " /v0/management/plugins/sync-config-write/model-info/catalog":      true,
		http.MethodPost + " /v0/management/plugins/sync-config-write/reconcile":               true,
		http.MethodGet + " /v0/management/plugins/sync-config-write/status":                   true,
	}
	if len(got) != len(want) {
		t.Fatalf("routes=%+v", got)
	}
	for _, route := range got {
		if !want[route.Method+" "+route.Path] {
			t.Fatalf("unexpected route %s %s", route.Method, route.Path)
		}
	}
}

func TestBlockedAdmissionAndBlockClearAreRaceFree(t *testing.T) {
	s := New(nil)
	defer s.Shutdown()
	for i := 0; i < 20_000; i++ {
		s.blockForTest(CodePlannerStalled)
		var runID string
		var code ErrorCode
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			runID, _, code = s.enqueue(OperationAutoPull)
		}()
		go func() {
			defer wg.Done()
			s.ClearBlockForReconcileProof()
		}()
		wg.Wait()
		if runID == "" || (code != "" && code != CodePlannerStalled) {
			t.Fatalf("invalid admission outcome: run_id=%q code=%q", runID, code)
		}
		if s.Status().WriterBlocked {
			t.Fatal("completed clear left Writer blocked")
		}
	}
}

func TestConfigureLeavesStartupBlockUntilExplicitReconcile(t *testing.T) {
	setValidSecrets(t)
	s := NewService()
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}

	status := s.statusWithRuns()
	if !status.WriterBlocked || status.ErrorCode != CodeStartupReconcileRequired {
		t.Fatalf("status=%+v", status)
	}
	reconcileRuns := 0
	for _, run := range status.Runs {
		if run.Operation != OperationReconcile {
			continue
		}
		reconcileRuns++
		if run.RunID != status.BlockingRunID || run.State != StateBlocked || run.ErrorCode != CodeStartupReconcileRequired {
			t.Fatalf("unexpected startup reconcile run: status=%+v run=%+v", status, run)
		}
	}
	if reconcileRuns != 1 {
		t.Fatalf("startup reconcile runs=%d status=%+v", reconcileRuns, status)
	}

	blocked := s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil))
	if blocked.StatusCode != http.StatusConflict || !jsonBodyHasCode(blocked.Body, CodeWriterBlocked) {
		t.Fatalf("blocked=%d %s", blocked.StatusCode, blocked.Body)
	}
	explicit := s.HandleManagement(managementRequest(http.MethodPost, reconcilePath, nil))
	if explicit.StatusCode != http.StatusAccepted || acceptedRunID(t, explicit) == status.BlockingRunID {
		t.Fatalf("explicit reconcile=%d %s", explicit.StatusCode, explicit.Body)
	}
}

func TestStartupBlocksWritesButAdmitsReconcile(t *testing.T) {
	setValidSecrets(t)
	s := New(ExecutorFunc(func(context.Context, Operation, Settings) Outcome { return Outcome{Code: CodeNotImplemented} }))
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}

	blocked := s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil))
	if blocked.StatusCode != http.StatusConflict || !jsonBodyHasCode(blocked.Body, CodeWriterBlocked) {
		t.Fatalf("blocked=%d %s", blocked.StatusCode, blocked.Body)
	}
	modelInfo := s.HandleManagement(managementRequest(http.MethodPost, modelInfoPath, nil))
	if modelInfo.StatusCode != http.StatusAccepted {
		t.Fatalf("read-only model-info blocked=%d %s", modelInfo.StatusCode, modelInfo.Body)
	}
	reconcile := s.HandleManagement(managementRequest(http.MethodPost, reconcilePath, nil))
	if reconcile.StatusCode != http.StatusAccepted {
		t.Fatalf("reconcile=%d %s", reconcile.StatusCode, reconcile.Body)
	}
	var body triggerResponse
	if err := json.Unmarshal(reconcile.Body, &body); err != nil || body.RunID == "" {
		t.Fatalf("body=%s err=%v", reconcile.Body, err)
	}
}

func TestModelInfoAdmissionIgnoresEveryWriteBlockKind(t *testing.T) {
	setValidSecrets(t)
	for _, code := range []ErrorCode{CodeStartupReconcileRequired, CodePlannerStalled, CodePersistedRuntimeUncertain, CodeCommitVerificationFailed} {
		t.Run(string(code), func(t *testing.T) {
			s := New(ExecutorFunc(func(_ context.Context, operation Operation, _ Settings) Outcome {
				if operation != OperationModelInfo {
					t.Fatalf("unexpected operation %s", operation)
				}
				return Outcome{State: StateSucceeded}
			}))
			defer s.Shutdown()
			if err := s.Configure(validConfigYAML()); err != nil {
				t.Fatal(err)
			}
			s.blockForTest(code)
			response := s.HandleManagement(managementRequest(http.MethodPost, modelInfoPath, nil))
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("model-info=%d %s", response.StatusCode, response.Body)
			}
			waitForState(t, s, acceptedRunID(t, response), StateSucceeded)
			blocked := s.HandleManagement(managementRequest(http.MethodPost, runMetadataPath, nil))
			if blocked.StatusCode != http.StatusConflict || !jsonBodyHasCode(blocked.Body, CodeWriterBlocked) {
				t.Fatalf("write=%d %s", blocked.StatusCode, blocked.Body)
			}
		})
	}
}

func TestOperationCoalescingAndActiveSettingsSnapshot(t *testing.T) {
	setValidSecrets(t)
	exec := &blockingExecutor{started: make(chan Settings, 1), release: make(chan struct{})}
	s := New(exec)
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	s.ClearBlockForReconcileProof()

	first := s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil))
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first=%d %s", first.StatusCode, first.Body)
	}
	var a triggerResponse
	_ = json.Unmarshal(first.Body, &a)
	active := <-exec.started

	second := s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil))
	var b triggerResponse
	_ = json.Unmarshal(second.Body, &b)
	if second.StatusCode != http.StatusAccepted || a.RunID != b.RunID {
		t.Fatalf("first=%+v second=%+v", a, b)
	}

	raw := replaceLine(string(validConfigYAML()), "sync_epoch: epoch-a", "sync_epoch: epoch-b")
	if err := s.Configure([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if active.SyncEpoch != "epoch-a" {
		t.Fatalf("active snapshot mutated: %q", active.SyncEpoch)
	}
	close(exec.release)
	waitForState(t, s, a.RunID, StateFailed)
}

func TestQueuePersistsAcrossConfigure(t *testing.T) {
	setValidSecrets(t)
	firstStarted := make(chan struct{})
	gate := make(chan struct{})
	var calls atomic.Int32
	exec := ExecutorFunc(func(_ context.Context, _ Operation, settings Settings) Outcome {
		if calls.Add(1) == 1 {
			close(firstStarted)
		}
		<-gate
		return Outcome{Code: CodeNotImplemented, ConfigSHA256: settings.ConfigSHA256}
	})
	s := New(exec)
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	s.ClearBlockForReconcileProof()

	a := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil)))
	<-firstStarted
	b := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runMetadataPath, nil)))
	if err := s.Configure([]byte(replaceLine(string(validConfigYAML()), "sync_epoch: epoch-a", "sync_epoch: epoch-b"))); err != nil {
		t.Fatal(err)
	}
	close(gate)
	waitForState(t, s, a, StateFailed)
	waitForState(t, s, b, StateFailed)
}

func TestManagementStatusSnapshotIsCoherentDuringConfigure(t *testing.T) {
	setValidSecrets(t)
	aRaw := validConfigYAML()
	bRaw := []byte(replaceLine(string(aRaw), "sync_epoch: epoch-a", "sync_epoch: epoch-b"))
	s := New(nil)
	defer s.Shutdown()
	if err := s.Configure(aRaw); err != nil {
		t.Fatal(err)
	}

	type mismatch struct {
		top StatusResponse
		run RunStatus
		err error
	}
	mismatches := make(chan mismatch, 1)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 20_000; i++ {
			raw := aRaw
			if i%2 == 1 {
				raw = bRaw
			}
			if err := s.Configure(raw); err != nil {
				select {
				case mismatches <- mismatch{err: err}:
				default:
				}
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 20_000; i++ {
			response := s.HandleManagement(managementRequest(http.MethodGet, statusPath, nil))
			var top StatusResponse
			if err := json.Unmarshal(response.Body, &top); err != nil {
				select {
				case mismatches <- mismatch{err: err}:
				default:
				}
				return
			}
			for _, run := range top.Runs {
				if run.InstanceID != top.InstanceID || run.ReconfigureSeq != top.ReconfigureSeq || run.ConfigSHA256 != top.ConfigSHA256 {
					select {
					case mismatches <- mismatch{top: top, run: run}:
					default:
					}
					return
				}
			}
		}
	}()
	close(start)
	wg.Wait()
	select {
	case got := <-mismatches:
		if got.err != nil {
			t.Fatal(got.err)
		}
		t.Fatalf("mixed status snapshot: top_seq=%d top_hash=%s run_seq=%d run_hash=%s", got.top.ReconfigureSeq, got.top.ConfigSHA256, got.run.ReconfigureSeq, got.run.ConfigSHA256)
	default:
	}
}

func TestRunIDsOpaque128BitAndStatusLookup(t *testing.T) {
	setValidSecrets(t)
	s := New(nil)
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	id := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, reconcilePath, nil)))
	decoded, err := decodeOpaqueID(id)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("id=%q len=%d err=%v", id, len(decoded), err)
	}

	q := url.Values{"run_id": []string{id}}
	response := s.HandleManagement(managementRequest(http.MethodGet, statusPath, q))
	if response.StatusCode != http.StatusOK || !json.Valid(response.Body) {
		t.Fatalf("status=%d %s", response.StatusCode, response.Body)
	}
}

func TestStatusRetentionCapsCompletedRunsAndPinsBlockerThroughManagement(t *testing.T) {
	setValidSecrets(t)
	s := New(nil)
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	blocker := s.Status().BlockingRunID

	ids := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		id := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, reconcilePath, nil)))
		waitForState(t, s, id, StateFailed)
		ids = append(ids, id)
	}
	queryRun := func(id string) pluginapi.ManagementResponse {
		return s.HandleManagement(managementRequest(http.MethodGet, statusPath, url.Values{"run_id": []string{id}}))
	}
	if got := queryRun(ids[0]); got.StatusCode != http.StatusNotFound {
		t.Fatalf("oldest status=%d %s", got.StatusCode, got.Body)
	}
	if got := queryRun(ids[len(ids)-1]); got.StatusCode != http.StatusOK {
		t.Fatalf("newest status=%d %s", got.StatusCode, got.Body)
	}
	if got := queryRun(blocker); got.StatusCode != http.StatusOK {
		t.Fatalf("blocker status=%d %s", got.StatusCode, got.Body)
	}

	summary := s.HandleManagement(managementRequest(http.MethodGet, statusPath, nil))
	var body StatusResponse
	if summary.StatusCode != http.StatusOK || json.Unmarshal(summary.Body, &body) != nil {
		t.Fatalf("summary=%d %s", summary.StatusCode, summary.Body)
	}
	seen := map[string]bool{}
	for _, status := range body.Runs {
		seen[status.RunID] = true
	}
	if !seen[ids[len(ids)-1]] || !seen[blocker] {
		t.Fatalf("summary omitted latest or blocker: %+v", body.Runs)
	}
}

func TestDeadlinesPreserveAbsoluteTimeAndResetOnlyChangedOwner(t *testing.T) {
	setValidSecrets(t)
	var unix atomic.Int64
	unix.Store(1000)
	now := func() time.Time { return time.Unix(unix.Load(), 0) }
	s := New(nil, WithClock(now))
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	before := s.deadlinesForTest()

	unix.Add(int64((2 * time.Minute) / time.Second))
	epochOnly := replaceLine(string(validConfigYAML()), "sync_epoch: epoch-a", "sync_epoch: epoch-b")
	if err := s.Configure([]byte(epochOnly)); err != nil {
		t.Fatal(err)
	}
	afterEpoch := s.deadlinesForTest()
	if !before[OperationAutoPull].Equal(afterEpoch[OperationAutoPull]) || !before[OperationMetadataSync].Equal(afterEpoch[OperationMetadataSync]) {
		t.Fatalf("deadline moved: before=%v after=%v", before, afterEpoch)
	}

	changed := replaceLine(epochOnly, "auto_pull_interval: 5m", "auto_pull_interval: 10m")
	if err := s.Configure([]byte(changed)); err != nil {
		t.Fatal(err)
	}
	afterChange := s.deadlinesForTest()
	if !afterChange[OperationAutoPull].Equal(now().Add(10 * time.Minute)) {
		t.Fatalf("auto=%v", afterChange[OperationAutoPull])
	}
	if !afterChange[OperationMetadataSync].Equal(before[OperationMetadataSync]) {
		t.Fatalf("metadata reset: %v -> %v", before[OperationMetadataSync], afterChange[OperationMetadataSync])
	}
	if _, ok := afterChange[OperationModelInfo]; ok {
		t.Fatal("disabled deadline retained")
	}
}

func TestTimerAdmissionAndDeadlineAdvanceAreAtomicWithNewBlocker(t *testing.T) {
	setValidSecrets(t)
	for iteration := 0; iteration < 50; iteration++ {
		var unix atomic.Int64
		unix.Store(1000)
		now := func() time.Time { return time.Unix(unix.Load(), 0) }
		executor := &queuedBlockReconciler{started: make(chan Operation, 8), releaseFirst: make(chan struct{}), blockCode: CodePlannerStalled}
		s := New(executor, WithClock(now))
		if err := s.Configure(validConfigYAML()); err != nil {
			s.Shutdown()
			t.Fatal(err)
		}
		s.ClearBlockForReconcileProof()
		autoID := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil)))
		if operation := <-executor.started; operation != OperationAutoPull {
			s.Shutdown()
			t.Fatalf("first operation=%s", operation)
		}
		withoutAutoTimer := []byte(replaceLine(string(validConfigYAML()), "auto_pull_interval: 5m", "auto_pull_interval: 0s"))
		if err := s.Configure(withoutAutoTimer); err != nil {
			s.Shutdown()
			t.Fatal(err)
		}
		deadlineBefore := s.deadlinesForTest()[OperationMetadataSync]
		unix.Store(5000)

		start := make(chan struct{})
		var contenders sync.WaitGroup
		contenders.Add(2)
		go func() {
			defer contenders.Done()
			<-start
			close(executor.releaseFirst)
		}()
		go func() {
			defer contenders.Done()
			<-start
			s.fireExpired()
		}()
		close(start)
		contenders.Wait()
		waitForState(t, s, autoID, StateUncertain)

		s.mu.Lock()
		metadataID := s.activeByOp[OperationMetadataSync]
		deadlineAfter := s.deadlines[OperationMetadataSync]
		s.mu.Unlock()
		if metadataID == "" && !deadlineAfter.Equal(deadlineBefore) {
			s.Shutdown()
			t.Fatalf("iteration %d lost blocked deadline: before=%v after=%v", iteration, deadlineBefore, deadlineAfter)
		}
		if metadataID != "" && !deadlineAfter.After(now()) {
			s.Shutdown()
			t.Fatalf("iteration %d admitted timer without advancing deadline: %v", iteration, deadlineAfter)
		}

		reconcileID := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, reconcilePath, nil)))
		if operation := <-executor.started; operation != OperationReconcile {
			s.Shutdown()
			t.Fatalf("operation=%s", operation)
		}
		waitForState(t, s, reconcileID, StateSucceeded)
		select {
		case operation := <-executor.started:
			if operation != OperationMetadataSync {
				s.Shutdown()
				t.Fatalf("post-reconcile operation=%s", operation)
			}
		case <-time.After(time.Second):
			s.Shutdown()
			t.Fatal("retained timer did not run after reconcile")
		}
		if metadataID == "" {
			for _, status := range s.statusWithRuns().Runs {
				if status.Operation == OperationMetadataSync {
					metadataID = status.RunID
					break
				}
			}
		}
		if metadataID == "" {
			s.Shutdown()
			t.Fatal("metadata status missing after timer run")
		}
		waitForState(t, s, metadataID, StateSucceeded)
		if executor.metadataCalls.Load() != 1 {
			s.Shutdown()
			t.Fatalf("metadata calls=%d", executor.metadataCalls.Load())
		}
		s.Shutdown()
	}
}

func TestSchedulerAdmitsRetainedExpiredDeadlinesOnceAfterUnblock(t *testing.T) {
	setValidSecrets(t)
	var unix atomic.Int64
	unix.Store(1000)
	now := func() time.Time { return time.Unix(unix.Load(), 0) }
	started := make(chan Operation, 1)
	release := make(chan struct{})
	exec := ExecutorFunc(func(_ context.Context, operation Operation, settings Settings) Outcome {
		started <- operation
		<-release
		return Outcome{Code: CodeNotImplemented, ConfigSHA256: settings.ConfigSHA256}
	})
	s := New(exec, WithClock(now))
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		releaseAll()
		s.Shutdown()
	}()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}

	unix.Store(1000 + int64((2*time.Hour)/time.Second))
	blockedDeadlines := s.deadlinesForTest()
	s.fireExpired()
	afterBlockedFire := s.deadlinesForTest()
	if !afterBlockedFire[OperationAutoPull].Equal(blockedDeadlines[OperationAutoPull]) || !afterBlockedFire[OperationMetadataSync].Equal(blockedDeadlines[OperationMetadataSync]) {
		t.Fatalf("blocked write deadlines advanced: before=%v after=%v", blockedDeadlines, afterBlockedFire)
	}
	if got := s.activeRunIDsForTest(); got[OperationAutoPull] != "" || got[OperationMetadataSync] != "" {
		t.Fatalf("blocked write owners admitted: %+v", got)
	}
	s.ClearBlockForReconcileProof()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("overdue work did not start after startup unblock")
	}

	var autoID, metadataID string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		autoID = s.activeByOp[OperationAutoPull]
		metadataID = s.activeByOp[OperationMetadataSync]
		queueLength := len(s.queue)
		s.mu.Unlock()
		if autoID != "" && metadataID != "" {
			if queueLength != 1 {
				t.Fatalf("queue length=%d, want one queued behind active job", queueLength)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if autoID == "" || metadataID == "" {
		t.Fatalf("expired owners not both admitted: auto=%q metadata=%q", autoID, metadataID)
	}
	if got := s.activeRunIDsForTest(); got[OperationModelInfo] != "" {
		t.Fatalf("disabled model-info owner admitted: %q", got[OperationModelInfo])
	}

	s.fireExpired()
	activeAfterRepeat := s.activeRunIDsForTest()
	if activeAfterRepeat[OperationAutoPull] != autoID || activeAfterRepeat[OperationMetadataSync] != metadataID {
		t.Fatalf("repeat fire duplicated or replaced runs: %+v", activeAfterRepeat)
	}
	deadlines := s.deadlinesForTest()
	if want := time.Unix(8500, 0); !deadlines[OperationAutoPull].Equal(want) {
		t.Fatalf("auto deadline=%v want=%v", deadlines[OperationAutoPull], want)
	}
	if want := time.Unix(11800, 0); !deadlines[OperationMetadataSync].Equal(want) {
		t.Fatalf("metadata deadline=%v want=%v", deadlines[OperationMetadataSync], want)
	}

	releaseAll()
	waitForState(t, s, autoID, StateFailed)
	waitForState(t, s, metadataID, StateFailed)
}

func TestBlockedSchedulerRunsModelInfoAndRetainsWriteDeadline(t *testing.T) {
	setValidSecrets(t)
	var unix atomic.Int64
	unix.Store(1000)
	now := func() time.Time { return time.Unix(unix.Load(), 0) }
	started := make(chan Operation, 4)
	s := New(ExecutorFunc(func(_ context.Context, operation Operation, _ Settings) Outcome {
		started <- operation
		return Outcome{State: StateSucceeded}
	}), WithClock(now))
	defer s.Shutdown()
	config := []byte(replaceLine(string(validConfigYAML()), "model_info_interval: 0s", "model_info_interval: 5m"))
	if err := s.Configure(config); err != nil {
		t.Fatal(err)
	}
	unix.Store(1400)
	s.fireExpired()
	select {
	case operation := <-started:
		if operation != OperationModelInfo {
			t.Fatalf("blocked scheduler started %s", operation)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked scheduler did not run read-only model-info")
	}
	deadlines := s.deadlinesForTest()
	if want := time.Unix(1300, 0); !deadlines[OperationAutoPull].Equal(want) {
		t.Fatalf("blocked write deadline=%v want=%v", deadlines[OperationAutoPull], want)
	}
	if !deadlines[OperationModelInfo].After(now()) {
		t.Fatalf("model-info deadline did not advance: %v", deadlines[OperationModelInfo])
	}
	s.ClearBlockForReconcileProof()
	select {
	case operation := <-started:
		if operation != OperationAutoPull {
			t.Fatalf("unblock started %s before overdue write", operation)
		}
	case <-time.After(time.Second):
		t.Fatal("unblock did not admit retained overdue write")
	}
}

type reportingExecutor struct{}

func (*reportingExecutor) Execute(context.Context, Operation, Settings) Outcome {
	return Outcome{State: StateFailed, Code: CodeNotImplemented}
}

func (*reportingExecutor) ExecuteWithProgress(_ context.Context, _ Operation, _ Settings, progress func(int, RunState)) Outcome {
	progress(2, StateCommitting)
	progress(2, StateWaiting)
	return Outcome{State: StateSucceeded, Version: configVersion([]byte("done")), Changed: true}
}

func TestServicePublishesExecutorAttemptAndPhaseProgress(t *testing.T) {
	setValidSecrets(t)
	s := New(&reportingExecutor{})
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	s.ClearBlockForReconcileProof()
	id := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil)))
	got := waitForState(t, s, id, StateSucceeded)
	if got.Attempt != 2 || got.Version != configVersion([]byte("done")) || !got.Changed {
		t.Fatalf("status=%+v", got)
	}
}

func TestTypedFoundationExecutorOutcome(t *testing.T) {
	setValidSecrets(t)
	s := New(nil)
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	s.ClearBlockForReconcileProof()
	id := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runMetadataPath, nil)))
	got := waitForState(t, s, id, StateFailed)
	if got.ErrorCode != CodeNotImplemented {
		t.Fatalf("status=%+v", got)
	}
}

func TestNewServicePreservesCommitIdentityAcrossConfigure(t *testing.T) {
	setValidSecrets(t)
	s := NewService()
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	first := s.executor.(*WriterExecutor).engine
	instance := s.Status().InstanceID
	if err := s.Configure([]byte(replaceLine(string(validConfigYAML()), "sync_epoch: epoch-a", "sync_epoch: epoch-b"))); err != nil {
		t.Fatal(err)
	}
	if s.executor.(*WriterExecutor).engine != first || s.Status().InstanceID != instance {
		t.Fatal("Configure replaced process-local commit identity")
	}
}

func TestUncertainOutcomeBlocksLaterWritesUntilReconcile(t *testing.T) {
	setValidSecrets(t)
	executor := ExecutorFunc(func(context.Context, Operation, Settings) Outcome {
		return Outcome{State: StateUncertain, Code: CodePersistedRuntimeUncertain, Version: configVersion([]byte("persisted")), Block: BlockRecord{Version: configVersion([]byte("persisted"))}}
	})
	s := New(executor)
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	s.ClearBlockForReconcileProof()
	id := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil)))
	waitForState(t, s, id, StateUncertain)
	blocked := s.HandleManagement(managementRequest(http.MethodPost, runMetadataPath, nil))
	if blocked.StatusCode != http.StatusConflict || !s.Status().WriterBlocked {
		t.Fatalf("blocked=%d %s status=%+v", blocked.StatusCode, blocked.Body, s.Status())
	}
}

type successfulUncertaintyReconciler struct{}

func (*successfulUncertaintyReconciler) Execute(context.Context, Operation, Settings) Outcome {
	version := configVersion([]byte("uncertain"))
	return Outcome{State: StateUncertain, Code: CodeCommitVerificationFailed, Version: version, Changed: true, Block: BlockRecord{Version: version}}
}

func (*successfulUncertaintyReconciler) ReconcileAndClear(_ context.Context, block BlockRecord, _ Settings, clear func(BlockRecord, string) bool) ReconcileResult {
	if !clear(block, block.Version) {
		return ReconcileResult{Code: CodeReconcileFailed}
	}
	return ReconcileResult{Cleared: true, Version: block.Version}
}

func TestSuccessfulReconcileRetainsFormerBlockingRun(t *testing.T) {
	setValidSecrets(t)
	s := New(&successfulUncertaintyReconciler{})
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	s.ClearBlockForReconcileProof()
	blockedRun := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil)))
	waitForState(t, s, blockedRun, StateUncertain)
	reconcileRun := acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, reconcilePath, nil)))
	waitForState(t, s, reconcileRun, StateSucceeded)
	if got := s.statusByID(blockedRun); got == nil || got.State != StateUncertain {
		t.Fatalf("former blocker was not retained: %+v", got)
	}
	summary := s.statusWithRuns()
	found := false
	for _, status := range summary.Runs {
		if status.RunID == blockedRun {
			found = true
		}
	}
	if !found {
		t.Fatalf("former blocker missing from latest operation summary: %+v", summary.Runs)
	}
}

func TestShutdownCancelsActiveExecutorAndJoinsScheduler(t *testing.T) {
	setValidSecrets(t)
	started := make(chan struct{})
	exec := ExecutorFunc(func(ctx context.Context, _ Operation, _ Settings) Outcome {
		close(started)
		<-ctx.Done()
		return Outcome{State: StateFailed, Code: CodeNotImplemented}
	})
	s := New(exec)
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	s.ClearBlockForReconcileProof()
	acceptedRunID(t, s.HandleManagement(managementRequest(http.MethodPost, runAutoPullPath, nil)))
	<-started

	done := make(chan struct{})
	go func() {
		s.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel and join runtime goroutines")
	}
	select {
	case <-s.schedulerDone:
	default:
		t.Fatal("scheduler goroutine was not joined")
	}
}

func managementRequest(method, path string, query url.Values) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: method, Path: path, Query: query}
}

func acceptedRunID(t *testing.T, response pluginapi.ManagementResponse) string {
	t.Helper()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("response=%d %s", response.StatusCode, response.Body)
	}
	var body triggerResponse
	if err := json.Unmarshal(response.Body, &body); err != nil || body.RunID == "" {
		t.Fatalf("body=%s err=%v", response.Body, err)
	}
	return body.RunID
}

func waitForState(t *testing.T, s *Service, id string, state RunState) RunStatus {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := s.statusByID(id); got != nil && got.State == state {
			return *got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", id, state)
	return RunStatus{}
}

func jsonBodyHasCode(body []byte, code ErrorCode) bool {
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	return got["error_code"] == string(code)
}

func mustOpaqueID(t *testing.T) string {
	t.Helper()
	id, err := newOpaqueID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
