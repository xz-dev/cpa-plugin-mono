package plugin

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

var pluginIDs = []string{"sync-config-write", "auto-pull-models", "model-metadata-sync", "model-info"}

func injectSyncEpoch(raw []byte, epoch string) ([]byte, map[string]string, error) {
	if decoded, err := decodeOpaqueID(epoch); err != nil || len(decoded) != 16 {
		return nil, nil, fmt.Errorf("invalid sync epoch")
	}
	root, err := parseOwnedDocument(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid config")
	}
	configs, err := pluginConfigsNode(root)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range pluginIDs {
		subtree, err := uniqueMappingValue(configs, id)
		if err != nil || subtree.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("missing plugin config")
		}
		if err := rejectMutableAmbiguity(subtree); err != nil {
			return nil, nil, fmt.Errorf("ambiguous plugin config")
		}
		setMappingString(subtree, "sync_epoch", epoch)
	}
	document := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	adjusted, err := yaml.Marshal(document)
	if err != nil {
		return nil, nil, fmt.Errorf("encode config")
	}
	hashes, err := runtimeConfigHashes(adjusted)
	if err != nil {
		return nil, nil, err
	}
	return adjusted, hashes, nil
}

func runtimeConfigHashes(raw []byte) (map[string]string, error) {
	root, err := parseOwnedDocument(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid config")
	}
	configs, err := pluginConfigsNode(root)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(pluginIDs))
	for _, id := range pluginIDs {
		subtree, err := uniqueMappingValue(configs, id)
		if err != nil || subtree.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("missing plugin config")
		}
		if err := rejectMutableAmbiguity(subtree); err != nil {
			return nil, fmt.Errorf("ambiguous plugin config")
		}
		runtimeRaw, err := runtimeConfigYAML(subtree)
		if err != nil {
			return nil, err
		}
		result[id] = configVersion(runtimeRaw)
	}
	return result, nil
}

func runtimeConfigYAML(subtree *yaml.Node) ([]byte, error) {
	node := cloneYAMLNode(subtree)
	if node.Kind == yaml.MappingNode {
		if !mappingHas(node, "enabled") {
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
		}
		if !mappingHas(node, "priority") {
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "priority"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0"})
		}
	}
	raw, err := yaml.Marshal(node)
	if err != nil {
		return nil, fmt.Errorf("encode plugin config")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []byte("enabled: false\npriority: 0\n"), nil
	}
	return append(append([]byte(nil), raw...), '\n'), nil
}

func normalizeCommentIndentation(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	changed := false
	for i, line := range lines {
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) == 0 || trimmed[0] != '#' || len(trimmed) == len(line) {
			continue
		}
		lines[i] = append([]byte(nil), trimmed...)
		changed = true
	}
	if !changed {
		return data
	}
	return bytes.Join(lines, []byte("\n"))
}

func pluginConfigsNode(root *yaml.Node) (*yaml.Node, error) {
	plugins, err := uniqueMappingValue(root, "plugins")
	if err != nil || plugins.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("missing plugins config")
	}
	configs, err := uniqueMappingValue(plugins, "configs")
	if err != nil || configs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("missing plugin configs")
	}
	return configs, nil
}

func uniqueMappingValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping")
	}
	var result *yaml.Node
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("duplicate mapping key")
		}
		result = mapping.Content[i+1]
	}
	if result == nil {
		return nil, fmt.Errorf("mapping key missing")
	}
	return result, nil
}

func mappingHas(mapping *yaml.Node, key string) bool {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return true
		}
	}
	return false
}

func setMappingString(mapping *yaml.Node, key, value string) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	copyNode := *node
	copyNode.Line, copyNode.Column = 0, 0
	if len(node.Content) != 0 {
		copyNode.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			copyNode.Content[i] = cloneYAMLNode(child)
		}
	}
	if node.Alias != nil {
		copyNode.Alias = cloneYAMLNode(node.Alias)
	}
	return &copyNode
}

func stripPluginEpochs(raw []byte) (*yaml.Node, error) {
	root, err := parseOwnedDocument(raw)
	if err != nil {
		return nil, err
	}
	configs, err := pluginConfigsNode(root)
	if err != nil {
		return nil, err
	}
	for _, id := range pluginIDs {
		subtree, err := uniqueMappingValue(configs, id)
		if err != nil {
			return nil, err
		}
		removeMappingKey(subtree, "sync_epoch")
	}
	return root, nil
}

func removeMappingKey(mapping *yaml.Node, key string) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}
