package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

var plannerPaths = map[Operation]string{
	OperationAutoPull:     "/v0/management/plugins/auto-pull-models/plan",
	OperationMetadataSync: "/v0/management/plugins/model-metadata-sync/plan",
}

type HTTPPlanner struct{ client *http.Client }

func NewHTTPPlanner(client *http.Client) *HTTPPlanner {
	if client == nil {
		client = NewLoopbackClient()
	}
	return &HTTPPlanner{client: client}
}

func (p *HTTPPlanner) Plan(ctx context.Context, operation Operation, snapshot ConfigSnapshot, settings Settings) (CommitProposal, ErrorCode) {
	path, ok := plannerPaths[operation]
	if !ok {
		return CommitProposal{}, CodeInvalidRequest
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
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
		return CommitProposal{}, CodeCoreUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return CommitProposal{}, CodeCoreUnavailable
	}
	limited := io.LimitReader(response.Body, 16<<20)
	var proposal CommitProposal
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return CommitProposal{}, CodeInvalidRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return CommitProposal{}, CodeInvalidRequest
	}
	if err := validatePlannerProposal(snapshot, proposal); err != nil {
		return CommitProposal{}, CodeInvalidRequest
	}
	return proposal, ""
}
