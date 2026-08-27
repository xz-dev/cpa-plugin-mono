package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeWorkerStatuses struct {
	mu       sync.Mutex
	statuses map[string]WorkerStatus
	err      error
}

func (f *fakeWorkerStatuses) Status(_ context.Context, id string, _ Settings) (WorkerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses[id], f.err
}

func (f *fakeWorkerStatuses) set(id string, status WorkerStatus) {
	f.mu.Lock()
	f.statuses[id] = status
	f.mu.Unlock()
}

type coreHarness struct {
	mu             sync.Mutex
	raw            []byte
	putCount       int
	putStatus      int
	persistOnError bool
	postGet        []byte
	puts           chan []byte
}

func (h *coreHarness) handler(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer mgmt-secret" {
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_, _ = w.Write(h.raw)
	case http.MethodPut:
		h.putCount++
		body, _ := io.ReadAll(r.Body)
		if h.puts != nil {
			h.puts <- append([]byte(nil), body...)
		}
		if h.putStatus != 0 && h.putStatus != http.StatusOK {
			if h.persistOnError {
				h.raw = normalizeCommentIndentation(body)
			}
			w.WriteHeader(h.putStatus)
			_, _ = w.Write([]byte(`{"message":"root-secret body"}`))
			return
		}
		if h.postGet != nil {
			h.raw = append([]byte(nil), h.postGet...)
		} else {
			h.raw = normalizeCommentIndentation(body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"changed":["config"]}`))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *coreHarness) snapshot() ([]byte, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.raw...), h.putCount
}

func commitTestConfig(epoch string) []byte {
	return []byte(`api-keys:
  - root-secret
plugins:
  enabled: true
  dir: plugins
  configs:
    sync-config-write:
      enabled: true
      core_origin: http://127.0.0.1:1
      management_key_env: MGMT
      model_info_proxy_api_key_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      worker_token_env: TOKEN
      sync_epoch: ` + epoch + `
    auto-pull-models:
      enabled: true
      worker_token_env: TOKEN
      sync_epoch: ` + epoch + `
    model-metadata-sync:
      enabled: true
      worker_token_env: TOKEN
      sync_epoch: ` + epoch + `
    model-info:
      enabled: true
      worker_token_env: TOKEN
      sync_epoch: ` + epoch + `
openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models:
      - name: old
        alias: old-alias
        max-context-length: 100
`)
}

func membershipProposal(base []byte) []byte {
	return bytes.Replace(base, []byte(`      - name: old
        alias: old-alias
        max-context-length: 100`), []byte(`      - name: new
      - name: old
        alias: old-alias
        max-context-length: 100`), 1)
}

func metadataProposal(base []byte) []byte {
	return bytes.Replace(base, []byte("        max-context-length: 100"), []byte("        max-context-length: 200\n        max-output-tokens: 50"), 1)
}

func newCommitHarness(t *testing.T, raw []byte) (*CommitEngine, *coreHarness, *fakeWorkerStatuses, func()) {
	t.Helper()
	core := &coreHarness{raw: append([]byte(nil), raw...)}
	server := httptest.NewServer(http.HandlerFunc(core.handler))
	workers := &fakeWorkerStatuses{statuses: make(map[string]WorkerStatus)}
	settings := Settings{CoreOrigin: server.URL, ManagementKey: "mgmt-secret", WorkerToken: "worker-secret"}
	engine := NewCommitEngine(NewLoopbackClient(), workers, func() Settings { return settings }, nil)
	engine.convergenceTimeout = 30 * time.Millisecond
	return engine, core, workers, server.Close
}

func statusTuplesFor(raw []byte, seq uint64) map[string]WorkerStatus {
	hashes, err := runtimeConfigHashes(raw)
	if err != nil {
		panic(err)
	}
	result := make(map[string]WorkerStatus, len(pluginIDs))
	for index, id := range pluginIDs {
		instanceRaw := bytes.Repeat([]byte{byte(index + 1)}, 16)
		result[id] = WorkerStatus{InstanceID: base64.RawURLEncoding.EncodeToString(instanceRaw), ReconfigureSeq: seq, ConfigSHA256: hashes[id]}
	}
	return result
}

func TestMalformedCommitProposalDoesNotContactCore(t *testing.T) {
	base := commitTestConfig("old")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(base)
	}))
	defer server.Close()
	settings := Settings{CoreOrigin: server.URL, ManagementKey: "mgmt-secret", WorkerToken: "worker-secret"}
	engine := NewCommitEngine(NewLoopbackClient(), &fakeWorkerStatuses{statuses: map[string]WorkerStatus{}}, func() Settings { return settings }, nil)
	result := engine.Commit(context.Background(), OperationAutoPull, CommitRequest{Proposal: CommitProposal{BaseVersion: "invalid", ConfigBase64: "***"}})
	if result.Code != CodeInvalidRequest || requests.Load() != 0 {
		t.Fatalf("result=%+v requests=%d", result, requests.Load())
	}
}

func TestCommitVersionConflictSkipsPUTAndExposesOnlyVersion(t *testing.T) {
	base := commitTestConfig("old")
	current := bytes.Replace(base, []byte("root-secret"), []byte("new-root-secret"), 1)
	engine, core, workers, closeFn := newCommitHarness(t, current)
	defer closeFn()
	for id, status := range statusTuplesFor(current, 1) {
		workers.set(id, status)
	}
	result := engine.Commit(context.Background(), OperationAutoPull, CommitRequest{
		Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(membershipProposal(base))},
	})
	_, puts := core.snapshot()
	if result.Code != CodeVersionConflict || result.Version != configVersion(current) || puts != 0 || len(result.Version) != 64 {
		t.Fatalf("result=%+v puts=%d", result, puts)
	}
}

func TestCommitExactAndSerializationOnlyNoopsSkipPUTEpochAndHandshake(t *testing.T) {
	base := commitTestConfig("old")
	for name, proposal := range map[string][]byte{
		"exact":         base,
		"serialization": bytes.ReplaceAll(base, []byte("\n"), []byte("\r\n")),
	} {
		t.Run(name, func(t *testing.T) {
			engine, core, workers, closeFn := newCommitHarness(t, base)
			defer closeFn()
			workers.err = context.DeadlineExceeded
			result := engine.Commit(context.Background(), OperationAutoPull, CommitRequest{Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(proposal)}})
			got, puts := core.snapshot()
			if result.Code != "" || result.Changed || puts != 0 || string(got) != string(base) {
				t.Fatalf("result=%+v puts=%d", result, puts)
			}
		})
	}
}

func TestCommitPreservesUnrelatedAliasAndMergeSemantics(t *testing.T) {
	unrelated := []byte("unrelated:\n  defaults: &defaults\n    value: one\n  copied: *defaults\n  merged:\n    <<: *defaults\n    extra: true\n  cycle: &cycle\n    self: *cycle\n")
	base := append(append([]byte(nil), unrelated...), commitTestConfig("old")...)
	engine, _, workers, closeFn := newCommitHarness(t, base)
	defer closeFn()
	for id, status := range statusTuplesFor(base, 1) {
		workers.set(id, status)
	}
	engine.afterVerified = func(expected []byte) {
		for id, status := range statusTuplesFor(expected, 2) {
			workers.set(id, status)
		}
	}
	result := engine.Commit(context.Background(), OperationAutoPull, CommitRequest{Proposal: ProposalFromBytes(configVersion(base), membershipProposal(base))})
	if result.Code != "" || result.State != StateSucceeded || !result.Changed {
		t.Fatalf("result=%+v", result)
	}
}

func TestCommitPUTValidationRejectIsSanitizedAndNonPersisting(t *testing.T) {
	for _, statusCode := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			base := commitTestConfig("old")
			engine, core, workers, closeFn := newCommitHarness(t, base)
			defer closeFn()
			core.putStatus = statusCode
			for id, status := range statusTuplesFor(base, 1) {
				workers.set(id, status)
			}
			result := engine.Commit(context.Background(), OperationMetadataSync, CommitRequest{
				Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(metadataProposal(base))},
			})
			authoritative, puts := core.snapshot()
			if result.Code != CodeInvalidConfig || bytes.Contains([]byte(result.Code), []byte("secret")) || puts != 1 || !bytes.Equal(authoritative, base) {
				t.Fatalf("result=%+v puts=%d", result, puts)
			}
		})
	}
}

func TestCommitHTTP500AfterPersistIsBlockingUncertain(t *testing.T) {
	base := commitTestConfig("old")
	engine, core, workers, closeFn := newCommitHarness(t, base)
	defer closeFn()
	core.putStatus = http.StatusInternalServerError
	core.persistOnError = true
	for id, status := range statusTuplesFor(base, 1) {
		workers.set(id, status)
	}
	result := engine.Commit(context.Background(), OperationMetadataSync, CommitRequest{
		Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(metadataProposal(base))},
	})
	authoritative, puts := core.snapshot()
	if result.Code != CodeCommitVerificationFailed || result.State != StateUncertain || !result.Changed || puts != 1 || result.Version != configVersion(authoritative) || len(result.PreStatus) != len(pluginIDs) {
		t.Fatalf("result=%+v puts=%d", result, puts)
	}
}

func TestCommitPostPUTMismatchIsTerminalUncertainWithoutRetry(t *testing.T) {
	base := commitTestConfig("old")
	engine, core, workers, closeFn := newCommitHarness(t, base)
	defer closeFn()
	core.postGet = bytes.Replace(base, []byte("root-secret"), []byte("external-change"), 1)
	for id, status := range statusTuplesFor(base, 1) {
		workers.set(id, status)
	}
	result := engine.Commit(context.Background(), OperationMetadataSync, CommitRequest{
		Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(metadataProposal(base))},
	})
	_, puts := core.snapshot()
	if result.Code != CodeCommitVerificationFailed || result.State != StateUncertain || puts != 1 || result.Version != configVersion(core.postGet) || !result.Changed || len(result.PreStatus) != len(pluginIDs) || len(result.ExpectedHashes) != len(pluginIDs) {
		t.Fatalf("result=%+v puts=%d", result, puts)
	}
}

func TestCommitPostPUTTransportFailureIsTerminalUncertainWithoutRetry(t *testing.T) {
	base := commitTestConfig("old")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodGet {
			_, _ = w.Write(base)
			return
		}
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()
	workers := &fakeWorkerStatuses{statuses: statusTuplesFor(base, 1)}
	settings := Settings{CoreOrigin: server.URL, ManagementKey: "mgmt-secret", WorkerToken: "worker-secret"}
	engine := NewCommitEngine(NewLoopbackClient(), workers, func() Settings { return settings }, nil)
	result := engine.Commit(context.Background(), OperationMetadataSync, CommitRequest{Proposal: ProposalFromBytes(configVersion(base), metadataProposal(base))})
	if result.Code != CodeCommitVerificationFailed || result.State != StateUncertain || requests.Load() != 3 || !result.Changed || result.Version != configVersion(base) || len(result.PreStatus) != len(pluginIDs) {
		t.Fatalf("result=%+v requests=%d", result, requests.Load())
	}
}

func TestCommitPostPUTAndVerificationTransportFailureOmitsVersion(t *testing.T) {
	base := commitTestConfig("old")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch requests.Add(1) {
		case 1:
			_, _ = w.Write(base)
		default:
			panic(http.ErrAbortHandler)
		}
	}))
	defer server.Close()
	workers := &fakeWorkerStatuses{statuses: statusTuplesFor(base, 1)}
	settings := Settings{CoreOrigin: server.URL, ManagementKey: "mgmt-secret", WorkerToken: "worker-secret"}
	engine := NewCommitEngine(NewLoopbackClient(), workers, func() Settings { return settings }, nil)
	result := engine.Commit(context.Background(), OperationMetadataSync, CommitRequest{Proposal: ProposalFromBytes(configVersion(base), metadataProposal(base))})
	if result.Code != CodeCommitVerificationFailed || result.State != StateUncertain || !result.Changed || result.Version != "" || requests.Load() != 3 || len(result.PreStatus) != len(pluginIDs) {
		t.Fatalf("result=%+v requests=%d", result, requests.Load())
	}
}

func TestCommitConvergenceSuccessAndFailureMatrix(t *testing.T) {
	for name, mutate := range map[string]func(map[string]WorkerStatus){
		"success": func(map[string]WorkerStatus) {},
		"instance": func(statuses map[string]WorkerStatus) {
			s := statuses["model-info"]
			s.InstanceID = "restarted"
			statuses["model-info"] = s
		},
		"sequence": func(statuses map[string]WorkerStatus) {
			s := statuses["auto-pull-models"]
			s.ReconfigureSeq = 1
			statuses["auto-pull-models"] = s
		},
		"hash": func(statuses map[string]WorkerStatus) {
			s := statuses["model-metadata-sync"]
			s.ConfigSHA256 = configVersion([]byte("wrong"))
			statuses["model-metadata-sync"] = s
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := commitTestConfig("old")
			engine, core, workers, closeFn := newCommitHarness(t, base)
			defer closeFn()
			pre := statusTuplesFor(base, 1)
			engine.afterVerified = func(expected []byte) {
				post := statusTuplesFor(expected, 2)
				mutate(post)
				for id, status := range post {
					workers.set(id, status)
				}
			}
			for id, status := range pre {
				workers.set(id, status)
			}
			result := engine.Commit(context.Background(), OperationAutoPull, CommitRequest{
				Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(membershipProposal(base))},
			})
			_, puts := core.snapshot()
			if name == "success" {
				if result.Code != "" || !result.Changed || result.State != StateSucceeded || puts != 1 {
					t.Fatalf("result=%+v puts=%d", result, puts)
				}
			} else if result.Code != CodePersistedRuntimeUncertain || result.State != StateUncertain || puts != 1 {
				t.Fatalf("result=%+v puts=%d", result, puts)
			}
		})
	}
}

func TestConcurrentCommitsSerialize(t *testing.T) {
	base := commitTestConfig("old")
	engine, core, workers, closeFn := newCommitHarness(t, base)
	defer closeFn()
	core.puts = make(chan []byte, 2)
	pre := statusTuplesFor(base, 1)
	for id, status := range pre {
		workers.set(id, status)
	}
	engine.afterVerified = func(expected []byte) {
		for id, status := range statusTuplesFor(expected, 2) {
			workers.set(id, status)
		}
	}
	firstDone := make(chan CommitResult, 1)
	go func() {
		firstDone <- engine.Commit(context.Background(), OperationAutoPull, CommitRequest{Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(membershipProposal(base))}})
	}()
	select {
	case <-core.puts:
	case <-time.After(time.Second):
		t.Fatal("first PUT not reached")
	}
	second := engine.Commit(context.Background(), OperationMetadataSync, CommitRequest{Proposal: CommitProposal{BaseVersion: configVersion(base), ConfigBase64: encodeBase64(metadataProposal(base))}})
	first := <-firstDone
	if first.Code != "" || second.Code != CodeVersionConflict {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func encodeBase64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }
