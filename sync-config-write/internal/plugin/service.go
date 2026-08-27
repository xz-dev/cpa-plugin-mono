package plugin

import (
	"context"
	"sync"
	"time"
)

type Executor interface {
	Execute(context.Context, Operation, Settings) Outcome
}

type ProgressExecutor interface {
	ExecuteWithProgress(context.Context, Operation, Settings, func(int, RunState)) Outcome
}

type ExecutorFunc func(context.Context, Operation, Settings) Outcome

func (f ExecutorFunc) Execute(ctx context.Context, op Operation, settings Settings) Outcome {
	return f(ctx, op, settings)
}

type Option func(*Service)

func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }
func withAutomaticStartupReconcile() Option {
	return func(s *Service) { s.automaticStartupReconcile = true }
}

type job struct {
	runID     string
	operation Operation
	settings  Settings
}

type Service struct {
	mu                        sync.Mutex
	queueCond                 *sync.Cond
	settings                  Settings
	configured                bool
	instanceID                string
	reconfigureSeq            uint64
	executor                  Executor
	now                       func() time.Time
	queue                     []job
	ctx                       context.Context
	cancel                    context.CancelFunc
	stop                      chan struct{}
	done                      chan struct{}
	schedulerDone             chan struct{}
	shutdownOnce              sync.Once
	statuses                  map[string]*RunStatus
	activeByOp                map[Operation]string
	completed                 []string
	blocker                   *RunStatus
	blockRecord               BlockRecord
	deadlines                 map[Operation]time.Time
	wakeScheduler             chan struct{}
	automaticStartupReconcile bool
}

func NewService(options ...Option) *Service {
	var service *Service
	client := NewLoopbackClient()
	settings := func() Settings {
		if service == nil {
			return Settings{}
		}
		return service.SettingsSnapshot()
	}
	localStatus := func() WorkerStatus {
		if service == nil {
			return WorkerStatus{}
		}
		return service.WorkerStatus()
	}
	workers := NewWorkerStatusClient(client)
	engine := NewCommitEngine(client, workers, settings, localStatus)
	service = New(NewWriterExecutor(NewHTTPPlanner(client), engine), append(options, withAutomaticStartupReconcile())...)
	return service
}

func New(executor Executor, options ...Option) *Service {
	if executor == nil {
		executor = ExecutorFunc(func(context.Context, Operation, Settings) Outcome {
			return Outcome{State: StateFailed, Code: CodeNotImplemented}
		})
	}
	instanceID, err := newOpaqueID()
	if err != nil {
		panic("crypto/rand unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		instanceID: instanceID, executor: executor, now: time.Now,
		ctx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{}), schedulerDone: make(chan struct{}),
		statuses: make(map[string]*RunStatus), activeByOp: make(map[Operation]string),
		deadlines: make(map[Operation]time.Time), wakeScheduler: make(chan struct{}, 1),
	}
	s.queueCond = sync.NewCond(&s.mu)
	for _, option := range options {
		option(s)
	}
	s.blocker = &RunStatus{RunID: mustNewOpaqueID(), Operation: OperationReconcile, State: StateBlocked, ErrorCode: CodeStartupReconcileRequired, QueuedAt: s.now(), InstanceID: instanceID}
	s.blockRecord = BlockRecord{ID: s.blocker.RunID, Code: CodeStartupReconcileRequired, Restarted: true, EvidenceCaptured: s.now()}
	s.statuses[s.blocker.RunID] = s.blocker
	go s.worker()
	go s.scheduler()
	return s
}

func mustNewOpaqueID() string {
	id, err := newOpaqueID()
	if err != nil {
		panic("crypto/rand unavailable")
	}
	return id
}

func (s *Service) Configure(configYAML []byte) error {
	next, err := parseSettings(configYAML)
	if err != nil {
		return err
	}
	return s.configureSettings(next)
}

