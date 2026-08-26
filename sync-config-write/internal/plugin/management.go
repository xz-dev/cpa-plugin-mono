package plugin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	runAutoPullPath = "/v0/management/plugins/sync-config-write/run/auto-pull-models"
	runMetadataPath = "/v0/management/plugins/sync-config-write/run/model-metadata-sync"
	modelInfoPath   = "/v0/management/plugins/sync-config-write/model-info/catalog"
	reconcilePath   = "/v0/management/plugins/sync-config-write/reconcile"
	statusPath      = "/v0/management/plugins/sync-config-write/status"
)

type triggerResponse struct {
	RunID     string `json:"run_id"`
	Coalesced bool   `json:"coalesced,omitempty"`
}

func (s *Service) ManagementRoutes() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{
		{Method: http.MethodPost, Path: runAutoPullPath, Description: "Queue membership synchronization"},
		{Method: http.MethodPost, Path: runMetadataPath, Description: "Queue metadata synchronization"},
		{Method: http.MethodPost, Path: modelInfoPath, Description: "Queue model catalog refresh"},
		{Method: http.MethodPost, Path: reconcilePath, Description: "Queue writer reconciliation"},
		{Method: http.MethodGet, Path: statusPath, Description: "Read writer run status"},
	}}
}

func (s *Service) HandleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimSpace(req.Path)
	if req.Method == http.MethodGet && path == statusPath {
		if id := strings.TrimSpace(req.Query.Get("run_id")); id != "" {
			status := s.statusByID(id)
			if status == nil {
				return jsonResponse(http.StatusNotFound, map[string]string{"error_code": "run_not_found"})
			}
			return jsonResponse(http.StatusOK, status)
		}
		return jsonResponse(http.StatusOK, s.statusWithRuns())
	}
	var operation Operation
	switch {
	case req.Method == http.MethodPost && path == runAutoPullPath:
		operation = OperationAutoPull
	case req.Method == http.MethodPost && path == runMetadataPath:
		operation = OperationMetadataSync
	case req.Method == http.MethodPost && path == modelInfoPath:
		operation = OperationModelInfo
	case req.Method == http.MethodPost && path == reconcilePath:
		operation = OperationReconcile
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error_code": "not_found"})
	}
	id, coalesced, code := s.enqueue(operation)
	if code != "" {
		return jsonResponse(http.StatusConflict, map[string]any{"error_code": CodeWriterBlocked, "blocking_run_id": id, "blocking_error_code": code})
	}
	return jsonResponse(http.StatusAccepted, triggerResponse{RunID: id, Coalesced: coalesced})
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: raw}
}
