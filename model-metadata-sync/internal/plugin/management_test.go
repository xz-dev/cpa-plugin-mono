package plugin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func managementRequest(method, path, token string, body []byte) pluginapi.ManagementRequest {
	headers := http.Header{"Authorization": []string{"Bearer management-secret-must-be-ignored"}}
	if token != "" {
		headers.Set(workerTokenHeader, token)
	}
	return pluginapi.ManagementRequest{Method: method, Path: path, Headers: headers, Body: body}
}

func TestManagementRegistersOnlyPlannerAndWriterStatus(t *testing.T) {
	routes := New(authFixture()).ManagementRoutes().Routes
	if len(routes) != 2 || routes[0].Method != http.MethodPost || routes[0].Path != planPath || routes[1].Method != http.MethodGet || routes[1].Path != writerStatusPath {
		t.Fatalf("routes=%+v", routes)
	}
}

func TestInternalRoutesRequireWorkerTokenBeyondManagementAuthorization(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New(authFixture())
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	request := managementRequest(http.MethodPost, planPath, "", initialRequest(t, strings.Repeat("a", 64), snapshotYAML()))
	response := service.HandleManagement(request)
	if response.StatusCode != http.StatusUnauthorized || strings.Contains(string(response.Body), "root-secret") || strings.Contains(string(response.Body), "management-secret") {
		t.Fatalf("response=%d %s", response.StatusCode, response.Body)
	}
	if response := service.HandleManagement(managementRequest(http.MethodGet, writerStatusPath, "wrong", nil)); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status response=%d %s", response.StatusCode, response.Body)
	}
}

func TestConfigYAMLAcceptsCoreNormalizedLifecycleAndOpaqueStore(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	raw := append([]byte(`enabled: true
priority: 7
store:
  version: 0.1.0
  manifest:
    linux:
      amd64:
        sha256: harmless-diagnostic-value
`), validConfigYAML()...)
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("Core-normalized ConfigYAML must be accepted: %v", err)
	}
	sum := sha256.Sum256(raw)
	if cfg.SHA256 != hex.EncodeToString(sum[:]) || len(cfg.Channels) != 2 || cfg.Channels[0].Selector.Name != "axis" {
		t.Fatalf("cfg=%+v", cfg)
	}
}

