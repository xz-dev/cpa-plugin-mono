package plugin

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

type ChannelResult struct {
	Kind            string                `json:"kind"`
	Selector        ChannelSelector       `json:"selector"`
	Enabled         bool                  `json:"enabled"`
	Models          int                   `json:"models"`
	Patches         int                   `json:"patches"`
	Skipped         bool                  `json:"skipped,omitempty"`
	Unchanged       bool                  `json:"unchanged,omitempty"`
	DryRun          bool                  `json:"dry_run,omitempty"`
	Metadata        []ModelMetadataResult `json:"metadata,omitempty"`
	CatalogErrors   []string              `json:"catalog_errors,omitempty"`
	ThinkingMatched int                   `json:"thinking_matched,omitempty"`
	ThinkingMissed  int                   `json:"thinking_missed,omitempty"`
	Error           string                `json:"error,omitempty"`
}

type SyncReport struct {
	At       time.Time       `json:"at"`
	OK       bool            `json:"ok"`
	DryRun   bool            `json:"dry_run,omitempty"`
	Channels []ChannelResult `json:"channels"`
	Error    string          `json:"error,omitempty"`
}
type ChannelSummary struct {
	Kind       string          `json:"kind"`
	Selector   ChannelSelector `json:"selector"`
	Disabled   bool            `json:"disabled"`
	Ready      bool            `json:"ready"`
	ModelCount int             `json:"model_count"`
}

func (s *Service) ListChannelSummaries(key string) ([]ChannelSummary, error) {
	channels, err := s.listModelChannels(key)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelSummary, 0, len(channels))
	for _, channel := range channels {
		if channel.Kind != KindOpenAI && channel.Kind != KindClaude {
			continue
		}
		selector, err := normalizeSelector(channel.Kind, channel.Selector)
		if err != nil {
			continue
		}
		out = append(out, ChannelSummary{Kind: channel.Kind, Selector: selector, Disabled: channel.Disabled, Ready: channel.Ready, ModelCount: len(channel.Models)})
	}
	return out, nil
}

func (s *Service) run(key, only string, dryRun bool, override *runtimeConfig) SyncReport {
	report := SyncReport{At: time.Now().UTC(), OK: true, DryRun: dryRun}
	cfg := s.Current()
	if override != nil {
		cfg = *override
	}
	if strings.TrimSpace(key) == "" {
		report.OK = false
		report.Error = "management key is required"
		return report
	}
	channels, err := s.listModelChannels(key)
	if err != nil {
		report.OK = false
		report.Error = err.Error()
		return report
	}
	needModelparams, needModelsdev := false, false
	for _, spec := range cfg.Channels {
		if !spec.Enabled {
			continue
		}
		for _, source := range spec.MetadataSources {
			needModelparams = needModelparams || source.Website == "modelparams.dev"
			needModelsdev = needModelsdev || source.Website == "models.dev"
		}
	}
	var modelparams *modelparamsCatalog
	var modelparamsErr error
	var modelsdev *modelsdevCatalog
	var modelsdevErr error
	if needModelparams {
		modelparams, modelparamsErr = s.fetchModelparamsCatalog(cfg.ModelparamsURL)
	}
	if needModelsdev {
		modelsdev, modelsdevErr = s.fetchModelsdevCatalog(cfg.ModelsdevURL)
	}
	for _, spec := range cfg.Channels {
		keyID := selectorKey(spec.Kind, spec.Selector)
		if only != "" && only != keyID && !strings.EqualFold(only, spec.Selector.Name) {
			continue
		}
		result := ChannelResult{Kind: spec.Kind, Selector: spec.Selector, Enabled: spec.Enabled, DryRun: dryRun}
		if !spec.Enabled {
			result.Skipped = true
			report.Channels = append(report.Channels, result)
			continue
		}
		channel, err := matchChannel(channels, spec)
		if err != nil {
			result.Error = err.Error()
			report.OK = false
			report.Channels = append(report.Channels, result)
			continue
		}
		result.Models = len(channel.Models)
		if channel.Disabled || !channel.Ready {
			result.Skipped = true
			result.Error = "channel is disabled or not ready"
			report.OK = false
			report.Channels = append(report.Channels, result)
			continue
		}
		entries, catalogErr := s.fetchChannelCatalog(key, channel, spec)
		if catalogErr != nil {
			result.CatalogErrors = append(result.CatalogErrors, catalogErr.Error())
		}
		byID := map[string]upstreamEntry{}
		for _, entry := range entries {
			byID[entry.ID] = entry
		}
		models := cloneModels(channel.Models)
		result.Metadata, result.ThinkingMatched, result.ThinkingMissed = enrichModels(models, byID, spec, modelparams, modelparamsErr, modelsdev, modelsdevErr)
		result.CatalogErrors = append(result.CatalogErrors, sourceErrors(spec, modelparamsErr, modelsdevErr)...)
		patches, err := buildModelPatches(channel.Models, models, result.Metadata)
		if err != nil {
			result.Error = err.Error()
			report.OK = false
			report.Channels = append(report.Channels, result)
			continue
		}
		result.Patches = len(patches)
		result.Unchanged = len(patches) == 0
		if !dryRun && len(patches) > 0 {
			if err := s.patchMetadata(key, channel, patches); err != nil {
				result.Error = err.Error()
				report.OK = false
			}
		}
		// Upstream errors stay visible and metadata from other configured sources may still patch existing models.
		if catalogErr != nil && len(spec.MetadataSources) == 0 {
			report.OK = false
			result.Error = catalogErr.Error()
		}
		report.Channels = append(report.Channels, result)
	}
	if only != "" && len(report.Channels) == 0 {
		report.OK = false
		report.Error = fmt.Sprintf("configured channel %q not found", only)
	}
	return report
}

