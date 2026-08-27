package plugin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type fakeAuthHost struct {
	mu             sync.Mutex
	entries        []AuthEntry
	physical       map[string]AuthPhysical
	listCalls      int
	runtimeCalls   int
	getCalls       int
	driftAfterList int
	blockList      chan struct{}
	listStarted    chan struct{}
}

func (h *fakeAuthHost) List() ([]AuthEntry, error) {
	h.mu.Lock()
	h.listCalls++
	call := h.listCalls
	entries := append([]AuthEntry(nil), h.entries...)
	block, started := h.blockList, h.listStarted
	drift := h.driftAfterList > 0 && call > h.driftAfterList
	h.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if drift {
		return nil, nil
	}
	return entries, nil
}

func (h *fakeAuthHost) GetRuntime(index string) (AuthEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runtimeCalls++
	for _, entry := range h.entries {
		if entry.AuthIndex == index {
			return entry, nil
		}
	}
	return AuthEntry{}, fmt.Errorf("missing")
}

func (h *fakeAuthHost) Get(index string) (AuthPhysical, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.getCalls++
	value, ok := h.physical[index]
	if !ok {
		return AuthPhysical{}, fmt.Errorf("missing")
	}
	value.JSON = append([]byte(nil), value.JSON...)
	return value, nil
}

func authFixture(selectors ...ChannelSelector) *fakeAuthHost {
	host := &fakeAuthHost{physical: make(map[string]AuthPhysical)}
	for index, selector := range selectors {
		authIndex := fmt.Sprintf("auth-%d", index)
		path := fmt.Sprintf("/auth/provider-%d.json", index)
		const provider = "openai-compatibility"
		host.entries = append(host.entries, AuthEntry{AuthIndex: authIndex, Provider: provider, Status: "active", Source: "file", Path: path})
		host.physical[authIndex] = AuthPhysical{AuthIndex: authIndex, Path: path, JSON: []byte(fmt.Sprintf(`{"type":%q,"base_url":%q,"api_key":"provider-secret-%d"}`, provider, selector.BaseURL, index))}
	}
	return host
}

