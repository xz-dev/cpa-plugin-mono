package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
)

var plannerPaths = map[Operation]string{
	OperationAutoPull:     "/v0/management/plugins/auto-pull-models/plan",
	OperationMetadataSync: "/v0/management/plugins/model-metadata-sync/plan",
}

type HTTPPlanner struct {
	client *http.Client
	relay  fetchRelay
}

func NewHTTPPlanner(client *http.Client) *HTTPPlanner {
	if client == nil {
		client = NewLoopbackClient()
	}
	return &HTTPPlanner{client: client, relay: fetchRelay{client: client}}
}

func (p *HTTPPlanner) Plan(ctx context.Context, operation Operation, snapshot ConfigSnapshot, settings Settings) (CommitProposal, ErrorCode) {
	return p.PlanWithProgress(ctx, operation, snapshot, settings, nil)
}

func (p *HTTPPlanner) PlanWithProgress(ctx context.Context, operation Operation, snapshot ConfigSnapshot, settings Settings, progress func(RunState)) (CommitProposal, ErrorCode) {
	path, ok := plannerPaths[operation]
	if !ok {
		return CommitProposal{}, CodeInvalidRequest
	}
	request := plannerRequest{Version: snapshot.Version, ConfigBase64: snapshot.ConfigBase64}
	seenContinuations := make(map[string]bool)
	seenDescriptors := make(map[string]bool)
	seenRequestIDs := make(map[string]bool)
	seenClaudeCursors := make(map[string]bool)
	providerBytes := 0
	for page := 0; ; page++ {
		envelope, code := p.call(ctx, operation, path, request, snapshot, settings)
		if code != "" {
			return CommitProposal{}, code
		}
		if envelope.NextFetch == nil {
			if _, err := envelope.Decode(snapshot.Version); err != nil {
				return CommitProposal{}, CodeInvalidRequest
			}
			return envelope, ""
		}
		if envelope.ConfigBase64 != "" || envelope.BaseVersion != snapshot.Version || page >= maxProviderPages {
			return CommitProposal{}, CodeProviderFetchInvalid
		}
		descriptor := *envelope.NextFetch
		if err := validateFetchDescriptor(descriptor, snapshot); err != nil {
			return CommitProposal{}, CodeProviderFetchInvalid
		}
		cursorKey := claudeCursorProgressionKey(descriptor)
		if seenRequestIDs[descriptor.RequestID] || seenContinuations[descriptor.ContinuationBase64] || seenDescriptors[descriptorProgressionKey(descriptor)] || (cursorKey != "" && seenClaudeCursors[cursorKey]) {
			return CommitProposal{}, CodeProviderFetchInvalid
		}
		seenRequestIDs[descriptor.RequestID] = true
		seenContinuations[descriptor.ContinuationBase64] = true
		seenDescriptors[descriptorProgressionKey(descriptor)] = true
		if cursorKey != "" {
			seenClaudeCursors[cursorKey] = true
		}
		if progress != nil {
			progress(StateFetching)
		}
		result, code := p.relay.fetch(ctx, descriptor, settings)
		if code != "" {
			return CommitProposal{}, code
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(result.BodyBase64)
		if err != nil {
			return CommitProposal{}, CodeProviderFetchFailed
		}
		if len(decoded) > maxProviderPayloadBytes-providerBytes {
			return CommitProposal{}, CodeProviderCatalogTooLarge
		}
		providerBytes += len(decoded)
		request = plannerRequest{
			Version: snapshot.Version, ConfigBase64: snapshot.ConfigBase64,
			ContinuationBase64: descriptor.ContinuationBase64,
			FetchResult:        &result,
		}
		if progress != nil {
			progress(StatePlanning)
		}
	}
}

type plannerEnvelopeWire struct {
	BaseVersion  json.RawMessage `json:"base_version"`
	ConfigBase64 json.RawMessage `json:"config_base64"`
	NextFetch    json.RawMessage `json:"next_fetch"`
	Report       json.RawMessage `json:"report"`
}

type plannerReportWire struct {
	Changed *bool `json:"changed"`
}

func decodePlannerEnvelope(raw []byte, expectedVersion string) (CommitProposal, ErrorCode) {
	var wire plannerEnvelopeWire
	if err := decodeStrictJSONBytes(raw, &wire); err != nil {
		var probe struct {
			NextFetch json.RawMessage `json:"next_fetch"`
		}
		if json.NewDecoder(bytes.NewReader(raw)).Decode(&probe) == nil && len(probe.NextFetch) != 0 {
			return CommitProposal{}, CodeProviderFetchInvalid
		}
		return CommitProposal{}, CodeInvalidRequest
	}
	var baseVersion string
	if len(wire.BaseVersion) == 0 || decodeStrictJSONBytes(wire.BaseVersion, &baseVersion) != nil || baseVersion != expectedVersion {
		return CommitProposal{}, CodeInvalidRequest
	}
	hasConfig, hasFetch, hasReport := len(wire.ConfigBase64) != 0, len(wire.NextFetch) != 0, len(wire.Report) != 0
	if hasConfig && !hasFetch && hasReport {
		var configBase64 string
		var report plannerReportWire
		if decodeStrictJSONBytes(wire.ConfigBase64, &configBase64) != nil || configBase64 == "" || len(wire.Report) > 64<<10 || decodeStrictJSONBytes(wire.Report, &report) != nil || report.Changed == nil {
			return CommitProposal{}, CodeInvalidRequest
		}
		return CommitProposal{BaseVersion: baseVersion, ConfigBase64: configBase64, Report: append(json.RawMessage(nil), wire.Report...)}, ""
	}
	if !hasConfig && hasFetch && !hasReport {
		if bytes.Equal(bytes.TrimSpace(wire.NextFetch), []byte("null")) {
			return CommitProposal{}, CodeProviderFetchInvalid
		}
		var descriptor FetchDescriptor
		if decodeStrictJSONBytes(wire.NextFetch, &descriptor) != nil {
			return CommitProposal{}, CodeProviderFetchInvalid
		}
		return CommitProposal{BaseVersion: baseVersion, NextFetch: &descriptor}, ""
	}
	if hasFetch {
		return CommitProposal{}, CodeProviderFetchInvalid
	}
	return CommitProposal{}, CodeInvalidRequest
}

func (p *HTTPPlanner) call(ctx context.Context, operation Operation, path string, request plannerRequest, snapshot ConfigSnapshot, settings Settings) (CommitProposal, ErrorCode) {
	body, err := json.Marshal(request)
	if err != nil || len(body) > 40<<20 {
		return CommitProposal{}, CodeInvalidRequest
	}
	req, err := NewCoreRequest(ctx, settings, http.MethodPost, path, body)
	if err != nil {
		return CommitProposal{}, CodeCoreUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(workerTokenHeader, settings.WorkerToken)
	response, err := p.client.Do(req)
	if err != nil {
		status, statusErr := NewWorkerStatusClient(p.client).Status(ctx, string(operation), settings)
		if statusErr != nil || status.ActivePlan {
			return CommitProposal{}, CodePlannerStalled
		}
		if isTimeout(err, ctx) {
			return CommitProposal{}, CodeLoopbackTimeout
		}
		return CommitProposal{}, CodeCoreUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := decodePlannerError(response.Body)
		if code != "" {
			return CommitProposal{}, code
		}
		return CommitProposal{}, CodeCoreUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPlannerEnvelopeBytes+1))
	if err != nil || len(raw) > maxPlannerEnvelopeBytes {
		return CommitProposal{}, CodeInvalidRequest
	}
	return decodePlannerEnvelope(raw, snapshot.Version)
}

func decodePlannerError(body io.Reader) ErrorCode {
	var response struct {
		ErrorCode ErrorCode `json:"error_code"`
	}
	if err := decodeStrictJSON(io.LimitReader(body, 4097), 4096, &response); err != nil {
		return ""
	}
	switch response.ErrorCode {
	case CodeProviderCredentialUnavailable, CodeProviderFetchInvalid, CodeProviderCatalogTooLarge:
		return response.ErrorCode
	default:
		return ""
	}
}
