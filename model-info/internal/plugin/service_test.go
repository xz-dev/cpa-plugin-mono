package plugin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeTransport struct{ body string }

type netError struct{ msg string }

func (e *netError) Error() string { return e.msg }

func (f fakeTransport) Do(method, url string, _ http.Header, _ []byte) (int, []byte, error) {
	if strings.Contains(url, "/v1/models") {
		return http.StatusOK, []byte(`{"models":[{"slug":"zproxy/glm-5.3","context_window":272000,"max_context_window":921000,"max_tokens":131072,"max_output_tokens":120000,"max_completion_tokens":110000,"supported_reasoning_levels":[{"effort":"low"},{"effort":"max"}]},{"slug":"gpt-x","context_window":1000,"max_context_window":1000,"max_input_tokens":900,"max_output_tokens":100,"max_completion_tokens":90},{"slug":"gpt-completion","context_window":2000,"max_completion_tokens":50},{"slug":"gpt-missing","context_window":3000}]}`), nil
	}
	return 0, nil, &netError{msg: "unexpected " + url}
}

func TestFetchParsesCatalog(t *testing.T) {
	s := New(fakeTransport{})
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"api_key_env":"MI_TEST_KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = s.Configure([]byte("config_file: " + cfgPath))
	t.Setenv("MI_TEST_KEY", "sk-test")
	c := s.FetchAndCache()
	if c.Error != "" || c.Count != 4 {
		t.Fatalf("catalog=%+v", c)
	}
	glm := c.Models[0]
	if glm.ID != "zproxy/glm-5.3" || glm.Provider != "zproxy" || glm.MaxInput != 921000 || glm.MaxTokens != 131072 {
		t.Fatalf("row=%+v", glm)
	}
	if len(glm.Levels) != 2 || glm.Levels[0] != "low" {
		t.Fatalf("levels=%v", glm.Levels)
	}
	if c.Models[1].MaxInput != 900 || c.Models[1].MaxTokens != 100 {
		t.Fatalf("explicit aliases not parsed: %+v", c.Models[1])
	}
	if c.Models[2].MaxInput != 0 || c.Models[2].MaxTokens != 50 {
		t.Fatalf("max_completion_tokens alias not parsed: %+v", c.Models[2])
	}
	if c.Models[3].MaxInput != 0 || c.Models[3].MaxTokens != 0 {
		t.Fatalf("missing maxima must stay 0: %+v", c.Models[3])
	}
}

func TestEffectiveFallsBackToContextForMissingMaxima(t *testing.T) {
	s := New(fakeTransport{})
	s.setLast(Catalog{Models: []ModelRow{
		{ID: "missing", Context: 2000},
		{ID: "native", Context: 3000, MaxInput: 2500, MaxTokens: 500},
	}})

	c := s.Effective()
	if c.Models[0].MaxInput != 2000 || c.Models[0].MaxTokens != 2000 || c.Models[0].MaxSource != "fallback-context" {
		t.Fatalf("fallback=%+v", c.Models[0])
	}
	if c.Models[1].MaxInput != 2500 || c.Models[1].MaxTokens != 500 || c.Models[1].MaxSource != "upstream" {
		t.Fatalf("native=%+v", c.Models[1])
	}
}

func TestFetchReportsKeyFailure(t *testing.T) {
	s := New(fakeTransport{})
	_ = s.Configure([]byte(`{}`))
	c := s.Fetch()
	if c.Error == "" || c.Models != nil {
		t.Fatalf("expected key error, got %+v", c)
	}
}
