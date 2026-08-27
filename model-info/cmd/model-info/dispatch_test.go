package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/xz-dev/cpa-plugin-mono/model-info/internal/plugin"
)

func TestManagementABIRoundTripAndRegistration(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	pluginService = plugin.New()
	configYAML := []byte("worker_token_env: TEST_WRITER_TOKEN\n")
	lifecycle, _ := json.Marshal(lifecycleRequest{ConfigYAML: configYAML})
	result, err := dispatch(pluginabi.MethodPluginRegister, lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	registration, ok := result.(registration)
	if !ok || !registration.Capabilities.ManagementAPI || len(registration.Metadata.ConfigFields) != 1 || registration.Metadata.ConfigFields[0].Name != "worker_token_env" {
		t.Fatalf("registration=%+v", result)
	}

	routes, err := dispatch(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := routes.(pluginapi.ManagementRegistrationResponse)
	if len(registered.Routes) != 5 || len(registered.Resources) != 1 {
		t.Fatalf("routes=%+v", registered)
	}

	managementRequest := pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/model-info/writer-status", Headers: http.Header{"X-Sync-Config-Writer-Token": []string{"worker-secret"}, "Authorization": []string{"Bearer ignored-secret"}}}
	requestJSON, _ := json.Marshal(managementRequest)
	response, err := dispatch(pluginabi.MethodManagementHandle, requestJSON)
	if err != nil {
		t.Fatal(err)
	}
	typed := response.(pluginapi.ManagementResponse)
	if typed.StatusCode != http.StatusOK || strings.Contains(string(typed.Body), "worker-secret") || strings.Contains(string(typed.Body), "ignored-secret") {
		t.Fatalf("response=%d %s", typed.StatusCode, typed.Body)
	}

	resourceRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/model-info/index.html"})
	resourceResult, err := dispatch(pluginabi.MethodManagementHandle, resourceRequest)
	if err != nil {
		t.Fatal(err)
	}
	resourceResponse := resourceResult.(pluginapi.ManagementResponse)
	if resourceResponse.StatusCode != http.StatusOK || resourceResponse.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(resourceResponse.Body), "<!doctype html>") {
		t.Fatalf("resource=%d %q %s", resourceResponse.StatusCode, resourceResponse.Headers.Get("Content-Type"), resourceResponse.Body)
	}

	envelope, fatal := handleMethod(pluginabi.MethodManagementHandle, requestJSON)
	if fatal {
		t.Fatalf("fatal envelope=%s", envelope)
	}
	var decoded pluginabi.Envelope
	if err := json.Unmarshal(envelope, &decoded); err != nil || !decoded.OK || len(decoded.Result) == 0 {
		t.Fatalf("envelope=%s err=%v", envelope, err)
	}
}
