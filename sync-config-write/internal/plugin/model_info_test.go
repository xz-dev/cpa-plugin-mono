package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func catalogFingerprint(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

type modelInfoCore struct {
	mu                 sync.Mutex
	config             []byte
	catalog            []byte
	catalogCode        int
	ingestCode         int
	ingestReceipt      []byte
	configGets         int
	configAfterCatalog []byte
	models             int
	ingests            int
	puts               int
	lastIngest         []byte
	selectedKey        string
	management         string
	workerToken        string
	wrongRequest       string
}

func (c *modelInfoCore) handler(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case r.URL.Path == coreConfigPath && r.Method == http.MethodGet:
		c.configGets++
		if r.Header.Get("Authorization") != "Bearer "+c.management {
			c.wrongRequest = "config authorization"
		}
		_, _ = w.Write(c.config)
	case r.URL.Path == coreConfigPath && r.Method == http.MethodPut:
		c.puts++
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		c.models++
		if r.URL.RequestURI() != modelInfoCatalogPath {
			c.wrongRequest = "catalog query"
		}
		if r.Header.Get("Authorization") != "Bearer "+c.selectedKey || strings.Contains(r.URL.RawQuery, c.selectedKey) || r.Header.Get("Authorization") == "Bearer "+c.management {
			c.wrongRequest = "catalog authorization"
		}
		status := c.catalogCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(c.catalog)
		if c.configAfterCatalog != nil {
			c.config = append([]byte(nil), c.configAfterCatalog...)
		}
	case r.URL.Path == modelInfoIngestPath && r.Method == http.MethodPost:
		c.ingests++
		if r.Header.Get("Authorization") != "Bearer "+c.management || r.Header.Get(workerTokenHeader) != c.workerToken {
			c.wrongRequest = "ingest authorization"
		}
		var request struct {
			CatalogBase64 string `json:"catalog_base64"`
		}
		if err := decodeStrictJSON(r.Body, 16<<20, &request); err != nil {
			c.wrongRequest = "ingest envelope"
		} else {
			c.lastIngest, _ = base64.StdEncoding.Strict().DecodeString(request.CatalogBase64)
		}
		status := c.ingestCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if c.ingestReceipt != nil {
			_, _ = w.Write(c.ingestReceipt)
		} else if status >= 200 && status < 300 {
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "catalog_sha256": configVersion(c.lastIngest)})
		} else {
			_, _ = w.Write([]byte(`{"secret":"must be ignored"}`))
		}
	default:
		c.wrongRequest = r.Method + " " + r.URL.RequestURI()
		http.NotFound(w, r)
	}
}

func newModelInfoHarness(t *testing.T, key string, config []byte) (*ModelInfoRefresher, Settings, *modelInfoCore, func()) {
	t.Helper()
	core := &modelInfoCore{config: config, catalog: []byte(`{"data":[{"id":"model-a"}]}`), selectedKey: strings.TrimSpace(key), management: "management-secret", workerToken: "worker-secret"}
	server := httptest.NewServer(http.HandlerFunc(core.handler))
	client := NewLoopbackClient()
	settings := Settings{CoreOrigin: server.URL, ManagementKey: core.management, WorkerToken: core.workerToken, ModelInfoKeyFingerprint: catalogFingerprint(key)}
	engine := NewCommitEngine(client, nil, func() Settings { return settings }, nil)
	return NewModelInfoRefresher(client, engine), settings, core, server.Close
}

func TestModelInfoRefreshUsesNormalizedRootKeyFixedQueryAndExactCatalog(t *testing.T) {
	key := "proxy-api-key"
	config := []byte("api-keys:\n  - \"  " + key + "  \"\n")
	refresher, settings, core, closeFn := newModelInfoHarness(t, key, config)
	defer closeFn()
	catalog := []byte{0, '<', '&', 0xc3, 0xa9}
	core.catalog = catalog
	var progress []RunState
	outcome := refresher.Refresh(context.Background(), settings, func(state RunState) { progress = append(progress, state) })
	core.mu.Lock()
	defer core.mu.Unlock()
	if outcome.Code != "" || outcome.State != StateSucceeded || outcome.Changed || outcome.Version != configVersion(config) {
		t.Fatalf("outcome=%+v", outcome)
	}
	if core.models != 1 || core.ingests != 1 || core.puts != 0 || core.wrongRequest != "" || !bytes.Equal(core.lastIngest, catalog) {
		t.Fatalf("models=%d ingests=%d puts=%d wrong=%q ingest=%x", core.models, core.ingests, core.puts, core.wrongRequest, core.lastIngest)
	}
	if len(progress) != 1 || progress[0] != StateFetching {
		t.Fatalf("progress=%v", progress)
	}
}

