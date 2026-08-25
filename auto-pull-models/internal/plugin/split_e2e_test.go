package plugin

import (
	"bytes"
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

func TestSplitE2EMembershipReconcileAndStaleRevision(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("SPLIT_E2E_URL"), "/")
	if baseURL == "" {
		t.Skip("SPLIT_E2E_URL is not set; run PLAN/tests/split-plugins-e2e.sh")
	}
	service := New(e2eHTTPTransport{client: http.DefaultClient})
	cfg, err := parseFileConfig([]byte(`{"interval":"0","management_base_url":"` + baseURL + `","keep_existing_aliases":true,"channels":[{"enabled":true,"selector":{"name":"Demo","base_url":"https://upstream.example/v1"},"mode":"exclude","patterns":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	service.cfg = cfg
	channels, err := service.listModelChannels("management-key")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := matchOpenAIChannel(channels, cfg.Channels[0].Selector)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Sync("management-key", "")
	if !report.OK || len(report.Channels) != 1 || report.Channels[0].Desired != 2 {
		t.Fatalf("report=%+v", report)
	}
	if err := service.reconcileMembership("management-key", stale, []string{"keep", "new"}, true); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("stale revision error=%v", err)
	}
}
