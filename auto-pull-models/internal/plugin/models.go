package plugin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type upstreamEntry struct {
	ID      string
	Efforts []string
	Input   []string
	Output  []string
	Context int
	// MaxTokens is the upstream-declared output limit (Codex manifest
	// max_tokens, OpenRouter top_provider.max_completion_tokens).
	MaxTokens int
}

func parseUpstreamModels(body []byte) ([]string, error) {
	entries, err := parseUpstreamCatalog(body)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids, nil
}

func parseUpstreamCatalog(body []byte) ([]upstreamEntry, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, fmt.Errorf("empty models response")
	}
	var items []json.RawMessage
	var wrapped struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	switch {
	case json.Unmarshal(body, &wrapped) == nil && (len(wrapped.Data) > 0 || len(wrapped.Models) > 0):
		if len(wrapped.Data) > 0 {
			items = wrapped.Data
		} else {
			items = wrapped.Models
		}
	case json.Unmarshal(body, &items) == nil && len(items) > 0:
	default:
		return nil, fmt.Errorf("unrecognized models payload")
	}
	out := make([]upstreamEntry, 0, len(items))
	seen := map[string]struct{}{}
	for _, raw := range items {
		entry, ok := decodeUpstreamItem(raw)
		if !ok || entry.ID == "" {
			continue
		}
		if _, dup := seen[entry.ID]; dup {
			continue
		}
		seen[entry.ID] = struct{}{}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("unrecognized models payload")
	}
	return out, nil
}

func decodeUpstreamItem(raw json.RawMessage) (upstreamEntry, bool) {
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		id := strings.TrimSpace(asString)
		if id == "" {
			return upstreamEntry{}, false
		}
		return upstreamEntry{ID: id}, true
	}
	var m struct {
		ID                       string   `json:"id"`
		Name                     string   `json:"name"`
		Slug                     string   `json:"slug"`
		Context                  float64  `json:"context_length"`
		ContextWindow            float64  `json:"context_window"`
		MaxTokens                float64  `json:"max_tokens"`
		InputModalities          []string `json:"input_modalities"`
		OutputModalities         []string `json:"output_modalities"`
		SupportedReasoningLevels []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
		TopProvider struct {
			MaxCompletionTokens float64 `json:"max_completion_tokens"`
		} `json:"top_provider"`
		Architecture struct {
			Input  []string `json:"input_modalities"`
			Output []string `json:"output_modalities"`
		} `json:"architecture"`
		Reasoning *struct {
			SupportedEfforts json.RawMessage `json:"supported_efforts"`
		} `json:"reasoning"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return upstreamEntry{}, false
	}
	id := strings.TrimSpace(m.ID)
	if id == "" {
		id = strings.TrimSpace(m.Slug)
	}
	if id == "" {
		id = strings.TrimSpace(m.Name)
	}
	if id == "" {
		return upstreamEntry{}, false
	}
	entry := upstreamEntry{
		ID:      id,
		Input:   m.Architecture.Input,
		Output:  m.Architecture.Output,
		Context: int(m.Context),
	}
	if m.ContextWindow > 0 {
		entry.Context = int(m.ContextWindow)
	}
	if m.MaxTokens > 0 {
		entry.MaxTokens = int(m.MaxTokens)
	} else if m.TopProvider.MaxCompletionTokens > 0 {
		entry.MaxTokens = int(m.TopProvider.MaxCompletionTokens)
	}
	if len(m.InputModalities) > 0 {
		entry.Input = m.InputModalities
	}
	if len(m.OutputModalities) > 0 {
		entry.Output = m.OutputModalities
	}
	if m.Reasoning != nil {
		entry.Efforts = parseSupportedEfforts(m.Reasoning.SupportedEfforts)
	} else if len(m.SupportedReasoningLevels) > 0 {
		efforts := make([]string, 0, len(m.SupportedReasoningLevels))
		for _, level := range m.SupportedReasoningLevels {
			efforts = append(efforts, level.Effort)
		}
		entry.Efforts = normalizeEfforts(efforts)
	}
	return entry, true
}

func parseSupportedEfforts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	trim := strings.TrimSpace(string(raw))
	if trim == "null" {
		return normalizeEfforts([]string{"max", "xhigh", "high", "medium", "low", "minimal", "none"})
	}
	var values []any
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, fmt.Sprint(v))
	}
	return normalizeEfforts(out)
}

func normalizeEfforts(values []string) []string {
	have := map[string]struct{}{}
	for _, v := range values {
		s := strings.ToLower(strings.TrimSpace(v))
		if _, ok := cpaThinkingLevels[s]; ok {
			have[s] = struct{}{}
		}
	}
	order := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "auto"}
	out := make([]string, 0, len(have))
	hasDepth := false
	for _, s := range order {
		if _, ok := have[s]; !ok {
			continue
		}
		out = append(out, s)
		if s != "none" && s != "auto" {
			hasDepth = true
		}
	}
	if !hasDepth {
		return nil
	}
	return out
}

func cpaModalities(values []string) []string {
	hasText, hasImage := false, false
	for _, v := range values {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "text":
			hasText = true
		case "image":
			hasImage = true
		}
	}
	if !hasText && !hasImage {
		return nil
	}
	out := make([]string, 0, 2)
	if hasText {
		out = append(out, "text")
	}
	if hasImage {
		out = append(out, "image")
	}
	return out
}

func modelsURL(baseURL string, codexManifest bool) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(parsed.Path), "/models") {
		parsed.Path += "/models"
	}
	if codexManifest {
		query := parsed.Query()
		query.Set("client_version", "1.0.0")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}