func (s *Service) Sync(key, only string) SyncReport { return s.run(key, only, false, nil) }
func (s *Service) Preview(key, only string, override *runtimeConfig) SyncReport {
	return s.run(key, only, true, override)
}

func cloneModels(models []ModelRef) []ModelRef {
	out := make([]ModelRef, len(models))
	copy(out, models)
	for i := range out {
		if out[i].Thinking != nil {
			out[i].Thinking = &ThinkingConfig{Levels: append([]string(nil), out[i].Thinking.Levels...)}
		}
		out[i].InputModalities = append([]string(nil), out[i].InputModalities...)
		out[i].OutputModalities = append([]string(nil), out[i].OutputModalities...)
	}
	return out
}

func buildModelPatches(before, after []ModelRef, reports []ModelMetadataResult) ([]ModelPatch, error) {
	beforeByName := map[string]ModelRef{}
	for _, model := range before {
		if _, exists := beforeByName[model.Name]; exists {
			return nil, fmt.Errorf("model %q is ambiguous", model.Name)
		}
		beforeByName[model.Name] = model
	}
	reportByName := map[string]ModelMetadataResult{}
	for _, report := range reports {
		reportByName[report.Model] = report
	}
	patches := make([]ModelPatch, 0)
	for _, model := range after {
		old, exists := beforeByName[model.Name]
		if !exists {
			continue
		}
		status := map[string]string{}
		for _, field := range reportByName[model.Name].Fields {
			status[field.Field] = field.Status
		}
		fields := map[string]FieldPatch{}
		add := func(name string, oldValue, newValue any) {
			if reflect.DeepEqual(oldValue, newValue) {
				return
			}
			mode := ""
			switch status[name] {
			case "upstream", "authoritative", "override":
				mode = "replace"
			case "completed":
				mode = "if-empty"
			default:
				return
			}
			fields[name] = FieldPatch{Mode: mode, Value: newValue}
		}
		oldThinking, newThinking := []string(nil), []string(nil)
		if old.Thinking != nil {
			oldThinking = old.Thinking.Levels
		}
		if model.Thinking != nil {
			newThinking = model.Thinking.Levels
		}
		add("thinking.levels", oldThinking, newThinking)
		add("max-context-length", old.MaxContextLength, model.MaxContextLength)
		add("max-input-tokens", old.MaxInputTokens, model.MaxInputTokens)
		add("max-output-tokens", old.MaxOutputTokens, model.MaxOutputTokens)
		add("input-modalities", old.InputModalities, model.InputModalities)
		add("output-modalities", old.OutputModalities, model.OutputModalities)
		if len(fields) > 0 {
			patches = append(patches, ModelPatch{Model: model.Name, Fields: fields})
		}
	}
	return patches, nil
}
