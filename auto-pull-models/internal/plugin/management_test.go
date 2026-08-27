package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func validConfigYAML() []byte {
	return []byte(`worker_token_env: TEST_WRITER_TOKEN
sync_epoch: epoch-a
channels:
  - enabled: true
    selector:
      name: provider-a
      base_url: https://a.example/v1
    mode: exclude
    patterns: []
`)
}

func managementRequest(method, path, token string, body []byte) pluginapi.ManagementRequest {
	headers := http.Header{}
	if token != "" {
		headers.Set(workerTokenHeader, token)
	}
	headers.Set("Authorization", "Bearer management-secret-must-be-ignored")
	return pluginapi.ManagementRequest{Method: method, Path: path, Headers: headers, Body: body}
}

func TestConfigYAMLRejectsLegacySecretsUnknownAndDuplicateSelectors(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	for name, raw := range map[string]string{
		"config file":        "worker_token_env: TEST_WRITER_TOKEN\nconfig_file: /tmp/config.json\n",
		"management key":     "worker_token_env: TEST_WRITER_TOKEN\nmanagement_key_env: MANAGEMENT_KEY\n",
		"plaintext token":    "worker_token_env: TEST_WRITER_TOKEN\nworker_token: plaintext\n",
		"keep aliases":       "worker_token_env: TEST_WRITER_TOKEN\nkeep_existing_aliases: true\n",
		"codex manifest":     "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n  - enabled: true\n    selector: {name: x, base_url: https://x.example/v1}\n    codex_manifest: true\n",
		"duplicate selector": "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n  - enabled: true\n    selector: {name: x, base_url: https://x.example/v1}\n  - enabled: false\n    selector: {name: x, base_url: https://x.example:443/v1/}\n",
		"http selector":      "worker_token_env: TEST_WRITER_TOKEN\nchannels:\n  - enabled: true\n    selector: {name: x, base_url: http://x.example/v1}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	t.Setenv("TEST_WRITER_TOKEN", "")
	if _, err := parseConfig(validConfigYAML()); err == nil {
		t.Fatal("missing worker token accepted")
	}
}

func TestAuthorizationCapturesOneConfigGeneration(t *testing.T) {
	t.Setenv("OLD_WRITER_TOKEN", "old-worker-secret")
	t.Setenv("NEW_WRITER_TOKEN", "new-worker-secret")
	service := New(&fakeAuthHost{})
	oldConfig := []byte("worker_token_env: OLD_WRITER_TOKEN\nchannels:\n  - enabled: true\n    selector: {name: old-channel, base_url: https://old.example/v1}\n    mode: exclude\n")
	newConfig := []byte("worker_token_env: NEW_WRITER_TOKEN\nchannels:\n  - enabled: true\n    selector: {name: new-channel, base_url: https://new.example/v1}\n    mode: exclude\n")
	if err := service.Configure(oldConfig); err != nil {
		t.Fatal(err)
	}
	captured, ok := service.beginAuthorizedPlan("old-worker-secret")
	if !ok {
		t.Fatal("old generation was not authorized")
	}
	if err := service.Configure(newConfig); err != nil {
		t.Fatal(err)
	}
	if captured.Channels[0].Selector.Name != "old-channel" {
		t.Fatalf("captured selector=%+v", captured.Channels[0].Selector)
	}
	service.endPlan()
	if _, ok := service.beginAuthorizedPlan("old-worker-secret"); ok {
		service.endPlan()
		t.Fatal("old token authorized after rotation")
	}
	if _, ok := service.authorizedWorkerStatus("old-worker-secret"); ok {
		t.Fatal("old token authorized for status after rotation")
	}
	current, ok := service.beginAuthorizedPlan("new-worker-secret")
	if !ok {
		t.Fatal("new token was not authorized")
	}
	defer service.endPlan()
	if current.Channels[0].Selector.Name != "new-channel" {
		t.Fatalf("current selector=%+v", current.Channels[0].Selector)
	}
}

func TestManagementRejectsContinuationReplayAndCrossAttemptSubstitution(t *testing.T) {
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	version, snapshot := strings.Repeat("a", 64), snapshotYAML()
	catalog := []byte(`{"data":[{"id":"old-a"}]}`)

	newService := func(t *testing.T) *Service {
		t.Helper()
		t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
		service := New(authFixture(selector))
		if err := service.Configure(validConfigYAML()); err != nil {
			t.Fatal(err)
		}
		return service
	}
	start := func(t *testing.T, service *Service, token string) fetchDescriptor {
		t.Helper()
		response := service.HandleManagement(managementRequest(http.MethodPost, planPath, token, initialRequest(t, version, snapshot)))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("initial status=%d body=%s", response.StatusCode, response.Body)
		}
		var envelope fetchEnvelope
		if err := json.Unmarshal(response.Body, &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.NextFetch
	}

	t.Run("exact replay", func(t *testing.T) {
		service := newService(t)
		descriptor := start(t, service, "worker-secret")
		raw := continuationRequest(t, version, snapshot, descriptor, catalog)
		if response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", raw)); response.StatusCode != http.StatusOK {
			t.Fatalf("first continuation status=%d body=%s", response.StatusCode, response.Body)
		}
		response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", raw))
		if response.StatusCode != http.StatusBadRequest || string(response.Body) != `{"error_code":"provider_fetch_invalid"}` {
			t.Fatalf("replay status=%d body=%s", response.StatusCode, response.Body)
		}
	})

	t.Run("concurrent replay consumes once", func(t *testing.T) {
		service := newService(t)
		descriptor := start(t, service, "worker-secret")
		raw := continuationRequest(t, version, snapshot, descriptor, catalog)
		startBoth := make(chan struct{})
		statuses := make(chan int, 2)
		for range 2 {
			go func() {
				<-startBoth
				response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", raw))
				statuses <- response.StatusCode
			}()
		}
		close(startBoth)
		ok, rejected := 0, 0
		for range 2 {
			switch status := <-statuses; status {
			case http.StatusOK:
				ok++
			case http.StatusBadRequest:
				rejected++
			default:
				t.Fatalf("unexpected status=%d", status)
			}
		}
		if ok != 1 || rejected != 1 {
			t.Fatalf("ok=%d rejected=%d", ok, rejected)
		}
	})

	t.Run("new attempt invalidates old", func(t *testing.T) {
		service := newService(t)
		oldDescriptor := start(t, service, "worker-secret")
		newDescriptor := start(t, service, "worker-secret")
		oldRaw := continuationRequest(t, version, snapshot, oldDescriptor, catalog)
		if response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", oldRaw)); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("old continuation status=%d body=%s", response.StatusCode, response.Body)
		}
		newRaw := continuationRequest(t, version, snapshot, newDescriptor, catalog)
		if response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", newRaw)); response.StatusCode != http.StatusOK {
			t.Fatalf("new continuation status=%d body=%s", response.StatusCode, response.Body)
		}
	})

	t.Run("shutdown invalidates pending", func(t *testing.T) {
		service := newService(t)
		descriptor := start(t, service, "worker-secret")
		service.Shutdown()
		raw := continuationRequest(t, version, snapshot, descriptor, catalog)
		if response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", raw)); response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("shutdown continuation status=%d body=%s", response.StatusCode, response.Body)
		}
	})

	t.Run("reconfigure invalidates pending", func(t *testing.T) {
		service := newService(t)
		descriptor := start(t, service, "worker-secret")
		t.Setenv("TEST_WRITER_TOKEN", "rotated-worker-secret")
		if err := service.Configure(validConfigYAML()); err != nil {
			t.Fatal(err)
		}
		raw := continuationRequest(t, version, snapshot, descriptor, catalog)
		if response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", raw)); response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("old token status=%d body=%s", response.StatusCode, response.Body)
		}
		response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "rotated-worker-secret", raw))
		if response.StatusCode != http.StatusBadRequest || string(response.Body) != `{"error_code":"provider_fetch_invalid"}` {
			t.Fatalf("stale continuation status=%d body=%s", response.StatusCode, response.Body)
		}
	})
}

