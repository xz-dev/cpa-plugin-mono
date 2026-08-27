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
	value, ok := h.physical[index]
	if !ok {
		return AuthPhysical{}, fmt.Errorf("missing")
	}
	value.JSON = append([]byte(nil), value.JSON...)
	return value, nil
}

func authFixture() *fakeAuthHost {
	return &fakeAuthHost{
		entries: []AuthEntry{
			{AuthIndex: "openai-auth", Provider: "openai-compatibility", Status: "active", Source: "file", Path: "/auth/openai.json"},
			{AuthIndex: "claude-auth", Provider: "claude", Status: "active", Source: "file", Path: "/auth/claude.json"},
		},
		physical: map[string]AuthPhysical{
			"openai-auth": {AuthIndex: "openai-auth", Path: "/auth/openai.json", JSON: []byte(`{"type":"openai-compatibility","base_url":"https://axis.example/v1","api_key":"secret"}`)},
			"claude-auth": {AuthIndex: "claude-auth", Path: "/auth/claude.json", JSON: []byte(`{"type":"claude","base_url":"https://api.anthropic.com","prefix":"anthropic","access_token":"secret"}`)},
		},
	}
}

func validConfigYAML() []byte {
	return []byte(`worker_token_env: TEST_WRITER_TOKEN
sync_epoch: epoch-a
channels:
  - enabled: true
    kind: openai-compatibility
    selector:
      name: axis
      base_url: https://axis.example/v1
    upstream_meta: true
    codex_manifest: true
    metadata_sources:
      - modelparams.dev/openai/subscription
      - models.dev/openai
    overrides:
      gpt-x:
        max_output_tokens: 120000
  - enabled: true
    kind: claude
    selector:
      config_index: 0
      base_url: https://api.anthropic.com
      prefix: anthropic
    upstream_meta: true
`)
}

