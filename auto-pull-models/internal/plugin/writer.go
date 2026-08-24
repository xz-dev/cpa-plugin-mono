package plugin

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	backupSuffix = ".auto-pull-bak."
	maxBackups   = 10
)

func readModelsFile(configFile string) (map[string][]ModelRef, error) {
	raw, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var doc struct {
		Providers []struct {
			Name   string     `yaml:"name"`
			Models []ModelRef `yaml:"models"`
		} `yaml:"openai-compatibility"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	models := make(map[string][]ModelRef, len(doc.Providers))
	for _, provider := range doc.Providers {
		models[provider.Name] = provider.Models
	}
	return models, nil
}

// writeModelsFile replaces the models list of the named openai-compatibility
// providers inside configFile. New YAML is fully prepared first, the previous
// file is copied into a FIFO of up to 10 backups, then the live file is
// overwritten in place so CPA's inode watcher sees a Write (rename would
// leave it watching a dead inode).
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
	if err := writeBackup(configFile, raw, mode); err != nil {
		return err
	}
	pruneBackups(configFile)
	if err := writeInPlace(configFile, buf.Bytes()); err != nil {
		_ = writeInPlace(configFile, raw)
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func writeBackup(configFile string, raw []byte, mode os.FileMode) error {
	path := fmt.Sprintf("%s%s%d", configFile, backupSuffix, time.Now().UnixNano())
	if err := os.WriteFile(path, raw, mode); err != nil {
		return fmt.Errorf("backup config: %w", err)
	}
	return nil
}

func pruneBackups(configFile string) {
	matches, err := filepath.Glob(configFile + backupSuffix + "*")
	if err != nil || len(matches) <= maxBackups {
		return
	}
	sort.Strings(matches)
	for _, path := range matches[:len(matches)-maxBackups] {
		_ = os.Remove(path)
	}
}

func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.Write(data)
	if err != nil {
		return err
	}
	if n < len(data) {
		return io.ErrShortWrite
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return err
	}
	return f.Sync()
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
