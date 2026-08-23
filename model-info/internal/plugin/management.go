package plugin

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed ui.html
var uiHTML []byte

func (s *Service) ManagementRoutes() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/v0/management/plugins/model-info/catalog", Description: "Fetch the current Codex-client model catalog with limits and reasoning levels"},
			{Method: http.MethodGet, Path: "/v0/management/plugins/model-info/last", Description: "Last fetched catalog (cached)"},
			{Method: http.MethodGet, Path: "/v0/management/plugins/model-info/effective", Description: "Effective limits after downstream fallback (context-window bound when max_tokens missing)"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/index.html", Menu: "Model Info", Description: "查看所有模型的上下文窗口、输出上限、推理等级、模态"},
		},
	}
}

func (s *Service) HandleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimSpace(req.Path)
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/last"):
		return jsonResponse(http.StatusOK, s.Last())
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/effective"):
		return jsonResponse(http.StatusOK, s.Effective())
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/catalog"):
		c := s.FetchAndCache()
		status := http.StatusOK
		if c.Error != "" {
			status = http.StatusBadGateway
		}
		return jsonResponse(status, c)
	default:
		return s.uiResponse()
	}
}

func (s *Service) uiResponse() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       uiHTML,
	}
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}
