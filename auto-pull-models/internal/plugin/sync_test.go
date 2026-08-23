package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type codexSyncTransport struct {
	t          *testing.T
	manifest   string
	apiCallURL string
	patched    []ModelRef
}

func (t *codexSyncTransport) Do(method, url string, _ http.Header, body []byte) (int, []byte, error) {
	switch {
	case method == http.MethodGet && strings.HasSuffix(url, "/v0/management/openai-compatibility"):
		return http.StatusOK, []byte(`{"openai-compatibility":[{"name":"ZCode","base-url":"http://zcode-proxy:8080/v1","api-key-entries":[{"auth-index":"zcode"}],"models":[]}]}`), nil
	case method == http.MethodPost && strings.HasSuffix(url, "/v0/management/api-call"):
		var req apiCallRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.t.Fatal(err)
		}
		t.apiCallURL = req.URL
		response, err := json.Marshal(apiCallResponse{StatusCode: http.StatusOK, Body: t.manifest})
		if err != nil {
			t.t.Fatal(err)
		}
		return http.StatusOK, response, nil
	case method == http.MethodPatch && strings.HasSuffix(url, "/v0/management/openai-compatibility"):
		var payload struct {
			Value struct {
				Models []ModelRef `json:"models"`
			} `json:"value"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.t.Fatal(err)
		}
		t.patched = payload.Value.Models
		return http.StatusOK, []byte(`{}`), nil
	default:
		t.t.Fatalf("unexpected request: %s %s", method, url)
		return 0, nil, nil
	}
}

func TestSyncConsumesCodexManifest(t *testing.T) {
	transport := &codexSyncTransport{
		t: t,
		manifest: `{"models":[
			{"slug":"glm-5.3","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]},
			{"slug":"glm-5.3[1m]","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]}
		]}`,
	}
	service := New(transport)
	service.cfg = runtimeConfig{
		ManagementBaseURL:   "http://cpa:8317",
		KeepExistingAliases: true,
		Providers: []compiledProvider{{
			Name:          "ZCode",
			Enabled:       true,
			Mode:          ModeExclude,
			UpstreamMeta:  true,
			CodexManifest: true,
		}},
	}

	report := service.Sync("management-key", "ZCode")
	if !report.OK || len(report.Providers) != 1 || report.Providers[0].ThinkingMatched != 2 {
		t.Fatalf("report=%+v", report)
	}
	if transport.apiCallURL != "http://zcode-proxy:8080/v1/models?client_version=1.0.0" {
		t.Fatalf("api-call URL=%s", transport.apiCallURL)
	}
	if len(transport.patched) != 2 {
		t.Fatalf("patched=%v", transport.patched)
	}
	for _, model := range transport.patched {
		if model.Thinking == nil || strings.Join(model.Thinking.Levels, ",") != "low,high,max" {
			t.Fatalf("model=%+v", model)
		}
	}
}
