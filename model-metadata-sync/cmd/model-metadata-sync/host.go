package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/xz-dev/cpa-plugin-mono/model-metadata-sync/internal/plugin"
)

type hostAuth struct{}

type authListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

func (hostAuth) List() ([]plugin.AuthEntry, error) {
	raw, err := doHostCall(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response authListResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode host auth list")
	}
	result := make([]plugin.AuthEntry, 0, len(response.Files))
	for _, entry := range response.Files {
		result = append(result, fromHostAuthEntry(entry))
	}
	return result, nil
}

func (hostAuth) GetRuntime(authIndex string) (plugin.AuthEntry, error) {
	raw, err := doHostCall(pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return plugin.AuthEntry{}, err
	}
	var response pluginapi.HostAuthGetRuntimeResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return plugin.AuthEntry{}, fmt.Errorf("decode host auth runtime")
	}
	return fromHostAuthEntry(response.Auth), nil
}

func (hostAuth) Get(authIndex string) (plugin.AuthPhysical, error) {
	raw, err := doHostCall(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return plugin.AuthPhysical{}, err
	}
	var response pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return plugin.AuthPhysical{}, fmt.Errorf("decode host auth file")
	}
	return plugin.AuthPhysical{AuthIndex: response.AuthIndex, Path: response.Path, JSON: append([]byte(nil), response.JSON...)}, nil
}

func fromHostAuthEntry(entry pluginapi.HostAuthFileEntry) plugin.AuthEntry {
	provider := strings.TrimSpace(entry.Provider)
	if provider == "" {
		provider = strings.TrimSpace(entry.Type)
	}
	return plugin.AuthEntry{
		AuthIndex:   entry.AuthIndex,
		Provider:    provider,
		Status:      entry.Status,
		Disabled:    entry.Disabled,
		Unavailable: entry.Unavailable,
		RuntimeOnly: entry.RuntimeOnly,
		Source:      entry.Source,
		Path:        entry.Path,
	}
}

var _ plugin.AuthHost = hostAuth{}
