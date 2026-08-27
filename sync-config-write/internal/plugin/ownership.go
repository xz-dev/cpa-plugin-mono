package plugin

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

var metadataKeys = map[string]bool{
	"thinking": true, "max-context-length": true, "max-input-tokens": true,
	"max-output-tokens": true, "input-modalities": true, "output-modalities": true,
}

func validateOwnership(operation Operation, baseRaw, proposedRaw []byte) (bool, error) {
	base, err := parseOwnedDocument(baseRaw)
	if err != nil {
		return false, fmt.Errorf("invalid base config")
	}
	proposed, err := parseOwnedDocument(proposedRaw)
	if err != nil {
		return false, fmt.Errorf("invalid proposed config")
	}
	if err := validateMutablePaths(base, operation); err != nil {
		return false, fmt.Errorf("invalid base config")
	}
	if err := validateMutablePaths(proposed, operation); err != nil {
		return false, fmt.Errorf("invalid proposed config")
	}
	switch operation {
	case OperationAutoPull:
		return compareMembershipRoot(base, proposed)
	case OperationMetadataSync:
		return compareMetadataRoot(base, proposed)
	default:
		return false, fmt.Errorf("unsupported commit operation")
	}
}

func parseOwnedDocument(raw []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("multiple YAML documents")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0] == nil || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected one root mapping")
	}
	if err := validateDocumentMappings(document.Content[0], make(map[*yaml.Node]bool)); err != nil {
		return nil, err
	}
	return document.Content[0], nil
}

