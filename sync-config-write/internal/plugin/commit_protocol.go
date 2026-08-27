package plugin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

var versionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ConfigSnapshot struct {
	Version      string `json:"version"`
	ConfigBase64 string `json:"config_base64"`
}

func NewConfigSnapshot(raw []byte) ConfigSnapshot {
	return ConfigSnapshot{Version: configVersion(raw), ConfigBase64: base64.StdEncoding.EncodeToString(raw)}
}

func (s ConfigSnapshot) Decode() ([]byte, error) {
	if !versionPattern.MatchString(s.Version) || s.ConfigBase64 == "" {
		return nil, fmt.Errorf("invalid snapshot")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s.ConfigBase64)
	if err != nil {
		return nil, fmt.Errorf("invalid snapshot")
	}
	if configVersion(raw) != s.Version {
		return nil, fmt.Errorf("invalid snapshot")
	}
	return raw, nil
}

type CommitProposal struct {
	BaseVersion  string           `json:"base_version"`
	ConfigBase64 string           `json:"config_base64,omitempty"`
	NextFetch    *FetchDescriptor `json:"next_fetch,omitempty"`
	Report       json.RawMessage  `json:"report,omitempty"`
}

func (p CommitProposal) Decode(expectedVersion string) ([]byte, error) {
	if !versionPattern.MatchString(expectedVersion) || p.BaseVersion != expectedVersion || !versionPattern.MatchString(p.BaseVersion) || p.ConfigBase64 == "" || p.NextFetch != nil {
		return nil, fmt.Errorf("invalid commit proposal")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(p.ConfigBase64)
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("invalid commit proposal")
	}
	return raw, nil
}

func configVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
