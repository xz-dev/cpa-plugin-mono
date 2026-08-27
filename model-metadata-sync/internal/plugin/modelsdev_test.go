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

func TestCatalogParsersFailClosedOnAmbiguity(t *testing.T) {
	openAI := map[string]string{
		"both wrappers":       `{"data":[{"id":"a"}],"models":[{"id":"b"}]}`,
		"folded wrapper":      `{"DATA":[{"id":"a"}]}`,
		"duplicate wrapper":   `{"data":[{"id":"a"}],"data":[{"id":"b"}]}`,
		"mixed invalid entry": `{"data":[{"id":"a"},{"id":1}]}`,
		"duplicate id":        `{"data":[{"id":"a"},{"id":"a"}]}`,
		"conflicting id":      `{"data":[{"id":"a","name":"b"}]}`,
		"folded field":        `{"data":[{"id":"a","MAX_TOKENS":4}]}`,
		"fractional limit":    `{"data":[{"id":"a","max_tokens":4.5}]}`,
		"trailing JSON":       `{"data":[{"id":"a"}]} {}`,
	}
	for name, raw := range openAI {
		t.Run("openai "+name, func(t *testing.T) {
			if _, err := parseOpenAICatalog([]byte(raw)); err == nil {
				t.Fatal("ambiguous OpenAI catalog accepted")
			}
		})
	}
	claude := map[string]string{
		"folded wrapper":   `{"DATA":[{"id":"a"}],"has_more":false}`,
		"duplicate id":     `{"data":[{"id":"a"},{"id":"a"}],"has_more":false}`,
		"missing id":       `{"data":[{"max_tokens":4}],"has_more":false}`,
		"folded field":     `{"data":[{"id":"a","MAX_TOKENS":4}],"has_more":false}`,
		"negative limit":   `{"data":[{"id":"a","max_tokens":-1}],"has_more":false}`,
		"duplicate nested": `{"data":[{"id":"a","supported_reasoning_levels":[{"effort":"low","Effort":"high"}]}],"has_more":false}`,
		"trailing JSON":    `{"data":[],"has_more":false} []`,
	}
	for name, raw := range claude {
		t.Run("claude "+name, func(t *testing.T) {
			if _, _, _, err := parseClaudeCatalog([]byte(raw)); err == nil {
				t.Fatal("ambiguous Claude catalog accepted")
			}
		})
	}
	modelsdev := map[string]string{
		"duplicate provider": `{"openai":{"models":{"a":{"limit":{"context":1}}}},"OpenAI":{"models":{"b":{"limit":{"context":2}}}}}`,
		"folded models":      `{"openai":{"Models":{"a":{"limit":{"context":1}}}}}`,
		"folded limit":       `{"openai":{"models":{"a":{"Limit":{"context":1}}}}}`,
		"negative limit":     `{"openai":{"models":{"a":{"limit":{"context":-1}}}}}`,
		"duplicate nested":   `{"openai":{"models":{"a":{"limit":{"context":1,"Context":2}}}}}`,
		"trailing JSON":      `{"openai":{"models":{"a":{"limit":{"context":1}}}}} {}`,
	}
	for name, raw := range modelsdev {
		t.Run("modelsdev "+name, func(t *testing.T) {
			if _, err := parseModelsdevCatalog([]byte(raw)); err == nil {
				t.Fatal("ambiguous models.dev catalog accepted")
			}
		})
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
