package plugin

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

var catalogSafeHeaders = map[string]string{
	"accept":            "Accept",
	"anthropic-beta":    "anthropic-beta",
	"anthropic-version": "anthropic-version",
	"content-type":      "Content-Type",
	"user-agent":        "User-Agent",
}

var forbiddenCatalogHeaders = map[string]bool{
	"api-key":                    true,
	"api_key":                    true,
	"authorization":              true,
	"cookie":                     true,
	"host":                       true,
	"proxy-authorization":        true,
	"x-api-key":                  true,
	"x-management-key":           true,
	"x-sync-config-writer-token": true,
}

var metadataYAMLKeys = map[string]bool{
	"thinking": true, "max-context-length": true, "max-input-tokens": true,
	"max-output-tokens": true, "input-modalities": true, "output-modalities": true,
}

type snapshotChannel struct {
	Node    *yaml.Node
	Models  *yaml.Node
	Headers map[string]string
}

func parseSnapshot(raw []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("invalid snapshot")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("invalid snapshot")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid snapshot")
	}
	if err := validateMappings(document.Content[0], make(map[*yaml.Node]bool)); err != nil {
		return nil, fmt.Errorf("invalid snapshot")
	}
	return &document, nil
}

func locateSnapshotChannel(document *yaml.Node, spec compiledChannel) (snapshotChannel, error) {
	root := document.Content[0]
	rootKey := "openai-compatibility"
	if spec.Kind == KindClaude {
		rootKey = "claude-api-key"
	}
	channels, err := uniqueMappingValue(root, rootKey)
	if err != nil || channels == nil || channels.Kind != yaml.SequenceNode {
		return snapshotChannel{}, fmt.Errorf("selected channel unavailable")
	}
	var selected *yaml.Node
	if spec.Kind == KindClaude {
		if spec.Selector.ConfigIndex == nil || *spec.Selector.ConfigIndex >= len(channels.Content) {
			return snapshotChannel{}, fmt.Errorf("selected channel unavailable")
		}
		candidate := channels.Content[*spec.Selector.ConfigIndex]
		baseRaw := "https://api.anthropic.com"
		if value, ok := scalarString(candidate, "base-url"); ok && strings.TrimSpace(value) != "" {
			baseRaw = value
		}
		base, baseErr := normalizeBaseURL(baseRaw)
		prefix, _ := scalarString(candidate, "prefix")
		disabled, disabledOK, disabledErr := scalarBool(candidate, "disabled")
		if disabledErr != nil || baseErr != nil || base != spec.Selector.BaseURL || strings.Trim(strings.TrimSpace(prefix), "/") != spec.Selector.Prefix || disabledOK && disabled {
			return snapshotChannel{}, fmt.Errorf("selected channel unavailable")
		}
		selected = candidate
	} else {
		matches := make([]*yaml.Node, 0, 1)
		for _, channel := range channels.Content {
			if channel.Kind != yaml.MappingNode {
				continue
			}
			name, nameOK := scalarString(channel, "name")
			baseRaw, baseOK := scalarString(channel, "base-url")
			disabled, disabledOK, disabledErr := scalarBool(channel, "disabled")
			base, baseErr := normalizeBaseURL(baseRaw)
			if disabledErr != nil {
				return snapshotChannel{}, fmt.Errorf("invalid selected channel")
			}
			if nameOK && baseOK && baseErr == nil && strings.TrimSpace(name) == spec.Selector.Name && base == spec.Selector.BaseURL && (!disabledOK || !disabled) {
				matches = append(matches, channel)
			}
		}
		if len(matches) != 1 {
			return snapshotChannel{}, fmt.Errorf("selected channel unavailable")
		}
		selected = matches[0]
	}
	if selected == nil || hasYAMLIndirection(selected, make(map[*yaml.Node]bool)) {
		return snapshotChannel{}, fmt.Errorf("selected channel is ambiguous")
	}
	models, err := uniqueMappingValue(selected, "models")
	if err != nil || models == nil || models.Kind != yaml.SequenceNode {
		return snapshotChannel{}, fmt.Errorf("selected channel models unavailable")
	}
	if _, err := namedModels(models); err != nil {
		return snapshotChannel{}, err
	}
	headers, err := snapshotCatalogHeaders(selected)
	if err != nil {
		return snapshotChannel{}, err
	}
	return snapshotChannel{Node: selected, Models: models, Headers: headers}, nil
}

