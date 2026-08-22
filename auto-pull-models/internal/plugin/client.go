package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type Transport interface {
	Do(method, url string, headers http.Header, body []byte) (status int, resp []byte, err error)
}

type CompatProvider struct {
	Name          string          `json:"name"`
	Disabled      bool            `json:"disabled"`
	BaseURL       string          `json:"base-url"`
	APIKeyEntries []APIKeyEntry   `json:"api-key-entries"`
	Models        []ModelRef      `json:"models"`
	Headers       json.RawMessage `json:"headers,omitempty"`
}

type APIKeyEntry struct {
	APIKey    string `json:"api-key"`
	ProxyURL  string `json:"proxy-url,omitempty"`
	AuthIndex string `json:"auth-index,omitempty"`
}

type apiCallRequest struct {
	AuthIndex string            `json:"auth_index,omitempty"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header,omitempty"`
}

type apiCallResponse struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func (s *Service) mgmtJSON(key, method, path string, payload any) (int, []byte, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	if payload != nil {
		headers.Set("Content-Type", "application/json")
	}
	url := s.cfg.ManagementBaseURL + path
	return s.transport.Do(method, url, headers, body)
}

func decodeCompatList(raw []byte) ([]CompatProvider, error) {
	var wrapped struct {
		Items []CompatProvider `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Items != nil {
		return wrapped.Items, nil
	}
	var list []CompatProvider
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	return nil, fmt.Errorf("decode openai-compatibility: %s", truncate(raw, 200))
}

func (s *Service) listCompat(key string) ([]CompatProvider, error) {
	status, body, err := s.mgmtJSON(key, http.MethodGet, "/v0/management/openai-compatibility", nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("list openai-compatibility: HTTP %d %s", status, truncate(body, 200))
	}
	return decodeCompatList(body)
}

func (s *Service) patchCompatModels(key string, index int, models []ModelRef) error {
	payload := map[string]any{
		"index": index,
		"value": map[string]any{"models": models},
	}
	status, body, err := s.mgmtJSON(key, http.MethodPatch, "/v0/management/openai-compatibility", payload)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return fmt.Errorf("patch openai-compatibility models: HTTP %d %s", status, truncate(body, 300))
}

func (s *Service) fetchUpstreamCatalog(key, authIndex, url string) ([]upstreamEntry, error) {
	req := apiCallRequest{
		AuthIndex: authIndex,
		Method:    http.MethodGet,
		URL:       url,
	}
	if authIndex != "" {
		req.Header = map[string]string{"Authorization": "Bearer $TOKEN$"}
	}
	status, body, err := s.mgmtJSON(key, http.MethodPost, "/v0/management/api-call", req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("api-call HTTP %d %s", status, truncate(body, 200))
	}
	var parsed apiCallResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode api-call: %w", err)
	}
	if parsed.StatusCode < 200 || parsed.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream models HTTP %d %s", parsed.StatusCode, truncate([]byte(parsed.Body), 200))
	}
	return parseUpstreamCatalog([]byte(parsed.Body))
}

func firstAuthIndex(entries []APIKeyEntry) string {
	for _, e := range entries {
		if strings.TrimSpace(e.AuthIndex) != "" {
			return strings.TrimSpace(e.AuthIndex)
		}
	}
	return ""
}

func truncate(raw []byte, n int) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
