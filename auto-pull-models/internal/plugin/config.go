package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ModeInclude = "include"
	ModeExclude = "exclude"

	// WriteModeAPI writes via PATCH /v0/management/openai-compatibility.
	WriteModeAPI = "api"
	// WriteModeFile writes config.yaml in place (same inode) after copying
	// the previous file into a FIFO of up to 10 backups. Same-inode Write
	// events are what CPA's watcher.Add(configPath) actually sees; rename
	// over the watched path leaves the watcher on a dead inode.
	WriteModeFile = "file"
)

type FileConfig struct {
	Interval            string                    `json:"interval"`
	ManagementBaseURL   string                    `json:"management_base_url"`
	ManagementKeyEnv    string                    `json:"management_key_env"`
	ManagementKeyFile   string                    `json:"management_key_file"`
	KeepExistingAliases bool                      `json:"keep_existing_aliases"`
	ModelparamsURL      string                    `json:"modelparams_url,omitempty"`
	ModelsdevURL        string                    `json:"modelsdev_url,omitempty"`
	WriteMode           string                    `json:"write_mode"`
	ConfigPath          string                    `json:"config_path"`
	Providers           map[string]ProviderConfig `json:"providers"`
}

type ProviderConfig struct {
	Enabled         bool                     `json:"enabled"`
	Mode            string                   `json:"mode"`
	Patterns        []string                 `json:"patterns"`
	MetadataSources []string                 `json:"metadata_sources,omitempty"`
	Modelparams     bool                     `json:"modelparams,omitempty"` // legacy, intentionally ignored
	UpstreamMeta    bool                     `json:"upstream_meta,omitempty"`
	CodexManifest   bool                     `json:"codex_manifest,omitempty"`
	Modelsdev       bool                     `json:"modelsdev,omitempty"` // legacy, intentionally ignored
	Overrides       map[string]ModelOverride `json:"overrides,omitempty"`
}

type metadataSource struct {
	ID       string
	Website  string
	Provider string
	AuthType string
}

