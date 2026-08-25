package plugin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type membershipTransport struct {
	t        *testing.T
	requests []struct {
		method string
		url    string
		body   []byte
	}
}

func (transport *membershipTransport) Do(method, url string, headers http.Header, body []byte) (int, []byte, error) {
	copyBody := append([]byte(nil), body...)
	transport.requests = append(transport.requests, struct {
		method string
		url    string
		body   []byte
	}{method, url, copyBody})
	if headers.Get("Authorization") != "Bearer management-key" {
		transport.t.Fatalf("authorization=%q", headers.Get("Authorization"))
	}
	switch {
	case method == http.MethodGet && strings.HasSuffix(url, "/v0/management/model-channels"):
		return http.StatusOK, []byte(`{"channels":[
			{"kind":"openai-compatibility","selector":{"name":"ZCode","base_url":"http://zcode-proxy:8080/v1/"},"disabled":false,"ready":true,"revision":"r1","models":[{"name":"glm-old","alias":"old"},{"name":"glm-5.3","alias":"prod"}]},
			{"kind":"claude","selector":{"base_url":"https://api.anthropic.com"},"revision":"secret-free"}
		]}`), nil
	case method == http.MethodPost && strings.HasSuffix(url, "/v0/management/model-channels/catalog"):
		var request catalogRequest
		if err := json.Unmarshal(body, &request); err != nil {
			transport.t.Fatal(err)
		}
		if request.Kind != "openai-compatibility" || request.ExpectedRevision != "r1" || request.Profile != "openai_models" || request.Query == nil || request.Query.ClientVersion != "1.0.0" {
			transport.t.Fatalf("catalog request=%+v", request)
		}
		response, _ := json.Marshal(catalogResponse{StatusCode: http.StatusOK, Body: json.RawMessage(`{"models":[{"slug":"glm-5.3"},{"slug":"embed-1"},{"slug":"glm-5.4"}]}`)})
		return http.StatusOK, response, nil
	case method == http.MethodPost && strings.HasSuffix(url, "/v0/management/model-channels/reconcile-membership"):
		return http.StatusOK, []byte(`{"status":"ok"}`), nil
	default:
		transport.t.Fatalf("unexpected request: %s %s", method, url)
		return 0, nil, nil
	}
}

func configuredMembershipService(t *testing.T, transport Transport) *Service {
	service := New(transport)
	service.jsonPath = filepath.Join(t.TempDir(), "config.json")
	cfg, err := parseFileConfig([]byte(`{
		"interval":"0","management_base_url":"http://cpa:8317","keep_existing_aliases":true,
		"channels":[{"enabled":true,"selector":{"name":"ZCode","base_url":"http://zcode-proxy:8080/v1"},"mode":"exclude","patterns":["^embed-"],"codex_manifest":true}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	service.cfg = cfg
	return service
}

func TestCatalogBodyAcceptsStringAndRawJSON(t *testing.T) {
	encoded, _ := json.Marshal(`{"data":[{"id":"string"}]}`)
	for name, raw := range map[string]json.RawMessage{
		"string": encoded,
		"raw":    json.RawMessage(`{"data":[{"id":"raw"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			entries, err := parseUpstreamCatalog(catalogBody(raw))
			if err != nil || len(entries) != 1 || entries[0].ID != name {
				t.Fatalf("entries=%+v err=%v", entries, err)
			}
		})
	}
}

func TestMembershipSyncUsesSanitizedContractAndFiltering(t *testing.T) {
	transport := &membershipTransport{t: t}
	service := configuredMembershipService(t, transport)
	report := service.Sync("management-key", "")
	if !report.OK || len(report.Channels) != 1 || report.Channels[0].Kept != 2 || report.Channels[0].Dropped != 1 {
		t.Fatalf("report=%+v", report)
	}
	if len(transport.requests) != 3 {
		t.Fatalf("requests=%d", len(transport.requests))
	}
	var request membershipRequest
	if err := json.Unmarshal(transport.requests[2].body, &request); err != nil {
		t.Fatal(err)
	}
	if request.Kind != "openai-compatibility" || request.ExpectedRevision != "r1" || strings.Join(request.ExpectedModelNames, ",") != "glm-old,glm-5.3" || strings.Join(request.DesiredModelNames, ",") != "glm-5.3,glm-5.4" || !request.KeepExistingAliases {
		t.Fatalf("membership request=%+v", request)
	}
	raw := string(transport.requests[2].body)
	for _, forbidden := range []string{"api-key", "auth-index", "headers", "proxy-url", "metadata", "desired_upstream_ids", "models\":"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("membership request contains forbidden field %q: %s", forbidden, raw)
		}
	}
}

func TestPreviewAndManagementRoutesDoNotWrite(t *testing.T) {
	transport := &membershipTransport{t: t}
	service := configuredMembershipService(t, transport)
	response := service.HandleManagement(pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: "/v0/management/plugins/auto-pull-models/preview",
		Headers: http.Header{"Authorization": []string{"Bearer management-key"}},
	})
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"dry_run":true`) {
		t.Fatalf("response=%d %s", response.StatusCode, response.Body)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("preview wrote membership: requests=%d", len(transport.requests))
	}
	routes := service.ManagementRoutes()
	serialized, _ := json.Marshal(routes)
	if strings.Contains(string(serialized), "metadata-sources") || !strings.Contains(string(serialized), "/channels") {
		t.Fatalf("routes=%s", serialized)
	}
}

func TestNormalizeBaseURLMatchesCoreCanonicalization(t *testing.T) {
	for _, testCase := range []struct {
		raw, want string
	}{
		{" HTTPS://Example.COM:443/v1/?tenant=A ", "https://example.com/v1?tenant=A"},
		{"http://Example.COM:80/", "http://example.com"},
		{"https://[2001:DB8::1]:443/v1/", "https://[2001:db8::1]/v1"},
		{"http://[2001:DB8::1]:8080/v1", "http://[2001:db8::1]:8080/v1"},
	} {
		got, err := normalizeBaseURL(testCase.raw)
		if err != nil || got != testCase.want {
			t.Fatalf("normalizeBaseURL(%q)=%q, %v; want %q", testCase.raw, got, err, testCase.want)
		}
	}
	for _, invalid := range []string{"https://user:pass@example.com", "https://example.com/#fragment", "ftp://example.com"} {
		if _, err := normalizeBaseURL(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestDefaultConfigureRemainsPaused(t *testing.T) {
	transport := &membershipTransport{t: t}
	service := New(transport)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := service.Configure([]byte(`{"config_file":"` + path + `"}`)); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown()
	time.Sleep(20 * time.Millisecond)
	if service.Current().Interval != 0 || len(transport.requests) != 0 {
		t.Fatalf("interval=%v requests=%d", service.Current().Interval, len(transport.requests))
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"interval": "0"`) {
		t.Fatalf("config=%s err=%v", raw, err)
	}
}

