package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func stdioNativeConfig(f *fakeCredentials) Config {
	return Config{
		Mode:              LocalAdapter,
		Hosts:             []string{"localhost"},
		LocalCredentialID: f.grant.CredentialID,
		NativeClients:     nativeRegistryFixture(f.grant),
	}
}

type stdioUnixDaemon struct {
	SocketPath  string
	Connections atomic.Int32
}

func (d *stdioUnixDaemon) adapterConfig(token string) AdapterConfig {
	return AdapterConfig{Endpoint: NativeAdapterEndpoint, NativeSocketPath: d.SocketPath, Token: token}
}

func newStdioUnixDaemon(t *testing.T, handler http.Handler) *stdioUnixDaemon {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	} else if runtime.GOOS != "linux" {
		t.Skip("native Unix socket fixture requires a supported platform")
	}
	dir, err := os.MkdirTemp(base, "wa-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(canonical)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	socket, err := appdata.ListenNativeSocket(context.Background(), paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	daemon := &stdioUnixDaemon{SocketPath: socket.Path()}
	if !validNativeSocketPath(daemon.SocketPath) {
		t.Fatal("native fixture path exceeds the supported socket boundary")
	}
	listener := socket.Listener()
	for _, protected := range []struct {
		path string
		mode os.FileMode
	}{{paths.WorkingDir, 0700}, {daemon.SocketPath, 0600}} {
		info, err := os.Lstat(protected.path)
		if err != nil || info.Mode().Perm() != protected.mode {
			t.Fatal("native fixture permissions are not private")
		}
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != "localhost" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			handler.ServeHTTP(w, r)
		}),
		ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: time.Second,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				daemon.Connections.Add(1)
			}
		},
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	return daemon
}

func syntheticAdapterDaemon(t *testing.T, token string, badTools bool) (*stdioUnixDaemon, *atomic.Bool, *atomic.Int32) {
	return syntheticAdapterDaemonWithDenial(t, token, badTools, http.StatusUnauthorized)
}

func syntheticAdapterDaemonWithDenial(t *testing.T, token string, badTools bool, deniedStatus int) (*stdioUnixDaemon, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	var revoked atomic.Bool
	var calls atomic.Int32
	f := credentialsFixture()
	f.digest = sha256.Sum256([]byte(token))
	f.grant.Scope.Principal = archive.NativeClientPrincipalPrefix + "v1/" + strings.Repeat("0", 32)
	config := stdioNativeConfig(f)
	server := mcp.NewServer(&mcp.Implementation{Name: "wa-indexer", Version: "test"}, nil)
	for _, name := range []string{"search_messages", "get_context", "list_chats", "get_index_status"} {
		if badTools && name == "get_index_status" {
			name = "send_message"
		}
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			text := "שָׁלוֹם · 1,250 · https://example.invalid/demo"
			if args["large"] == true {
				text = strings.Repeat("x", defaultResponseBytes+1)
			}
			if args["native_block"] == true {
				text = strings.Repeat("x", defaultResponseBytes-1024)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		})
	}
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})
	httpServer := newStdioUnixDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || revoked.Load() {
			http.Error(w, "mcp_unauthorized", deniedStatus)
			return
		}
		if _, err := authenticate(r, config, f, nil); err != nil {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		sdk.ServeHTTP(w, r)
	}))
	return httpServer, &revoked, &calls
}

func TestStdioAdapterDistinguishesRevocationFromDaemonOutages(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, "mcp_unauthorized"},
		{"forbidden", http.StatusForbidden, "mcp_unavailable"},
		{"missing", http.StatusNotFound, "mcp_unavailable"},
		{"server_error", http.StatusInternalServerError, "mcp_unavailable"},
		{"unavailable", http.StatusServiceUnavailable, "mcp_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			daemon, revoked, calls := syntheticAdapterDaemonWithDenial(t, token, false, test.status)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			done := make(chan error, 1)
			go func() { done <- runStdioTransport(ctx, daemon.adapterConfig(token), serverTransport) }()
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Error("stdio adapter did not join after failed assertion")
					}
				}
			}()
			client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-revocation-client", Version: "test"}, nil)
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_index_status", Arguments: map[string]any{}})
			if err != nil || reply == nil || reply.IsError || calls.Load() != 1 {
				t.Fatal("healthy same-session call did not reach the daemon")
			}
			revoked.Store(true)
			for _, tool := range []string{"search_messages", "get_context", "list_chats", "get_index_status"} {
				reply, err = session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
				if err != nil || reply == nil || !reply.IsError || len(reply.Content) != 1 || reply.StructuredContent != nil {
					t.Fatalf("%s did not return a bounded tool failure", tool)
				}
				text, ok := reply.Content[0].(*mcp.TextContent)
				if !ok || text.Text != test.want || calls.Load() != 1 {
					t.Fatalf("%s denial = %#v, want %q; daemon calls = %d", tool, reply.Content, test.want, calls.Load())
				}
			}
			session.Close()
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Fatalf("stdio adapter ended: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("stdio adapter did not join after session close")
			}
		})
	}
}