type ModelOverride struct {
	MaxContextLength int      `json:"max_context_length,omitempty"`
	MaxInputTokens   int      `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
	ThinkingLevels   []string `json:"thinking_levels,omitempty"`
}

type compiledProvider struct {
	Name            string
	Enabled         bool
	Mode            string
	Patterns        []*regexp.Regexp
	MetadataSources []metadataSource
	UpstreamMeta    bool
	CodexManifest   bool
	Overrides       map[string]ModelOverride
}

type runtimeConfig struct {
	Interval            time.Duration
	ManagementBaseURL   string
	ManagementKeyEnv    string
	ManagementKeyFile   string
	KeepExistingAliases bool
	ModelparamsURL      string
	ModelsdevURL        string
	WriteMode           string
	ConfigPath          string
	Providers           []compiledProvider
	Raw                 FileConfig
}

func defaultFileConfig() FileConfig {
	return FileConfig{
		Interval:            "1h",
		ManagementBaseURL:   "http://127.0.0.1:8317",
		KeepExistingAliases: true,
		Providers:           map[string]ProviderConfig{},
	}
}

func parseFileConfig(raw []byte) (runtimeConfig, error) {
	cfg := defaultFileConfig()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return runtimeConfig{}, fmt.Errorf("decode auto-pull-models json: %w", err)
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
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}

	var interval time.Duration
	if strings.TrimSpace(cfg.Interval) != "" && cfg.Interval != "0" {
		parsed, err := time.ParseDuration(cfg.Interval)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("interval: %w", err)
		}
		if parsed < 0 {
			return runtimeConfig{}, fmt.Errorf("interval must be >= 0")
		}
		interval = parsed
	}

	writeMode := strings.ToLower(strings.TrimSpace(cfg.WriteMode))
	if writeMode == "" {
		writeMode = WriteModeAPI
	}
	if writeMode != WriteModeAPI && writeMode != WriteModeFile {
		return runtimeConfig{}, fmt.Errorf("write_mode must be api or file")
	}
	cfg.ConfigPath = strings.TrimSpace(cfg.ConfigPath)

	out := runtimeConfig{
		Interval:            interval,
		ManagementBaseURL:   cfg.ManagementBaseURL,
		ManagementKeyEnv:    cfg.ManagementKeyEnv,
		ManagementKeyFile:   cfg.ManagementKeyFile,
		KeepExistingAliases: cfg.KeepExistingAliases,
		ModelparamsURL:      strings.TrimSpace(cfg.ModelparamsURL),
		ModelsdevURL:        strings.TrimSpace(cfg.ModelsdevURL),
		WriteMode:           writeMode,
		ConfigPath:          cfg.ConfigPath,
		Raw:                 cfg,
	}

	for name, spec := range cfg.Providers {
		name = strings.TrimSpace(name)
		if name == "" {
			return runtimeConfig{}, fmt.Errorf("provider name is empty")
		}
		mode := strings.ToLower(strings.TrimSpace(spec.Mode))
		if mode == "" {
			mode = ModeInclude
		}
		if mode != ModeInclude && mode != ModeExclude {
			return runtimeConfig{}, fmt.Errorf("provider %s: mode must be include or exclude", name)
		}
		if spec.Modelparams || spec.Modelsdev {
			return runtimeConfig{}, fmt.Errorf("provider %s: replace legacy modelparams/modelsdev with metadata_sources", name)
		}
		compiled := compiledProvider{
			Name:          name,
			Enabled:       spec.Enabled,
			Mode:          mode,
			UpstreamMeta:  spec.UpstreamMeta,
			CodexManifest: spec.CodexManifest,
			Overrides:     make(map[string]ModelOverride, len(spec.Overrides)),
		}
		seenSources := map[string]struct{}{}
		seenModelsdev := false
		for i, rawSource := range spec.MetadataSources {
			source, err := parseMetadataSource(rawSource)
			if err != nil {
				return runtimeConfig{}, fmt.Errorf("provider %s metadata_sources[%d]: %w", name, i, err)
			}
			if _, exists := seenSources[source.ID]; exists {
				return runtimeConfig{}, fmt.Errorf("provider %s metadata_sources[%d]: duplicate source %q", name, i, source.ID)
			}
			if source.Website == "modelparams.dev" && seenModelsdev {
				return runtimeConfig{}, fmt.Errorf("provider %s metadata_sources[%d]: modelparams.dev sources must precede models.dev sources", name, i)
			}
			if source.Website == "models.dev" {
				seenModelsdev = true
			}
			seenSources[source.ID] = struct{}{}
			compiled.MetadataSources = append(compiled.MetadataSources, source)
		}
		for model, override := range spec.Overrides {
			trimmedModel := strings.TrimSpace(model)
			if trimmedModel == "" {
				return runtimeConfig{}, fmt.Errorf("provider %s: override model name is empty", name)
			}
			if trimmedModel != model {
				return runtimeConfig{}, fmt.Errorf("provider %s model %q: override model name must not have surrounding whitespace", name, model)
			}
			if override.MaxContextLength < 0 || override.MaxInputTokens < 0 || override.MaxOutputTokens < 0 {
				return runtimeConfig{}, fmt.Errorf("provider %s model %s: token limits must be >= 0", name, model)
			}
			if len(override.ThinkingLevels) > 0 {
				for _, level := range override.ThinkingLevels {
					normalized := strings.ToLower(strings.TrimSpace(level))
					if _, ok := cpaThinkingLevels[normalized]; !ok {
						return runtimeConfig{}, fmt.Errorf("provider %s model %s: unsupported thinking level %q", name, model, level)
					}
				}
				override.ThinkingLevels = normalizeEfforts(override.ThinkingLevels)
				if len(override.ThinkingLevels) == 0 {
					return runtimeConfig{}, fmt.Errorf("provider %s model %s: thinking_levels must include a reasoning depth", name, model)
				}
			}
			compiled.Overrides[model] = override
		}
		for i, pat := range spec.Patterns {
			pat = strings.TrimSpace(pat)
			if pat == "" {
				continue
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return runtimeConfig{}, fmt.Errorf("provider %s pattern[%d]: %w", name, i, err)
			}
			compiled.Patterns = append(compiled.Patterns, re)
		}
		out.Providers = append(out.Providers, compiled)
	}
	return out, nil
}

func parseMetadataSource(raw string) (metadataSource, error) {
	if raw != strings.TrimSpace(raw) || raw == "" {
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
		return metadataSource{}, fmt.Errorf("invalid source %q; want modelparams.dev/<provider>/<api_key|subscription> or models.dev/<provider>", raw)
	}
	if source.Provider != strings.ToLower(source.Provider) || strings.TrimSpace(source.Provider) != source.Provider {
		return metadataSource{}, fmt.Errorf("invalid source %q; provider must be lowercase without whitespace", raw)
	}
	return source, nil
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
		ConfigFile string `json:"config_file" yaml:"config_file"`
	}
	_ = json.Unmarshal(pluginYAML, &wrapper)
	if strings.TrimSpace(wrapper.ConfigFile) != "" {
		path = strings.TrimSpace(wrapper.ConfigFile)
	} else {
		// CPA plugin config is YAML, not JSON. Parse a tiny subset.
		for _, line := range strings.Split(string(pluginYAML), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "config_file:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "config_file:"))
				val = strings.Trim(val, `"'`)
				if val != "" {
					path = val
				}
			}
		}
	}
	return path
}

func resolveManagementKey(cfg runtimeConfig) string {
	if cfg.ManagementKeyFile != "" {
		raw, err := os.ReadFile(cfg.ManagementKeyFile)
		if err == nil {
			if key := strings.TrimSpace(string(raw)); key != "" {
				return key
			}
		}
	}
	if cfg.ManagementKeyEnv != "" {
		if key := strings.TrimSpace(os.Getenv(cfg.ManagementKeyEnv)); key != "" {
			return key
		}
	}
	return ""
}
