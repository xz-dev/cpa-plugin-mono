package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	KindOpenAI = "openai-compatibility"
	KindClaude = "claude"
)

type FileConfig struct {
	Interval          string          `json:"interval"`
	ManagementBaseURL string          `json:"management_base_url"`
	ManagementKeyEnv  string          `json:"management_key_env"`
	ManagementKeyFile string          `json:"management_key_file"`
	ModelparamsURL    string          `json:"modelparams_url,omitempty"`
	ModelsdevURL      string          `json:"modelsdev_url,omitempty"`
	Channels          []ChannelConfig `json:"channels"`
}

type ChannelConfig struct {
	Enabled         bool                     `json:"enabled"`
	Kind            string                   `json:"kind"`
	Selector        ChannelSelector          `json:"selector"`
	UpstreamMeta    bool                     `json:"upstream_meta,omitempty"`
	CodexManifest   bool                     `json:"codex_manifest,omitempty"`
	Profile         string                   `json:"profile,omitempty"`
	MetadataSources []string                 `json:"metadata_sources,omitempty"`
	Overrides       map[string]ModelOverride `json:"overrides,omitempty"`
}

type ChannelSelector struct {
	Name        string `json:"name,omitempty"`
	BaseURL     string `json:"base_url"`
	ConfigIndex *int   `json:"config_index,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
}

type metadataSource struct {
	ID, Website, Provider, AuthType string
}

type ModelOverride struct {
	MaxContextLength int      `json:"max_context_length,omitempty"`
	MaxInputTokens   int      `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
	ThinkingLevels   []string `json:"thinking_levels,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
}

type compiledChannel struct {
	Enabled         bool
	Kind            string
	Selector        ChannelSelector
	UpstreamMeta    bool
	CodexManifest   bool
	Profile         string
	MetadataSources []metadataSource
	Overrides       map[string]ModelOverride
}

type runtimeConfig struct {
	Interval          time.Duration
	ManagementBaseURL string
	ManagementKeyEnv  string
	ManagementKeyFile string
	ModelparamsURL    string
	ModelsdevURL      string
	Channels          []compiledChannel
	Raw               FileConfig
}

func defaultFileConfig() FileConfig {
	return FileConfig{Interval: "0", ManagementBaseURL: "http://127.0.0.1:8317", Channels: []ChannelConfig{}}
}

func parseFileConfig(raw []byte) (runtimeConfig, error) {
	cfg := defaultFileConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return runtimeConfig{}, fmt.Errorf("decode model-metadata-sync channels schema (combined provider-name config is no longer supported): %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return runtimeConfig{}, fmt.Errorf("decode model-metadata-sync: trailing JSON value")
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
		if err != nil || parsed < 0 {
			return runtimeConfig{}, fmt.Errorf("interval must be a non-negative Go duration")
		}
		interval = parsed
	}
	out := runtimeConfig{
		Interval: interval, ManagementBaseURL: cfg.ManagementBaseURL, ManagementKeyEnv: cfg.ManagementKeyEnv,
		ManagementKeyFile: cfg.ManagementKeyFile, ModelparamsURL: strings.TrimSpace(cfg.ModelparamsURL), ModelsdevURL: strings.TrimSpace(cfg.ModelsdevURL), Raw: cfg,
	}
	seen := map[string]struct{}{}
	for i, spec := range cfg.Channels {
		kind := strings.ToLower(strings.TrimSpace(spec.Kind))
		if kind != KindOpenAI && kind != KindClaude {
			return runtimeConfig{}, fmt.Errorf("channels[%d].kind must be %s or %s", i, KindOpenAI, KindClaude)
		}
		selector, err := normalizeSelector(kind, spec.Selector)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("channels[%d].selector: %w", i, err)
		}
		key := selectorKey(kind, selector)
		if _, exists := seen[key]; exists {
			return runtimeConfig{}, fmt.Errorf("channels[%d]: duplicate selector %s", i, key)
		}
		seen[key] = struct{}{}
		profile := strings.TrimSpace(spec.Profile)
		if profile == "" {
			if kind == KindClaude {
				profile = "claude_models"
			} else {
				profile = "openai_models"
			}
		}
		if (kind == KindOpenAI && profile != "openai_models") || (kind == KindClaude && profile != "claude_models") {
			return runtimeConfig{}, fmt.Errorf("channels[%d]: profile %q does not match kind %q", i, profile, kind)
		}
		compiled := compiledChannel{Enabled: spec.Enabled, Kind: kind, Selector: selector, UpstreamMeta: spec.UpstreamMeta, CodexManifest: spec.CodexManifest, Profile: profile, Overrides: map[string]ModelOverride{}}
		seenSources, modelsdevSeen := map[string]struct{}{}, false
		for j, rawSource := range spec.MetadataSources {
			source, err := parseMetadataSource(rawSource)
			if err != nil {
				return runtimeConfig{}, fmt.Errorf("channels[%d].metadata_sources[%d]: %w", i, j, err)
			}
			if _, duplicate := seenSources[source.ID]; duplicate {
				return runtimeConfig{}, fmt.Errorf("channels[%d]: duplicate source %q", i, source.ID)
			}
			if source.Website == "modelparams.dev" && modelsdevSeen {
				return runtimeConfig{}, fmt.Errorf("channels[%d]: modelparams.dev sources must precede models.dev sources", i)
			}
			modelsdevSeen = modelsdevSeen || source.Website == "models.dev"
			seenSources[source.ID] = struct{}{}
			compiled.MetadataSources = append(compiled.MetadataSources, source)
		}
		for model, override := range spec.Overrides {
			if model == "" || model != strings.TrimSpace(model) {
				return runtimeConfig{}, fmt.Errorf("channels[%d]: override model names must be exact non-empty upstream names", i)
			}
			if override.MaxContextLength < 0 || override.MaxInputTokens < 0 || override.MaxOutputTokens < 0 {
				return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: token limits must be >= 0", i, model)
			}
			if len(override.ThinkingLevels) > 0 {
				for _, level := range override.ThinkingLevels {
					if _, ok := cpaThinkingLevels[strings.ToLower(strings.TrimSpace(level))]; !ok {
						return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: unsupported thinking level %q", i, model, level)
					}
				}
				override.ThinkingLevels = normalizeEfforts(override.ThinkingLevels)
				if len(override.ThinkingLevels) == 0 {
					return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: thinking_levels must include a reasoning depth", i, model)
				}
			}
			for field, values := range map[string][]string{"input_modalities": override.InputModalities, "output_modalities": override.OutputModalities} {
				if len(values) == 0 {
					continue
				}
				normalized := uniqueNormalized(values)
				valid := true
				for _, value := range normalized {
					if value != "text" && value != "image" {
						valid = false
					}
				}
				if !valid {
					return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: %s supports only text/image", i, model, field)
				}
			}
			override.InputModalities = cpaModalities(override.InputModalities)
			override.OutputModalities = cpaModalities(override.OutputModalities)
			compiled.Overrides[model] = override
		}
		cfg.Channels[i].Kind, cfg.Channels[i].Selector, cfg.Channels[i].Profile = kind, selector, profile
		out.Channels = append(out.Channels, compiled)
	}
	out.Raw = cfg
	return out, nil
}

func normalizeSelector(kind string, selector ChannelSelector) (ChannelSelector, error) {
	base, err := normalizeBaseURL(selector.BaseURL)
	if err != nil {
		return ChannelSelector{}, err
	}
	selector.BaseURL, selector.Name, selector.Prefix = base, strings.TrimSpace(selector.Name), strings.Trim(strings.TrimSpace(selector.Prefix), "/")
	if kind == KindOpenAI {
		if selector.Name == "" || selector.ConfigIndex != nil || selector.Prefix != "" {
			return ChannelSelector{}, fmt.Errorf("openai-compatibility requires name + base_url only")
		}
	} else if selector.ConfigIndex == nil || *selector.ConfigIndex < 0 || selector.Name != "" {
		return ChannelSelector{}, fmt.Errorf("claude requires config_index + base_url + prefix and no name")
	}
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
	if kind == KindClaude {
		return fmt.Sprintf("%s|%d|%s|%s", kind, *selector.ConfigIndex, selector.BaseURL, selector.Prefix)
	}
	return kind + "|" + selector.Name + "|" + selector.BaseURL
}

func parseMetadataSource(raw string) (metadataSource, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return metadataSource{}, fmt.Errorf("source must be a canonical non-empty ID")
	}
	parts := strings.Split(raw, "/")
	source := metadataSource{ID: raw}
	switch {
	case len(parts) == 3 && parts[0] == "modelparams.dev" && parts[1] != "" && (parts[2] == "api_key" || parts[2] == "subscription"):
		source.Website, source.Provider, source.AuthType = parts[0], parts[1], parts[2]
	case len(parts) == 2 && parts[0] == "models.dev" && parts[1] != "":
		source.Website, source.Provider = parts[0], parts[1]
	default:
		return metadataSource{}, fmt.Errorf("invalid source %q", raw)
	}
	if source.Provider != strings.ToLower(source.Provider) || strings.TrimSpace(source.Provider) != source.Provider {
		return metadataSource{}, fmt.Errorf("source provider must be lowercase without whitespace")
	}
	return source, nil
}

func uniqueNormalized(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
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
	path := "plugins/model-metadata-sync/config.json"
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
			if value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "config_file:")), `"'`); value != "" {
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