type stdioResponseTransport func(*http.Request) (*http.Response, error)

func (f stdioResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type stdioUnreadBody struct{ reads, closes int }

func (b *stdioUnreadBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("synthetic response body must not be read")
}
func (b *stdioUnreadBody) Close() error { b.closes++; return nil }

func TestStdioAdapterHTTP401MarkerRequiresAnActualResponse(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &stdioUnreadBody{}
			response := &http.Response{StatusCode: status, Body: body, ContentLength: 32, Header: make(http.Header)}
			base := &http.Transport{}
			base.RegisterProtocol("http", stdioResponseTransport(func(*http.Request) (*http.Response, error) { return response, nil }))
			wrapper := &adapterHTTPTransport{endpoint: NativeAdapterEndpoint, token: "synthetic-token", transport: base}
			request, err := http.NewRequest(http.MethodPost, NativeAdapterEndpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			reply, err := wrapper.RoundTrip(request)
			if status == http.StatusUnauthorized {
				if reply != nil || err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnauthorized) || body.reads != 0 || body.closes != 1 {
					t.Fatal("actual HTTP 401 did not become a private marker with exactly one unread-body close")
				}
			} else {
				if reply != response || err != nil || body.reads != 0 || body.closes != 0 {
					t.Fatal("non-401 response was misclassified or consumed")
				}
				_ = reply.Body.Close()
			}
		})
	}
	for _, test := range []struct {
		name string
		err  error
	}{
		{"generic", errors.New("synthetic transport failure")},
		{"exported_unauthorized", ErrUnauthorized},
		{"matching_text", errors.New("mcp_adapter_http_unauthorized")},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &http.Transport{}
			base.RegisterProtocol("http", stdioResponseTransport(func(*http.Request) (*http.Response, error) { return nil, test.err }))
			wrapper := &adapterHTTPTransport{endpoint: NativeAdapterEndpoint, token: "synthetic-token", transport: base}
			request, err := http.NewRequest(http.MethodPost, NativeAdapterEndpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			if reply, err := wrapper.RoundTrip(request); reply != nil || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnauthorized) {
				t.Fatal("base transport failure was incorrectly treated as an authorization denial")
			}
		})
	}
}

func TestStdioAdapterSDKRoundTripHebrewRevocationAndLimits(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	daemon, revoked, calls := syntheticAdapterDaemon(t, token, false)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- runStdioTransport(ctx, daemon.adapterConfig(token), serverTransport)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-native-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("bounded tool discovery: %v", err)
	}
	reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search_messages", Arguments: map[string]any{"query": "שלום"}})
	if err != nil || reply.IsError || len(reply.Content) != 1 || reply.Content[0].(*mcp.TextContent).Text != "שָׁלוֹם · 1,250 · https://example.invalid/demo" {
		t.Fatalf("logical Hebrew changed: %#v %v", reply, err)
	}
	before := calls.Load()
	reply, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "search_messages", Arguments: map[string]any{"query": strings.Repeat("x", int(defaultRequestBytes)+1)}})
	if err != nil || !reply.IsError || calls.Load() != before {
		t.Fatal("oversized argument reached daemon")
	}
	reply, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"large": true}})
	if err != nil || !reply.IsError || len(reply.Content) != 1 || len(reply.Content[0].(*mcp.TextContent).Text) > 100 {
		t.Fatal("oversized output reached stdio")
	}
	revoked.Store(true)
	reply, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_index_status", Arguments: map[string]any{}})
	if err != nil || !reply.IsError || calls.Load() != before+1 {
		t.Fatal("existing stdio session bypassed daemon revocation")
	}
	if strings.Contains(reply.Content[0].(*mcp.TextContent).Text, token) {
		t.Fatal("tool error exposed token")
	}
	session.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stdio adapter ended: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stdio adapter did not stop on transport close")
	}
}

