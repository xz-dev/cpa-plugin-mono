package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
)

type AuthEntry struct {
	AuthIndex   string
	Provider    string
	Status      string
	Disabled    bool
	Unavailable bool
	RuntimeOnly bool
	Source      string
	Path        string
}

type AuthPhysical struct {
	AuthIndex string
	Path      string
	JSON      []byte
}

type AuthHost interface {
	List() ([]AuthEntry, error)
	GetRuntime(authIndex string) (AuthEntry, error)
	Get(authIndex string) (AuthPhysical, error)
}

type authIdentity struct {
	AuthIndex string
	Path      string
	Provider  string
	BaseURL   string
}

type physicalIdentity struct {
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
}

func resolveAuth(host AuthHost, selector ChannelSelector, requiredIndex string) (authIdentity, error) {
	if host == nil {
		return authIdentity{}, fmt.Errorf("provider credential unavailable")
	}
	entries, err := host.List()
	if err != nil {
		return authIdentity{}, fmt.Errorf("provider credential unavailable")
	}
	matches := make([]authIdentity, 0, 1)
	for _, listed := range entries {
		listedProvider := strings.TrimSpace(listed.Provider)
		if !usableAuthEntry(listed) || !isOpenAICompatibleProvider(listedProvider) {
			continue
		}
		authIndex := strings.TrimSpace(listed.AuthIndex)
		runtime, runtimeErr := host.GetRuntime(authIndex)
		if runtimeErr != nil || !sameAuthEntry(listed, runtime) || !usableAuthEntry(runtime) || !strings.EqualFold(strings.TrimSpace(runtime.Provider), listedProvider) {
			return authIdentity{}, fmt.Errorf("provider credential unavailable")
		}
		physical, physicalErr := host.Get(authIndex)
		if physicalErr != nil || strings.TrimSpace(physical.AuthIndex) != authIndex || strings.TrimSpace(physical.Path) == "" || strings.TrimSpace(physical.Path) != strings.TrimSpace(listed.Path) {
			return authIdentity{}, fmt.Errorf("provider credential unavailable")
		}
		identity, identityErr := decodePhysicalIdentity(physical.JSON)
		physical.JSON = nil
		if identityErr != nil || !strings.EqualFold(strings.TrimSpace(identity.Type), listedProvider) {
			return authIdentity{}, fmt.Errorf("provider credential unavailable")
		}
		base, baseErr := normalizeBaseURL(identity.BaseURL)
		if baseErr != nil {
			return authIdentity{}, fmt.Errorf("provider credential unavailable")
		}
		if base != selector.BaseURL {
			continue
		}
		matches = append(matches, authIdentity{AuthIndex: authIndex, Path: strings.TrimSpace(listed.Path), Provider: listedProvider, BaseURL: base})
	}
	if len(matches) != 1 || requiredIndex != "" && matches[0].AuthIndex != requiredIndex {
		return authIdentity{}, fmt.Errorf("provider credential unavailable")
	}
	return matches[0], nil
}

func isOpenAICompatibleProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return provider == "openai-compatibility" || strings.HasPrefix(provider, "openai-compatible-") || strings.HasPrefix(provider, "openai-compatibility:")
}

func usableAuthEntry(entry AuthEntry) bool {
	return strings.TrimSpace(entry.AuthIndex) != "" && strings.TrimSpace(entry.Path) != "" && strings.EqualFold(strings.TrimSpace(entry.Source), "file") && !entry.RuntimeOnly && !entry.Disabled && !entry.Unavailable && strings.EqualFold(strings.TrimSpace(entry.Status), "active")
}

func sameAuthEntry(listed, runtime AuthEntry) bool {
	return strings.TrimSpace(listed.AuthIndex) == strings.TrimSpace(runtime.AuthIndex) && strings.TrimSpace(listed.Path) == strings.TrimSpace(runtime.Path)
}

func decodePhysicalIdentity(raw []byte) (physicalIdentity, error) {
	var fields map[string]json.RawMessage
	if decodeStrictJSONBytes(raw, &fields) != nil {
		return physicalIdentity{}, fmt.Errorf("invalid credential JSON")
	}
	var identity physicalIdentity
	if decodeStrictJSONBytes(fields["type"], &identity.Type) != nil || decodeStrictJSONBytes(fields["base_url"], &identity.BaseURL) != nil || strings.TrimSpace(identity.Type) == "" || strings.TrimSpace(identity.BaseURL) == "" {
		return physicalIdentity{}, fmt.Errorf("invalid credential identity")
	}
	return identity, nil
}
