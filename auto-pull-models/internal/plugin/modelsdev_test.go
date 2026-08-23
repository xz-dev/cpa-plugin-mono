package plugin

import "testing"

func TestParseAndMatchModelsdev(t *testing.T) {
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
	// Exact provider-qualified key.
	if e, ok := cat.lookup("zai-org/glm-5.2"); !ok || e.Context != 1048576 || e.MaxOut != 131072 {
		t.Fatalf("qualified lookup: %+v ok=%v", e, ok)
	}
	// Bare id resolves through the bare index.
	if e, ok := cat.lookup("glm-5.2"); !ok || e.MaxOut != 131072 {
		t.Fatalf("bare lookup: %+v ok=%v", e, ok)
	}
	// Plain provider entry.
	if e, ok := cat.lookup("gpt-4o"); !ok || e.Context != 128000 || e.MaxOut != 16384 {
		t.Fatalf("plain lookup: %+v ok=%v", e, ok)
	}
	// Zero-limit rows are dropped.
	if _, ok := cat.lookup("gpt-image-1.5"); ok {
		t.Fatal("zero-limit entry must be skipped")
	}
	// Unknown id.
	if _, ok := cat.lookup("nope"); ok {
		t.Fatal("unknown id must not match")
	}
}

func TestModelsdevPrefersOpenRouterThenLexicographic(t *testing.T) {
	cat, err := parseModelsdevCatalog([]byte(`{
		"zgate": {"models": {"glm-x": {"id": "glm-x", "limit": {"context": 111, "output": 1}}}},
		"openrouter": {"models": {"z-ai/glm-x": {"id": "glm-x", "limit": {"context": 222, "output": 2}}}},
		"agg-b": {"models": {"glm-x": {"id": "glm-x", "limit": {"context": 333, "output": 3}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := cat.lookup("glm-x"); !ok || e.Context != 222 {
		t.Fatalf("openrouter must win: %+v ok=%v", e, ok)
	}
}

func TestDecodeUpstreamMaxTokens(t *testing.T) {
	entries, err := parseUpstreamCatalog([]byte(`{"models":[
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
