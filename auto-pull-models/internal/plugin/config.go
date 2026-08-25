package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ModeInclude = "include"
	ModeExclude = "exclude"
)

type FileConfig struct {
	Interval            string          `json:"interval"`
	ManagementBaseURL   string          `json:"management_base_url"`
	ManagementKeyEnv    string          `json:"management_key_env"`
	ManagementKeyFile   string          `json:"management_key_file"`
	KeepExistingAliases bool            `json:"keep_existing_aliases"`
	Channels            []ChannelConfig `json:"channels"`
}

type ChannelConfig struct {
	Enabled       bool            `json:"enabled"`
	Selector      ChannelSelector `json:"selector"`
	Mode          string          `json:"mode"`
	Patterns      []string        `json:"patterns"`
	CodexManifest bool            `json:"codex_manifest,omitempty"`
}

type ChannelSelector struct {
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"base_url"`
}

type compiledChannel struct {
	Enabled       bool
	Selector      ChannelSelector
	Mode          string
	Patterns      []*regexp.Regexp
	CodexManifest bool
}

type runtimeConfig struct {
	Interval            time.Duration
	ManagementBaseURL   string
	ManagementKeyEnv    string
	ManagementKeyFile   string
	KeepExistingAliases bool
	Channels            []compiledChannel
	Raw                 FileConfig
}

func defaultFileConfig() FileConfig {
	return FileConfig{
		Interval:            "0",
		ManagementBaseURL:   "http://127.0.0.1:8317",
		KeepExistingAliases: true,
		Channels:            []ChannelConfig{},
	}
}

func parseFileConfig(raw []byte) (runtimeConfig, error) {
	cfg := defaultFileConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return runtimeConfig{}, fmt.Errorf("decode auto-pull-models channels schema (combined providers/metadata config is no longer supported): %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return runtimeConfig{}, fmt.Errorf("decode auto-pull-models: trailing JSON value")
		}
	}
	return compileConfig(cfg)
}

func compileConfig(cfg FileConfig) (runtimeConfig, error) {
	cfg.ManagementBaseURL = strings.TrimRight(strings.TrimSpace(cfg.ManagementBaseURL), "/")
	if cfg.ManagementBaseURL == "" {
		cfg.ManagementBaseURL = "http://127.0.0.1:8317"
	}
	cfg.ManagementKeyEnv = strings.TrimSpace(cfg.ManagementKeyEnv)
	cfg.ManagementKeyFile = strings.TrimSpace(cfg.ManagementKeyFile)
	if cfg.Channels == nil {
		cfg.Channels = []ChannelConfig{}
	}

	var interval time.Duration
	if value := strings.TrimSpace(cfg.Interval); value != "" && value != "0" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("interval: %w", err)
		}
		if parsed < 0 {
			return runtimeConfig{}, fmt.Errorf("interval must be >= 0")
		}
		interval = parsed
	}

	out := runtimeConfig{
		Interval:            interval,
		ManagementBaseURL:   cfg.ManagementBaseURL,
		ManagementKeyEnv:    cfg.ManagementKeyEnv,
		ManagementKeyFile:   cfg.ManagementKeyFile,
		KeepExistingAliases: cfg.KeepExistingAliases,
		Raw:                 cfg,
	}
	seen := map[string]struct{}{}
	for i, spec := range cfg.Channels {
		selector, err := normalizeOpenAISelector(spec.Selector)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("channels[%d].selector: %w", i, err)
		}
		key := selectorKey("openai-compatibility", selector)
		if _, exists := seen[key]; exists {
			return runtimeConfig{}, fmt.Errorf("channels[%d]: duplicate selector %s", i, key)
		}
		seen[key] = struct{}{}
		mode := strings.ToLower(strings.TrimSpace(spec.Mode))
		if mode == "" {
			mode = ModeInclude
		}
		if mode != ModeInclude && mode != ModeExclude {
			return runtimeConfig{}, fmt.Errorf("channels[%d]: mode must be include or exclude", i)
		}
		compiled := compiledChannel{Enabled: spec.Enabled, Selector: selector, Mode: mode, CodexManifest: spec.CodexManifest}
		for j, pattern := range spec.Patterns {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return runtimeConfig{}, fmt.Errorf("channels[%d].patterns[%d]: %w", i, j, err)
			}
			compiled.Patterns = append(compiled.Patterns, re)
		}
		out.Channels = append(out.Channels, compiled)
		cfg.Channels[i].Selector = selector
		cfg.Channels[i].Mode = mode
	}
	out.Raw = cfg
	return out, nil
}

func normalizeOpenAISelector(selector ChannelSelector) (ChannelSelector, error) {
	selector.Name = strings.TrimSpace(selector.Name)
	if selector.Name == "" {
		return ChannelSelector{}, fmt.Errorf("name is required")
	}
	base, err := normalizeBaseURL(selector.BaseURL)
	if err != nil {
		return ChannelSelector{}, err
	}
	selector.BaseURL = base
	return selector, nil
}

func normalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base_url must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("base_url must not contain userinfo or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("base_url scheme must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	parsed.Host = host
	if parsed.Path == "/" {
		parsed.Path = ""
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	return parsed.String(), nil
}

func selectorKey(kind string, selector ChannelSelector) string {
	return kind + "|" + selector.Name + "|" + selector.BaseURL
}

func loadJSONFile(path string) (runtimeConfig, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg, cerr := compileConfig(defaultFileConfig())
			return cfg, nil, cerr
		}
		return runtimeConfig{}, nil, err
	}
	cfg, err := parseFileConfig(raw)
	return cfg, raw, err
}

func writeJSONFile(path string, cfg FileConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func resolveJSONPath(pluginYAML []byte) string {
	path := "plugins/auto-pull-models/config.json"
	var wrapper struct {
		ConfigFile string `json:"config_file"`
	}
	_ = json.Unmarshal(pluginYAML, &wrapper)
	if value := strings.TrimSpace(wrapper.ConfigFile); value != "" {
		return value
	}
	for _, line := range strings.Split(string(pluginYAML), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "config_file:") {
			value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "config_file:")), `"'`)
			if value != "" {
				return value
			}
		}
	}
	return path
}

func resolveManagementKey(cfg runtimeConfig) string {
	if cfg.ManagementKeyFile != "" {
		if raw, err := os.ReadFile(cfg.ManagementKeyFile); err == nil {
			if key := strings.TrimSpace(string(raw)); key != "" {
				return key
			}
		}
	}
	if cfg.ManagementKeyEnv != "" {
		return strings.TrimSpace(os.Getenv(cfg.ManagementKeyEnv))
	}
	return ""
}
