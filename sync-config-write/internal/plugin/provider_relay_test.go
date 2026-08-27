package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func relaySnapshot() (ConfigSnapshot, []byte) {
	raw := []byte(`api-keys:
  - root-key
openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
claude-api-key:
  - api-key: secret-not-used-by-writer
    prefix: claude
plugins:
  configs:
    sync-config-write: {}
    auto-pull-models: {}
    model-metadata-sync: {}
    model-info: {}
`)
	return NewConfigSnapshot(raw), raw
}

func validOpenAIDescriptor() FetchDescriptor {
	return FetchDescriptor{
		RequestID: "request-1", Kind: "openai_models",
		Selector:  &FetchSelector{ChannelName: "provider", BaseURL: "https://provider.example/v1"},
		AuthIndex: "file-auth-index", Method: http.MethodGet,
		URL:                "https://provider.example/v1/models",
		Header:             map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json"},
		ContinuationBase64: base64.StdEncoding.EncodeToString([]byte("state-1")),
	}
}

func TestOpenAICodexManifestQueryIsMetadataOnlyAndExact(t *testing.T) {
	snapshot, _ := relaySnapshot()
	descriptor := validOpenAIDescriptor()
	descriptor.URL += "?client_version=1.0.0"
	if err := validateFetchDescriptorForOperation(descriptor, snapshot, OperationMetadataSync); err != nil {
		t.Fatalf("metadata Codex manifest rejected: %v", err)
	}
	if err := validateFetchDescriptorForOperation(descriptor, snapshot, OperationAutoPull); err == nil {
		t.Fatal("auto-pull accepted metadata-only Codex manifest query")
	}
	for _, rawQuery := range []string{
		"client_version=1.0.1",
		"client_version=1.0.0&extra=true",
		"CLIENT_VERSION=1.0.0",
		"client_version=1%2E0%2E0",
		"client_version=",
	} {
		descriptor.URL = "https://provider.example/v1/models?" + rawQuery
		if err := validateFetchDescriptorForOperation(descriptor, snapshot, OperationMetadataSync); err == nil {
			t.Fatalf("metadata accepted query %q", rawQuery)
		}
	}
	descriptor.URL = "https://provider.example/v1/models"
	if err := validateFetchDescriptorForOperation(descriptor, snapshot, OperationMetadataSync); err != nil {
		t.Fatalf("ordinary metadata catalog rejected: %v", err)
	}
}

func TestHTTPPlannerRelaysCodexManifestOnlyForMetadataOperation(t *testing.T) {
	snapshot, proposed := relaySnapshot()
	descriptor := validOpenAIDescriptor()
	descriptor.URL += "?client_version=1.0.0"
	apiCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case plannerPaths[OperationAutoPull], plannerPaths[OperationMetadataSync]:
			var request plannerRequest
			if err := decodeStrictJSON(r.Body, 40<<20, &request); err != nil {
				t.Errorf("planner request: %v", err)
			}
			if request.FetchResult == nil {
				_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: ptrDescriptor(descriptor)})
				return
			}
			_ = json.NewEncoder(w).Encode(ProposalFromBytes(snapshot.Version, proposed))
		case apiCallPath:
			apiCalls++
			var call apiCallRequest
			if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
				t.Fatal(err)
			}
			if call.URL != descriptor.URL {
				t.Errorf("relay URL=%q", call.URL)
			}
			_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: http.StatusOK, Body: `{"data":[{"id":"gpt"}]}`})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	settings := Settings{CoreOrigin: server.URL, ManagementKey: "management-secret", WorkerToken: "worker-secret"}
	planner := NewHTTPPlanner(NewLoopbackClient())
	if _, code := planner.Plan(context.Background(), OperationMetadataSync, snapshot, settings); code != "" {
		t.Fatalf("metadata code=%s", code)
	}
	if _, code := planner.Plan(context.Background(), OperationAutoPull, snapshot, settings); code != CodeProviderFetchInvalid {
		t.Fatalf("auto-pull code=%s", code)
	}
	if apiCalls != 1 {
		t.Fatalf("api calls=%d", apiCalls)
	}
}

