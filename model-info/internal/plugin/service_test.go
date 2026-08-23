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
		return http.StatusOK, []byte(`{"models":[{"slug":"zproxy/glm-5.3","context_window":272000,"max_tokens":131072,"supported_reasoning_levels":[{"effort":"low"},{"effort":"max"}]},{"slug":"gpt-x","context_window":1000}]}`), nil
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
	if c.Error != "" || c.Count != 2 {
		t.Fatalf("catalog=%+v", c)
	}
	glm := c.Models[0]
	if glm.ID != "zproxy/glm-5.3" || glm.Provider != "zproxy" || glm.MaxTokens != 131072 {
		t.Fatalf("row=%+v", glm)
	}
	if len(glm.Levels) != 2 || glm.Levels[0] != "low" {
		t.Fatalf("levels=%v", glm.Levels)
	}
	if c.Models[1].MaxTokens != 0 {
		t.Fatalf("missing max must stay 0")
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
