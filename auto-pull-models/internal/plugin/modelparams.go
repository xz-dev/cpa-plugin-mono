package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const defaultModelparamsURL = "https://modelparams.dev/api/v1/models.json"

var cpaThinkingLevels = map[string]struct{}{
	"none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {},
	"xhigh": {}, "max": {}, "ultra": {}, "auto": {},
}

var effortParamPaths = []string{
	"reasoning.effort",
	"reasoning_effort",
	"output_config.effort",
	"generationConfig.thinkingConfig.thinkingLevel",
}

type modelparamsEntry struct {
	Provider string             `json:"provider"`
	AuthType string             `json:"authType"`
	Model    string             `json:"model"`
	Params   []modelparamsParam `json:"params"`
}

type modelparamsParam struct {
	Path   string `json:"path"`
	Group  string `json:"group"`
	Type   string `json:"type"`
	Values []any  `json:"values"`
}

type modelparamsCatalog struct {
	byKey   map[string]modelparamsEntry
	byModel map[string][]modelparamsEntry
}

func (s *Service) fetchModelparamsCatalog(url string) (*modelparamsCatalog, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		url = defaultModelparamsURL
	}
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "auto-pull-models")
	status, body, err := s.transport.Do(http.MethodGet, url, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("modelparams: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("modelparams HTTP %d %s", status, truncate(body, 200))
	}
	return parseModelparamsCatalog(body)
}

func parseModelparamsCatalog(raw []byte) (*modelparamsCatalog, error) {
	var wrapped struct {
		Models []modelparamsEntry `json:"models"`
	}
	var list []modelparamsEntry
	switch {
	case json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Models) > 0:
		list = wrapped.Models
	case json.Unmarshal(raw, &list) == nil && len(list) > 0:
	default:
		return nil, fmt.Errorf("modelparams: unrecognized catalog")
	}
	cat := &modelparamsCatalog{
		byKey:   map[string]modelparamsEntry{},
		byModel: map[string][]modelparamsEntry{},
	}
	for _, e := range list {
		e.Provider = strings.ToLower(strings.TrimSpace(e.Provider))
		e.Model = strings.TrimSpace(e.Model)
		e.AuthType = strings.ToLower(strings.TrimSpace(e.AuthType))
		if e.Provider == "" || e.Model == "" {
			continue
		}
		if e.AuthType == "" {
			e.AuthType = "api_key"
		}
		key := e.Provider + "/" + strings.ToLower(e.Model) + "/" + e.AuthType
		cat.byKey[key] = e
		mk := strings.ToLower(e.Model)
		cat.byModel[mk] = append(cat.byModel[mk], e)
	}
	if len(cat.byKey) == 0 {
		return nil, fmt.Errorf("modelparams: empty catalog")
	}
	return cat, nil
}

func (c *modelparamsCatalog) levelsFor(id string) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	entry, ok := c.lookup(id)
	if !ok {
		return nil, false
	}
	levels := extractThinkingLevels(entry.Params)
	if len(levels) == 0 {
		return nil, false
	}
	return levels, true
}

func (c *modelparamsCatalog) lookup(id string) (modelparamsEntry, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return modelparamsEntry{}, false
	}
	provider, model, prefixed := splitProviderModel(id)
	names := candidateModelNames(model)
	preferSub := !prefixed
	if prefixed {
		for _, name := range names {
			if e, ok := c.pick(provider, name, false); ok {
				return e, true
			}
		}
		return modelparamsEntry{}, false
	}
	guesses := guessProviders(model)
	for _, name := range names {
		for _, p := range guesses {
			if e, ok := c.pick(p, name, preferSub); ok {
				return e, true
			}
		}
		if e, ok := c.pick("", name, preferSub); ok {
			return e, true
		}
	}
	return modelparamsEntry{}, false
}

