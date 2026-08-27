package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	coreConfigPath    = "/v0/management/config.yaml"
	workerTokenHeader = "X-Sync-Config-Writer-Token"
)

var workerStatusPaths = map[string]string{
	"auto-pull-models":    "/v0/management/plugins/auto-pull-models/writer-status",
	"model-metadata-sync": "/v0/management/plugins/model-metadata-sync/writer-status",
	"model-info":          "/v0/management/plugins/model-info/writer-status",
}

type WorkerStatus struct {
	InstanceID     string `json:"instance_id"`
	ReconfigureSeq uint64 `json:"reconfigure_seq"`
	ConfigSHA256   string `json:"config_sha256"`
	ActivePlan     bool   `json:"active_plan"`
}

type WorkerStatusClient interface {
	Status(context.Context, string, Settings) (WorkerStatus, error)
}

type HTTPWorkerStatusClient struct{ client *http.Client }

func NewWorkerStatusClient(client *http.Client) *HTTPWorkerStatusClient {
	if client == nil {
		client = NewLoopbackClient()
	}
	return &HTTPWorkerStatusClient{client: client}
}

func (c *HTTPWorkerStatusClient) Status(ctx context.Context, id string, settings Settings) (WorkerStatus, error) {
	path, ok := workerStatusPaths[id]
	if !ok {
		return WorkerStatus{}, fmt.Errorf("unknown worker")
	}
	req, err := NewCoreRequest(ctx, settings, http.MethodGet, path, nil)
	if err != nil {
		return WorkerStatus{}, err
	}
	req.Header.Set(workerTokenHeader, settings.WorkerToken)
	response, err := c.client.Do(req)
	if err != nil {
		return WorkerStatus{}, fmt.Errorf("worker status unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return WorkerStatus{}, fmt.Errorf("worker status unavailable")
	}
	limited := io.LimitReader(response.Body, 4097)
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&fields); err != nil {
		return WorkerStatus{}, fmt.Errorf("invalid worker status")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return WorkerStatus{}, fmt.Errorf("invalid worker status")
	}
	allowed := map[string]bool{"instance_id": true, "reconfigure_seq": true, "config_sha256": true}
	if id != "model-info" {
		allowed["active_plan"] = true
	}
	if len(fields) != len(allowed) {
		return WorkerStatus{}, fmt.Errorf("invalid worker status")
	}
	for key := range fields {
		if !allowed[key] {
			return WorkerStatus{}, fmt.Errorf("invalid worker status")
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return WorkerStatus{}, fmt.Errorf("invalid worker status")
	}
	var status WorkerStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return WorkerStatus{}, fmt.Errorf("invalid worker status")
	}
	if err := validateWorkerStatus(status); err != nil {
		return WorkerStatus{}, err
	}
	return status, nil
}

func validateWorkerStatus(status WorkerStatus) error {
	decoded, err := decodeOpaqueID(status.InstanceID)
	if err != nil || len(decoded) != 16 || status.ReconfigureSeq == 0 || !versionPattern.MatchString(status.ConfigSHA256) {
		return fmt.Errorf("invalid worker status")
	}
	return nil
}

type CommitRequest struct {
	Proposal CommitProposal
}

type CommitResult struct {
	State            RunState
	Code             ErrorCode
	Version          string
	Changed          bool
	ExpectedHashes   map[string]string
	PreStatus        map[string]WorkerStatus
	PersistedVersion string
}

type CommitEngine struct {
	mu                             sync.Mutex
	client                         *http.Client
	workers                        WorkerStatusClient
	settings                       func() Settings
	localStatus                    func() WorkerStatus
	afterVerified                  func([]byte)
	beforeCommitForTest            func()
	beforeReconcileFinalGetForTest func()
	epoch                          func() (string, error)
	convergenceTimeout             time.Duration
}

func NewCommitEngine(client *http.Client, workers WorkerStatusClient, settings func() Settings, localStatus func() WorkerStatus) *CommitEngine {
	if client == nil {
		client = NewLoopbackClient()
	}
	if workers == nil {
		workers = NewWorkerStatusClient(client)
	}
	if settings == nil {
		settings = func() Settings { return Settings{} }
	}
	return &CommitEngine{client: client, workers: workers, settings: settings, localStatus: localStatus, epoch: newOpaqueID, convergenceTimeout: 120 * time.Second}
}

func (e *CommitEngine) Commit(ctx context.Context, operation Operation, request CommitRequest) CommitResult {
	return e.CommitWithSettings(ctx, operation, request, e.settings())
}

func (e *CommitEngine) CommitWithSettings(ctx context.Context, operation Operation, request CommitRequest, settings Settings) CommitResult {
	return e.commitWithSettings(ctx, operation, request, settings, nil)
}

