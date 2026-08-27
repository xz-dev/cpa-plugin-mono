package plugin

import (
	"strings"
	"testing"
)

const modelparamsFixture = `{
  "count": 6,
  "models": [
    {
      "provider": "openai",
      "authType": "api_key",
      "model": "gpt-5.6-sol",
      "params": [
        {"path": "reasoning_effort", "group": "reasoning", "type": "enum", "values": ["none", "low", "medium", "high", "xhigh"]}
      ]
    },
    {
      "provider": "openai",
      "authType": "subscription",
      "model": "gpt-5.6-sol",
      "params": [
        {"path": "reasoning.effort", "group": "reasoning", "type": "enum", "values": ["none", "low", "medium", "high", "xhigh", "max"]}
      ]
    },
    {
      "provider": "anthropic",
      "authType": "api_key",
      "model": "claude-opus-4-6",
      "params": [
        {"path": "thinking.type", "group": "reasoning", "type": "enum", "values": ["disabled", "enabled"]},
        {"path": "output_config.effort", "group": "reasoning", "type": "enum", "values": ["low", "medium", "high", "max"]}
      ]
    },
    {
      "provider": "google",
      "authType": "api_key",
      "model": "gemini-3-flash-preview",
      "params": [
        {"path": "generationConfig.thinkingConfig.thinkingLevel", "group": "reasoning", "type": "enum", "values": ["minimal", "low", "medium", "high"]}
      ]
    },
    {
      "provider": "xai",
      "authType": "api_key",
      "model": "grok-4.20-0309-non-reasoning",
      "params": [
        {"path": "temperature", "group": "sampling", "type": "number"}
      ]
    },
    {
      "provider": "moonshot",
      "authType": "api_key",
      "model": "kimi-k3",
      "params": [
        {"path": "reasoning_effort", "group": "reasoning", "type": "enum", "values": ["low", "high", "max"]}
      ]
    }
  ]
}`

func TestParseAndMatchModelparams(t *testing.T) {
	cat, err := parseModelparamsCatalog([]byte(modelparamsFixture))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		source  metadataSource
		id      string
		want    string
		missing bool
	}{
		{metadataSource{Website: "modelparams.dev", Provider: "openai", AuthType: "subscription"}, "gpt-5.6-sol", "none,low,medium,high,xhigh,max", false},
		{metadataSource{Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"}, "openai/gpt-5.6-sol", "none,low,medium,high,xhigh", false},
		{metadataSource{Website: "modelparams.dev", Provider: "anthropic", AuthType: "api_key"}, "claude-opus-4-6-thinking", "", true},
		{metadataSource{Website: "modelparams.dev", Provider: "google", AuthType: "api_key"}, "gemini-3-flash", "", true},
		{metadataSource{Website: "modelparams.dev", Provider: "moonshot", AuthType: "api_key"}, "kimi-k3", "low,high,max", false},
		{metadataSource{Website: "modelparams.dev", Provider: "xai", AuthType: "api_key"}, "grok-4.20-0309-non-reasoning", "", true},
		{metadataSource{Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"}, "gpt-image-1", "", true},
	}
	for _, tc := range cases {
		entry, ok := cat.lookupSource(tc.source, tc.id)
		got := extractThinkingLevels(entry.Params)
		ok = ok && len(got) > 0
		if tc.missing {
			if ok {
				t.Fatalf("%s: expected miss, got %v", tc.id, got)
			}
			continue
		}
		if !ok {
			t.Fatalf("%s: expected hit", tc.id)
		}
		if strings.Join(got, ",") != tc.want {
			t.Fatalf("%s: got %v want %s", tc.id, got, tc.want)
		}
	}
}

func TestModelparamsLookupUsesOnlyExactOrSinglePrefixIDs(t *testing.T) {
	cat, err := parseModelparamsCatalog([]byte(`{"models":[
		{"provider":"openai","authType":"api_key","model":"foo","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["low","high"]}]},
		{"provider":"openai","authType":"api_key","model":"bar","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["high"]}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	source := metadataSource{Website: "modelparams.dev", Provider: "openai", AuthType: "api_key"}
	for _, id := range []string{"foo-preview", "foo-thinking", "foo:variant", "vendor/nested/foo"} {
		if _, ok := cat.lookupSource(source, id); ok {
			t.Fatalf("semantic variant %q must not match foo", id)
		}
	}
	if _, ok := cat.lookupSource(source, "openai/bar"); !ok {
		t.Fatal("single provider prefix must resolve exact bare catalog id")
	}
	if _, ok := cat.lookupSource(source, "foo"); !ok {
		t.Fatal("exact catalog id must match")
	}

	previewOnly, err := parseModelparamsCatalog([]byte(`{"models":[{"provider":"openai","authType":"api_key","model":"foo-preview","params":[{"path":"reasoning_effort","group":"reasoning","type":"enum","values":["high"]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := previewOnly.lookupSource(source, "foo"); ok {
		t.Fatal("foo must not match foo-preview")
	}
}

func TestModelparamsCatalogFailsClosedOnAmbiguity(t *testing.T) {
	cases := map[string]string{
		"folded wrapper":    `{"Models":[{"provider":"openai","model":"a"}]}`,
		"duplicate wrapper": `{"models":[{"provider":"openai","model":"a"}],"Models":[]}`,
		"invalid mixed":     `{"models":[{"provider":"openai","model":"a"},{"provider":"","model":"b"}]}`,
		"duplicate entry":   `{"models":[{"provider":"openai","model":"a"},{"provider":"openai","model":"a"}]}`,
		"folded entry":      `{"models":[{"Provider":"openai","model":"a"}]}`,
		"duplicate path":    `{"models":[{"provider":"openai","model":"a","params":[{"path":"max_tokens"},{"path":"max_tokens"}]}]}`,
		"folded parameter":  `{"models":[{"provider":"openai","model":"a","params":[{"Path":"max_tokens"}]}]}`,
		"fractional range":  `{"models":[{"provider":"openai","model":"a","params":[{"path":"max_tokens","range":{"max":4.5}}]}]}`,
		"non-string enum":   `{"models":[{"provider":"openai","model":"a","params":[{"path":"reasoning_effort","type":"enum","values":["low",1]}]}]}`,
		"trailing JSON":     `{"models":[{"provider":"openai","model":"a"}]} []`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseModelparamsCatalog([]byte(raw)); err == nil {
				t.Fatal("ambiguous modelparams catalog accepted")
			}
		})
	}
}

func TestExtractSkipsThinkingTypeOnly(t *testing.T) {
	got := extractThinkingLevels([]modelparamsParam{
		{Path: "thinking.type", Group: "reasoning", Type: "enum", Values: []any{"disabled", "enabled"}},
	})
	if got != nil {
		t.Fatalf("got %v", got)
	}
}
