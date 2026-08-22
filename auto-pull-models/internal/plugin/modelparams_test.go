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
		id      string
		want    string
		missing bool
	}{
		{"gpt-5.6-sol", "none,low,medium,high,xhigh,max", false},
		{"openai/gpt-5.6-sol", "none,low,medium,high,xhigh", false},
		{"claude-opus-4-6-thinking", "low,medium,high,max", false},
		{"gemini-3-flash", "minimal,low,medium,high", false},
		{"kimi-k3", "low,high,max", false},
		{"grok-4.20-0309-non-reasoning", "", true},
		{"gpt-image-1", "", true},
	}
	for _, tc := range cases {
		got, ok := cat.levelsFor(tc.id)
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

func TestExtractSkipsThinkingTypeOnly(t *testing.T) {
	got := extractThinkingLevels([]modelparamsParam{
		{Path: "thinking.type", Group: "reasoning", Type: "enum", Values: []any{"disabled", "enabled"}},
	})
	if got != nil {
		t.Fatalf("got %v", got)
	}
}
