package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type ThinkingConfig struct {
	Levels []string `json:"levels,omitempty"`
}

type ModelRef struct {
	Name             string
	MaxContextLength int
	MaxInputTokens   int
	MaxOutputTokens  int
	Thinking         *ThinkingConfig
	InputModalities  []string
	OutputModalities []string
}

type upstreamEntry struct {
	ID                     string
	Efforts, Input, Output []string
	Context, MaxTokens     int
	ClaudeMaxInput         int
}

func parseOpenAICatalog(body []byte) ([]upstreamEntry, error) {
	if err := validateCatalogJSON(body); err != nil {
		return nil, fmt.Errorf("unrecognized OpenAI models payload")
	}
	trimmed := bytes.TrimSpace(body)
	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(trimmed, &items); err != nil || items == nil {
			return nil, fmt.Errorf("unrecognized OpenAI models payload")
		}
	case '{':
		wrapped, err := catalogObject(trimmed)
		if err != nil || hasFoldedCatalogField(wrapped, "data") || hasFoldedCatalogField(wrapped, "models") {
			return nil, fmt.Errorf("unrecognized OpenAI models payload")
		}
		data, hasData := wrapped["data"]
		models, hasModels := wrapped["models"]
		if hasData == hasModels {
			return nil, fmt.Errorf("unrecognized OpenAI models payload")
		}
		container := data
		if hasModels {
			container = models
		}
		if err := json.Unmarshal(container, &items); err != nil || items == nil {
			return nil, fmt.Errorf("unrecognized OpenAI models payload")
		}
	default:
		return nil, fmt.Errorf("unrecognized OpenAI models payload")
	}
	entries := make([]upstreamEntry, 0, len(items))
	seen := make(map[string]bool)
	for _, raw := range items {
		entry, err := decodeOpenAIItem(raw)
		if err != nil {
			return nil, err
		}
		if seen[entry.ID] {
			return nil, fmt.Errorf("duplicate model ID")
		}
		seen[entry.ID] = true
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("models payload contains no model IDs")
	}
	return entries, nil
}

