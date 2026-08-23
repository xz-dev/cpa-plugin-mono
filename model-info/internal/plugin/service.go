package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Transport interface {
	Do(method, url string, headers http.Header, body []byte) (status int, resp []byte, err error)
}

// ModelRow is one model as shown in the viewer.
type ModelRow struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Context     int      `json:"context_window,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Levels      []string `json:"reasoning_levels,omitempty"`
	Input       []string `json:"input_modalities,omitempty"`
	Output      []string `json:"output_modalities,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
}

type Catalog struct {
	At     time.Time  `json:"at"`
	Count  int        `json:"count"`
	Models []ModelRow `json:"models"`
	Error  string     `json:"error,omitempty"`
}

type Service struct {
	mu        sync.Mutex
	cfg       Config
	transport Transport
	last      Catalog
}

type Config struct {
	ManagementBaseURL string `json:"management_base_url"`
	ManagementKeyEnv  string `json:"management_key_env"`
	ManagementKeyFile string `json:"management_key_file"`
}

func New(t Transport) *Service {
	return &Service{transport: t}
}

func (s *Service) Configure(pluginYAML []byte) error {
	cfg := Config{ManagementBaseURL: "http://127.0.0.1:8317"}
	var wrapper struct {
		ManagementBaseURL string `json:"management_base_url"`
		ManagementKeyEnv  string `json:"management_key_env"`
		ManagementKeyFile string `json:"management_key_file"`
	}
	_ = json.Unmarshal(pluginYAML, &wrapper)
	if strings.TrimSpace(wrapper.ManagementBaseURL) != "" {
		cfg.ManagementBaseURL = strings.TrimRight(strings.TrimSpace(wrapper.ManagementBaseURL), "/")
	}
	cfg.ManagementKeyEnv = strings.TrimSpace(wrapper.ManagementKeyEnv)
	cfg.ManagementKeyFile = strings.TrimSpace(wrapper.ManagementKeyFile)
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return nil
}

func (s *Service) Last() Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Service) setLast(c Catalog) {
	s.mu.Lock()
	s.last = c
	s.mu.Unlock()
}

// Fetch pulls the Codex-client catalog from the local CPA via the management
// api-call proxy, so no API key is exposed to the browser.
func (s *Service) Fetch() Catalog {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	catalog := Catalog{At: time.Now().UTC()}
	key := s.resolveKey(cfg)
	if key == "" {
		catalog.Error = "management key unavailable (set management_key_file/env)"
		return catalog
	}

	req, _ := json.Marshal(map[string]any{
		"method": http.MethodGet,
		"url":    cfg.ManagementBaseURL + "/v1/models?client_version=1.0.0",
	})
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Content-Type", "application/json")
	status, body, err := s.transport.Do(http.MethodPost, cfg.ManagementBaseURL+"/v0/management/api-call", headers, req)
	if err != nil {
		catalog.Error = err.Error()
		return catalog
	}
	if status < 200 || status >= 300 {
		catalog.Error = fmt.Sprintf("api-call HTTP %d: %s", status, truncate(body, 200))
		return catalog
	}
	var parsed struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		catalog.Error = "decode api-call: " + err.Error()
		return catalog
	}
	if parsed.StatusCode < 200 || parsed.StatusCode >= 300 {
		catalog.Error = fmt.Sprintf("catalog HTTP %d", parsed.StatusCode)
		return catalog
	}
	models, err := parseCatalog([]byte(parsed.Body))
	if err != nil {
		catalog.Error = err.Error()
		return catalog
	}
	catalog.Models = models
	catalog.Count = len(models)
	return catalog
}

// FetchAndCache refreshes and stores the last successful-or-not result.
func (s *Service) FetchAndCache() Catalog {
	c := s.Fetch()
	s.setLast(c)
	return c
}

func parseCatalog(raw []byte) ([]ModelRow, error) {
	var payload struct {
		Models []struct {
			ID     string `json:"id"`
			Slug   string `json:"slug"`
			Name   string `json:"display_name"`
			Ctx    int    `json:"context_window"`
			Max    int    `json:"max_tokens"`
			Vis    string `json:"visibility"`
			Levels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			In  []string `json:"input_modalities"`
			Out []string `json:"output_modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if payload.Models == nil {
		return nil, fmt.Errorf("not a Codex model catalog")
	}
	out := make([]ModelRow, 0, len(payload.Models))
	for _, m := range payload.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.Slug)
		}
		if id == "" {
			continue
		}
		row := ModelRow{
			ID:          id,
			Slug:        m.Slug,
			DisplayName: m.Name,
			Context:     m.Ctx,
			MaxTokens:   m.Max,
			Visibility:  m.Vis,
			Input:       m.In,
			Output:      m.Out,
		}
		if i := strings.Index(id, "/"); i > 0 {
			row.Provider = id[:i]
		}
		for _, l := range m.Levels {
			row.Levels = append(row.Levels, l.Effort)
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Service) resolveKey(cfg Config) string {
	if cfg.ManagementKeyFile != "" {
		if raw, err := os.ReadFile(cfg.ManagementKeyFile); err == nil {
			if key := strings.TrimSpace(string(raw)); key != "" {
				return key
			}
		}
	}
	if cfg.ManagementKeyEnv != "" {
		return strings.TrimSpace(os.Getenv(cfg.ManagementKeyEnv))
	}
	return ""
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