func validateDocumentMappings(node *yaml.Node, visited map[*yaml.Node]bool) error {
	if node == nil {
		return fmt.Errorf("nil YAML node")
	}
	if visited[node] {
		return nil
	}
	visited[node] = true
	if node.Kind == yaml.AliasNode && node.Alias == nil {
		return fmt.Errorf("invalid YAML alias")
	}
	if node.Kind == yaml.MappingNode {
		if len(node.Content)%2 != 0 {
			return fmt.Errorf("invalid YAML mapping")
		}
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key == nil || key.Kind != yaml.ScalarNode || key.Value == "" {
				return fmt.Errorf("ambiguous YAML mapping key")
			}
			if seen[key.Value] {
				return fmt.Errorf("duplicate YAML mapping key")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := validateDocumentMappings(child, visited); err != nil {
			return err
		}
	}
	if node.Alias != nil {
		return validateDocumentMappings(node.Alias, visited)
	}
	return nil
}

func validateMutablePaths(root *yaml.Node, operation Operation) error {
	configs, err := pluginConfigsNode(root)
	if err != nil {
		return err
	}
	for _, id := range pluginIDs {
		subtree, err := uniqueMappingValue(configs, id)
		if err != nil || subtree.Kind != yaml.MappingNode {
			return fmt.Errorf("missing plugin config")
		}
		if err := rejectMutableAmbiguity(subtree); err != nil {
			return err
		}
	}
	ownedRoots := map[string]bool{"openai-compatibility": true}
	if operation == OperationMetadataSync {
		ownedRoots["claude-api-key"] = true
	} else if operation != OperationAutoPull {
		return fmt.Errorf("unsupported commit operation")
	}
	for i := 0; i < len(root.Content); i += 2 {
		if ownedRoots[root.Content[i].Value] {
			if err := rejectMutableAmbiguity(root.Content[i+1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectMutableAmbiguity(node *yaml.Node) error {
	if node == nil {
		return fmt.Errorf("nil mutable YAML node")
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" {
		return fmt.Errorf("ambiguous YAML alias in mutable path")
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == "<<" || node.Content[i].Tag == "!!merge" {
				return fmt.Errorf("YAML merge key in mutable path")
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectMutableAmbiguity(child); err != nil {
			return err
		}
	}
	return nil
}

func compareMembershipRoot(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || len(base.Content) != len(proposed.Content) {
		return false, fmt.Errorf("root config changed")
	}
	changedProviders := 0
	ownedChanged := false
	for i := 0; i < len(base.Content); i += 2 {
		baseKey, proposedKey := base.Content[i], proposed.Content[i]
		if !nodesIdentical(baseKey, proposedKey) {
			return false, fmt.Errorf("root config changed")
		}
		if baseKey.Value != "openai-compatibility" {
			if !nodesIdentical(base.Content[i+1], proposed.Content[i+1]) {
				return false, fmt.Errorf("non-owned config changed")
			}
			continue
		}
		count, changed, err := compareMembershipProviders(base.Content[i+1], proposed.Content[i+1])
		if err != nil {
			return false, err
		}
		changedProviders += count
		ownedChanged = ownedChanged || changed
	}
	if changedProviders > 1 {
		return false, fmt.Errorf("multiple model sequences changed")
	}
	return ownedChanged, nil
}

func compareMembershipProviders(base, proposed *yaml.Node) (int, bool, error) {
	if !sameNodeShell(base, proposed) || base.Kind != yaml.SequenceNode || len(base.Content) != len(proposed.Content) {
		return 0, false, fmt.Errorf("provider membership changed")
	}
	changedProviders := 0
	ownedChanged := false
	for i := range base.Content {
		changed, err := compareMembershipProvider(base.Content[i], proposed.Content[i])
		if err != nil {
			return 0, false, err
		}
		if changed {
			changedProviders++
			ownedChanged = true
		}
	}
	return changedProviders, ownedChanged, nil
}

func compareMembershipProvider(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || base.Kind != yaml.MappingNode || len(base.Content) != len(proposed.Content) {
		return false, fmt.Errorf("provider config changed")
	}
	foundModels := false
	ownedChanged := false
	for i := 0; i < len(base.Content); i += 2 {
		if !nodesIdentical(base.Content[i], proposed.Content[i]) {
			return false, fmt.Errorf("provider config changed")
		}
		if base.Content[i].Value != "models" {
			if !nodesIdentical(base.Content[i+1], proposed.Content[i+1]) {
				return false, fmt.Errorf("provider config changed")
			}
			continue
		}
		foundModels = true
		changed, err := compareMembershipModels(base.Content[i+1], proposed.Content[i+1])
		if err != nil {
			return false, err
		}
		ownedChanged = changed
	}
	if !foundModels && !nodesIdentical(base, proposed) {
		return false, fmt.Errorf("models sequence missing")
	}
	return ownedChanged, nil
}

func compareMembershipModels(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || base.Kind != yaml.SequenceNode {
		return false, fmt.Errorf("models must remain a sequence")
	}
	baseModels, err := namedModels(base)
	if err != nil {
		return false, err
	}
	proposedModels, err := namedModels(proposed)
	if err != nil {
		return false, err
	}
	for name, model := range proposedModels {
		if retained := baseModels[name]; retained != nil {
			if !nodesIdentical(retained, model) {
				return false, fmt.Errorf("retained model changed")
			}
			continue
		}
		if !minimalNewModel(model, name) {
			return false, fmt.Errorf("new model must contain only name")
		}
	}
	return !nodesIdentical(base, proposed), nil
}

func compareMetadataRoot(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || len(base.Content) != len(proposed.Content) {
		return false, fmt.Errorf("root config changed")
	}
	ownedChanged := false
	for i := 0; i < len(base.Content); i += 2 {
		if !nodesIdentical(base.Content[i], proposed.Content[i]) {
			return false, fmt.Errorf("root config changed")
		}
		key := base.Content[i].Value
		if key != "openai-compatibility" && key != "claude-api-key" {
			if !nodesIdentical(base.Content[i+1], proposed.Content[i+1]) {
				return false, fmt.Errorf("non-owned config changed")
			}
			continue
		}
		changed, err := compareMetadataProviders(base.Content[i+1], proposed.Content[i+1])
		if err != nil {
			return false, err
		}
		ownedChanged = ownedChanged || changed
	}
	return ownedChanged, nil
}

func compareMetadataProviders(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || base.Kind != yaml.SequenceNode || len(base.Content) != len(proposed.Content) {
		return false, fmt.Errorf("provider membership changed")
	}
	ownedChanged := false
	for i := range base.Content {
		changed, err := compareMetadataProvider(base.Content[i], proposed.Content[i])
		if err != nil {
			return false, err
		}
		ownedChanged = ownedChanged || changed
	}
	return ownedChanged, nil
}

func compareMetadataProvider(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || base.Kind != yaml.MappingNode || len(base.Content) != len(proposed.Content) {
		return false, fmt.Errorf("provider config changed")
	}
	ownedChanged := false
	for i := 0; i < len(base.Content); i += 2 {
		if !nodesIdentical(base.Content[i], proposed.Content[i]) {
			return false, fmt.Errorf("provider config changed")
		}
		if base.Content[i].Value != "models" {
			if !nodesIdentical(base.Content[i+1], proposed.Content[i+1]) {
				return false, fmt.Errorf("provider config changed")
			}
			continue
		}
		changed, err := compareMetadataModels(base.Content[i+1], proposed.Content[i+1])
		if err != nil {
			return false, err
		}
		ownedChanged = ownedChanged || changed
	}
	return ownedChanged, nil
}

func compareMetadataModels(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || base.Kind != yaml.SequenceNode || len(base.Content) != len(proposed.Content) {
		return false, fmt.Errorf("model membership changed")
	}
	ownedChanged := false
	for i := range base.Content {
		changed, err := compareMetadataModel(base.Content[i], proposed.Content[i])
		if err != nil {
			return false, err
		}
		ownedChanged = ownedChanged || changed
	}
	return ownedChanged, nil
}

type metadataField struct {
	key *yaml.Node
}

func compareMetadataModel(base, proposed *yaml.Node) (bool, error) {
	if !sameNodeShell(base, proposed) || base.Kind != yaml.MappingNode {
		return false, fmt.Errorf("model mapping changed")
	}
	baseName, err := modelName(base)
	if err != nil {
		return false, err
	}
	proposedName, err := modelName(proposed)
	if err != nil || proposedName != baseName {
		return false, fmt.Errorf("model name changed")
	}
	baseUnowned, baseOwned, baseOrder := splitMetadataFields(base)
	proposedUnowned, proposedOwned, proposedOrder := splitMetadataFields(proposed)
	if len(baseUnowned) != len(proposedUnowned) {
		return false, fmt.Errorf("non-owned model field changed")
	}
	for i := range baseUnowned {
		if !nodesIdentical(baseUnowned[i], proposedUnowned[i]) {
			return false, fmt.Errorf("non-owned model field changed")
		}
	}
	for name, baseField := range baseOwned {
		if proposedField, ok := proposedOwned[name]; ok && !nodesIdentical(baseField.key, proposedField.key) {
			return false, fmt.Errorf("metadata key identity changed")
		}
	}
	for name, proposedField := range proposedOwned {
		if _, existed := baseOwned[name]; !existed && !canonicalMetadataKey(proposedField.key) {
			return false, fmt.Errorf("new metadata key is not canonical")
		}
	}
	if !sameRetainedMetadataOrder(baseOrder, proposedOrder, baseOwned, proposedOwned) {
		return false, fmt.Errorf("metadata key order changed")
	}
	return !nodesIdentical(base, proposed), nil
}

func splitMetadataFields(node *yaml.Node) ([]*yaml.Node, map[string]metadataField, []string) {
	unowned := make([]*yaml.Node, 0, len(node.Content))
	owned := make(map[string]metadataField)
	order := make([]string, 0, len(metadataKeys))
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind == yaml.ScalarNode && key.Tag == "!!str" && metadataKeys[key.Value] {
			owned[key.Value] = metadataField{key: key}
			order = append(order, key.Value)
			continue
		}
		unowned = append(unowned, key, value)
	}
	return unowned, owned, order
}

func canonicalMetadataKey(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!str" && metadataKeys[node.Value] && emptyNodeDecorations(node)
}

func sameRetainedMetadataOrder(baseOrder, proposedOrder []string, base, proposed map[string]metadataField) bool {
	baseRetained := make([]string, 0, len(baseOrder))
	for _, name := range baseOrder {
		if _, ok := proposed[name]; ok {
			baseRetained = append(baseRetained, name)
		}
	}
	proposedRetained := make([]string, 0, len(proposedOrder))
	for _, name := range proposedOrder {
		if _, ok := base[name]; ok {
			proposedRetained = append(proposedRetained, name)
		}
	}
	if len(baseRetained) != len(proposedRetained) {
		return false
	}
	for i := range baseRetained {
		if baseRetained[i] != proposedRetained[i] {
			return false
		}
	}
	return true
}

func namedModels(sequence *yaml.Node) (map[string]*yaml.Node, error) {
	models := make(map[string]*yaml.Node, len(sequence.Content))
	for _, model := range sequence.Content {
		name, err := modelName(model)
		if err != nil {
			return nil, err
		}
		if models[name] != nil {
			return nil, fmt.Errorf("duplicate model name")
		}
		models[name] = model
	}
	return models, nil
}

func modelName(model *yaml.Node) (string, error) {
	if model == nil || model.Kind != yaml.MappingNode {
		return "", fmt.Errorf("model must be a mapping")
	}
	var name string
	for i := 0; i < len(model.Content); i += 2 {
		if model.Content[i].Value != "name" {
			continue
		}
		value := model.Content[i+1]
		if value.Kind != yaml.ScalarNode || strings.TrimSpace(value.Value) == "" || name != "" {
			return "", fmt.Errorf("model name must be unique and non-empty")
		}
		name = value.Value
	}
	if name == "" {
		return "", fmt.Errorf("model name missing")
	}
	return name, nil
}

func minimalNewModel(model *yaml.Node, name string) bool {
	if model == nil || model.Kind != yaml.MappingNode || len(model.Content) != 2 || !emptyNodeDecorations(model) {
		return false
	}
	key, value := model.Content[0], model.Content[1]
	return key.Kind == yaml.ScalarNode && key.Tag == "!!str" && key.Value == "name" && emptyNodeDecorations(key) &&
		value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Value == name && strings.TrimSpace(name) != "" && emptyNodeDecorations(value)
}

func emptyNodeDecorations(node *yaml.Node) bool {
	return node.Style == 0 && node.Anchor == "" && node.HeadComment == "" && node.LineComment == "" && node.FootComment == "" && node.Alias == nil
}

type yamlNodePair struct {
	left  *yaml.Node
	right *yaml.Node
}

func sameNodeShell(left, right *yaml.Node) bool {
	if !sameNodeProperties(left, right) {
		return false
	}
	return nodesIdenticalSeen(left.Alias, right.Alias, make(map[yamlNodePair]bool))
}

func nodesIdentical(left, right *yaml.Node) bool {
	return nodesIdenticalSeen(left, right, make(map[yamlNodePair]bool))
}

func nodesIdenticalSeen(left, right *yaml.Node, seen map[yamlNodePair]bool) bool {
	if left == nil || right == nil {
		return left == right
	}
	pair := yamlNodePair{left: left, right: right}
	if seen[pair] {
		return true
	}
	seen[pair] = true
	if !sameNodeProperties(left, right) || !nodesIdenticalSeen(left.Alias, right.Alias, seen) || len(left.Content) != len(right.Content) {
		return false
	}
	for i := range left.Content {
		if !nodesIdenticalSeen(left.Content[i], right.Content[i], seen) {
			return false
		}
	}
	return true
}

func sameNodeProperties(left, right *yaml.Node) bool {
	return left != nil && right != nil && left.Kind == right.Kind && left.Tag == right.Tag && left.Value == right.Value &&
		left.Style == right.Style && left.Anchor == right.Anchor && left.HeadComment == right.HeadComment &&
		left.LineComment == right.LineComment && left.FootComment == right.FootComment
}
