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
	// APIKey authorizes the inner /v1/models request (CPA static api-key).
	// Env/APIKeyFile override this plaintext field.
	APIKey     string `json:"api_key,omitempty"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
	APIKeyFile string `json:"api_key_file,omitempty"`
}

func New(t Transport) *Service {
	return &Service{transport: t}
}

func (s *Service) Configure(pluginYAML []byte) error {
	cfg := Config{ManagementBaseURL: "http://127.0.0.1:8317"}
	var wrapper Config
	if raw := readConfigJSON(pluginYAML); len(raw) > 0 {
		_ = json.Unmarshal(raw, &wrapper)
	}
	if strings.TrimSpace(wrapper.ManagementBaseURL) != "" {
		cfg.ManagementBaseURL = strings.TrimRight(strings.TrimSpace(wrapper.ManagementBaseURL), "/")
	}
	cfg.ManagementKeyEnv = strings.TrimSpace(wrapper.ManagementKeyEnv)
	cfg.ManagementKeyFile = strings.TrimSpace(wrapper.ManagementKeyFile)
	cfg.APIKey = strings.TrimSpace(wrapper.APIKey)
	cfg.APIKeyEnv = strings.TrimSpace(wrapper.APIKeyEnv)
	cfg.APIKeyFile = strings.TrimSpace(wrapper.APIKeyFile)
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
	apiKey := s.resolveAPIKey(cfg)
	if apiKey == "" {
		catalog.Error = "api key unavailable (set api_key / api_key_env / api_key_file in model-info config)"
		return catalog
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)
	// During host boot the API server may not be listening yet; poll briefly.
	for attempt := 0; attempt < 5; attempt++ {
		status, body, err := s.transport.Do(http.MethodGet, cfg.ManagementBaseURL+"/v1/models?client_version=1.0.0", headers, nil)
		if err == nil && status == http.StatusOK {
			models, perr := parseCatalog(body)
			if perr == nil {
				catalog.Models = models
				catalog.Count = len(models)
				return catalog
			}
			catalog.Error = perr.Error()
			return catalog
		}
		if err == nil && status != http.StatusServiceUnavailable && status != http.StatusNotFound {
			catalog.Error = fmt.Sprintf("catalog HTTP %d: %s", status, truncate(body, 200))
			return catalog
		}
		time.Sleep(2 * time.Second)
	}
	catalog.Error = "catalog endpoint unreachable"
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

func (s *Service) resolveAPIKey(cfg Config) string {
	for _, candidate := range []struct {
		value string
		kind  string
	}{
		{cfg.APIKey, "plain"},
		{cfg.APIKeyFile, "file"},
		{cfg.APIKeyEnv, "env"},
	} {
		if candidate.value == "" {
			continue
		}
		if candidate.kind == "file" {
			if raw, err := os.ReadFile(candidate.value); err == nil {
				if key := strings.TrimSpace(string(raw)); key != "" {
					return key
				}
			}
			continue
		}
		if candidate.kind == "env" {
			if key := strings.TrimSpace(os.Getenv(candidate.value)); key != "" {
				return key
			}
			continue
		}
		return candidate.value
	}
	return ""
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// readConfigJSON resolves the plugin YAML's config_file hint and returns the
// JSON config file contents. Plugin YAML is not JSON; parse just the hint.
func readConfigJSON(pluginYAML []byte) []byte {
	path := "plugins/model-info/config.json"
	for _, line := range strings.Split(string(pluginYAML), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "config_file:") {
			if v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "config_file:")), `"'`); v != "" {
				path = v
			}
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return raw
}
