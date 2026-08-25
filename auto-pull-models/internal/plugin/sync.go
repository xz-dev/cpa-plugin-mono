package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ProviderResult struct {
	Name             string                `json:"name"`
	Enabled          bool                  `json:"enabled"`
	Fetched          int                   `json:"fetched"`
	Kept             int                   `json:"kept"`
	Dropped          int                   `json:"dropped"`
	Written          int                   `json:"written"`
	Current          int                   `json:"current"`
	Skipped          bool                  `json:"skipped,omitempty"`
	Unchanged        bool                  `json:"unchanged,omitempty"`
	DryRun           bool                  `json:"dry_run,omitempty"`
	KeptSamples      []string              `json:"kept_samples,omitempty"`
	DroppedSamples   []string              `json:"dropped_samples,omitempty"`
	ThinkingMatched  int                   `json:"thinking_matched,omitempty"`
	ThinkingMissed   int                   `json:"thinking_missed,omitempty"`
	ThinkingSamples  []string              `json:"thinking_samples,omitempty"`
	Metadata         []ModelMetadataResult `json:"metadata,omitempty"`
	ModelparamsError string                `json:"modelparams_error,omitempty"`
	CatalogErrors    []string              `json:"catalog_errors,omitempty"`
	Error            string                `json:"error,omitempty"`
}

type SyncReport struct {
	At        time.Time        `json:"at"`
	OK        bool             `json:"ok"`
	DryRun    bool             `json:"dry_run,omitempty"`
	Providers []ProviderResult `json:"providers"`
	Error     string           `json:"error,omitempty"`
}

type CompatSummary struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Disabled   bool   `json:"disabled"`
	ModelCount int    `json:"model_count"`
}

func (s *Service) ListCompatSummaries(key string) ([]CompatSummary, error) {
	list, err := s.listCompat(key)
	if err != nil {
		return nil, err
	}
	out := make([]CompatSummary, 0, len(list))
	for _, p := range list {
		out = append(out, CompatSummary{
			Name:       p.Name,
			BaseURL:    p.BaseURL,
			Disabled:   p.Disabled,
			ModelCount: len(p.Models),
		})
	}
	return out, nil
}