func snapshotCatalogHeaders(channel *yaml.Node) (map[string]string, error) {
	result := make(map[string]string)
	headers, err := uniqueMappingValue(channel, "headers")
	if err != nil {
		return nil, fmt.Errorf("invalid selected channel headers")
	}
	if headers == nil {
		return result, nil
	}
	if headers.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid selected channel headers")
	}
	for index := 0; index < len(headers.Content); index += 2 {
		key, value := headers.Content[index], headers.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return nil, fmt.Errorf("invalid selected channel headers")
		}
		lower := strings.ToLower(key.Value)
		if key.Value == "" || hasControl(key.Value) || forbiddenCatalogHeaders[lower] {
			return nil, fmt.Errorf("invalid selected channel headers")
		}
		canonical, safe := catalogSafeHeaders[lower]
		if !safe {
			continue
		}
		if _, duplicate := result[canonical]; duplicate || hasControl(value.Value) || strings.Contains(value.Value, "$TOKEN$") {
			return nil, fmt.Errorf("invalid selected channel headers")
		}
		result[canonical] = value.Value
	}
	return result, nil
}

func snapshotModels(channel snapshotChannel) ([]ModelRef, error) {
	models := make([]ModelRef, 0, len(channel.Models.Content))
	for _, node := range channel.Models.Content {
		name, err := modelName(node)
		if err != nil {
			return nil, err
		}
		model := ModelRef{Name: name}
		model.MaxContextLength, err = optionalPositiveInt(node, "max-context-length")
		if err != nil {
			return nil, err
		}
		model.MaxInputTokens, err = optionalPositiveInt(node, "max-input-tokens")
		if err != nil {
			return nil, err
		}
		model.MaxOutputTokens, err = optionalPositiveInt(node, "max-output-tokens")
		if err != nil {
			return nil, err
		}
		model.InputModalities, err = scalarStringSequence(node, "input-modalities")
		if err != nil {
			return nil, err
		}
		model.OutputModalities, err = scalarStringSequence(node, "output-modalities")
		if err != nil {
			return nil, err
		}
		thinking, thinkingErr := uniqueMappingValue(node, "thinking")
		if thinkingErr != nil {
			return nil, thinkingErr
		}
		if thinking != nil {
			if thinking.Kind != yaml.MappingNode || hasYAMLIndirection(thinking, make(map[*yaml.Node]bool)) {
				return nil, fmt.Errorf("invalid thinking metadata")
			}
			levels, levelsErr := scalarStringSequence(thinking, "levels")
			if levelsErr != nil {
				return nil, levelsErr
			}
			if len(levels) > 0 {
				model.Thinking = &ThinkingConfig{Levels: levels}
			}
		}
		models = append(models, model)
	}
	return models, nil
}

func applyMetadata(document *yaml.Node, cfg runtimeConfig, results []plannedChannel) (bool, error) {
	changed := false
	for _, result := range results {
		if result.ChannelIndex < 0 || result.ChannelIndex >= len(cfg.Channels) {
			return false, fmt.Errorf("selected channel unavailable")
		}
		channel, err := locateSnapshotChannel(document, cfg.Channels[result.ChannelIndex])
		if err != nil {
			return false, err
		}
		models, err := namedModels(channel.Models)
		if err != nil {
			return false, err
		}
		for _, patch := range result.Patches {
			model := models[patch.Model]
			if model == nil {
				return false, fmt.Errorf("selected model unavailable")
			}
			for _, field := range metadataFieldNames {
				value, exists := patch.Fields[field]
				if !exists {
					continue
				}
				fieldChanged, fieldErr := applyMetadataField(model, field, value)
				if fieldErr != nil {
					return false, fieldErr
				}
				changed = changed || fieldChanged
			}
		}
	}
	return changed, nil
}

