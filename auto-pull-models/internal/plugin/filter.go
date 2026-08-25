package plugin

func matchAny(id string, spec compiledChannel) bool {
	for _, re := range spec.Patterns {
		if re.MatchString(id) {
			return true
		}
	}
	return false
}

func filterIDs(ids []string, spec compiledChannel) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		matched := matchAny(id, spec)
		keep := matched
		if spec.Mode == ModeExclude {
			keep = !matched || len(spec.Patterns) == 0
		}
		if keep {
			out = append(out, id)
		}
	}
	return out
}
