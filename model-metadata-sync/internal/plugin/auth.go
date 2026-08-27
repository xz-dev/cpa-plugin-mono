package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
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
	Prefix    string
}

type physicalIdentity struct {
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	Prefix  string `json:"prefix"`
}

func resolveAuth(host AuthHost, kind string, selector ChannelSelector, requiredIndex string) (authIdentity, error) {
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
		if !usableAuthEntry(listed) || !validAuthText(listedProvider, 256) || !providerMatchesKind(listedProvider, kind) {
			continue
		}
		authIndex := strings.TrimSpace(listed.AuthIndex)
		runtime, runtimeErr := host.GetRuntime(authIndex)
		if runtimeErr != nil || !sameAuthEntry(listed, runtime) || !usableAuthEntry(runtime) || !strings.EqualFold(strings.TrimSpace(runtime.Provider), listedProvider) {
			return authIdentity{}, fmt.Errorf("provider credential unavailable")
		}
		physical, physicalErr := host.Get(authIndex)
		if physicalErr != nil || strings.TrimSpace(physical.AuthIndex) != authIndex || !validAuthText(strings.TrimSpace(physical.Path), 4096) || strings.TrimSpace(physical.Path) != strings.TrimSpace(listed.Path) {
			return authIdentity{}, fmt.Errorf("provider credential unavailable")
		}
		identity, identityErr := decodePhysicalIdentity(physical.JSON)
		physical.JSON = nil
		if identityErr != nil || !strings.EqualFold(strings.TrimSpace(identity.Type), listedProvider) {
			return authIdentity{}, fmt.Errorf("provider credential unavailable")
		}
		baseRaw := identity.BaseURL
		if kind == KindClaude && strings.TrimSpace(baseRaw) == "" {
			baseRaw = "https://api.anthropic.com"
		}
		base, baseErr := normalizeBaseURL(baseRaw)
		if baseErr != nil || base != selector.BaseURL {
			continue
		}
		prefix := strings.Trim(strings.TrimSpace(identity.Prefix), "/")
		if kind == KindClaude && prefix != selector.Prefix {
			continue
		}
		matches = append(matches, authIdentity{AuthIndex: authIndex, Path: strings.TrimSpace(listed.Path), Provider: listedProvider, BaseURL: base, Prefix: prefix})
	}
	if len(matches) != 1 || requiredIndex != "" && matches[0].AuthIndex != requiredIndex {
		return authIdentity{}, fmt.Errorf("provider credential unavailable")
	}
	return matches[0], nil
}

func providerMatchesKind(provider, kind string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if kind == KindClaude {
		return provider == "claude" || provider == "anthropic"
	}
	return provider == "openai-compatibility" || strings.HasPrefix(provider, "openai-compatible-") || strings.HasPrefix(provider, "openai-compatibility:")
}

func usableAuthEntry(entry AuthEntry) bool {
	return validAuthText(strings.TrimSpace(entry.AuthIndex), 512) && validAuthText(strings.TrimSpace(entry.Path), 4096) && strings.EqualFold(strings.TrimSpace(entry.Source), "file") && !entry.RuntimeOnly && !entry.Disabled && !entry.Unavailable && strings.EqualFold(strings.TrimSpace(entry.Status), "active")
}

func validAuthText(value string, maxLength int) bool {
	return value != "" && len(value) <= maxLength && utf8.ValidString(value) && !hasControl(value)
}

func sameAuthEntry(listed, runtime AuthEntry) bool {
	return strings.TrimSpace(listed.AuthIndex) == strings.TrimSpace(runtime.AuthIndex) && strings.TrimSpace(listed.Path) == strings.TrimSpace(runtime.Path)
}

func decodePhysicalIdentity(raw []byte) (physicalIdentity, error) {
	var fields map[string]json.RawMessage
	if decodeStrictJSONBytes(raw, &fields) != nil {
		return physicalIdentity{}, fmt.Errorf("invalid credential JSON")
	}
	if hasAnyFoldedCatalogField(fields, "type", "base_url", "prefix") {
		return physicalIdentity{}, fmt.Errorf("invalid credential identity")
	}
	var identity physicalIdentity
	if decodeStrictJSONBytes(fields["type"], &identity.Type) != nil || strings.TrimSpace(identity.Type) == "" {
		return physicalIdentity{}, fmt.Errorf("invalid credential identity")
	}
	if rawBase := fields["base_url"]; len(rawBase) != 0 && decodeStrictJSONBytes(rawBase, &identity.BaseURL) != nil {
		return physicalIdentity{}, fmt.Errorf("invalid credential identity")
	}
	if rawPrefix := fields["prefix"]; len(rawPrefix) != 0 && decodeStrictJSONBytes(rawPrefix, &identity.Prefix) != nil {
		return physicalIdentity{}, fmt.Errorf("invalid credential identity")
	}
	return identity, nil
}
