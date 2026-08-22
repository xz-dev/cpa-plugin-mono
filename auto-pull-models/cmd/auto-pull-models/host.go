package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xz-dev/cpa-plugin-mono/auto-pull-models/internal/plugin"
)

type hostHTTPRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

type hostHTTPResponse struct {
	StatusCode    int                 `json:"status_code"`
	Headers       map[string][]string `json:"headers"`
	Body          []byte              `json:"body"`
	StatusCodeAlt int                 `json:"StatusCode"`
	HeadersAlt    map[string][]string `json:"Headers"`
	BodyAlt       []byte              `json:"Body"`
}

type hostTransport struct{}

func (hostTransport) Do(method, url string, headers http.Header, body []byte) (int, []byte, error) {
	raw, err := doHostHTTP(hostHTTPRequest{
		Method:  method,
		URL:     url,
		Headers: headers,
		Body:    body,
	})
	if err != nil {
		return 0, nil, err
	}
	var resp hostHTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, nil, fmt.Errorf("decode host HTTP response: %w", err)
	}
	status := resp.StatusCode
	if status == 0 {
		status = resp.StatusCodeAlt
	}
	out := resp.Body
	if len(out) == 0 {
		out = resp.BodyAlt
	}
	return status, out, nil
}

var _ plugin.Transport = hostTransport{}