func TestStdioAdapterRejectsAuthorityAndUnexpectedTools(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, config := range []AdapterConfig{
		{Endpoint: "http://localhost:9876/mcp", NativeSocketPath: "/private/tmp/fixture/native.sock", Token: token},
		{Endpoint: NativeAdapterEndpoint, NativeSocketPath: "/private/tmp/fixture/native.sock", Token: "raw-secret"},
		{Endpoint: "http://127.0.0.1:9876/mcp?secret", NativeSocketPath: "/private/tmp/fixture/native.sock", Token: token},
		{Endpoint: NativeAdapterEndpoint, Token: token},
	} {
		serverTransport, _ := mcp.NewInMemoryTransports()
		if err := runStdioTransport(context.Background(), config, serverTransport); !errors.Is(err, ErrConfiguration) || strings.Contains(err.Error(), "raw-secret") {
			t.Fatal("invalid adapter configuration was accepted or exposed")
		}
	}
	daemon, _, _ := syntheticAdapterDaemon(t, token, true)
	serverTransport, _ := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runStdioTransport(ctx, daemon.adapterConfig(token), serverTransport); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unexpected send tool was accepted")
	}
}

func TestRunStdioRejectsLegacyTCPBeforeDial(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var connections atomic.Int32
	var requests atomic.Int32
	canary := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	canary.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	canary.Start()
	defer canary.Close()
	for _, config := range []AdapterConfig{
		{Endpoint: canary.URL + "/mcp", Token: token},
		{Endpoint: canary.URL + "/mcp", NativeSocketPath: "/private/tmp/fixture/native.sock", Token: token},
		{Endpoint: NativeAdapterEndpoint, Token: token},
	} {
		if err := RunStdio(context.Background(), config); !errors.Is(err, ErrConfiguration) {
			t.Fatal("legacy or unbound configuration reached native stdio")
		}
		if transport, err := newAdapterHTTPTransport(config); transport != nil || !errors.Is(err, ErrConfiguration) {
			t.Fatal("legacy or unbound configuration constructed a dialer")
		}
	}
	if connections.Load() != 0 || requests.Load() != 0 {
		t.Fatal("legacy configuration reached the TCP canary")
	}
}

func TestNativeStdioRejectsProtectedRemoteBearerFile(t *testing.T) {
	_, dir := privateTestRoot(t)
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	publicURL := "https://mcp.example.invalid/mcp"
	data, err := json.Marshal(map[string]any{
		"version": 1, "mode": "remote_bearer", "credential_id": "remote-only-fixture",
		"public_url": publicURL, "token": token,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "remote-only.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("remote fixture was not a protected regular file")
	}
	parent, err := os.Lstat(dir)
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		t.Fatal("remote fixture parent was not private")
	}
	config, err := ReadCredentialFile(file)
	if !errors.Is(err, ErrCredentialFile) || config != (AdapterConfig{}) {
		t.Fatal("native credential reader accepted a remote bearer capability")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), publicURL) || strings.Contains(err.Error(), file) {
		t.Fatal("native rejection exposed remote capability contents")
	}
	if transport, err := newAdapterHTTPTransport(config); transport != nil || !errors.Is(err, ErrConfiguration) {
		t.Fatal("rejected remote capability constructed a native HTTP transport")
	}
	if err := RunStdio(context.Background(), config); !errors.Is(err, ErrConfiguration) {
		t.Fatal("rejected remote capability reached the native launcher")
	}
}

func TestStdioHTTPResponseLimitDetectsOverflow(t *testing.T) {
	for _, size := range []int{7, 8, 9} {
		body := &boundedAdapterBody{source: io.NopCloser(strings.NewReader(strings.Repeat("x", size))), remaining: 8}
		data, err := io.ReadAll(body)
		body.Close()
		if size <= 8 && (err != nil || len(data) != size) {
			t.Fatal("bounded body rejected allowed data")
		}
		if size > 8 && (!errors.Is(err, ErrResponseTooLarge) || len(data) != 8) {
			t.Fatal("body cap silently truncated overflow")
		}
	}
}

