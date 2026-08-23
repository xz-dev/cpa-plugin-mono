package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type enrichTransport struct {
	t         *testing.T
	manifest  string
	modelsdev string
}

func (t *enrichTransport) Do(method, url string, _ http.Header, _ []byte) (int, []byte, error) {
	switch {
	case method == http.MethodGet && strings.HasSuffix(url, "/v0/management/openai-compatibility"):
		return http.StatusOK, []byte(`{"openai-compatibility":[{"name":"ZCode","base-url":"http://zcode-proxy:8080/v1","api-key-entries":[{"auth-index":"zcode"}],"models":[]}]}`), nil
	case method == http.MethodPost && strings.HasSuffix(url, "/v0/management/api-call"):
		response, err := json.Marshal(apiCallResponse{StatusCode: http.StatusOK, Body: t.manifest})
		if err != nil {
			t.t.Fatal(err)
		}
		return http.StatusOK, response, nil
	case method == http.MethodPatch && strings.HasSuffix(url, "/v0/management/openai-compatibility"):
		return http.StatusOK, []byte(`{}`), nil
	case strings.Contains(url, "models.dev"):
		return http.StatusOK, []byte(t.modelsdev), nil
	default:
		t.t.Fatalf("unexpected request: %s %s", method, url)
		return 0, nil, nil
	}
}

func newEnrichTestService(t *testing.T) (*Service, *enrichTransport) {
	transport := &enrichTransport{
		t: t,
		manifest: `{"models":[
			{"slug":"glm-5.3","context_window":272000,"max_tokens":null},
			{"slug":"gpt-5.6-sol","context_window":272000,"max_tokens":128000}
		]}`,
		modelsdev: `{
			"zai-org": {"models": {"glm-5.3": {"id": "glm-5.3", "limit": {"context": 1048576, "output": 131072}}}},
			"openai": {"models": {"gpt-5.6-sol": {"id": "gpt-5.6-sol", "limit": {"context": 400000, "output": 100000}}}}
		}`,
	}
	service := New(transport)
	service.cfg = runtimeConfig{
		ManagementBaseURL: "http://cpa:8317",
		Providers: []compiledProvider{{
			Name:         "ZCode",
			Enabled:      true,
			Mode:         ModeExclude,
			UpstreamMeta: true,
			Modelsdev:    true,
		}},
	}
	return service, transport
}

func TestEnrichedUpstreamWinsOverModelsdev(t *testing.T) {
	service, _ := newEnrichTestService(t)
	report := service.Sync("key", "ZCode")
	if !report.OK {
		t.Fatalf("report=%+v", report)
	}
	models := service.enrichedModels("ZCode")
	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	// glm-5.3: upstream max_tokens null → models.dev fills 131072; ctx upstream 272000 wins over dev 1048576? No: upstream ctx is present, dev only fills missing.
	glm := byID["glm-5.3"]
	if glm.ContextLength != 272000 {
		t.Fatalf("glm-5.3 ctx: upstream must win, got %d", glm.ContextLength)
	}
	if glm.MaxCompletionTokens != 131072 {
		t.Fatalf("glm-5.3 max tokens: models.dev must fill gap, got %d", glm.MaxCompletionTokens)
	}
	// gpt-5.6-sol: upstream has both → dev ignored.
	gpt := byID["gpt-5.6-sol"]
	if gpt.ContextLength != 272000 || gpt.MaxCompletionTokens != 128000 {
		t.Fatalf("gpt-5.6-sol: upstream must win both, got ctx=%d max=%d", gpt.ContextLength, gpt.MaxCompletionTokens)
	}
}

func TestModelsForAuthDeclinesUnmanagedAndServesManaged(t *testing.T) {
	service, _ := newEnrichTestService(t)
	service.cfg.ManagementKeyEnv = "APM_TEST_KEY"
	t.Setenv("APM_TEST_KEY", "key")

	declined := service.ModelsForAuth(pluginapi.AuthModelRequest{
		Attributes: map[string]string{"compat_name": "OtherGateway"},
	})
	if declined.Provider == compatProviderKey || declined.Provider == "" {
		t.Fatalf("unmanaged provider must decline via mismatched provider key, got %q", declined.Provider)
	}

	served := service.ModelsForAuth(pluginapi.AuthModelRequest{
		Attributes: map[string]string{"compat_name": "zcode"},
	})
	if served.Provider != compatProviderKey {
		t.Fatalf("managed provider must be served under %q, got %q", compatProviderKey, served.Provider)
	}
	if len(served.Models) != 2 {
		t.Fatalf("models=%+v", served.Models)
	}
}

func TestStaticModelsEmptyWithoutSync(t *testing.T) {
	service, _ := newEnrichTestService(t)
	// No management key available → ensureEnriched is a no-op → empty response
	// means the host keeps native registration untouched.
	resp := service.StaticModels()
	if resp.Provider != "" || len(resp.Models) != 0 {
		t.Fatalf("expected empty static response, got %+v", resp)
	}
}
