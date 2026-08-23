package plugin

import (
	"strings"
	"sync"
	"time"
)

type Service struct {
	mu        sync.Mutex
	cfg       runtimeConfig
	jsonPath  string
	transport Transport
	stop      chan struct{}
	last      SyncReport
	// enriched holds the per-provider registry metadata (limits/thinking/
	// modalities) served through model.static / model.for_auth.
	enriched map[string][]enrichedModel
}

func New(t Transport) *Service {
	return &Service{transport: t, enriched: map[string][]enrichedModel{}}
}

func (s *Service) Configure(pluginYAML []byte) error {
	path := resolveJSONPath(pluginYAML)
	cfg, raw, err := loadJSONFile(path)
	if err != nil {
		return err
	}
	if raw == nil {
		if err := writeJSONFile(path, defaultFileConfig()); err != nil {
			return err
		}
		cfg, _, err = loadJSONFile(path)
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.cfg = cfg
	s.jsonPath = path
	s.enriched = map[string][]enrichedModel{}
	s.mu.Unlock()
	s.restartTicker()
	return nil
}

func (s *Service) JSONPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jsonPath
}

func (s *Service) Current() runtimeConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Service) Last() SyncReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Service) SetLast(report SyncReport) {
	s.mu.Lock()
	s.last = report
	s.mu.Unlock()
}

func (s *Service) SaveJSON(raw []byte) error {
	cfg, err := parseFileConfig(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	path := s.jsonPath
	s.mu.Unlock()
	if err := writeJSONFile(path, cfg.Raw); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	s.restartTicker()
	return nil
}

func (s *Service) SyncWithKey(key, only string) SyncReport {
	report := s.Sync(key, only)
	s.SetLast(report)
	return report
}

func (s *Service) PreviewWithKey(key, only string, raw []byte) (SyncReport, error) {
	var override *runtimeConfig
	if len(strings.TrimSpace(string(raw))) > 0 {
		cfg, err := parseFileConfig(raw)
		if err != nil {
			return SyncReport{}, err
		}
		override = &cfg
	}
	return s.Preview(key, only, override), nil
}

func (s *Service) restartTicker() {
	s.mu.Lock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	interval := s.cfg.Interval
	s.mu.Unlock()
	if interval <= 0 {
		return
	}
	stop := make(chan struct{})
	s.mu.Lock()
	s.stop = stop
	s.mu.Unlock()
	go s.loop(interval, stop)
}

func (s *Service) loop(interval time.Duration, stop chan struct{}) {
	// First tick fires immediately so the enriched snapshot is warm at boot.
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			s.mu.Lock()
			cfg := s.cfg
			s.mu.Unlock()
			if key := resolveManagementKey(cfg); key != "" {
				s.SyncWithKey(key, "")
			}
			next := cfg.Interval
			if next <= 0 {
				return
			}
			timer.Reset(next)
		}
	}
}

func (s *Service) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
}
