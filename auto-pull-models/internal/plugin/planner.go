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
)

const (
	errorCredential = "provider_credential_unavailable"
	errorInvalid    = "provider_fetch_invalid"
	errorTooLarge   = "provider_catalog_too_large"
)

type fetchSelector struct {
	ChannelName string `json:"channel_name"`
	BaseURL     string `json:"base_url"`
}

type fetchDescriptor struct {
	RequestID          string            `json:"request_id"`
	Kind               string            `json:"kind"`
	Selector           fetchSelector     `json:"selector"`
	AuthIndex          string            `json:"auth_index"`
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	Header             map[string]string `json:"header"`
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

type plannedChannel struct {
	ChannelIndex int             `json:"channel_index"`
	Selector     ChannelSelector `json:"selector"`
	Desired      []string        `json:"desired"`
}

type continuationState struct {
	Version        string           `json:"version"`
	SnapshotSHA256 string           `json:"snapshot_sha256"`
	ConfigSHA256   string           `json:"config_sha256"`
	Generation     uint64           `json:"generation"`
	AttemptID      string           `json:"attempt_id"`
	ChannelIndex   int              `json:"channel_index"`
	RequestID      string           `json:"request_id"`
	AuthIndex      string           `json:"auth_index"`
	Results        []plannedChannel `json:"results"`
}

type plannerReport struct {
	Changed bool `json:"changed"`
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
	snapshotSHA256 := sha256Hex(request.Snapshot)
	if request.FetchResult == nil {
		if len(request.Continuation) != 0 {
			return nil, errorInvalid
		}
		index := nextEnabledChannel(cfg.Channels, 0)
		if index < 0 {
			return finishPlan(request.Version, document, nil)
		}
		return nextFetch(request.Version, snapshotSHA256, document, cfg, host, index, nil)
	}
	var state continuationState
	if len(request.Continuation) == 0 || decodeStrictJSONBytes(request.Continuation, &state) != nil || !validContinuationState(state, request.Version, snapshotSHA256, cfg) || request.FetchResult.RequestID != state.RequestID || request.FetchResult.StatusCode < 200 || request.FetchResult.StatusCode >= 300 {
		return nil, errorInvalid
	}
	body, err := base64.StdEncoding.Strict().DecodeString(request.FetchResult.BodyBase64)
	if err != nil {
		return nil, errorInvalid
	}
	if len(body) > maxCatalogBytes {
		return nil, errorTooLarge
	}
	if !utf8.Valid(body) {
		return nil, errorInvalid
	}
	spec := cfg.Channels[state.ChannelIndex]
	if _, err := locateSnapshotChannel(document, spec.Selector); err != nil {
		return nil, errorInvalid
	}
	if _, err := resolveAuth(host, spec.Selector, state.AuthIndex); err != nil {
		return nil, errorCredential
	}
	entries, err := parseUpstreamCatalog(body)
	body = nil
	if err != nil {
		return nil, errorInvalid
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	desired := filterIDs(ids, spec)
	state.Results = append(state.Results, plannedChannel{ChannelIndex: state.ChannelIndex, Selector: spec.Selector, Desired: desired})
	next := nextEnabledChannel(cfg.Channels, state.ChannelIndex+1)
	if next >= 0 {
		return nextFetch(request.Version, snapshotSHA256, document, cfg, host, next, state.Results)
	}
	return finishPlan(request.Version, document, state.Results)
}

func nextFetch(version, snapshotSHA256 string, document *yaml.Node, cfg runtimeConfig, host AuthHost, channelIndex int, results []plannedChannel) (any, string) {
	spec := cfg.Channels[channelIndex]
	channel, err := locateSnapshotChannel(document, spec.Selector)
	if err != nil {
		return nil, errorInvalid
	}
	identity, err := resolveAuth(host, spec.Selector, "")
	if err != nil {
		return nil, errorCredential
	}
	requestID, err := opaqueID()
	if err != nil {
		return nil, errorInvalid
	}
	state := continuationState{Version: version, SnapshotSHA256: snapshotSHA256, ConfigSHA256: cfg.SHA256, Generation: cfg.Generation, AttemptID: cfg.AttemptID, ChannelIndex: channelIndex, RequestID: requestID, AuthIndex: identity.AuthIndex, Results: append([]plannedChannel{}, results...)}
	stateRaw, err := json.Marshal(state)
	if err != nil || len(stateRaw) == 0 || len(stateRaw) > maxContinuationBytes {
		return nil, errorTooLarge
	}
	headers := make(map[string]string, len(channel.Headers)+2)
	for name, value := range channel.Headers {
		headers[name] = value
	}
	if _, configured := headers["Accept"]; !configured {
		headers["Accept"] = "application/json"
	}
	headers["Authorization"] = "Bearer $TOKEN$"
	return fetchEnvelope{BaseVersion: version, NextFetch: fetchDescriptor{
		RequestID:          requestID,
		Kind:               "openai_models",
		Selector:           fetchSelector{ChannelName: spec.Selector.Name, BaseURL: spec.Selector.BaseURL},
		AuthIndex:          identity.AuthIndex,
		Method:             http.MethodGet,
		URL:                spec.Selector.BaseURL + "/models",
		Header:             headers,
		ContinuationBase64: base64.StdEncoding.EncodeToString(stateRaw),
	}}, ""
}

func finishPlan(version string, document *yaml.Node, results []plannedChannel) (any, string) {
	changed, err := applyMembership(document, results)
	if err != nil {
		return nil, errorInvalid
	}
	proposed, err := encodeSnapshot(document)
	if err != nil || len(proposed) == 0 || len(proposed) > maxSnapshotBytes {
		return nil, errorInvalid
	}
	return finalEnvelope{BaseVersion: version, ConfigBase64: base64.StdEncoding.EncodeToString(proposed), Report: plannerReport{Changed: changed}}, ""
}

func nextEnabledChannel(channels []compiledChannel, start int) int {
	for index := start; index < len(channels); index++ {
		if channels[index].Enabled {
			return index
		}
	}
	return -1
}

func validContinuationState(state continuationState, version, snapshotSHA256 string, cfg runtimeConfig) bool {
	if state.Version != version || state.SnapshotSHA256 != snapshotSHA256 || state.ConfigSHA256 != cfg.SHA256 || state.Generation != cfg.Generation || state.AttemptID == "" || state.AttemptID != cfg.AttemptID || len(state.AttemptID) > 128 || hasControl(state.AttemptID) || state.ChannelIndex < 0 || state.ChannelIndex >= len(cfg.Channels) || !cfg.Channels[state.ChannelIndex].Enabled || state.RequestID == "" || len(state.RequestID) > 128 || hasControl(state.RequestID) || state.AuthIndex == "" || state.AuthIndex != strings.TrimSpace(state.AuthIndex) || len(state.AuthIndex) > 512 || hasControl(state.AuthIndex) || state.Results == nil || len(state.Results) > len(cfg.Channels) {
		return false
	}
	seen := make(map[int]bool)
	last := -1
	for _, result := range state.Results {
		if result.ChannelIndex <= last || result.ChannelIndex >= state.ChannelIndex || result.ChannelIndex < 0 || seen[result.ChannelIndex] || !cfg.Channels[result.ChannelIndex].Enabled || result.Selector != cfg.Channels[result.ChannelIndex].Selector {
			return false
		}
		seen[result.ChannelIndex] = true
		last = result.ChannelIndex
		if len(result.Desired) > 100_000 {
			return false
		}
		names := make(map[string]bool, len(result.Desired))
		for _, name := range result.Desired {
			if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || len(name) > 1024 || hasControl(name) || names[name] {
				return false
			}
			names[name] = true
		}
	}
	for index := 0; index < state.ChannelIndex; index++ {
		if cfg.Channels[index].Enabled && !seen[index] {
			return false
		}
	}
	return true
}

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
	hasContinuation := len(wire.ContinuationBase64) != 0
	hasResult := len(wire.FetchResult) != 0
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

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

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