func testConfig(t *testing.T, channels []ChannelConfig) runtimeConfig {
	t.Helper()
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	var body strings.Builder
	body.WriteString("worker_token_env: TEST_WRITER_TOKEN\nsync_epoch: epoch-a\nchannels:\n")
	for _, channel := range channels {
		body.WriteString(fmt.Sprintf("  - enabled: %t\n    selector:\n      name: %s\n      base_url: %s\n    mode: %s\n    patterns:\n", channel.Enabled, channel.Selector.Name, channel.Selector.BaseURL, channel.Mode))
		for _, pattern := range channel.Patterns {
			body.WriteString(fmt.Sprintf("      - %q\n", pattern))
		}
	}
	cfg, err := parseConfig([]byte(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Generation = 1
	cfg.AttemptID = "test-attempt"
	return cfg
}

func snapshotYAML() []byte {
	return []byte(`# root comment
api-keys:
  - root-secret
unrelated:
  nested: keep-me # keep comment
openai-compatibility:
  - name: provider-a
    base-url: https://a.example/v1
    headers:
      User-Agent: catalog-client
      Content-Type: application/json
    models:
      - name: old-a # retained comment
        alias: kept-alias
        max-output-tokens: 123
      - name: remove-a
  - name: provider-b
    base-url: https://b.example/v1
    models:
      - name: old-b
        thinking:
          type: enabled
  - name: disabled-provider
    base-url: https://disabled.example/v1
    models:
      - name: untouched
plugins:
  enabled: true
  dir: plugins
  configs:
    sync-config-write: {worker_token_env: TEST_WRITER_TOKEN}
    auto-pull-models: {worker_token_env: TEST_WRITER_TOKEN}
    model-metadata-sync: {worker_token_env: TEST_WRITER_TOKEN}
    model-info: {worker_token_env: TEST_WRITER_TOKEN}
`)
}

func initialRequest(t *testing.T, version string, snapshot []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": version, "config_base64": base64.StdEncoding.EncodeToString(snapshot)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func continuationRequest(t *testing.T, version string, snapshot []byte, descriptor fetchDescriptor, catalog []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"version":             version,
		"config_base64":       base64.StdEncoding.EncodeToString(snapshot),
		"continuation_base64": descriptor.ContinuationBase64,
		"fetch_result":        map[string]any{"request_id": descriptor.RequestID, "status_code": 200, "body_base64": base64.StdEncoding.EncodeToString(catalog)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPlanProcessesConfiguredChannelsInOrderAndPreservesMembershipData(t *testing.T) {
	a := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	b := ChannelSelector{Name: "provider-b", BaseURL: "https://b.example/v1"}
	disabled := ChannelSelector{Name: "disabled-provider", BaseURL: "https://disabled.example/v1"}
	cfg := testConfig(t, []ChannelConfig{
		{Enabled: true, Selector: a, Mode: ModeInclude, Patterns: []string{`^(old-a|new-a)$`}},
		{Enabled: true, Selector: b, Mode: ModeExclude, Patterns: []string{`^drop-`}},
		{Enabled: false, Selector: disabled, Mode: ModeInclude, Patterns: []string{`.*`}},
	})
	host := authFixture(a, b)
	version, snapshot := strings.Repeat("a", 64), snapshotYAML()

	firstRaw, code := plan(initialRequest(t, version, snapshot), cfg, host)
	if code != "" {
		t.Fatalf("initial code=%s", code)
	}
	first := firstRaw.(fetchEnvelope).NextFetch
	encoded, _ := json.Marshal(first)
	if first.Selector.ChannelName != a.Name || first.URL != a.BaseURL+"/models" || first.AuthIndex != "auth-0" || first.Header["Authorization"] != "Bearer $TOKEN$" || first.Header["User-Agent"] != "catalog-client" || first.Header["Content-Type"] != "application/json" || strings.Contains(string(encoded), "root-secret") || strings.Contains(string(encoded), "provider-secret") {
		t.Fatalf("first descriptor=%s", encoded)
	}

	secondRaw, code := plan(continuationRequest(t, version, snapshot, first, []byte(`{"data":[{"id":"old-a"},{"id":"new-a"},{"id":"drop-a"}]}`)), cfg, host)
	if code != "" {
		t.Fatalf("first continuation code=%s", code)
	}
	second := secondRaw.(fetchEnvelope).NextFetch
	if second.Selector.ChannelName != b.Name || second.AuthIndex != "auth-1" {
		t.Fatalf("second descriptor=%+v", second)
	}

	finalRaw, code := plan(continuationRequest(t, version, snapshot, second, []byte(`{"models":[{"id":"old-b"},{"id":"new-b"},{"id":"drop-b"}]}`)), cfg, host)
	if code != "" {
		t.Fatalf("second continuation code=%s", code)
	}
	final := finalRaw.(finalEnvelope)
	if !final.Report.Changed || final.BaseVersion != version {
		t.Fatalf("final=%+v", final)
	}
	proposed, err := base64.StdEncoding.Strict().DecodeString(final.ConfigBase64)
	if err != nil {
		t.Fatal(err)
	}
	text := string(proposed)
	for _, expected := range []string{"# root comment", "keep-me # keep comment", "name: old-a # retained comment", "alias: kept-alias", "max-output-tokens: 123", "name: new-a", "name: old-b", "name: new-b", "type: enabled", "name: untouched", "root-secret"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("proposal lost %q:\n%s", expected, text)
		}
	}
	for _, removed := range []string{"name: remove-a", "name: drop-b"} {
		if strings.Contains(text, removed) {
			t.Fatalf("proposal retained %q:\n%s", removed, text)
		}
	}
	proposedDocument, err := parseSnapshot(proposed)
	if err != nil {
		t.Fatal(err)
	}
	proposedA, err := locateSnapshotChannel(proposedDocument, a)
	if err != nil {
		t.Fatal(err)
	}
	modelsA, err := namedModels(proposedA.Models)
	if err != nil {
		t.Fatal(err)
	}
	if len(modelsA["new-a"].Content) != 2 {
		t.Fatalf("new model mapping=%+v", modelsA["new-a"].Content)
	}
	if alias, err := uniqueMappingValue(modelsA["old-a"], "alias"); err != nil || alias == nil || alias.Value != "kept-alias" {
		t.Fatalf("retained alias=%+v err=%v", alias, err)
	}
	if host.listCalls != 4 || host.runtimeCalls != 8 || host.getCalls != 8 {
		t.Fatalf("auth calls list=%d runtime=%d get=%d want all generic candidates checked before/after both channels", host.listCalls, host.runtimeCalls, host.getCalls)
	}
}

func TestPlanNoopAndDisabledChannelNeedNoCredential(t *testing.T) {
	disabled := ChannelSelector{Name: "disabled-provider", BaseURL: "https://disabled.example/v1"}
	cfg := testConfig(t, []ChannelConfig{{Enabled: false, Selector: disabled, Mode: ModeInclude}})
	result, code := plan(initialRequest(t, strings.Repeat("b", 64), snapshotYAML()), cfg, &fakeAuthHost{})
	if code != "" || result.(finalEnvelope).Report.Changed {
		t.Fatalf("result=%+v code=%s", result, code)
	}
}

func TestPlanFailsClosedForCredentialAndPostFetchDrift(t *testing.T) {
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	cfg := testConfig(t, []ChannelConfig{{Enabled: true, Selector: selector, Mode: ModeExclude}})
	version, snapshot := strings.Repeat("c", 64), snapshotYAML()
	specific := authFixture(selector)
	specific.entries[0].Provider = "openai-compatible-provider-a"
	specific.physical["auth-0"] = AuthPhysical{AuthIndex: "auth-0", Path: "/auth/provider-0.json", JSON: []byte(`{"type":"openai-compatible-provider-a","base_url":"https://a.example/v1","api_key":"secret"}`)}
	if _, code := plan(initialRequest(t, version, snapshot), cfg, specific); code != "" {
		t.Fatalf("specific compatible provider code=%s", code)
	}
	for name, mutate := range map[string]func(*fakeAuthHost){
		"runtime only":  func(host *fakeAuthHost) { host.entries[0].RuntimeOnly = true },
		"config source": func(host *fakeAuthHost) { host.entries[0].Source, host.entries[0].Path = "memory", "" },
		"disabled":      func(host *fakeAuthHost) { host.entries[0].Disabled = true },
		"unavailable":   func(host *fakeAuthHost) { host.entries[0].Unavailable = true },
		"physical base drift": func(host *fakeAuthHost) {
			host.physical["auth-0"] = AuthPhysical{AuthIndex: "auth-0", Path: "/auth/provider-0.json", JSON: []byte(`{"type":"openai-compatibility","base_url":"https://other.example/v1","api_key":"secret"}`)}
		},
		"physical case alias": func(host *fakeAuthHost) {
			host.physical["auth-0"] = AuthPhysical{AuthIndex: "auth-0", Path: "/auth/provider-0.json", JSON: []byte(`{"TYPE":"openai-compatibility","BASE_URL":"https://a.example/v1","api_key":"secret"}`)}
		},
		"physical semantic duplicate": func(host *fakeAuthHost) {
			host.physical["auth-0"] = AuthPhysical{AuthIndex: "auth-0", Path: "/auth/provider-0.json", JSON: []byte(`{"type":"openai-compatibility","TYPE":"openai-compatibility","base_url":"https://a.example/v1","api_key":"secret"}`)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			host := authFixture(selector)
			mutate(host)
			if _, code := plan(initialRequest(t, version, snapshot), cfg, host); code != errorCredential {
				t.Fatalf("code=%s", code)
			}
		})
	}
	ambiguous := authFixture(selector, selector)
	if _, code := plan(initialRequest(t, version, snapshot), cfg, ambiguous); code != errorCredential {
		t.Fatalf("ambiguous code=%s", code)
	}

	drift := authFixture(selector)
	drift.driftAfterList = 1
	firstRaw, code := plan(initialRequest(t, version, snapshot), cfg, drift)
	if code != "" {
		t.Fatal(code)
	}
	first := firstRaw.(fetchEnvelope).NextFetch
	if result, code := plan(continuationRequest(t, version, snapshot, first, []byte(`{"data":[{"id":"secret-body-marker"}]}`)), cfg, drift); code != errorCredential || result != nil {
		t.Fatalf("post-fetch drift result=%+v code=%s", result, code)
	}
}

func TestPlanRejectsDuplicateModelsSelectorsAndYAMLIndirection(t *testing.T) {
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	cfg := testConfig(t, []ChannelConfig{{Enabled: true, Selector: selector, Mode: ModeExclude}})
	host := authFixture(selector)
	duplicateModels := []byte("openai-compatibility:\n  - name: provider-a\n    base-url: https://a.example/v1\n    models:\n      - name: duplicate\n      - name: duplicate\n")
	if _, code := plan(initialRequest(t, strings.Repeat("d", 64), duplicateModels), cfg, host); code != errorInvalid {
		t.Fatalf("duplicate models code=%s", code)
	}
	merged := []byte("defaults: &defaults\n  headers: {User-Agent: hidden}\nopenai-compatibility:\n  - <<: *defaults\n    name: provider-a\n    base-url: https://a.example/v1\n    models: []\n")
	if _, code := plan(initialRequest(t, strings.Repeat("d", 64), merged), cfg, host); code != errorInvalid {
		t.Fatalf("merged channel code=%s", code)
	}
}

func TestPlanBindsContinuationToExactSnapshotVersionAndRequest(t *testing.T) {
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	cfg := testConfig(t, []ChannelConfig{{Enabled: true, Selector: selector, Mode: ModeExclude}})
	host := authFixture(selector)
	version, snapshot := strings.Repeat("f", 64), snapshotYAML()
	firstRaw, code := plan(initialRequest(t, version, snapshot), cfg, host)
	if code != "" {
		t.Fatal(code)
	}
	first := firstRaw.(fetchEnvelope).NextFetch
	catalog := []byte(`{"data":[{"id":"old-a"}]}`)

	mutatedSnapshot := append([]byte("# different exact snapshot\n"), snapshot...)
	if result, code := plan(continuationRequest(t, version, mutatedSnapshot, first, catalog), cfg, host); result != nil || code != errorInvalid {
		t.Fatalf("snapshot substitution result=%+v code=%s", result, code)
	}
	if result, code := plan(continuationRequest(t, strings.Repeat("a", 64), snapshot, first, catalog), cfg, host); result != nil || code != errorInvalid {
		t.Fatalf("version substitution result=%+v code=%s", result, code)
	}
	secondRaw, code := plan(initialRequest(t, version, snapshot), cfg, host)
	if code != "" {
		t.Fatal(code)
	}
	mixed := first
	mixed.RequestID = secondRaw.(fetchEnvelope).NextFetch.RequestID
	if result, code := plan(continuationRequest(t, version, snapshot, mixed, catalog), cfg, host); result != nil || code != errorInvalid {
		t.Fatalf("request substitution result=%+v code=%s", result, code)
	}
}

func TestPlanRejectsForbiddenCatalogHeaders(t *testing.T) {
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	cfg := testConfig(t, []ChannelConfig{{Enabled: true, Selector: selector, Mode: ModeExclude}})
	version := strings.Repeat("a", 64)
	for _, header := range []string{"Authorization", "x-api-key", "api-key", "api_key", "Host", "Proxy-Authorization", "Cookie", "X-Management-Key", "X-Sync-Config-Writer-Token"} {
		snapshot := []byte(fmt.Sprintf("openai-compatibility:\n  - name: provider-a\n    base-url: https://a.example/v1\n    headers:\n      %s: should-not-forward\n    models: []\n", header))
		if result, code := plan(initialRequest(t, version, snapshot), cfg, authFixture(selector)); result != nil || code != errorInvalid {
			t.Fatalf("header %q result=%+v code=%s", header, result, code)
		}
	}
}

func TestPlannerRejectsMalformedAndOversizeCatalogWithoutLeakingBody(t *testing.T) {
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	cfg := testConfig(t, []ChannelConfig{{Enabled: true, Selector: selector, Mode: ModeExclude}})
	host := authFixture(selector)
	version, snapshot := strings.Repeat("e", 64), snapshotYAML()
	firstRaw, code := plan(initialRequest(t, version, snapshot), cfg, host)
	if code != "" {
		t.Fatal(code)
	}
	descriptor := firstRaw.(fetchEnvelope).NextFetch
	for _, catalog := range [][]byte{
		[]byte(`provider-secret-body-not-json`),
		[]byte(`{"data":[{"id":"kept"},{"unexpected":"provider-secret-body-marker"}]}`),
		[]byte(`{"data":[{"id":"a"}],"models":[{"id":"b"}]}`),
		[]byte(`{"data":[{"id":"a","ID":"b"}]}`),
	} {
		if result, code := plan(continuationRequest(t, version, snapshot, descriptor, catalog), cfg, host); result != nil || code != errorInvalid {
			t.Fatalf("malformed result=%+v code=%s", result, code)
		}
	}
	if result, code := plan(continuationRequest(t, version, snapshot, descriptor, make([]byte, maxCatalogBytes+1)), cfg, host); result != nil || code != errorTooLarge {
		t.Fatalf("oversize result=%+v code=%s", result, code)
	}
}

func TestPlannerRequestStrictnessAndBounds(t *testing.T) {
	validConfig := base64.StdEncoding.EncodeToString(snapshotYAML())
	version := strings.Repeat("a", 64)
	versionJSON := `"` + version + `"`
	cases := []string{
		`{"version":` + versionJSON + `,"config_base64":"` + validConfig + `","unknown":true}`,
		`{"VERSION":` + versionJSON + `,"config_base64":"` + validConfig + `"}`,
		`{"version":` + versionJSON + `,"VERSION":` + versionJSON + `,"config_base64":"` + validConfig + `"}`,
		`{"version":` + versionJSON + `,"config_base64":"` + validConfig + `"} {}`,
		`{"version":` + versionJSON + `,"config_base64":"` + validConfig + `","continuation_base64":null,"fetch_result":null}`,
		`{"version":` + versionJSON + `,"config_base64":"` + validConfig + `","continuation_base64":"%%%","fetch_result":{"request_id":"x","status_code":200,"body_base64":""}}`,
		`{"version":` + versionJSON + `,"config_base64":"` + validConfig + `","continuation_base64":"e30=","fetch_result":{"REQUEST_ID":"x","status_code":200,"body_base64":""}}`,
		`{"version":` + versionJSON + `,"config_base64":"` + validConfig + `","continuation_base64":"e30=","fetch_result":{"request_id":"x","REQUEST_ID":"x","status_code":200,"body_base64":""}}`,
		`{"version":` + versionJSON + `,"config_base64":"` + validConfig + `","continuation_base64":"e30=","fetch_result":{"request_id":"x","status_code":200,"body_base64":"","unknown":true}}`,
	}
	for _, raw := range cases {
		if _, err := decodePlannerRequest([]byte(raw)); err == nil {
			t.Fatalf("accepted malformed request: %s", raw)
		}
	}
	for _, invalidVersion := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), "sha256:" + strings.Repeat("a", 64)} {
		raw := fmt.Sprintf(`{"version":%q,"config_base64":%q}`, invalidVersion, validConfig)
		if _, err := decodePlannerRequest([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid version %q", invalidVersion)
		}
	}
	validRaw := fmt.Sprintf(`{"version":%q,"config_base64":%q}`, version, validConfig)
	if _, err := decodePlannerRequest([]byte(validRaw)); err != nil {
		t.Fatalf("rejected valid request: %v", err)
	}
	oversize := base64.StdEncoding.EncodeToString(make([]byte, maxContinuationBytes+1))
	raw := fmt.Sprintf(`{"version":%q,"config_base64":%q,"continuation_base64":%q,"fetch_result":{"request_id":"x","status_code":200,"body_base64":""}}`, version, validConfig, oversize)
	if _, err := decodePlannerRequest([]byte(raw)); err == nil {
		t.Fatal("oversize continuation accepted")
	}
}
