package plugin

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// compatProviderKey is the registry provider id this plugin registers under.
// The host only consults a model_provider plugin for an auth when the plugin's
// declared provider id equals auth.Provider, which for openai-compatibility
// auths is always "openai-compatibility".
const compatProviderKey = "openai-compatibility"

// StaticModels answers model.static: it declares the plugin's provider id and
// registers the managed models. An empty response leaves the host's native
// registration untouched (feature off for this boot).
func (s *Service) StaticModels() pluginapi.ModelResponse {
	s.ensureEnriched("")
	models := s.enrichedModels("")
	if len(models) == 0 {
		return pluginapi.ModelResponse{}
	}
	return pluginapi.ModelResponse{Provider: compatProviderKey, Models: models}
}

// ModelsForAuth answers model.for_auth for one openai-compatibility auth.
// Compat providers we do not manage decline by returning a Provider different
// from the auth's provider key: the host then skips us and native
// registration proceeds unchanged. If a managed provider has no snapshot
// (first sync failed), we decline as well rather than registering nothing.
func (s *Service) ModelsForAuth(req pluginapi.AuthModelRequest) pluginapi.ModelResponse {
	name := strings.TrimSpace(req.Attributes["compat_name"])
	if name == "" {
		name = strings.TrimSpace(req.Attributes["provider_key"])
	}
	if !s.managesProvider(name) {
		return pluginapi.ModelResponse{Provider: "unmanaged:" + name}
	}
	s.ensureEnriched(name)
	models := s.enrichedModels(name)
	if len(models) == 0 {
		return pluginapi.ModelResponse{Provider: "unmanaged:" + name}
	}
	return pluginapi.ModelResponse{Provider: compatProviderKey, Models: models}
}

// managesProvider reports whether name is an enabled provider in our config.
func (s *Service) managesProvider(name string) bool {
	if name == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, spec := range s.cfg.Providers {
		if spec.Enabled && strings.EqualFold(spec.Name, name) {
			return true
		}
	}
	return false
}

// ensureEnriched runs a bounded on-demand sync when the snapshot is missing.
// name == "" means "any provider". ponytail: no dedup of concurrent syncs;
// duplicate fetches are harmless and rare (startup only).
func (s *Service) ensureEnriched(name string) {
	if name != "" {
		s.mu.Lock()
		_, ok := s.lookupEnrichedLocked(name)
		s.mu.Unlock()
		if ok {
			return
		}
	} else {
		s.mu.Lock()
		any := len(s.enriched) > 0
		s.mu.Unlock()
		if any {
			return
		}
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	key := resolveManagementKey(cfg)
	if key == "" {
		return
	}
	s.SyncWithKey(key, name)
}

// enrichedModels builds pluginapi.ModelInfo entries from the snapshot.
// name == "" returns the union across all managed providers.
func (s *Service) enrichedModels(name string) []pluginapi.ModelInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pluginapi.ModelInfo
	appendProvider := func(models []enrichedModel) {
		for _, m := range models {
			out = append(out, toModelInfo(m))
		}
	}
	if name == "" {
		// Deterministic order across restarts.
		names := make([]string, 0, len(s.enriched))
		for provider := range s.enriched {
			names = append(names, provider)
		}
		sort.Strings(names)
		for _, provider := range names {
			appendProvider(s.enriched[provider])
		}
		return out
	}
	if models, ok := s.lookupEnrichedLocked(name); ok {
		appendProvider(models)
	}
	return out
}

// lookupEnrichedLocked resolves a snapshot by provider name, case-insensitive.
func (s *Service) lookupEnrichedLocked(name string) ([]enrichedModel, bool) {
	if models, ok := s.enriched[name]; ok {
		return models, true
	}
	for provider, models := range s.enriched {
		if strings.EqualFold(provider, name) {
			return models, true
		}
	}
	return nil, false
}

func toModelInfo(m enrichedModel) pluginapi.ModelInfo {
	display := m.DisplayName
	if display == "" {
		display = m.ID
	}
	info := pluginapi.ModelInfo{
		ID:                         m.ID,
		Object:                     "model",
		OwnedBy:                    compatProviderKey,
		DisplayName:                display,
		Name:                       m.Name,
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   m.Input,
		SupportedOutputModalities:  m.Output,
		ContextLength:              int64(m.Context),
		MaxCompletionTokens:        int64(m.MaxOutput),
		UserDefined:                true,
	}
	if len(m.Thinking) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: m.Thinking}
	}
	return info
}
