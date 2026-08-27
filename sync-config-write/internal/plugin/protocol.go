package plugin

import (
	"crypto/rand"
	"encoding/base64"
	"time"
)

type Operation string

const (
	OperationAutoPull     Operation = "auto-pull-models"
	OperationMetadataSync Operation = "model-metadata-sync"
	OperationModelInfo    Operation = "model-info-catalog"
	OperationReconcile    Operation = "reconcile"
)

type RunState string

const (
	StateQueued      RunState = "queued"
	StatePlanning    RunState = "planning"
	StateFetching    RunState = "fetching"
	StateCommitting  RunState = "committing"
	StateWaiting     RunState = "waiting_reconfigure"
	StateReconciling RunState = "reconciling"
	StateSucceeded   RunState = "succeeded"
	StateFailed      RunState = "failed"
	StateUncertain   RunState = "uncertain"
	StateBlocked     RunState = "blocked"
)

type ErrorCode string

const (
	CodeNotImplemented                ErrorCode = "not_yet_implemented"
	CodeInvalidRequest                ErrorCode = "invalid_request"
	CodeVersionConflict               ErrorCode = "version_conflict"
	CodeCommitVerificationFailed      ErrorCode = "commit_verification_failed"
	CodePersistedRuntimeUncertain     ErrorCode = "persisted_runtime_uncertain"
	CodeInvalidConfig                 ErrorCode = "invalid_config"
	CodeProviderCredentialUnavailable ErrorCode = "provider_credential_unavailable"
	CodeProviderFetchInvalid          ErrorCode = "provider_fetch_invalid"
	CodeProviderFetchFailed           ErrorCode = "provider_fetch_failed"
	CodeProviderCatalogTooLarge       ErrorCode = "provider_catalog_too_large"
	CodeCatalogKeyUnavailable         ErrorCode = "catalog_key_unavailable"
	CodeCatalogInvalid                ErrorCode = "catalog_invalid"
	CodeCatalogTooLarge               ErrorCode = "catalog_too_large"
	CodeCoreUnavailable               ErrorCode = "core_unavailable"
	CodeLoopbackTimeout               ErrorCode = "loopback_timeout"
	CodeWriterBlocked                 ErrorCode = "writer_blocked"
	CodeStartupReconcileRequired      ErrorCode = "startup_reconcile_required"
	CodePlannerStalled                ErrorCode = "planner_stalled"
	CodeReconcileFailed               ErrorCode = "reconcile_failed"
)

type Outcome struct {
	State        RunState
	Code         ErrorCode
	Version      string
	ConfigSHA256 string
	Changed      bool
	Block        BlockRecord
}

type RunStatus struct {
	RunID          string    `json:"run_id"`
	Operation      Operation `json:"operation"`
	State          RunState  `json:"state"`
	Attempt        int       `json:"attempt"`
	QueuedAt       time.Time `json:"queued_at"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	Version        string    `json:"version,omitempty"`
	Changed        bool      `json:"changed,omitempty"`
	ErrorCode      ErrorCode `json:"error_code,omitempty"`
	BlockingRunID  string    `json:"blocking_run_id,omitempty"`
	InstanceID     string    `json:"instance_id"`
	ReconfigureSeq uint64    `json:"reconfigure_seq"`
	ConfigSHA256   string    `json:"config_sha256"`
}

type StatusResponse struct {
	InstanceID     string      `json:"instance_id"`
	ReconfigureSeq uint64      `json:"reconfigure_seq"`
	ConfigSHA256   string      `json:"config_sha256"`
	WriterBlocked  bool        `json:"writer_blocked"`
	BlockingRunID  string      `json:"blocking_run_id,omitempty"`
	ErrorCode      ErrorCode   `json:"error_code,omitempty"`
	Runs           []RunStatus `json:"runs,omitempty"`
}

func newOpaqueID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func decodeOpaqueID(id string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(id) }
