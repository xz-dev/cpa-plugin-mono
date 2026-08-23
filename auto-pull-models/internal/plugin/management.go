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
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/v0/management/plugins/auto-pull-models/status", Description: "Last auto-pull-models sync report"},
			{Method: http.MethodGet, Path: "/v0/management/plugins/auto-pull-models/json", Description: "Read auto-pull-models JSON config"},
			{Method: http.MethodPut, Path: "/v0/management/plugins/auto-pull-models/json", Description: "Write auto-pull-models JSON config"},
			{Method: http.MethodGet, Path: "/v0/management/plugins/auto-pull-models/compat-providers", Description: "List CPA openai-compatibility providers"},
			{Method: http.MethodPost, Path: "/v0/management/plugins/auto-pull-models/preview", Description: "Dry-run model sync without writing"},
			{Method: http.MethodPost, Path: "/v0/management/plugins/auto-pull-models/sync", Description: "Run model list sync now"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/index.html", Menu: "Auto Pull Models", Description: "选择 provider、填写规则、试运行后再同步模型列表"},
		},
	}
}

func (s *Service) HandleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimSpace(req.Path)
	key := bearerKey(req.Headers)
	if key == "" {
		key = resolveManagementKey(s.Current())
	}
	only := strings.TrimSpace(req.Query.Get("provider"))
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/status"):
		return jsonResponse(http.StatusOK, s.statusPayload())
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/compat-providers"):
		list, err := s.ListCompatSummaries(key)
		if err != nil {
			return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"providers": list})
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
		status := http.StatusOK
		if !report.OK {
			status = http.StatusBadGateway
		}
		return jsonResponse(status, report)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/sync"):
		if len(req.Body) > 0 {
			if err := s.SaveJSON(req.Body); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
		}
		report := s.SyncWithKey(key, only)
		status := http.StatusOK
		if !report.OK {
			status = http.StatusBadGateway
		}
		return jsonResponse(status, report)
	default:
		return s.uiResponse()
	}
}

func (s *Service) statusPayload() map[string]any {
	cfg := s.Current()
	names := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		names = append(names, p.Name)
	}
	return map[string]any{
		"config_file": s.JSONPath(),
		"interval":    cfg.Raw.Interval,
		"write_mode":  cfg.WriteMode,
		"config_path": cfg.ConfigPath,
		"providers":   names,
		"has_key":     resolveManagementKey(cfg) != "",
		"last":        s.Last(),
	}
}

func (s *Service) readConfigResponse() pluginapi.ManagementResponse {
	path := s.JSONPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			raw, _ = json.MarshalIndent(defaultFileConfig(), "", "  ")
			raw = append(raw, '\n')
			return pluginapi.ManagementResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
				Body:       raw,
			}
		}
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func (s *Service) uiResponse() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       uiHTML,
	}
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(v)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func bearerKey(h http.Header) string {
	if h == nil {
		return ""
	}
	val := strings.TrimSpace(h.Get("Authorization"))
	if val == "" {
		val = strings.TrimSpace(h.Get("authorization"))
	}
	const prefix = "Bearer "
	if strings.HasPrefix(val, prefix) {
		return strings.TrimSpace(val[len(prefix):])
	}
	if strings.HasPrefix(strings.ToLower(val), "bearer ") {
		return strings.TrimSpace(val[7:])
	}
	return ""
}
