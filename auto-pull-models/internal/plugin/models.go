package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
)

type upstreamEntry struct {
	ID string
}

func parseUpstreamModels(body []byte) ([]string, error) {
	entries, err := parseUpstreamCatalog(body)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return ids, nil
}

func parseUpstreamCatalog(body []byte) ([]upstreamEntry, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, fmt.Errorf("empty models response")
	}
	var items []json.RawMessage
	var wrapped struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	switch {
	case json.Unmarshal(body, &wrapped) == nil && (wrapped.Data != nil || wrapped.Models != nil):
		items = wrapped.Data
		if items == nil {
			items = wrapped.Models
		}
	case json.Unmarshal(body, &items) == nil:
	default:
		return nil, fmt.Errorf("unrecognized models payload")
	}
	out := make([]upstreamEntry, 0, len(items))
	seen := map[string]struct{}{}
	for _, raw := range items {
		id := decodeUpstreamID(raw)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, upstreamEntry{ID: id})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models payload contains no model IDs")
	}
	return out, nil
}

func decodeUpstreamID(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	var item struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	for _, candidate := range []string{item.ID, item.Slug, item.Name} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return ""
}
