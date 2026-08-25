package plugin

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed ui.html
var uiHTML []byte

func (s *Service) ManagementRoutes() pluginapi.ManagementRegistrationResponse {
	base := "/v0/management/plugins/model-metadata-sync/"
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: base + "status", Description: "Last metadata sync report"},
			{Method: http.MethodGet, Path: base + "json", Description: "Read metadata sync config"},
			{Method: http.MethodPut, Path: base + "json", Description: "Write metadata sync config"},
			{Method: http.MethodGet, Path: base + "channels", Description: "List sanitized model channels"},
			{Method: http.MethodGet, Path: base + "metadata-sources", Description: "List external metadata source identities"},
			{Method: http.MethodPost, Path: base + "preview", Description: "Preview metadata patches"},
			{Method: http.MethodPost, Path: base + "sync", Description: "Run metadata sync"},
		},
		Resources: []pluginapi.ResourceRoute{{Path: "/index.html", Menu: "Model Metadata Sync", Description: "Enrich existing channel models without changing membership"}},
	}
}

func (s *Service) HandleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimSpace(req.Path)
	key := bearerKey(req.Headers)
	if key == "" {
		key = resolveManagementKey(s.Current())
	}
	only := strings.TrimSpace(req.Query.Get("channel"))
	if only == "" {
		only = strings.TrimSpace(req.Query.Get("provider"))
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/status"):
		return jsonResponse(http.StatusOK, s.statusPayload())
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/channels"):
		channels, err := s.ListChannelSummaries(key)
		if err != nil {
			return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"channels": channels})
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/metadata-sources"):
		return jsonResponse(http.StatusOK, s.ListMetadataSources())
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/json"):
		return s.readConfigResponse()
	case req.Method == http.MethodPut && strings.HasSuffix(path, "/json"):
		if err := s.SaveJSON(req.Body); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]string{"status": "ok"})
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/preview"):
		report, err := s.PreviewWithKey(key, only, req.Body)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return jsonResponse(reportStatus(report.OK), report)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/sync"):
		if len(req.Body) > 0 {
			if err := s.SaveJSON(req.Body); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
		}
		report := s.SyncWithKey(key, only)
		return jsonResponse(reportStatus(report.OK), report)
	default:
		return s.uiResponse()
	}
}

func reportStatus(ok bool) int {
	if ok {
		return http.StatusOK
	}
	return http.StatusBadGateway
}
func (s *Service) statusPayload() map[string]any {
	cfg := s.Current()
	selectors := []map[string]any{}
	for _, channel := range cfg.Channels {
		selectors = append(selectors, map[string]any{"kind": channel.Kind, "selector": channel.Selector})
	}
	return map[string]any{"config_file": s.JSONPath(), "interval": cfg.Raw.Interval, "channels": selectors, "has_key": resolveManagementKey(cfg) != "", "last": s.Last()}
}
func (s *Service) readConfigResponse() pluginapi.ManagementResponse {
	raw, err := os.ReadFile(s.JSONPath())
	if err != nil {
		if !os.IsNotExist(err) {
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		raw, _ = json.MarshalIndent(defaultFileConfig(), "", "  ")
		raw = append(raw, '\n')
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: raw}
}
func (s *Service) uiResponse() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: uiHTML}
}
func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: raw}
}
func bearerKey(headers http.Header) string {
	value := strings.TrimSpace(headers.Get("Authorization"))
	if len(value) >= 7 && strings.EqualFold(value[:7], "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}