func TestRoutesTokenStatusSequenceHashAndActivePlan(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	host := authFixture(selector)
	host.blockList = make(chan struct{})
	host.listStarted = make(chan struct{}, 1)
	service := New(host)
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	routes := service.ManagementRoutes()
	if len(routes.Routes) != 2 || len(routes.Resources) != 0 || routes.Routes[0].Method != http.MethodPost || routes.Routes[0].Path != planPath || routes.Routes[1].Method != http.MethodGet || routes.Routes[1].Path != writerStatusPath {
		t.Fatalf("routes=%+v resources=%+v", routes.Routes, routes.Resources)
	}
	if response := service.HandleManagement(managementRequest(http.MethodGet, writerStatusPath, "wrong", nil)); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status=%d", response.StatusCode)
	}

	planRequest := managementRequest(http.MethodPost, planPath, "worker-secret", initialRequest(t, strings.Repeat("a", 64), snapshotYAML()))
	done := make(chan pluginapi.ManagementResponse, 1)
	go func() {
		done <- service.HandleManagement(planRequest)
	}()
	select {
	case <-host.listStarted:
	case <-time.After(time.Second):
		t.Fatal("plan did not reach auth preflight")
	}
	statusResponse := service.HandleManagement(managementRequest(http.MethodGet, writerStatusPath, "worker-secret", nil))
	if statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("status=%d %s", statusResponse.StatusCode, statusResponse.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(statusResponse.Body, &raw); err != nil || len(raw) != 4 {
		t.Fatalf("status body=%s err=%v", statusResponse.Body, err)
	}
	var status WorkerStatus
	if err := json.Unmarshal(statusResponse.Body, &status); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(validConfigYAML())
	if !status.ActivePlan || status.InstanceID == "" || status.ReconfigureSeq != 1 || status.ConfigSHA256 != hex.EncodeToString(sum[:]) || strings.Contains(string(statusResponse.Body), "worker-secret") || strings.Contains(string(statusResponse.Body), "management-secret") {
		t.Fatalf("status=%+v body=%s", status, statusResponse.Body)
	}
	close(host.blockList)
	if response := <-done; response.StatusCode != http.StatusOK {
		t.Fatalf("plan=%d %s", response.StatusCode, response.Body)
	}
	statusResponse = service.HandleManagement(managementRequest(http.MethodGet, writerStatusPath, "worker-secret", nil))
	_ = json.Unmarshal(statusResponse.Body, &status)
	if status.ActivePlan {
		t.Fatal("active plan remained set")
	}

	if err := service.Configure([]byte("worker_token_env: TEST_WRITER_TOKEN\nunknown: true\n")); err == nil {
		t.Fatal("invalid reconfigure accepted")
	}
	statusResponse = service.HandleManagement(managementRequest(http.MethodGet, writerStatusPath, "worker-secret", nil))
	_ = json.Unmarshal(statusResponse.Body, &status)
	if status.ReconfigureSeq != 1 || status.ConfigSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("failed configure changed status=%+v", status)
	}
	second := []byte(strings.Replace(string(validConfigYAML()), "epoch-a", "epoch-b", 1))
	if err := service.Configure(second); err != nil {
		t.Fatal(err)
	}
	statusResponse = service.HandleManagement(managementRequest(http.MethodGet, writerStatusPath, "worker-secret", nil))
	_ = json.Unmarshal(statusResponse.Body, &status)
	if status.ReconfigureSeq != 2 {
		t.Fatalf("sequence=%d", status.ReconfigureSeq)
	}
}

