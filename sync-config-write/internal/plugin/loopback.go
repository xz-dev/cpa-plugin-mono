package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func NewLoopbackClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("redirects are disabled")
		},
	}
}

func NewCoreRequest(ctx context.Context, settings Settings, method, fixedPath string, body []byte) (*http.Request, error) {
	if !strings.HasPrefix(fixedPath, "/") || strings.Contains(fixedPath, "://") {
		return nil, fmt.Errorf("fixed loopback path required")
	}
	origin, err := url.Parse(settings.CoreOrigin)
	if err != nil {
		return nil, fmt.Errorf("invalid configured origin")
	}
	relative, err := url.Parse(fixedPath)
	if err != nil || relative.IsAbs() || relative.Host != "" {
		return nil, fmt.Errorf("fixed loopback path required")
	}
	target := origin.ResolveReference(relative)
	if target.Scheme != origin.Scheme || target.Host != origin.Host {
		return nil, fmt.Errorf("request escaped configured origin")
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create loopback request")
	}
	req.Header.Set("Authorization", "Bearer "+settings.ManagementKey)
	return req, nil
}
