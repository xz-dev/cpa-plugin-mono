package plugin

import (
	"sort"
	"strings"
)

var metadataFieldNames = []string{
	"thinking.levels",
	"max-context-length",
	"max-input-tokens",
	"max-output-tokens",
	"input-modalities",
	"output-modalities",
}

type MetadataFieldResult struct {
	Field        string   `json:"field"`
	Value        any      `json:"value,omitempty"`
	Source       string   `json:"source,omitempty"`
	Status       string   `json:"status"`
	Reason       string   `json:"reason,omitempty"`
	TriedSources []string `json:"tried_sources,omitempty"`
}

type ModelMetadataResult struct {
	Model  string                `json:"model"`
	Fields []MetadataFieldResult `json:"fields"`
}

type MetadataSourceOption struct {
	ID       string `json:"id"`
	Website  string `json:"website"`
	Provider string `json:"provider"`
	AuthType string `json:"auth_type,omitempty"`
	Label    string `json:"label"`
}

type MetadataSourcesResponse struct {
	Sources []MetadataSourceOption `json:"sources"`
	Errors  map[string]string      `json:"errors,omitempty"`
}

type fieldState struct {
	value  any
	source string
	status string
	reason string
	set    bool
	tried  []string
}

func (s *Service) ListMetadataSources() MetadataSourcesResponse {
	response := MetadataSourcesResponse{Errors: map[string]string{}}
	modelparams, modelparamsErr := s.fetchModelparamsCatalog(s.Current().ModelparamsURL)
	if modelparamsErr != nil {
		response.Errors["modelparams.dev"] = modelparamsErr.Error()
	} else {
		for _, source := range modelparams.sources() {
			response.Sources = append(response.Sources, MetadataSourceOption{
				ID: source.ID, Website: source.Website, Provider: source.Provider, AuthType: source.AuthType,
				Label: source.Website + " / " + source.Provider + " / " + source.AuthType,
			})
		}
	}
	modelsdev, modelsdevErr := s.fetchModelsdevCatalog(s.Current().ModelsdevURL)
	if modelsdevErr != nil {
		response.Errors["models.dev"] = modelsdevErr.Error()
	} else {
		for _, source := range modelsdev.sources() {
			response.Sources = append(response.Sources, MetadataSourceOption{
				ID: source.ID, Website: source.Website, Provider: source.Provider,
				Label: source.Website + " / " + source.Provider,
			})
		}
	}
	if len(response.Errors) == 0 {
		response.Errors = nil
	}
	return response
}

func (c *modelparamsCatalog) sources() []metadataSource {
	seen := map[string]metadataSource{}
	if c != nil {
		for _, entry := range c.byKey {
			id := "modelparams.dev/" + entry.Provider + "/" + entry.AuthType
			seen[id] = metadataSource{ID: id, Website: "modelparams.dev", Provider: entry.Provider, AuthType: entry.AuthType}
		}
	}
	return sortedMetadataSources(seen)
}

func (c *modelsdevCatalog) sources() []metadataSource {
	seen := map[string]metadataSource{}
	if c != nil {
		for provider := range c.providers {
			id := "models.dev/" + provider
			seen[id] = metadataSource{ID: id, Website: "models.dev", Provider: provider}
		}
	}
	return sortedMetadataSources(seen)
}

func sortedMetadataSources(seen map[string]metadataSource) []metadataSource {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]metadataSource, 0, len(ids))
	for _, id := range ids {
		out = append(out, seen[id])
	}
	return out
}