func (s *Service) configureSettings(next Settings) error {
	now := s.now()
	s.mu.Lock()
	previous := s.settings
	wasConfigured := s.configured
	s.settings = next
	s.configured = true
	s.reconfigureSeq++
	s.updateDeadlineLocked(OperationAutoPull, previous.AutoPullInterval, next.AutoPullInterval, wasConfigured, now)
	s.updateDeadlineLocked(OperationMetadataSync, previous.MetadataSyncInterval, next.MetadataSyncInterval, wasConfigured, now)
	s.updateDeadlineLocked(OperationModelInfo, previous.ModelInfoInterval, next.ModelInfoInterval, wasConfigured, now)
	s.refreshStatusIdentityLocked()
	s.mu.Unlock()
	s.wake()
	if !wasConfigured && s.automaticStartupReconcile {
		_, _, _ = s.enqueue(OperationReconcile)
	}
	return nil
}

func (s *Service) updateDeadlineLocked(op Operation, old, next time.Duration, existed bool, now time.Time) {
	if next == 0 {
		delete(s.deadlines, op)
		return
	}
	if !existed || old != next {
		s.deadlines[op] = now.Add(next)
	}
}

func (s *Service) SettingsSnapshot() Settings { return s.settingsSnapshot() }

func (s *Service) WorkerStatus() WorkerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WorkerStatus{InstanceID: s.instanceID, ReconfigureSeq: s.reconfigureSeq, ConfigSHA256: s.settings.ConfigSHA256}
}

func (s *Service) settingsSnapshot() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// ClearBlockForReconcileProof is retained for foundation tests. Runtime reconcile
// uses matching blocker/version evidence in finishReconcile.
func (s *Service) ClearBlockForReconcileProof() {
	s.mu.Lock()
	s.clearBlockLocked(false)
	s.mu.Unlock()
	s.wake()
}

func (s *Service) Status() StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Service) statusWithRuns() StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	response := s.statusLocked()
	response.Runs = s.currentStatusesLocked()
	return response
}

func (s *Service) statusLocked() StatusResponse {
	response := StatusResponse{InstanceID: s.instanceID, ReconfigureSeq: s.reconfigureSeq, ConfigSHA256: s.settings.ConfigSHA256}
	if s.blocker != nil {
		response.WriterBlocked = true
		response.BlockingRunID = s.blocker.RunID
		response.ErrorCode = s.blocker.ErrorCode
	}
	return response
}

func (s *Service) enqueue(op Operation) (string, bool, ErrorCode) {
	s.mu.Lock()
	if op != OperationReconcile && s.blocker != nil {
		blockingRunID, code := s.blocker.RunID, s.blocker.ErrorCode
		s.mu.Unlock()
		return blockingRunID, false, code
	}
	if id := s.activeByOp[op]; id != "" {
		s.mu.Unlock()
		return id, true, ""
	}
	id, err := newOpaqueID()
	if err != nil {
		s.mu.Unlock()
		return "", false, CodeNotImplemented
	}
	status := &RunStatus{RunID: id, Operation: op, State: StateQueued, Attempt: 0, QueuedAt: s.now(), InstanceID: s.instanceID, ReconfigureSeq: s.reconfigureSeq, ConfigSHA256: s.settings.ConfigSHA256}
	s.statuses[id] = status
	s.activeByOp[op] = id
	item := job{runID: id, operation: op, settings: s.settings}
	s.queue = append(s.queue, item)
	s.queueCond.Signal()
	s.mu.Unlock()
	return id, false, ""
}

func (s *Service) worker() {
	defer close(s.done)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.isStoppedLocked() {
			s.queueCond.Wait()
		}
		if s.isStoppedLocked() {
			s.mu.Unlock()
			return
		}
		item := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		s.updateRun(item.runID, func(status *RunStatus) {
			status.Attempt = 1
			status.StartedAt = s.now()
			if item.operation == OperationReconcile {
				status.State = StateReconciling
			} else {
				status.State = StatePlanning
			}
		})
		var outcome Outcome
		if item.operation == OperationReconcile {
			s.mu.Lock()
			block := s.blockRecord
			s.mu.Unlock()
			if executor, ok := s.executor.(AtomicReconcileExecutor); ok {
				result := executor.ReconcileAndClear(s.ctx, block, item.settings, s.clearMatchingBlock)
				s.finishAtomicReconcile(item, result)
				continue
			}
			if executor, ok := s.executor.(ReconcileExecutor); ok {
				result := executor.Reconcile(s.ctx, block, item.settings)
				s.finishReconcile(item, block, result)
				continue
			}
		}
		if executor, ok := s.executor.(ProgressExecutor); ok {
			outcome = executor.ExecuteWithProgress(s.ctx, item.operation, item.settings, func(attempt int, state RunState) {
				s.updateRun(item.runID, func(status *RunStatus) {
					if attempt > 0 {
						status.Attempt = attempt
					}
					if state != "" {
						status.State = state
					}
				})
			})
		} else {
			outcome = s.executor.Execute(s.ctx, item.operation, item.settings)
		}
		s.finish(item, outcome)
	}
}

