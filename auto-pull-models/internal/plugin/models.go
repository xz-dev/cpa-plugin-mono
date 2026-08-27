package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty models response")
	}
	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := decodeCatalogValue(trimmed, &items); err != nil || items == nil {
			return nil, fmt.Errorf("unrecognized models payload")
		}
	case '{':
		wrapped, err := decodeCatalogObject(trimmed)
		if err != nil {
			return nil, fmt.Errorf("unrecognized models payload")
		}
		for key := range wrapped {
			lower := strings.ToLower(key)
			if (lower == "data" || lower == "models") && key != lower {
				return nil, fmt.Errorf("unrecognized models payload")
			}
		}
		data, hasData := wrapped["data"]
		models, hasModels := wrapped["models"]
		if hasData == hasModels {
			return nil, fmt.Errorf("unrecognized models payload")
		}
		container := data
		if hasModels {
			container = models
		}
		if err := decodeCatalogValue(container, &items); err != nil || items == nil {
			return nil, fmt.Errorf("unrecognized models payload")
		}
	default:
		return nil, fmt.Errorf("unrecognized models payload")
	}
	out := make([]upstreamEntry, 0, len(items))
	seen := map[string]struct{}{}
	for _, raw := range items {
		id, err := decodeUpstreamID(raw)
		if err != nil {
			return nil, err
		}
		if len(id) > 1024 || hasControl(id) {
			return nil, fmt.Errorf("invalid model ID")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate model ID")
		}
		seen[id] = struct{}{}
		out = append(out, upstreamEntry{ID: id})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models payload contains no model IDs")
	}
	return out, nil
}

func decodeUpstreamID(raw json.RawMessage) (string, error) {
	var value string
	if decodeCatalogValue(raw, &value) == nil {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", fmt.Errorf("invalid model ID")
		}
		return value, nil
	}
	fields, err := decodeCatalogObject(raw)
	if err != nil {
		return "", fmt.Errorf("invalid model entry")
	}
	for key := range fields {
		lower := strings.ToLower(key)
		if (lower == "id" || lower == "slug" || lower == "name") && key != lower {
			return "", fmt.Errorf("invalid model entry")
		}
	}
	var id string
	found := false
	for _, key := range []string{"id", "slug", "name"} {
		rawValue, exists := fields[key]
		if !exists {
			continue
		}
		var candidate string
		if decodeCatalogValue(rawValue, &candidate) != nil {
			return "", fmt.Errorf("invalid model entry")
		}
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || found && candidate != id {
			return "", fmt.Errorf("ambiguous model entry")
		}
		id, found = candidate, true
	}
	if !found {
		return "", fmt.Errorf("model entry has no ID")
	}
	return id, nil
}

func decodeCatalogObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("invalid object")
	}
	result := make(map[string]json.RawMessage)
	seen := make(map[string]bool)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		folded := strings.ToUpper(key)
		if err != nil || !ok || seen[folded] {
			return nil, fmt.Errorf("invalid object key")
		}
		seen[folded] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid object value")
		}
		result[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("invalid object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return result, nil
}

func decodeCatalogValue(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
