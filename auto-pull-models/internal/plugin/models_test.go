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
	if got := modelsURL("https://openrouter.ai/api/v1"); got != "https://openrouter.ai/api/v1/models" {
		t.Fatalf("%s", got)
	}
	if got := modelsURL("https://x/v1/models"); got != "https://x/v1/models" {
		t.Fatalf("%s", got)
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