func (s *Service) isStoppedLocked() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *Service) clearMatchingBlock(expected BlockRecord, version string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocker == nil || s.blockRecord.ID != expected.ID || s.blockRecord.Code != expected.Code || s.blockRecord.Version != expected.Version || (expected.Version != "" && version != expected.Version) {
		return false
	}
	s.clearBlockLocked(true)
	return true
}

func (s *Service) clearBlockLocked(retainUncertain bool) {
	blocked := s.blocker
	s.blocker = nil
	s.blockRecord = BlockRecord{}
	if blocked == nil {
		return
	}
	if retainUncertain && blocked.State == StateUncertain {
		s.retainCompletedLocked(blocked)
		return
	}
	delete(s.statuses, blocked.RunID)
}

func (s *Service) finishAtomicReconcile(item job, result ReconcileResult) {
	s.mu.Lock()
	status := s.statuses[item.runID]
	if status != nil {
		if result.Cleared {
			status.State, status.ErrorCode, status.Version = StateSucceeded, "", result.Version
		} else {
			status.State, status.ErrorCode = StateFailed, CodeReconcileFailed
		}
		status.FinishedAt = s.now()
		delete(s.activeByOp, item.operation)
		s.retainCompletedLocked(status)
	}
	s.mu.Unlock()
	if result.Cleared {
		s.wake()
	}
}

func (s *Service) finishReconcile(item job, expected BlockRecord, result ReconcileResult) {
	s.mu.Lock()
	status := s.statuses[item.runID]
	if status == nil {
		s.mu.Unlock()
		return
	}
	if !result.Cleared || s.blocker == nil || s.blockRecord.ID != expected.ID || s.blockRecord.Code != expected.Code || s.blockRecord.Version != expected.Version || (expected.Version != "" && result.Version != expected.Version) {
		status.State, status.ErrorCode = StateFailed, CodeReconcileFailed
	} else {
		s.clearBlockLocked(true)
		status.State, status.ErrorCode, status.Version = StateSucceeded, "", result.Version
	}
	status.FinishedAt = s.now()
	delete(s.activeByOp, item.operation)
	s.retainCompletedLocked(status)
	s.mu.Unlock()
	s.wake()
}

func (s *Service) finish(item job, outcome Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.statuses[item.runID]
	if status == nil {
		return
	}
	if outcome.State != "" {
		status.State = outcome.State
	} else if outcome.Code != "" {
		status.State = StateFailed
	} else {
		status.State = StateSucceeded
	}
	status.ErrorCode, status.Version, status.Changed = outcome.Code, outcome.Version, outcome.Changed
	if outcome.Code == CodePlannerStalled || outcome.Code == CodePersistedRuntimeUncertain || outcome.Code == CodeCommitVerificationFailed {
		status.State = StateUncertain
		s.blocker = status
		s.blockRecord = outcome.Block
		s.blockRecord.ID, s.blockRecord.Code = status.RunID, outcome.Code
		if s.blockRecord.EvidenceCaptured.IsZero() {
			s.blockRecord.EvidenceCaptured = s.now()
		}
	}
	if outcome.ConfigSHA256 != "" {
		status.ConfigSHA256 = outcome.ConfigSHA256
	}
	status.FinishedAt = s.now()
	delete(s.activeByOp, item.operation)
	s.retainCompletedLocked(status)
}

