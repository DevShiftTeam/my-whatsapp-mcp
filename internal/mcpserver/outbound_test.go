package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sendBackend struct {
	mu      sync.Mutex
	calls   int
	grant   archive.CredentialGrant
	request outbound.Request
}

func (b *sendBackend) Send(_ context.Context, g archive.CredentialGrant, r outbound.Request) (outbound.Receipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.grant = g
	b.request = r
	return outbound.Receipt{RequestID: r.RequestID, ChatID: r.ChatID, MessageID: "sent-1", State: "accepted"}, nil
}
func (b *sendBackend) snapshot() (int, archive.CredentialGrant, outbound.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.grant, b.request
}
func (b *sendBackend) callCount() int { n, _, _ := b.snapshot(); return n }
func (b *sendBackend) SendStatus(_ context.Context, _ archive.CredentialGrant, id string) (outbound.Receipt, error) {
	return outbound.Receipt{RequestID: id, State: "outcome_unknown"}, nil
}
func TestOutboundMCPDiscoveryAndAuthenticatedJSONAPI(t *testing.T) {
	b := &sendBackend{}
	server, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), func(c *Config) { c.Outbound = b })
	status, body := rpc(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if status != 200 || !strings.Contains(body, "send_message") || !strings.Contains(body, "send_voice_note") || !strings.Contains(body, "get_send_status") {
		t.Fatal("outbound tools missing")
	}
	request, _ := http.NewRequest("POST", server.URL+"/api/send/message", strings.NewReader(`{"chat_id":"chat","request_id":"r1","text":"שלום"}`))
	request.Header.Set("Authorization", "Bearer "+fakeToken())
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got outbound.Receipt
	decodeErr := json.NewDecoder(response.Body).Decode(&got)
	calls, grant, sent := b.snapshot()
	if decodeErr != nil || response.StatusCode != 200 || got.State != "accepted" || calls != 1 || grant.CredentialID == "" || sent.Text != "שלום" {
		t.Fatal("authenticated JSON send failed")
	}
}
func TestOutboundRejectsInvalidBeforeProvider(t *testing.T) {
	b := &sendBackend{}
	s, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), func(c *Config) { c.Outbound = b })
	for _, args := range []string{`{"chat_id":"chat","request_id":"r","text":""}`, `{"chat_id":"chat","request_id":"r","text":"hello","account":"other"}`, `{"chat_id":"chat","request_id":"r","text":"hello","text":"changed"}`} {
		status, body := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":`+args+`}}`, nil)
		if status != 200 || !strings.Contains(body, `"isError":true`) || b.callCount() != 0 {
			t.Fatalf("invalid reached provider: %s", body)
		}
	}
}

func TestSendAdapterAllowsBase64SizedArgumentsOnlyForSending(t *testing.T) {
	raw := json.RawMessage(`{"data_base64":"` + strings.Repeat("A", int(defaultRequestBytes)) + `"}`)
	if validAdapterArgumentsFor("search_messages", raw) || !validAdapterArgumentsFor("send_media", raw) {
		t.Fatal("adapter request limits not specific to send tools")
	}
}

type delayedSendBackend struct{ sendBackend }

func (b *delayedSendBackend) Send(ctx context.Context, g archive.CredentialGrant, r outbound.Request) (outbound.Receipt, error) {
	select {
	case <-time.After(3200 * time.Millisecond):
		return b.sendBackend.Send(ctx, g, r)
	case <-ctx.Done():
		return outbound.Receipt{}, ctx.Err()
	}
}
func TestStdioSendingSurvivesSlowProviderResponseHeaders(t *testing.T) {
	token := fakeToken()
	credentials := credentialsFixture()
	credentials.grant.Scope.Principal = archive.NativeClientPrincipalPrefix + "v1/" + strings.Repeat("0", 32)
	cfg := stdioNativeConfig(credentials)
	backend := &delayedSendBackend{}
	cfg.Outbound = backend
	h, err := NewHandler(cfg, &fakeReader{}, credentials)
	if err != nil {
		t.Fatal(err)
	}
	daemon := newStdioUnixDaemon(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- runStdioTransport(ctx, daemon.adapterConfig(token), serverTransport) }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "send-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"chat_id": "chat", "request_id": "slow", "text": "hello"}})
	if err != nil || result.IsError || backend.callCount() != 1 {
		t.Fatalf("slow sending failed: %#v %v", result, err)
	}
	_ = session.Close()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("adapter did not stop")
	}
}

func TestRunStdioNativeBase64MediaAbove64KiB(t *testing.T) {
	root, dir := privateTestRoot(t)
	store := new(fileCredentialStore)
	f := credentialsFixture()
	f.grant.Scope.Principal = archive.NativeClientPrincipalPrefix + "v1/" + strings.Repeat("0", 32)
	type publishedHandler struct{ handler http.Handler }
	var published atomic.Pointer[publishedHandler]
	daemon := newStdioUnixDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler := published.Load()
		if handler == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.handler.ServeHTTP(w, r)
	}))
	created, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{
		Label: "synthetic native outbound", Principal: f.grant.Scope.Principal,
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
	backend := &sendBackend{}
	cfg := stdioNativeConfig(f)
	cfg.Outbound = backend
	handler, err := NewHandler(cfg, &fakeReader{}, f)
	if err != nil {
		t.Fatal(err)
	}
	published.Store(&publishedHandler{handler: handler})
	command := exec.Command(os.Args[0], "-test.run=^TestMCPStdioChild$")
	command.Env = append(os.Environ(), "WA_INDEXER_SYNTHETIC_STDIO_CHILD=1", "WA_INDEXER_SYNTHETIC_CREDENTIAL_FILE="+file)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "native-media-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("native initialization failed: %v", err)
	}
	defer session.Close()
	payload := bytes.Repeat([]byte{0x31}, 80*1024)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_media", Arguments: map[string]any{
		"chat_id": "chat", "request_id": "native-large", "kind": "document", "filename": "sample.bin",
		"mime_type": "application/octet-stream", "data_base64": base64.StdEncoding.EncodeToString(payload),
	}})
	if err != nil || result.IsError {
		t.Fatalf("native base64 media failed: %#v %v", result, err)
	}
	calls, _, sent := backend.snapshot()
	if calls != 1 || sent.DataBase64 != base64.StdEncoding.EncodeToString(payload) {
		t.Fatal("native adapter changed or dropped media")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatal("native adapter emitted diagnostic output")
	}
}
