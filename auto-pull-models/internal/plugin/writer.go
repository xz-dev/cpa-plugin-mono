package plugin

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// writeModelsFile atomically replaces the models list of the named
// openai-compatibility providers inside configFile.
//
// It writes a temp file and renames it over the original, so the CPA file
// watcher only ever observes a complete file — the same property that keeps
// CPA's own auth-file writes safe. A management PATCH instead rewrites the
// whole config with os.Create (truncate + write), and the watcher can read
// the truncated/partial YAML mid-write and permanently disable management
// routes because remote-management.secret-key appeared to vanish.
func writeModelsFile(configFile string, updates map[string][]ModelRef) error {
	raw, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("invalid yaml document structure")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("expected root mapping node")
	}

	seq := findMappingValue(root, "openai-compatibility")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return fmt.Errorf("openai-compatibility section not found")
	}

	pending := make(map[string][]ModelRef, len(updates))
	for name := range updates {
		pending[name] = updates[name]
	}
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		nameVal := findMappingValue(item, "name")
		if nameVal == nil || nameVal.Tag != "!!str" {
			continue
		}
		models, ok := pending[nameVal.Value]
		if !ok {
			continue
		}
		rendered, err := yaml.Marshal(models)
		if err != nil {
			return fmt.Errorf("marshal models for %s: %w", nameVal.Value, err)
		}
		var modelsNode yaml.Node
		if err := yaml.Unmarshal(rendered, &modelsNode); err != nil {
			return fmt.Errorf("parse rendered models for %s: %w", nameVal.Value, err)
		}
		if modelsNode.Kind != yaml.DocumentNode || modelsNode.Content[0] == nil {
			return fmt.Errorf("invalid rendered models for %s", nameVal.Value)
		}
		setMappingValue(item, "models", modelsNode.Content[0])
		delete(pending, nameVal.Value)
	}
	if len(pending) > 0 {
		names := make([]string, 0, len(pending))
		for name := range pending {
			names = append(names, name)
		}
		return fmt.Errorf("provider(s) not found in config: %v", names)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		_ = enc.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close encoder: %w", err)
	}

	mode := os.FileMode(0o600)
	if info, err := os.Stat(configFile); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := configFile + ".auto-pull-tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmp, configFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

func findMappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Tag == "!!str" && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setMappingValue(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Tag == "!!str" && m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	m.Content = append(m.Content, keyNode, value)
}