func TestStdioAdapterDoesNotRedirectOrForwardOtherCredentials(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer target.Close()
	redirect := newStdioUnixDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(localAdapterHeader) != token || r.Header.Get("Authorization") != "" || r.Header.Get(accessHeader) != "" {
			t.Error("adapter did not use its exclusive credential header")
		}
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	serverTransport, _ := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runStdioTransport(ctx, redirect.adapterConfig(token), serverTransport); !errors.Is(err, ErrUnavailable) {
		t.Fatal("redirect was accepted")
	}
	if reached.Load() {
		t.Fatal("credential-bearing request followed redirect")
	}
	transport, err := newAdapterHTTPTransport(redirect.adapterConfig(token))
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	wrapper := &adapterHTTPTransport{endpoint: NativeAdapterEndpoint, token: token, transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, NativeAdapterEndpoint, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer unrelated-secret")
	request.Header.Set(accessHeader, "unrelated-assertion")
	response, err := wrapper.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if request.Header.Get("Authorization") != "Bearer unrelated-secret" {
		t.Fatal("wrapper mutated caller headers")
	}
	request.Host = "other.invalid"
	if _, err := wrapper.RoundTrip(request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("wrapper sent credential with another HTTP authority")
	}
	request.Host = "localhost"
	request.URL.Path = "/different"
	if _, err := wrapper.RoundTrip(request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("wrapper sent credential to another endpoint")
	}
}

func TestStdioAdapterChecksSocketOnEveryNewConnection(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var requests atomic.Int32
	daemon := newStdioUnixDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get(localAdapterHeader) != token {
			t.Error("verified native connection did not receive its credential")
		}
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "{}")
	}))
	transport, err := newAdapterHTTPTransport(daemon.adapterConfig(token))
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	wrapper := &adapterHTTPTransport{endpoint: NativeAdapterEndpoint, token: token, transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, target := range []struct{ network, address string }{
		{"tcp", "127.0.0.1:80"}, {"tcp", "localhost:81"}, {"unix", "localhost:80"},
	} {
		if conn, err := transport.DialContext(ctx, target.network, target.address); conn != nil || !errors.Is(err, ErrUnavailable) {
			if conn != nil {
				_ = conn.Close()
			}
			t.Fatal("dialer accepted another network or logical authority")
		}
	}
	call := func() error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, NativeAdapterEndpoint, strings.NewReader("{}"))
		if err != nil {
			return err
		}
		response, err := wrapper.RoundTrip(request)
		if err != nil {
			return err
		}
		_, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			return readErr
		}
		return closeErr
	}
	for range 3 {
		if err := call(); err != nil {
			t.Fatal("verified native reconnect failed")
		}
	}
	if requests.Load() != 3 || daemon.Connections.Load() != 3 {
		t.Fatal("fixture did not exercise three independent native connections")
	}
	if err := os.Chmod(daemon.SocketPath, 0644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := call(); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), daemon.SocketPath) {
			t.Fatal("unsafe native socket was accepted or reflected in an error")
		}
	}
	if requests.Load() != 3 || daemon.Connections.Load() != 3 {
		t.Fatal("new connection reused previous socket trust or dispatched its token")
	}
	if err := os.Chmod(daemon.SocketPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil || requests.Load() != 4 || daemon.Connections.Load() != 4 {
		t.Fatal("repaired native socket was not independently verified")
	}
}

// Darwin's FWASWRITTEN (Apple's sys/fcntl.h) records a successful write and
// cannot be cleared by F_SETFL. Permit only stdout's unset-to-set transition;
// every other status bit, including access mode and O_NONBLOCK, must match.
func nativeFixtureFlagsRestored(platform string, before, after [2]int) bool {
	if before[0] != after[0] {
		return false
	}
	if before[1] == after[1] {
		return true
	}
	const darwinWasWritten = 0x10000
	return platform == "darwin" && after[1] == (before[1]|darwinWasWritten)
}