func decodeOpenAIItem(raw json.RawMessage) (upstreamEntry, error) {
	var scalar string
	if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '"' {
		if err := json.Unmarshal(raw, &scalar); err != nil {
			return upstreamEntry{}, fmt.Errorf("invalid model entry")
		}
		scalar = strings.TrimSpace(scalar)
		if scalar == "" || len(scalar) > 1024 || hasControl(scalar) {
			return upstreamEntry{}, fmt.Errorf("invalid model ID")
		}
		return upstreamEntry{ID: scalar}, nil
	}
	fields, err := catalogObject(raw)
	if err != nil || hasAnyFoldedCatalogField(fields,
		"id", "slug", "name", "context_length", "context_window", "max_tokens",
		"input_modalities", "output_modalities", "supported_reasoning_levels", "top_provider", "architecture", "reasoning") {
		return upstreamEntry{}, fmt.Errorf("invalid model entry")
	}
	for key, names := range map[string][]string{
		"top_provider": {"max_completion_tokens"},
		"architecture": {"input_modalities", "output_modalities"},
		"reasoning":    {"supported_efforts"},
	} {
		if nested, ok := fields[key]; ok {
			if err := validateOptionalCatalogObject(nested, names...); err != nil {
				return upstreamEntry{}, fmt.Errorf("invalid model entry")
			}
		}
	}
	if rawLevels, ok := fields["supported_reasoning_levels"]; ok {
		if err := validateReasoningLevels(rawLevels); err != nil {
			return upstreamEntry{}, fmt.Errorf("invalid model entry")
		}
	}
	id := ""
	for _, key := range []string{"id", "slug", "name"} {
		rawValue, exists := fields[key]
		if !exists {
			continue
		}
		var candidate string
		if json.Unmarshal(rawValue, &candidate) != nil {
			return upstreamEntry{}, fmt.Errorf("invalid model entry")
		}
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || id != "" && candidate != id {
			return upstreamEntry{}, fmt.Errorf("ambiguous model entry")
		}
		id = candidate
	}
	if id == "" || len(id) > 1024 || hasControl(id) {
		return upstreamEntry{}, fmt.Errorf("model entry has no valid ID")
	}
	var item struct {
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
	if err := json.Unmarshal(raw, &item); err != nil {
		return upstreamEntry{}, fmt.Errorf("invalid model entry")
	}
	for _, value := range []float64{item.Context, item.ContextWindow, item.MaxTokens, item.TopProvider.MaxCompletionTokens} {
		if !validCatalogInteger(value) {
			return upstreamEntry{}, fmt.Errorf("invalid model token limit")
		}
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
	if item.Reasoning != nil && len(item.Reasoning.SupportedEfforts) > 0 {
		entry.Efforts, err = parseSupportedEfforts(item.Reasoning.SupportedEfforts)
		if err != nil {
			return upstreamEntry{}, err
		}
	} else {
		for _, level := range item.SupportedReasoningLevels {
			if strings.TrimSpace(level.Effort) == "" {
				return upstreamEntry{}, fmt.Errorf("invalid reasoning level")
			}
			entry.Efforts = append(entry.Efforts, level.Effort)
		}
		entry.Efforts = normalizeEfforts(entry.Efforts)
	}
	return entry, nil
}

func parseClaudeCatalog(body []byte) ([]upstreamEntry, string, bool, error) {
	if err := validateCatalogJSON(body); err != nil {
		return nil, "", false, fmt.Errorf("unrecognized Claude models payload")
	}
	fields, err := catalogObject(body)
	if err != nil {
		return nil, "", false, fmt.Errorf("unrecognized Claude models payload")
	}
	for _, key := range []string{"data", "last_id", "has_more"} {
		if hasFoldedCatalogField(fields, key) {
			return nil, "", false, fmt.Errorf("unrecognized Claude models payload")
		}
	}
	var response struct {
		Data    []json.RawMessage `json:"data"`
		LastID  string            `json:"last_id"`
		HasMore bool              `json:"has_more"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Data == nil {
		return nil, "", false, fmt.Errorf("unrecognized Claude models payload")
	}
	response.LastID = strings.TrimSpace(response.LastID)
	if len(response.LastID) > 256 || hasControl(response.LastID) {
		return nil, "", false, fmt.Errorf("invalid Claude cursor")
	}
	entries := make([]upstreamEntry, 0, len(response.Data))
	seen := make(map[string]bool)
	for _, raw := range response.Data {
		itemFields, err := catalogObject(raw)
		if err != nil || hasAnyFoldedCatalogField(itemFields,
			"id", "max_input_tokens", "max_tokens", "input_modalities", "supported_reasoning_levels", "supported_efforts") {
			return nil, "", false, fmt.Errorf("invalid Claude model entry")
		}
		if rawLevels, ok := itemFields["supported_reasoning_levels"]; ok {
			if err := validateReasoningLevels(rawLevels); err != nil {
				return nil, "", false, fmt.Errorf("invalid Claude model entry")
			}
		}
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
		if json.Unmarshal(raw, &item) != nil {
			return nil, "", false, fmt.Errorf("invalid Claude model entry")
		}
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" || len(item.ID) > 1024 || hasControl(item.ID) || item.MaxInputTokens < 0 || item.MaxTokens < 0 || seen[item.ID] {
			return nil, "", false, fmt.Errorf("invalid Claude model entry")
		}
		seen[item.ID] = true
		entry := upstreamEntry{ID: item.ID, ClaudeMaxInput: item.MaxInputTokens, Context: item.MaxInputTokens, MaxTokens: item.MaxTokens, Input: cpaModalities(item.InputModalities)}
		if len(item.SupportedEfforts) > 0 {
			entry.Efforts, err = parseSupportedEfforts(item.SupportedEfforts)
			if err != nil {
				return nil, "", false, err
			}
		} else {
			for _, level := range item.SupportedReasoningLevels {
				if strings.TrimSpace(level.Effort) == "" {
					return nil, "", false, fmt.Errorf("invalid reasoning level")
				}
				entry.Efforts = append(entry.Efforts, level.Effort)
			}
			entry.Efforts = normalizeEfforts(entry.Efforts)
		}
		entries = append(entries, entry)
	}
	return entries, response.LastID, response.HasMore, nil
}

func validateOptionalCatalogObject(raw json.RawMessage, names ...string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	fields, err := catalogObject(raw)
	if err != nil || hasAnyFoldedCatalogField(fields, names...) {
		return fmt.Errorf("invalid catalog object")
	}
	return nil
}

func validateReasoningLevels(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var levels []json.RawMessage
	if json.Unmarshal(raw, &levels) != nil {
		return fmt.Errorf("invalid reasoning levels")
	}
	for _, level := range levels {
		fields, err := catalogObject(level)
		if err != nil || hasFoldedCatalogField(fields, "effort") {
			return fmt.Errorf("invalid reasoning level")
		}
	}
	return nil
}

func parseSupportedEfforts(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil, fmt.Errorf("invalid supported efforts")
	}
	return normalizeEfforts(values), nil
}

func validCatalogInteger(value float64) bool {
	return value >= 0 && value <= float64(math.MaxInt) && math.Trunc(value) == value
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
			hasDepth = hasDepth || value != "none" && value != "auto"
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
	var out []string
	if hasText {
		out = append(out, "text")
	}
	if hasImage {
		out = append(out, "image")
	}
	return out
}
