package plugin

type ThinkingConfig struct {
	Levels []string `json:"levels,omitempty"`
}

type ModelRef struct {
	Name             string          `json:"name"`
	Alias            string          `json:"alias,omitempty"`
	DisplayName      string          `json:"display-name,omitempty"`
	Thinking         *ThinkingConfig `json:"thinking,omitempty"`
	InputModalities  []string        `json:"input-modalities,omitempty"`
	OutputModalities []string        `json:"output-modalities,omitempty"`
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
			ref.Thinking = prev.Thinking
			ref.InputModalities = prev.InputModalities
			ref.OutputModalities = prev.OutputModalities
		}
		out = append(out, ref)
	}
	return out
}