func TestConfigYAMLStrictAndForbiddenFields(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	for name, raw := range map[string]string{
		"config file":         "worker_token_env: TEST_WRITER_TOKEN\nconfig_file: /tmp/config.json\n",
		"management key":      "worker_token_env: TEST_WRITER_TOKEN\nmanagement_key_env: MANAGEMENT_KEY\n",
		"management url":      "worker_token_env: TEST_WRITER_TOKEN\nmanagement_base_url: http://127.0.0.1:8317\n",
		"interval":            "worker_token_env: TEST_WRITER_TOKEN\ninterval: 1h\n",
		"legacy profile":      "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n- {enabled: true, kind: openai-compatibility, selector: {name: x, base_url: https://x.example/v1}, profile: openai_models}\n",
		"duplicate token key": "worker_token_env: TEST_WRITER_TOKEN\nworker_token_env: TEST_WRITER_TOKEN\n",
		"aliased channel":     "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n- &channel {enabled: true, kind: openai-compatibility, selector: {name: x, base_url: https://x.example/v1}}\n",
		"public source url":   "worker_token_env: TEST_WRITER_TOKEN\nmodelparams_url: https://evil.example\n",
		"plaintext token":     "worker_token_env: TEST_WRITER_TOKEN\nworker_token: secret\n",
		"duplicate selector":  "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n- {enabled: true, kind: openai-compatibility, selector: {name: x, base_url: https://x.example/v1}}\n- {enabled: false, kind: openai-compatibility, selector: {name: x, base_url: https://x.example:443/v1/}}\n",
		"http selector":       "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n- {enabled: true, kind: openai-compatibility, selector: {name: x, base_url: http://x.example/v1}}\n",
		"bad source order":    "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n- enabled: true\n  kind: openai-compatibility\n  selector: {name: x, base_url: https://x.example/v1}\n  metadata_sources: [models.dev/openai, modelparams.dev/openai/subscription]\n",
		"claude codex":        "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n- enabled: true\n  kind: claude\n  selector: {config_index: 0, base_url: https://api.anthropic.com, prefix: anthropic}\n  codex_manifest: true\n",
		"store indirection":   "worker_token_env: TEST_WRITER_TOKEN\nstore: &store {version: 0.1.0}\nchannels: []\n",
		"multiple documents":  "worker_token_env: TEST_WRITER_TOKEN\nchannels: []\n---\nchannels: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if cfg, err := parseConfig(validConfigYAML()); err != nil || len(cfg.Channels) != 2 || !cfg.Channels[0].CodexManifest {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	t.Setenv("TEST_WRITER_TOKEN", "")
	if _, err := parseConfig(validConfigYAML()); err == nil {
		t.Fatal("missing token accepted")
	}
}

func TestManagementReplayFenceGenerationShutdownAndStatus(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	version, snapshot := strings.Repeat("a", 64), snapshotYAML()
	newService := func(t *testing.T) *Service {
		service := New(authFixture())
		if err := service.Configure(validConfigYAML()); err != nil {
			t.Fatal(err)
		}
		return service
	}
	start := func(t *testing.T, s *Service) fetchDescriptor {
		r := s.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", initialRequest(t, version, snapshot)))
		if r.StatusCode != 200 {
			t.Fatalf("start=%d %s", r.StatusCode, r.Body)
		}
		var e fetchEnvelope
		if err := json.Unmarshal(r.Body, &e); err != nil {
			t.Fatal(err)
		}
		return e.NextFetch
	}
	body := []byte(`{"models":[{"slug":"gpt-x"}]}`)
	t.Run("exact and concurrent replay", func(t *testing.T) {
		s := newService(t)
		d := start(t, s)
		raw := continuationRequest(t, version, snapshot, d, body)
		goStart := make(chan struct{})
		statuses := make(chan int, 2)
		for range 2 {
			go func() {
				<-goStart
				statuses <- s.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", raw)).StatusCode
			}()
		}
		close(goStart)
		ok, bad := 0, 0
		for range 2 {
			switch <-statuses {
			case 200:
				ok++
			case 400:
				bad++
			}
		}
		if ok != 1 || bad != 1 {
			t.Fatalf("ok=%d bad=%d", ok, bad)
		}
	})
	t.Run("latest attempt", func(t *testing.T) {
		s := newService(t)
		old := start(t, s)
		current := start(t, s)
		if r := s.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", continuationRequest(t, version, snapshot, old, body))); r.StatusCode != 400 {
			t.Fatalf("old=%d", r.StatusCode)
		}
		if r := s.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", continuationRequest(t, version, snapshot, current, body))); r.StatusCode != 200 {
			t.Fatalf("current=%d %s", r.StatusCode, r.Body)
		}
	})
	t.Run("reconfigure and shutdown", func(t *testing.T) {
		s := newService(t)
		d := start(t, s)
		t.Setenv("TEST_WRITER_TOKEN", "rotated")
		if err := s.Configure(validConfigYAML()); err != nil {
			t.Fatal(err)
		}
		raw := continuationRequest(t, version, snapshot, d, body)
		if r := s.HandleManagement(managementRequest(http.MethodPost, planPath, "rotated", raw)); r.StatusCode != 400 {
			t.Fatalf("stale=%d %s", r.StatusCode, r.Body)
		}
		s.Shutdown()
		if r := s.HandleManagement(managementRequest(http.MethodPost, planPath, "rotated", raw)); r.StatusCode != 401 {
			t.Fatalf("shutdown=%d", r.StatusCode)
		}
	})
	t.Run("status secrecy and active", func(t *testing.T) {
		host := authFixture()
		host.blockList = make(chan struct{})
		host.listStarted = make(chan struct{}, 1)
		s := New(host)
		if err := s.Configure(validConfigYAML()); err != nil {
			t.Fatal(err)
		}
		done := make(chan pluginapi.ManagementResponse, 1)
		go func() {
			done <- s.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", initialRequest(t, version, snapshot)))
		}()
		select {
		case <-host.listStarted:
		case <-time.After(time.Second):
			t.Fatal("plan not active")
		}
		response := s.HandleManagement(managementRequest(http.MethodGet, writerStatusPath, "worker-secret", nil))
		var raw map[string]json.RawMessage
		if json.Unmarshal(response.Body, &raw) != nil || len(raw) != 4 {
			t.Fatalf("status=%s", response.Body)
		}
		var status WorkerStatus
		_ = json.Unmarshal(response.Body, &status)
		sum := sha256.Sum256(validConfigYAML())
		if !status.ActivePlan || status.ReconfigureSeq != 1 || status.ConfigSHA256 != hex.EncodeToString(sum[:]) || strings.Contains(string(response.Body), "secret") {
			t.Fatalf("status=%+v %s", status, response.Body)
		}
		close(host.blockList)
		<-done
	})
}

