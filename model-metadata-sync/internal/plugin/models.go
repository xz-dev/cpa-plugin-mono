package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
)

type upstreamEntry struct {
	ID, LastID             string
	Efforts, Input, Output []string
	Context, MaxTokens     int
	ClaudeMaxInput         int
}

func parseOpenAICatalog(body []byte) ([]upstreamEntry, error) {
	var items []json.RawMessage
	var wrapped struct{ Data, Models []json.RawMessage }
	switch {
	case json.Unmarshal(body, &wrapped) == nil && (wrapped.Data != nil || wrapped.Models != nil):
		items = wrapped.Data
		if items == nil {
			items = wrapped.Models
		}
	case json.Unmarshal(body, &items) == nil:
	default:
		return nil, fmt.Errorf("unrecognized OpenAI models payload")
	}
	out := make([]upstreamEntry, 0, len(items))
	for _, raw := range items {
		if entry, ok := decodeOpenAIItem(raw); ok {
			out = append(out, entry)
		}
	}
	out = dedupeEntries(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("models payload contains no model IDs")
	}
	return out, nil
}

func decodeOpenAIItem(raw json.RawMessage) (upstreamEntry, bool) {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		value = strings.TrimSpace(value)
		return upstreamEntry{ID: value}, value != ""
	}
	var item struct {
		ID, Name, Slug           string
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
	if json.Unmarshal(raw, &item) != nil {
		return upstreamEntry{}, false
	}
	id := strings.TrimSpace(item.ID)
	if id == "" {
		id = strings.TrimSpace(item.Slug)
	}
	if id == "" {
		id = strings.TrimSpace(item.Name)
	}
	if id == "" {
		return upstreamEntry{}, false
	}
	entry := upstreamEntry{ID: id, Context: int(item.Context), MaxTokens: int(item.MaxTokens), Input: item.Architecture.Input, Output: item.Architecture.Output}
	if item.ContextWindow > 0 {
		entry.Context = int(item.ContextWindow)
	}
	if entry.MaxTokens == 0 && item.TopProvider.MaxCompletionTokens > 0 {
		entry.MaxTokens = int(item.TopProvider.MaxCompletionTokens)
	}
	if len(item.InputModalities) > 0 {
		entry.Input = item.InputModalities
	}
	if len(item.OutputModalities) > 0 {
		entry.Output = item.OutputModalities
	}
	if item.Reasoning != nil {
		entry.Efforts = parseSupportedEfforts(item.Reasoning.SupportedEfforts)
	} else {
		for _, level := range item.SupportedReasoningLevels {
			entry.Efforts = append(entry.Efforts, level.Effort)
		}
		entry.Efforts = normalizeEfforts(entry.Efforts)
	}
	return entry, true
}

func parseClaudeCatalog(body []byte) ([]upstreamEntry, string, bool, error) {
	var response struct {
		Data    []json.RawMessage `json:"data"`
		LastID  string            `json:"last_id"`
		HasMore bool              `json:"has_more"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Data == nil {
		return nil, "", false, fmt.Errorf("unrecognized Claude models payload")
	}
	entries := make([]upstreamEntry, 0, len(response.Data))
	for _, raw := range response.Data {
		var item struct {
			ID                       string   `json:"id"`
			MaxInputTokens           int      `json:"max_input_tokens"`
			MaxTokens                int      `json:"max_tokens"`
			InputModalities          []string `json:"input_modalities"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			SupportedEfforts json.RawMessage `json:"supported_efforts"`
		}
		if json.Unmarshal(raw, &item) != nil || strings.TrimSpace(item.ID) == "" {
			continue
		}
		entry := upstreamEntry{ID: strings.TrimSpace(item.ID), ClaudeMaxInput: item.MaxInputTokens, Context: item.MaxInputTokens, MaxTokens: item.MaxTokens, Input: cpaModalities(item.InputModalities)}
		if len(item.SupportedEfforts) > 0 {
			entry.Efforts = parseSupportedEfforts(item.SupportedEfforts)
		} else {
			for _, level := range item.SupportedReasoningLevels {
				entry.Efforts = append(entry.Efforts, level.Effort)
			}
			entry.Efforts = normalizeEfforts(entry.Efforts)
		}
		entries = append(entries, entry)
	}
	return entries, response.LastID, response.HasMore, nil
}

func dedupeEntries(entries []upstreamEntry) []upstreamEntry {
	seen := map[string]struct{}{}
	out := make([]upstreamEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.ID == "" {
			continue
		}
		if _, ok := seen[entry.ID]; ok {
			continue
		}
		seen[entry.ID] = struct{}{}
		out = append(out, entry)
	}
	return out
}

func parseSupportedEfforts(raw json.RawMessage) []string {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var values []any
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, fmt.Sprint(value))
	}
	return normalizeEfforts(out)
}

func normalizeEfforts(values []string) []string {
	have := map[string]struct{}{}
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if _, ok := cpaThinkingLevels[normalized]; ok {
			have[normalized] = struct{}{}
		}
	}
	order := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "auto"}
	out := make([]string, 0, len(have))
	hasDepth := false
	for _, value := range order {
		if _, ok := have[value]; ok {
			out = append(out, value)
			hasDepth = hasDepth || (value != "none" && value != "auto")
		}
	}
	if !hasDepth {
		return nil
	}
	return out
}

func cpaModalities(values []string) []string {
	hasText, hasImage := false, false
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "text":
			hasText = true
		case "image":
			hasImage = true
		}
	}
	out := []string{}
	if hasText {
		out = append(out, "text")
	}
	if hasImage {
		out = append(out, "image")
	}
	return out
}
