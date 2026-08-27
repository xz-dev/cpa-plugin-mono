package plugin

import (
	"crypto/subtle"
	"sync"
)

type Service struct {
	mu             sync.Mutex
	host           AuthHost
	cfg            runtimeConfig
	configured     bool
	instanceID     string
	reconfigureSeq uint64
	activePlans    uint64
	currentAttempt string
	pendingRequest string
}

func New(host AuthHost) *Service {
	instanceID, err := opaqueID()
	if err != nil {
		panic("crypto/rand unavailable")
	}
	return &Service{host: host, instanceID: instanceID}
}

func (s *Service) Configure(pluginYAML []byte) error {
	cfg, err := parseConfig(pluginYAML)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.configured = true
	s.reconfigureSeq++
	s.currentAttempt = ""
	s.pendingRequest = ""
	s.mu.Unlock()
	return nil
}

func (s *Service) beginAuthorizedPlan(token string) (runtimeConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorizedLocked(token) {
		return runtimeConfig{}, false
	}
	cfg := s.cfg
	cfg.Channels = append([]compiledChannel(nil), s.cfg.Channels...)
	cfg.Generation = s.reconfigureSeq
	s.activePlans++
	return cfg, true
}

func (s *Service) authorized(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorizedLocked(token)
}

func (s *Service) authorizedLocked(token string) bool {
	expected := s.cfg.WorkerToken
	return s.configured && len(token) == len(expected) && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func (s *Service) startPlannerAttempt(generation uint64) (string, bool) {
	attemptID, err := opaqueID()
	if err != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.configured || s.reconfigureSeq != generation {
		return "", false
	}
	s.currentAttempt = attemptID
	s.pendingRequest = ""
	return attemptID, true
}

func (s *Service) consumePlannerStep(requestID string, generation uint64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.configured || s.reconfigureSeq != generation || s.currentAttempt == "" || s.pendingRequest != requestID {
		return "", false
	}
	s.pendingRequest = ""
	return s.currentAttempt, true
}

func (s *Service) registerPlannerStep(attemptID, requestID string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.configured || s.reconfigureSeq != generation || s.currentAttempt != attemptID || s.pendingRequest != "" {
		return false
	}
	s.pendingRequest = requestID
	return true
}

func (s *Service) completePlannerAttempt(attemptID string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.configured || s.reconfigureSeq != generation || s.currentAttempt != attemptID || s.pendingRequest != "" {
		return false
	}
	s.currentAttempt = ""
	return true
}

func (s *Service) abandonPlannerAttempt(attemptID string, generation uint64) {
	s.mu.Lock()
	if s.reconfigureSeq == generation && s.currentAttempt == attemptID {
		s.currentAttempt = ""
		s.pendingRequest = ""
	}
	s.mu.Unlock()
}

func (s *Service) endPlan() {
	s.mu.Lock()
	if s.activePlans > 0 {
		s.activePlans--
	}
	s.mu.Unlock()
}

func (s *Service) authorizedWorkerStatus(token string) (WorkerStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorizedLocked(token) {
		return WorkerStatus{}, false
	}
	return WorkerStatus{InstanceID: s.instanceID, ReconfigureSeq: s.reconfigureSeq, ConfigSHA256: s.cfg.SHA256, ActivePlan: s.activePlans > 0}, true
}

func (s *Service) Shutdown() {
	s.mu.Lock()
	s.cfg = runtimeConfig{}
	s.configured = false
	s.currentAttempt = ""
	s.pendingRequest = ""
	s.mu.Unlock()
}

type WorkerStatus struct {
	InstanceID     string `json:"instance_id"`
	ReconfigureSeq uint64 `json:"reconfigure_seq"`
	ConfigSHA256   string `json:"config_sha256"`
	ActivePlan     bool   `json:"active_plan"`
}
