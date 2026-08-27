package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	KindOpenAI = "openai-compatibility"
	KindClaude = "claude"

	defaultModelparamsURL = "https://modelparams.dev/api/v1/models.json"
	defaultModelsdevURL   = "https://models.dev/api.json"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type PlannerConfig struct {
	WorkerTokenEnv string          `yaml:"worker_token_env" json:"worker_token_env"`
	SyncEpoch      string          `yaml:"sync_epoch,omitempty" json:"sync_epoch,omitempty"`
	Channels       []ChannelConfig `yaml:"channels" json:"channels"`
}

type ChannelConfig struct {
	Enabled         bool                     `yaml:"enabled" json:"enabled"`
	Kind            string                   `yaml:"kind" json:"kind"`
	Selector        ChannelSelector          `yaml:"selector" json:"selector"`
	UpstreamMeta    bool                     `yaml:"upstream_meta,omitempty" json:"upstream_meta,omitempty"`
	CodexManifest   bool                     `yaml:"codex_manifest,omitempty" json:"codex_manifest,omitempty"`
	MetadataSources []string                 `yaml:"metadata_sources,omitempty" json:"metadata_sources,omitempty"`
	Overrides       map[string]ModelOverride `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

type ChannelSelector struct {
	Name        string `yaml:"name,omitempty" json:"name,omitempty"`
	BaseURL     string `yaml:"base_url" json:"base_url"`
	ConfigIndex *int   `yaml:"config_index,omitempty" json:"config_index,omitempty"`
	Prefix      string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
}

type metadataSource struct {
	ID       string `json:"id"`
	Website  string `json:"website"`
	Provider string `json:"provider"`
	AuthType string `json:"auth_type,omitempty"`
}

type ModelOverride struct {
	MaxContextLength int      `yaml:"max_context_length,omitempty" json:"max_context_length,omitempty"`
	MaxInputTokens   int      `yaml:"max_input_tokens,omitempty" json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int      `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	ThinkingLevels   []string `yaml:"thinking_levels,omitempty" json:"thinking_levels,omitempty"`
	InputModalities  []string `yaml:"input_modalities,omitempty" json:"input_modalities,omitempty"`
	OutputModalities []string `yaml:"output_modalities,omitempty" json:"output_modalities,omitempty"`
}

type compiledChannel struct {
	Enabled         bool                     `json:"enabled"`
	Kind            string                   `json:"kind"`
	Selector        ChannelSelector          `json:"selector"`
	UpstreamMeta    bool                     `json:"upstream_meta"`
	CodexManifest   bool                     `json:"codex_manifest"`
	MetadataSources []metadataSource         `json:"metadata_sources"`
	Overrides       map[string]ModelOverride `json:"overrides"`
}

type runtimeConfig struct {
	WorkerToken string
	Channels    []compiledChannel
	SHA256      string
	Generation  uint64
	AttemptID   string
}

func parseConfig(raw []byte) (runtimeConfig, error) {
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := nodeDecoder.Decode(&document); err != nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || validateMappings(document.Content[0], make(map[*yaml.Node]bool)) != nil || hasYAMLIndirection(document.Content[0], make(map[*yaml.Node]bool)) {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	var trailing yaml.Node
	if err := nodeDecoder.Decode(&trailing); err != io.EOF {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var cfg PlannerConfig
	if err := decoder.Decode(&cfg); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	cfg.WorkerTokenEnv = strings.TrimSpace(cfg.WorkerTokenEnv)
	if !envNamePattern.MatchString(cfg.WorkerTokenEnv) {
		return runtimeConfig{}, fmt.Errorf("worker_token_env is required")
	}
	token := os.Getenv(cfg.WorkerTokenEnv)
	if token == "" {
		return runtimeConfig{}, fmt.Errorf("worker coordination token is unavailable")
	}
	if cfg.Channels == nil {
		cfg.Channels = []ChannelConfig{}
	}
	out := runtimeConfig{WorkerToken: token}
	sum := sha256.Sum256(raw)
	out.SHA256 = hex.EncodeToString(sum[:])
	seen := make(map[string]bool)
	enabled := 0
	for index, spec := range cfg.Channels {
		kind := strings.ToLower(strings.TrimSpace(spec.Kind))
		if kind != KindOpenAI && kind != KindClaude {
			return runtimeConfig{}, fmt.Errorf("channels[%d].kind must be %s or %s", index, KindOpenAI, KindClaude)
		}
		selector, err := normalizeSelector(kind, spec.Selector)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("channels[%d].selector: %w", index, err)
		}
		key := selectorKey(kind, selector)
		if seen[key] {
			return runtimeConfig{}, fmt.Errorf("channels[%d]: duplicate selector", index)
		}
		seen[key] = true
		if spec.CodexManifest && kind != KindOpenAI {
			return runtimeConfig{}, fmt.Errorf("channels[%d]: codex_manifest requires openai-compatibility", index)
		}
		compiled := compiledChannel{Enabled: spec.Enabled, Kind: kind, Selector: selector, UpstreamMeta: spec.UpstreamMeta, CodexManifest: spec.CodexManifest, Overrides: map[string]ModelOverride{}}
		if spec.Enabled {
			enabled++
			if enabled > 100 {
				return runtimeConfig{}, fmt.Errorf("at most 100 channels may be enabled")
			}
		}
		seenSources, modelsdevSeen := map[string]bool{}, false
		for sourceIndex, rawSource := range spec.MetadataSources {
			source, err := parseMetadataSource(rawSource)
			if err != nil {
				return runtimeConfig{}, fmt.Errorf("channels[%d].metadata_sources[%d]: %w", index, sourceIndex, err)
			}
			if seenSources[source.ID] {
				return runtimeConfig{}, fmt.Errorf("channels[%d]: duplicate metadata source", index)
			}
			if source.Website == "modelparams.dev" && modelsdevSeen {
				return runtimeConfig{}, fmt.Errorf("channels[%d]: modelparams.dev sources must precede models.dev sources", index)
			}
			seenSources[source.ID] = true
			modelsdevSeen = modelsdevSeen || source.Website == "models.dev"
			compiled.MetadataSources = append(compiled.MetadataSources, source)
		}
		for model, override := range spec.Overrides {
			if model == "" || model != strings.TrimSpace(model) || len(model) > 1024 || hasControl(model) {
				return runtimeConfig{}, fmt.Errorf("channels[%d]: override model names must be exact non-empty upstream names", index)
			}
			if override.MaxContextLength < 0 || override.MaxInputTokens < 0 || override.MaxOutputTokens < 0 {
				return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: token limits must be >= 0", index, model)
			}
			if len(override.ThinkingLevels) > 0 {
				for _, level := range override.ThinkingLevels {
					if _, ok := cpaThinkingLevels[strings.ToLower(strings.TrimSpace(level))]; !ok {
						return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: unsupported thinking level", index, model)
					}
				}
				override.ThinkingLevels = normalizeEfforts(override.ThinkingLevels)
				if len(override.ThinkingLevels) == 0 {
					return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: thinking_levels require a reasoning depth", index, model)
				}
			}
			for field, values := range map[string][]string{"input_modalities": override.InputModalities, "output_modalities": override.OutputModalities} {
				if len(values) == 0 {
					continue
				}
				for _, value := range uniqueNormalized(values) {
					if value != "text" && value != "image" {
						return runtimeConfig{}, fmt.Errorf("channels[%d] model %s: %s supports only text/image", index, model, field)
					}
				}
			}
			override.InputModalities = cpaModalities(override.InputModalities)
			override.OutputModalities = cpaModalities(override.OutputModalities)
			compiled.Overrides[model] = override
		}
		out.Channels = append(out.Channels, compiled)
	}
	return out, nil
}

func normalizeSelector(kind string, selector ChannelSelector) (ChannelSelector, error) {
	base, err := normalizeBaseURL(selector.BaseURL)
	if err != nil {
		return ChannelSelector{}, err
	}
	selector.BaseURL = base
	selector.Name = strings.TrimSpace(selector.Name)
	selector.Prefix = strings.Trim(strings.TrimSpace(selector.Prefix), "/")
	if kind == KindOpenAI {
		if selector.Name == "" || len(selector.Name) > 1024 || hasControl(selector.Name) || selector.ConfigIndex != nil || selector.Prefix != "" {
			return ChannelSelector{}, fmt.Errorf("openai-compatibility requires name + base_url only")
		}
	} else if selector.ConfigIndex == nil || *selector.ConfigIndex < 0 || selector.Name != "" || len(selector.Prefix) > 256 || hasControl(selector.Prefix) {
		return ChannelSelector{}, fmt.Errorf("claude requires config_index + base_url + optional prefix and no name")
	}
	return selector, nil
}

func normalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" {
		return "", fmt.Errorf("base_url must be an absolute HTTPS URL without query or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "443" {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	parsed.Scheme = "https"
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
	if len(source.Provider) > 256 || hasControl(source.Provider) || source.Provider != strings.ToLower(source.Provider) || strings.TrimSpace(source.Provider) != source.Provider {
		return metadataSource{}, fmt.Errorf("source provider must be lowercase without whitespace")
	}
	return source, nil
}

func uniqueNormalized(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}
