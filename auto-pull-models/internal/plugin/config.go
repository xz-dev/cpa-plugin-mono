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
	ModeInclude = "include"
	ModeExclude = "exclude"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type PlannerConfig struct {
	WorkerTokenEnv string          `yaml:"worker_token_env" json:"worker_token_env"`
	SyncEpoch      string          `yaml:"sync_epoch,omitempty" json:"sync_epoch,omitempty"`
	Channels       []ChannelConfig `yaml:"channels" json:"channels"`
}

type ChannelConfig struct {
	Enabled  bool            `yaml:"enabled" json:"enabled"`
	Selector ChannelSelector `yaml:"selector" json:"selector"`
	Mode     string          `yaml:"mode" json:"mode"`
	Patterns []string        `yaml:"patterns" json:"patterns"`
}

type ChannelSelector struct {
	Name    string `yaml:"name" json:"name"`
	BaseURL string `yaml:"base_url" json:"base_url"`
}

type compiledChannel struct {
	Enabled  bool
	Selector ChannelSelector
	Mode     string
	Patterns []*regexp.Regexp
}

type runtimeConfig struct {
	WorkerToken string
	Channels    []compiledChannel
	SHA256      string
	Generation  uint64
	AttemptID   string
}

func parseConfig(raw []byte) (runtimeConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var cfg PlannerConfig
	if err := decoder.Decode(&cfg); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	var trailing yaml.Node
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
		selector, err := normalizeOpenAISelector(spec.Selector)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("channels[%d].selector: %w", index, err)
		}
		key := selectorKey(selector)
		if seen[key] {
			return runtimeConfig{}, fmt.Errorf("channels[%d]: duplicate selector", index)
		}
		seen[key] = true
		mode := strings.ToLower(strings.TrimSpace(spec.Mode))
		if mode == "" {
			mode = ModeInclude
		}
		if mode != ModeInclude && mode != ModeExclude {
			return runtimeConfig{}, fmt.Errorf("channels[%d].mode must be include or exclude", index)
		}
		compiled := compiledChannel{Enabled: spec.Enabled, Selector: selector, Mode: mode}
		if spec.Enabled {
			enabled++
			if enabled > 100 {
				return runtimeConfig{}, fmt.Errorf("at most 100 channels may be enabled")
			}
		}
		for patternIndex, pattern := range spec.Patterns {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			expression, err := regexp.Compile(pattern)
			if err != nil {
				return runtimeConfig{}, fmt.Errorf("channels[%d].patterns[%d] is invalid", index, patternIndex)
			}
			compiled.Patterns = append(compiled.Patterns, expression)
		}
		out.Channels = append(out.Channels, compiled)
	}
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

func selectorKey(selector ChannelSelector) string {
	return selector.Name + "\x00" + selector.BaseURL
}