func TestMalformedCatalogResponseIsSanitized(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	version, snapshot := strings.Repeat("a", 64), snapshotYAML()
	validOpenAI := []byte(`{"models":[{"slug":"gpt-x"}]}`)
	validModelparams := []byte(`{"models":[{"provider":"openai","authType":"subscription","model":"gpt-x","params":[]}]}`)
	malformed := map[string][]byte{
		"openai_models": []byte(`{"models":[{"id":"secret-body-marker","id":1}]}`),
		"modelparams":   []byte(`{"models":[{"provider":"openai","model":"valid"},{"provider":"openai","provider":"secret-body-marker"}]}`),
		"modelsdev":     []byte(`{"openai":{"models":{"valid":{"id":"valid","limit":{"context":1000}},"secret-body-marker":{"id":1}}}}`),
	}
	for target, malformedBody := range malformed {
		t.Run(target, func(t *testing.T) {
			service := New(authFixture())
			if err := service.Configure(validConfigYAML()); err != nil {
				t.Fatal(err)
			}
			response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", initialRequest(t, version, snapshot)))
			var envelope fetchEnvelope
			if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &envelope) != nil {
				t.Fatalf("initial=%d %s", response.StatusCode, response.Body)
			}
			for envelope.NextFetch.Kind != target {
				var body []byte
				switch envelope.NextFetch.Kind {
				case "openai_models":
					body = validOpenAI
				case "modelparams":
					body = validModelparams
				default:
					t.Fatalf("unexpected prerequisite fetch %s", envelope.NextFetch.Kind)
				}
				response = service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", continuationRequest(t, version, snapshot, envelope.NextFetch, body)))
				if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &envelope) != nil {
					t.Fatalf("prerequisite=%d %s", response.StatusCode, response.Body)
				}
			}
			response = service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", continuationRequest(t, version, snapshot, envelope.NextFetch, malformedBody)))
			if response.StatusCode != http.StatusBadRequest || strings.Contains(string(response.Body), "secret-body-marker") || string(response.Body) != `{"error_code":"provider_fetch_invalid"}` {
				t.Fatalf("response=%d %s", response.StatusCode, response.Body)
			}
		})
	}
}