func (e *CommitEngine) commitWithSettings(ctx context.Context, operation Operation, request CommitRequest, settings Settings, progress func(RunState)) CommitResult {
	proposal, err := request.Proposal.Decode(request.Proposal.BaseVersion)
	if err != nil {
		return CommitResult{State: StateFailed, Code: CodeInvalidRequest}
	}
	if progress != nil {
		progress(StateCommitting)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.beforeCommitForTest != nil {
		e.beforeCommitForTest()
	}
	current, code := e.getConfig(ctx, settings)
	if code != "" {
		return CommitResult{State: StateFailed, Code: code}
	}
	currentVersion := configVersion(current)
	if currentVersion != request.Proposal.BaseVersion {
		return CommitResult{State: StateFailed, Code: CodeVersionConflict, Version: currentVersion}
	}
	if string(current) == string(proposal) {
		return CommitResult{State: StateSucceeded, Version: currentVersion}
	}
	changed, err := validateOwnership(operation, current, proposal)
	if err != nil {
		return CommitResult{State: StateFailed, Code: CodeInvalidRequest}
	}
	if !changed {
		return CommitResult{State: StateSucceeded, Version: currentVersion}
	}
	currentHashes, err := runtimeConfigHashes(current)
	if err != nil {
		return CommitResult{State: StateFailed, Code: CodeInvalidRequest}
	}
	preStatus, err := e.captureStatus(ctx, settings, currentHashes)
	if err != nil {
		return CommitResult{State: StateFailed, Code: CodeCoreUnavailable}
	}
	epoch, err := e.epoch()
	if err != nil {
		return CommitResult{State: StateFailed, Code: CodeCoreUnavailable}
	}
	adjusted, expectedHashes, err := injectSyncEpoch(proposal, epoch)
	if err != nil {
		return CommitResult{State: StateFailed, Code: CodeInvalidRequest}
	}
	expectedPersisted := normalizeCommentIndentation(adjusted)
	if code, uncertain := e.putConfig(ctx, settings, adjusted); code != "" {
		if !uncertain {
			return CommitResult{State: StateFailed, Code: code}
		}
		result := CommitResult{State: StateUncertain, Code: code, Changed: true, ExpectedHashes: expectedHashes, PreStatus: preStatus}
		if authoritative, getCode := e.getConfig(ctx, settings); getCode == "" {
			result.Version = configVersion(authoritative)
			result.PersistedVersion = result.Version
		}
		return result
	}
	expectedResult := CommitResult{State: StateUncertain, Code: CodeCommitVerificationFailed, Changed: true, ExpectedHashes: expectedHashes, PreStatus: preStatus}
	authoritative, code := e.getConfig(ctx, settings)
	if code != "" {
		return expectedResult
	}
	authoritativeVersion := configVersion(authoritative)
	if string(authoritative) != string(expectedPersisted) {
		expectedResult.Version = authoritativeVersion
		expectedResult.PersistedVersion = authoritativeVersion
		return expectedResult
	}
	if e.afterVerified != nil {
		e.afterVerified(authoritative)
	}
	if progress != nil {
		progress(StateWaiting)
	}
	if !e.converged(ctx, settings, preStatus, expectedHashes) {
		return CommitResult{State: StateUncertain, Code: CodePersistedRuntimeUncertain, Version: authoritativeVersion, Changed: true, ExpectedHashes: expectedHashes, PreStatus: preStatus, PersistedVersion: authoritativeVersion}
	}
	return CommitResult{State: StateSucceeded, Version: authoritativeVersion, Changed: true, ExpectedHashes: expectedHashes, PersistedVersion: authoritativeVersion}
}

func (e *CommitEngine) captureStatus(ctx context.Context, settings Settings, expected map[string]string) (map[string]WorkerStatus, error) {
	statuses := make(map[string]WorkerStatus, len(pluginIDs))
	for _, id := range pluginIDs {
		var status WorkerStatus
		var err error
		if id == "sync-config-write" && e.localStatus != nil {
			status = e.localStatus()
		} else {
			status, err = e.workers.Status(ctx, id, settings)
		}
		if err != nil || validateWorkerStatus(status) != nil || status.ConfigSHA256 != expected[id] {
			return nil, fmt.Errorf("runtime status unavailable")
		}
		statuses[id] = status
	}
	return statuses, nil
}

func (e *CommitEngine) converged(ctx context.Context, settings Settings, before map[string]WorkerStatus, expected map[string]string) bool {
	ctx, cancel := context.WithTimeout(ctx, e.convergenceTimeout)
	defer cancel()
	for {
		matched, terminal := e.checkConvergence(ctx, settings, before, expected)
		if matched {
			return true
		}
		if terminal {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (e *CommitEngine) checkConvergence(ctx context.Context, settings Settings, before map[string]WorkerStatus, expected map[string]string) (bool, bool) {
	for _, id := range pluginIDs {
		var status WorkerStatus
		var err error
		if id == "sync-config-write" {
			if e.localStatus == nil {
				status, err = e.workers.Status(ctx, id, settings)
			} else {
				status = e.localStatus()
			}
		} else {
			status, err = e.workers.Status(ctx, id, settings)
		}
		if err != nil || validateWorkerStatus(status) != nil {
			return false, false
		}
		if status.InstanceID != before[id].InstanceID {
			return false, true
		}
		if status.ReconfigureSeq <= before[id].ReconfigureSeq || status.ConfigSHA256 != expected[id] {
			return false, false
		}
	}
	return true, false
}

func (e *CommitEngine) getConfig(ctx context.Context, settings Settings) ([]byte, ErrorCode) {
	req, err := NewCoreRequest(ctx, settings, http.MethodGet, coreConfigPath, nil)
	if err != nil {
		return nil, CodeCoreUnavailable
	}
	response, err := e.client.Do(req)
	if err != nil {
		return nil, CodeCoreUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, CodeCoreUnavailable
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, CodeCoreUnavailable
	}
	return raw, ""
}

func (e *CommitEngine) putConfig(ctx context.Context, settings Settings, raw []byte) (ErrorCode, bool) {
	req, err := NewCoreRequest(ctx, settings, http.MethodPut, coreConfigPath, raw)
	if err != nil {
		return CodeCoreUnavailable, false
	}
	req.Header.Set("Content-Type", "application/yaml")
	response, err := e.client.Do(req)
	if err != nil {
		return CodeCommitVerificationFailed, true
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return "", false
	}
	if response.StatusCode == http.StatusUnprocessableEntity || response.StatusCode == http.StatusBadRequest {
		return CodeInvalidConfig, false
	}
	return CodeCommitVerificationFailed, true
}
