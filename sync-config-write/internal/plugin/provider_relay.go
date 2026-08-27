package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	apiCallPath             = "/v0/management/api-call"
	maxProviderPages        = 100
	maxProviderPayloadBytes = 8 << 20
	maxContinuationBytes    = 8 << 20
	maxPlannerEnvelopeBytes = 16 << 20
	maxAPICallEnvelopeBytes = 64 << 20
)

var safeProviderHeaders = map[string]bool{
	"accept":            true,
	"anthropic-beta":    true,
	"anthropic-version": true,
	"authorization":     true,
	"content-type":      true,
	"user-agent":        true,
	"x-api-key":         true,
}

type FetchSelector struct {
	ChannelName string `json:"channel_name,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	ConfigIndex *int   `json:"config_index,omitempty"`
}

type FetchDescriptor struct {
	RequestID          string            `json:"request_id"`
	Kind               string            `json:"kind"`
	Selector           *FetchSelector    `json:"selector"`
	AuthIndex          string            `json:"auth_index,omitempty"`
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	Header             map[string]string `json:"header,omitempty"`
	ContinuationBase64 string            `json:"continuation_base64"`
}

type FetchResult struct {
	RequestID  string `json:"request_id"`
	StatusCode int    `json:"status_code"`
	BodyBase64 string `json:"body_base64"`
}

type plannerRequest struct {
	Version            string       `json:"version"`
	ConfigBase64       string       `json:"config_base64"`
	ContinuationBase64 string       `json:"continuation_base64,omitempty"`
	FetchResult        *FetchResult `json:"fetch_result,omitempty"`
}

type apiCallRequest struct {
	AuthIndex string            `json:"auth_index,omitempty"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header,omitempty"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

type fetchRelay struct{ client *http.Client }

func (r fetchRelay) fetch(ctx context.Context, descriptor FetchDescriptor, settings Settings) (FetchResult, ErrorCode) {
	body, err := json.Marshal(apiCallRequest{
		AuthIndex: descriptor.AuthIndex,
		Method:    http.MethodGet,
		URL:       descriptor.URL,
		Header:    descriptor.Header,
	})
	if err != nil {
		return FetchResult{}, CodeProviderFetchInvalid
	}
	req, err := NewCoreRequest(ctx, settings, http.MethodPost, apiCallPath, body)
	if err != nil {
		return FetchResult{}, CodeProviderFetchFailed
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(req)
	if err != nil {
		if isTimeout(err, ctx) {
			return FetchResult{}, CodeLoopbackTimeout
		}
		return FetchResult{}, CodeProviderFetchFailed
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return FetchResult{}, CodeProviderFetchFailed
	}
	var result apiCallResponse
	if err := decodeStrictJSON(response.Body, maxAPICallEnvelopeBytes, &result); err != nil {
		return FetchResult{}, CodeProviderFetchFailed
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 || !utf8.ValidString(result.Body) {
		return FetchResult{}, CodeProviderFetchFailed
	}
	bodyBytes := []byte(result.Body)
	if len(bodyBytes) > maxProviderPayloadBytes {
		return FetchResult{}, CodeProviderCatalogTooLarge
	}
	return FetchResult{
		RequestID:  descriptor.RequestID,
		StatusCode: result.StatusCode,
		BodyBase64: base64.StdEncoding.EncodeToString(bodyBytes),
	}, ""
}

func validateFetchDescriptor(descriptor FetchDescriptor, snapshot ConfigSnapshot) error {
	if descriptor.Method != http.MethodGet || descriptor.RequestID == "" || len(descriptor.RequestID) > 128 || strings.TrimSpace(descriptor.RequestID) != descriptor.RequestID || hasControl(descriptor.RequestID) {
		return fmt.Errorf("invalid request")
	}
	continuation, err := base64.StdEncoding.Strict().DecodeString(descriptor.ContinuationBase64)
	if err != nil || len(continuation) == 0 || len(continuation) > maxContinuationBytes {
		return fmt.Errorf("invalid continuation")
	}
	if len(descriptor.AuthIndex) > 512 || strings.TrimSpace(descriptor.AuthIndex) != descriptor.AuthIndex || hasControl(descriptor.AuthIndex) {
		return fmt.Errorf("invalid auth index")
	}
	u, err := url.Parse(descriptor.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || len(descriptor.URL) > 4096 {
		return fmt.Errorf("invalid url")
	}
	credentialHeaders := 0
	totalHeaderBytes := 0
	seenHeaderNames := make(map[string]bool)
	if len(descriptor.Header) > 16 {
		return fmt.Errorf("too many headers")
	}
	for name, value := range descriptor.Header {
		lower := strings.ToLower(name)
		totalHeaderBytes += len(name) + len(value)
		if name == "" || len(name) > 64 || len(value) > 4096 || totalHeaderBytes > 16<<10 || hasControl(name) || hasControl(value) || !safeProviderHeaders[lower] || seenHeaderNames[lower] {
			return fmt.Errorf("unsafe header")
		}
		seenHeaderNames[lower] = true
		switch lower {
		case "authorization":
			credentialHeaders++
			if value != "Bearer $TOKEN$" {
				return fmt.Errorf("invalid credential header")
			}
		case "x-api-key":
			credentialHeaders++
			if value != "$TOKEN$" {
				return fmt.Errorf("invalid credential header")
			}
		default:
			if strings.Contains(value, "$TOKEN$") {
				return fmt.Errorf("invalid token placeholder")
			}
		}
	}
	credentialKind := descriptor.Kind == "openai_models" || descriptor.Kind == "claude_models"
	if credentialKind != (descriptor.AuthIndex != "" && credentialHeaders == 1) {
		return fmt.Errorf("invalid credential binding")
	}
	if !credentialKind && credentialHeaders != 0 {
		return fmt.Errorf("unexpected credential header")
	}
	raw, err := snapshot.Decode()
	if err != nil {
		return fmt.Errorf("invalid snapshot")
	}
	if descriptor.Selector == nil {
		return fmt.Errorf("missing selector")
	}
	switch descriptor.Kind {
	case "openai_models":
		return validateOpenAIFetch(descriptor, u, raw)
	case "claude_models":
		return validateClaudeFetch(descriptor, u, raw)
	case "modelparams":
		if !emptySelector(*descriptor.Selector) || descriptor.AuthIndex != "" || descriptor.URL != "https://modelparams.dev/api/v1/models.json" {
			return fmt.Errorf("invalid modelparams request")
		}
		return validateCatalogHeaders(descriptor, map[string]string{}, "", nil)
	case "modelsdev":
		if !emptySelector(*descriptor.Selector) || descriptor.AuthIndex != "" || descriptor.URL != "https://models.dev/api.json" {
			return fmt.Errorf("invalid models.dev request")
		}
		return validateCatalogHeaders(descriptor, map[string]string{}, "", nil)
	default:
		return fmt.Errorf("unknown fetch kind")
	}
}

func validateOpenAIFetch(descriptor FetchDescriptor, u *url.URL, raw []byte) error {
	selector := *descriptor.Selector
	if selector.ChannelName == "" || selector.BaseURL == "" || selector.Prefix != "" || selector.ConfigIndex != nil {
		return fmt.Errorf("invalid selector")
	}
	base, err := normalizeHTTPSBaseURL(selector.BaseURL)
	if err != nil || selector.ChannelName != strings.TrimSpace(selector.ChannelName) || selector.BaseURL != base || descriptor.URL != base+"/models" || u.RawQuery != "" {
		return fmt.Errorf("invalid catalog url")
	}
	root, err := parseOwnedDocument(raw)
	if err != nil {
		return fmt.Errorf("invalid snapshot")
	}
	channels, err := uniqueMappingValue(root, "openai-compatibility")
	if err != nil || channels.Kind != yaml.SequenceNode {
		return fmt.Errorf("channel unavailable")
	}
	matches := 0
	var selected *yaml.Node
	for _, channel := range channels.Content {
		name, nameOK := scalarMappingString(channel, "name")
		baseURL, baseOK := scalarMappingString(channel, "base-url")
		disabled, disabledOK := scalarMappingBool(channel, "disabled")
		normalized, normalizeErr := normalizeHTTPSBaseURL(baseURL)
		if nameOK && baseOK && normalizeErr == nil && (!disabledOK || !disabled) && strings.TrimSpace(name) == selector.ChannelName && normalized == base {
			matches++
			selected = channel
		}
	}
	if matches != 1 {
		return fmt.Errorf("ambiguous channel")
	}
	if hasYAMLIndirection(selected, make(map[*yaml.Node]bool)) {
		return fmt.Errorf("indirect channel configuration")
	}
	configured, credentialForms, err := catalogHeaders(selected)
	if err != nil || credentialForms["x-api-key"] || credentialForms["authorization"] {
		return fmt.Errorf("invalid channel headers")
	}
	return validateCatalogHeaders(descriptor, configured, "authorization", nil)
}

func validateClaudeFetch(descriptor FetchDescriptor, u *url.URL, raw []byte) error {
	selector := *descriptor.Selector
	if selector.ChannelName != "" || selector.BaseURL == "" || selector.ConfigIndex == nil || *selector.ConfigIndex < 0 {
		return fmt.Errorf("invalid selector")
	}
	base, err := normalizeHTTPSBaseURL(selector.BaseURL)
	if err != nil || selector.BaseURL != base || selector.Prefix != normalizeSelectorPrefix(selector.Prefix) {
		return fmt.Errorf("invalid selector")
	}
	baseURL, _ := url.Parse(base)
	if !strings.EqualFold(baseURL.Host, u.Host) || u.Path != "/v1/models" {
		return fmt.Errorf("invalid catalog url")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("invalid catalog query")
	}
	if len(query) < 1 || len(query) > 2 || len(query["limit"]) != 1 || query.Get("limit") != "1000" {
		return fmt.Errorf("invalid catalog query")
	}
	if after, ok := query["after_id"]; ok && (len(after) != 1 || after[0] == "" || len(after[0]) > 256 || hasControl(after[0])) {
		return fmt.Errorf("invalid cursor")
	}
	for key := range query {
		if key != "limit" && key != "after_id" {
			return fmt.Errorf("invalid catalog query")
		}
	}
	root, err := parseOwnedDocument(raw)
	if err != nil {
		return fmt.Errorf("invalid snapshot")
	}
	entries, err := uniqueMappingValue(root, "claude-api-key")
	if err != nil || entries.Kind != yaml.SequenceNode || *selector.ConfigIndex >= len(entries.Content) {
		return fmt.Errorf("credential unavailable")
	}
	entry := entries.Content[*selector.ConfigIndex]
	if hasYAMLIndirection(entry, make(map[*yaml.Node]bool)) {
		return fmt.Errorf("indirect credential configuration")
	}
	configuredBase := "https://api.anthropic.com"
	if value, ok := scalarMappingString(entry, "base-url"); ok && strings.TrimSpace(value) != "" {
		configuredBase = value
	}
	normalized, err := normalizeHTTPSBaseURL(configuredBase)
	if err != nil || normalized != base {
		return fmt.Errorf("credential drift")
	}
	prefix, _ := scalarMappingString(entry, "prefix")
	if normalizeSelectorPrefix(prefix) != selector.Prefix {
		return fmt.Errorf("credential drift")
	}
	configured, credentialForms, err := catalogHeaders(entry)
	if err != nil {
		return fmt.Errorf("invalid credential headers")
	}
	if strings.EqualFold(baseURL.Hostname(), "api.anthropic.com") {
		if credentialForms["authorization"] || credentialForms["x-api-key"] {
			return fmt.Errorf("invalid anthropic headers")
		}
		return validateCatalogHeaders(descriptor, configured, "x-api-key", map[string]string{"anthropic-version": "2023-06-01"})
	}
	if credentialForms["authorization"] && credentialForms["x-api-key"] {
		return fmt.Errorf("ambiguous credential headers")
	}
	credential := "authorization"
	if credentialForms["x-api-key"] {
		credential = "x-api-key"
	}
	return validateCatalogHeaders(descriptor, configured, credential, nil)
}

func normalizeHTTPSBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return "", fmt.Errorf("invalid base url")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "443" {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Scheme = "https"
	u.Host = host
	if u.Path == "/" {
		u.Path = ""
	} else {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String(), nil
}

func normalizeSelectorPrefix(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), "/")
}

func scalarMappingString(mapping *yaml.Node, key string) (string, bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return "", false
	}
	value, err := uniqueMappingValue(mapping, key)
	if err != nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return "", false
	}
	return value.Value, true
}

func scalarMappingBool(mapping *yaml.Node, key string) (bool, bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false, false
	}
	value, err := uniqueMappingValue(mapping, key)
	if err != nil {
		return false, false
	}
	if value == nil {
		return false, false
	}
	if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
		return true, true
	}
	var result bool
	if err := value.Decode(&result); err != nil {
		return true, true
	}
	return result, true
}