func TestMCPStdioChild(t *testing.T) {
	if os.Getenv("WA_INDEXER_SYNTHETIC_STDIO_CHILD") != "1" {
		return
	}
	config, err := ReadCredentialFile(os.Getenv("WA_INDEXER_SYNTHETIC_CREDENTIAL_FILE"))
	if err != nil {
		os.Exit(2)
	}
	beforeFlags, err := currentNativeStdioFlags()
	if err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelFile := os.Getenv("WA_INDEXER_SYNTHETIC_CANCEL_FILE"); cancelFile != "" {
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if _, err := os.Stat(cancelFile); err == nil {
						cancel()
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	err = RunStdio(ctx, config)
	afterFlags, flagErr := currentNativeStdioFlags()
	_, inputErr := os.Stdin.Stat()
	_, outputErr := os.Stdout.Stat()
	if flagErr != nil || inputErr != nil || outputErr != nil || !nativeFixtureFlagsRestored(runtime.GOOS, beforeFlags, afterFlags) {
		if cancelFile := os.Getenv("WA_INDEXER_SYNTHETIC_CANCEL_FILE"); cancelFile != "" {
			status, _ := json.Marshal(map[string]any{"before": beforeFlags, "after": afterFlags, "flags_failed": flagErr != nil, "input_failed": inputErr != nil, "output_failed": outputErr != nil})
			_ = os.WriteFile(cancelFile+".failure", status, 0600)
		}
		os.Exit(4)
	}
	if ctx.Err() != nil && os.Getenv("WA_INDEXER_SYNTHETIC_CANCEL_FILE") != "" && (err == nil || errors.Is(err, ErrUnavailable)) {
		os.Exit(0)
	}
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestRunStdioNativeCancellationWithStalledStdout(t *testing.T) {
	t.Run("flags_allow_only_darwin_stdout_write_history", func(t *testing.T) {
		for _, fixture := range []struct {
			name, platform string
			before, after  [2]int
		}{
			{"darwin_unchanged", "darwin", [2]int{0, 1}, [2]int{0, 1}},
			{"darwin_stdout_written", "darwin", [2]int{0, 1}, [2]int{0, 0x10001}},
			{"darwin_already_written", "darwin", [2]int{0, 0x10001}, [2]int{0, 0x10001}},
			{"darwin_original_nonblocking", "darwin", [2]int{4, 5}, [2]int{4, 0x10005}},
			{"linux_unchanged", "linux", [2]int{0, 1}, [2]int{0, 1}},
			{"other_unix_unchanged", "freebsd", [2]int{0, 1}, [2]int{0, 1}},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				if !nativeFixtureFlagsRestored(fixture.platform, fixture.before, fixture.after) {
					t.Fatal("unchanged semantic flags or documented stdout bookkeeping were rejected")
				}
			})
		}
	})
	t.Run("flags_reject_semantic_changes", func(t *testing.T) {
		for _, fixture := range []struct {
			name, platform string
			before, after  [2]int
		}{
			{"stdout_nonblocking_added", "darwin", [2]int{0, 1}, [2]int{0, 5}},
			{"stdout_written_and_nonblocking_added", "darwin", [2]int{0, 1}, [2]int{0, 0x10005}},
			{"stdout_nonblocking_removed", "darwin", [2]int{0, 5}, [2]int{0, 0x10001}},
			{"stdin_nonblocking_added", "darwin", [2]int{0, 1}, [2]int{4, 0x10001}},
			{"stdout_append_added", "darwin", [2]int{0, 1}, [2]int{0, 0x10009}},
			{"stdout_async_added", "darwin", [2]int{0, 1}, [2]int{0, 0x10041}},
			{"stdout_sync_added", "darwin", [2]int{0, 1}, [2]int{0, 0x10081}},
			{"stdout_dsync_added", "darwin", [2]int{0, 1}, [2]int{0, 0x410001}},
			{"stdout_access_readonly", "darwin", [2]int{0, 1}, [2]int{0, 0x10000}},
			{"stdout_access_readwrite", "darwin", [2]int{0, 1}, [2]int{0, 0x10002}},
			{"stdout_unknown_flag_added", "darwin", [2]int{0, 1}, [2]int{0, 0x10010001}},
			{"stdout_write_history_cleared", "darwin", [2]int{0, 0x10001}, [2]int{0, 1}},
			{"stdin_write_history_added", "darwin", [2]int{0, 1}, [2]int{0x10000, 0x10001}},
			{"linux_requires_exact_flags", "linux", [2]int{0, 1}, [2]int{0, 0x10001}},
			{"other_unix_requires_exact_flags", "freebsd", [2]int{0, 1}, [2]int{0, 0x10001}},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				if nativeFixtureFlagsRestored(fixture.platform, fixture.before, fixture.after) {
					t.Fatal("semantic, unknown or unsupported-platform flag change was accepted")
				}
			})
		}
	})
	_, dir := privateTestRoot(t)
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	daemon, _, _ := syntheticAdapterDaemon(t, token, false)
	file := filepath.Join(dir, "blocked.json")
	data, err := json.Marshal(credentialFile{Version: 2, CredentialID: credentialsFixture().grant.CredentialID, Endpoint: NativeAdapterEndpoint, NativeSocketPath: daemon.SocketPath, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cancelFile := filepath.Join(dir, "cancel.signal")
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPStdioChild$")
	command.Env = append(os.Environ(), "WA_INDEXER_SYNTHETIC_STDIO_CHILD=1", "WA_INDEXER_SYNTHETIC_CREDENTIAL_FILE="+file, "WA_INDEXER_SYNTHETIC_CANCEL_FILE="+cancelFile)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"stalled-client","version":"test"}}}` + "\n"
	if _, err := io.WriteString(stdin, initialize); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadBytes('\n')
	if err != nil || !json.Valid(line) {
		t.Fatal("native initialization was not protocol JSON")
	}
	call := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_messages","arguments":{"native_block":true}}}` + "\n"
	if _, err := io.WriteString(stdin, call); err != nil {
		t.Fatal(err)
	}
	// A response larger than the pipe capacity has started. Stop reading while
	// leaving stdin open, then cancel the adapter from inside its own process.
	if _, err := reader.ReadByte(); err != nil {
		t.Fatal("large native reply did not begin")
	}
	if err := os.WriteFile(cancelFile, []byte("cancel"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			status, _ := os.ReadFile(cancelFile + ".failure")
			t.Fatalf("cancelled adapter failed: %v; descriptor checks: %s", err, status)
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		<-waited
		t.Fatal("native adapter stayed blocked on unread stdout after cancellation")
	}
	if stderr.Len() != 0 {
		t.Fatal("cancelled native adapter emitted diagnostics")
	}
}

