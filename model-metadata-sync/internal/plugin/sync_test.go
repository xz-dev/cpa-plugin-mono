package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type metadataTransport struct {
	t               *testing.T
	claudePages     int
	duplicateOpenAI bool
	requests        []struct {
		method, url string
		body        []byte
	}
	modelparamsError error
}

func (transport *metadataTransport) Do(method, url string, headers http.Header, body []byte) (int, []byte, error) {
	transport.requests = append(transport.requests, struct {
		method, url string
		body        []byte
	}{method, url, append([]byte(nil), body...)})
	if url == defaultModelparamsURL {
		if transport.modelparamsError != nil {
			return 0, nil, transport.modelparamsError
		}
		return http.StatusOK, []byte(`{"models":[{"provider":"openai","authType":"subscription","model":"gpt-x","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high","max"]}]}]}`), nil
	}
	if url == defaultModelsdevURL {
		return http.StatusOK, []byte(`{"openai":{"models":{"gpt-x":{"id":"gpt-x","limit":{"context":128000,"output":32000},"modalities":{"input":["text","image"],"output":["text"]}}}}}`), nil
	}
	if headers.Get("Authorization") != "Bearer management-key" {
		transport.t.Fatalf("authorization=%q", headers.Get("Authorization"))
	}
	switch {
	case method == http.MethodGet && strings.HasSuffix(url, "/v0/management/model-channels"):
		if transport.duplicateOpenAI {
			return http.StatusOK, []byte(`{"channels":[{"kind":"openai-compatibility","selector":{"name":"元流","base_url":"https://axis.example/v1"},"disabled":false,"ready":true,"revision":"or1","models":[{"name":"gpt-x"},{"name":"gpt-x"}]}]}`), nil
		}
		return http.StatusOK, []byte(`{"channels":[
			{"kind":"openai-compatibility","selector":{"name":"元流","base_url":"https://axis.example/v1"},"disabled":false,"ready":true,"revision":"or1","models":[{"name":"gpt-x","alias":"public","display_name":"GPT X","max_context_length":111000,"max_input_tokens":90000,"max_output_tokens":16000,"thinking":{"levels":["max"]},"input_modalities":["text"],"output_modalities":["text"]}]},
			{"kind":"claude","selector":{"config_index":2,"base_url":"https://api.anthropic.com","prefix":"anthropic"},"disabled":false,"ready":true,"revision":"cl1","models":[{"name":"claude-x","alias":"claude-public"},{"name":"claude-y"}]}
		]}`), nil
	case method == http.MethodPost && strings.HasSuffix(url, "/v0/management/model-channels/catalog"):
		var request catalogRequest
		if err := json.Unmarshal(body, &request); err != nil {
			transport.t.Fatal(err)
		}
		if request.ExpectedRevision == "" {
			transport.t.Fatal("catalog request missing expected_revision")
		}
		if request.Profile == "openai_models" {
			if request.Kind != KindOpenAI || request.ExpectedRevision != "or1" {
				transport.t.Fatalf("openai catalog request=%+v", request)
			}
			response, _ := json.Marshal(catalogResponse{StatusCode: 200, Body: json.RawMessage(`{"models":[{"slug":"gpt-x","context_window":256000,"max_tokens":64000,"input_modalities":["text"],"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`)})
			return 200, response, nil
		}
		transport.claudePages++
		if request.Kind != KindClaude || request.ExpectedRevision != "cl1" || request.Query == nil || request.Query.Limit != 1000 {
			transport.t.Fatalf("claude query=%+v", request.Query)
		}
		var payload string
		if request.Query.AfterID == "" {
			payload = `{"data":[{"id":"claude-x","max_input_tokens":200000,"max_tokens":8192,"input_modalities":["text","image","document"],"output_modalities":["text"],"supported_efforts":["low","high"]}],"last_id":"claude-x","has_more":true}`
		} else if request.Query.AfterID == "claude-x" {
			payload = `{"data":[{"id":"claude-y"}],"last_id":"claude-y","has_more":false}`
		} else {
			transport.t.Fatalf("after_id=%q", request.Query.AfterID)
		}
		response, _ := json.Marshal(catalogResponse{StatusCode: 200, Body: json.RawMessage(payload)})
		return 200, response, nil
	case method == http.MethodPatch && strings.HasSuffix(url, "/v0/management/model-channels/metadata"):
		return http.StatusOK, []byte(`{"status":"ok"}`), nil
	default:
		transport.t.Fatalf("unexpected %s %s", method, url)
		return 0, nil, nil
	}
}

