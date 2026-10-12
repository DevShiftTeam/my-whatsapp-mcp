package main

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type blockedServeProvider struct {
	opened  chan *blockedServeSession
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}
type blockedServeSession struct {
	owner    *blockedServeProvider
	listener net.Listener
	done     chan error
}

func (p *blockedServeProvider) Open(context.Context, remoteaccess.Intent, remoteaccess.Secret, func(remoteaccess.Notice)) (remoteaccess.Session, error) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	s := &blockedServeSession{owner: p, listener: l, done: make(chan error)}
	p.opened <- s
	return s, nil
}
func (s *blockedServeSession) Origin() string         { return "https://archive.example.com" }
func (s *blockedServeSession) Listener() net.Listener { return s.listener }
func (s *blockedServeSession) Done() <-chan error     { return s.done }
func (s *blockedServeSession) Close(context.Context) error {
	s.listener.Close()
	s.owner.once.Do(func() { close(s.owner.entered) })
	<-s.owner.release
	return nil
}
func TestServeDeadlineRetainsRealLeaseUntilProviderJoins(t *testing.T) {
	root := t.TempDir()
	paths, e := appdata.Prepare(root)
	if e != nil {
		t.Fatal(e)
	}
	a, e := app.New(context.Background(), app.Config{DataDir: root, Demo: true})
	if e != nil {
		t.Fatal(e)
	}
	provider := &blockedServeProvider{opened: make(chan *blockedServeSession, 1), release: make(chan struct{}), entered: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(provider.release)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if e := drainServeApplication(ctx, a); e != nil {
			t.Error("fixture cleanup did not join")
		}
	}()
	_, e = a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: provider}, nil)
	if e != nil {
		t.Fatal(e)
	}
	e = a.ConfigureRemoteAccess(context.Background(), localui.RemoteAccessSubmission{Intent: remoteaccess.Intent{Version: 1, Provider: remoteaccess.Ngrok, Enabled: true, SavedOrigin: "https://archive.example.com"}, Secret: []byte("synthetic-only")})
	if e != nil {
		t.Fatal(e)
	}
	var session *blockedServeSession
	select {
	case session = <-provider.opened:
	case <-time.After(time.Second):
		t.Fatal("provider not opened")
	}
	request, _ := http.NewRequest("GET", "http://"+session.listener.Addr().String()+"/mcp", nil)
	request.Host = "archive.example.com"
	client := &http.Client{Timeout: time.Second}
	response, e := client.Do(request)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	worker := startServeCleanup(ctx, a)
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("provider cleanup not entered")
	}
	if e = waitServeCleanup(ctx, worker); !errors.Is(e, errServeShutdown) || a.CleanupComplete() {
		t.Fatal("unfinished provider reported complete")
	}
	if lease, e := appdata.Acquire(paths); !errors.Is(e, appdata.ErrInstanceAlreadyRunning) {
		if lease != nil {
			lease.Close()
		}
		t.Fatal("deadline released live archive lease", e)
	}
	close(provider.release)
	released = true
	select {
	case <-worker:
	case <-time.After(time.Second):
		t.Fatal("retained cleanup worker not joined")
	}
	final, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if e = drainServeApplication(final, a); e != nil || !a.CleanupComplete() {
		t.Fatal("joined provider cleanup failed", e)
	}
	lease, e := appdata.Acquire(paths)
	if e != nil {
		t.Fatal("joined cleanup retained lease", e)
	}
	lease.Close()
}
