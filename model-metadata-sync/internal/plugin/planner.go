package plugin

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxPlanRequestBytes  = 40 << 20
	maxSnapshotBytes     = 32 << 20
	maxContinuationBytes = 8 << 20
	maxCatalogBytes      = 8 << 20
	maxClaudePages       = 100
)

const (
	errorCredential = "provider_credential_unavailable"
	errorInvalid    = "provider_fetch_invalid"
	errorTooLarge   = "provider_catalog_too_large"
)

type fetchSelector struct {
	ChannelName string `json:"channel_name,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	ConfigIndex *int   `json:"config_index,omitempty"`
}

type fetchDescriptor struct {
	RequestID          string            `json:"request_id"`
	Kind               string            `json:"kind"`
	Selector           fetchSelector     `json:"selector"`
	AuthIndex          string            `json:"auth_index,omitempty"`
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	Header             map[string]string `json:"header,omitempty"`
	ContinuationBase64 string            `json:"continuation_base64"`
}

type fetchResult struct {
	RequestID  string `json:"request_id"`
	StatusCode int    `json:"status_code"`
	BodyBase64 string `json:"body_base64"`
}

type plannerRequestWire struct {
	Version            json.RawMessage `json:"version"`
	ConfigBase64       json.RawMessage `json:"config_base64"`
	ContinuationBase64 json.RawMessage `json:"continuation_base64"`
	FetchResult        json.RawMessage `json:"fetch_result"`
}

type plannerRequest struct {
	Version      string
	Snapshot     []byte
	Continuation []byte
	FetchResult  *fetchResult
}

type fetchStep struct {
	Kind         string `json:"kind"`
	ChannelIndex int    `json:"channel_index"`
	SourceIndex  int    `json:"source_index"`
	Page         int    `json:"page"`
	AfterID      string `json:"after_id"`
	RequestID    string `json:"request_id"`
	AuthIndex    string `json:"auth_index"`
}

type boundAuth struct {
	ChannelIndex   int    `json:"channel_index"`
	AuthIndex      string `json:"auth_index"`
	IdentitySHA256 string `json:"identity_sha256"`
}

type plannedChannel struct {
	ChannelIndex int                   `json:"channel_index"`
	Spec         compiledChannel       `json:"spec"`
	Patches      []plannedModelPatch   `json:"patches"`
	Metadata     []ModelMetadataResult `json:"metadata"`
}

type plannedModelPatch struct {
	Model  string                       `json:"model"`
	Fields map[string]plannedFieldValue `json:"fields"`
}

type plannedFieldValue struct {
	Integer *int     `json:"integer,omitempty"`
	Strings []string `json:"strings,omitempty"`
}

type continuationState struct {
	Version        string                   `json:"version"`
	SnapshotSHA256 string                   `json:"snapshot_sha256"`
	ConfigSHA256   string                   `json:"config_sha256"`
	Generation     uint64                   `json:"generation"`
	AttemptID      string                   `json:"attempt_id"`
	Step           fetchStep                `json:"step"`
	ProviderBytes  int                      `json:"provider_bytes"`
	UpstreamByID   map[string]upstreamEntry `json:"upstream_by_id"`
	Modelparams    *modelparamsCatalog      `json:"modelparams,omitempty"`
	Modelsdev      *modelsdevCatalog        `json:"modelsdev,omitempty"`
	BoundAuth      []boundAuth              `json:"bound_auth"`
	ClaudeCursors  map[string]bool          `json:"claude_cursors"`
	Results        []plannedChannel         `json:"results"`
}

type plannerReport struct {
	Changed  bool             `json:"changed"`
	Channels []plannedChannel `json:"channels,omitempty"`
}

type finalEnvelope struct {
	BaseVersion  string        `json:"base_version"`
	ConfigBase64 string        `json:"config_base64"`
	Report       plannerReport `json:"report"`
}

type fetchEnvelope struct {
	BaseVersion string          `json:"base_version"`
	NextFetch   fetchDescriptor `json:"next_fetch"`
}

func plan(raw []byte, cfg runtimeConfig, host AuthHost) (any, string) {
	request, err := decodePlannerRequest(raw)
	if err != nil {
		return nil, errorInvalid
	}
	return planDecoded(request, cfg, host)
}

func planDecoded(request plannerRequest, cfg runtimeConfig, host AuthHost) (any, string) {
	document, err := parseSnapshot(request.Snapshot)
	if err != nil {
		return nil, errorInvalid
	}
	snapshotSHA := sha256Hex(request.Snapshot)
	if request.FetchResult == nil {
		if len(request.Continuation) != 0 {
			return nil, errorInvalid
		}
		state := continuationState{Version: request.Version, SnapshotSHA256: snapshotSHA, ConfigSHA256: cfg.SHA256, Generation: cfg.Generation, AttemptID: cfg.AttemptID, UpstreamByID: map[string]upstreamEntry{}, BoundAuth: []boundAuth{}, ClaudeCursors: map[string]bool{}, Results: []plannedChannel{}}
		step, ok := nextStep(cfg, &state, document)
		if !ok {
			return finishPlan(request.Version, document, cfg, nil)
		}
		state.Step = step
		return nextFetch(request.Version, document, cfg, host, state)
	}
	var state continuationState
	if len(request.Continuation) == 0 || decodeStrictJSONBytes(request.Continuation, &state) != nil || !validContinuationState(&state, request.Version, snapshotSHA, cfg) || request.FetchResult.RequestID != state.Step.RequestID || request.FetchResult.StatusCode < 200 || request.FetchResult.StatusCode >= 300 {
		return nil, errorInvalid
	}
	body, err := base64.StdEncoding.Strict().DecodeString(request.FetchResult.BodyBase64)
	if err != nil {
		return nil, errorInvalid
	}
	if len(body) > maxCatalogBytes || state.ProviderBytes > maxCatalogBytes-len(body) {
		return nil, errorTooLarge
	}
	if !utf8.Valid(body) {
		return nil, errorInvalid
	}
	state.ProviderBytes += len(body)
	if !revalidateBoundAuth(cfg, host, state.BoundAuth) {
		return nil, errorCredential
	}
	if code := consumeFetch(&state, body); code != "" {
		return nil, code
	}
	body = nil
	step, ok := nextStep(cfg, &state, document)
	if ok {
		state.Step = step
		return nextFetch(request.Version, document, cfg, host, state)
	}
	if !revalidateBoundAuth(cfg, host, state.BoundAuth) {
		return nil, errorCredential
	}
	return finishPlan(request.Version, document, cfg, state.Results)
}

func consumeFetch(state *continuationState, body []byte) string {
	switch state.Step.Kind {
	case "openai_models":
		entries, err := parseOpenAICatalog(body)
		if err != nil {
			return errorInvalid
		}
		state.UpstreamByID = entriesByID(entries)
	case "claude_models":
		if state.ClaudeCursors == nil {
			state.ClaudeCursors = map[string]bool{}
		}
		entries, lastID, hasMore, err := parseClaudeCatalog(body)
		if err != nil {
			return errorInvalid
		}
		for _, entry := range entries {
			if _, exists := state.UpstreamByID[entry.ID]; exists {
				return errorInvalid
			}
			state.UpstreamByID[entry.ID] = entry
		}
		if hasMore {
			if strings.TrimSpace(lastID) == "" || lastID == state.Step.AfterID || state.ClaudeCursors[lastID] || state.Step.Page+1 >= maxClaudePages {
				return errorInvalid
			}
			state.ClaudeCursors[lastID] = true
			state.Step.Page++
			state.Step.AfterID = lastID
			state.Step.RequestID = ""
			return ""
		}
		state.Step.AfterID = ""
		state.ClaudeCursors = map[string]bool{}
	case "modelparams":
		catalog, err := parseModelparamsCatalog(body)
		if err != nil {
			return errorInvalid
		}
		state.Modelparams = catalog
	case "modelsdev":
		catalog, err := parseModelsdevCatalog(body)
		if err != nil {
			return errorInvalid
		}
		state.Modelsdev = catalog
	default:
		return errorInvalid
	}
	state.Step.RequestID = ""
	return ""
}

func nextStep(cfg runtimeConfig, state *continuationState, document *yaml.Node) (fetchStep, bool) {

	if state.Step.RequestID == "" && state.Step.Kind == "claude_models" && state.Step.AfterID != "" {
		return state.Step, true
	}
	startChannel := 0
	if state.Step.Kind != "" {
		startChannel = state.Step.ChannelIndex
	}
	for channelIndex := startChannel; channelIndex < len(cfg.Channels); channelIndex++ {
		spec := cfg.Channels[channelIndex]
		if !spec.Enabled || resultExists(state.Results, channelIndex) {
			continue
		}
		if state.Step.Kind == "" || state.Step.ChannelIndex != channelIndex {
			if _, err := locateSnapshotChannel(document, spec); err != nil {
				return fetchStep{Kind: "invalid", ChannelIndex: channelIndex}, true
			}
			if spec.UpstreamMeta || hasModelsdevSource(spec) {
				return fetchStep{Kind: catalogKind(spec), ChannelIndex: channelIndex}, true
			}
		}
		for sourceIndex := 0; sourceIndex < len(spec.MetadataSources); sourceIndex++ {
			source := spec.MetadataSources[sourceIndex]
			if source.Website == "modelparams.dev" && state.Modelparams == nil {
				return fetchStep{Kind: "modelparams", ChannelIndex: channelIndex, SourceIndex: sourceIndex}, true
			}
			if source.Website == "models.dev" && state.Modelsdev == nil {
				return fetchStep{Kind: "modelsdev", ChannelIndex: channelIndex, SourceIndex: sourceIndex}, true
			}
		}
		channel, err := locateSnapshotChannel(document, spec)
		if err != nil {
			return fetchStep{Kind: "invalid", ChannelIndex: channelIndex}, true
		}
		before, err := snapshotModels(channel)
		if err != nil {
			return fetchStep{Kind: "invalid", ChannelIndex: channelIndex}, true
		}
		after := cloneModels(before)
		reports, _, _ := enrichModels(after, state.UpstreamByID, spec, state.Modelparams, state.Modelsdev)
		patches, err := plannedPatches(before, after, reports)
		if err != nil {
			return fetchStep{Kind: "invalid", ChannelIndex: channelIndex}, true
		}
		state.Results = append(state.Results, plannedChannel{ChannelIndex: channelIndex, Spec: spec, Patches: patches, Metadata: reports})
		state.UpstreamByID = map[string]upstreamEntry{}
		state.Step = fetchStep{}
	}
	return fetchStep{}, false
}

func nextFetch(version string, document *yaml.Node, cfg runtimeConfig, host AuthHost, state continuationState) (any, string) {
	if !revalidateBoundAuth(cfg, host, state.BoundAuth) {
		return nil, errorCredential
	}
	step := state.Step
	if step.Kind == "invalid" || step.ChannelIndex < 0 || step.ChannelIndex >= len(cfg.Channels) {
		return nil, errorInvalid
	}
	spec := cfg.Channels[step.ChannelIndex]
	requestID, err := opaqueID()
	if err != nil {
		return nil, errorInvalid
	}
	step.RequestID = requestID
	state.Step = step
	descriptor := fetchDescriptor{RequestID: requestID, Kind: step.Kind, Method: http.MethodGet, Selector: fetchSelector{}}
	switch step.Kind {
	case "openai_models":
		channel, err := locateSnapshotChannel(document, spec)
		if err != nil {
			return nil, errorInvalid
		}
		identity, err := resolveAuth(host, spec.Kind, spec.Selector, "")
		if err != nil || !bindChannelAuth(&state, step.ChannelIndex, identity) {
			return nil, errorCredential
		}
		state.Step.AuthIndex = identity.AuthIndex
		descriptor.AuthIndex = identity.AuthIndex
		descriptor.Selector = fetchSelector{ChannelName: spec.Selector.Name, BaseURL: spec.Selector.BaseURL}
		descriptor.URL = spec.Selector.BaseURL + "/models"
		if spec.CodexManifest {
			descriptor.URL += "?client_version=1.0.0"
		}
		descriptor.Header = cloneStringMap(channel.Headers)
		if descriptor.Header["Accept"] == "" {
			descriptor.Header["Accept"] = "application/json"
		}
		descriptor.Header["Authorization"] = "Bearer $TOKEN$"
	case "claude_models":
		channel, err := locateSnapshotChannel(document, spec)
		if err != nil {
			return nil, errorInvalid
		}
		identity, err := resolveAuth(host, spec.Kind, spec.Selector, "")
		if err != nil || !bindChannelAuth(&state, step.ChannelIndex, identity) {
			return nil, errorCredential
		}
		state.Step.AuthIndex = identity.AuthIndex
		descriptor.AuthIndex = identity.AuthIndex
		descriptor.Selector = fetchSelector{BaseURL: spec.Selector.BaseURL, Prefix: spec.Selector.Prefix, ConfigIndex: spec.Selector.ConfigIndex}
		query := url.Values{"limit": {"1000"}}
		if step.AfterID != "" {
			query.Set("after_id", step.AfterID)
		}
		descriptor.URL = spec.Selector.BaseURL + "/v1/models?" + query.Encode()
		descriptor.Header = cloneStringMap(channel.Headers)
		if strings.EqualFold(mustParseHost(spec.Selector.BaseURL), "api.anthropic.com") {
			descriptor.Header["x-api-key"] = "$TOKEN$"
			descriptor.Header["anthropic-version"] = "2023-06-01"
		} else if _, xAPI := descriptor.Header["x-api-key"]; xAPI {
			descriptor.Header["x-api-key"] = "$TOKEN$"
		} else {
			descriptor.Header["Authorization"] = "Bearer $TOKEN$"
		}
	case "modelparams":
		descriptor.URL = defaultModelparamsURL
		descriptor.Header = map[string]string{"Accept": "application/json"}
	case "modelsdev":
		descriptor.URL = defaultModelsdevURL
		descriptor.Header = map[string]string{"Accept": "application/json"}
	default:
		return nil, errorInvalid
	}
	stateRaw, err := json.Marshal(state)
	if err != nil || len(stateRaw) == 0 || len(stateRaw) > maxContinuationBytes {
		return nil, errorTooLarge
	}
	descriptor.ContinuationBase64 = base64.StdEncoding.EncodeToString(stateRaw)
	return fetchEnvelope{BaseVersion: version, NextFetch: descriptor}, ""
}

func finishPlan(version string, document *yaml.Node, cfg runtimeConfig, results []plannedChannel) (any, string) {
	changed, err := applyMetadata(document, cfg, results)
	if err != nil {
		return nil, errorInvalid
	}
	proposed, err := encodeSnapshot(document)
	if err != nil || len(proposed) == 0 || len(proposed) > maxSnapshotBytes {
		return nil, errorInvalid
	}
	return finalEnvelope{BaseVersion: version, ConfigBase64: base64.StdEncoding.EncodeToString(proposed), Report: plannerReport{Changed: changed, Channels: results}}, ""
}

func plannedPatches(before, after []ModelRef, reports []ModelMetadataResult) ([]plannedModelPatch, error) {
	beforeByName := make(map[string]ModelRef, len(before))
	for _, model := range before {
		if _, exists := beforeByName[model.Name]; exists {
			return nil, fmt.Errorf("ambiguous model")
		}
		beforeByName[model.Name] = model
	}
	reportByName := make(map[string]ModelMetadataResult, len(reports))
	for _, report := range reports {
		reportByName[report.Model] = report
	}
	patches := make([]plannedModelPatch, 0)
	for _, model := range after {
		old, exists := beforeByName[model.Name]
		if !exists {
			continue
		}
		status := map[string]string{}
		for _, field := range reportByName[model.Name].Fields {
			status[field.Field] = field.Status
		}
		fields := map[string]plannedFieldValue{}
		var valueErr error
		add := func(name string, oldValue, newValue any) {
			if valueErr != nil || reflect.DeepEqual(oldValue, newValue) {
				return
			}
			switch status[name] {
			case "upstream", "authoritative", "override", "completed":
				fields[name], valueErr = newPlannedFieldValue(newValue)
			}
		}
		oldThinking, newThinking := []string(nil), []string(nil)
		if old.Thinking != nil {
			oldThinking = old.Thinking.Levels
		}
		if model.Thinking != nil {
			newThinking = model.Thinking.Levels
		}
		add("thinking.levels", oldThinking, newThinking)
		add("max-context-length", old.MaxContextLength, model.MaxContextLength)
		add("max-input-tokens", old.MaxInputTokens, model.MaxInputTokens)
		add("max-output-tokens", old.MaxOutputTokens, model.MaxOutputTokens)
		add("input-modalities", old.InputModalities, model.InputModalities)
		add("output-modalities", old.OutputModalities, model.OutputModalities)
		if valueErr != nil {
			return nil, valueErr
		}
		if len(fields) > 0 {
			patches = append(patches, plannedModelPatch{Model: model.Name, Fields: fields})
		}
	}
	return patches, nil
}

func newPlannedFieldValue(value any) (plannedFieldValue, error) {
	switch typed := value.(type) {
	case int:
		if typed <= 0 {
			return plannedFieldValue{}, fmt.Errorf("invalid metadata value")
		}
		integer := typed
		return plannedFieldValue{Integer: &integer}, nil
	case []string:
		if len(typed) == 0 {
			return plannedFieldValue{}, fmt.Errorf("invalid metadata value")
		}
		return plannedFieldValue{Strings: append([]string(nil), typed...)}, nil
	default:
		return plannedFieldValue{}, fmt.Errorf("invalid metadata value")
	}
}

func authIdentitySHA256(identity authIdentity) string {
	raw, _ := json.Marshal([]string{identity.AuthIndex, identity.Path, strings.ToLower(identity.Provider), identity.BaseURL, identity.Prefix})
	return sha256Hex(raw)
}

func bindChannelAuth(state *continuationState, channelIndex int, identity authIdentity) bool {
	binding := boundAuth{ChannelIndex: channelIndex, AuthIndex: identity.AuthIndex, IdentitySHA256: authIdentitySHA256(identity)}
	for _, existing := range state.BoundAuth {
		if existing.ChannelIndex == channelIndex {
			return existing == binding
		}
		if existing.ChannelIndex > channelIndex {
			return false
		}
	}
	state.BoundAuth = append(state.BoundAuth, binding)
	return true
}

func revalidateBoundAuth(cfg runtimeConfig, host AuthHost, bindings []boundAuth) bool {
	for _, binding := range bindings {
		if binding.ChannelIndex < 0 || binding.ChannelIndex >= len(cfg.Channels) {
			return false
		}
		spec := cfg.Channels[binding.ChannelIndex]
		identity, err := resolveAuth(host, spec.Kind, spec.Selector, binding.AuthIndex)
		if err != nil || authIdentitySHA256(identity) != binding.IdentitySHA256 {
			return false
		}
	}
	return true
}

func credentialFetchRequired(spec compiledChannel) bool {
	return spec.UpstreamMeta || hasModelsdevSource(spec)
}

func validFetchStep(step fetchStep, cfg runtimeConfig) bool {
	if step.ChannelIndex < 0 || step.ChannelIndex >= len(cfg.Channels) || !cfg.Channels[step.ChannelIndex].Enabled || step.SourceIndex < 0 {
		return false
	}
	spec := cfg.Channels[step.ChannelIndex]
	switch step.Kind {
	case "openai_models":
		return spec.Kind == KindOpenAI && credentialFetchRequired(spec) && step.SourceIndex == 0
	case "claude_models":
		return spec.Kind == KindClaude && credentialFetchRequired(spec) && step.SourceIndex == 0
	case "modelparams":
		return step.SourceIndex < len(spec.MetadataSources) && spec.MetadataSources[step.SourceIndex].Website == "modelparams.dev"
	case "modelsdev":
		return step.SourceIndex < len(spec.MetadataSources) && spec.MetadataSources[step.SourceIndex].Website == "models.dev"
	default:
		return false
	}
}

func validUpstreamState(entries map[string]upstreamEntry) bool {
	if entries == nil {
		return false
	}
	for key, entry := range entries {
		if key == "" || key != entry.ID || key != strings.TrimSpace(key) || len(key) > 1024 || hasControl(key) || entry.Context < 0 || entry.MaxTokens < 0 || entry.ClaudeMaxInput < 0 || !reflect.DeepEqual(entry.Efforts, normalizeEfforts(entry.Efforts)) || !reflect.DeepEqual(entry.Input, cpaModalities(entry.Input)) || !reflect.DeepEqual(entry.Output, cpaModalities(entry.Output)) {
			return false
		}
	}
	return true
}

func validContinuationState(state *continuationState, version, snapshotSHA string, cfg runtimeConfig) bool {
	if state == nil || state.Version != version || state.SnapshotSHA256 != snapshotSHA || state.ConfigSHA256 != cfg.SHA256 || state.Generation != cfg.Generation || state.AttemptID == "" || state.AttemptID != cfg.AttemptID || len(state.AttemptID) > 128 || hasControl(state.AttemptID) || state.Step.RequestID == "" || len(state.Step.RequestID) > 128 || hasControl(state.Step.RequestID) || !validFetchStep(state.Step, cfg) || state.ProviderBytes < 0 || state.ProviderBytes > maxCatalogBytes || len(state.Results) > len(cfg.Channels) || state.BoundAuth == nil || state.ClaudeCursors == nil || !validUpstreamState(state.UpstreamByID) {
		return false
	}
	credentialStep := state.Step.Kind == "openai_models" || state.Step.Kind == "claude_models"
	if credentialStep {
		if state.Step.AuthIndex == "" || state.Step.AuthIndex != strings.TrimSpace(state.Step.AuthIndex) || len(state.Step.AuthIndex) > 512 || hasControl(state.Step.AuthIndex) {
			return false
		}
	} else if state.Step.AuthIndex != "" {
		return false
	}
	if state.Step.Page < 0 || state.Step.Page >= maxClaudePages || len(state.Step.AfterID) > 256 || hasControl(state.Step.AfterID) {
		return false
	}
	if state.Step.Kind == "claude_models" {
		if len(state.ClaudeCursors) != state.Step.Page || state.Step.Page == 0 && state.Step.AfterID != "" || state.Step.Page > 0 && (state.Step.AfterID == "" || !state.ClaudeCursors[state.Step.AfterID]) {
			return false
		}
		for cursor, seen := range state.ClaudeCursors {
			if !seen || cursor == "" || cursor != strings.TrimSpace(cursor) || len(cursor) > 256 || hasControl(cursor) {
				return false
			}
		}
	} else if state.Step.Page != 0 || state.Step.AfterID != "" || len(state.ClaudeCursors) != 0 {
		return false
	}
	boundChannels := make(map[int]boundAuth, len(state.BoundAuth))
	previous := -1
	for _, binding := range state.BoundAuth {
		if binding.ChannelIndex < 0 || binding.ChannelIndex >= len(cfg.Channels) || binding.ChannelIndex <= previous || binding.ChannelIndex > state.Step.ChannelIndex || binding.AuthIndex == "" || binding.AuthIndex != strings.TrimSpace(binding.AuthIndex) || len(binding.AuthIndex) > 512 || hasControl(binding.AuthIndex) || !validVersion(binding.IdentitySHA256) || !cfg.Channels[binding.ChannelIndex].Enabled || !credentialFetchRequired(cfg.Channels[binding.ChannelIndex]) {
			return false
		}
		previous = binding.ChannelIndex
		boundChannels[binding.ChannelIndex] = binding
	}
	currentBinding, currentBound := boundChannels[state.Step.ChannelIndex]
	if (credentialFetchRequired(cfg.Channels[state.Step.ChannelIndex]) && !currentBound) || (credentialStep && currentBinding.AuthIndex != state.Step.AuthIndex) {
		return false
	}
	seenResults := make(map[int]bool)
	for _, result := range state.Results {
		if result.ChannelIndex < 0 || result.ChannelIndex >= state.Step.ChannelIndex || seenResults[result.ChannelIndex] || !reflect.DeepEqual(result.Spec, cfg.Channels[result.ChannelIndex]) || !validPlannedPatches(result.Patches) || credentialFetchRequired(result.Spec) && boundChannels[result.ChannelIndex].AuthIndex == "" {
			return false
		}
		seenResults[result.ChannelIndex] = true
	}
	if (state.Modelparams != nil && !validModelparamsCatalog(state.Modelparams)) || (state.Modelsdev != nil && !validModelsdevCatalog(state.Modelsdev)) {
		return false
	}
	return true
}

func validPlannedPatches(patches []plannedModelPatch) bool {
	seenModels := make(map[string]bool)
	for _, patch := range patches {
		if patch.Model == "" || patch.Model != strings.TrimSpace(patch.Model) || len(patch.Model) > 1024 || hasControl(patch.Model) || seenModels[patch.Model] || len(patch.Fields) == 0 || len(patch.Fields) > len(metadataFieldNames) {
			return false
		}
		seenModels[patch.Model] = true
		for field, value := range patch.Fields {
			switch field {
			case "max-context-length", "max-input-tokens", "max-output-tokens":
				if value.Integer == nil || *value.Integer <= 0 || len(value.Strings) != 0 {
					return false
				}
			case "thinking.levels":
				if value.Integer != nil || len(normalizeEfforts(value.Strings)) == 0 || !reflect.DeepEqual(value.Strings, normalizeEfforts(value.Strings)) {
					return false
				}
			case "input-modalities", "output-modalities":
				if value.Integer != nil || len(value.Strings) == 0 || !reflect.DeepEqual(value.Strings, cpaModalities(value.Strings)) {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

func resultExists(results []plannedChannel, channel int) bool {
	for _, result := range results {
		if result.ChannelIndex == channel {
			return true
		}
	}
	return false
}

func hasModelsdevSource(spec compiledChannel) bool {
	for _, source := range spec.MetadataSources {
		if source.Website == "models.dev" {
			return true
		}
	}
	return false
}

func catalogKind(spec compiledChannel) string {
	if spec.Kind == KindClaude {
		return "claude_models"
	}
	return "openai_models"
}

func entriesByID(entries []upstreamEntry) map[string]upstreamEntry {
	out := make(map[string]upstreamEntry, len(entries))
	for _, entry := range entries {
		out[entry.ID] = entry
	}
	return out
}

func cloneStringMap(input map[string]string) map[string]string {
	out := make(map[string]string, len(input)+2)
	for key, value := range input {
		out[key] = value
	}
	return out
}

func mustParseHost(raw string) string { parsed, _ := url.Parse(raw); return parsed.Hostname() }

func decodePlannerRequest(raw []byte) (plannerRequest, error) {
	if len(raw) == 0 || len(raw) > maxPlanRequestBytes {
		return plannerRequest{}, fmt.Errorf("invalid request")
	}
	var wire plannerRequestWire
	if err := decodeStrictJSONBytes(raw, &wire); err != nil {
		return plannerRequest{}, err
	}
	var version, configBase64 string
	if len(wire.Version) == 0 || len(wire.ConfigBase64) == 0 || decodeStrictJSONBytes(wire.Version, &version) != nil || decodeStrictJSONBytes(wire.ConfigBase64, &configBase64) != nil || !validVersion(version) {
		return plannerRequest{}, fmt.Errorf("invalid request")
	}
	snapshot, err := base64.StdEncoding.Strict().DecodeString(configBase64)
	if err != nil || len(snapshot) == 0 || len(snapshot) > maxSnapshotBytes {
		return plannerRequest{}, fmt.Errorf("invalid request")
	}
	hasContinuation, hasResult := len(wire.ContinuationBase64) != 0, len(wire.FetchResult) != 0
	if hasContinuation != hasResult {
		return plannerRequest{}, fmt.Errorf("invalid request")
	}
	request := plannerRequest{Version: version, Snapshot: snapshot}
	if !hasContinuation {
		return request, nil
	}
	if bytes.Equal(bytes.TrimSpace(wire.ContinuationBase64), []byte("null")) || bytes.Equal(bytes.TrimSpace(wire.FetchResult), []byte("null")) {
		return plannerRequest{}, fmt.Errorf("invalid request")
	}
	var continuationBase64 string
	var result fetchResult
	if decodeStrictJSONBytes(wire.ContinuationBase64, &continuationBase64) != nil || decodeStrictJSONBytes(wire.FetchResult, &result) != nil {
		return plannerRequest{}, fmt.Errorf("invalid request")
	}
	continuation, err := base64.StdEncoding.Strict().DecodeString(continuationBase64)
	if err != nil || len(continuation) == 0 || len(continuation) > maxContinuationBytes {
		return plannerRequest{}, fmt.Errorf("invalid request")
	}
	request.Continuation, request.FetchResult = continuation, &result
	return request, nil
}

func validVersion(version string) bool {
	if len(version) != 64 {
		return false
	}
	for _, value := range []byte(version) {
		if value < '0' || value > '9' {
			if value < 'a' || value > 'f' {
				return false
			}
		}
	}
	return true
}

func sha256Hex(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func opaqueID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func decodeStrictJSONBytes(raw []byte, target any) error {
	if !utf8.Valid(raw) || validateExactJSONKeys(raw, reflect.TypeOf(target)) != nil {
		return fmt.Errorf("invalid json")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid json")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid json")
	}
	return nil
}

func validateExactJSONKeys(raw []byte, targetType reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func(reflect.Type) error
	value = func(expected reflect.Type) error {
		for expected != nil && expected.Kind() == reflect.Pointer {
			expected = expected.Elem()
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			var fields map[string]reflect.Type
			if expected != nil && expected.Kind() == reflect.Struct {
				fields = exactJSONFieldTypes(expected)
			}
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[strings.ToUpper(key)] {
					return fmt.Errorf("duplicate json key")
				}
				seen[strings.ToUpper(key)] = true
				var child reflect.Type
				if fields != nil {
					var exists bool
					child, exists = fields[key]
					if !exists {
						return fmt.Errorf("unknown json key")
					}
				} else if expected != nil && expected.Kind() == reflect.Map {
					child = expected.Elem()
				}
				if err := value(child); err != nil {
					return err
				}
			}
		case '[':
			var child reflect.Type
			if expected != nil && (expected.Kind() == reflect.Array || expected.Kind() == reflect.Slice) {
				child = expected.Elem()
			}
			for decoder.More() {
				if err := value(child); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid json")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(targetType); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing json")
	}
	return nil
}

func exactJSONFieldTypes(target reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		if field.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}
