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
	// WriteModeFile writes config.yaml directly with tmp+rename so the CPA
	// file watcher only ever sees complete files. Management PATCH truncates
	// the whole config in place (os.Create + write), which can race the
	// watcher into reading a partial YAML and permanently disabling the
	// management routes. File mode mirrors how CPA's own auth-file writes
	// stay safe.
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
	Enabled       bool     `json:"enabled"`
	Mode          string   `json:"mode"`
	Patterns      []string `json:"patterns"`
	Modelparams   bool     `json:"modelparams,omitempty"`
	UpstreamMeta  bool     `json:"upstream_meta,omitempty"`
	CodexManifest bool     `json:"codex_manifest,omitempty"`
	Modelsdev     bool     `json:"modelsdev,omitempty"`
}

type compiledProvider struct {
	Name          string
	Enabled       bool
	Mode          string
	Patterns      []*regexp.Regexp
	Modelparams   bool
	UpstreamMeta  bool
	CodexManifest bool
	Modelsdev     bool
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
		compiled := compiledProvider{
			Name:          name,
			Enabled:       spec.Enabled,
			Mode:          mode,
			Modelparams:   spec.Modelparams,
			UpstreamMeta:  spec.UpstreamMeta,
			CodexManifest: spec.CodexManifest,
			Modelsdev:     spec.Modelsdev,
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
