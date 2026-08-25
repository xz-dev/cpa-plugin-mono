package plugin

import (
	"regexp"
	"strings"
	"testing"
)

func TestFilterIDsInclude(t *testing.T) {
	spec := compiledProvider{
		Mode: ModeInclude,
		Patterns: []*regexp.Regexp{
			regexp.MustCompile(`^openai/`),
			regexp.MustCompile(`gpt-`),
		},
	}
	got := filterIDs([]string{"openai/gpt-4", "anthropic/claude", "foo-gpt-mini", "other"}, spec)
	want := []string{"openai/gpt-4", "foo-gpt-mini"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %v got %v", len(got), want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d: got %s want %s", i, got[i], want[i])
		}
	}
}

func TestFilterIDsIncludeEmptyKeepsNone(t *testing.T) {
	spec := compiledProvider{Mode: ModeInclude}
	got := filterIDs([]string{"a", "b"}, spec)
	if len(got) != 0 {
		t.Fatalf("expected none, got %v", got)
	}
}

func TestFilterIDsExclude(t *testing.T) {
	spec := compiledProvider{
		Mode:     ModeExclude,
		Patterns: []*regexp.Regexp{regexp.MustCompile(`embed`), regexp.MustCompile(`-vl$`)},
	}
	got := filterIDs([]string{"chat", "text-embed-1", "qwen-vl"}, spec)
	if len(got) != 1 || got[0] != "chat" {
		t.Fatalf("got %v", got)
	}
}

func TestFilterIDsExcludeEmptyKeepsAll(t *testing.T) {
	spec := compiledProvider{Mode: ModeExclude}
	got := filterIDs([]string{"a", "b"}, spec)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestMergeModelsKeepsAlias(t *testing.T) {
	existing := []ModelRef{{Name: "openai/gpt-4", Alias: "gpt4"}}
	got := mergeModels(existing, []string{"openai/gpt-4", "openai/gpt-5"}, true)
	if got[0].Alias != "gpt4" {
		t.Fatalf("alias=%s", got[0].Alias)
	}
	if got[1].Alias != "openai/gpt-5" {
		t.Fatalf("new alias=%s", got[1].Alias)
	}
}

func TestMergeModelsResetsAliasButPreservesMetadata(t *testing.T) {
	thinking := &ThinkingConfig{Levels: []string{"low", "high"}}
	existing := []ModelRef{{
		Name: "gpt-x", Alias: "old-alias", DisplayName: "GPT X",
		MaxContextLength: 128000, MaxInputTokens: 120000, MaxOutputTokens: 32000,
		Thinking: thinking, InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
	}}
	got := mergeModels(existing, []string{"gpt-x"}, false)
	if len(got) != 1 || got[0].Alias != "gpt-x" || got[0].DisplayName != "GPT X" {
		t.Fatalf("merged=%+v", got)
	}
	if got[0].MaxContextLength != 128000 || got[0].MaxInputTokens != 120000 || got[0].MaxOutputTokens != 32000 {
		t.Fatalf("limits=%+v", got[0])
	}
	if got[0].Thinking == nil || strings.Join(got[0].Thinking.Levels, ",") != "low,high" || strings.Join(got[0].InputModalities, ",") != "text,image" || strings.Join(got[0].OutputModalities, ",") != "text" {
		t.Fatalf("metadata=%+v", got[0])
	}
	got[0].Thinking.Levels[0] = "max"
	got[0].InputModalities[0] = "audio"
	if thinking.Levels[0] != "low" || existing[0].InputModalities[0] != "text" {
		t.Fatal("merged metadata aliases existing slices or pointers")
	}
}

func TestMergeModelsDedupe(t *testing.T) {
	got := mergeModels(nil, []string{"a", "a", "b"}, true)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestMergeModelsKeepsThinking(t *testing.T) {
	existing := []ModelRef{{Name: "gpt-5.6-sol", Alias: "sol", Thinking: &ThinkingConfig{Levels: []string{"low", "high"}}}}
	got := mergeModels(existing, []string{"gpt-5.6-sol"}, true)
	if got[0].Thinking == nil || strings.Join(got[0].Thinking.Levels, ",") != "low,high" {
		t.Fatalf("thinking=%v", got[0].Thinking)
	}
}
