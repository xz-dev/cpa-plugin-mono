package plugin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

type codexSyncTransport struct {
	t          *testing.T
	manifest   string
	apiCallURL string
	patched    []ModelRef
}

func (t *codexSyncTransport) Do(method, url string, _ http.Header, body []byte) (int, []byte, error) {
	switch {
	case method == http.MethodGet && strings.HasSuffix(url, "/v0/management/openai-compatibility"):
		return http.StatusOK, []byte(`{"openai-compatibility":[{"name":"ZCode","base-url":"http://zcode-proxy:8080/v1","api-key-entries":[{"auth-index":"zcode"}],"models":[]}]}`), nil
	case method == http.MethodPost && strings.HasSuffix(url, "/v0/management/api-call"):
		var req apiCallRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.t.Fatal(err)
		}
		t.apiCallURL = req.URL
		response, err := json.Marshal(apiCallResponse{StatusCode: http.StatusOK, Body: t.manifest})
		if err != nil {
			t.t.Fatal(err)
		}
		return http.StatusOK, response, nil
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

func TestModelsEqualIncludesTokenLimits(t *testing.T) {
	base := []ModelRef{{Name: "model", Alias: "model"}}
	for name, changed := range map[string]ModelRef{
		"context": {Name: "model", Alias: "model", MaxContextLength: 100},
		"input":   {Name: "model", Alias: "model", MaxInputTokens: 90},
		"output":  {Name: "model", Alias: "model", MaxOutputTokens: 10},
	} {
		if modelsEqual(base, []ModelRef{changed}) {
			t.Fatalf("%s token limit was ignored", name)
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
