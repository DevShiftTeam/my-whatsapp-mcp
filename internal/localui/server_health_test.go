package localui

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProcessHealthRequiresExactAuthorityWithoutOwnerSession(t *testing.T) {
	owner := newTestServer(t, &fakeApplication{})
	for _, test := range []struct {
		method, host, origin string
		status               int
	}{
		{"GET", owner.host, "", 200}, {"GET", owner.host, owner.origin, 200},
		{"POST", owner.host, owner.origin, 405}, {"HEAD", owner.host, "", 405},
		{"GET", "localhost:9000", "", 403}, {"GET", owner.host, "http://127.0.0.1:9999", 403},
	} {
		request := httptest.NewRequest(test.method, owner.origin+"/healthz", nil)
		request.Host = test.host
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		reply := httptest.NewRecorder()
		owner.Handler().ServeHTTP(reply, request)
		if reply.Code != test.status {
			t.Fatalf("health method/authority status: got %d want %d", reply.Code, test.status)
		}
		if test.status == 200 && (reply.Body.String() != "ready\n" || reply.Header().Get("Cache-Control") != "no-store") {
			t.Fatal("health exposed application data or cached readiness")
		}
	}
}

func TestProcessHealthStopsClaimingReadyDuringShutdown(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := New(Config{Origin: "http://" + listener.Addr().String()}, &fakeApplication{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- owner.Serve(ctx, listener) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown did not join")
	}
	request := httptest.NewRequest(http.MethodGet, owner.origin+"/healthz", nil)
	reply := httptest.NewRecorder()
	owner.Handler().ServeHTTP(reply, request)
	if reply.Code != 503 || reply.Body.String() == "ready\n" {
		t.Fatal("shutdown still claimed readiness")
	}
}

func TestProcessHealthDeniesReadinessAsSoonAsServeContextIsCanceled(t *testing.T) {
	owner := newTestServer(t, &fakeApplication{})
	ctx, cancel := context.WithCancel(context.Background())
	owner.mu.Lock()
	owner.serveContext = ctx
	owner.mu.Unlock()
	cancel()
	request := httptest.NewRequest(http.MethodGet, owner.origin+"/healthz", nil)
	reply := httptest.NewRecorder()
	owner.Handler().ServeHTTP(reply, request)
	if reply.Code != 503 || reply.Body.String() == "ready\n" {
		t.Fatal("canceled process claimed readiness before shutdown worker quiesced")
	}
}