func enrichModels(models []ModelRef, byID map[string]upstreamEntry, spec compiledChannel, modelparams *modelparamsCatalog, modelparamsErr error, modelsdev *modelsdevCatalog, modelsdevErr error) (reports []ModelMetadataResult, matched, missed int) {
	reports = make([]ModelMetadataResult, 0, len(models))
	thinkingRequested := spec.UpstreamMeta
	hasModelsdevSource := false
	for _, source := range spec.MetadataSources {
		if source.Website == "modelparams.dev" {
			thinkingRequested = true
		}
		if source.Website == "models.dev" {
			hasModelsdevSource = true
		}
	}
	for i := range models {
		states := initialFieldStates(models[i])
		thinkingEnriched := false
		upstream := byID[models[i].Name]
		if spec.UpstreamMeta {
			for _, field := range []string{"thinking.levels", "input-modalities", "output-modalities"} {
				addFieldAttempt(states, field, "upstream /models")
			}
			for _, field := range []string{"max-context-length", "max-output-tokens"} {
				if !states[field].set {
					addFieldAttempt(states, field, "upstream /models")
				}
			}
			if spec.Kind == KindClaude && !states["max-input-tokens"].set {
				addFieldAttempt(states, "max-input-tokens", "upstream /models")
			}
			applyUpstreamFields(&models[i], upstream, states)
			thinkingEnriched = len(upstream.Efforts) > 0
		} else if hasModelsdevSource {
			applyUpstreamLimits(&models[i], upstream, states)
		}
		for _, source := range spec.MetadataSources {
			switch source.Website {
			case "modelparams.dev":
				thinkingOpen := !states["thinking.levels"].set || states["thinking.levels"].status == "preserved"
				outputOpen := !states["max-output-tokens"].set || states["max-output-tokens"].status == "preserved"
				if thinkingOpen {
					addFieldAttempt(states, "thinking.levels", source.ID)
				}
				if outputOpen {
					addFieldAttempt(states, "max-output-tokens", source.ID)
				}
				if !thinkingOpen && !outputOpen || modelparamsErr != nil {
					continue
				}
				entry, ok := modelparams.lookupSource(source, models[i].Name)
				if !ok {
					continue
				}
				if thinkingOpen {
					if levels := extractThinkingLevels(entry.Params); len(levels) > 0 {
						models[i].Thinking = &ThinkingConfig{Levels: levels}
						setField(states, "thinking.levels", append([]string(nil), levels...), source.ID, "authoritative", "first configured authoritative source supplying this field")
						thinkingEnriched = true
					}
				}
				if outputOpen {
					if limit := extractMaxOutputTokens(entry.Params); limit > 0 {
						models[i].MaxOutputTokens = limit
						setField(states, "max-output-tokens", limit, source.ID, "authoritative", "concrete generation-length range.max from authoritative source")
					}
				}
			case "models.dev":
				openFields := make([]string, 0, 4)
				for _, field := range []string{"max-context-length", "max-output-tokens", "input-modalities", "output-modalities"} {
					if !states[field].set {
						openFields = append(openFields, field)
						addFieldAttempt(states, field, source.ID)
					}
				}
				if len(openFields) == 0 || modelsdevErr != nil {
					continue
				}
				entry, ok := modelsdev.lookupSource(source, models[i].Name)
				if !ok {
					continue
				}
				fillModelsdevFields(&models[i], entry, source.ID, states)
			}
		}
		applyOverridesWithProvenance(&models[i], spec.Overrides[models[i].Name], states)
		fields := make([]MetadataFieldResult, 0, len(metadataFieldNames))
		for _, name := range metadataFieldNames {
			state := states[name]
			if !state.set {
				reason := "no configured source supports this field"
				if len(state.tried) > 0 {
					reason = "tried sources supplied no value"
				} else if name == "max-input-tokens" {
					reason = "no automatic source supports this field; preserve an existing value or use manual override"
				}
				if errors := attemptedSourceErrors(state.tried, modelparamsErr, modelsdevErr); len(errors) > 0 {
					reason += "; catalog errors: " + strings.Join(errors, "; ")
				}
				state = &fieldState{status: "skipped", reason: reason, tried: state.tried}
			}
			fields = append(fields, MetadataFieldResult{
				Field: name, Value: state.value, Source: state.source, Status: state.status, Reason: state.reason,
				TriedSources: append([]string(nil), state.tried...),
			})
		}
		if thinkingRequested {
			if thinkingEnriched {
				matched++
			} else {
				missed++
			}
		}
		reports = append(reports, ModelMetadataResult{Model: models[i].Name, Fields: fields})
	}
	return reports, matched, missed
}