func TestManagementFullPlannerRoundTrip(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New(authFixture())
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	version, snapshot := strings.Repeat("9", 64), snapshotYAML()
	response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", initialRequest(t, version, snapshot)))
	bodies := map[string][]byte{
		"openai_models": []byte(`{"models":[{"slug":"gpt-x","max_tokens":48000}]}`),
		"modelparams":   []byte(`{"models":[{"provider":"openai","authType":"subscription","model":"gpt-x","params":[{"path":"max_tokens","group":"generation_length","type":"integer","range":{"max":32000}}]}]}`),
		"modelsdev":     []byte(`{"openai":{"models":{"gpt-x":{"id":"gpt-x","modalities":{"output":["text"]}}}}}`),
		"claude_models": []byte(`{"data":[{"id":"claude-x","max_input_tokens":200000,"max_tokens":8192}],"has_more":false}`),
	}
	for range 4 {
		if response.StatusCode != http.StatusOK {
			t.Fatalf("response=%d %s", response.StatusCode, response.Body)
		}
		var envelope fetchEnvelope
		if err := json.Unmarshal(response.Body, &envelope); err != nil || envelope.NextFetch.Kind == "" {
			t.Fatalf("fetch envelope=%s err=%v", response.Body, err)
		}
		body, ok := bodies[envelope.NextFetch.Kind]
		if !ok {
			t.Fatalf("unexpected fetch kind %s", envelope.NextFetch.Kind)
		}
		response = service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", continuationRequest(t, version, snapshot, envelope.NextFetch, body)))
	}
	if response.StatusCode != http.StatusOK || strings.Contains(string(response.Body), "worker-secret") || strings.Contains(string(response.Body), `"api_key":"secret"`) {
		t.Fatalf("final=%d %s", response.StatusCode, response.Body)
	}
	var final finalEnvelope
	if err := json.Unmarshal(response.Body, &final); err != nil || !final.Report.Changed {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	proposed, err := base64.StdEncoding.Strict().DecodeString(final.ConfigBase64)
	if err != nil || !strings.Contains(string(proposed), "max-output-tokens: 120000") || !strings.Contains(string(proposed), "max-input-tokens: 200000") {
		t.Fatalf("proposal error=%v\n%s", err, proposed)
	}
}

func TestPersistentAuthBindingAcrossPublicAndLaterChannelFetches(t *testing.T) {
	version, snapshot := strings.Repeat("f", 64), snapshotYAML()
	openAI := []byte(`{"models":[{"slug":"gpt-x"}]}`)
	modelparams := []byte(`{"models":[{"provider":"openai","authType":"subscription","model":"gpt-x","params":[]}]}`)
	modelsdev := []byte(`{"openai":{"models":{"gpt-x":{"id":"gpt-x","limit":{"context":1000}}}}}`)
	removeOpenAI := func(host *fakeAuthHost) {
		host.mu.Lock()
		defer host.mu.Unlock()
		host.entries = append([]AuthEntry(nil), host.entries[1:]...)
		delete(host.physical, "openai-auth")
	}
	continuePlan := func(t *testing.T, cfg runtimeConfig, host *fakeAuthHost, result any, body []byte) (any, string) {
		t.Helper()
		descriptor := result.(fetchEnvelope).NextFetch
		return plan(continuationRequest(t, version, snapshot, descriptor, body), cfg, host)
	}
	start := func(t *testing.T) (runtimeConfig, *fakeAuthHost, any) {
		t.Helper()
		cfg, host := testConfig(t), authFixture()
		result, code := plan(initialRequest(t, version, snapshot), cfg, host)
		if code != "" {
			t.Fatal(code)
		}
		return cfg, host, result
	}
	t.Run("before modelparams response", func(t *testing.T) {
		cfg, host, result := start(t)
		result = advance(t, cfg, host, version, snapshot, result, openAI)
		removeOpenAI(host)
		if next, code := continuePlan(t, cfg, host, result, modelparams); next != nil || code != errorCredential {
			t.Fatalf("result=%+v code=%s", next, code)
		}
	})
	t.Run("before modelsdev response", func(t *testing.T) {
		cfg, host, result := start(t)
		result = advance(t, cfg, host, version, snapshot, result, openAI)
		result = advance(t, cfg, host, version, snapshot, result, modelparams)
		removeOpenAI(host)
		if next, code := continuePlan(t, cfg, host, result, modelsdev); next != nil || code != errorCredential {
			t.Fatalf("result=%+v code=%s", next, code)
		}
	})
	t.Run("prior channel during later channel", func(t *testing.T) {
		cfg, host, result := start(t)
		result = advance(t, cfg, host, version, snapshot, result, openAI)
		result = advance(t, cfg, host, version, snapshot, result, modelparams)
		result = advance(t, cfg, host, version, snapshot, result, modelsdev)
		if descriptor := result.(fetchEnvelope).NextFetch; descriptor.Kind != "claude_models" {
			t.Fatalf("descriptor=%+v", descriptor)
		}
		removeOpenAI(host)
		if next, code := continuePlan(t, cfg, host, result, []byte(`{"data":[],"has_more":false}`)); next != nil || code != errorCredential {
			t.Fatalf("result=%+v code=%s", next, code)
		}
	})
	t.Run("same index path drift", func(t *testing.T) {
		cfg, host, result := start(t)
		host.mu.Lock()
		host.entries[0].Path = "/auth/replaced-openai.json"
		host.physical["openai-auth"] = AuthPhysical{AuthIndex: "openai-auth", Path: "/auth/replaced-openai.json", JSON: []byte(`{"type":"openai-compatibility","base_url":"https://axis.example/v1"}`)}
		host.mu.Unlock()
		if next, code := continuePlan(t, cfg, host, result, openAI); next != nil || code != errorCredential {
			t.Fatalf("result=%+v code=%s", next, code)
		}
	})
}

func TestContinuationSubstitutionAndBounds(t *testing.T) {
	cfg, host := testConfig(t), authFixture()
	version, snapshot := strings.Repeat("b", 64), snapshotYAML()
	result, code := plan(initialRequest(t, version, snapshot), cfg, host)
	if code != "" {
		t.Fatal(code)
	}
	descriptor := result.(fetchEnvelope).NextFetch
	body := []byte(`{"models":[{"slug":"gpt-x"}]}`)
	mutated := append([]byte("# changed\n"), snapshot...)
	for name, raw := range map[string][]byte{"snapshot": continuationRequest(t, version, mutated, descriptor, body), "version": continuationRequest(t, strings.Repeat("c", 64), snapshot, descriptor, body)} {
		t.Run(name, func(t *testing.T) {
			if result, code := plan(raw, cfg, host); result != nil || code != errorInvalid {
				t.Fatalf("result=%+v code=%s", result, code)
			}
		})
	}
	wrong := descriptor
	wrong.RequestID = "substituted"
	if result, code := plan(continuationRequest(t, version, snapshot, wrong, body), cfg, host); result != nil || code != errorInvalid {
		t.Fatalf("request substitution result=%+v code=%s", result, code)
	}
	if result, code := plan(continuationRequest(t, version, snapshot, descriptor, make([]byte, maxCatalogBytes+1)), cfg, host); result != nil || code != errorTooLarge {
		t.Fatalf("oversize result=%+v code=%s", result, code)
	}
	var state continuationState
	stateRaw, err := base64.StdEncoding.Strict().DecodeString(descriptor.ContinuationBase64)
	if err != nil || decodeStrictJSONBytes(stateRaw, &state) != nil {
		t.Fatal("decode continuation state")
	}
	state.ProviderBytes = maxCatalogBytes - 1
	stateRaw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	descriptor.ContinuationBase64 = base64.StdEncoding.EncodeToString(stateRaw)
	if result, code := plan(continuationRequest(t, version, snapshot, descriptor, []byte("{}")), cfg, host); result != nil || code != errorTooLarge {
		t.Fatalf("cumulative oversize result=%+v code=%s", result, code)
	}
	state = continuationState{Step: fetchStep{Kind: "modelparams", ChannelIndex: 0}, UpstreamByID: map[string]upstreamEntry{"oversize": {ID: strings.Repeat("x", maxContinuationBytes)}}}
	if result, code := nextFetch(version, documentForTest(t, snapshot), cfg, host, state); result != nil || code != errorTooLarge {
		t.Fatalf("oversize continuation result=%+v code=%s", result, code)
	}
}

func documentForTest(t *testing.T, snapshot []byte) *yaml.Node {
	t.Helper()
	document, err := parseSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestAuthFailsClosedAndPostFetchRevalidates(t *testing.T) {
	cfg := testConfig(t)
	cfg.Channels = cfg.Channels[:1]
	version, snapshot := strings.Repeat("d", 64), snapshotYAML()
	for name, mutate := range map[string]func(*fakeAuthHost){
		"missing":     func(h *fakeAuthHost) { h.entries = nil },
		"config only": func(h *fakeAuthHost) { h.entries[0].Source = "memory"; h.entries[0].Path = "" },
		"disabled":    func(h *fakeAuthHost) { h.entries[0].Disabled = true },
		"folded physical identity": func(h *fakeAuthHost) {
			h.physical["openai-auth"] = AuthPhysical{AuthIndex: "openai-auth", Path: "/auth/openai.json", JSON: []byte(`{"type":"openai-compatibility","BASE_URL":"https://axis.example/v1"}`)}
		},
		"duplicate physical identity": func(h *fakeAuthHost) {
			h.physical["openai-auth"] = AuthPhysical{AuthIndex: "openai-auth", Path: "/auth/openai.json", JSON: []byte(`{"type":"openai-compatibility","base_url":"https://axis.example/v1","BASE_URL":"https://axis.example/v1"}`)}
		},
		"ambiguous": func(h *fakeAuthHost) {
			h.entries = append(h.entries, h.entries[0])
			h.entries[2].AuthIndex = "duplicate"
			h.physical["duplicate"] = AuthPhysical{AuthIndex: "duplicate", Path: "/auth/openai-duplicate.json", JSON: []byte(`{"type":"openai-compatibility","base_url":"https://axis.example/v1"}`)}
			h.entries[2].Path = "/auth/openai-duplicate.json"
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := authFixture()
			mutate(h)
			if _, code := plan(initialRequest(t, version, snapshot), cfg, h); code != errorCredential {
				t.Fatalf("code=%s", code)
			}
		})
	}
	h := authFixture()
	h.driftAfterList = 1
	first, code := plan(initialRequest(t, version, snapshot), cfg, h)
	if code != "" {
		t.Fatal(code)
	}
	if result, code := plan(continuationRequest(t, version, snapshot, first.(fetchEnvelope).NextFetch, []byte(`{"models":[{"slug":"gpt-x"}]}`)), cfg, h); result != nil || code != errorCredential {
		t.Fatalf("drift result=%+v code=%s", result, code)
	}
}
