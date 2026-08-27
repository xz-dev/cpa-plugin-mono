package plugin

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// modelsdevModel carries the limit/modality metadata we use from models.dev.
type modelsdevModel struct {
	FullKey  string   `json:"full_key"`
	Provider string   `json:"provider"`
	ID       string   `json:"id"`
	Context  int      `json:"context"`
	MaxOut   int      `json:"max_output"`
	Input    []string `json:"input"`
	Output   []string `json:"output"`
}

type modelsdevProviderCatalog struct {
	ByKey  map[string]modelsdevModel `json:"by_key"`
	byID   map[string][]modelsdevModel
	byBare map[string][]modelsdevModel
}

type modelsdevCatalog struct {
	Providers map[string]*modelsdevProviderCatalog `json:"providers"`
}

func parseModelsdevCatalog(raw []byte) (*modelsdevCatalog, error) {
	if err := validateCatalogJSON(raw); err != nil {
		return nil, fmt.Errorf("modelsdev: invalid catalog")
	}
	providers, err := catalogObject(raw)
	if err != nil {
		return nil, fmt.Errorf("modelsdev: invalid catalog")
	}
	cat := &modelsdevCatalog{Providers: map[string]*modelsdevProviderCatalog{}}
	for rawProvider, rawPayload := range providers {
		provider := strings.ToLower(strings.TrimSpace(rawProvider))
		if provider == "" || rawProvider != strings.TrimSpace(rawProvider) || hasControl(provider) {
			return nil, fmt.Errorf("modelsdev: invalid provider")
		}
		payload, err := catalogObject(rawPayload)
		if err != nil || hasFoldedCatalogField(payload, "models") {
			return nil, fmt.Errorf("modelsdev: invalid provider")
		}
		rawModels, ok := payload["models"]
		if !ok {
			continue
		}
		var models map[string]json.RawMessage
		if json.Unmarshal(rawModels, &models) != nil || models == nil {
			return nil, fmt.Errorf("modelsdev: invalid models")
		}
		pc := &modelsdevProviderCatalog{ByKey: map[string]modelsdevModel{}, byID: map[string][]modelsdevModel{}, byBare: map[string][]modelsdevModel{}}
		for rawKey, rawModel := range models {
			key := strings.TrimSpace(rawKey)
			if key == "" || key != rawKey || hasControl(key) {
				return nil, fmt.Errorf("modelsdev: invalid model key")
			}
			fields, err := catalogObject(rawModel)
			if err != nil || hasAnyFoldedCatalogField(fields, "id", "modalities", "limit") {
				return nil, fmt.Errorf("modelsdev: invalid model")
			}
			if rawModalities, ok := fields["modalities"]; ok {
				modalityFields, err := catalogObject(rawModalities)
				if err != nil || hasAnyFoldedCatalogField(modalityFields, "input", "output") {
					return nil, fmt.Errorf("modelsdev: invalid modalities")
				}
			}
			if rawLimit, ok := fields["limit"]; ok {
				limitFields, err := catalogObject(rawLimit)
				if err != nil || hasAnyFoldedCatalogField(limitFields, "context", "output") {
					return nil, fmt.Errorf("modelsdev: invalid limit")
				}
			}
			var model struct {
				ID         string `json:"id"`
				Modalities struct {
					Input  []string `json:"input"`
					Output []string `json:"output"`
				} `json:"modalities"`
				Limit struct {
					Context int `json:"context"`
					Output  int `json:"output"`
				} `json:"limit"`
			}
			if json.Unmarshal(rawModel, &model) != nil || model.Limit.Context < 0 || model.Limit.Output < 0 {
				return nil, fmt.Errorf("modelsdev: invalid model")
			}
			id := strings.TrimSpace(model.ID)
			if id == "" {
				id = key
			}
			if id == "" || id != strings.TrimSpace(id) || hasControl(id) {
				return nil, fmt.Errorf("modelsdev: invalid model ID")
			}
			entry := modelsdevModel{FullKey: key, Provider: provider, ID: id, Context: model.Limit.Context, MaxOut: model.Limit.Output, Input: model.Modalities.Input, Output: model.Modalities.Output}
			if entry.Context <= 0 && entry.MaxOut <= 0 && len(entry.Input) == 0 && len(entry.Output) == 0 {
				continue
			}
			pc.ByKey[strings.ToLower(entry.FullKey)] = entry
			pc.byID[strings.ToLower(entry.ID)] = append(pc.byID[strings.ToLower(entry.ID)], entry)
			bare := entry.ID
			if index := strings.LastIndex(bare, "/"); index >= 0 {
				bare = bare[index+1:]
			}
			pc.byBare[strings.ToLower(bare)] = append(pc.byBare[strings.ToLower(bare)], entry)
		}
		if len(pc.ByKey) > 0 {
			if _, duplicate := cat.Providers[provider]; duplicate {
				return nil, fmt.Errorf("modelsdev: duplicate provider")
			}
			cat.Providers[provider] = pc
		}
	}
	if len(cat.Providers) == 0 {
		return nil, fmt.Errorf("modelsdev: empty catalog")
	}
	return cat, nil
}

func validModelsdevCatalog(catalog *modelsdevCatalog) bool {
	if catalog == nil || len(catalog.Providers) == 0 {
		return false
	}
	wire := make(map[string]any, len(catalog.Providers))
	for provider, providerCatalog := range catalog.Providers {
		if providerCatalog == nil || len(providerCatalog.ByKey) == 0 {
			return false
		}
		models := make(map[string]any, len(providerCatalog.ByKey))
		for _, entry := range providerCatalog.ByKey {
			models[entry.FullKey] = map[string]any{
				"id": entry.ID,
				"modalities": map[string]any{
					"input":  entry.Input,
					"output": entry.Output,
				},
				"limit": map[string]any{
					"context": entry.Context,
					"output":  entry.MaxOut,
				},
			}
		}
		wire[provider] = map[string]any{"models": models}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return false
	}
	reparsed, err := parseModelsdevCatalog(raw)
	if err != nil || len(reparsed.Providers) != len(catalog.Providers) {
		return false
	}
	for provider, providerCatalog := range catalog.Providers {
		if reparsedProvider := reparsed.Providers[provider]; reparsedProvider == nil || !reflect.DeepEqual(reparsedProvider.ByKey, providerCatalog.ByKey) {
			return false
		}
	}
	*catalog = *reparsed
	return true
}

func (c *modelsdevCatalog) lookupSource(source metadataSource, id string) (modelsdevModel, bool) {
	if c == nil || source.Website != "models.dev" {
		return modelsdevModel{}, false
	}
	pc := c.Providers[source.Provider]
	if pc == nil {
		return modelsdevModel{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return modelsdevModel{}, false
	}
	if entry, ok := pc.ByKey[strings.ToLower(id)]; ok {
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
