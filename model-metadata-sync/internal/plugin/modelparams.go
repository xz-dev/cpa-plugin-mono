package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

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
	ByKey map[string]modelparamsEntry `json:"by_key"`
}

func parseModelparamsCatalog(raw []byte) (*modelparamsCatalog, error) {
	if err := validateCatalogJSON(raw); err != nil {
		return nil, fmt.Errorf("modelparams: unrecognized catalog")
	}
	trimmed := bytes.TrimSpace(raw)
	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if json.Unmarshal(trimmed, &items) != nil || items == nil {
			return nil, fmt.Errorf("modelparams: unrecognized catalog")
		}
	case '{':
		wrapped, err := catalogObject(trimmed)
		if err != nil || hasFoldedCatalogField(wrapped, "models") {
			return nil, fmt.Errorf("modelparams: unrecognized catalog")
		}
		models, ok := wrapped["models"]
		if !ok || json.Unmarshal(models, &items) != nil || items == nil {
			return nil, fmt.Errorf("modelparams: unrecognized catalog")
		}
	default:
		return nil, fmt.Errorf("modelparams: unrecognized catalog")
	}
	cat := &modelparamsCatalog{ByKey: map[string]modelparamsEntry{}}
	for _, rawEntry := range items {
		fields, err := catalogObject(rawEntry)
		if err != nil || hasAnyFoldedCatalogField(fields, "provider", "authType", "model", "params") {
			return nil, fmt.Errorf("modelparams: invalid entry")
		}
		var entry modelparamsEntry
		if json.Unmarshal(rawEntry, &entry) != nil {
			return nil, fmt.Errorf("modelparams: invalid entry")
		}
		var rawParams []json.RawMessage
		if rawValue, ok := fields["params"]; ok {
			if json.Unmarshal(rawValue, &rawParams) != nil {
				return nil, fmt.Errorf("modelparams: invalid parameters")
			}
		}
		entry.Params = make([]modelparamsParam, 0, len(rawParams))
		for _, rawParam := range rawParams {
			paramFields, err := catalogObject(rawParam)
			if err != nil || hasAnyFoldedCatalogField(paramFields, "path", "group", "type", "values", "range") {
				return nil, fmt.Errorf("modelparams: invalid parameter")
			}
			if rawRange, ok := paramFields["range"]; ok {
				rangeFields, err := catalogObject(rawRange)
				if err != nil || hasFoldedCatalogField(rangeFields, "max") {
					return nil, fmt.Errorf("modelparams: invalid parameter range")
				}
			}
			var param modelparamsParam
			if json.Unmarshal(rawParam, &param) != nil || param.Path == "" || param.Path != strings.TrimSpace(param.Path) || param.Group != strings.TrimSpace(param.Group) || param.Type != strings.TrimSpace(param.Type) || hasControl(param.Path) || hasControl(param.Group) || hasControl(param.Type) || !validCatalogInteger(param.Range.Max) {
				return nil, fmt.Errorf("modelparams: invalid parameter")
			}
			if param.Type == "enum" {
				for _, value := range param.Values {
					if _, ok := value.(string); !ok {
						return nil, fmt.Errorf("modelparams: invalid enum parameter")
					}
				}
			}
			entry.Params = append(entry.Params, param)
		}
		entry.Provider = strings.ToLower(strings.TrimSpace(entry.Provider))
		entry.Model = strings.TrimSpace(entry.Model)
		entry.AuthType = strings.ToLower(strings.TrimSpace(entry.AuthType))
		if entry.Provider == "" || entry.Model == "" || hasControl(entry.Provider) || hasControl(entry.Model) || hasControl(entry.AuthType) {
			return nil, fmt.Errorf("modelparams: invalid entry")
		}
		if entry.AuthType == "" {
			entry.AuthType = "api_key"
		}
		seenPaths := make(map[string]bool)
		for _, param := range entry.Params {
			if seenPaths[param.Path] {
				return nil, fmt.Errorf("modelparams: invalid parameter")
			}
			seenPaths[param.Path] = true
		}
		key := entry.Provider + "/" + strings.ToLower(entry.Model) + "/" + entry.AuthType
		if _, duplicate := cat.ByKey[key]; duplicate {
			return nil, fmt.Errorf("modelparams: duplicate entry")
		}
		cat.ByKey[key] = entry
	}
	if len(cat.ByKey) == 0 {
		return nil, fmt.Errorf("modelparams: empty catalog")
	}
	return cat, nil
}

func validModelparamsCatalog(catalog *modelparamsCatalog) bool {
	if catalog == nil || len(catalog.ByKey) == 0 {
		return false
	}
	keys := make([]string, 0, len(catalog.ByKey))
	for key := range catalog.ByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]modelparamsEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, catalog.ByKey[key])
	}
	raw, err := json.Marshal(struct {
		Models []modelparamsEntry `json:"models"`
	}{Models: entries})
	if err != nil {
		return false
	}
	reparsed, err := parseModelparamsCatalog(raw)
	return err == nil && reflect.DeepEqual(reparsed.ByKey, catalog.ByKey)
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
		entry, ok := c.ByKey[key]
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