func hasYAMLIndirection(node *yaml.Node, visited map[*yaml.Node]bool) bool {
	if node == nil || visited[node] {
		return false
	}
	visited[node] = true
	if node.Kind == yaml.AliasNode || node.Alias != nil {
		return true
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag == "!!merge" || key.Value == "<<" {
				return true
			}
		}
	}
	for _, child := range node.Content {
		if hasYAMLIndirection(child, visited) {
			return true
		}
	}
	return false
}

func catalogHeaders(mapping *yaml.Node) (map[string]string, map[string]bool, error) {
	configured := make(map[string]string)
	credentialForms := make(map[string]bool)
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("invalid catalog headers")
	}
	var headers *yaml.Node
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "headers" {
			headers = mapping.Content[i+1]
			break
		}
	}
	if headers == nil {
		return configured, credentialForms, nil
	}
	if headers.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("invalid catalog headers")
	}
	for i := 0; i < len(headers.Content); i += 2 {
		key, value := headers.Content[i], headers.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return nil, nil, fmt.Errorf("invalid catalog headers")
		}
		lower := strings.ToLower(key.Value)
		if lower == "authorization" || lower == "x-api-key" {
			if credentialForms[lower] {
				return nil, nil, fmt.Errorf("duplicate catalog header")
			}
			credentialForms[lower] = true
			continue
		}
		if !safeProviderHeaders[lower] {
			continue
		}
		if _, exists := configured[lower]; exists || hasControl(value.Value) || strings.Contains(value.Value, "$TOKEN$") {
			return nil, nil, fmt.Errorf("invalid catalog header")
		}
		configured[lower] = value.Value
	}
	return configured, credentialForms, nil
}