func applyMetadataField(model *yaml.Node, field string, value plannedFieldValue) (bool, error) {
	key := metadataYAMLKey(field)
	if key == "" || !metadataYAMLKeys[key] {
		return false, fmt.Errorf("invalid metadata field")
	}
	newNode, err := metadataValueNode(field, value)
	if err != nil {
		return false, err
	}
	if field == "thinking.levels" {
		current, currentErr := uniqueMappingValue(model, "thinking")
		if currentErr != nil {
			return false, currentErr
		}
		if current != nil {
			if current.Kind != yaml.MappingNode {
				return false, fmt.Errorf("invalid thinking metadata")
			}
			levels, levelsErr := uniqueMappingValue(current, "levels")
			if levelsErr != nil {
				return false, levelsErr
			}
			replacement := newNode.Content[1]
			if levels != nil {
				if yamlNodesEqual(levels, replacement) {
					return false, nil
				}
				preserveNodePresentation(replacement, levels)
				for index := 0; index < len(current.Content); index += 2 {
					if current.Content[index].Value == "levels" {
						current.Content[index+1] = replacement
						return true, nil
					}
				}
			}
			current.Content = append(current.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "levels"},
				replacement,
			)
			return true, nil
		}
	}
	for index := 0; index < len(model.Content); index += 2 {
		if model.Content[index].Value != key {
			continue
		}
		if yamlNodesEqual(model.Content[index+1], newNode) {
			return false, nil
		}
		preserveNodePresentation(newNode, model.Content[index+1])
		model.Content[index+1] = newNode
		return true, nil
	}
	model.Content = append(model.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		newNode,
	)
	return true, nil
}

func metadataYAMLKey(field string) string {
	if field == "thinking.levels" {
		return "thinking"
	}
	return field
}

func metadataValueNode(field string, value plannedFieldValue) (*yaml.Node, error) {
	if field == "thinking.levels" {
		if value.Integer != nil || len(value.Strings) == 0 {
			return nil, fmt.Errorf("invalid thinking levels")
		}
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "levels"},
			stringSequenceNode(value.Strings),
		}}, nil
	}
	switch field {
	case "max-context-length", "max-input-tokens", "max-output-tokens":
		if value.Integer == nil || *value.Integer <= 0 || len(value.Strings) != 0 {
			return nil, fmt.Errorf("invalid token limit")
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", *value.Integer)}, nil
	case "input-modalities", "output-modalities":
		if value.Integer != nil || len(value.Strings) == 0 {
			return nil, fmt.Errorf("invalid modalities")
		}
		return stringSequenceNode(value.Strings), nil
	default:
		return nil, fmt.Errorf("invalid metadata field")
	}
}

func stringSequenceNode(values []string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, value := range values {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	return node
}

func preserveNodePresentation(target, existing *yaml.Node) {
	if target.Kind == existing.Kind {
		target.Style = existing.Style
	}
	target.HeadComment = existing.HeadComment
	target.LineComment = existing.LineComment
	target.FootComment = existing.FootComment
}

func yamlNodesEqual(left, right *yaml.Node) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.Kind != right.Kind || left.Tag != right.Tag || left.Value != right.Value || len(left.Content) != len(right.Content) {
		return false
	}
	for index := range left.Content {
		if !yamlNodesEqual(left.Content[index], right.Content[index]) {
			return false
		}
	}
	return true
}

func encodeSnapshot(document *yaml.Node) ([]byte, error) {
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("invalid proposal")
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("invalid proposal")
	}
	return output.Bytes(), nil
}

