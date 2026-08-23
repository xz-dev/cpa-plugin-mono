package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const defaultModelsdevURL = "https://models.dev/api.json"

// modelsdevModel carries the limit/modality metadata we use from models.dev.
type modelsdevModel struct {
	FullKey  string
	Provider string
	ID       string
	Context  int
	MaxOut   int
	Input    []string
	Output   []string
}

type modelsdevCatalog struct {
	byKey  map[string]modelsdevModel
	byBare map[string][]modelsdevModel
}

func (s *Service) fetchModelsdevCatalog(url string) (*modelsdevCatalog, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		url = defaultModelsdevURL
	}
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "auto-pull-models")
	status, body, err := s.transport.Do(http.MethodGet, url, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("modelsdev: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("modelsdev HTTP %d %s", status, truncate(body, 200))
	}
	return parseModelsdevCatalog(body)
}

func parseModelsdevCatalog(raw []byte) (*modelsdevCatalog, error) {
	var providers map[string]struct {
		Models map[string]struct {
			ID         string `json:"id"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &providers); err != nil {
		return nil, fmt.Errorf("modelsdev: %w", err)
	}
	cat := &modelsdevCatalog{
		byKey:  map[string]modelsdevModel{},
		byBare: map[string][]modelsdevModel{},
	}
	for provider, payload := range providers {
		for key, model := range payload.Models {
			id := strings.TrimSpace(model.ID)
			if id == "" {
				id = strings.TrimSpace(key)
			}
			if id == "" {
				continue
			}
			// Aggregator entries key models as "vendor/id"; bare entries are plain ids.
			fullKey := key
			if !strings.Contains(key, "/") {
				fullKey = provider + "/" + key
			}
			entry := modelsdevModel{
				FullKey:  fullKey,
				Provider: provider,
				ID:       id,
				Context:  model.Limit.Context,
				MaxOut:   model.Limit.Output,
				Input:    model.Modalities.Input,
				Output:   model.Modalities.Output,
			}
			if entry.Context <= 0 && entry.MaxOut <= 0 && len(entry.Input) == 0 {
				continue // placeholder rows with zero limits carry no data
			}
			cat.byKey[strings.ToLower(fullKey)] = entry
			bare := id
			if i := strings.LastIndex(bare, "/"); i >= 0 {
				bare = bare[i+1:]
			}
			lower := strings.ToLower(bare)
			cat.byBare[lower] = append(cat.byBare[lower], entry)
		}
	}
	if len(cat.byKey) == 0 {
		return nil, fmt.Errorf("modelsdev: empty catalog")
	}
	for _, list := range cat.byBare {
		sort.Slice(list, func(i, j int) bool { return modelsdevRank(list[i]) < modelsdevRank(list[j]) })
	}
	return cat, nil
}

// modelsdevRank prefers the OpenRouter listing when the same bare id appears
// under several vendors, then falls back to lexicographic order.
func modelsdevRank(m modelsdevModel) string {
	if strings.EqualFold(m.Provider, "openrouter") {
		return "\x00" + m.FullKey
	}
	return m.FullKey
}

// lookup resolves a model id: exact provider-qualified key first, then the
// lexicographically first bare-id match. ponytail: first-match is a heuristic;
// add per-provider vendor hints if ambiguous limits ever cause mismatches.
func (c *modelsdevCatalog) lookup(id string) (modelsdevModel, bool) {
	if c == nil {
		return modelsdevModel{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return modelsdevModel{}, false
	}
	if entry, ok := c.byKey[strings.ToLower(id)]; ok {
		return entry, true
	}
	bare := id
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	if list := c.byBare[strings.ToLower(bare)]; len(list) > 0 {
		return list[0], true
	}
	return modelsdevModel{}, false
}