func validateCatalogHeaders(descriptor FetchDescriptor, configured map[string]string, credential string, required map[string]string) error {
	actual := make(map[string]string, len(descriptor.Header))
	for name, value := range descriptor.Header {
		actual[strings.ToLower(name)] = value
	}
	switch credential {
	case "authorization":
		if descriptor.Header["Authorization"] != "Bearer $TOKEN$" || actual["x-api-key"] != "" {
			return fmt.Errorf("invalid credential header")
		}
	case "x-api-key":
		if descriptor.Header["x-api-key"] != "$TOKEN$" || actual["authorization"] != "" {
			return fmt.Errorf("invalid credential header")
		}
	case "":
		if actual["authorization"] != "" || actual["x-api-key"] != "" {
			return fmt.Errorf("unexpected credential header")
		}
	default:
		return fmt.Errorf("invalid credential header")
	}
	for name, expected := range configured {
		if fixed, ok := required[name]; ok && fixed != expected {
			return fmt.Errorf("configured header conflicts with fixed header")
		}
		if actual[name] != expected {
			return fmt.Errorf("configured header mismatch")
		}
	}
	for name, expected := range required {
		if actual[name] != expected {
			return fmt.Errorf("required header mismatch")
		}
	}
	for name, value := range actual {
		if credential != "" && name == credential {
			continue
		}
		if expected, ok := configured[name]; ok && value == expected {
			continue
		}
		if expected, ok := required[name]; ok && value == expected {
			continue
		}
		if name == "accept" && value == "application/json" {
			continue
		}
		return fmt.Errorf("unconfigured catalog header")
	}
	return nil
}

