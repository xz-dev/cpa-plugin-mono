package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/xz-dev/cpa-plugin-mono/sync-config-write/internal/plugin"
)

func TestDispatchPreservesServiceAcrossLifecycleAndRoutesManagement(t *testing.T) {
	t.Setenv("WRITER_MANAGEMENT_KEY", "management-secret")
	t.Setenv("WRITER_WORKER_TOKEN", "worker-secret")
	pluginService.Shutdown()
	pluginService = plugin.New(nil)
	t.Cleanup(pluginService.Shutdown)

	configA := []byte(`enabled: true
priority: 7
store:
  version: 0.1.0
core_origin: http://127.0.0.1:8317
management_key_env: WRITER_MANAGEMENT_KEY
model_info_proxy_api_key_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
worker_token_env: WRITER_WORKER_TOKEN
auto_pull_interval: 5m
metadata_sync_interval: 1h
model_info_interval: 0s
max_version_retries: 2
sync_epoch: epoch-a
`)
	registerRequest, err := json.Marshal(lifecycleRequest{ConfigYAML: configA})
	if err != nil {
		t.Fatal(err)
	}
	registerEnvelope := dispatchEnvelope(t, pluginabi.MethodPluginRegister, registerRequest)
	var registered registration
	if err := json.Unmarshal(registerEnvelope.Result, &registered); err != nil {
		t.Fatal(err)
	}
	if registered.SchemaVersion != pluginabi.SchemaVersion || !registered.Capabilities.ManagementAPI {
		t.Fatalf("registration=%+v", registered)
	}
	before := pluginService.Status()

	configB := []byte(strings.Replace(string(configA), "sync_epoch: epoch-a", "sync_epoch: epoch-b", 1))
	reconfigureRequest, err := json.Marshal(lifecycleRequest{ConfigYAML: configB})
	if err != nil {
		t.Fatal(err)
	}
	dispatchEnvelope(t, pluginabi.MethodPluginReconfigure, reconfigureRequest)
	after := pluginService.Status()
	if after.InstanceID != before.InstanceID || after.ReconfigureSeq != before.ReconfigureSeq+1 || after.ConfigSHA256 == before.ConfigSHA256 {
		t.Fatalf("service was replaced or not reconfigured: before=%+v after=%+v", before, after)
	}

	managementRegistration := dispatchEnvelope(t, pluginabi.MethodManagementRegister, nil)
	var routes pluginapi.ManagementRegistrationResponse
	if err := json.Unmarshal(managementRegistration.Result, &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes.Routes) != 5 {
		t.Fatalf("management routes=%+v", routes.Routes)
	}

	managementRequest, err := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/sync-config-write/status",
	})
	if err != nil {
		t.Fatal(err)
	}
	managementEnvelope := dispatchEnvelope(t, pluginabi.MethodManagementHandle, managementRequest)
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(managementEnvelope.Result, &response); err != nil {
		t.Fatal(err)
	}
	var status plugin.StatusResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &status) != nil {
		t.Fatalf("management response=%+v", response)
	}
	if status.InstanceID != before.InstanceID || status.ReconfigureSeq != after.ReconfigureSeq {
		t.Fatalf("management reached wrong service: %+v", status)
	}
}

func TestDispatchReturnsErrorEnvelopeForUnknownMethod(t *testing.T) {
	raw, fatal := handleMethod("unknown.method", nil)
	if !fatal {
		t.Fatal("unknown method must fail")
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "plugin_error" {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func dispatchEnvelope(t *testing.T, method string, request []byte) pluginabi.Envelope {
	t.Helper()
	raw, fatal := handleMethod(method, request)
	if fatal {
		t.Fatalf("%s returned fatal envelope: %s", method, raw)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Error != nil {
		t.Fatalf("%s envelope=%+v", method, envelope)
	}
	return envelope
}