func TestHTTPPlannerRelaysExactAPICallAndOnlyBodyToContinuation(t *testing.T) {
	snapshot, proposed := relaySnapshot()
	providerBody := []byte{0, '<', '&', 0xc3, 0xa9}
	var plannerCalls, apiCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management-secret" {
			t.Errorf("management authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case plannerPaths[OperationAutoPull]:
			plannerCalls++
			var request plannerRequest
			if err := decodeStrictJSON(r.Body, 40<<20, &request); err != nil {
				t.Errorf("planner request: %v", err)
			}
			if request.Version != snapshot.Version || request.ConfigBase64 != snapshot.ConfigBase64 {
				t.Error("snapshot changed")
			}
			if plannerCalls == 1 {
				if request.ContinuationBase64 != "" || request.FetchResult != nil {
					t.Error("initial request contained continuation")
				}
				_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: ptrDescriptor(validOpenAIDescriptor())})
				return
			}
			if request.ContinuationBase64 != validOpenAIDescriptor().ContinuationBase64 || request.FetchResult == nil || request.FetchResult.RequestID != "request-1" || request.FetchResult.StatusCode != 200 {
				t.Errorf("continuation=%+v", request)
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(request.FetchResult.BodyBase64)
			if err != nil || !bytes.Equal(decoded, providerBody) {
				t.Errorf("body fidelity got=%x err=%v", decoded, err)
			}
			body, _ := json.Marshal(request)
			if bytes.Contains(body, []byte("X-Upstream-Secret")) {
				t.Error("upstream header reached planner")
			}
			_ = json.NewEncoder(w).Encode(ProposalFromBytes(snapshot.Version, proposed))
		case apiCallPath:
			apiCalls++
			var raw map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if len(raw) != 4 || raw["proxy_url"] != nil || raw["data"] != nil {
				t.Errorf("api-call fields=%v", raw)
			}
			var call apiCallRequest
			encoded, _ := json.Marshal(raw)
			if err := json.Unmarshal(encoded, &call); err != nil {
				t.Fatal(err)
			}
			if call.AuthIndex != "file-auth-index" || call.Method != http.MethodGet || call.URL != "https://provider.example/v1/models" || call.Header["Authorization"] != "Bearer $TOKEN$" {
				t.Errorf("api-call=%+v", call)
			}
			_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: 200, Header: map[string][]string{"X-Upstream-Secret": {"strip-me"}}, Body: string(providerBody)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	settings := Settings{CoreOrigin: server.URL, ManagementKey: "management-secret", WorkerToken: "worker-secret"}
	var progress []RunState
	proposal, code := NewHTTPPlanner(NewLoopbackClient()).PlanWithProgress(context.Background(), OperationAutoPull, snapshot, settings, func(state RunState) { progress = append(progress, state) })
	if code != "" {
		t.Fatalf("code=%s", code)
	}
	decoded, err := proposal.Decode(snapshot.Version)
	if err != nil || !bytes.Equal(decoded, proposed) {
		t.Fatalf("proposal mismatch err=%v", err)
	}
	if plannerCalls != 2 || apiCalls != 1 || fmt.Sprint(progress) != "[fetching planning]" {
		t.Fatalf("planner=%d api=%d progress=%v", plannerCalls, apiCalls, progress)
	}
}