func initialFieldStates(model ModelRef) map[string]*fieldState {
	states := map[string]*fieldState{}
	for _, field := range metadataFieldNames {
		states[field] = &fieldState{}
	}
	preserve := func(field string, value any) {
		setField(states, field, value, "existing config", "preserved", "copied from existing CPA model configuration")
	}
	if model.Thinking != nil && len(model.Thinking.Levels) > 0 {
		preserve("thinking.levels", append([]string(nil), model.Thinking.Levels...))
	}
	if model.MaxContextLength > 0 {
		preserve("max-context-length", model.MaxContextLength)
	}
	if model.MaxInputTokens > 0 {
		preserve("max-input-tokens", model.MaxInputTokens)
	}
	if model.MaxOutputTokens > 0 {
		preserve("max-output-tokens", model.MaxOutputTokens)
	}
	if len(model.InputModalities) > 0 {
		preserve("input-modalities", append([]string(nil), model.InputModalities...))
	}
	if len(model.OutputModalities) > 0 {
		preserve("output-modalities", append([]string(nil), model.OutputModalities...))
	}
	return states
}

func applyUpstreamLimits(model *ModelRef, upstream upstreamEntry, states map[string]*fieldState) {
	if !states["max-context-length"].set {
		addFieldAttempt(states, "max-context-length", "upstream /models")
		if upstream.Context > 0 {
			model.MaxContextLength = upstream.Context
			setField(states, "max-context-length", upstream.Context, "upstream /models", "upstream", "supplied by upstream metadata before models.dev fallback")
		}
	}
	if !states["max-output-tokens"].set {
		addFieldAttempt(states, "max-output-tokens", "upstream /models")
		if upstream.MaxTokens > 0 {
			model.MaxOutputTokens = upstream.MaxTokens
			setField(states, "max-output-tokens", upstream.MaxTokens, "upstream /models", "upstream", "supplied by upstream metadata before models.dev fallback")
		}
	}
}

func applyUpstreamFields(model *ModelRef, upstream upstreamEntry, states map[string]*fieldState) {
	if len(upstream.Efforts) > 0 {
		model.Thinking = &ThinkingConfig{Levels: append([]string(nil), upstream.Efforts...)}
		setField(states, "thinking.levels", append([]string(nil), upstream.Efforts...), "upstream /models", "upstream", "supplied by enabled upstream metadata")
	}
	if model.MaxContextLength == 0 && upstream.Context > 0 {
		model.MaxContextLength = upstream.Context
		setField(states, "max-context-length", upstream.Context, "upstream /models", "upstream", "supplied by enabled upstream metadata")
	}
	if model.MaxInputTokens == 0 && upstream.ClaudeMaxInput > 0 {
		model.MaxInputTokens = upstream.ClaudeMaxInput
		setField(states, "max-input-tokens", upstream.ClaudeMaxInput, "upstream /models", "upstream", "supplied by enabled upstream metadata")
	}
	if model.MaxOutputTokens == 0 && upstream.MaxTokens > 0 {
		model.MaxOutputTokens = upstream.MaxTokens
		setField(states, "max-output-tokens", upstream.MaxTokens, "upstream /models", "upstream", "supplied by enabled upstream metadata")
	}
	if modalities := cpaModalities(upstream.Input); len(modalities) > 0 {
		model.InputModalities = modalities
		setField(states, "input-modalities", append([]string(nil), modalities...), "upstream /models", "upstream", "supplied by enabled upstream metadata")
	}
	if modalities := cpaModalities(upstream.Output); len(modalities) > 0 {
		model.OutputModalities = modalities
		setField(states, "output-modalities", append([]string(nil), modalities...), "upstream /models", "upstream", "supplied by enabled upstream metadata")
	}
}