func TestModelInfoExecutorEmitsPlanningFetchingAndNeverPUTs(t *testing.T) {
	key := "proxy-api-key"
	config := []byte("api-keys:\n  - " + key + "\n")
	refresher, settings, core, closeFn := newModelInfoHarness(t, key, config)
	defer closeFn()
	executor := NewWriterExecutor(nil, refresher.engine, refresher)
	type event struct {
		attempt int
		state   RunState
	}
	var events []event
	outcome := executor.ExecuteWithProgress(context.Background(), OperationModelInfo, settings, func(attempt int, state RunState) {
		events = append(events, event{attempt, state})
	})
	core.mu.Lock()
	defer core.mu.Unlock()
	if outcome.Code != "" || core.puts != 0 {
		t.Fatalf("outcome=%+v puts=%d", outcome, core.puts)
	}
	want := []event{{1, StatePlanning}, {1, StateFetching}}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Fatalf("events=%v", events)
	}
}

func TestSelectCatalogAPIKeyRequiresOneNormalizedMatch(t *testing.T) {
	key := "proxy-api-key"
	fingerprint := catalogFingerprint(key)
	tests := []struct {
		name   string
		config string
		want   bool
	}{
		{name: "one", config: "api-keys:\n  - other\n  - \"  proxy-api-key  \"\n", want: true},
		{name: "none", config: "api-keys:\n  - other\n"},
		{name: "duplicate normalized", config: "api-keys:\n  - proxy-api-key\n  - \" proxy-api-key \"\n"},
		{name: "empty", config: "api-keys:\n  - \" \"\n"},
		{name: "non scalar", config: "api-keys:\n  - value: proxy-api-key\n"},
		{name: "duplicate key", config: "api-keys:\n  - proxy-api-key\napi-keys:\n  - other\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := selectCatalogAPIKey([]byte(test.config), fingerprint)
			if ok != test.want || ok && got != key {
				t.Fatalf("got=%q ok=%v", got, ok)
			}
		})
	}
	if _, ok := selectCatalogAPIKey([]byte("api-keys:\n  - proxy-api-key\n"), strings.ToUpper(fingerprint)); ok {
		t.Fatal("uppercase fingerprint accepted")
	}
}

func TestModelInfoUnavailableKeyNeverFetchesOrIngests(t *testing.T) {
	key := "proxy-api-key"
	for _, config := range [][]byte{
		[]byte("api-keys:\n  - other\n"),
		[]byte("api-keys:\n  - proxy-api-key\n  - \" proxy-api-key \"\n"),
	} {
		refresher, settings, core, closeFn := newModelInfoHarness(t, key, config)
		outcome := refresher.Refresh(context.Background(), settings, nil)
		closeFn()
		core.mu.Lock()
		if outcome.Code != CodeCatalogKeyUnavailable || core.models != 0 || core.ingests != 0 || core.puts != 0 {
			t.Fatalf("outcome=%+v models=%d ingests=%d puts=%d", outcome, core.models, core.ingests, core.puts)
		}
		core.mu.Unlock()
	}
}

func TestModelInfoRejectsAuthoritativeConfigDriftBeforeIngest(t *testing.T) {
	key := "proxy-api-key"
	initial := []byte("api-keys:\n  - " + key + "\n")
	refresher, settings, core, closeFn := newModelInfoHarness(t, key, initial)
	defer closeFn()
	core.configAfterCatalog = append(append([]byte(nil), initial...), []byte("# external change\n")...)
	outcome := refresher.Refresh(context.Background(), settings, nil)
	core.mu.Lock()
	defer core.mu.Unlock()
	if outcome.Code != CodeVersionConflict || outcome.State != StateFailed || outcome.Version != configVersion(core.configAfterCatalog) || core.configGets != 2 || core.models != 1 || core.ingests != 0 || core.puts != 0 {
		t.Fatalf("outcome=%+v gets=%d models=%d ingests=%d puts=%d", outcome, core.configGets, core.models, core.ingests, core.puts)
	}
}

