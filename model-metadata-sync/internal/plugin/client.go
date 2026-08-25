package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type Transport interface {
	Do(method, url string, headers http.Header, body []byte) (status int, resp []byte, err error)
}

type ThinkingConfig struct {
	Levels []string `json:"levels,omitempty"`
}

type ModelRef struct {
	Name             string          `json:"name"`
	Alias            string          `json:"alias,omitempty"`
	DisplayName      string          `json:"display_name,omitempty"`
	MaxContextLength int             `json:"max_context_length,omitempty"`
	MaxInputTokens   int             `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int             `json:"max_output_tokens,omitempty"`
	Thinking         *ThinkingConfig `json:"thinking,omitempty"`
	InputModalities  []string        `json:"input_modalities,omitempty"`
	OutputModalities []string        `json:"output_modalities,omitempty"`
}

type ModelChannelDescriptor struct {
	Kind     string          `json:"kind"`
	Selector ChannelSelector `json:"selector"`
	Disabled bool            `json:"disabled"`
	Ready    bool            `json:"ready"`
	Revision string          `json:"revision"`
	Models   []ModelRef      `json:"models"`
}

type catalogRequest struct {
	Kind             string          `json:"kind"`
	Selector         ChannelSelector `json:"selector"`
	ExpectedRevision string          `json:"expected_revision"`
	Profile          string          `json:"profile"`
	Query            *catalogQuery   `json:"query,omitempty"`
}
type catalogQuery struct {
	ClientVersion string `json:"client_version,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	AfterID       string `json:"after_id,omitempty"`
	BeforeID      string `json:"before_id,omitempty"`
}
type catalogResponse struct {
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body"`
}

type metadataPatchRequest struct {
	Kind               string          `json:"kind"`
	Selector           ChannelSelector `json:"selector"`
	ExpectedRevision   string          `json:"expected_revision"`
	ExpectedModelNames []string        `json:"expected_model_names"`
	Operations         []ModelPatch    `json:"operations"`
}
type ModelPatch struct {
	Model  string                `json:"model"`
	Fields map[string]FieldPatch `json:"fields"`
}
type FieldPatch struct {
	Mode  string `json:"mode"`
	Value any    `json:"value"`
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
	headers := http.Header{"Authorization": []string{"Bearer " + key}}
	if payload != nil {
		headers.Set("Content-Type", "application/json")
	}
	return s.transport.Do(method, s.Current().ManagementBaseURL+path, headers, body)
}

