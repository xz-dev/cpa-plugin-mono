package plugin

import "testing"

func TestParseAndMatchModelsdevSelectedProvider(t *testing.T) {
	raw := []byte(`{
		"openai": {"models": {
			"gpt-4o": {"id": "gpt-4o", "limit": {"context": 128000, "output": 16384}, "modalities": {"input": ["text","image"], "output": ["text"]}},
			"gpt-image-1.5": {"id": "gpt-image-1.5", "limit": {"context": 0, "output": 0}}
		}},
		"hpc-ai": {"models": {
			"zai-org/glm-5.2": {"id": "glm-5.2", "limit": {"context": 1048576, "output": 131072}}
		}}
	}`)
	cat, err := parseModelsdevCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	hpc := metadataSource{Website: "models.dev", Provider: "hpc-ai"}
	if e, ok := cat.lookupSource(hpc, "zai-org/glm-5.2"); !ok || e.Context != 1048576 || e.MaxOut != 131072 {
		t.Fatalf("qualified lookup: %+v ok=%v", e, ok)
	}
	if e, ok := cat.lookupSource(hpc, "glm-5.2"); !ok || e.MaxOut != 131072 {
		t.Fatalf("exact catalog id lookup: %+v ok=%v", e, ok)
	}
	openai := metadataSource{Website: "models.dev", Provider: "openai"}
	if e, ok := cat.lookupSource(openai, "gpt-4o"); !ok || e.Context != 128000 || e.MaxOut != 16384 {
		t.Fatalf("plain lookup: %+v ok=%v", e, ok)
	}
	if _, ok := cat.lookupSource(openai, "gpt-image-1.5"); ok {
		t.Fatal("empty entry must be skipped")
	}
	if _, ok := cat.lookupSource(openai, "glm-5.2"); ok {
		t.Fatal("lookup must not leave selected provider")
	}
}

func TestModelsdevAmbiguousBareModelMisses(t *testing.T) {
	cat, err := parseModelsdevCatalog([]byte(`{
		"openrouter": {"models": {
			"vendor-a/glm-x": {"id": "vendor-a/glm-x", "limit": {"context": 111}},
			"vendor-b/glm-x": {"id": "vendor-b/glm-x", "limit": {"context": 222}}
		}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	source := metadataSource{Website: "models.dev", Provider: "openrouter"}
	if _, ok := cat.lookupSource(source, "glm-x"); ok {
		t.Fatal("ambiguous bare id must miss")
	}
	if e, ok := cat.lookupSource(source, "vendor-b/glm-x"); !ok || e.Context != 222 {
		t.Fatalf("exact key must match: %+v ok=%v", e, ok)
	}
}

func TestModelsdevSinglePrefixFallbackRejectsNestedCPAID(t *testing.T) {
	cat, err := parseModelsdevCatalog([]byte(`{
		"openrouter": {"models": {"foo": {"id": "foo", "limit": {"context": 111}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	source := metadataSource{Website: "models.dev", Provider: "openrouter"}
	if entry, ok := cat.lookupSource(source, "vendor/foo"); !ok || entry.Context != 111 {
		t.Fatalf("single prefix fallback: %+v ok=%v", entry, ok)
	}
	if entry, ok := cat.lookupSource(source, "vendor/nested/foo"); ok {
		t.Fatalf("nested id must not degrade to bare match: %+v", entry)
	}
}

func TestDecodeUpstreamMaxTokens(t *testing.T) {
	entries, err := parseOpenAICatalog([]byte(`{"models":[
		{"slug":"gpt-5.6-sol","context_window":272000,"max_tokens":128000},
		{"slug":"glm-5.3","context_window":272000,"max_tokens":null},
		{"id":"or/model","context_length":200000,"top_provider":{"max_completion_tokens":8192}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]upstreamEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	if byID["gpt-5.6-sol"].MaxTokens != 128000 {
		t.Fatalf("codex max_tokens: %+v", byID["gpt-5.6-sol"])
	}
	if byID["glm-5.3"].MaxTokens != 0 {
		t.Fatalf("null max_tokens must stay 0: %+v", byID["glm-5.3"])
	}
	if byID["or/model"].MaxTokens != 8192 || byID["or/model"].Context != 200000 {
		t.Fatalf("openrouter entry: %+v", byID["or/model"])
	}
}