func TestModelInfoOversizeSkipsIngest(t *testing.T) {
	key := "proxy-api-key"
	refresher, settings, core, closeFn := newModelInfoHarness(t, key, []byte("api-keys:\n  - "+key+"\n"))
	defer closeFn()
	core.catalog = bytes.Repeat([]byte("x"), maxCatalogBytes+1)
	outcome := refresher.Refresh(context.Background(), settings, nil)
	core.mu.Lock()
	defer core.mu.Unlock()
	if outcome.Code != CodeCatalogTooLarge || core.models != 1 || core.ingests != 0 || core.puts != 0 {
		t.Fatalf("outcome=%+v models=%d ingests=%d puts=%d", outcome, core.models, core.ingests, core.puts)
	}
}

func TestModelInfoIngestStatusMappingIsSanitized(t *testing.T) {
	key := "proxy-api-key"
	for _, test := range []struct {
		status int
		code   ErrorCode
	}{
		{status: http.StatusBadRequest, code: CodeCatalogInvalid},
		{status: http.StatusRequestEntityTooLarge, code: CodeCatalogTooLarge},
		{status: http.StatusInternalServerError, code: CodeCoreUnavailable},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			refresher, settings, core, closeFn := newModelInfoHarness(t, key, []byte("api-keys:\n  - "+key+"\n"))
			core.ingestCode = test.status
			outcome := refresher.Refresh(context.Background(), settings, nil)
			closeFn()
			core.mu.Lock()
			defer core.mu.Unlock()
			encoded, _ := json.Marshal(outcome)
			if outcome.Code != test.code || core.puts != 0 || bytes.Contains(encoded, []byte(key)) || bytes.Contains(encoded, []byte("must be ignored")) {
				t.Fatalf("outcome=%+v puts=%d", outcome, core.puts)
			}
		})
	}
}

func TestModelInfoRequiresExactSuccessfulIngestReceipt(t *testing.T) {
	key := "proxy-api-key"
	catalogHash := configVersion([]byte(`{"data":[{"id":"model-a"}]}`))
	for _, receipt := range [][]byte{
		[]byte(`{"count":1,"catalog_sha256":"wrong"}`),
		[]byte(fmt.Sprintf(`{"count":1,"catalog_sha256":%q,"unexpected":true}`, catalogHash)),
		[]byte(fmt.Sprintf(`{"count":-1,"catalog_sha256":%q}`, catalogHash)),
		[]byte(fmt.Sprintf(`{"catalog_sha256":%q}`, catalogHash)),
		[]byte(fmt.Sprintf(`{"count":1,"count":1,"catalog_sha256":%q}`, catalogHash)),
		[]byte(fmt.Sprintf(`{"count":1,"catalog_sha256":%q} {}`, catalogHash)),
		[]byte(fmt.Sprintf(`{"COUNT":1,"CATALOG_SHA256":%q}`, catalogHash)),
		[]byte(fmt.Sprintf(`{"count":1,"COUNT":1,"catalog_sha256":%q}`, catalogHash)),
		[]byte(`not-json`),
	} {
		refresher, settings, core, closeFn := newModelInfoHarness(t, key, []byte("api-keys:\n  - "+key+"\n"))
		core.ingestReceipt = receipt
		outcome := refresher.Refresh(context.Background(), settings, nil)
		closeFn()
		if outcome.State != StateFailed || outcome.Code != CodeCatalogInvalid {
			t.Fatalf("receipt=%s outcome=%+v", receipt, outcome)
		}
	}
}

func TestModelInfoCatalogNon2xxIsSanitizedAndSkipsIngest(t *testing.T) {
	key := "proxy-api-key"
	refresher, settings, core, closeFn := newModelInfoHarness(t, key, []byte("api-keys:\n  - "+key+"\n"))
	defer closeFn()
	core.catalogCode = http.StatusUnauthorized
	core.catalog = []byte("provider-secret-detail")
	outcome := refresher.Refresh(context.Background(), settings, nil)
	core.mu.Lock()
	defer core.mu.Unlock()
	if outcome.Code != CodeCoreUnavailable || core.ingests != 0 || core.puts != 0 || strings.Contains(string(outcome.Code), "secret") {
		t.Fatalf("outcome=%+v ingests=%d puts=%d", outcome, core.ingests, core.puts)
	}
}
