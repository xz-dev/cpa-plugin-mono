package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

func TestLoopbackClientNeverUsesEnvironmentProxy(t *testing.T) {
	var proxyHits, targetHits int
	var mu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		proxyHits++
		mu.Unlock()
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetHits++
		mu.Unlock()
		if r.Header.Get("Authorization") == "" {
			t.Error("management authorization missing")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	settings := Settings{CoreOrigin: target.URL, ManagementKey: "memory-only"}
	req, err := NewCoreRequest(context.Background(), settings, http.MethodGet, "/v0/management/config.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := NewLoopbackClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if proxyHits != 0 || targetHits != 1 {
		t.Fatalf("proxy_hits=%d target_hits=%d", proxyHits, targetHits)
	}
}

func TestSettingsStringRepresentationDoesNotExposeSecrets(t *testing.T) {
	setValidSecrets(t)
	settings, err := parseSettings(validConfigYAML())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{os.Getenv("WRITER_MANAGEMENT_KEY"), os.Getenv("WRITER_WORKER_TOKEN")} {
		if secret == "" {
			t.Fatal("test secret missing")
		}
		if contains(settings.String(), secret) {
			t.Fatal("settings string exposed secret")
		}
	}
}

func contains(value, substring string) bool {
	for i := 0; i+len(substring) <= len(value); i++ {
		if value[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}
