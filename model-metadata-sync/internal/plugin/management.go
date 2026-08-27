package plugin

import (
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	planPath          = "/v0/management/plugins/model-metadata-sync/plan"
	writerStatusPath  = "/v0/management/plugins/model-metadata-sync/writer-status"
	workerTokenHeader = "X-Sync-Config-Writer-Token"
)

func (s *Service) ManagementRoutes() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{
		{Method: http.MethodPost, Path: planPath, Description: "Compute existing-model metadata proposal"},
		{Method: http.MethodGet, Path: writerStatusPath, Description: "Report Writer coordination status"},
	}}
}

func (s *Service) HandleManagement(request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if request.Path != planPath && request.Path != writerStatusPath {
		return jsonResponse(http.StatusNotFound, map[string]string{"error_code": errorInvalid})
	}
	token := request.Headers.Get(workerTokenHeader)
	switch {
	case request.Method == http.MethodGet && request.Path == writerStatusPath:
		status, ok := s.authorizedWorkerStatus(token)
		if !ok {
			return jsonResponse(http.StatusUnauthorized, map[string]string{"error_code": "unauthorized"})
		}
		return jsonResponse(http.StatusOK, status)
	case request.Method == http.MethodPost && request.Path == planPath:
		cfg, ok := s.beginAuthorizedPlan(token)
		if !ok {
			return jsonResponse(http.StatusUnauthorized, map[string]string{"error_code": "unauthorized"})
		}
		defer s.endPlan()
		result, code := s.executePlan(request.Body, cfg)
		if code != "" {
			return jsonResponse(errorStatus(code), map[string]string{"error_code": code})
		}
		return jsonResponse(http.StatusOK, result)
	default:
		if !s.authorized(token) {
			return jsonResponse(http.StatusUnauthorized, map[string]string{"error_code": "unauthorized"})
		}
		return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error_code": errorInvalid})
	}
}

func (s *Service) executePlan(raw []byte, cfg runtimeConfig) (any, string) {
	request, err := decodePlannerRequest(raw)
	if err != nil {
		return nil, errorInvalid
	}
	var attemptID string
	var ok bool
	if request.FetchResult == nil {
		attemptID, ok = s.startPlannerAttempt(cfg.Generation)
	} else {
		attemptID, ok = s.consumePlannerStep(request.FetchResult.RequestID, cfg.Generation)
	}
	if !ok {
		return nil, errorInvalid
	}
	cfg.AttemptID = attemptID
	result, code := planDecoded(request, cfg, s.host)
	if code != "" {
		s.abandonPlannerAttempt(attemptID, cfg.Generation)
		return nil, code
	}
	switch envelope := result.(type) {
	case fetchEnvelope:
		if !s.registerPlannerStep(attemptID, envelope.NextFetch.RequestID, cfg.Generation) {
			return nil, errorInvalid
		}
	case finalEnvelope:
		if !s.completePlannerAttempt(attemptID, cfg.Generation) {
			return nil, errorInvalid
		}
	default:
		s.abandonPlannerAttempt(attemptID, cfg.Generation)
		return nil, errorInvalid
	}
	return result, ""
}

func errorStatus(code string) int {
	switch code {
	case errorCredential:
		return http.StatusUnprocessableEntity
	case errorTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusBadRequest
	}
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte(`{"error_code":"provider_fetch_invalid"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: raw}
}
