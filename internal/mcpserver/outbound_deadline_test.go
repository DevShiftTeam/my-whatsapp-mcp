package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
)

type deadlineSendBackend struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *deadlineSendBackend) Send(ctx context.Context, _ archive.CredentialGrant, r outbound.Request) (outbound.Receipt, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return outbound.Receipt{RequestID: r.RequestID, ChatID: r.ChatID, State: "accepted"}, nil
	case <-ctx.Done():
		return outbound.Receipt{}, ctx.Err()
	}
}

func (b *deadlineSendBackend) SendStatus(context.Context, archive.CredentialGrant, string) (outbound.Receipt, error) {
	return outbound.Receipt{}, outbound.ErrUnavailable
}
func (b *deadlineSendBackend) CheckSendResponse(context.Context, archive.CredentialGrant, outbound.Receipt) error {
	return nil
}

func TestOutboundResponseDeadlineOverridesShortListenerWriteTimeout(t *testing.T) {
	b := &deadlineSendBackend{started: make(chan struct{}), release: make(chan struct{})}
	h, err := NewHandler(Config{Mode: Bearer, Hosts: []string{"mcp.example.com"}, Outbound: b, RequestTimeout: time.Second}, &fakeReader{}, credentialsFixture())
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(h)
	s.Config.WriteTimeout = 20 * time.Millisecond
	s.Start()
	defer s.Close()
	var release sync.Once
	defer release.Do(func() { close(b.release) })
	r, err := http.NewRequest(http.MethodPost, s.URL+"/api/send/message", strings.NewReader(`{"chat_id":"chat","request_id":"slow","text":"synthetic"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "mcp.example.com"
	r.Header.Set("Authorization", "Bearer "+fakeToken())
	r.Header.Set("Content-Type", "application/json")
	client := s.Client()
	client.Timeout = 2 * time.Second
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() { response, err := client.Do(r); done <- result{response, err} }()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("synthetic outbound work was not admitted")
	}
	// Exceed the listener's deadline while remaining inside the handler budget.
	time.Sleep(80 * time.Millisecond)
	release.Do(func() { close(b.release) })
	got := <-done
	if got.err != nil {
		t.Fatal("listener deadline prevented bounded outbound response")
	}
	defer got.response.Body.Close()
	var receipt outbound.Receipt
	if got.response.StatusCode != http.StatusOK || json.NewDecoder(got.response.Body).Decode(&receipt) != nil || receipt.State != "accepted" {
		t.Fatal("bounded outbound response was lost after listener timeout")
	}
}
