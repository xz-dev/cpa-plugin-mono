package plugin

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func validConfigYAML() []byte {
	return []byte(`core_origin: http://127.0.0.1:8317
management_key_env: WRITER_MANAGEMENT_KEY
model_info_proxy_api_key_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
worker_token_env: WRITER_WORKER_TOKEN
auto_pull_interval: 5m
metadata_sync_interval: 1h
model_info_interval: 0s
max_version_retries: 2
sync_epoch: epoch-a
`)
}

func setValidSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("WRITER_MANAGEMENT_KEY", "management-secret")
	t.Setenv("WRITER_WORKER_TOKEN", "worker-secret")
}

func TestParseConfigValidatesFoundationContract(t *testing.T) {
	setValidSecrets(t)
	got, err := parseSettings(validConfigYAML())
	if err != nil {
		t.Fatal(err)
	}
	if got.CoreOrigin != "http://127.0.0.1:8317" || got.AutoPullInterval != 5*time.Minute || got.MetadataSyncInterval != time.Hour || got.MaxVersionRetries != 2 {
		t.Fatalf("settings=%+v", got)
	}
	if got.ManagementKey == "" || got.WorkerToken == "" {
		t.Fatal("resolved secrets must be retained in memory")
	}
}

func TestParseConfigAcceptsCoreNormalizedFields(t *testing.T) {
	setValidSecrets(t)
	raw := append([]byte("enabled: true\npriority: 7\nstore:\n  version: 0.1.0\n"), validConfigYAML()...)
	if _, err := parseSettings(raw); err != nil {
		t.Fatalf("Core-normalized ConfigYAML must be accepted: %v", err)
	}
}

func TestParseConfigRejectsMultipleDocuments(t *testing.T) {
	setValidSecrets(t)
	raw := append(append([]byte(nil), validConfigYAML()...), []byte("---\ncore_origin: http://127.0.0.1:8317\n")...)
	if _, err := parseSettings(raw); err == nil {
		t.Fatal("multiple YAML documents must be rejected")
	}
}

