package plugin

import (
	"strings"
	"testing"
)

func TestParseUpstreamModelsData(t *testing.T) {
	ids, err := parseUpstreamModels([]byte(`{"object":"list","data":[{"id":"a"},{"id":"b","name":"ignored"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("%v", ids)
	}
}

func TestParseUpstreamModelsArray(t *testing.T) {
	ids, err := parseUpstreamModels([]byte(`["x",{"name":"y"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "x" || ids[1] != "y" {
		t.Fatalf("%v", ids)
	}
}

func TestModelsURL(t *testing.T) {
	if got := modelsURL("https://openrouter.ai/api/v1", false); got != "https://openrouter.ai/api/v1/models" {
		t.Fatalf("%s", got)
	}
	if got := modelsURL("https://x/v1/models", false); got != "https://x/v1/models" {
		t.Fatalf("%s", got)
	}
	if got := modelsURL("https://x/v1?existing=1", true); got != "https://x/v1/models?client_version=1.0.0&existing=1" {
		t.Fatalf("%s", got)
	}
}

func TestParseCodexManifest(t *testing.T) {
	body := []byte(`{"models":[
		{"slug":"glm-5.3","context_window":1048576,"input_modalities":["text","image"],"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]},
		{"slug":"glm-5.3[1m]","context_window":1048576,"input_modalities":["text"],"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]}
	]}`)
	entries, err := parseUpstreamCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != "glm-5.3" || entries[1].ID != "glm-5.3[1m]" {
		t.Fatalf("entries=%v", entries)
	}
	if strings.Join(entries[0].Efforts, ",") != "low,high,max" {
		t.Fatalf("efforts=%v", entries[0].Efforts)
	}
	if entries[0].Context != 1048576 {
		t.Fatalf("context=%d", entries[0].Context)
	}
	if got := cpaModalities(entries[0].Input); strings.Join(got, ",") != "text,image" {
		t.Fatalf("modalities=%v", got)
	}
}

func TestParseCodexManifestConfig(t *testing.T) {
	cfg, err := parseFileConfig([]byte(`{"providers":{"ZCode":{"enabled":true,"codex_manifest":true,"upstream_meta":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || !cfg.Providers[0].CodexManifest || !cfg.Providers[0].UpstreamMeta {
		t.Fatalf("providers=%v", cfg.Providers)
	}
}

func TestParseOpenRouterCatalog(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"openai/gpt-5.6-sol","context_length":1050000,"architecture":{"input_modalities":["text","image","file"],"output_modalities":["text"]},"reasoning":{"supported_efforts":["max","xhigh","high","medium","low","none"],"default_effort":"medium"}},
		{"id":"google/gemini-2.5-pro","context_length":1048576,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"reasoning":{"mandatory":true}},
		{"id":"x-ai/grok-imagine","context_length":272000,"architecture":{"input_modalities":["text"],"output_modalities":["image"]}}
	]}`)
	entries, err := parseUpstreamCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("len=%d", len(entries))
	}
	if strings.Join(entries[0].Efforts, ",") != "none,low,medium,high,xhigh,max" {
		t.Fatalf("sol efforts %v", entries[0].Efforts)
	}
	if entries[0].Context != 1050000 {
		t.Fatalf("ctx %d", entries[0].Context)
	}
	if got := cpaModalities(entries[0].Input); strings.Join(got, ",") != "text,image" {
		t.Fatalf("mods %v", got)
	}
	if entries[1].Efforts != nil {
		t.Fatalf("gemini-2.5-pro should not invent efforts: %v", entries[1].Efforts)
	}
	if entries[2].Efforts != nil {
		t.Fatalf("image model should have no efforts")
	}
}