func decodeModelChannels(raw []byte) ([]ModelChannelDescriptor, error) {
	if err := rejectSecretDescriptorFields(raw); err != nil {
		return nil, err
	}
	var wrapped struct {
		Channels []ModelChannelDescriptor `json:"channels"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&wrapped); err != nil || wrapped.Channels == nil {
		return nil, fmt.Errorf("decode sanitized model-channels response: %s", truncate(raw, 200))
	}
	return wrapped.Channels, nil
}

func (s *Service) listModelChannels(key string) ([]ModelChannelDescriptor, error) {
	status, body, err := s.mgmtJSON(key, http.MethodGet, "/v0/management/model-channels", nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("list model-channels: HTTP %d %s", status, truncate(body, 200))
	}
	return decodeModelChannels(body)
}

func matchChannel(channels []ModelChannelDescriptor, spec compiledChannel) (ModelChannelDescriptor, error) {
	var matches []ModelChannelDescriptor
	for _, channel := range channels {
		if channel.Kind != spec.Kind {
			continue
		}
		selector, err := normalizeSelector(channel.Kind, channel.Selector)
		if err == nil && selectorKey(channel.Kind, selector) == selectorKey(spec.Kind, spec.Selector) {
			channel.Selector = selector
			matches = append(matches, channel)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return ModelChannelDescriptor{}, fmt.Errorf("channel selector not found: %s", selectorKey(spec.Kind, spec.Selector))
	}
	return ModelChannelDescriptor{}, fmt.Errorf("channel selector is ambiguous: %s", selectorKey(spec.Kind, spec.Selector))
}

func (s *Service) fetchCatalogPage(key string, channel ModelChannelDescriptor, spec compiledChannel, query *catalogQuery) (json.RawMessage, error) {
	if channel.Revision == "" {
		return nil, fmt.Errorf("channel descriptor missing revision")
	}
	request := catalogRequest{
		Kind:             channel.Kind,
		Selector:         channel.Selector,
		ExpectedRevision: channel.Revision,
		Profile:          spec.Profile,
		Query:            query,
	}
	status, body, err := s.mgmtJSON(key, http.MethodPost, "/v0/management/model-channels/catalog", request)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("channel catalog: HTTP %d %s", status, truncate(body, 200))
	}
	var response catalogResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode channel catalog: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream models HTTP %d %s", response.StatusCode, truncate(response.Body, 200))
	}
	return response.Body, nil
}

func (s *Service) fetchChannelCatalog(key string, channel ModelChannelDescriptor, spec compiledChannel) ([]upstreamEntry, error) {
	if spec.Kind == KindClaude {
		return s.fetchClaudeCatalog(key, channel, spec)
	}
	var query *catalogQuery
	if spec.CodexManifest {
		query = &catalogQuery{ClientVersion: "1.0.0"}
	}
	body, err := s.fetchCatalogPage(key, channel, spec, query)
	if err != nil {
		return nil, err
	}
	return parseOpenAICatalog(catalogBody(body))
}

func (s *Service) fetchClaudeCatalog(key string, channel ModelChannelDescriptor, spec compiledChannel) ([]upstreamEntry, error) {
	var entries []upstreamEntry
	afterID := ""
	for page := 0; page < 100; page++ {
		body, err := s.fetchCatalogPage(key, channel, spec, &catalogQuery{Limit: 1000, AfterID: afterID})
		if err != nil {
			return nil, err
		}
		parsed, lastID, hasMore, err := parseClaudeCatalog(catalogBody(body))
		if err != nil {
			return nil, err
		}
		entries = append(entries, parsed...)
		if !hasMore {
			return dedupeEntries(entries), nil
		}
		if strings.TrimSpace(lastID) == "" || lastID == afterID {
			return nil, fmt.Errorf("claude catalog pagination did not advance")
		}
		afterID = lastID
	}
	return nil, fmt.Errorf("claude catalog exceeded 100 pages")
}

func (s *Service) patchMetadata(key string, channel ModelChannelDescriptor, patches []ModelPatch) error {
	if len(patches) == 0 {
		return nil
	}
	if channel.Revision == "" {
		return fmt.Errorf("channel descriptor missing revision")
	}
	request := metadataPatchRequest{
		Kind:               channel.Kind,
		Selector:           channel.Selector,
		ExpectedRevision:   channel.Revision,
		ExpectedModelNames: modelNames(channel.Models),
		Operations:         patches,
	}
	status, body, err := s.mgmtJSON(key, http.MethodPatch, "/v0/management/model-channels/metadata", request)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("patch metadata: HTTP %d %s", status, truncate(body, 300))
	}
	return nil
}

func modelNames(models []ModelRef) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		if name := strings.TrimSpace(model.Name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func catalogBody(raw json.RawMessage) []byte {
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		return []byte(encoded)
	}
	return raw
}

func rejectSecretDescriptorFields(raw []byte) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	forbidden := map[string]struct{}{"api-key": {}, "api_key": {}, "token": {}, "cookie": {}, "headers": {}, "header": {}, "proxy-url": {}, "proxy_url": {}, "auth-index": {}, "auth_index": {}, "auth-id": {}, "auth_id": {}, "credential": {}, "credentials": {}}
	var walk func(any) error
	walk = func(current any) error {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if _, blocked := forbidden[strings.ToLower(strings.TrimSpace(key))]; blocked {
					return fmt.Errorf("model-channels response contains forbidden secret-bearing field %q", key)
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func truncate(raw []byte, n int) string {
	value := strings.TrimSpace(string(raw))
	if len(value) > n {
		return value[:n] + "..."
	}
	return value
}
