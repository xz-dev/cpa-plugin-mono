package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPPlannerUsesFixedRouteAuthTokenAndExactBase64Snapshot(t *testing.T) {
	base := []byte("x: 1\r\n")
	snapshot := NewConfigSnapshot(base)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != plannerPaths[OperationAutoPull] || r.Header.Get(workerTokenHeader) != "worker" || r.Header.Get("Authorization") != "Bearer mgmt" {
			t.Fatalf("request=%s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		var got ConfigSnapshot
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != snapshot {
			t.Fatalf("got=%+v err=%v", got, err)
		}
		_ = json.NewEncoder(w).Encode(ProposalFromBytes(snapshot.Version, base))
	}))
	defer server.Close()
	proposal, code := NewHTTPPlanner(NewLoopbackClient()).Plan(context.Background(), OperationAutoPull, snapshot, Settings{CoreOrigin: server.URL, ManagementKey: "mgmt", WorkerToken: "worker"})
	if code != "" || proposal.BaseVersion != snapshot.Version {
		t.Fatalf("proposal=%+v code=%s", proposal, code)
	}
}

func TestWorkerStatusesOmitSecretsAndBodies(t *testing.T) {
	status := WorkerStatus{InstanceID: mustOpaqueID(t), ReconfigureSeq: 1, ConfigSHA256: configVersion([]byte("runtime")), ActivePlan: false}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"management", "worker-secret", "root-secret", "config_base64", "Authorization"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("status leaked %q: %s", forbidden, raw)
		}
	}
}

func TestHTTPWorkerStatusClientUsesFixedRouteTokenAndExactTuple(t *testing.T) {
	instance, _ := newOpaqueID()
	want := WorkerStatus{InstanceID: instance, ReconfigureSeq: 7, ConfigSHA256: configVersion([]byte("runtime")), ActivePlan: false}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workerStatusPaths["auto-pull-models"] || r.Method != http.MethodGet {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get(workerTokenHeader) != "token-secret" || r.Header.Get("Authorization") != "Bearer mgmt-secret" {
			t.Fatalf("headers=%v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer server.Close()
	client := NewWorkerStatusClient(NewLoopbackClient())
	got, err := client.Status(context.Background(), "auto-pull-models", Settings{CoreOrigin: server.URL, ManagementKey: "mgmt-secret", WorkerToken: "token-secret"})
	if err != nil || got != want {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestHTTPWorkerStatusClientRejectsExtraOrInvalidFieldsWithoutLeakage(t *testing.T) {
	for name, body := range map[string]string{
		"extra":        `{"instance_id":"AAAAAAAAAAAAAAAAAAAAAA","reconfigure_seq":1,"config_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","active_plan":false,"secret":"leak"}`,
		"bad instance": `{"instance_id":"not-an-id","reconfigure_seq":1,"config_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","active_plan":false}`,
		"bad sequence": `{"instance_id":"AAAAAAAAAAAAAAAAAAAAAA","reconfigure_seq":0,"config_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","active_plan":false}`,
		"bad hash":     `{"instance_id":"AAAAAAAAAAAAAAAAAAAAAA","reconfigure_seq":1,"config_sha256":"UPPER","active_plan":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			_, err := NewWorkerStatusClient(NewLoopbackClient()).Status(context.Background(), "auto-pull-models", Settings{CoreOrigin: server.URL, ManagementKey: "mgmt", WorkerToken: "token"})
			if err == nil || err.Error() == body || err.Error() == "leak" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