func TestFetchDescriptorRejectsAttacksAndBadCredentialBindings(t *testing.T) {
	snapshot, _ := relaySnapshot()
	valid := validOpenAIDescriptor()
	tests := map[string]func(*FetchDescriptor){
		"missing selector":     func(d *FetchDescriptor) { d.Selector = nil },
		"post":                 func(d *FetchDescriptor) { d.Method = http.MethodPost },
		"http":                 func(d *FetchDescriptor) { d.URL = "http://provider.example/v1/models" },
		"userinfo":             func(d *FetchDescriptor) { d.URL = "https://u@provider.example/v1/models" },
		"fragment":             func(d *FetchDescriptor) { d.URL += "#secret" },
		"wrong path":           func(d *FetchDescriptor) { d.URL = "https://provider.example/private" },
		"host":                 func(d *FetchDescriptor) { d.Header["Host"] = "evil.example" },
		"hop by hop":           func(d *FetchDescriptor) { d.Header["Connection"] = "keep-alive" },
		"management":           func(d *FetchDescriptor) { d.Header["X-Management-Key"] = "$TOKEN$" },
		"worker":               func(d *FetchDescriptor) { d.Header[workerTokenHeader] = "$TOKEN$" },
		"crlf":                 func(d *FetchDescriptor) { d.Header["Accept"] = "ok\r\nX-Evil: 1" },
		"header control":       func(d *FetchDescriptor) { d.Header["Accept"] = "ok\x00bad" },
		"plaintext auth":       func(d *FetchDescriptor) { d.Header["Authorization"] = "Bearer plaintext" },
		"wrong placeholder":    func(d *FetchDescriptor) { d.Header["Authorization"] = "$TOKEN$" },
		"unconfigured literal": func(d *FetchDescriptor) { d.Header["User-Agent"] = "root-key" },
		"openai x-api-key": func(d *FetchDescriptor) {
			delete(d.Header, "Authorization")
			d.Header["x-api-key"] = "$TOKEN$"
		},
		"placeholder safe":     func(d *FetchDescriptor) { d.Header["Accept"] = "$TOKEN$" },
		"missing auth index":   func(d *FetchDescriptor) { d.AuthIndex = "" },
		"missing auth header":  func(d *FetchDescriptor) { delete(d.Header, "Authorization") },
		"two credential heads": func(d *FetchDescriptor) { d.Header["x-api-key"] = "$TOKEN$" },
		"empty continuation":   func(d *FetchDescriptor) { d.ContinuationBase64 = "" },
		"bad continuation":     func(d *FetchDescriptor) { d.ContinuationBase64 = "%%%" },
		"oversize continuation": func(d *FetchDescriptor) {
			d.ContinuationBase64 = base64.StdEncoding.EncodeToString(make([]byte, maxContinuationBytes+1))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d := valid
			d.Header = cloneHeaders(valid.Header)
			mutate(&d)
			if err := validateFetchDescriptor(d, snapshot); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if err := validateFetchDescriptor(valid, snapshot); err != nil {
		t.Fatalf("valid descriptor rejected: %v", err)
	}
	for _, header := range []string{"Accept", "User-Agent", "Content-Type", "anthropic-version", "anthropic-beta"} {
		descriptor := valid
		descriptor.Header = cloneHeaders(valid.Header)
		descriptor.Header[header] = "root-key"
		if err := validateFetchDescriptor(descriptor, snapshot); err == nil {
			t.Fatalf("unconfigured secret accepted in %s", header)
		}
	}
	public := FetchDescriptor{RequestID: "public-1", Kind: "modelsdev", Selector: &FetchSelector{}, Method: http.MethodGet, URL: "https://models.dev/api.json", ContinuationBase64: base64.StdEncoding.EncodeToString([]byte("public-state"))}
	if err := validateFetchDescriptor(public, snapshot); err != nil {
		t.Fatalf("valid public descriptor rejected: %v", err)
	}
	public.Header = map[string]string{"User-Agent": "root-key"}
	if err := validateFetchDescriptor(public, snapshot); err == nil {
		t.Fatal("public metadata accepted unconfigured literal header")
	}
	public.Header = map[string]string{"Accept": "application/json"}
	if err := validateFetchDescriptor(public, snapshot); err != nil {
		t.Fatalf("public metadata fixed Accept rejected: %v", err)
	}
	public.AuthIndex, public.Header = "auth", map[string]string{"Authorization": "Bearer $TOKEN$"}
	if err := validateFetchDescriptor(public, snapshot); err == nil {
		t.Fatal("public metadata credential must be rejected")
	}
}

func TestFetchDescriptorUsesCanonicalSnapshotSelectorsAndRejectsDisabledChannels(t *testing.T) {
	if got, err := normalizeHTTPSBaseURL(" HTTPS://EXAMPLE.com:443/v1/ "); err != nil || got != "https://example.com/v1" {
		t.Fatalf("normalized base=%q err=%v", got, err)
	}

	disabledRaw := []byte("openai-compatibility:\n  - name: provider\n    disabled: true\n    base-url: https://provider.example/v1\n")
	if err := validateFetchDescriptor(validOpenAIDescriptor(), NewConfigSnapshot(disabledRaw)); err == nil {
		t.Fatal("disabled OpenAI-compatible channel accepted")
	}

	index := 0
	claudeRaw := []byte("claude-api-key:\n  - api-key: unused\n    base-url: HTTPS://API.ANTHROPIC.COM:443/\n    prefix: /claude/\n")
	valid := FetchDescriptor{
		RequestID: "claude-request", Kind: "claude_models",
		Selector:  &FetchSelector{BaseURL: "https://api.anthropic.com", Prefix: "claude", ConfigIndex: &index},
		AuthIndex: "claude-file-auth", Method: http.MethodGet,
		URL:                "https://api.anthropic.com/v1/models?limit=1000",
		Header:             map[string]string{"x-api-key": "$TOKEN$", "anthropic-version": "2023-06-01"},
		ContinuationBase64: base64.StdEncoding.EncodeToString([]byte("claude-state")),
	}
	if err := validateFetchDescriptor(valid, NewConfigSnapshot(claudeRaw)); err != nil {
		t.Fatalf("canonical Claude selector rejected: %v", err)
	}
	for _, rawURL := range []string{
		"https://api.anthropic.com/v1/models?limit=1000&after_id=%zz",
		"https://api.anthropic.com/v1/models?limit=1000&evil=%zz",
	} {
		malformed := valid
		malformed.URL = rawURL
		if err := validateFetchDescriptor(malformed, NewConfigSnapshot(claudeRaw)); err == nil {
			t.Fatalf("malformed Claude query accepted: %s", rawURL)
		}
	}
	invalid := valid
	selector := *valid.Selector
	selector.Prefix = "/claude/"
	invalid.Selector = &selector
	if err := validateFetchDescriptor(invalid, NewConfigSnapshot(claudeRaw)); err == nil {
		t.Fatal("noncanonical Claude prefix accepted")
	}

	configuredRaw := []byte("openai-compatibility:\n  - name: provider\n    base-url: https://provider.example/v1\n    headers:\n      User-Agent: catalog-client\n")
	configured := validOpenAIDescriptor()
	configured.Header["User-Agent"] = "catalog-client"
	if err := validateFetchDescriptor(configured, NewConfigSnapshot(configuredRaw)); err != nil {
		t.Fatalf("configured safe header rejected: %v", err)
	}
	configured.Header["User-Agent"] = "root-key"
	if err := validateFetchDescriptor(configured, NewConfigSnapshot(configuredRaw)); err == nil {
		t.Fatal("configured safe header value mismatch accepted")
	}

	compatibleRaw := []byte("claude-api-key:\n  - api-key: unused\n    base-url: https://claude.example\n    prefix: claude\n")
	compatible := valid
	compatible.Selector = &FetchSelector{BaseURL: "https://claude.example", Prefix: "claude", ConfigIndex: &index}
	compatible.URL = "https://claude.example/v1/models?limit=1000"
	compatible.Header = map[string]string{"Authorization": "Bearer $TOKEN$"}
	if err := validateFetchDescriptor(compatible, NewConfigSnapshot(compatibleRaw)); err != nil {
		t.Fatalf("compatible Claude default authorization rejected: %v", err)
	}
	compatible.Header = map[string]string{"x-api-key": "$TOKEN$"}
	if err := validateFetchDescriptor(compatible, NewConfigSnapshot(compatibleRaw)); err == nil {
		t.Fatal("unconfigured compatible Claude x-api-key accepted")
	}
	compatibleXAPI := []byte("claude-api-key:\n  - api-key: unused\n    base-url: https://claude.example\n    prefix: claude\n    headers:\n      x-api-key: configured-contract\n")
	if err := validateFetchDescriptor(compatible, NewConfigSnapshot(compatibleXAPI)); err != nil {
		t.Fatalf("configured compatible Claude x-api-key rejected: %v", err)
	}
}

func TestFetchDescriptorRejectsMergedOrAliasedSelectedProviderConfiguration(t *testing.T) {
	openAI := validOpenAIDescriptor()
	openAI.Header["User-Agent"] = "catalog-client"
	for name, raw := range map[string]string{
		"parent merge": `defaults: &defaults
  headers:
    User-Agent: catalog-client
openai-compatibility:
  - <<: *defaults
    name: provider
    base-url: https://provider.example/v1
`,
		"header alias": `catalog-headers: &catalog-headers
  User-Agent: catalog-client
openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    headers: *catalog-headers
`,
		"nested header merge": `catalog-headers: &catalog-headers
  User-Agent: catalog-client
openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    headers:
      <<: *catalog-headers
`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateFetchDescriptor(openAI, NewConfigSnapshot([]byte(raw))); err == nil {
				t.Fatal("indirect OpenAI-compatible configuration accepted")
			}
		})
	}

	index := 0
	claude := FetchDescriptor{
		RequestID: "claude-request", Kind: "claude_models",
		Selector:  &FetchSelector{BaseURL: "https://claude.example", Prefix: "claude", ConfigIndex: &index},
		AuthIndex: "claude-file-auth", Method: http.MethodGet,
		URL:                "https://claude.example/v1/models?limit=1000",
		Header:             map[string]string{"x-api-key": "$TOKEN$"},
		ContinuationBase64: base64.StdEncoding.EncodeToString([]byte("claude-state")),
	}
	mergedClaude := []byte(`defaults: &defaults
  headers:
    x-api-key: configured-contract
claude-api-key:
  - <<: *defaults
    api-key: unused
    base-url: https://claude.example
    prefix: claude
`)
	if err := validateFetchDescriptor(claude, NewConfigSnapshot(mergedClaude)); err == nil {
		t.Fatal("indirect Claude credential configuration accepted")
	}
}