func TestCommittedExampleInstallsPaused(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseFileConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 0 || cfg.Raw.Interval != "0" {
		t.Fatalf("example must install paused: interval=%v raw=%q", cfg.Interval, cfg.Raw.Interval)
	}
}

func TestConfigRequiresCompositeSelectorsAndRejectsCombinedSchema(t *testing.T) {
	for name, raw := range map[string]string{
		"name only":     `{"channels":[{"selector":{"name":"ZCode"}}]}`,
		"base only":     `{"channels":[{"selector":{"base_url":"https://example.com/v1"}}]}`,
		"duplicate":     `{"channels":[{"selector":{"name":"ZCode","base_url":"https://example.com/v1"}},{"selector":{"name":"ZCode","base_url":"https://EXAMPLE.com/v1/"}}]}`,
		"old providers": `{"providers":{"ZCode":{"enabled":true,"upstream_meta":true}}}`,
		"metadata":      `{"channels":[{"selector":{"name":"ZCode","base_url":"https://example.com/v1"},"upstream_meta":true}]}`,
		"write mode":    `{"channels":[],"write_mode":"file"}`,
		"config path":   `{"channels":[],"config_path":"/tmp/config.yaml"}`,
		"modelparams":   `{"channels":[],"modelparams_url":"https://example.com/models.json"}`,
		"modelsdev":     `{"channels":[],"modelsdev_url":"https://example.com/api.json"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFileConfig([]byte(raw)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestDecodeModelChannelsRejectsSecretBearingLegacyShape(t *testing.T) {
	legacy := []byte(`{"openai-compatibility":[{"name":"ZCode","api-key-entries":[{"api-key":"secret"}]}]}`)
	if _, err := decodeModelChannels(legacy); err == nil {
		t.Fatal("legacy secret-bearing DTO accepted")
	}
}

func TestMissingRevisionFailsBeforeCatalogOrWrite(t *testing.T) {
	service := configuredMembershipService(t, &membershipTransport{t: t})
	channel := ModelChannelDescriptor{Kind: "openai-compatibility", Selector: service.cfg.Channels[0].Selector}
	if _, err := service.fetchOpenAICatalog("management-key", channel, false); err == nil {
		t.Fatal("catalog accepted empty revision")
	}
	if err := service.reconcileMembership("management-key", channel, []string{"m"}, true); err == nil {
		t.Fatal("membership accepted empty revision")
	}
}

func TestSelectorDriftAndAmbiguityFailClosed(t *testing.T) {
	selector, err := normalizeOpenAISelector(ChannelSelector{Name: "ZCode", BaseURL: "https://example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	channels := []ModelChannelDescriptor{
		{Kind: "openai-compatibility", Selector: ChannelSelector{Name: "ZCode", BaseURL: "https://example.com/v2"}},
	}
	if _, err := matchOpenAIChannel(channels, selector); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("drift err=%v", err)
	}
	channels = []ModelChannelDescriptor{
		{Kind: "openai-compatibility", Selector: selector},
		{Kind: "openai-compatibility", Selector: selector},
	}
	if _, err := matchOpenAIChannel(channels, selector); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity err=%v", err)
	}
}
