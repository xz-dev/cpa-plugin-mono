package plugin

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStatusAndJSONRoutes(t *testing.T) {
	transport := &membershipTransport{t: t}
	service := configuredMembershipService(t, transport)
	response := service.HandleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/auto-pull-models/status"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	response = service.HandleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/auto-pull-models/json"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("json=%d %s", response.StatusCode, response.Body)
	}
}
