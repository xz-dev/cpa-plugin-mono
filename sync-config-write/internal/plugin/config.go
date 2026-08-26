package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultCoreOrigin = "http://127.0.0.1:8317"

var (
	envNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Settings struct {
	CoreOrigin              string
	ManagementKeyEnv        string
	ManagementKey           string
	ModelInfoKeyFingerprint string
	WorkerTokenEnv          string
	WorkerToken             string
	AutoPullInterval        time.Duration
	MetadataSyncInterval    time.Duration
	ModelInfoInterval       time.Duration
	MaxVersionRetries       int
	SyncEpoch               string
	ConfigSHA256            string
}

func (s Settings) String() string {
	return fmt.Sprintf("core_origin=%s management_key_env=%s model_info_proxy_api_key_sha256=%s worker_token_env=%s auto_pull_interval=%s metadata_sync_interval=%s model_info_interval=%s max_version_retries=%d sync_epoch=%s config_sha256=%s", s.CoreOrigin, s.ManagementKeyEnv, s.ModelInfoKeyFingerprint, s.WorkerTokenEnv, s.AutoPullInterval, s.MetadataSyncInterval, s.ModelInfoInterval, s.MaxVersionRetries, s.SyncEpoch, s.ConfigSHA256)
}

type rawSettings struct {
	Enabled                    *bool     `yaml:"enabled"`
	Priority                   int       `yaml:"priority"`
	Store                      yaml.Node `yaml:"store"`
	CoreOrigin                 string    `yaml:"core_origin"`
	ManagementKeyEnv           string    `yaml:"management_key_env"`
	ModelInfoProxyAPIKeySHA256 string    `yaml:"model_info_proxy_api_key_sha256"`
	WorkerTokenEnv             string    `yaml:"worker_token_env"`
	AutoPullInterval           string    `yaml:"auto_pull_interval"`
	MetadataSyncInterval       string    `yaml:"metadata_sync_interval"`
	ModelInfoInterval          string    `yaml:"model_info_interval"`
	MaxVersionRetries          *int      `yaml:"max_version_retries"`
	SyncEpoch                  string    `yaml:"sync_epoch"`
	ManagementKey              string    `yaml:"management_key"`
	WorkerToken                string    `yaml:"worker_token"`
	ModelInfoProxyAPIKey       string    `yaml:"model_info_proxy_api_key"`
}

func parseSettings(raw []byte) (Settings, error) {
	var cfg rawSettings
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Settings{}, fmt.Errorf("invalid ConfigYAML")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Settings{}, fmt.Errorf("invalid ConfigYAML")
	}
	if cfg.ManagementKey != "" || cfg.WorkerToken != "" || cfg.ModelInfoProxyAPIKey != "" {
		return Settings{}, fmt.Errorf("plaintext secrets are forbidden")
	}
	if cfg.CoreOrigin == "" {
		cfg.CoreOrigin = defaultCoreOrigin
	}
	origin, err := validateCoreOrigin(cfg.CoreOrigin)
	if err != nil {
		return Settings{}, err
	}
	managementKey, err := resolveSecret(cfg.ManagementKeyEnv)
	if err != nil {
		return Settings{}, fmt.Errorf("management_key_env: %w", err)
	}
	workerToken, err := resolveSecret(cfg.WorkerTokenEnv)
	if err != nil {
		return Settings{}, fmt.Errorf("worker_token_env: %w", err)
	}
	fingerprint := strings.TrimSpace(cfg.ModelInfoProxyAPIKeySHA256)
	if !fingerprintPattern.MatchString(fingerprint) {
		return Settings{}, fmt.Errorf("model_info_proxy_api_key_sha256 must be 64 lowercase hexadecimal digits")
	}
	auto, err := parseInterval("auto_pull_interval", cfg.AutoPullInterval)
	if err != nil {
		return Settings{}, err
	}
	metadata, err := parseInterval("metadata_sync_interval", cfg.MetadataSyncInterval)
	if err != nil {
		return Settings{}, err
	}
	modelInfo, err := parseInterval("model_info_interval", cfg.ModelInfoInterval)
	if err != nil {
		return Settings{}, err
	}
	retries := 2
	if cfg.MaxVersionRetries != nil {
		retries = *cfg.MaxVersionRetries
	}
	if retries < 0 || retries > 5 {
		return Settings{}, fmt.Errorf("max_version_retries must be between 0 and 5")
	}
	sum := sha256.Sum256(raw)
	return Settings{
		CoreOrigin: origin, ManagementKeyEnv: cfg.ManagementKeyEnv, ManagementKey: managementKey,
		ModelInfoKeyFingerprint: fingerprint, WorkerTokenEnv: cfg.WorkerTokenEnv, WorkerToken: workerToken,
		AutoPullInterval: auto, MetadataSyncInterval: metadata, ModelInfoInterval: modelInfo,
		MaxVersionRetries: retries, SyncEpoch: cfg.SyncEpoch, ConfigSHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func validateCoreOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return "", fmt.Errorf("core_origin must be an HTTP numeric-loopback origin")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || u.Port() == "" {
		return "", fmt.Errorf("core_origin must use a numeric loopback host and explicit port")
	}
	return u.String(), nil
}

func resolveSecret(name string) (string, error) {
	if !envNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid environment variable name")
	}
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("environment variable is empty")
	}
	return value, nil
}

func parseInterval(name, raw string) (time.Duration, error) {
	if raw == "" {
		raw = "0s"
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || (d > 0 && (d < time.Minute || d > 24*time.Hour)) {
		return 0, fmt.Errorf("%s must be 0s or between 1m and 24h", name)
	}
	return d, nil
}
