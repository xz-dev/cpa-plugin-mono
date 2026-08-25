package plugin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type codexSyncTransport struct {
	t               *testing.T
	compatJSON      string
	manifest        string
	manifests       map[string]string
	modelparams     string
	modelsdev       string
	modelparamsHits int
	modelsdevHits   int
	apiCallURL      string
	patched         []ModelRef
}

func (t *codexSyncTransport) Do(method, url string, _ http.Header, body []byte) (int, []byte, error) {
	switch {
	case method == http.MethodGet && strings.HasSuffix(url, "/v0/management/openai-compatibility"):
		if t.compatJSON != "" {
			return http.StatusOK, []byte(t.compatJSON), nil
		}
		return http.StatusOK, []byte(`{"openai-compatibility":[{"name":"ZCode","base-url":"http://zcode-proxy:8080/v1","api-key-entries":[{"auth-index":"zcode"}],"models":[]}]}`), nil
	case method == http.MethodPost && strings.HasSuffix(url, "/v0/management/api-call"):
		var req apiCallRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.t.Fatal(err)
		}
		t.apiCallURL = req.URL
		manifest := t.manifest
		if configured, ok := t.manifests[req.URL]; ok {
			manifest = configured
		}
		response, err := json.Marshal(apiCallResponse{StatusCode: http.StatusOK, Body: manifest})
		if err != nil {
			t.t.Fatal(err)
		}
		return http.StatusOK, response, nil
	case method == http.MethodGet && url == defaultModelparamsURL:
		t.modelparamsHits++
		return http.StatusOK, []byte(t.modelparams), nil
	case method == http.MethodGet && url == defaultModelsdevURL:
		t.modelsdevHits++
		return http.StatusOK, []byte(t.modelsdev), nil
	case method == http.MethodPatch && strings.HasSuffix(url, "/v0/management/openai-compatibility"):
		var payload struct {
			Value struct {
				Models []ModelRef `json:"models"`
			} `json:"value"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.t.Fatal(err)
		}
		t.patched = payload.Value.Models
		return http.StatusOK, []byte(`{}`), nil
	default:
		t.t.Fatalf("unexpected request: %s %s", method, url)
		return 0, nil, nil
	}
}

func TestSyncConsumesCodexManifest(t *testing.T) {
	transport := &codexSyncTransport{
		t: t,
		manifest: `{"models":[
			{"slug":"glm-5.3","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]},
			{"slug":"glm-5.3[1m]","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]}
		]}`,
	}
	service := New(transport)
	service.jsonPath = filepath.Join(t.TempDir(), "config.json")
	service.cfg = runtimeConfig{
		ManagementBaseURL:   "http://cpa:8317",
		KeepExistingAliases: true,
		Providers: []compiledProvider{{
			Name:          "ZCode",
			Enabled:       true,
			Mode:          ModeExclude,
			UpstreamMeta:  true,
			CodexManifest: true,
		}},
	}

	report := service.Sync("management-key", "ZCode")
	if !report.OK || len(report.Providers) != 1 || report.Providers[0].ThinkingMatched != 2 {
		t.Fatalf("report=%+v", report)
	}
	if transport.apiCallURL != "http://zcode-proxy:8080/v1/models?client_version=1.0.0" {
		t.Fatalf("api-call URL=%s", transport.apiCallURL)
	}
	if len(transport.patched) != 2 {
		t.Fatalf("patched=%v", transport.patched)
	}
	for _, model := range transport.patched {
		if model.Thinking == nil || strings.Join(model.Thinking.Levels, ",") != "low,high,max" {
			t.Fatalf("model=%+v", model)
		}
	}
}

func TestSyncAppliesOverridesAfterUpstreamMetadata(t *testing.T) {
	transport := &codexSyncTransport{
		t: t,
		manifest: `{"models":[
			{"slug":"glm-5.3","context_window":262144,"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}
		]}`,
	}
	service := New(transport)
	service.jsonPath = filepath.Join(t.TempDir(), "config.json")
	service.cfg = runtimeConfig{
		ManagementBaseURL:   "http://cpa:8317",
		KeepExistingAliases: true,
		Providers: []compiledProvider{{
			Name:          "ZCode",
			Enabled:       true,
			Mode:          ModeExclude,
			UpstreamMeta:  true,
			CodexManifest: true,
			Overrides: map[string]ModelOverride{
				"glm-5.3": {MaxContextLength: 1050000, MaxInputTokens: 1000000, MaxOutputTokens: 50000, ThinkingLevels: []string{"none", "medium", "max"}},
			},
		}},
	}

	report := service.Sync("management-key", "ZCode")
	if !report.OK || len(transport.patched) != 1 {
		t.Fatalf("report=%+v patched=%v", report, transport.patched)
	}
	model := transport.patched[0]
	if model.MaxContextLength != 1050000 || model.MaxInputTokens != 1000000 || model.MaxOutputTokens != 50000 {
		t.Fatalf("token limits=%+v", model)
	}
	if got := strings.Join(model.Thinking.Levels, ","); got != "none,medium,max" {
		t.Fatalf("thinking=%s", got)
	}
}

func TestPreviewReportsFieldSourcesAndSecondaryCompletion(t *testing.T) {
	transport := &codexSyncTransport{
		t:        t,
		manifest: `{"data":[{"id":"gpt-x"}]}`,
		modelparams: `{"models":[
			{"provider":"openai","authType":"subscription","model":"gpt-x","params":[
				{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high","max"]}
			]},
			{"provider":"openai","authType":"api_key","model":"gpt-x","params":[
				{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]}
			]}
		]}`,
		modelsdev: `{"openrouter":{"models":{"vendor/gpt-x":{
			"id":"gpt-x","limit":{"context":128000,"output":16384}
		}}}}`,
	}
	service := New(transport)
	service.jsonPath = filepath.Join(t.TempDir(), "config.json")
	service.cfg = runtimeConfig{ManagementBaseURL: "http://cpa:8317"}

	report, err := service.PreviewWithKey("management-key", "ZCode", []byte(`{
		"management_base_url":"http://cpa:8317",
		"providers":{"ZCode":{
			"enabled":true,
			"mode":"exclude",
			"metadata_sources":[
				"modelparams.dev/openai/subscription",
				"modelparams.dev/openai/api_key",
				"models.dev/openrouter"
			]
		}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || len(report.Providers) != 1 {
		t.Fatalf("report=%+v", report)
	}

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Providers []struct {
			Metadata []struct {
				Model  string `json:"model"`
				Fields []struct {
					Field  string          `json:"field"`
					Value  json.RawMessage `json:"value"`
					Source string          `json:"source"`
					Status string          `json:"status"`
				} `json:"fields"`
			} `json:"metadata"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Providers[0].Metadata) != 1 {
		t.Fatalf("metadata report missing: %s", raw)
	}
	fields := map[string]struct {
		value  string
		source string
		status string
	}{}
	for _, field := range payload.Providers[0].Metadata[0].Fields {
		fields[field.Field] = struct {
			value  string
			source string
			status string
		}{string(field.Value), field.Source, field.Status}
	}
	if got := fields["thinking.levels"]; got.value != `["low","high","max"]` || got.source != "modelparams.dev/openai/subscription" || got.status != "authoritative" {
		t.Fatalf("thinking provenance=%+v", got)
	}
	if got := fields["max-context-length"]; got.value != "128000" || got.source != "models.dev/openrouter" || got.status != "completed" {
		t.Fatalf("context provenance=%+v", got)
	}
	if got := fields["max-output-tokens"]; got.value != "16384" || got.source != "models.dev/openrouter" || got.status != "completed" {
		t.Fatalf("output provenance=%+v", got)
	}
}

func TestModelsdevFallbackKeepsUpstreamLimitsWithoutUpstreamMeta(t *testing.T) {
	modelsdev, err := parseModelsdevCatalog([]byte(`{"openrouter":{"models":{
		"gpt-x":{"id":"gpt-x","limit":{"context":64000,"output":8000}},
		"missing-output":{"id":"missing-output","limit":{"context":64000,"output":16000}}
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	spec := compiledProvider{MetadataSources: []metadataSource{{
		ID: "models.dev/openrouter", Website: "models.dev", Provider: "openrouter",
	}}}
	models := []ModelRef{{Name: "gpt-x"}, {Name: "missing-output"}}
	byID := map[string]upstreamEntry{
		"gpt-x":          {ID: "gpt-x", Context: 128000, MaxTokens: 32000},
		"missing-output": {ID: "missing-output", Context: 256000},
	}

	reports, _, _ := enrichModels(models, byID, spec, nil, nil, modelsdev, nil)
	if models[0].MaxContextLength != 128000 || models[0].MaxOutputTokens != 32000 {
		t.Fatalf("upstream limits overwritten: %+v", models[0])
	}
	if models[1].MaxContextLength != 256000 || models[1].MaxOutputTokens != 16000 {
		t.Fatalf("gap fill failed: %+v", models[1])
	}
	fields := func(report ModelMetadataResult) map[string]MetadataFieldResult {
		out := map[string]MetadataFieldResult{}
		for _, field := range report.Fields {
			out[field.Field] = field
		}
		return out
	}
	first := fields(reports[0])
	for _, name := range []string{"max-context-length", "max-output-tokens"} {
		if first[name].Source != "upstream /models" || first[name].Status != "upstream" || strings.Join(first[name].TriedSources, ",") != "upstream /models" {
			t.Fatalf("upstream provenance %s=%+v", name, first[name])
		}
	}
	second := fields(reports[1])
	if second["max-context-length"].Source != "upstream /models" || strings.Join(second["max-context-length"].TriedSources, ",") != "upstream /models" {
		t.Fatalf("context provenance=%+v", second["max-context-length"])
	}
	if second["max-output-tokens"].Source != "models.dev/openrouter" || second["max-output-tokens"].Status != "completed" || strings.Join(second["max-output-tokens"].TriedSources, ",") != "upstream /models,models.dev/openrouter" {
		t.Fatalf("output provenance=%+v", second["max-output-tokens"])
	}
}

func TestKeepAliasesFalsePreservesExistingMetadataReport(t *testing.T) {
	existing := []ModelRef{{
		Name: "gpt-x", Alias: "old", DisplayName: "GPT X", MaxContextLength: 128000,
		MaxInputTokens: 120000, MaxOutputTokens: 32000, Thinking: &ThinkingConfig{Levels: []string{"low", "high"}},
		InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
	}}
	models := mergeModels(existing, []string{"gpt-x"}, false)
	reports, _, _ := enrichModels(models, nil, compiledProvider{}, nil, nil, nil, nil)
	if models[0].Alias != "gpt-x" || models[0].DisplayName != "GPT X" || models[0].MaxContextLength != 128000 || models[0].MaxInputTokens != 120000 || models[0].MaxOutputTokens != 32000 || models[0].Thinking == nil {
		t.Fatalf("model metadata lost: %+v", models[0])
	}
	for _, field := range reports[0].Fields {
		if field.Status != "preserved" || field.Source != "existing config" || len(field.TriedSources) != 0 {
			t.Fatalf("field not preserved without external attempts: %+v", field)
		}
	}
}

func TestMetadataReportListsOnlyActuallyTriedSources(t *testing.T) {
	modelparams, err := parseModelparamsCatalog([]byte(`{"models":[
		{"provider":"first","authType":"api_key","model":"other","params":[]},
		{"provider":"second","authType":"api_key","model":"gpt-x","params":[
			{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]}
		]},
		{"provider":"third","authType":"api_key","model":"gpt-x","params":[
			{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["max"]}
		]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	modelsdev, err := parseModelsdevCatalog([]byte(`{"openrouter":{"models":{"gpt-x":{
		"id":"gpt-x","limit":{"context":128000}
	}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	spec := compiledProvider{MetadataSources: []metadataSource{
		{ID: "modelparams.dev/first/api_key", Website: "modelparams.dev", Provider: "first", AuthType: "api_key"},
		{ID: "modelparams.dev/second/api_key", Website: "modelparams.dev", Provider: "second", AuthType: "api_key"},
		{ID: "modelparams.dev/third/api_key", Website: "modelparams.dev", Provider: "third", AuthType: "api_key"},
		{ID: "models.dev/openrouter", Website: "models.dev", Provider: "openrouter"},
	}}
	models := []ModelRef{{Name: "gpt-x"}}
	reports, _, _ := enrichModels(models, nil, spec, modelparams, nil, modelsdev, nil)
	fields := map[string]MetadataFieldResult{}
	for _, field := range reports[0].Fields {
		fields[field.Field] = field
	}
	if got := strings.Join(fields["thinking.levels"].TriedSources, ","); got != "modelparams.dev/first/api_key,modelparams.dev/second/api_key" {
		t.Fatalf("thinking tried=%s", got)
	}
	if fields["thinking.levels"].Source != "modelparams.dev/second/api_key" {
		t.Fatalf("thinking source=%+v", fields["thinking.levels"])
	}
	if got := strings.Join(fields["max-context-length"].TriedSources, ","); got != "upstream /models,models.dev/openrouter" || fields["max-context-length"].Source != "models.dev/openrouter" {
		t.Fatalf("context provenance=%+v", fields["max-context-length"])
	}
	if got := strings.Join(fields["max-output-tokens"].TriedSources, ","); got != "upstream /models,modelparams.dev/first/api_key,modelparams.dev/second/api_key,modelparams.dev/third/api_key,models.dev/openrouter" {
		t.Fatalf("output tried=%s", got)
	}
	if fields["max-output-tokens"].Status != "skipped" || !strings.Contains(fields["max-output-tokens"].Reason, "tried sources") {
		t.Fatalf("output skipped=%+v", fields["max-output-tokens"])
	}
	if len(fields["max-input-tokens"].TriedSources) != 0 || !strings.Contains(fields["max-input-tokens"].Reason, "manual override") {
		t.Fatalf("input limit skipped=%+v", fields["max-input-tokens"])
	}
}

func TestMetadataSourcesKeepAuthVariantsSeparateAndNeverLeaveList(t *testing.T) {
	cat, err := parseModelparamsCatalog([]byte(`{"models":[
		{"provider":"openai","authType":"subscription","model":"gpt-x","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","max"]}]},
		{"provider":"openai","authType":"api_key","model":"gpt-x","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]}]},
		{"provider":"other","authType":"api_key","model":"only-other","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["max"]}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	spec := compiledProvider{MetadataSources: []metadataSource{
		{ID: "modelparams.dev/openai/subscription", Website: "modelparams.dev", Provider: "openai", AuthType: "subscription"},
		{ID: "modelparams.dev/openai/api_key", Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"},
	}}
	models := []ModelRef{{Name: "gpt-x"}, {Name: "only-other"}}
	reports, _, _ := enrichModels(models, nil, spec, cat, nil, nil, nil)
	if got := strings.Join(models[0].Thinking.Levels, ","); got != "low,max" {
		t.Fatalf("first auth variant did not win: %s", got)
	}
	if models[1].Thinking != nil {
		t.Fatalf("out-of-list source matched: %+v", models[1])
	}
	if reports[1].Fields[0].Status != "skipped" {
		t.Fatalf("missing field report=%+v", reports[1].Fields[0])
	}
}

func TestConfiguredSourcesReplaceExistingMetadataButPreserveUnsupportedFields(t *testing.T) {
	modelparams, _ := parseModelparamsCatalog([]byte(`{"models":[{"provider":"openai","authType":"api_key","model":"gpt-x","params":[
		{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]},
		{"path":"max_tokens","group":"generation_length","type":"integer","range":{"max":32000}}
	]}]}`))
	modelsdev, _ := parseModelsdevCatalog([]byte(`{"openrouter":{"models":{"gpt-x":{"id":"gpt-x","limit":{"context":128000},"modalities":{"input":["text"]}}}}}`))
	models := []ModelRef{{
		Name: "gpt-x", Thinking: &ThinkingConfig{Levels: []string{"max"}}, MaxContextLength: 64000,
		MaxInputTokens: 9000, MaxOutputTokens: 16000,
	}}
	spec := compiledProvider{MetadataSources: []metadataSource{
		{ID: "modelparams.dev/openai/api_key", Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"},
		{ID: "models.dev/openrouter", Website: "models.dev", Provider: "openrouter"},
	}}
	reports, _, _ := enrichModels(models, nil, spec, modelparams, nil, modelsdev, nil)
	if strings.Join(models[0].Thinking.Levels, ",") != "low,high" || models[0].MaxOutputTokens != 32000 {
		t.Fatalf("authoritative source did not replace existing values: %+v", models[0])
	}
	if models[0].MaxContextLength != 64000 || models[0].MaxInputTokens != 9000 {
		t.Fatalf("existing unsupported fields were not preserved: %+v", models[0])
	}
	fields := map[string]MetadataFieldResult{}
	for _, field := range reports[0].Fields {
		fields[field.Field] = field
	}
	if fields["thinking.levels"].Status != "authoritative" || strings.Join(fields["thinking.levels"].TriedSources, ",") != "modelparams.dev/openai/api_key" || fields["max-output-tokens"].Status != "authoritative" || strings.Join(fields["max-output-tokens"].TriedSources, ",") != "modelparams.dev/openai/api_key" {
		t.Fatalf("authoritative reports=%+v", fields)
	}
	if fields["max-context-length"].Status != "preserved" || len(fields["max-context-length"].TriedSources) != 0 || fields["max-input-tokens"].Status != "preserved" || len(fields["max-input-tokens"].TriedSources) != 0 {
		t.Fatalf("preserved reports=%+v", fields)
	}
}

func TestExistingAndUpstreamMetadataProvenance(t *testing.T) {
	models := []ModelRef{{Name: "gpt-x", MaxInputTokens: 9000}}
	spec := compiledProvider{UpstreamMeta: true}
	reports, _, _ := enrichModels(models, map[string]upstreamEntry{"gpt-x": {
		ID: "gpt-x", Efforts: []string{"low", "high"}, Context: 128000, MaxTokens: 32000,
		Input: []string{"text", "image"}, Output: []string{"text"},
	}}, spec, nil, nil, nil, nil)
	fields := map[string]MetadataFieldResult{}
	for _, field := range reports[0].Fields {
		fields[field.Field] = field
	}
	if fields["max-input-tokens"].Source != "existing config" || fields["max-input-tokens"].Status != "preserved" {
		t.Fatalf("existing report=%+v", fields["max-input-tokens"])
	}
	for _, name := range []string{"thinking.levels", "max-context-length", "max-output-tokens", "input-modalities", "output-modalities"} {
		if fields[name].Source != "upstream /models" || fields[name].Status != "upstream" {
			t.Fatalf("upstream %s=%+v", name, fields[name])
		}
	}
}

func TestMetadataSourcesSecondaryOnlyFillsGapsAndOverridesLast(t *testing.T) {
	modelparams, _ := parseModelparamsCatalog([]byte(`{"models":[{"provider":"openai","authType":"api_key","model":"gpt-x","params":[
		{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]},
		{"path":"max_completion_tokens","group":"generation_length","type":"integer","range":{"min":1,"max":32000},"default":1000}
	]}]}`))
	modelsdev, _ := parseModelsdevCatalog([]byte(`{"openrouter":{"models":{"vendor/gpt-x":{"id":"gpt-x","limit":{"context":128000,"output":64000},"modalities":{"input":["text","image"],"output":["text"]}}}}}`))
	spec := compiledProvider{
		MetadataSources: []metadataSource{
			{ID: "modelparams.dev/openai/api_key", Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"},
			{ID: "models.dev/openrouter", Website: "models.dev", Provider: "openrouter"},
		},
		Overrides: map[string]ModelOverride{"gpt-x": {MaxContextLength: 256000}},
	}
	models := []ModelRef{{Name: "gpt-x"}}
	reports, _, _ := enrichModels(models, nil, spec, modelparams, nil, modelsdev, nil)
	if models[0].MaxOutputTokens != 32000 {
		t.Fatalf("secondary overwrote authoritative output limit: %+v", models[0])
	}
	if models[0].MaxContextLength != 256000 || strings.Join(models[0].InputModalities, ",") != "text,image" {
		t.Fatalf("secondary/override fields=%+v", models[0])
	}
	fields := map[string]MetadataFieldResult{}
	for _, field := range reports[0].Fields {
		fields[field.Field] = field
	}
	if fields["max-output-tokens"].Source != "modelparams.dev/openai/api_key" || fields["max-output-tokens"].Status != "authoritative" {
		t.Fatalf("output report=%+v", fields["max-output-tokens"])
	}
	if fields["max-context-length"].Source != "manual override" || fields["max-context-length"].Status != "override" {
		t.Fatalf("override report=%+v", fields["max-context-length"])
	}
}

func TestModelparamsMaxOutputRequiresConcreteRangeMax(t *testing.T) {
	entry := modelparamsEntry{Params: []modelparamsParam{{Path: "max_tokens", Group: "generation_length", Type: "integer"}}}
	if got := extractMaxOutputTokens(entry.Params); got != 0 {
		t.Fatalf("default/parameter support must not become capability limit: %d", got)
	}
	entry.Params[0].Range.Max = 8192
	if got := extractMaxOutputTokens(entry.Params); got != 8192 {
		t.Fatalf("range max=%d", got)
	}
}

func TestParseConfigValidatesMetadataSources(t *testing.T) {
	valid, err := parseFileConfig([]byte(`{"providers":{"ZCode":{"enabled":true,"metadata_sources":["modelparams.dev/openai/subscription","modelparams.dev/openai/api_key","models.dev/openrouter"]}}}`))
	if err != nil || len(valid.Providers[0].MetadataSources) != 3 {
		t.Fatalf("valid config=%+v err=%v", valid, err)
	}
	for name, raw := range map[string]string{
		"malformed": `{"providers":{"ZCode":{"metadata_sources":["modelparams.dev/openai"]}}}`,
		"duplicate": `{"providers":{"ZCode":{"metadata_sources":["models.dev/openrouter","models.dev/openrouter"]}}}`,
		"authority": `{"providers":{"ZCode":{"metadata_sources":["models.dev/openrouter","modelparams.dev/openai/api_key"]}}}`,
		"auth":      `{"providers":{"ZCode":{"metadata_sources":["modelparams.dev/openai/oauth"]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFileConfig([]byte(raw)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSyncFetchesEachNeededWebsiteOnceAcrossProviders(t *testing.T) {
	transport := &codexSyncTransport{
		t: t,
		compatJSON: `{"openai-compatibility":[
			{"name":"First","base-url":"http://first/v1","api-key-entries":[{"auth-index":"first"}],"models":[]},
			{"name":"Second","base-url":"http://second/v1","api-key-entries":[{"auth-index":"second"}],"models":[]}
		]}`,
		manifests: map[string]string{
			"http://first/v1/models":  `{"data":[{"id":"gpt-x"}]}`,
			"http://second/v1/models": `{"data":[{"id":"gpt-x"}]}`,
		},
		modelparams: `{"models":[{"provider":"openai","authType":"api_key","model":"gpt-x","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]}]}]}`,
		modelsdev:   `{"openrouter":{"models":{"gpt-x":{"id":"gpt-x","limit":{"context":128000}}}}}`,
	}
	service := New(transport)
	service.cfg = runtimeConfig{
		ManagementBaseURL: "http://cpa:8317",
		Providers: []compiledProvider{
			{Name: "First", Enabled: true, Mode: ModeExclude},
			{Name: "Second", Enabled: true, Mode: ModeExclude, MetadataSources: []metadataSource{
				{ID: "modelparams.dev/openai/api_key", Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"},
				{ID: "models.dev/openrouter", Website: "models.dev", Provider: "openrouter"},
			}},
		},
	}
	report := service.Preview("management-key", "", nil)
	if !report.OK {
		t.Fatalf("report=%+v", report)
	}
	if transport.modelparamsHits != 1 || transport.modelsdevHits != 1 {
		t.Fatalf("catalog fetches modelparams=%d modelsdev=%d", transport.modelparamsHits, transport.modelsdevHits)
	}
}

func TestSyncFileModeUsesConfigModelsForEquality(t *testing.T) {
	path := writeTemp(t, `openai-compatibility:
  - name: ZCode
    models:
      - name: a
        alias: a
        max-context-length: 100
        max-output-tokens: 10
      - name: b
        alias: b
        max-context-length: 200
        max-output-tokens: 20
`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	transport := &codexSyncTransport{
		t: t,
		compatJSON: `{"openai-compatibility":[{"name":"ZCode","base-url":"http://zcode-proxy:8080/v1","models":[
			{"name":"b","alias":"b","max-context-length":200},
			{"name":"a","alias":"a","max-context-length":100}
		]}]}`,
		manifest: `{"data":[{"id":"b"},{"id":"a"},{"id":"a"}]}`,
	}
	service := New(transport)
	service.cfg = runtimeConfig{
		ManagementBaseURL:   "http://cpa:8317",
		KeepExistingAliases: true,
		WriteMode:           WriteModeFile,
		ConfigPath:          path,
		Providers: []compiledProvider{{
			Name:    "ZCode",
			Enabled: true,
			Mode:    ModeExclude,
			Overrides: map[string]ModelOverride{
				"a": {MaxOutputTokens: 10},
				"b": {MaxOutputTokens: 20},
			},
		}},
	}

	report := service.Sync("management-key", "ZCode")
	if !report.OK || len(report.Providers) != 1 || !report.Providers[0].Unchanged {
		t.Fatalf("report=%+v", report)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("unchanged model set rewrote config:\n%s", after)
	}
	backups, err := filepath.Glob(path + backupSuffix + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("unchanged sync created backups: %v", backups)
	}

	service.cfg.Providers[0].Overrides["a"] = ModelOverride{MaxOutputTokens: 30}
	report = service.Sync("management-key", "ZCode")
	if !report.OK || report.Providers[0].Unchanged {
		t.Fatalf("metadata change report=%+v", report)
	}
	models, err := readModelsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range models["ZCode"] {
		if model.Name == "a" {
			found = true
			if model.MaxOutputTokens != 30 {
				t.Fatalf("max output tokens=%d, want 30", model.MaxOutputTokens)
			}
		}
	}
	if !found {
		t.Fatal("model a missing after metadata update")
	}
}

func TestParseConfigRejectsLegacyMetadataBooleans(t *testing.T) {
	for name, raw := range map[string]string{
		"modelparams":         `{"providers":{"ZCode":{"modelparams":true}}}`,
		"modelsdev":           `{"providers":{"ZCode":{"modelsdev":true}}}`,
		"legacy with sources": `{"providers":{"ZCode":{"modelparams":true,"metadata_sources":["modelparams.dev/openai/api_key"]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseFileConfig([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), "replace legacy modelparams/modelsdev with metadata_sources") {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if _, err := parseFileConfig([]byte(`{"providers":{"ZCode":{"modelparams":false,"modelsdev":false}}}`)); err != nil {
		t.Fatalf("false legacy values must remain harmless: %v", err)
	}
}

func TestThinkingCountersIgnorePreservedAndOverrideOnly(t *testing.T) {
	models := []ModelRef{{Name: "preserved", Thinking: &ThinkingConfig{Levels: []string{"high"}}}}
	reports, matched, missed := enrichModels(models, nil, compiledProvider{}, nil, nil, nil, nil)
	if matched != 0 || missed != 0 || reports[0].Fields[0].Status != "preserved" {
		t.Fatalf("preserved-only matched=%d missed=%d report=%+v", matched, missed, reports)
	}

	models = []ModelRef{{Name: "override"}}
	reports, matched, missed = enrichModels(models, nil, compiledProvider{Overrides: map[string]ModelOverride{"override": {ThinkingLevels: []string{"max"}}}}, nil, nil, nil, nil)
	if matched != 0 || missed != 0 || reports[0].Fields[0].Status != "override" {
		t.Fatalf("override-only matched=%d missed=%d report=%+v", matched, missed, reports)
	}
}

func TestThinkingCountersKeepEnrichmentMatchBeforeOverride(t *testing.T) {
	cat, err := parseModelparamsCatalog([]byte(`{"models":[{"provider":"openai","authType":"api_key","model":"external","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	source := metadataSource{ID: "modelparams.dev/openai/api_key", Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"}
	models := []ModelRef{{Name: "upstream"}, {Name: "external"}, {Name: "missing"}}
	spec := compiledProvider{
		UpstreamMeta:    true,
		MetadataSources: []metadataSource{source},
		Overrides: map[string]ModelOverride{
			"upstream": {ThinkingLevels: []string{"max"}},
			"external": {ThinkingLevels: []string{"max"}},
			"missing":  {ThinkingLevels: []string{"max"}},
		},
	}
	reports, matched, missed := enrichModels(models, map[string]upstreamEntry{"upstream": {ID: "upstream", Efforts: []string{"low"}}}, spec, cat, nil, nil, nil)
	if matched != 2 || missed != 1 {
		t.Fatalf("matched=%d missed=%d reports=%+v", matched, missed, reports)
	}
	for i := range models {
		if got := strings.Join(models[i].Thinking.Levels, ","); got != "max" {
			t.Fatalf("model[%d] final thinking=%s", i, got)
		}
	}
}

func TestParseConfigNormalizesOverrides(t *testing.T) {
	cfg, err := parseFileConfig([]byte(`{
		"providers":{"ZCode":{"enabled":true,"mode":"exclude","overrides":{
			"glm-5.3":{"max_context_length":1050000,"max_input_tokens":1000000,"max_output_tokens":50000,"thinking_levels":["MAX","low","low"]}
		}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	override := cfg.Providers[0].Overrides["glm-5.3"]
	if override.MaxContextLength != 1050000 || override.MaxInputTokens != 1000000 || override.MaxOutputTokens != 50000 || strings.Join(override.ThinkingLevels, ",") != "low,max" {
		t.Fatalf("override=%+v", override)
	}
}

func TestParseConfigRejectsOverrideModelWhitespace(t *testing.T) {
	_, err := parseFileConfig([]byte(`{
		"providers":{"ZCode":{"enabled":true,"mode":"exclude","overrides":{
			" glm-5.3 ":{"max_input_tokens":1000000}
		}}}
	}`))
	if err == nil || !strings.Contains(err.Error(), "must not have surrounding whitespace") {
		t.Fatalf("err=%v", err)
	}
}

func TestModelsEqualIgnoresOrderAndDuplicates(t *testing.T) {
	a := ModelRef{Name: "a", Alias: "a", MaxContextLength: 100}
	b := ModelRef{Name: "b", Alias: "b", Thinking: &ThinkingConfig{Levels: []string{"low", "high"}}}
	if !modelsEqual([]ModelRef{a, b}, []ModelRef{b, a, a}) {
		t.Fatal("model equality must use set semantics")
	}
}

func TestModelsEqualIncludesMetadata(t *testing.T) {
	base := []ModelRef{{Name: "model", Alias: "model"}}
	for name, changed := range map[string]ModelRef{
		"alias":             {Name: "model", Alias: "alias"},
		"display":           {Name: "model", Alias: "model", DisplayName: "Model"},
		"context":           {Name: "model", Alias: "model", MaxContextLength: 100},
		"input limit":       {Name: "model", Alias: "model", MaxInputTokens: 90},
		"output limit":      {Name: "model", Alias: "model", MaxOutputTokens: 10},
		"thinking":          {Name: "model", Alias: "model", Thinking: &ThinkingConfig{Levels: []string{"high"}}},
		"input modalities":  {Name: "model", Alias: "model", InputModalities: []string{"image"}},
		"output modalities": {Name: "model", Alias: "model", OutputModalities: []string{"image"}},
	} {
		if modelsEqual(base, []ModelRef{changed}) {
			t.Fatalf("%s metadata was ignored", name)
		}
	}
}

func TestParseConfigRejectsInvalidOverride(t *testing.T) {
	_, err := parseFileConfig([]byte(`{
		"providers":{"ZCode":{"enabled":true,"mode":"exclude","overrides":{
			"glm-5.3":{"thinking_levels":["low","bad"]}
		}}}
	}`))
	if err == nil || !strings.Contains(err.Error(), `unsupported thinking level "bad"`) {
		t.Fatalf("err=%v", err)
	}
}
