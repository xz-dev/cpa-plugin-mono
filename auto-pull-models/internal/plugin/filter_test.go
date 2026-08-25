package plugin

import "testing"

func TestFilterIDs(t *testing.T) {
	include, err := compileConfig(FileConfig{Channels: []ChannelConfig{{Selector: ChannelSelector{Name: "x", BaseURL: "https://example.com/v1"}, Mode: ModeInclude, Patterns: []string{"^gpt-"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := filterIDs([]string{"gpt-5", "embed-1"}, include.Channels[0]); len(got) != 1 || got[0] != "gpt-5" {
		t.Fatalf("include=%v", got)
	}
	exclude, err := compileConfig(FileConfig{Channels: []ChannelConfig{{Selector: ChannelSelector{Name: "x", BaseURL: "https://example.com/v1"}, Mode: ModeExclude}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := filterIDs([]string{"gpt-5", "embed-1"}, exclude.Channels[0]); len(got) != 2 {
		t.Fatalf("exclude=%v", got)
	}
}