func fillModelsdevFields(model *ModelRef, entry modelsdevModel, source string, states map[string]*fieldState) {
	fill := func(field string, value any, apply func()) {
		if states[field].set {
			return
		}
		apply()
		setField(states, field, value, source, "completed", "secondary source filled a field missing from authoritative and earlier sources")
	}
	if entry.Context > 0 {
		fill("max-context-length", entry.Context, func() { model.MaxContextLength = entry.Context })
	}
	if entry.MaxOut > 0 {
		fill("max-output-tokens", entry.MaxOut, func() { model.MaxOutputTokens = entry.MaxOut })
	}
	if modalities := cpaModalities(entry.Input); len(modalities) > 0 {
		fill("input-modalities", append([]string(nil), modalities...), func() { model.InputModalities = modalities })
	}
	if modalities := cpaModalities(entry.Output); len(modalities) > 0 {
		fill("output-modalities", append([]string(nil), modalities...), func() { model.OutputModalities = modalities })
	}
}

func applyOverridesWithProvenance(model *ModelRef, override ModelOverride, states map[string]*fieldState) {
	set := func(field string, value any, apply func()) {
		apply()
		setField(states, field, value, "manual override", "override", "explicit per-model override applied last")
	}
	if override.MaxContextLength > 0 {
		set("max-context-length", override.MaxContextLength, func() { model.MaxContextLength = override.MaxContextLength })
	}
	if override.MaxInputTokens > 0 {
		set("max-input-tokens", override.MaxInputTokens, func() { model.MaxInputTokens = override.MaxInputTokens })
	}
	if override.MaxOutputTokens > 0 {
		set("max-output-tokens", override.MaxOutputTokens, func() { model.MaxOutputTokens = override.MaxOutputTokens })
	}
	if len(override.ThinkingLevels) > 0 {
		levels := append([]string(nil), override.ThinkingLevels...)
		set("thinking.levels", levels, func() { model.Thinking = &ThinkingConfig{Levels: levels} })
	}
	if len(override.InputModalities) > 0 {
		values := append([]string(nil), override.InputModalities...)
		set("input-modalities", values, func() { model.InputModalities = values })
	}
	if len(override.OutputModalities) > 0 {
		values := append([]string(nil), override.OutputModalities...)
		set("output-modalities", values, func() { model.OutputModalities = values })
	}
}

func setField(states map[string]*fieldState, field string, value any, source, status, reason string) {
	state := states[field]
	state.value, state.source, state.status, state.reason, state.set = value, source, status, reason, true
}

func addFieldAttempt(states map[string]*fieldState, field, source string) {
	state := states[field]
	for _, existing := range state.tried {
		if existing == source {
			return
		}
	}
	state.tried = append(state.tried, source)
}

func attemptedSourceErrors(tried []string, modelparamsErr, modelsdevErr error) []string {
	needModelparams, needModelsdev := false, false
	for _, source := range tried {
		needModelparams = needModelparams || strings.HasPrefix(source, "modelparams.dev/")
		needModelsdev = needModelsdev || strings.HasPrefix(source, "models.dev/")
	}
	var errors []string
	if needModelparams && modelparamsErr != nil {
		errors = append(errors, modelparamsErr.Error())
	}
	if needModelsdev && modelsdevErr != nil {
		errors = append(errors, modelsdevErr.Error())
	}
	return errors
}

func sourceErrors(spec compiledChannel, modelparamsErr, modelsdevErr error) []string {
	need := map[string]bool{}
	for _, source := range spec.MetadataSources {
		need[source.Website] = true
	}
	var errors []string
	if need["modelparams.dev"] && modelparamsErr != nil {
		errors = append(errors, modelparamsErr.Error())
	}
	if need["models.dev"] && modelsdevErr != nil {
		errors = append(errors, modelsdevErr.Error())
	}
	return errors
}
