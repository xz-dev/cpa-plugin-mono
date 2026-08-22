package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
)

type upstreamEntry struct {
	ID      string
	Efforts []string
	Input   []string
	Output  []string
	Context int
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
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		Context      float64 `json:"context_length"`
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
	if m.Reasoning != nil {
		entry.Efforts = parseSupportedEfforts(m.Reasoning.SupportedEfforts)
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

func modelsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, "/models") {
		return base
	}
	return base + "/models"
}
