package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// guardedReader captures source contributors in the same read frame as content.
// A later fresh lookup cannot bless stale text that an earlier read returned.
type guardedReader interface {
	archive.GuardedReader
	CheckResponse(context.Context, archive.ReadScope, archive.ResponseCheck) (archive.ReadSnapshot, error)
}

type requestKey struct{}
type pendingCheck struct {
	grant archive.CredentialGrant
	guard archive.ResponseGuard
}
type requestState struct {
	principal principal
	ctx       context.Context
	mu        sync.Mutex
	checks    []pendingCheck
}

func normalizeConfig(c Config) (Config, error) {
	if c.Mode != LocalAdapter && c.Mode != Bearer && c.Mode != Access {
		return c, ErrConfiguration
	}
	if (c.Mode == LocalAdapter && !nativeRegistryConfigured(c.NativeClients)) || len(c.LocalCredentialID) > 128 {
		return c, ErrConfiguration
	}
	if c.Mode == LocalAdapter && (len(c.Hosts) != 1 || c.Hosts[0] != "localhost") {
		return c, ErrConfiguration
	}
	if len(c.Hosts) == 0 || len(c.Hosts) > 16 || len(c.Origins) > 16 {
		return c, ErrConfiguration
	}
	c.Hosts = append([]string(nil), c.Hosts...)
	c.Origins = append([]string(nil), c.Origins...)
	for _, h := range c.Hosts {
		u, err := url.Parse("http://" + h)
		if err != nil || h == "" || u.Host != h || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(h, "/*?#@ \\\t\r\n") || u.Hostname() == "" {
			return c, ErrConfiguration
		}
	}
	for _, o := range c.Origins {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != o {
			return c, ErrConfiguration
		}
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = defaultRequestBytes
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = defaultResponseBytes
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.MaxRequestBytes < 256 || c.MaxRequestBytes > 256<<10 || c.MaxResponseBytes < 1024 || c.MaxResponseBytes > 1<<20 || c.MaxConcurrent < 1 || c.MaxConcurrent > 16 || c.RequestTimeout < time.Millisecond || c.RequestTimeout > 15*time.Second {
		return c, ErrConfiguration
	}
	if c.Mode != Access && (c.Access.Issuer != "" || c.Access.Audience != "" || c.Access.JWKSURL != "" || len(c.Access.Identities) != 0) {
		return c, ErrConfiguration
	}
	return c, nil
}

// NewHandler exposes only /mcp. The caller owns the dedicated loopback listener.
// SDK transport is stateless: a transport session never caches archive authority.
func NewHandler(config Config, reader archive.Reader, credentials Credentials) (http.Handler, error) {
	c, err := normalizeConfig(config)
	if err != nil || reader == nil || credentials == nil {
		return nil, ErrConfiguration
	}
	g, ok := reader.(guardedReader)
	if !ok {
		return nil, ErrConfiguration
	}
	var verifier accessVerifier
	if c.Mode == Access {
		verifier, err = newAccessVerifier(c.Access)
		if err != nil {
			return nil, ErrConfiguration
		}
	}
	slots := make(chan struct{}, c.MaxConcurrent)
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return newToolServer(c, g, credentials)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: c.MaxRequestBytes, PropagateRequestCancellation: true, DisableLocalhostProtection: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/mcp" || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
			fixedHTTP(w, http.StatusNotFound, "mcp_not_found")
			return
		}
		if !contains(c.Hosts, r.Host) || len(r.Header.Values("Origin")) > 1 || (r.Header.Get("Origin") != "" && !contains(c.Origins, r.Header.Get("Origin"))) {
			fixedHTTP(w, http.StatusForbidden, "mcp_forbidden")
			return
		}
		if r.Method != http.MethodPost {
			fixedHTTP(w, http.StatusMethodNotAllowed, "mcp_method_not_allowed")
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			fixedHTTP(w, http.StatusTooManyRequests, "mcp_busy")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), c.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		controller := http.NewResponseController(w)
		deadline, _ := ctx.Deadline()
		_ = controller.SetReadDeadline(deadline)
		// A short fixed grace permits a body-free timeout response after archive
		// work has stopped; it does not extend admission or tool authorization.
		_ = controller.SetWriteDeadline(deadline.Add(250 * time.Millisecond))
		p, err := authenticate(r, c, credentials, verifier)
		if err != nil {
			fixedHTTP(w, http.StatusUnauthorized, "mcp_unauthorized")
			return
		}
		kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || kind != "application/json" {
			fixedHTTP(w, http.StatusUnsupportedMediaType, "mcp_json_required")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, c.MaxRequestBytes+1))
		if err != nil || int64(len(body)) > c.MaxRequestBytes || ctx.Err() != nil {
			fixedHTTP(w, http.StatusRequestEntityTooLarge, "mcp_request_rejected")
			return
		}
		// Legacy SDK protocol versions accept batches and execute their members
		// concurrently. Admission counts one bounded request and one tool call.
		if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
			fixedHTTP(w, http.StatusBadRequest, "mcp_request_rejected")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		state := &requestState{principal: p, ctx: ctx}
		r = r.WithContext(context.WithValue(ctx, requestKey{}, state))
		buffer := &boundedResponse{header: make(http.Header), max: c.MaxResponseBytes}
		sdk.ServeHTTP(buffer, r)
		if buffer.overflow {
			fixedHTTP(w, http.StatusInternalServerError, "mcp_response_too_large")
			return
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			fixedHTTP(w, http.StatusGatewayTimeout, "mcp_timeout")
			return
		}
		if _, err = p.resolve(ctx, credentials); err != nil {
			fixedHTTP(w, http.StatusUnauthorized, "mcp_unauthorized")
			return
		}
		payload := scrubProtocolError(buffer.body.Bytes())
		state.mu.Lock()
		checks := append([]pendingCheck(nil), state.checks...)
		state.mu.Unlock()
		for _, check := range checks {
			if err = credentials.CheckCredentialGrant(ctx, check.grant); err != nil {
				fixedHTTP(w, http.StatusUnauthorized, "mcp_unauthorized")
				return
			}
			// This shared source/credential commit fence is the final read before
			// flushing bytes. No potentially blocking credential lookup follows it.
			if _, err = g.CheckResponse(ctx, check.grant.Scope, archive.ResponseCheck{Guard: check.guard, Credential: &check.grant}); err != nil {
				fixedHTTP(w, http.StatusConflict, "mcp_response_invalidated")
				return
			}
		}
		for name, values := range buffer.header {
			if name == "Access-Control-Allow-Origin" || name == "Access-Control-Allow-Credentials" || name == "Cache-Control" || name == "X-Content-Type-Options" || name == "Content-Length" {
				continue
			}
			w.Header()[name] = values
		}
		status := buffer.status
		if status == 0 {
			status = http.StatusOK
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			fixedHTTP(w, http.StatusGatewayTimeout, "mcp_timeout")
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	}), nil
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func fixedHTTP(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`"}`)
}

type boundedResponse struct {
	header      http.Header
	body        bytes.Buffer
	max, status int
	overflow    bool
}

func (b *boundedResponse) Header() http.Header { return b.header }
func (b *boundedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *boundedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.overflow || b.body.Len()+len(p) > b.max {
		b.overflow = true
		return 0, ErrResponseTooLarge
	}
	return b.body.Write(p)
}
func (b *boundedResponse) Flush() {}

// SDK error data can contain caller arguments. Keep protocol code/id, with a
// fixed diagnostic instead of reflecting those arguments into another client.
func scrubProtocolError(p []byte) []byte {
	if len(p) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(p, &envelope) != nil {
		return []byte(`{"error":"mcp_protocol_error"}`)
	}
	if raw, ok := envelope["error"]; ok {
		var e struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(raw, &e) == nil {
			safe, _ := json.Marshal(struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{e.Code, "mcp_protocol_error"})
			envelope["error"] = safe
			out, err := json.Marshal(envelope)
			if err == nil {
				return out
			}
		}
	}
	return p
}

func quietServerOptions() *mcp.ServerOptions {
	return &mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}