func snapshotYAML() []byte {
	return []byte(`# root comment
api-keys: [root-secret]
unrelated:
  nested: keep-me # keep comment
openai-compatibility:
  - name: axis
    base-url: https://axis.example/v1
    headers:
      User-Agent: metadata-client
    models:
      - name: gpt-x # model comment
        alias: public-gpt
        display-name: GPT X
        max-context-length: 111000 # context comment
        max-input-tokens: 90000
        max-output-tokens: 16000
        thinking:
          min: 1024 # preserve thinking budget
          max: 32768
          levels: [max] # preserve levels comment
        input-modalities: [text]
        output-modalities: [text]
      - name: untouched
        alias: untouched-alias
claude-api-key:
  - api-key: secret-not-used
    base-url: https://api.anthropic.com
    prefix: anthropic
    models:
      - name: claude-x
        alias: claude-public
      - name: claude-y
        alias: claude-y-public
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

func testConfig(t *testing.T) runtimeConfig {
	t.Helper()
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	cfg, err := parseConfig(validConfigYAML())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Generation, cfg.AttemptID = 1, "test-attempt"
	return cfg
}

func initialRequest(t *testing.T, version string, snapshot []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": version, "config_base64": base64.StdEncoding.EncodeToString(snapshot)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func continuationRequest(t *testing.T, version string, snapshot []byte, descriptor fetchDescriptor, body []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": version, "config_base64": base64.StdEncoding.EncodeToString(snapshot), "continuation_base64": descriptor.ContinuationBase64, "fetch_result": map[string]any{"request_id": descriptor.RequestID, "status_code": 200, "body_base64": base64.StdEncoding.EncodeToString(body)}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func advance(t *testing.T, cfg runtimeConfig, host AuthHost, version string, snapshot []byte, result any, body []byte) any {
	t.Helper()
	descriptor := result.(fetchEnvelope).NextFetch
	next, code := plan(continuationRequest(t, version, snapshot, descriptor, body), cfg, host)
	if code != "" {
		t.Fatalf("continuation kind=%s code=%s", descriptor.Kind, code)
	}
	return next
}

func TestPlanAllSelectorsMetadataOnlyAndPreservesDocument(t *testing.T) {
	cfg, host := testConfig(t), authFixture()
	version, snapshot := strings.Repeat("a", 64), snapshotYAML()
	result, code := plan(initialRequest(t, version, snapshot), cfg, host)
	if code != "" {
		t.Fatal(code)
	}
	first := result.(fetchEnvelope).NextFetch
	if first.Kind != "openai_models" || first.URL != "https://axis.example/v1/models?client_version=1.0.0" || first.AuthIndex != "openai-auth" || first.Header["Authorization"] != "Bearer $TOKEN$" {
		t.Fatalf("first=%+v", first)
	}
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"models":[{"slug":"gpt-x","context_window":256000,"max_tokens":64000,"input_modalities":["text"],"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`))
	if descriptor := result.(fetchEnvelope).NextFetch; descriptor.Kind != "modelparams" || descriptor.AuthIndex != "" || descriptor.URL != defaultModelparamsURL {
		t.Fatalf("modelparams=%+v", descriptor)
	}
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"models":[{"provider":"openai","authType":"subscription","model":"gpt-x","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high","max"]},{"path":"max_tokens","group":"generation_length","type":"integer","range":{"max":32000}}]}]}`))
	if descriptor := result.(fetchEnvelope).NextFetch; descriptor.Kind != "modelsdev" || descriptor.URL != defaultModelsdevURL {
		t.Fatalf("modelsdev=%+v", descriptor)
	}
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"openai":{"models":{"gpt-x":{"id":"gpt-x","limit":{"context":128000,"output":64000},"modalities":{"input":["text","image"],"output":["text"]}}}}}`))
	claude := result.(fetchEnvelope).NextFetch
	if claude.Kind != "claude_models" || claude.AuthIndex != "claude-auth" || claude.URL != "https://api.anthropic.com/v1/models?limit=1000" || claude.Header["x-api-key"] != "$TOKEN$" || claude.Header["anthropic-version"] != "2023-06-01" {
		t.Fatalf("claude=%+v", claude)
	}
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"data":[{"id":"claude-x","max_input_tokens":200000,"max_tokens":8192,"input_modalities":["text","image","document"],"supported_efforts":["low","high"]}],"last_id":"claude-x","has_more":true}`))
	page2 := result.(fetchEnvelope).NextFetch
	if page2.URL != "https://api.anthropic.com/v1/models?after_id=claude-x&limit=1000" {
		t.Fatalf("page2=%+v", page2)
	}
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"data":[{"id":"claude-y"}],"last_id":"claude-y","has_more":false}`))
	if intermediate, ok := result.(fetchEnvelope); ok {
		t.Fatalf("unexpected extra fetch: %+v", intermediate.NextFetch)
	}
	final := result.(finalEnvelope)
	if !final.Report.Changed || len(final.Report.Channels) != 2 {
		t.Fatalf("report=%+v", final.Report)
	}
	proposed, err := base64.StdEncoding.Strict().DecodeString(final.ConfigBase64)
	if err != nil {
		t.Fatal(err)
	}
	text := string(proposed)
	for _, expected := range []string{"# root comment", "keep-me # keep comment", "name: gpt-x # model comment", "alias: public-gpt", "display-name: GPT X", "max-context-length: 111000 # context comment", "max-input-tokens: 90000", "max-output-tokens: 120000", "min: 1024 # preserve thinking budget", "max: 32768", "levels: [low, high] # preserve levels comment", "name: untouched", "untouched-alias", "name: claude-x", "alias: claude-public", "max-context-length: 200000", "max-input-tokens: 200000", "max-output-tokens: 8192", "input-modalities: [text, image]", "name: claude-y", "claude-y-public", "root-secret"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q\n%s", expected, text)
		}
	}
	if strings.Contains(text, "document") {
		t.Fatalf("unsupported modality leaked\n%s", text)
	}
	doc, err := parseSnapshot(proposed)
	if err != nil {
		t.Fatal(err)
	}
	open, err := locateSnapshotChannel(doc, cfg.Channels[0])
	if err != nil {
		t.Fatal(err)
	}
	models, err := namedModels(open.Models)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models["gpt-x"] == nil || models["untouched"] == nil {
		t.Fatalf("membership changed: %v", models)
	}
}

