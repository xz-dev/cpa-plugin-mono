package plugin

type ThinkingConfig struct {
	Levels []string `json:"levels,omitempty" yaml:"levels,omitempty"`
}

type ModelRef struct {
	Name             string          `json:"name" yaml:"name"`
	Alias            string          `json:"alias,omitempty" yaml:"alias,omitempty"`
	DisplayName      string          `json:"display-name,omitempty" yaml:"display-name,omitempty"`
	MaxContextLength int             `json:"max-context-length,omitempty" yaml:"max-context-length,omitempty"`
	MaxInputTokens   int             `json:"max-input-tokens,omitempty" yaml:"max-input-tokens,omitempty"`
	MaxOutputTokens  int             `json:"max-output-tokens,omitempty" yaml:"max-output-tokens,omitempty"`
	Thinking         *ThinkingConfig `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	InputModalities  []string        `json:"input-modalities,omitempty" yaml:"input-modalities,omitempty"`
	OutputModalities []string        `json:"output-modalities,omitempty" yaml:"output-modalities,omitempty"`
}

func matchAny(id string, patterns compiledProvider) bool {
	if len(patterns.Patterns) == 0 {
		return false
	}
	for _, re := range patterns.Patterns {
		if re.MatchString(id) {
			return true
		}
	}
	return false
}

func filterIDs(ids []string, spec compiledProvider) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		matched := matchAny(id, spec)
		keep := false
		switch spec.Mode {
		case ModeExclude:
			keep = !matched
			if len(spec.Patterns) == 0 {
				keep = true
			}
		default: // include
			keep = matched
		}
		if keep {
			out = append(out, id)
		}
	}
	return out
}

func mergeModels(existing []ModelRef, ids []string, keepAliases bool) []ModelRef {
	aliasByName := map[string]ModelRef{}
	for _, m := range existing {
		if m.Name == "" {
			continue
		}
		aliasByName[m.Name] = m
	}
	out := make([]ModelRef, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ref := ModelRef{Name: id, Alias: id}
		if prev, ok := aliasByName[id]; ok && keepAliases {
			if prev.Alias != "" {
				ref.Alias = prev.Alias
			}
			ref.DisplayName = prev.DisplayName
			ref.MaxContextLength = prev.MaxContextLength
			ref.MaxInputTokens = prev.MaxInputTokens
			ref.MaxOutputTokens = prev.MaxOutputTokens
			ref.Thinking = prev.Thinking
			ref.InputModalities = prev.InputModalities
			ref.OutputModalities = prev.OutputModalities
		}
		out = append(out, ref)
	}
	return out
}

func applyModelOverrides(models []ModelRef, overrides map[string]ModelOverride) {
	for i := range models {
		override, ok := overrides[models[i].Name]
		if !ok {
			continue
		}
		if override.MaxContextLength > 0 {
			models[i].MaxContextLength = override.MaxContextLength
		}
		if override.MaxInputTokens > 0 {
			models[i].MaxInputTokens = override.MaxInputTokens
		}
		if override.MaxOutputTokens > 0 {
			models[i].MaxOutputTokens = override.MaxOutputTokens
		}
		if len(override.ThinkingLevels) > 0 {
			models[i].Thinking = &ThinkingConfig{Levels: append([]string(nil), override.ThinkingLevels...)}
		}
	}
}
