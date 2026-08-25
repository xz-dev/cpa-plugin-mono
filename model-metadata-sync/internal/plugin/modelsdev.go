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

type modelsdevProviderCatalog struct {
	byKey  map[string]modelsdevModel
	byID   map[string][]modelsdevModel
	byBare map[string][]modelsdevModel
}

type modelsdevCatalog struct {
	providers map[string]*modelsdevProviderCatalog
}

func (s *Service) fetchModelsdevCatalog(url string) (*modelsdevCatalog, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		url = defaultModelsdevURL
	}
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "model-metadata-sync")
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
	cat := &modelsdevCatalog{providers: map[string]*modelsdevProviderCatalog{}}
	for provider, payload := range providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		pc := &modelsdevProviderCatalog{
			byKey:  map[string]modelsdevModel{},
			byID:   map[string][]modelsdevModel{},
			byBare: map[string][]modelsdevModel{},
		}
		for key, model := range payload.Models {
			id := strings.TrimSpace(model.ID)
			if id == "" {
				id = strings.TrimSpace(key)
			}
			if id == "" {
				continue
			}
			entry := modelsdevModel{
				FullKey:  strings.TrimSpace(key),
				Provider: provider,
				ID:       id,
				Context:  model.Limit.Context,
				MaxOut:   model.Limit.Output,
				Input:    model.Modalities.Input,
				Output:   model.Modalities.Output,
			}
			if entry.Context <= 0 && entry.MaxOut <= 0 && len(entry.Input) == 0 && len(entry.Output) == 0 {
				continue
			}
			pc.byKey[strings.ToLower(entry.FullKey)] = entry
			pc.byID[strings.ToLower(entry.ID)] = append(pc.byID[strings.ToLower(entry.ID)], entry)
			bare := entry.ID
			if i := strings.LastIndex(bare, "/"); i >= 0 {
				bare = bare[i+1:]
			}
			pc.byBare[strings.ToLower(bare)] = append(pc.byBare[strings.ToLower(bare)], entry)
		}
		if len(pc.byKey) > 0 {
			cat.providers[provider] = pc
		}
	}
	if len(cat.providers) == 0 {
		return nil, fmt.Errorf("modelsdev: empty catalog")
	}
	return cat, nil
}

func (c *modelsdevCatalog) lookupSource(source metadataSource, id string) (modelsdevModel, bool) {
	if c == nil || source.Website != "models.dev" {
		return modelsdevModel{}, false
	}
	pc := c.providers[source.Provider]
	if pc == nil {
		return modelsdevModel{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return modelsdevModel{}, false
	}
	if entry, ok := pc.byKey[strings.ToLower(id)]; ok {
		return entry, true
	}
	if entries := uniqueModelsdevEntries(pc.byID[strings.ToLower(id)]); len(entries) == 1 {
		return entries[0], true
	}
	if strings.Count(id, "/") != 1 {
		return modelsdevModel{}, false
	}
	bare := id[strings.IndexByte(id, '/')+1:]
	if entries := uniqueModelsdevEntries(pc.byBare[strings.ToLower(bare)]); len(entries) == 1 {
		return entries[0], true
	}
	return modelsdevModel{}, false
}

func uniqueModelsdevEntries(entries []modelsdevModel) []modelsdevModel {
	seen := map[string]struct{}{}
	out := make([]modelsdevModel, 0, len(entries))
	for _, entry := range entries {
		key := strings.ToLower(entry.FullKey)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullKey < out[j].FullKey })
	return out
}