func (s *Service) updateRun(id string, update func(*RunStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status := s.statuses[id]; status != nil {
		update(status)
	}
}

func (s *Service) retainCompletedLocked(status *RunStatus) {
	if s.blocker != nil && status.RunID == s.blocker.RunID {
		return
	}
	for _, id := range s.completed {
		if id == status.RunID {
			return
		}
	}
	s.completed = append(s.completed, status.RunID)
	for len(s.completed) > 32 {
		delete(s.statuses, s.completed[0])
		s.completed = s.completed[1:]
	}
}

func (s *Service) statusByID(id string) *RunStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.statuses[id]
	if status == nil {
		return nil
	}
	copy := *status
	copy.InstanceID, copy.ReconfigureSeq, copy.ConfigSHA256 = s.instanceID, s.reconfigureSeq, s.settings.ConfigSHA256
	return &copy
}

func (s *Service) currentStatusesLocked() []RunStatus {
	result := make([]RunStatus, 0, 5)
	seen := make(map[string]bool)
	for _, operation := range []Operation{OperationAutoPull, OperationMetadataSync, OperationModelInfo, OperationReconcile} {
		if id := s.activeByOp[operation]; id != "" {
			if status := s.statuses[id]; status != nil {
				result = append(result, *status)
				seen[id] = true
				continue
			}
		}
		for i := len(s.completed) - 1; i >= 0; i-- {
			status := s.statuses[s.completed[i]]
			if status != nil && status.Operation == operation {
				result = append(result, *status)
				seen[status.RunID] = true
				break
			}
		}
	}
	if s.blocker != nil && !seen[s.blocker.RunID] {
		result = append(result, *s.blocker)
	}
	for i := range result {
		result[i].InstanceID, result[i].ReconfigureSeq, result[i].ConfigSHA256 = s.instanceID, s.reconfigureSeq, s.settings.ConfigSHA256
	}
	return result
}

func (s *Service) refreshStatusIdentityLocked() {
	for _, status := range s.statuses {
		status.InstanceID, status.ReconfigureSeq, status.ConfigSHA256 = s.instanceID, s.reconfigureSeq, s.settings.ConfigSHA256
	}
}

func (s *Service) scheduler() {
	defer close(s.schedulerDone)
	for {
		next, wait := s.nextDeadline()
		var timer *time.Timer
		var timerC <-chan time.Time
		if wait {
			duration := next.Sub(s.now())
			if duration < 0 {
				duration = 0
			}
			timer = time.NewTimer(duration)
			timerC = timer.C
		}
		fired := false
		select {
		case <-s.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.wakeScheduler:
		case <-timerC:
			fired = true
		}
		if timer != nil && !fired {
			timer.Stop()
		}
		if fired {
			s.fireExpired()
		}
	}
}

func (s *Service) nextDeadline() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocker != nil {
		return time.Time{}, false
	}
	var next time.Time
	for _, deadline := range s.deadlines {
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	return next, !next.IsZero()
}

func (s *Service) fireExpired() {
	now := s.now()
	var operations []Operation
	s.mu.Lock()
	for op, deadline := range s.deadlines {
		if !deadline.After(now) {
			operations = append(operations, op)
			interval := intervalForOperation(s.settings, op)
			if interval == 0 {
				delete(s.deadlines, op)
			} else {
				s.deadlines[op] = deadline.Add(interval)
				for !s.deadlines[op].After(now) {
					s.deadlines[op] = s.deadlines[op].Add(interval)
				}
			}
		}
	}
	s.mu.Unlock()
	for _, op := range operations {
		_, _, _ = s.enqueue(op)
	}
}

func intervalForOperation(settings Settings, op Operation) time.Duration {
	switch op {
	case OperationAutoPull:
		return settings.AutoPullInterval
	case OperationMetadataSync:
		return settings.MetadataSyncInterval
	case OperationModelInfo:
		return settings.ModelInfoInterval
	}
	return 0
}

func (s *Service) wake() {
	select {
	case s.wakeScheduler <- struct{}{}:
	default:
	}
}

func (s *Service) Shutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		close(s.stop)
		s.cancel()
		for _, item := range s.queue {
			if status := s.statuses[item.runID]; status != nil {
				status.State = StateFailed
				status.ErrorCode = CodeNotImplemented
				status.FinishedAt = s.now()
				s.retainCompletedLocked(status)
			}
			delete(s.activeByOp, item.operation)
		}
		s.queue = nil
		s.queueCond.Broadcast()
		s.mu.Unlock()
		<-s.done
		<-s.schedulerDone
	})
}