func TestParseConfigRejectsInvalidInputs(t *testing.T) {
	setValidSecrets(t)
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{"hostname", "core_origin: http://127.0.0.1:8317", "core_origin: http://localhost:8317"},
		{"non-loopback", "core_origin: http://127.0.0.1:8317", "core_origin: http://192.0.2.1:8317"},
		{"userinfo", "core_origin: http://127.0.0.1:8317", "core_origin: http://u@127.0.0.1:8317"},
		{"path", "core_origin: http://127.0.0.1:8317", "core_origin: http://127.0.0.1:8317/x"},
		{"query", "core_origin: http://127.0.0.1:8317", "core_origin: 'http://127.0.0.1:8317?q=1'"},
		{"fragment", "core_origin: http://127.0.0.1:8317", "core_origin: 'http://127.0.0.1:8317#x'"},
		{"https", "core_origin: http://127.0.0.1:8317", "core_origin: https://127.0.0.1:8317"},
		{"bad env name", "management_key_env: WRITER_MANAGEMENT_KEY", "management_key_env: bad-name"},
		{"missing secret", "management_key_env: WRITER_MANAGEMENT_KEY", "management_key_env: MISSING_WRITER_SECRET"},
		{"fingerprint uppercase", "model_info_proxy_api_key_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "model_info_proxy_api_key_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeF"},
		{"short interval", "auto_pull_interval: 5m", "auto_pull_interval: 59s"},
		{"long interval", "metadata_sync_interval: 1h", "metadata_sync_interval: 25h"},
		{"retry too high", "max_version_retries: 2", "max_version_retries: 6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Replace(string(validConfigYAML()), tt.old, tt.new, 1)
			if _, err := parseSettings([]byte(raw)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseConfigRejectsPlaintextSecretFields(t *testing.T) {
	setValidSecrets(t)
	for _, field := range []string{"management_key", "worker_token", "model_info_proxy_api_key"} {
		raw := append(validConfigYAML(), []byte(field+": plaintext\n")...)
		if _, err := parseSettings(raw); err == nil {
			t.Fatalf("expected %s rejection", field)
		}
	}
}

func TestConfigurePreservesIdentityAndActiveSnapshot(t *testing.T) {
	setValidSecrets(t)
	now := time.Unix(1000, 0)
	s := New(ExecutorFunc(func(context.Context, Operation, Settings) Outcome { return Outcome{Code: CodeNotImplemented} }), WithClock(func() time.Time { return now }))
	defer s.Shutdown()
	if err := s.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	before := s.Status()
	active := s.settingsSnapshot()

	raw := []byte(string(validConfigYAML()))
	raw = []byte(replaceLine(string(raw), "sync_epoch: epoch-a", "sync_epoch: epoch-b"))
	if err := s.Configure(raw); err != nil {
		t.Fatal(err)
	}
	after := s.Status()
	if before.InstanceID != after.InstanceID || after.ReconfigureSeq != before.ReconfigureSeq+1 {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	if before.ConfigSHA256 == after.ConfigSHA256 || active.SyncEpoch != "epoch-a" || s.settingsSnapshot().SyncEpoch != "epoch-b" {
		t.Fatalf("snapshot/config hash not preserved: active=%q current=%q", active.SyncEpoch, s.settingsSnapshot().SyncEpoch)
	}
}

func TestConfigureConcurrentWithSnapshotReads(t *testing.T) {
	setValidSecrets(t)
	aRaw := validConfigYAML()
	bRaw := []byte(replaceLine(replaceLine(replaceLine(string(aRaw),
		"sync_epoch: epoch-a", "sync_epoch: epoch-b"),
		"max_version_retries: 2", "max_version_retries: 4"),
		"auto_pull_interval: 5m", "auto_pull_interval: 10m"))
	a, err := parseSettings(aRaw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseSettings(bRaw)
	if err != nil {
		t.Fatal(err)
	}

	s := New(nil)
	defer s.Shutdown()
	if err := s.Configure(aRaw); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				got := s.settingsSnapshot()
				switch got.ConfigSHA256 {
				case a.ConfigSHA256:
					if got.SyncEpoch != a.SyncEpoch || got.MaxVersionRetries != a.MaxVersionRetries || got.AutoPullInterval != a.AutoPullInterval {
						t.Errorf("mixed A snapshot: %+v", got)
						return
					}
				case b.ConfigSHA256:
					if got.SyncEpoch != b.SyncEpoch || got.MaxVersionRetries != b.MaxVersionRetries || got.AutoPullInterval != b.AutoPullInterval {
						t.Errorf("mixed B snapshot: %+v", got)
						return
					}
				default:
					t.Errorf("unknown snapshot hash: %q", got.ConfigSHA256)
					return
				}
			}
		}()
		go func(i int) {
			defer wg.Done()
			raw := aRaw
			if i%2 == 1 {
				raw = bRaw
			}
			if err := s.Configure(raw); err != nil {
				t.Errorf("configure: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

func TestLoopbackClientRejectsProxyAndRedirects(t *testing.T) {
	setValidSecrets(t)
	settings, err := parseSettings(validConfigYAML())
	if err != nil {
		t.Fatal(err)
	}
	client := NewLoopbackClient()
	if client.Timeout != 120*time.Second {
		t.Fatalf("timeout=%v", client.Timeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("unexpected loopback transport: %T; proxy disabled=%v", client.Transport, ok && tr.Proxy == nil)
	}
	if err := client.CheckRedirect(nil, nil); err == nil {
		t.Fatal("redirect must be rejected")
	}
	req, err := NewCoreRequest(context.Background(), settings, http.MethodGet, "/v0/management/config.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "http://127.0.0.1:8317/v0/management/config.yaml" || req.Header.Get("Authorization") == "" {
		t.Fatalf("request=%s authorized=%v", req.URL, req.Header.Get("Authorization") != "")
	}
	if _, err := NewCoreRequest(context.Background(), settings, http.MethodGet, "https://example.com", nil); err == nil {
		t.Fatal("caller-selected destination must fail")
	}
}

func replaceLine(s, old, new string) string {
	return strings.Replace(s, old, new, 1)
}
