package plugin

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	ingestPath           = "/v0/management/plugins/model-info/ingest"
	writerStatusPath     = "/v0/management/plugins/model-info/writer-status"
	catalogPath          = "/v0/management/plugins/model-info/catalog"
	lastPath             = "/v0/management/plugins/model-info/last"
	effectivePath        = "/v0/management/plugins/model-info/effective"
	resourceIndexPath    = "/v0/resource/plugins/model-info/index.html"
	workerTokenHeader    = "X-Sync-Config-Writer-Token"
	maxCatalogBytes      = 8 << 20
	maxIngestBodyBytes   = 12 << 20
	errorCatalogInvalid  = "catalog_invalid"
	errorCatalogTooLarge = "catalog_too_large"
)

//go:embed ui.html
var uiHTML []byte

type ingestRequest struct {
	CatalogBase64 string `json:"catalog_base64"`
}

type ingestReceipt struct {
	Count         int    `json:"count"`
	CatalogSHA256 string `json:"catalog_sha256"`
}

func (s *Service) ManagementRoutes() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodPost, Path: ingestPath, Description: "Validate and atomically ingest a Writer-fetched Codex model catalog"},
			{Method: http.MethodGet, Path: writerStatusPath, Description: "Report Writer coordination status"},
			{Method: http.MethodGet, Path: catalogPath, Description: "Read cached Codex model catalog"},
			{Method: http.MethodGet, Path: lastPath, Description: "Read cached Codex model catalog"},
			{Method: http.MethodGet, Path: effectivePath, Description: "Read cached model limits with context fallback"},
		},
		Resources: []pluginapi.ResourceRoute{{Path: "/index.html", Menu: "Model Info", Description: "查看缓存模型的上下文窗口、最大输入/输出、推理等级、模态"}},
	}
}

func (s *Service) HandleManagement(request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch {
	case request.Method == http.MethodGet && request.Path == writerStatusPath:
		status, ok := s.workerStatus(request.Headers.Get(workerTokenHeader))
		if !ok {
			return errorResponse(http.StatusUnauthorized, "unauthorized")
		}
		return jsonResponse(http.StatusOK, status)
	case request.Method == http.MethodPost && request.Path == ingestPath:
		return s.handleIngest(request)
	case request.Method == http.MethodGet && (request.Path == catalogPath || request.Path == lastPath):
		return jsonResponse(http.StatusOK, s.Last())
	case request.Method == http.MethodGet && request.Path == effectivePath:
		return jsonResponse(http.StatusOK, s.Effective())
	case request.Method == http.MethodGet && request.Path == resourceIndexPath:
		return uiResponse()
	default:
		return errorResponse(http.StatusNotFound, "not_found")
	}
}

func (s *Service) handleIngest(request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	authorization, ok := s.authorize(request.Headers.Get(workerTokenHeader))
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized")
	}
	if len(request.Body) > maxIngestBodyBytes {
		return errorResponse(http.StatusRequestEntityTooLarge, errorCatalogTooLarge)
	}
	envelope, err := decodeExactIngestRequest(request.Body)
	if err != nil {
		return errorResponse(http.StatusBadRequest, errorCatalogInvalid)
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(envelope.CatalogBase64)))
	count, err := base64.StdEncoding.Strict().Decode(decoded, []byte(envelope.CatalogBase64))
	if err != nil {
		return errorResponse(http.StatusBadRequest, errorCatalogInvalid)
	}
	decoded = decoded[:count]
	if envelope.CatalogBase64 != base64.StdEncoding.EncodeToString(decoded) {
		return errorResponse(http.StatusBadRequest, errorCatalogInvalid)
	}
	if len(decoded) > maxCatalogBytes {
		return errorResponse(http.StatusRequestEntityTooLarge, errorCatalogTooLarge)
	}
	catalog, err := parseCatalog(decoded)
	if err != nil {
		return errorResponse(http.StatusBadRequest, errorCatalogInvalid)
	}
	if !s.replaceCatalog(authorization, catalog) {
		return errorResponse(http.StatusUnauthorized, "unauthorized")
	}
	sum := sha256.Sum256(decoded)
	return jsonResponse(http.StatusOK, ingestReceipt{Count: catalog.Count, CatalogSHA256: hex.EncodeToString(sum[:])})
}

func decodeExactIngestRequest(raw []byte) (ingestRequest, error) {
	invalid := func() (ingestRequest, error) { return ingestRequest{}, io.ErrUnexpectedEOF }
	if !utf8.Valid(raw) {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') || !decoder.More() {
		return invalid()
	}
	key, err := decoder.Token()
	if err != nil || key != "catalog_base64" {
		return invalid()
	}
	value, err := decoder.Token()
	catalogBase64, ok := value.(string)
	if err != nil || !ok || decoder.More() {
		return invalid()
	}
	envelope := ingestRequest{CatalogBase64: catalogBase64}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return invalid()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid()
	}
	return envelope, nil
}

func uiResponse() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: uiHTML}
}

func errorResponse(status int, code string) pluginapi.ManagementResponse {
	return jsonResponse(status, map[string]string{"error_code": code})
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: raw}
}
