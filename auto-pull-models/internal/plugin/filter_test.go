package plugin

import (
	"regexp"
	"testing"
)

func TestFilterIDs(t *testing.T) {
	include := compiledChannel{Mode: ModeInclude, Patterns: []*regexp.Regexp{regexp.MustCompile(`^gpt-`)}}
	if got := filterIDs([]string{"gpt-5", "embed-1"}, include); len(got) != 1 || got[0] != "gpt-5" {
		t.Fatalf("include=%v", got)
	}
	exclude := compiledChannel{Mode: ModeExclude}
	if got := filterIDs([]string{"gpt-5", "embed-1"}, exclude); len(got) != 2 {
		t.Fatalf("exclude=%v", got)
	}
}