func TestClaudeCursorAndHundredPageBound(t *testing.T) {
	state := continuationState{Step: fetchStep{Kind: "claude_models"}, UpstreamByID: map[string]upstreamEntry{}}
	for page := 0; page < maxClaudePages-1; page++ {
		body := fmt.Appendf(nil, `{"data":[],"last_id":"cursor-%d","has_more":true}`, page)
		if code := consumeFetch(&state, body); code != "" {
			t.Fatalf("page %d code=%s", page+1, code)
		}
	}
	if state.Step.Page != maxClaudePages-1 {
		t.Fatalf("page index=%d", state.Step.Page)
	}
	finalState := state
	if code := consumeFetch(&finalState, []byte(`{"data":[],"last_id":"final","has_more":false}`)); code != "" {
		t.Fatalf("hundredth final page code=%s", code)
	}
	if code := consumeFetch(&state, []byte(`{"data":[],"last_id":"overflow","has_more":true}`)); code != errorInvalid {
		t.Fatalf("page 101 request code=%s", code)
	}
	repeated := continuationState{Step: fetchStep{Kind: "claude_models", Page: 1, AfterID: "same"}, UpstreamByID: map[string]upstreamEntry{}, ClaudeCursors: map[string]bool{"same": true}}
	if code := consumeFetch(&repeated, []byte(`{"data":[],"last_id":"same","has_more":true}`)); code != errorInvalid {
		t.Fatalf("repeated cursor code=%s", code)
	}
	cycle := continuationState{Step: fetchStep{Kind: "claude_models"}, UpstreamByID: map[string]upstreamEntry{}, ClaudeCursors: map[string]bool{}}
	for _, cursor := range []string{"a", "b"} {
		if code := consumeFetch(&cycle, fmt.Appendf(nil, `{"data":[],"last_id":%q,"has_more":true}`, cursor)); code != "" {
			t.Fatalf("cursor %s code=%s", cursor, code)
		}
	}
	if code := consumeFetch(&cycle, []byte(`{"data":[],"last_id":"a","has_more":true}`)); code != errorInvalid {
		t.Fatalf("non-adjacent cursor cycle code=%s", code)
	}
	duplicates := continuationState{Step: fetchStep{Kind: "claude_models"}, UpstreamByID: map[string]upstreamEntry{}, ClaudeCursors: map[string]bool{}}
	if code := consumeFetch(&duplicates, []byte(`{"data":[{"id":"same-model"}],"last_id":"first","has_more":true}`)); code != "" {
		t.Fatalf("first duplicate page code=%s", code)
	}
	if code := consumeFetch(&duplicates, []byte(`{"data":[{"id":"same-model"}],"last_id":"second","has_more":false}`)); code != errorInvalid {
		t.Fatalf("cross-page duplicate code=%s", code)
	}
}

func TestPublicCatalogsSurviveContinuationRoundTrips(t *testing.T) {
	cfg, host := testConfig(t), authFixture()
	cfg.Channels = cfg.Channels[:1]
	cfg.Channels[0].Overrides = map[string]ModelOverride{}
	version := strings.Repeat("e", 64)
	snapshot := []byte(strings.Replace(string(snapshotYAML()), "        output-modalities: [text]\n", "", 1))
	result, code := plan(initialRequest(t, version, snapshot), cfg, host)
	if code != "" {
		t.Fatal(code)
	}
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"models":[{"slug":"gpt-x","max_tokens":48000}]}`))
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"models":[{"provider":"openai","authType":"subscription","model":"gpt-x","params":[{"path":"max_tokens","group":"generation_length","type":"integer","range":{"max":32000}}]}]}`))
	result = advance(t, cfg, host, version, snapshot, result, []byte(`{"openai":{"models":{"gpt-x":{"id":"gpt-x","limit":{"output":64000},"modalities":{"output":["text"]}}}}}`))
	final, ok := result.(finalEnvelope)
	if !ok {
		t.Fatalf("result=%T", result)
	}
	proposed, err := base64.StdEncoding.Strict().DecodeString(final.ConfigBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(proposed), "max-output-tokens: 32000") || !strings.Contains(string(proposed), "output-modalities: [text]") {
		t.Fatalf("public metadata was lost across continuation:\n%s", proposed)
	}
	fields := map[string]MetadataFieldResult{}
	for _, field := range final.Report.Channels[0].Metadata[0].Fields {
		fields[field.Field] = field
	}
	if fields["max-output-tokens"].Source != "modelparams.dev/openai/subscription" || fields["output-modalities"].Source != "models.dev/openai" {
		t.Fatalf("provenance=%+v", fields)
	}
}

