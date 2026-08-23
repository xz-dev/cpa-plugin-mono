package plugin

import (
	"fmt"
	"strings"
	"time"
)

type ProviderResult struct {
	Name             string   `json:"name"`
	Enabled          bool     `json:"enabled"`
	Fetched          int      `json:"fetched"`
	Kept             int      `json:"kept"`
	Dropped          int      `json:"dropped"`
	Written          int      `json:"written"`
	Current          int      `json:"current"`
	Skipped          bool     `json:"skipped,omitempty"`
	Unchanged        bool     `json:"unchanged,omitempty"`
	DryRun           bool     `json:"dry_run,omitempty"`
	KeptSamples      []string `json:"kept_samples,omitempty"`
	DroppedSamples   []string `json:"dropped_samples,omitempty"`
	ThinkingMatched  int      `json:"thinking_matched,omitempty"`
	ThinkingMissed   int      `json:"thinking_missed,omitempty"`
	ThinkingSamples  []string `json:"thinking_samples,omitempty"`
	ModelparamsError string   `json:"modelparams_error,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type SyncReport struct {
	At        time.Time        `json:"at"`
	OK        bool             `json:"ok"`
	DryRun    bool             `json:"dry_run,omitempty"`
	Providers []ProviderResult `json:"providers"`
	Error     string           `json:"error,omitempty"`
}

// enrichedModel is the resolved registry metadata for one model:
// upstream values win, models.dev fills only the gaps.
type enrichedModel struct {
	ID          string
	Name        string
	DisplayName string
	Context     int
	MaxOutput   int
	Thinking    []string
	Input       []string
	Output      []string
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
		if spec.Enabled && spec.Modelparams {
			needCatalog = true
		}
		if spec.Enabled && spec.Modelsdev {
			needDev = true
		}
		if spec.Enabled {
			break
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
		res.Current = len(host.Models)
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
		merged := mergeModels(host.Models, kept, cfg.KeepExistingAliases)
		if spec.UpstreamMeta {
			matched, missed, samples := applyUpstreamMeta(merged, byID)
			res.ThinkingMatched = matched
			res.ThinkingMissed = missed
			res.ThinkingSamples = samples
		}
		if spec.Modelparams {
			if catalogErr != nil {
				res.ModelparamsError = catalogErr.Error()
			} else {
				matched, missed, samples := applyModelparamsThinking(merged, catalog, spec.UpstreamMeta)
				if !spec.UpstreamMeta {
					res.ThinkingMatched = matched
					res.ThinkingMissed = missed
					res.ThinkingSamples = samples
				} else {
					res.ThinkingMatched += matched
					res.ThinkingMissed = missed
					if len(res.ThinkingSamples) < 12 {
						res.ThinkingSamples = append(res.ThinkingSamples, samples...)
					}
				}
			}
		}
		enriched := buildEnriched(merged, byID, devCatalog, devErr)
		res.Written = len(merged)
		if modelsEqual(host.Models, merged) {
			res.Unchanged = true
			if !dryRun {
				s.setEnriched(spec.Name, enriched)
			}
			report.Providers = append(report.Providers, res)
			continue
		}
		if dryRun {
			report.Providers = append(report.Providers, res)
			continue
		}
		if err := s.patchCompatModels(key, idx, merged); err != nil {
			res.Error = err.Error()
			report.OK = false
			report.Providers = append(report.Providers, res)
			continue
		}
		s.setEnriched(spec.Name, enriched)
		report.Providers = append(report.Providers, res)
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
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Alias != b[i].Alias || a[i].DisplayName != b[i].DisplayName {
			return false
		}
		if !thinkingEqual(a[i].Thinking, b[i].Thinking) {
			return false
		}
		if !stringSliceEqual(a[i].InputModalities, b[i].InputModalities) || !stringSliceEqual(a[i].OutputModalities, b[i].OutputModalities) {
			return false
		}
	}
	return true
}

func thinkingEqual(a, b *ThinkingConfig) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a.Levels) != len(b.Levels) {
		return false
	}
	for i := range a.Levels {
		if a.Levels[i] != b.Levels[i] {
			return false
		}
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func applyUpstreamMeta(models []ModelRef, byID map[string]upstreamEntry) (matched, missed int, samples []string) {
	for i := range models {
		e, ok := byID[models[i].Name]
		if !ok {
			missed++
			continue
		}
		if mods := cpaModalities(e.Input); len(mods) > 0 {
			models[i].InputModalities = mods
		}
		if mods := cpaModalities(e.Output); len(mods) > 0 {
			models[i].OutputModalities = mods
		}
		if len(e.Efforts) == 0 {
			missed++
			continue
		}
		models[i].Thinking = &ThinkingConfig{Levels: e.Efforts}
		matched++
		if len(samples) < 12 {
			line := models[i].Name + ": " + strings.Join(e.Efforts, ",")
			if e.Context > 0 {
				line += fmt.Sprintf(" ctx=%d", e.Context)
			}
			samples = append(samples, line)
		}
	}
	return
}

func applyModelparamsThinking(models []ModelRef, cat *modelparamsCatalog, skipFilled bool) (matched, missed int, samples []string) {
	for i := range models {
		if skipFilled && models[i].Thinking != nil && len(models[i].Thinking.Levels) > 0 {
			continue
		}
		levels, ok := cat.levelsFor(models[i].Name)
		if !ok {
			missed++
			continue
		}
		models[i].Thinking = &ThinkingConfig{Levels: levels}
		matched++
		if len(samples) < 12 {
			samples = append(samples, models[i].Name+": "+strings.Join(levels, ","))
		}
	}
	return
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

// buildEnriched resolves registry metadata per model. Field-level priority:
// upstream catalog first, then models.dev fills only the missing gaps.
func buildEnriched(models []ModelRef, byID map[string]upstreamEntry, dev *modelsdevCatalog, devErr error) []enrichedModel {
	out := make([]enrichedModel, 0, len(models))
	for _, m := range models {
		e := byID[m.Name]
		entry := enrichedModel{
			ID:          m.Alias,
			Name:        m.Name,
			DisplayName: m.DisplayName,
		}
		if entry.ID == "" {
			entry.ID = m.Name
		}
		var devEntry modelsdevModel
		var devOK bool
		if devErr == nil {
			devEntry, devOK = dev.lookup(m.Name)
		}
		if e.Context > 0 {
			entry.Context = e.Context
		} else if devOK {
			entry.Context = devEntry.Context
		}
		if e.MaxTokens > 0 {
			entry.MaxOutput = e.MaxTokens
		} else if devOK {
			entry.MaxOutput = devEntry.MaxOut
		}
		if m.Thinking != nil {
			entry.Thinking = m.Thinking.Levels
		}
		if len(m.InputModalities) > 0 {
			entry.Input = m.InputModalities
		} else if len(e.Input) > 0 {
			entry.Input = cpaModalities(e.Input)
		} else if devOK {
			entry.Input = cpaModalities(devEntry.Input)
		}
		if len(m.OutputModalities) > 0 {
			entry.Output = m.OutputModalities
		} else if len(e.Output) > 0 {
			entry.Output = cpaModalities(e.Output)
		} else if devOK {
			entry.Output = cpaModalities(devEntry.Output)
		}
		out = append(out, entry)
	}
	return out
}

func (s *Service) setEnriched(provider string, models []enrichedModel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enriched == nil {
		s.enriched = map[string][]enrichedModel{}
	}
	s.enriched[provider] = models
}
