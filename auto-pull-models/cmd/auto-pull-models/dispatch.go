package main

import (
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ManagementAPI bool `json:"management_api"`
	ModelProvider bool `json:"model_provider"`
}

func handleMethod(method string, request []byte) ([]byte, bool) {
	result, err := dispatch(method, request)
	if err != nil {
		return errorEnvelope("plugin_error", err.Error()), true
	}
	raw, err := okEnvelope(result)
	if err != nil {
		return errorEnvelope("encoding_error", err.Error()), true
	}
	return raw, false
}

func dispatch(method string, request []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		if err := pluginService.Configure(req.ConfigYAML); err != nil {
			return nil, err
		}
		return pluginRegistration(), nil
	case pluginabi.MethodManagementRegister:
		return pluginService.ManagementRoutes(), nil
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		return pluginService.HandleManagement(req), nil
	case pluginabi.MethodModelStatic:
		return pluginService.StaticModels(), nil
	case pluginabi.MethodModelForAuth:
		var req pluginapi.AuthModelRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		return pluginService.ModelsForAuth(req), nil
	default:
		return nil, &httpError{status: http.StatusNotImplemented, msg: "unknown plugin method: " + method}
	}
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Auto Pull Models",
			Version:          pluginVersion,
			Author:           "xz-dev",
			GitHubRepository: "https://github.com/xz-dev/cpa-plugin-mono",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "config_file", Type: pluginapi.ConfigFieldTypeString, Description: "Path to auto-pull-models JSON config. Default plugins/auto-pull-models/config.json"},
			},
		},
		Capabilities: registrationCapability{ManagementAPI: true, ModelProvider: true},
	}
}

func okEnvelope(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:    code,
			Message: message,
		},
	})
	return raw
}