func namedModels(models *yaml.Node) (map[string]*yaml.Node, error) {
	result := make(map[string]*yaml.Node, len(models.Content))
	for _, model := range models.Content {
		name, err := modelName(model)
		if err != nil {
			return nil, err
		}
		if result[name] != nil {
			return nil, fmt.Errorf("duplicate model name")
		}
		result[name] = model
	}
	return result, nil
}

func modelName(model *yaml.Node) (string, error) {
	if model == nil || model.Kind != yaml.MappingNode {
		return "", fmt.Errorf("invalid model mapping")
	}
	if hasYAMLIndirection(model, make(map[*yaml.Node]bool)) {
		return "", fmt.Errorf("ambiguous model mapping")
	}
	value, err := uniqueMappingValue(model, "name")
	if err != nil || value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return "", fmt.Errorf("invalid model name")
	}
	name := strings.TrimSpace(value.Value)
	if name == "" || name != value.Value || len(name) > 1024 || hasControl(name) {
		return "", fmt.Errorf("invalid model name")
	}
	return name, nil
}

func uniqueMappingValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid mapping")
	}
	var found *yaml.Node
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value != key {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("duplicate mapping key")
		}
		found = mapping.Content[index+1]
	}
	return found, nil
}

func scalarString(mapping *yaml.Node, key string) (string, bool) {
	value, err := uniqueMappingValue(mapping, key)
	if err != nil || value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return "", false
	}
	return value.Value, true
}

func scalarBool(mapping *yaml.Node, key string) (bool, bool, error) {
	value, err := uniqueMappingValue(mapping, key)
	if err != nil {
		return false, false, err
	}
	if value == nil {
		return false, false, nil
	}
	if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
		return false, true, fmt.Errorf("invalid bool")
	}
	var result bool
	if err := value.Decode(&result); err != nil {
		return false, true, err
	}
	return result, true, nil
}

func optionalPositiveInt(mapping *yaml.Node, key string) (int, error) {
	value, err := uniqueMappingValue(mapping, key)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, nil
	}
	if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
		return 0, fmt.Errorf("invalid positive integer")
	}
	var result int
	if value.Decode(&result) != nil || result <= 0 {
		return 0, fmt.Errorf("invalid positive integer")
	}
	return result, nil
}

func scalarStringSequence(mapping *yaml.Node, key string) ([]string, error) {
	value, err := uniqueMappingValue(mapping, key)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("invalid string sequence")
	}
	values := make([]string, 0, len(value.Content))
	for _, item := range value.Content {
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || item.Value == "" || hasControl(item.Value) {
			return nil, fmt.Errorf("invalid string sequence")
		}
		values = append(values, item.Value)
	}
	return values, nil
}

func validateMappings(node *yaml.Node, visited map[*yaml.Node]bool) error {
	if node == nil {
		return fmt.Errorf("invalid YAML node")
	}
	if visited[node] {
		return nil
	}
	visited[node] = true
	if node.Kind == yaml.MappingNode {
		if len(node.Content)%2 != 0 {
			return fmt.Errorf("invalid YAML mapping")
		}
		seen := make(map[string]bool)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Value == "" || seen[key.Value] {
				return fmt.Errorf("invalid YAML mapping")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := validateMappings(child, visited); err != nil {
			return err
		}
	}
	if node.Alias != nil {
		return validateMappings(node.Alias, visited)
	}
	return nil
}

func hasYAMLIndirection(node *yaml.Node, visited map[*yaml.Node]bool) bool {
	if node == nil || visited[node] {
		return false
	}
	visited[node] = true
	if node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" {
		return true
	}
	if node.Kind == yaml.MappingNode {
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Tag == "!!merge" || key.Value == "<<" {
				return true
			}
		}
	}
	for _, child := range node.Content {
		if hasYAMLIndirection(child, visited) {
			return true
		}
	}
	return false
}

func hasControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
