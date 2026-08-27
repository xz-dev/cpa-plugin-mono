package plugin

import "time"

func (s *Service) deadlinesForTest() map[Operation]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Operation]time.Time, len(s.deadlines))
	for operation, deadline := range s.deadlines {
		out[operation] = deadline
	}
	return out
}

func (s *Service) activeRunIDsForTest() map[Operation]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Operation]string, len(s.activeByOp))
	for operation, runID := range s.activeByOp {
		out[operation] = runID
	}
	return out
}

func (s *Service) blockForTest(code ErrorCode) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocker != nil {
		delete(s.statuses, s.blocker.RunID)
	}
	status := &RunStatus{RunID: mustNewOpaqueID(), Operation: OperationReconcile, State: StateBlocked, ErrorCode: code, QueuedAt: s.now(), InstanceID: s.instanceID}
	s.blocker = status
	s.blockRecord = BlockRecord{ID: status.RunID, Code: code}
	s.statuses[status.RunID] = status
	return status.RunID
}

func (s *Service) blockRecordForTest() BlockRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blockRecord
}

func (s *Service) replaceBlockForTest(code ErrorCode, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocker != nil {
		delete(s.statuses, s.blocker.RunID)
	}
	status := &RunStatus{RunID: mustNewOpaqueID(), Operation: OperationReconcile, State: StateBlocked, ErrorCode: code, Version: version, QueuedAt: s.now(), InstanceID: s.instanceID}
	s.blocker = status
	s.blockRecord = BlockRecord{ID: status.RunID, Code: code, Version: version}
	s.statuses[status.RunID] = status
}
