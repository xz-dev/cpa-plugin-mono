package plugin

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type metadataSourcesTransport struct {
	modelparamsError error
	modelsdevError   error
}

func (t *metadataSourcesTransport) Do(method, url string, _ http.Header, _ []byte) (int, []byte, error) {
	switch url {
	case defaultModelparamsURL:
		if t.modelparamsError != nil {
			return 0, nil, t.modelparamsError
		}
		return http.StatusOK, []byte(`{"models":[
			{"provider":"openai","authType":"subscription","model":"gpt-x","params":[]},
			{"provider":"openai","authType":"api_key","model":"gpt-x","params":[]}
		]}`), nil
	case defaultModelsdevURL:
		if t.modelsdevError != nil {
			return 0, nil, t.modelsdevError
		}
		return http.StatusOK, []byte(`{"openrouter":{"models":{"gpt-x":{"id":"gpt-x","limit":{"context":1}}}}}`), nil
	default:
		return http.StatusNotFound, nil, nil
	}
}

func TestListMetadataSourcesIncludesExactIDs(t *testing.T) {
	service := New(&metadataSourcesTransport{})
	response := service.ListMetadataSources()
	want := []string{
		"modelparams.dev/openai/api_key",
		"modelparams.dev/openai/subscription",
		"models.dev/openrouter",
	}
	if len(response.Sources) != len(want) {
		t.Fatalf("sources=%+v", response.Sources)
	}
	for i, id := range want {
		if response.Sources[i].ID != id {
			t.Fatalf("source[%d]=%+v want %s", i, response.Sources[i], id)
		}
	}
	if response.Errors != nil {
		t.Fatalf("errors=%v", response.Errors)
	}
}

func TestMetadataSourcesManagementEndpoint(t *testing.T) {
	service := New(&metadataSourcesTransport{})
	response := service.HandleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/auto-pull-models/metadata-sources"})
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), "modelparams.dev/openai/subscription") || !strings.Contains(string(response.Body), "models.dev/openrouter") {
		t.Fatalf("response=%d %s", response.StatusCode, response.Body)
	}
}

func TestListMetadataSourcesReturnsPartialResultsAndSiteError(t *testing.T) {
	service := New(&metadataSourcesTransport{modelparamsError: errors.New("offline")})
	response := service.ListMetadataSources()
	if len(response.Sources) != 1 || response.Sources[0].ID != "models.dev/openrouter" {
		t.Fatalf("partial sources=%+v", response.Sources)
	}
	if response.Errors["modelparams.dev"] == "" {
		t.Fatalf("errors=%v", response.Errors)
	}
}
