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

func locateSnapshotChannel(document *yaml.Node, selector ChannelSelector) (snapshotChannel, error) {
	root := document.Content[0]
	channels, err := uniqueMappingValue(root, "openai-compatibility")
	if err != nil || channels == nil || channels.Kind != yaml.SequenceNode {
		return snapshotChannel{}, fmt.Errorf("selected channel unavailable")
	}
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
		if nameOK && baseOK && baseErr == nil && strings.TrimSpace(name) == selector.Name && base == selector.BaseURL && (!disabledOK || !disabled) {
			matches = append(matches, channel)
		}
	}
	if len(matches) != 1 {
		return snapshotChannel{}, fmt.Errorf("selected channel unavailable")
	}
	selected := matches[0]
	if hasYAMLIndirection(selected, make(map[*yaml.Node]bool)) {
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

func applyMembership(document *yaml.Node, results []plannedChannel) (bool, error) {
	changed := false
	for _, result := range results {
		channel, err := locateSnapshotChannel(document, result.Selector)
		if err != nil {
			return false, err
		}
		existing, err := namedModels(channel.Models)
		if err != nil {
			return false, err
		}
		if sameModelOrder(channel.Models, result.Desired) {
			continue
		}
		next := make([]*yaml.Node, 0, len(result.Desired))
		for _, name := range result.Desired {
			if retained := existing[name]; retained != nil {
				next = append(next, retained)
				continue
			}
			next = append(next, minimalModelNode(name))
		}
		channel.Models.Content = next
		changed = true
	}
	return changed, nil
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

func sameModelOrder(models *yaml.Node, desired []string) bool {
	if len(models.Content) != len(desired) {
		return false
	}
	for index, model := range models.Content {
		name, err := modelName(model)
		if err != nil || name != desired[index] {
			return false
		}
	}
	return true
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
	value, err := uniqueMappingValue(model, "name")
	if err != nil || value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return "", fmt.Errorf("invalid model name")
	}
	name := strings.TrimSpace(value.Value)
	if name == "" {
		return "", fmt.Errorf("invalid model name")
	}
	return name, nil
}

func minimalModelNode(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
	}}
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