func TestRunStdioNativeSDKProtocolOnly(t *testing.T) {
	root, dir := privateTestRoot(t)
	store := new(fileCredentialStore)
	// The fixture learns its token from the owner-protected file, not argv.
	f := credentialsFixture()
	f.grant.Scope.Principal = archive.NativeClientPrincipalPrefix + "v1/" + strings.Repeat("0", 32)
	var authConfig atomic.Pointer[Config]
	server := mcp.NewServer(&mcp.Implementation{Name: "wa-indexer", Version: "test"}, nil)
	for _, name := range []string{"search_messages", "get_context", "list_chats", "get_index_status"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "שלום native stdio"}}}, nil
		})
	}
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})
	daemon := newStdioUnixDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		published := authConfig.Load()
		if r.URL.Path != "/mcp" || published == nil {
			w.WriteHeader(401)
			return
		}
		if _, err := authenticate(r, *published, f, nil); err != nil {
			w.WriteHeader(401)
			return
		}
		sdk.ServeHTTP(w, r)
	}))
	created, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{
		Label: "synthetic native stdio", Principal: f.grant.Scope.Principal,
		Policy: archive.CredentialPolicy{Account: f.grant.Scope.Account, AllChats: true},
	}, root, "native.json", NativeEndpoint{SocketPath: daemon.SocketPath})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "native.json")
	config, err := ReadCredentialFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f.digest = sha256.Sum256([]byte(config.Token))
	f.grant.CredentialID = created.ID
	f.grant.CredentialRevision = created.Revision
	published := stdioNativeConfig(f)
	authConfig.Store(&published)
	command := exec.Command(os.Args[0], "-test.run=^TestMCPStdioChild$")
	command.Env = append(os.Environ(), "WA_INDEXER_SYNTHETIC_STDIO_CHILD=1", "WA_INDEXER_SYNTHETIC_CREDENTIAL_FILE="+file)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "native-sdk-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("native stdout did not initialize as protocol: %v", err)
	}
	reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search_messages", Arguments: map[string]any{"query": "שלום"}})
	if err != nil || reply.IsError || reply.Content[0].(*mcp.TextContent).Text != "שלום native stdio" {
		t.Fatalf("native protocol did not preserve logical text: %v", err)
	}
	session.Close()
	if stderr.Len() != 0 {
		t.Fatal("native adapter emitted diagnostic output")
	}
}