func TestHTTPPlannerRejectsUnknownFetchFieldsAndTrailingJSON(t *testing.T) {
	snapshot, _ := relaySnapshot()
	descriptor, _ := json.Marshal(validOpenAIDescriptor())
	descriptor = descriptor[:len(descriptor)-1]
	validDescriptor := append(append([]byte(nil), descriptor...), '}')
	caseAliasedDescriptor := strings.Replace(string(validDescriptor), `"method"`, `"METHOD"`, 1)
	caseAliasedSelector := strings.Replace(string(validDescriptor), `"channel_name"`, `"CHANNEL_NAME"`, 1)
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "descriptor body", payload: fmt.Sprintf(`{"base_version":%q,"next_fetch":%s}`, snapshot.Version, append(append([]byte(nil), descriptor...), []byte(`,"data":"forbidden"}`)...))},
		{name: "descriptor proxy", payload: fmt.Sprintf(`{"base_version":%q,"next_fetch":%s}`, snapshot.Version, append(append([]byte(nil), descriptor...), []byte(`,"proxy_url":"https://proxy.example"}`)...))},
		{name: "top unknown", payload: fmt.Sprintf(`{"base_version":%q,"next_fetch":%s,"unknown":true}`, snapshot.Version, validDescriptor)},
		{name: "descriptor case alias", payload: fmt.Sprintf(`{"base_version":%q,"next_fetch":%s}`, snapshot.Version, caseAliasedDescriptor)},
		{name: "selector case alias", payload: fmt.Sprintf(`{"base_version":%q,"next_fetch":%s}`, snapshot.Version, caseAliasedSelector)},
		{name: "trailing", payload: fmt.Sprintf(`{"base_version":%q,"next_fetch":%s} {"trailing":true}`, snapshot.Version, validDescriptor)},
		{name: "duplicate", payload: fmt.Sprintf(`{"base_version":%q,"base_version":%q,"next_fetch":%s}`, snapshot.Version, snapshot.Version, validDescriptor)},
	} {
		t.Run(test.name, func(t *testing.T) {
			apiCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == apiCallPath {
					apiCalls++
				}
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			_, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationAutoPull, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"})
			if code != CodeProviderFetchInvalid || apiCalls != 0 {
				t.Fatalf("code=%s api_calls=%d", code, apiCalls)
			}
		})
	}
}