func TestManagementErrorsAreSanitized(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	selector := ChannelSelector{Name: "provider-a", BaseURL: "https://a.example/v1"}
	service := New(authFixture(selector))
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	snapshot := []byte("api-keys: [root-secret]\nopenai-compatibility: [not-valid]\n")
	response := service.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", initialRequest(t, strings.Repeat("d", 64), snapshot)))
	if response.StatusCode != http.StatusBadRequest || string(response.Body) != `{"error_code":"provider_fetch_invalid"}` || strings.Contains(string(response.Body), "root-secret") {
		t.Fatalf("response=%d %s", response.StatusCode, response.Body)
	}
	host := authFixture(selector)
	host.physical["auth-0"] = AuthPhysical{AuthIndex: "auth-0", Path: "/auth/provider-0.json", JSON: []byte(`{"type":"openai-compatibility","base_url":"https://wrong.example/v1","api_key":"credential-secret-marker"}`)}
	credentialService := New(host)
	if err := credentialService.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	credentialResponse := credentialService.HandleManagement(managementRequest(http.MethodPost, planPath, "worker-secret", initialRequest(t, strings.Repeat("e", 64), snapshotYAML())))
	if credentialResponse.StatusCode != http.StatusUnprocessableEntity || string(credentialResponse.Body) != `{"error_code":"provider_credential_unavailable"}` || strings.Contains(string(credentialResponse.Body), "credential-secret-marker") {
		t.Fatalf("credential response=%d %s", credentialResponse.StatusCode, credentialResponse.Body)
	}

	unknown := service.HandleManagement(managementRequest(http.MethodGet, "/v0/management/plugins/auto-pull-models/json", "worker-secret", nil))
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy route status=%d", unknown.StatusCode)
	}
}