func TestOpenAIUpstreamMaxInputIsIgnoredForLegacyParity(t *testing.T) {
	entries, err := parseOpenAICatalog([]byte(`{"models":[{"slug":"m","max_input_tokens":99999,"context_window":128000,"max_tokens":32000}]}`))
	if err != nil {
		t.Fatal(err)
	}
	models := []ModelRef{{Name: "m"}}
	reports, _, _ := enrichModels(models, map[string]upstreamEntry{"m": entries[0]}, compiledChannel{Kind: KindOpenAI, UpstreamMeta: true}, nil, nil)
	if models[0].MaxInputTokens != 0 {
		t.Fatalf("max input changed: %+v", models[0])
	}
	var field MetadataFieldResult
	for _, candidate := range reports[0].Fields {
		if candidate.Field == "max-input-tokens" {
			field = candidate
		}
	}
	if field.Source != "" || field.Status != "skipped" || field.Reason != "no automatic source supports this field; preserve an existing value or use manual override" || len(field.TriedSources) != 0 {
		t.Fatalf("field=%+v", field)
	}
}

func TestGoldenMetadataPrecedenceAndProvenance(t *testing.T) {
	modelparams, _ := parseModelparamsCatalog([]byte(`{"models":[{"provider":"openai","authType":"subscription","model":"gpt-x","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high","max"]},{"path":"max_tokens","group":"generation_length","type":"integer","range":{"max":32000}}]}]}`))
	modelsdev, _ := parseModelsdevCatalog([]byte(`{"openai":{"models":{"gpt-x":{"id":"gpt-x","limit":{"context":128000,"output":64000},"modalities":{"input":["text","image"],"output":["text"]}}}}}`))
	spec := compiledChannel{Kind: KindOpenAI, UpstreamMeta: true, MetadataSources: []metadataSource{{ID: "modelparams.dev/openai/subscription", Website: "modelparams.dev", Provider: "openai", AuthType: "subscription"}, {ID: "models.dev/openai", Website: "models.dev", Provider: "openai"}}, Overrides: map[string]ModelOverride{"gpt-x": {MaxContextLength: 256000, MaxInputTokens: 240000}}}
	models := []ModelRef{{Name: "gpt-x", Thinking: &ThinkingConfig{Levels: []string{"max"}}, MaxOutputTokens: 16000}}
	reports, _, _ := enrichModels(models, map[string]upstreamEntry{"gpt-x": {ID: "gpt-x", Efforts: []string{"low", "high"}, Context: 200000, MaxTokens: 48000, Input: []string{"text"}}}, spec, modelparams, modelsdev)
	if strings.Join(models[0].Thinking.Levels, ",") != "low,high" || models[0].MaxContextLength != 256000 || models[0].MaxInputTokens != 240000 || models[0].MaxOutputTokens != 32000 || strings.Join(models[0].InputModalities, ",") != "text" {
		t.Fatalf("model=%+v", models[0])
	}
	fields := map[string]MetadataFieldResult{}
	for _, field := range reports[0].Fields {
		fields[field.Field] = field
	}
	checks := map[string]struct {
		source, status, reason, tried string
	}{
		"thinking.levels":    {"upstream /models", "upstream", "supplied by enabled upstream metadata", "upstream /models"},
		"max-context-length": {"manual override", "override", "explicit per-model override applied last", "upstream /models"},
		"max-input-tokens":   {"manual override", "override", "explicit per-model override applied last", ""},
		"max-output-tokens":  {"modelparams.dev/openai/subscription", "authoritative", "concrete generation-length range.max from authoritative source", "modelparams.dev/openai/subscription"},
		"input-modalities":   {"upstream /models", "upstream", "supplied by enabled upstream metadata", "upstream /models"},
		"output-modalities":  {"models.dev/openai", "completed", "secondary source filled a field missing from authoritative and earlier sources", "upstream /models,models.dev/openai"},
	}
	for name, want := range checks {
		got := fields[name]
		if got.Source != want.source || got.Status != want.status || got.Reason != want.reason || strings.Join(got.TriedSources, ",") != want.tried {
			t.Fatalf("%s=%+v want=%+v", name, got, want)
		}
	}
}