func TestHTTPPlannerRequiresExactFinalReportAndNonNullFetch(t *testing.T) {
	snapshot, proposed := relaySnapshot()
	configBase64 := base64.StdEncoding.EncodeToString(proposed)
	for _, test := range []struct {
		name    string
		payload string
		want    ErrorCode
	}{
		{name: "missing report", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "empty report", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"report":{}}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "wrong changed type", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"report":{"changed":"yes"}}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "unexpected report channels", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"report":{"changed":true,"channels":[]}}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "null report", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"report":null}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "top case aliases", payload: fmt.Sprintf(`{"BASE_VERSION":%q,"CONFIG_BASE64":%q,"REPORT":{"changed":true}}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "top case duplicate", payload: fmt.Sprintf(`{"base_version":%q,"BASE_VERSION":%q,"config_base64":%q,"report":{"changed":true}}`, snapshot.Version, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "report case alias", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"report":{"CHANGED":true}}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "report case duplicate", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"report":{"changed":true,"CHANGED":true}}`, snapshot.Version, configBase64), want: CodeInvalidRequest},
		{name: "explicit null fetch", payload: fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"next_fetch":null,"report":{"changed":true}}`, snapshot.Version, configBase64), want: CodeProviderFetchInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			_, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationAutoPull, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"})
			if code != test.want {
				t.Fatalf("code=%s want=%s", code, test.want)
			}
		})
	}
	valid := fmt.Sprintf(`{"base_version":%q,"config_base64":%q,"report":{"changed":false}}`, snapshot.Version, configBase64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(valid)) }))
	defer server.Close()
	if _, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationAutoPull, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"}); code != "" {
		t.Fatalf("valid final report rejected: %s", code)
	}
}

func TestHTTPPlannerRejectsRepeatedDescriptorBeforeSecondFetch(t *testing.T) {
	snapshot, raw := relaySnapshot()
	plannerCalls, apiCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case plannerPaths[OperationMetadataSync]:
			plannerCalls++
			d := validOpenAIDescriptor()
			if plannerCalls == 2 {
				d.RequestID = "request-2"
				d.ContinuationBase64 = base64.StdEncoding.EncodeToString([]byte("state-2"))
			}
			_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: &d})
		case apiCallPath:
			apiCalls++
			_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: 200, Header: map[string][]string{}, Body: string(raw[:8])})
		}
	}))
	defer server.Close()
	_, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationMetadataSync, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"})
	if code != CodeProviderFetchInvalid || plannerCalls != 2 || apiCalls != 1 {
		t.Fatalf("code=%s planner=%d api=%d", code, plannerCalls, apiCalls)
	}
}

func TestHTTPPlannerRejectsRepeatedClaudeCursorDespiteHeaderChanges(t *testing.T) {
	snapshot, _ := relaySnapshot()
	plannerCalls, apiCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case plannerPaths[OperationMetadataSync]:
			plannerCalls++
			index := 0
			header := map[string]string{"x-api-key": "$TOKEN$", "anthropic-version": "2023-06-01"}
			if plannerCalls == 2 {
				header["Accept"] = "application/json"
			}
			descriptor := FetchDescriptor{
				RequestID: fmt.Sprintf("claude-%d", plannerCalls), Kind: "claude_models",
				Selector:  &FetchSelector{BaseURL: "https://api.anthropic.com", Prefix: "claude", ConfigIndex: &index},
				AuthIndex: "claude-file-auth", Method: http.MethodGet,
				URL:                "https://api.anthropic.com/v1/models?limit=1000&after_id=same-cursor",
				Header:             header,
				ContinuationBase64: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("claude-state-%d", plannerCalls))),
			}
			_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: &descriptor})
		case apiCallPath:
			apiCalls++
			_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: http.StatusOK, Header: map[string][]string{}, Body: `{}`})
		}
	}))
	defer server.Close()
	_, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationMetadataSync, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"})
	if code != CodeProviderFetchInvalid || plannerCalls != 2 || apiCalls != 1 {
		t.Fatalf("code=%s planner=%d api=%d", code, plannerCalls, apiCalls)
	}
}

func TestHTTPPlannerProviderPayloadLimitAndAuthRemoval(t *testing.T) {
	snapshot, _ := relaySnapshot()
	for _, test := range []struct {
		name       string
		apiBody    string
		secondCode int
		want       ErrorCode
	}{
		{name: "oversize", apiBody: strings.Repeat("x", maxProviderPayloadBytes+1), want: CodeProviderCatalogTooLarge},
		{name: "auth removed", apiBody: "ok", secondCode: http.StatusUnprocessableEntity, want: CodeProviderCredentialUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			plannerCalls, apiCalls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case plannerPaths[OperationAutoPull]:
					plannerCalls++
					if plannerCalls == 1 {
						d := validOpenAIDescriptor()
						_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: &d})
						return
					}
					w.WriteHeader(test.secondCode)
					_ = json.NewEncoder(w).Encode(map[string]ErrorCode{"error_code": CodeProviderCredentialUnavailable})
				case apiCallPath:
					apiCalls++
					_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: 200, Header: map[string][]string{"Authorization": {"never-forward"}}, Body: test.apiBody})
				}
			}))
			defer server.Close()
			_, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationAutoPull, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"})
			if code != test.want || apiCalls != 1 {
				t.Fatalf("code=%s api_calls=%d", code, apiCalls)
			}
			if test.want == CodeProviderCatalogTooLarge && plannerCalls != 1 {
				t.Fatalf("oversize reached continuation: %d", plannerCalls)
			}
		})
	}
}

func TestAuthRemovalAndProviderFailureCanNetworkButNeverPUT(t *testing.T) {
	snapshot, raw := relaySnapshot()
	for _, test := range []struct {
		name         string
		continuation bool
		want         ErrorCode
	}{
		{name: "post-fetch auth removal", continuation: true, want: CodeProviderCredentialUnavailable},
		{name: "provider non-2xx", want: CodeProviderFetchFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			plannerCalls, apiCalls, puts := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == coreConfigPath && r.Method == http.MethodGet:
					_, _ = w.Write(raw)
				case r.URL.Path == coreConfigPath && r.Method == http.MethodPut:
					puts++
					w.WriteHeader(http.StatusNoContent)
				case r.URL.Path == plannerPaths[OperationAutoPull]:
					plannerCalls++
					if plannerCalls == 1 {
						d := validOpenAIDescriptor()
						_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: &d})
						return
					}
					w.WriteHeader(http.StatusUnprocessableEntity)
					_ = json.NewEncoder(w).Encode(map[string]ErrorCode{"error_code": CodeProviderCredentialUnavailable})
				case r.URL.Path == apiCallPath:
					apiCalls++
					status := http.StatusUnauthorized
					if test.continuation {
						status = http.StatusOK
					}
					_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: status, Header: map[string][]string{}, Body: "discarded"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			settings := Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"}
			client := NewLoopbackClient()
			engine := NewCommitEngine(client, nil, func() Settings { return settings }, nil)
			outcome := NewWriterExecutor(NewHTTPPlanner(client), engine).Execute(context.Background(), OperationAutoPull, settings)
			if outcome.Code != test.want || apiCalls != 1 || puts != 0 {
				t.Fatalf("outcome=%+v api=%d puts=%d planner=%d", outcome, apiCalls, puts, plannerCalls)
			}
		})
	}
}

func TestHTTPPlannerStripsOuterAndUpstreamFailures(t *testing.T) {
	snapshot, _ := relaySnapshot()
	for _, test := range []struct {
		name        string
		outerStatus int
		upstream    int
		malformed   bool
	}{
		{name: "outer", outerStatus: http.StatusBadGateway},
		{name: "upstream", outerStatus: http.StatusOK, upstream: http.StatusUnauthorized},
		{name: "malformed", outerStatus: http.StatusOK, malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == plannerPaths[OperationAutoPull] {
					d := validOpenAIDescriptor()
					_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: &d})
					return
				}
				w.Header().Set("X-Secret", "header-secret")
				w.WriteHeader(test.outerStatus)
				if test.malformed {
					_, _ = w.Write([]byte(`{"status_code":200,"body":`))
					return
				}
				if test.outerStatus == http.StatusOK {
					_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: test.upstream, Header: map[string][]string{"X-Secret": {"hidden"}}, Body: "upstream-secret-detail"})
				} else {
					_, _ = w.Write([]byte("outer-secret-detail"))
				}
			}))
			defer server.Close()
			_, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationAutoPull, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"})
			if code != CodeProviderFetchFailed || strings.Contains(string(code), "secret") {
				t.Fatalf("code=%q", code)
			}
		})
	}
}

func TestHTTPPlannerEnforcesPageAndCumulativeLimits(t *testing.T) {
	snapshot, _ := relaySnapshot()
	for _, test := range []struct {
		name         string
		pageBody     string
		wantPlanner  int
		wantAPICalls int
		wantCode     ErrorCode
	}{
		{name: "page limit", pageBody: "x", wantPlanner: maxProviderPages + 1, wantAPICalls: maxProviderPages, wantCode: CodeProviderFetchInvalid},
		{name: "cumulative size", pageBody: strings.Repeat("x", maxProviderPayloadBytes/2+1), wantPlanner: 2, wantAPICalls: 2, wantCode: CodeProviderCatalogTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			plannerCalls, apiCalls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case plannerPaths[OperationMetadataSync]:
					plannerCalls++
					index := 0
					selector := FetchSelector{BaseURL: "https://api.anthropic.com", Prefix: "claude", ConfigIndex: &index}
					d := FetchDescriptor{
						RequestID: fmt.Sprintf("request-%d", plannerCalls), Kind: "claude_models", Selector: &selector,
						AuthIndex: "claude-file-auth", Method: http.MethodGet,
						URL:                fmt.Sprintf("https://api.anthropic.com/v1/models?limit=1000&after_id=page-%d", plannerCalls),
						Header:             map[string]string{"x-api-key": "$TOKEN$", "anthropic-version": "2023-06-01"},
						ContinuationBase64: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("state-%d", plannerCalls))),
					}
					_ = json.NewEncoder(w).Encode(CommitProposal{BaseVersion: snapshot.Version, NextFetch: &d})
				case apiCallPath:
					apiCalls++
					_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: 200, Header: map[string][]string{}, Body: test.pageBody})
				}
			}))
			defer server.Close()
			_, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationMetadataSync, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "m", WorkerToken: "w"})
			if code != test.wantCode || plannerCalls != test.wantPlanner || apiCalls != test.wantAPICalls {
				t.Fatalf("code=%s planner=%d api=%d", code, plannerCalls, apiCalls)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestPlannerTimeoutMapsByActivePlan(t *testing.T) {
	snapshot, _ := relaySnapshot()
	for _, test := range []struct {
		name   string
		active bool
		code   ErrorCode
	}{
		{name: "cleared", code: CodeLoopbackTimeout},
		{name: "active", active: true, code: CodePlannerStalled},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := WorkerStatus{InstanceID: mustOpaqueID(t), ReconfigureSeq: 1, ConfigSHA256: strings.Repeat("a", 64), ActivePlan: test.active}
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == plannerPaths[OperationAutoPull] {
					return nil, context.DeadlineExceeded
				}
				body, _ := json.Marshal(status)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})}
			_, code := NewHTTPPlanner(client).Plan(context.Background(), OperationAutoPull, snapshot, Settings{CoreOrigin: "http://127.0.0.1:8317", ManagementKey: "m", WorkerToken: "w"})
			if code != test.code {
				t.Fatalf("code=%s", code)
			}
		})
	}
}

func ptrDescriptor(value FetchDescriptor) *FetchDescriptor { return &value }

func cloneHeaders(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
