package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type fakeTransport struct{ body string }

type netError struct{ msg string }

func (e *netError) Error() string { return e.msg }

func (f fakeTransport) Do(method, url string, _ http.Header, _ []byte) (int, []byte, error) {
	if strings.Contains(url, "/api-call") {
		inner := `{"models":[{"slug":"zproxy/glm-5.3","context_window":272000,"max_tokens":131072,"supported_reasoning_levels":[{"effort":"low"},{"effort":"max"}]},{"slug":"gpt-x","context_window":1000}]}`
		envelope, _ := json.Marshal(map[string]any{"status_code": 200, "body": inner})
		return http.StatusOK, envelope, nil
	}
	return 0, nil, &netError{msg: "unexpected " + url}
}

func TestFetchParsesCatalog(t *testing.T) {
	s := New(fakeTransport{})
	_ = s.Configure([]byte(`{"management_key_env":"MI_TEST_KEY"}`))
	t.Setenv("MI_TEST_KEY", "mgmt-key")
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
