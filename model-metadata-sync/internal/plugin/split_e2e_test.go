package plugin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type e2eHTTPTransport struct{ client *http.Client }

func (transport e2eHTTPTransport) Do(method, url string, headers http.Header, body []byte) (int, []byte, error) {
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header = headers.Clone()
	response, err := transport.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	return response.StatusCode, raw, err
}

func TestSplitE2EMetadataBeforeMembership(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("SPLIT_E2E_URL"), "/")
	if baseURL == "" {
		t.Skip("SPLIT_E2E_URL is not set; run PLAN/tests/split-plugins-e2e.sh")
	}
	service := New(e2eHTTPTransport{client: http.DefaultClient})
	cfg, err := parseFileConfig([]byte(`{"interval":"0","management_base_url":"` + baseURL + `","channels":[{"enabled":true,"kind":"openai-compatibility","selector":{"name":"Demo","base_url":"https://upstream.example/v1"},"upstream_meta":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	service.cfg = cfg
	report := service.Sync("management-key", "")
	if !report.OK || len(report.Channels) != 1 || report.Channels[0].Patches > 1 {
		t.Fatalf("report=%+v", report)
	}
	status, raw, err := service.mgmtJSON("management-key", http.MethodGet, "/v0/management/model-channels", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("inventory status=%d err=%v", status, err)
	}
	channels, err := decodeModelChannels(raw)
	if err != nil {
		t.Fatal(err)
	}
	expectedSecond := "remove"
	if os.Getenv("SPLIT_E2E_AFTER_MEMBERSHIP") == "1" {
		expectedSecond = "new"
	}
	if len(channels) != 1 || len(channels[0].Models) != 2 || channels[0].Models[0].Name != "keep" || channels[0].Models[0].Alias != "custom" || channels[0].Models[0].MaxOutputTokens != 64000 || channels[0].Models[1].Name != expectedSecond {
		encoded, _ := json.Marshal(channels)
		t.Fatalf("post-membership inventory=%s", encoded)
	}
}