func (s *Service) run(key, onlyProvider string, dryRun bool, override *runtimeConfig) SyncReport {
	report := SyncReport{At: time.Now().UTC(), OK: true, DryRun: dryRun}
	cfg := s.cfg
	if override != nil {
		cfg = *override
	}
	if strings.TrimSpace(key) == "" {
		report.OK = false
		report.Error = "management key is required"
		return report
	}
	var catalog *modelparamsCatalog
	var catalogErr error
	needCatalog := false
	var devCatalog *modelsdevCatalog
	var devErr error
	needDev := false
	for _, spec := range cfg.Providers {
		if onlyProvider != "" && !strings.EqualFold(spec.Name, onlyProvider) {
			continue
		}
		if !spec.Enabled {
			continue
		}
		for _, source := range spec.MetadataSources {
			if source.Website == "modelparams.dev" {
				needCatalog = true
			}
			if source.Website == "models.dev" {
				needDev = true
			}
		}
	}
	if needDev {
		devCatalog, devErr = s.fetchModelsdevCatalog(cfg.ModelsdevURL)
	}
	if needCatalog {
		catalog, catalogErr = s.fetchModelparamsCatalog(cfg.ModelparamsURL)
	}
	list, err := s.listCompat(key)
	if err != nil {
		report.OK = false
		report.Error = err.Error()
		return report
	}
	indexByName := map[string]int{}
	for i, p := range list {
		indexByName[strings.TrimSpace(p.Name)] = i
	}
	pendingWrites := map[string][]ModelRef{}
	fileMode := cfg.WriteMode == WriteModeFile
	fileModels := map[string][]ModelRef{}
	if fileMode {
		fileModels, err = readModelsFile(cfg.ConfigPath)
		if err != nil {
			report.OK = false
			report.Error = "file read: " + err.Error()
			return report
		}
	}

	for _, spec := range cfg.Providers {
		if onlyProvider != "" && !strings.EqualFold(spec.Name, onlyProvider) {
			continue
		}
		res := ProviderResult{Name: spec.Name, Enabled: spec.Enabled, DryRun: dryRun}
		if !spec.Enabled {
			res.Skipped = true
			report.Providers = append(report.Providers, res)
			continue
		}
		idx, ok := indexByName[spec.Name]
		if !ok {
			res.Error = "CPA 里没有这个 openai-compatibility provider，请先在 AI Providers 添加"
			report.OK = false
			report.Providers = append(report.Providers, res)
			continue
		}
		host := list[idx]
		currentModels := host.Models
		if fileMode {
			var found bool
			currentModels, found = fileModels[spec.Name]
			if !found {
				res.Error = "config.yaml 里没有这个 openai-compatibility provider"
				report.OK = false
				report.Providers = append(report.Providers, res)
				continue
			}
		}
		res.Current = len(currentModels)
		if host.Disabled {
			res.Skipped = true
			res.Error = "provider 已在 CPA 禁用"
			report.Providers = append(report.Providers, res)
			continue
		}
		url := modelsURL(host.BaseURL, spec.CodexManifest)
		if url == "" {
			res.Error = "provider 没有 base-url"
			report.OK = false
			report.Providers = append(report.Providers, res)
			continue
		}
		entries, err := s.fetchUpstreamCatalog(key, firstAuthIndex(host.APIKeyEntries), url)
		if err != nil {
			res.Error = err.Error()
			report.OK = false
			report.Providers = append(report.Providers, res)
			continue
		}
		ids := make([]string, 0, len(entries))
		byID := map[string]upstreamEntry{}
		for _, e := range entries {
			ids = append(ids, e.ID)
			byID[e.ID] = e
		}
		res.Fetched = len(ids)
		kept := filterIDs(ids, spec)
		dropped := difference(ids, kept)
		res.Kept = len(kept)
		res.Dropped = len(dropped)
		res.KeptSamples = sample(kept, 40)
		res.DroppedSamples = sample(dropped, 40)
		merged := mergeModels(currentModels, kept, cfg.KeepExistingAliases)
		res.Metadata, res.ThinkingMatched, res.ThinkingMissed = enrichModels(merged, byID, spec, catalog, catalogErr, devCatalog, devErr)
		res.ThinkingSamples = metadataSamples(res.Metadata)
		res.CatalogErrors = sourceErrors(spec, catalogErr, devErr)
		if catalogErr != nil {
			for _, source := range spec.MetadataSources {
				if source.Website == "modelparams.dev" {
					res.ModelparamsError = catalogErr.Error()
					break
				}
			}
		}
		res.Written = len(merged)
		if modelsEqual(currentModels, merged) {
			res.Unchanged = true
			report.Providers = append(report.Providers, res)
			continue
		}
		if dryRun {
			report.Providers = append(report.Providers, res)
			continue
		}
		if fileMode {
			pendingWrites[spec.Name] = merged
			report.Providers = append(report.Providers, res)
			continue
		}
		if err := s.patchCompatModels(key, idx, merged); err != nil {
			res.Error = err.Error()
			report.OK = false
			report.Providers = append(report.Providers, res)
			continue
		}
		report.Providers = append(report.Providers, res)
	}
	if !dryRun && len(pendingWrites) > 0 {
		if err := writeModelsFile(cfg.ConfigPath, pendingWrites); err != nil {
			report.OK = false
			report.Error = "file write: " + err.Error()
		}
	}
	if onlyProvider != "" && len(report.Providers) == 0 {
		report.OK = false
		report.Error = fmt.Sprintf("规则里没有 provider %q，请先勾选添加", onlyProvider)
	}
	return report
}

func (s *Service) Sync(key, onlyProvider string) SyncReport {
	return s.run(key, onlyProvider, false, nil)
}

func (s *Service) Preview(key, onlyProvider string, override *runtimeConfig) SyncReport {
	return s.run(key, onlyProvider, true, override)
}

func modelsEqual(a, b []ModelRef) bool {
	aSet := modelSet(a)
	bSet := modelSet(b)
	if len(aSet) != len(bSet) {
		return false
	}
	for model := range aSet {
		if _, ok := bSet[model]; !ok {
			return false
		}
	}
	return true
}

func modelSet(models []ModelRef) map[string]struct{} {
	set := make(map[string]struct{}, len(models))
	for _, model := range models {
		raw, _ := json.Marshal(model)
		set[string(raw)] = struct{}{}
	}
	return set
}

func difference(all, kept []string) []string {
	have := map[string]struct{}{}
	for _, id := range kept {
		have[id] = struct{}{}
	}
	out := make([]string, 0)
	for _, id := range all {
		if _, ok := have[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func sample(ids []string, n int) []string {
	if len(ids) <= n {
		return append([]string(nil), ids...)
	}
	return append([]string(nil), ids[:n]...)
}