func (c *modelparamsCatalog) pick(provider, model string, preferSubscription bool) (modelparamsEntry, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return modelparamsEntry{}, false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "" {
		first, second := "api_key", "subscription"
		if preferSubscription {
			first, second = "subscription", "api_key"
		}
		if e, ok := c.byKey[provider+"/"+model+"/"+first]; ok {
			return e, true
		}
		if e, ok := c.byKey[provider+"/"+model+"/"+second]; ok {
			return e, true
		}
		return modelparamsEntry{}, false
	}
	cands := c.byModel[model]
	if len(cands) == 0 {
		return modelparamsEntry{}, false
	}
	if len(cands) == 1 {
		return cands[0], true
	}
	var sub, key, rest []modelparamsEntry
	for _, e := range cands {
		switch e.AuthType {
		case "subscription":
			sub = append(sub, e)
		case "api_key":
			key = append(key, e)
		default:
			rest = append(rest, e)
		}
	}
	pool := key
	if preferSubscription && len(sub) > 0 {
		pool = sub
	} else if !preferSubscription && len(key) > 0 {
		pool = key
	} else if len(sub) > 0 {
		pool = sub
	} else if len(rest) > 0 {
		pool = rest
	}
	if len(pool) == 1 {
		return pool[0], true
	}
	return modelparamsEntry{}, false
}

func splitProviderModel(id string) (provider, model string, prefixed bool) {
	id = strings.TrimSpace(id)
	if i := strings.Index(id, "/"); i > 0 {
		return strings.ToLower(id[:i]), strings.TrimSpace(id[i+1:]), true
	}
	return "", id, false
}

func candidateModelNames(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		k := strings.ToLower(s)
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	add(model)
	if i := strings.Index(model, ":"); i > 0 {
		add(model[:i])
	}
	lower := strings.ToLower(model)
	if strings.HasSuffix(lower, "-thinking") {
		add(model[:len(model)-len("-thinking")])
	}
	if strings.HasSuffix(lower, "-preview") {
		add(model[:len(model)-len("-preview")])
	} else {
		add(model + "-preview")
	}
	return out
}

func guessProviders(model string) []string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") || strings.HasPrefix(m, "codex"):
		return []string{"openai"}
	case strings.HasPrefix(m, "claude-"):
		return []string{"anthropic"}
	case strings.HasPrefix(m, "gemini-"):
		return []string{"google"}
	case strings.HasPrefix(m, "grok-"):
		return []string{"xai"}
	case strings.HasPrefix(m, "kimi-"):
		return []string{"moonshot", "fireworks", "alibaba"}
	case strings.HasPrefix(m, "deepseek-"):
		return []string{"deepseek", "alibaba"}
	default:
		return nil
	}
}

func extractThinkingLevels(params []modelparamsParam) []string {
	byPath := map[string]modelparamsParam{}
	for _, p := range params {
		byPath[p.Path] = p
	}
	for _, path := range effortParamPaths {
		if levels := enumLevels(byPath[path]); len(levels) > 0 {
			return levels
		}
	}
	for _, p := range params {
		if p.Type != "enum" || p.Group != "reasoning" || skipReasoningPath(p.Path) {
			continue
		}
		if levels := enumLevels(p); len(levels) > 0 {
			return levels
		}
	}
	return nil
}

func skipReasoningPath(path string) bool {
	switch path {
	case "thinking.type", "thinking.display", "reasoning.summary", "reasoning_format",
		"additionalModelRequestFields.thinking.type", "prompt_mode", "thinking.keep":
		return true
	default:
		return false
	}
}

func enumLevels(p modelparamsParam) []string {
	if p.Type != "enum" || len(p.Values) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Values))
	seen := map[string]struct{}{}
	hasDepth := false
	for _, raw := range p.Values {
		s := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
		if _, ok := cpaThinkingLevels[s]; !ok {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if s != "none" && s != "auto" {
			hasDepth = true
		}
	}
	if !hasDepth {
		return nil
	}
	return out
}
