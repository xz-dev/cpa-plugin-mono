package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxCatalogJSONDepth        = 128
	maxCatalogModels           = 4096
	maxCatalogObjectMembers    = 256
	maxCatalogArrayElements    = 4096
	maxCatalogScannedValues    = 262144
	maxCatalogObjectKeyBytes   = 256
	maxCatalogModelIDBytes     = 1024
	maxCatalogDisplayNameBytes = 4096
	maxCatalogShortStringBytes = 128
	maxCatalogReasoningLevels  = 32
	maxCatalogModalities       = 16
)

// ModelRow is one model as shown in the viewer.
type ModelRow struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Context     int64    `json:"context_window,omitempty"`
	MaxInput    int64    `json:"max_input_tokens,omitempty"`
	MaxTokens   int64    `json:"max_tokens,omitempty"`
	Levels      []string `json:"reasoning_levels,omitempty"`
	Input       []string `json:"input_modalities,omitempty"`
	Output      []string `json:"output_modalities,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
	MaxSource   string   `json:"max_source,omitempty"`
}

type Catalog struct {
	Count  int        `json:"count"`
	Models []ModelRow `json:"models"`
}

type catalogPayload struct {
	Models []json.RawMessage `json:"models"`
}

func parseCatalog(raw []byte) (Catalog, error) {
	if err := validateCatalogJSON(raw, catalogJSONRoot); err != nil {
		return Catalog{}, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Catalog{}, fmt.Errorf("invalid catalog")
	}
	for key := range top {
		if strings.EqualFold(key, "models") && key != "models" {
			return Catalog{}, fmt.Errorf("invalid catalog")
		}
	}
	var payload catalogPayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Models == nil {
		return Catalog{}, fmt.Errorf("invalid catalog")
	}
	models := make([]ModelRow, 0, len(payload.Models))
	seen := make(map[string]bool, len(payload.Models))
	for _, modelRaw := range payload.Models {
		model, err := parseModel(modelRaw)
		if err != nil || seen[model.ID] {
			return Catalog{}, fmt.Errorf("invalid catalog")
		}
		seen[model.ID] = true
		models = append(models, model)
	}
	return Catalog{Count: len(models), Models: models}, nil
}

func parseModel(raw json.RawMessage) (ModelRow, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ModelRow{}, fmt.Errorf("invalid model")
	}
	for key := range fields {
		for _, relevant := range []string{"id", "slug", "display_name", "visibility", "context_window", "max_context_window", "max_input_tokens", "max_tokens", "max_output_tokens", "max_completion_tokens", "supported_reasoning_levels", "input_modalities", "output_modalities"} {
			if strings.EqualFold(key, relevant) && key != relevant {
				return ModelRow{}, fmt.Errorf("invalid model")
			}
		}
	}
	id, idSet, err := optionalString(fields, "id")
	if err != nil {
		return ModelRow{}, err
	}
	slug, slugSet, err := optionalString(fields, "slug")
	if err != nil {
		return ModelRow{}, err
	}
	id = strings.TrimSpace(id)
	slug = strings.TrimSpace(slug)
	if idSet && id == "" || slugSet && slug == "" || id == "" && slug == "" || id != "" && slug != "" && id != slug || hasControl(id) || hasControl(slug) {
		return ModelRow{}, fmt.Errorf("invalid model id")
	}
	identity := id
	if identity == "" {
		identity = slug
	}
	displayName, _, err := optionalString(fields, "display_name")
	if err != nil || hasControl(displayName) {
		return ModelRow{}, fmt.Errorf("invalid display name")
	}
	visibility, _, err := optionalString(fields, "visibility")
	if err != nil || hasControl(visibility) {
		return ModelRow{}, fmt.Errorf("invalid visibility")
	}
	context, _, err := optionalNonnegativeInteger(fields, "context_window")
	if err != nil {
		return ModelRow{}, err
	}
	maxContext, _, err := optionalNonnegativeInteger(fields, "max_context_window")
	if err != nil {
		return ModelRow{}, err
	}
	maxInput, maxInputSet, err := optionalNonnegativeInteger(fields, "max_input_tokens")
	if err != nil {
		return ModelRow{}, err
	}
	maxTokens, maxTokensSet, err := optionalNonnegativeInteger(fields, "max_tokens")
	if err != nil {
		return ModelRow{}, err
	}
	maxOutput, maxOutputSet, err := optionalNonnegativeInteger(fields, "max_output_tokens")
	if err != nil {
		return ModelRow{}, err
	}
	maxCompletion, _, err := optionalNonnegativeInteger(fields, "max_completion_tokens")
	if err != nil {
		return ModelRow{}, err
	}
	if !maxInputSet {
		maxInput = maxContext
	}
	if !maxTokensSet {
		if maxOutputSet {
			maxTokens = maxOutput
		} else {
			maxTokens = maxCompletion
		}
	}
	levels, err := reasoningLevels(fields)
	if err != nil {
		return ModelRow{}, err
	}
	input, err := optionalStringArray(fields, "input_modalities")
	if err != nil {
		return ModelRow{}, err
	}
	output, err := optionalStringArray(fields, "output_modalities")
	if err != nil {
		return ModelRow{}, err
	}
	model := ModelRow{ID: identity, Slug: slug, DisplayName: displayName, Context: context, MaxInput: maxInput, MaxTokens: maxTokens, Levels: levels, Input: input, Output: output, Visibility: visibility}
	if index := strings.Index(identity, "/"); index > 0 {
		model.Provider = identity[:index]
	}
	return model, nil
}

func optionalString(fields map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, fmt.Errorf("invalid string")
	}
	return value, true, nil
}

func optionalNonnegativeInteger(fields map[string]json.RawMessage, key string) (int64, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, false, nil
	}
	text := string(raw)
	if text == "" || strings.ContainsAny(text, `.eE`) || text[0] != '-' && (text[0] < '0' || text[0] > '9') {
		return 0, true, fmt.Errorf("invalid integer")
	}
	integer, err := strconv.ParseInt(text, 10, 64)
	if err != nil || integer < 0 {
		return 0, true, fmt.Errorf("invalid integer")
	}
	return integer, true, nil
}

func optionalStringArray(fields map[string]json.RawMessage, key string) ([]string, error) {
	raw, ok := fields[key]
	if !ok {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, fmt.Errorf("invalid string array")
	}
	for _, value := range values {
		if hasControl(value) {
			return nil, fmt.Errorf("invalid string array")
		}
	}
	return values, nil
}

func reasoningLevels(fields map[string]json.RawMessage) ([]string, error) {
	raw, ok := fields["supported_reasoning_levels"]
	if !ok {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("invalid reasoning levels")
	}
	levels := make([]string, 0, len(entries))
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("invalid reasoning level")
		}
		for key := range fields {
			if strings.EqualFold(key, "effort") && key != "effort" {
				return nil, fmt.Errorf("invalid reasoning level")
			}
		}
		effort, ok, err := optionalString(fields, "effort")
		if err != nil || !ok || strings.TrimSpace(effort) == "" || hasControl(effort) {
			return nil, fmt.Errorf("invalid reasoning level")
		}
		levels = append(levels, effort)
	}
	return levels, nil
}

func validateCatalogJSON(raw []byte, rootContext ...catalogJSONContext) error {
	if !utf8.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("invalid catalog")
	}
	context := catalogJSONGeneric
	if len(rootContext) != 0 {
		context = rootContext[0]
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	scanner := catalogJSONScanner{}
	if err := scanner.scan(decoder, 0, context); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("invalid catalog")
	}
	return nil
}

type catalogJSONContext uint8

const (
	catalogJSONGeneric catalogJSONContext = iota
	catalogJSONRoot
	catalogJSONModels
	catalogJSONModel
	catalogJSONReasoningLevels
	catalogJSONReasoningLevel
	catalogJSONModalities
	catalogJSONModelID
	catalogJSONDisplayName
	catalogJSONShortString
)

type catalogJSONScanner struct {
	scanned int
}

func (s *catalogJSONScanner) scan(decoder *json.Decoder, depth int, context catalogJSONContext) error {
	if depth > maxCatalogJSONDepth || !s.addScanned(1) {
		return fmt.Errorf("invalid catalog")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid catalog")
	}
	delim, composite := token.(json.Delim)
	if !composite {
		if value, ok := token.(string); ok && len(value) > maxStringBytes(context) {
			return fmt.Errorf("invalid catalog")
		}
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		members := 0
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok || len(key) > maxCatalogObjectKeyBytes {
				return fmt.Errorf("invalid catalog")
			}
			members++
			folded := strings.ToUpper(key)
			if members > maxCatalogObjectMembers || seen[folded] || !s.addScanned(1) {
				return fmt.Errorf("invalid catalog")
			}
			seen[folded] = true
			if err := s.scan(decoder, depth+1, objectChildContext(context, key)); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid catalog")
		}
	case '[':
		limit, childContext := arrayLimits(context)
		elements := 0
		for decoder.More() {
			elements++
			if elements > limit {
				return fmt.Errorf("invalid catalog")
			}
			if err := s.scan(decoder, depth+1, childContext); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid catalog")
		}
	default:
		return fmt.Errorf("invalid catalog")
	}
	return nil
}

func (s *catalogJSONScanner) addScanned(count int) bool {
	if count > maxCatalogScannedValues-s.scanned {
		return false
	}
	s.scanned += count
	return true
}

func objectChildContext(parent catalogJSONContext, key string) catalogJSONContext {
	switch parent {
	case catalogJSONRoot:
		if key == "models" {
			return catalogJSONModels
		}
	case catalogJSONModel:
		switch key {
		case "id", "slug":
			return catalogJSONModelID
		case "display_name":
			return catalogJSONDisplayName
		case "visibility":
			return catalogJSONShortString
		case "supported_reasoning_levels":
			return catalogJSONReasoningLevels
		case "input_modalities", "output_modalities":
			return catalogJSONModalities
		}
	case catalogJSONReasoningLevel:
		if key == "effort" {
			return catalogJSONShortString
		}
	}
	return catalogJSONGeneric
}

func arrayLimits(context catalogJSONContext) (int, catalogJSONContext) {
	switch context {
	case catalogJSONModels:
		return maxCatalogModels, catalogJSONModel
	case catalogJSONReasoningLevels:
		return maxCatalogReasoningLevels, catalogJSONReasoningLevel
	case catalogJSONModalities:
		return maxCatalogModalities, catalogJSONShortString
	default:
		return maxCatalogArrayElements, catalogJSONGeneric
	}
}

func maxStringBytes(context catalogJSONContext) int {
	switch context {
	case catalogJSONModelID:
		return maxCatalogModelIDBytes
	case catalogJSONDisplayName:
		return maxCatalogDisplayNameBytes
	case catalogJSONShortString:
		return maxCatalogShortStringBytes
	default:
		return maxIngestBodyBytes
	}
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