func emptySelector(selector FetchSelector) bool {
	return selector.ChannelName == "" && selector.BaseURL == "" && selector.Prefix == "" && selector.ConfigIndex == nil
}

func claudeCursorProgressionKey(descriptor FetchDescriptor) string {
	if descriptor.Kind != "claude_models" || descriptor.Selector == nil {
		return ""
	}
	u, err := url.Parse(descriptor.URL)
	if err != nil {
		return ""
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	selector, _ := json.Marshal(descriptor.Selector)
	return string(selector) + "\x00" + query.Get("after_id")
}

func descriptorProgressionKey(descriptor FetchDescriptor) string {
	copy := descriptor
	copy.RequestID = ""
	copy.ContinuationBase64 = ""
	raw, _ := json.Marshal(copy)
	return string(raw)
}

func decodeStrictJSON(reader io.Reader, max int64, target any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil || int64(len(raw)) > max {
		return fmt.Errorf("invalid json envelope")
	}
	return decodeStrictJSONBytes(raw, target)
}

func decodeStrictJSONBytes(raw []byte, target any) error {
	if !utf8.Valid(raw) || validateExactJSONKeys(raw, reflect.TypeOf(target)) != nil {
		return fmt.Errorf("invalid json envelope")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid json envelope")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid json envelope")
	}
	return nil
}

func validateExactJSONKeys(raw []byte, targetType reflect.Type) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
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
				if !ok {
					return fmt.Errorf("invalid json key")
				}
				folded := strings.ToUpper(key)
				if seen[folded] {
					return fmt.Errorf("duplicate json key")
				}
				seen[folded] = true
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
			return fmt.Errorf("invalid json delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(targetType); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("invalid trailing json")
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
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				for embeddedName, embeddedType := range exactJSONFieldTypes(embedded) {
					fields[embeddedName] = embeddedType
				}
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}

func isTimeout(err error, ctx context.Context) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