func metadataConfig(t *testing.T) *runtimeConfig {
	index := 2
	cfg, err := parseFileConfig([]byte(`{
		"interval":"0","management_base_url":"http://cpa:8317",
		"channels":[
			{"enabled":true,"kind":"openai-compatibility","selector":{"name":"元流","base_url":"https://axis.example/v1"},"upstream_meta":true,"codex_manifest":true,"metadata_sources":["modelparams.dev/openai/subscription","models.dev/openai"],"overrides":{"gpt-x":{"max_output_tokens":120000}}},
			{"enabled":true,"kind":"claude","selector":{"config_index":2,"base_url":"https://api.anthropic.com","prefix":"anthropic"},"upstream_meta":true,"profile":"claude_models"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels[1].Selector.ConfigIndex == nil || *cfg.Channels[1].Selector.ConfigIndex != index {
		t.Fatal("claude selector")
	}
	return &cfg
}

func configuredMetadataService(t *testing.T, transport Transport) *Service {
	service := New(transport)
	service.jsonPath = filepath.Join(t.TempDir(), "config.json")
	service.cfg = *metadataConfig(t)
	return service
}

func TestMetadataCatalogBodyAcceptsStringAndRawJSON(t *testing.T) {
	encoded, _ := json.Marshal(`{"data":[{"id":"string"}]}`)
	for name, raw := range map[string]json.RawMessage{
		"string": encoded,
		"raw":    json.RawMessage(`{"data":[{"id":"raw"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			entries, err := parseOpenAICatalog(catalogBody(raw))
			if err != nil || len(entries) != 1 || entries[0].ID != name {
				t.Fatalf("entries=%+v err=%v", entries, err)
			}
		})
	}
}

func TestMetadataSyncUsesSanitizedPatchOnly(t *testing.T) {
	transport := &metadataTransport{t: t}
	service := configuredMetadataService(t, transport)
	report := service.Sync("management-key", "")
	if !report.OK || len(report.Channels) != 2 || transport.claudePages != 2 {
		t.Fatalf("report=%+v pages=%d", report, transport.claudePages)
	}
	var patches []metadataPatchRequest
	for _, request := range transport.requests {
		if request.method == http.MethodPatch {
			var patch metadataPatchRequest
			if err := json.Unmarshal(request.body, &patch); err != nil {
				t.Fatal(err)
			}
			patches = append(patches, patch)
		}
	}
	if len(patches) != 2 {
		t.Fatalf("patches=%d", len(patches))
	}
	openai := patches[0]
	if openai.Kind != KindOpenAI || openai.ExpectedRevision != "or1" || strings.Join(openai.ExpectedModelNames, ",") != "gpt-x" || len(openai.Operations) != 1 || openai.Operations[0].Model != "gpt-x" {
		t.Fatalf("openai patch=%+v", openai)
	}
	fields := openai.Operations[0].Fields
	if fields["thinking.levels"].Mode != "replace" || fields["max-output-tokens"].Mode != "replace" {
		t.Fatalf("fields=%+v", fields)
	}
	if _, exists := fields["max-context-length"]; exists {
		t.Fatalf("preserved max context must not be patched: %+v", fields["max-context-length"])
	}
	if _, exists := fields["max-input-tokens"]; exists {
		t.Fatalf("preserved max input must not be patched: %+v", fields["max-input-tokens"])
	}
	claude := patches[1]
	if claude.Kind != KindClaude || claude.ExpectedRevision != "cl1" || len(claude.Operations) != 1 || claude.Operations[0].Model != "claude-x" {
		t.Fatalf("claude patch=%+v", claude)
	}
	cf := claude.Operations[0].Fields
	if cf["max-input-tokens"].Mode != "replace" || cf["max-context-length"].Value != float64(200000) && cf["max-context-length"].Value != 200000 {
		t.Fatalf("claude fields=%+v", cf)
	}
	if _, exists := cf["output-modalities"]; exists {
		t.Fatalf("claude output modality must be ignored: %+v", cf["output-modalities"])
	}
	if got := cf["input-modalities"].Value; strings.Contains(strings.ToLower(strings.TrimSpace(toJSON(got))), "document") {
		t.Fatalf("invented/unsupported modality: %v", got)
	}
	for _, request := range transport.requests {
		raw := string(request.body)
		for _, forbidden := range []string{"api-key", "auth-index", "proxy-url", "headers"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("secret DTO field %q in %s", forbidden, raw)
			}
		}
	}
}

func TestDecodeCoreShapedDescriptorPreservesRichFields(t *testing.T) {
	raw := []byte(`{"channels":[{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"revision":"r1","models":[{"name":"m","alias":"public","display_name":"Model M","max_context_length":128000,"max_input_tokens":120000,"max_output_tokens":32000,"thinking":{"levels":["low","high"]},"input_modalities":["text","image"],"output_modalities":["text"]}]}]}`)
	channels, err := decodeModelChannels(raw)
	if err != nil {
		t.Fatal(err)
	}
	model := channels[0].Models[0]
	if model.DisplayName != "Model M" || model.MaxContextLength != 128000 || model.MaxInputTokens != 120000 || model.MaxOutputTokens != 32000 || strings.Join(model.Thinking.Levels, ",") != "low,high" || strings.Join(model.InputModalities, ",") != "text,image" || strings.Join(model.OutputModalities, ",") != "text" {
		t.Fatalf("model=%+v", model)
	}
	reports, _, _ := enrichModels(channels[0].Models, nil, compiledChannel{}, nil, nil, nil, nil)
	patches, err := buildModelPatches(channels[0].Models, cloneModels(channels[0].Models), reports)
	if err != nil || len(patches) != 0 {
		t.Fatalf("patches=%+v err=%v", patches, err)
	}
}

func TestOpenAIUpstreamMaxInputIsIgnoredForLegacyParity(t *testing.T) {
	entries, err := parseOpenAICatalog([]byte(`{"models":[{"slug":"m","max_input_tokens":99999,"context_window":128000,"max_tokens":32000}]}`))
	if err != nil {
		t.Fatal(err)
	}
	models := []ModelRef{{Name: "m"}}
	reports, _, _ := enrichModels(models, map[string]upstreamEntry{"m": entries[0]}, compiledChannel{Kind: KindOpenAI, UpstreamMeta: true}, nil, nil, nil, nil)
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
	spec := compiledChannel{UpstreamMeta: true, MetadataSources: []metadataSource{{ID: "modelparams.dev/openai/subscription", Website: "modelparams.dev", Provider: "openai", AuthType: "subscription"}, {ID: "models.dev/openai", Website: "models.dev", Provider: "openai"}}, Overrides: map[string]ModelOverride{"gpt-x": {MaxContextLength: 256000, MaxInputTokens: 240000}}}
	models := []ModelRef{{Name: "gpt-x", Thinking: &ThinkingConfig{Levels: []string{"max"}}, MaxOutputTokens: 16000}}
	reports, _, _ := enrichModels(models, map[string]upstreamEntry{"gpt-x": {ID: "gpt-x", Efforts: []string{"low", "high"}, Context: 200000, MaxTokens: 48000, Input: []string{"text"}}}, spec, modelparams, nil, modelsdev, nil)
	if strings.Join(models[0].Thinking.Levels, ",") != "low,high" || models[0].MaxContextLength != 256000 || models[0].MaxInputTokens != 240000 || models[0].MaxOutputTokens != 32000 || strings.Join(models[0].InputModalities, ",") != "text" {
		t.Fatalf("thinking=%v context=%d input=%d output=%d modalities=%v model=%+v", models[0].Thinking.Levels, models[0].MaxContextLength, models[0].MaxInputTokens, models[0].MaxOutputTokens, models[0].InputModalities, models[0])
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
	for field, want := range checks {
		got := fields[field]
		if got.Source != want.source || got.Status != want.status || got.Reason != want.reason || strings.Join(got.TriedSources, ",") != want.tried {
			t.Fatalf("%s=%+v want=%+v", field, got, want)
		}
	}
}

func TestClaudeExplicitOnlyMappingAndPagination(t *testing.T) {
	transport := &metadataTransport{t: t}
	service := configuredMetadataService(t, transport)
	channels, err := service.listModelChannels("management-key")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := matchChannel(channels, service.cfg.Channels[1])
	if err != nil {
		t.Fatal(err)
	}
	entries, err := service.fetchChannelCatalog("management-key", channel, service.cfg.Channels[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != "claude-x" || entries[0].Context != 200000 || entries[0].ClaudeMaxInput != 200000 || entries[0].MaxTokens != 8192 || strings.Join(entries[0].Input, ",") != "text,image" || len(entries[0].Output) != 0 || entries[1].Context != 0 || entries[1].Efforts != nil {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestPreviewRoutesAndMetadataSourceErrors(t *testing.T) {
	transport := &metadataTransport{t: t, modelparamsError: errors.New("offline")}
	service := configuredMetadataService(t, transport)
	response := service.HandleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/model-metadata-sync/metadata-sources"})
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "models.dev/openai") || !strings.Contains(string(response.Body), "offline") {
		t.Fatalf("sources=%d %s", response.StatusCode, response.Body)
	}
	response = service.HandleManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/model-metadata-sync/preview", Headers: http.Header{"Authorization": []string{"Bearer management-key"}}})
	if !strings.Contains(string(response.Body), `"dry_run":true`) {
		t.Fatalf("preview=%d %s", response.StatusCode, response.Body)
	}
	for _, request := range transport.requests {
		if request.method == http.MethodPatch {
			t.Fatal("preview wrote metadata")
		}
	}
}

func TestNormalizeBaseURLMatchesCoreCanonicalization(t *testing.T) {
	for _, testCase := range []struct {
		raw, want string
	}{
		{" HTTPS://Example.COM:443/v1/?tenant=A ", "https://example.com/v1?tenant=A"},
		{"http://Example.COM:80/", "http://example.com"},
		{"https://[2001:DB8::1]:443/v1/", "https://[2001:db8::1]/v1"},
		{"http://[2001:DB8::1]:8080/v1", "http://[2001:db8::1]:8080/v1"},
	} {
		got, err := normalizeBaseURL(testCase.raw)
		if err != nil || got != testCase.want {
			t.Fatalf("normalizeBaseURL(%q)=%q, %v; want %q", testCase.raw, got, err, testCase.want)
		}
	}
	for _, invalid := range []string{"https://user:pass@example.com", "https://example.com/#fragment", "ftp://example.com"} {
		if _, err := normalizeBaseURL(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestDefaultConfigureRemainsPaused(t *testing.T) {
	transport := &metadataTransport{t: t}
	service := New(transport)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := service.Configure([]byte(`{"config_file":"` + path + `"}`)); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown()
	time.Sleep(20 * time.Millisecond)
	if service.Current().Interval != 0 || len(transport.requests) != 0 {
		t.Fatalf("interval=%v requests=%d", service.Current().Interval, len(transport.requests))
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"interval": "0"`) {
		t.Fatalf("config=%s err=%v", raw, err)
	}
}

func TestProductionExampleSourceRules(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseFileConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 0 || cfg.Raw.Interval != "0" {
		t.Fatalf("example must install paused: interval=%v raw=%q", cfg.Interval, cfg.Raw.Interval)
	}
	byName := map[string]compiledChannel{}
	for _, channel := range cfg.Channels {
		byName[channel.Selector.Name] = channel
	}
	if got := sourceIDs(byName["元流"].MetadataSources); strings.Join(got, ",") != "modelparams.dev/openai/subscription,models.dev/openai" || len(byName["元流"].Overrides) != 0 {
		t.Fatalf("元流=%+v", byName["元流"])
	}
	if got := sourceIDs(byName["Ollama Cloud"].MetadataSources); strings.Join(got, ",") != "models.dev/ollama-cloud" || len(byName["Ollama Cloud"].Overrides) != 0 {
		t.Fatalf("Ollama Cloud=%+v", byName["Ollama Cloud"])
	}
	zcode := byName["ZCode"]
	if !zcode.UpstreamMeta || !zcode.CodexManifest || len(zcode.MetadataSources) != 0 || len(zcode.Overrides) != 0 {
		t.Fatalf("ZCode=%+v", zcode)
	}
}

func sourceIDs(sources []metadataSource) []string {
	out := make([]string, len(sources))
	for i, source := range sources {
		out[i] = source.ID
	}
	return out
}

func TestConfigRejectsUnsafeSelectorsSourcesAndModalities(t *testing.T) {
	for name, raw := range map[string]string{"old providers": `{"providers":{"ZCode":{"upstream_meta":true}}}`, "openai name only": `{"channels":[{"kind":"openai-compatibility","selector":{"name":"ZCode"}}]}`, "claude no index": `{"channels":[{"kind":"claude","selector":{"base_url":"https://api.anthropic.com","prefix":"x"}}]}`, "source order": `{"channels":[{"kind":"openai-compatibility","selector":{"name":"x","base_url":"https://x.example/v1"},"metadata_sources":["models.dev/openai","modelparams.dev/openai/subscription"]}]}`, "bad modality": `{"channels":[{"kind":"openai-compatibility","selector":{"name":"x","base_url":"https://x.example/v1"},"overrides":{"m":{"input_modalities":["document"]}}}]}`} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFileConfig([]byte(raw)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestDuplicateDescriptorFailsChannelBeforePatch(t *testing.T) {
	transport := &metadataTransport{t: t, duplicateOpenAI: true}
	service := configuredMetadataService(t, transport)
	service.cfg.Channels = service.cfg.Channels[:1]
	report := service.Sync("management-key", "")
	if report.OK || len(report.Channels) != 1 || !strings.Contains(report.Channels[0].Error, "ambiguous") {
		t.Fatalf("report=%+v", report)
	}
	for _, request := range transport.requests {
		if request.method == http.MethodPatch {
			t.Fatal("duplicate inventory sent metadata patch")
		}
	}
}

func TestMissingRevisionFailsBeforeCatalogOrPatch(t *testing.T) {
	transport := &metadataTransport{t: t}
	service := configuredMetadataService(t, transport)
	channel := ModelChannelDescriptor{Kind: KindOpenAI, Selector: service.cfg.Channels[0].Selector}
	if _, err := service.fetchChannelCatalog("management-key", channel, service.cfg.Channels[0]); err == nil {
		t.Fatal("catalog accepted empty revision")
	}
	if err := service.patchMetadata("management-key", channel, []ModelPatch{{Model: "m", Fields: map[string]FieldPatch{"max-output-tokens": {Mode: "replace", Value: 1}}}}); err == nil {
		t.Fatal("patch accepted empty revision")
	}
}

func TestDecodeModelChannelsRejectsLegacySecretDTO(t *testing.T) {
	if _, err := decodeModelChannels([]byte(`{"openai-compatibility":[{"api-key-entries":[{"api-key":"secret"}]}]}`)); err == nil {
		t.Fatal("legacy DTO accepted")
	}
}

func toJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }
