package main

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

var errHealth = errors.New("process_unavailable")

type healthIO struct {
	readOwnerURL func(app.Config) (string, error)
	dial         func(context.Context, string, string) (net.Conn, error)
}

// Health reads only the protected handoff and dials one selected literal
// loopback listener. It never composes an Application, leases data or loads AI.
func health(ctx context.Context, opts options, stdout io.Writer) error {
	return healthWithIO(ctx, opts, stdout, healthIO{readOwnerURL: app.OwnerAttachURL, dial: (&net.Dialer{Timeout: 2 * time.Second}).DialContext})
}

func healthWithIO(ctx context.Context, opts options, stdout io.Writer, operations healthIO) error {
	if !localui.ValidOwnerConnect(opts.connect) {
		return errHealth
	}
	candidate, err := operations.readOwnerURL(app.Config{DataDir: opts.dataDir, Demo: opts.demo})
	if err != nil {
		return errHealth
	}
	endpoint, err := url.Parse(candidate)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.Opaque != "" || endpoint.Path != "/" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || !localui.ValidOwnerConnect(endpoint.Host) {
		return errHealth
	}
	origin := "http://" + endpoint.Host
	// A readiness probe sends neither the private fragment nor an attach proof.
	endpoint.Fragment, endpoint.RawFragment = "", ""
	endpoint.Path = "/healthz"
	operation, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(operation, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errHealth
	}
	request.Header.Set("Origin", origin)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 2 * time.Second, MaxResponseHeaderBytes: 2048,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(operation, 2*time.Second)
			defer cancel()
			return operations.dial(dialCtx, network, opts.connect)
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errHealth }}
	response, err := client.Do(request)
	if err != nil {
		return errHealth
	}
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || err != nil || mediaType != "text/plain" || len(response.Header.Values("Content-Type")) != 1 || response.ContentLength > 64 {
		return errHealth
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65))
	if err != nil || string(body) != "ready\n" {
		return errHealth
	}
	if _, err := io.WriteString(stdout, "ready\n"); err != nil {
		return errHealth
	}
	return nil
}
