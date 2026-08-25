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
	Range  struct {
		Max float64 `json:"max"`
	} `json:"range"`
}

type modelparamsCatalog struct {
	byKey map[string]modelparamsEntry
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
	cat := &modelparamsCatalog{byKey: map[string]modelparamsEntry{}}
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
	}
	if len(cat.byKey) == 0 {
		return nil, fmt.Errorf("modelparams: empty catalog")
	}
	return cat, nil
}

func (c *modelparamsCatalog) lookupSource(source metadataSource, id string) (modelparamsEntry, bool) {
	if c == nil || source.Website != "modelparams.dev" {
		return modelparamsEntry{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return modelparamsEntry{}, false
	}
	lookup := func(model string) (modelparamsEntry, bool) {
		key := source.Provider + "/" + strings.ToLower(model) + "/" + source.AuthType
		entry, ok := c.byKey[key]
		return entry, ok
	}
	if entry, ok := lookup(id); ok {
		return entry, true
	}
	if strings.Count(id, "/") == 1 {
		return lookup(id[strings.IndexByte(id, '/')+1:])
	}
	return modelparamsEntry{}, false
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

func extractMaxOutputTokens(params []modelparamsParam) int {
	paths := []string{"max_output_tokens", "max_completion_tokens", "max_tokens", "generationConfig.maxOutputTokens", "inferenceConfig.maxTokens"}
	byPath := map[string]modelparamsParam{}
	for _, param := range params {
		if param.Group == "generation_length" && param.Type == "integer" && param.Range.Max > 0 {
			byPath[param.Path] = param
		}
	}
	for _, path := range paths {
		if param, ok := byPath[path]; ok {
			return int(param.Range.Max)
		}
	}
	return 0
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
