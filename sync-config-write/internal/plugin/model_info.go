package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	modelInfoCatalogPath = "/v1/models?client_version=1.0.0"
	modelInfoIngestPath  = "/v0/management/plugins/model-info/ingest"
	maxCatalogBytes      = 8 << 20
)

type ModelInfoRefresher struct {
	client *http.Client
	engine *CommitEngine
}

func NewModelInfoRefresher(client *http.Client, engine *CommitEngine) *ModelInfoRefresher {
	if client == nil {
		client = NewLoopbackClient()
	}
	return &ModelInfoRefresher{client: client, engine: engine}
}

func (r *ModelInfoRefresher) Refresh(ctx context.Context, settings Settings, progress func(RunState)) Outcome {
	if r == nil || r.engine == nil || r.client == nil {
		return Outcome{State: StateFailed, Code: CodeNotImplemented}
	}
	raw, code := r.engine.getConfig(ctx, settings)
	if code != "" {
		return Outcome{State: StateFailed, Code: code}
	}
	key, ok := selectCatalogAPIKey(raw, settings.ModelInfoKeyFingerprint)
	if !ok {
		return Outcome{State: StateFailed, Code: CodeCatalogKeyUnavailable}
	}
	if progress != nil {
		progress(StateFetching)
	}
	req, err := NewCoreRequest(ctx, settings, http.MethodGet, modelInfoCatalogPath, nil)
	if err != nil {
		return Outcome{State: StateFailed, Code: CodeCoreUnavailable}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	response, err := r.client.Do(req)
	if err != nil {
		if isTimeout(err, ctx) {
			return Outcome{State: StateFailed, Code: CodeLoopbackTimeout}
		}
		return Outcome{State: StateFailed, Code: CodeCoreUnavailable}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Outcome{State: StateFailed, Code: CodeCoreUnavailable}
	}
	catalog, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil {
		return Outcome{State: StateFailed, Code: CodeCoreUnavailable}
	}
	if len(catalog) > maxCatalogBytes {
		return Outcome{State: StateFailed, Code: CodeCatalogTooLarge}
	}
	latestRaw, latestCode := r.engine.getConfig(ctx, settings)
	if latestCode != "" {
		return Outcome{State: StateFailed, Code: latestCode}
	}
	if latestVersion := configVersion(latestRaw); latestVersion != configVersion(raw) {
		return Outcome{State: StateFailed, Code: CodeVersionConflict, Version: latestVersion}
	}
	ingestBody, err := json.Marshal(struct {
		CatalogBase64 string `json:"catalog_base64"`
	}{CatalogBase64: base64.StdEncoding.EncodeToString(catalog)})
	if err != nil {
		return Outcome{State: StateFailed, Code: CodeCatalogInvalid}
	}
	ingest, err := NewCoreRequest(ctx, settings, http.MethodPost, modelInfoIngestPath, ingestBody)
	if err != nil {
		return Outcome{State: StateFailed, Code: CodeCoreUnavailable}
	}
	ingest.Header.Set("Content-Type", "application/json")
	ingest.Header.Set(workerTokenHeader, settings.WorkerToken)
	ingestResponse, err := r.client.Do(ingest)
	if err != nil {
		if isTimeout(err, ctx) {
			return Outcome{State: StateFailed, Code: CodeLoopbackTimeout}
		}
		return Outcome{State: StateFailed, Code: CodeCoreUnavailable}
	}
	defer ingestResponse.Body.Close()
	if ingestResponse.StatusCode >= 200 && ingestResponse.StatusCode < 300 {
		var receipt struct {
			Count         *int    `json:"count"`
			CatalogSHA256 *string `json:"catalog_sha256"`
		}
		if err := decodeStrictJSON(ingestResponse.Body, 4096, &receipt); err != nil || receipt.Count == nil || *receipt.Count < 0 || receipt.CatalogSHA256 == nil || *receipt.CatalogSHA256 != configVersion(catalog) {
			return Outcome{State: StateFailed, Code: CodeCatalogInvalid}
		}
		return Outcome{State: StateSucceeded, Version: configVersion(raw)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(ingestResponse.Body, 4096))
	switch ingestResponse.StatusCode {
	case http.StatusBadRequest:
		return Outcome{State: StateFailed, Code: CodeCatalogInvalid}
	case http.StatusRequestEntityTooLarge:
		return Outcome{State: StateFailed, Code: CodeCatalogTooLarge}
	default:
		return Outcome{State: StateFailed, Code: CodeCoreUnavailable}
	}
}

func selectCatalogAPIKey(raw []byte, fingerprint string) (string, bool) {
	if !fingerprintPattern.MatchString(fingerprint) {
		return "", false
	}
	root, err := parseOwnedDocument(raw)
	if err != nil {
		return "", false
	}
	keys, err := uniqueMappingValue(root, "api-keys")
	if err != nil || keys.Kind != yaml.SequenceNode {
		return "", false
	}
	match := ""
	matches := 0
	for _, node := range keys.Content {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return "", false
		}
		candidate := strings.TrimSpace(node.Value)
		if candidate == "" {
			continue
		}
		sum := sha256.Sum256([]byte(candidate))
		if hex.EncodeToString(sum[:]) == fingerprint {
			match = candidate
			matches++
		}
	}
	return match, matches == 1
}
