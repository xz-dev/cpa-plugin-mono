package plugin

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ConfigYAML includes Core-owned lifecycle/store fields plus model-info fields.
type pluginConfig struct {
	Enabled        *bool     `yaml:"enabled"`
	Priority       int       `yaml:"priority"`
	Store          yaml.Node `yaml:"store"`
	WorkerTokenEnv string    `yaml:"worker_token_env"`
	SyncEpoch      string    `yaml:"sync_epoch,omitempty"`
}

type runtimeConfig struct {
	WorkerToken string
	SHA256      string
}

type ingestAuthorization struct {
	generation uint64
	ticket     uint64
}

type Service struct {
	mu                    sync.Mutex
	cfg                   runtimeConfig
	configured            bool
	instanceID            string
	reconfigureSeq        uint64
	latestIngestTicket    uint64
	committedIngestTicket uint64
	last                  Catalog
}

func New() *Service {
	instanceID, err := opaqueID()
	if err != nil {
		panic("crypto/rand unavailable")
	}
	return &Service{instanceID: instanceID}
}

func (s *Service) Configure(configYAML []byte) error {
	cfg, err := parseConfig(configYAML)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.configured = true
	s.reconfigureSeq++
	s.latestIngestTicket++
	s.mu.Unlock()
	return nil
}

func parseConfig(raw []byte) (runtimeConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var cfg pluginConfig
	if err := decoder.Decode(&cfg); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}

	nodeDecoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := nodeDecoder.Decode(&document); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || document.Content[0].Tag != "!!map" || document.Content[0].Anchor != "" {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	root := document.Content[0]
	if len(root.Content)%2 != 0 {
		return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
	}
	seen := make([]string, 0, len(root.Content)/2)
	for index := 0; index < len(root.Content); index += 2 {
		key, value := root.Content[index], root.Content[index+1]
		if key == nil || value == nil || key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" || hasControl(key.Value) || key.Anchor != "" || key.Alias != nil || key.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || foldedDuplicate(seen, key.Value) {
			return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
		}
		seen = append(seen, key.Value)
		switch key.Value {
		case "store":
			if value.Kind != yaml.MappingNode || validateOpaqueStoreNode(value, make(map[*yaml.Node]bool)) != nil {
				return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
			}
		case "worker_token_env":
			if !plainScalar(value, "!!str") {
				return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
			}
		case "sync_epoch":
			if !plainScalar(value, "!!str") {
				return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
			}
		case "enabled":
			if !plainScalar(value, "!!bool") {
				return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
			}
		case "priority":
			if !plainScalar(value, "!!int") {
				return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
			}
		default:
			return runtimeConfig{}, fmt.Errorf("invalid plugin configuration")
		}
	}
	workerTokenEnv := strings.TrimSpace(cfg.WorkerTokenEnv)
	if !envNamePattern.MatchString(workerTokenEnv) {
		return runtimeConfig{}, fmt.Errorf("worker_token_env is required")
	}
	workerToken := os.Getenv(workerTokenEnv)
	if strings.TrimSpace(workerToken) == "" {
		return runtimeConfig{}, fmt.Errorf("worker coordination token is unavailable")
	}
	sum := sha256.Sum256(raw)
	return runtimeConfig{WorkerToken: workerToken, SHA256: hex.EncodeToString(sum[:])}, nil
}

func plainScalar(node *yaml.Node, tag string) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == tag && node.Anchor == "" && node.Alias == nil && node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0
}

func foldedDuplicate(seen []string, candidate string) bool {
	for _, key := range seen {
		if strings.EqualFold(key, candidate) {
			return true
		}
	}
	return false
}

func validateOpaqueStoreNode(node *yaml.Node, visited map[*yaml.Node]bool) error {
	if node == nil || node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" {
		return fmt.Errorf("invalid store node")
	}
	if visited[node] {
		return fmt.Errorf("invalid store graph")
	}
	visited[node] = true
	switch node.Kind {
	case yaml.MappingNode:
		if node.Tag != "!!map" || len(node.Content)%2 != 0 {
			return fmt.Errorf("invalid store mapping")
		}
		seen := make([]string, 0, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key == nil || key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" || hasControl(key.Value) || key.Anchor != "" || key.Alias != nil || key.Value == "<<" || key.Tag == "!!merge" || foldedDuplicate(seen, key.Value) {
				return fmt.Errorf("invalid store mapping key")
			}
			seen = append(seen, key.Value)
			if err := validateOpaqueStoreNode(value, visited); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return fmt.Errorf("invalid store sequence")
		}
		for _, child := range node.Content {
			if err := validateOpaqueStoreNode(child, visited); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str", "!!bool", "!!int", "!!float", "!!null", "!!timestamp":
		default:
			return fmt.Errorf("invalid store scalar")
		}
	default:
		return fmt.Errorf("invalid store node")
	}
	return nil
}

func (s *Service) authorize(token string) (ingestAuthorization, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorizedLocked(token) {
		return ingestAuthorization{}, false
	}
	s.latestIngestTicket++
	return ingestAuthorization{generation: s.reconfigureSeq, ticket: s.latestIngestTicket}, true
}

func (s *Service) authorizedLocked(token string) bool {
	expected := s.cfg.WorkerToken
	return s.configured && len(token) == len(expected) && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func (s *Service) replaceCatalog(authorization ingestAuthorization, catalog Catalog) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.configured || s.reconfigureSeq != authorization.generation || authorization.ticket != s.latestIngestTicket || authorization.ticket <= s.committedIngestTicket {
		return false
	}
	s.last = catalog
	s.committedIngestTicket = authorization.ticket
	return true
}

func (s *Service) Last() Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneCatalog(s.last)
}

func (s *Service) Effective() Catalog {
	catalog := s.Last()
	models := make([]ModelRow, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		if model.MaxInput <= 0 {
			model.MaxInput = model.Context
		}
		if model.MaxTokens > 0 {
			model.MaxSource = "upstream"
		} else {
			model.MaxTokens = model.Context
			model.MaxSource = "fallback-context"
		}
		models = append(models, model)
	}
	catalog.Models = models
	return catalog
}

func cloneCatalog(catalog Catalog) Catalog {
	if catalog.Models != nil {
		models := make([]ModelRow, len(catalog.Models))
		copy(models, catalog.Models)
		catalog.Models = models
	}
	for index := range catalog.Models {
		catalog.Models[index].Levels = append([]string(nil), catalog.Models[index].Levels...)
		catalog.Models[index].Input = append([]string(nil), catalog.Models[index].Input...)
		catalog.Models[index].Output = append([]string(nil), catalog.Models[index].Output...)
	}
	return catalog
}

func (s *Service) workerStatus(token string) (WorkerStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorizedLocked(token) {
		return WorkerStatus{}, false
	}
	return WorkerStatus{InstanceID: s.instanceID, ReconfigureSeq: s.reconfigureSeq, ConfigSHA256: s.cfg.SHA256}, true
}

func (s *Service) Shutdown() {
	s.mu.Lock()
	s.cfg = runtimeConfig{}
	s.configured = false
	s.latestIngestTicket++
	s.mu.Unlock()
}

type WorkerStatus struct {
	InstanceID     string `json:"instance_id"`
	ReconfigureSeq uint64 `json:"reconfigure_seq"`
	ConfigSHA256   string `json:"config_sha256"`
}

func opaqueID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}
