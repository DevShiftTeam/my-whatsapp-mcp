package localui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These lifecycle regressions are source-authored during the execution hold.
// They require the root's serialized validation lane before they may be run.
func TestDeleteLocalDataBurnsAttachAndEveryPendingBrowserBootstrap(t *testing.T) {
	owner, peer := attachedTestServer(t, false)
	privateURL := owner.AttachURL()
	session, pending := lifecycleOwnerAndPendingBootstraps(t, owner, privateURL)
	app, ok := owner.app.(*fakeApplication)
	if !ok {
		t.Fatal("lifecycle fixture has no observable application")
	}

	deletion := lifecycleOwnerRequest(owner, http.MethodPost, "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`, session)
	if deletion.Code != http.StatusOK || app.count("delete") != 1 {
		t.Fatalf("confirmed deletion did not finish once (status %d)", deletion.Code)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(deletion.Body.Bytes(), &result) != nil || !result.OK {
		t.Fatal("confirmed deletion has no success acknowledgement")
	}

	lifecycleAssertCapabilitiesBurned(t, owner, peer, privateURL, session, pending)
	if app.total() != 1 {
		t.Fatal("revoked credentials reached an application operation")
	}
}

func TestServeCancellationBurnsAttachAndEveryPendingBrowserBootstrap(t *testing.T) {
	owner, peer := attachedTestServer(t, false)
	privateURL := owner.AttachURL()
	session, pending := lifecycleOwnerAndPendingBootstraps(t, owner, privateURL)

	// A channel-backed listener exposes the same literal loopback address as
	// the healthy peer. It deterministically enters Accept and records Close.
	// The peer keeps serving this owner's Handler after Serve stops, so a failed
	// Attach below proves capability burn rather than a refused connection.
	listener := &lifecycleChannelListener{address: peer.Listener.Addr(), accepting: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		served <- owner.Serve(ctx, listener)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			t.Error("owner Serve did not finish during bounded cleanup")
		}
	})

	ready := time.NewTimer(5 * time.Second)
	select {
	case <-listener.accepting:
	case <-served:
		ready.Stop()
		t.Fatal("owner Serve exited before entering Accept")
	case <-ready.C:
		t.Fatal("owner Serve did not enter Accept before its deadline")
	}
	ready.Stop()
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", session).Code != http.StatusOK {
		t.Fatal("owner session was unavailable before cancellation")
	}

	cancel()
	stopped := time.NewTimer(5 * time.Second)
	defer stopped.Stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal("owner Serve did not shut down successfully")
		}
	case <-stopped.C:
		t.Fatal("owner Serve did not shut down before its deadline")
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("owner Serve returned without closing its listener")
	}
	lifecycleAssertCapabilitiesBurned(t, owner, peer, privateURL, session, pending)
	if app, ok := owner.app.(*fakeApplication); !ok || app.total() != 0 {
		t.Fatal("shutdown credential probes reached an application operation")
	}
}

// Establish a working attached owner, then leave the original startup bootstrap
// and two independently minted reopen bootstraps unconsumed. Only public
// authentication responses are observed; no flags or capability maps are read.
func lifecycleOwnerAndPendingBootstraps(t *testing.T, owner *Server, privateURL string) (testSession, []string) {
	t.Helper()
	original := lifecycleBootstrapToken(t, owner.BootstrapURL())
	ownerToken := lifecycleMintAttachedBootstrap(t, privateURL)
	if ownerToken == original {
		t.Fatal("fresh attach reused the original pending startup bootstrap")
	}
	authenticated := consumeAttachedBootstrap(owner, ownerToken)
	if authenticated.Code != http.StatusOK {
		t.Fatal("attached owner bootstrap was not usable before lifecycle action")
	}
	cookies := authenticated.Result().Cookies()
	var reply struct {
		CSRF string `json:"csrf_token"`
	}
	origin, _ := url.Parse(owner.origin)
	if len(cookies) != 1 || cookies[0].Name != "owner_session_"+origin.Port() || cookies[0].Value == "" || json.Unmarshal(authenticated.Body.Bytes(), &reply) != nil || reply.CSRF == "" {
		t.Fatal("attached owner response has no usable credentials")
	}
	session := testSession{cookie: cookies[0], csrf: reply.CSRF}
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", session).Code != http.StatusOK {
		t.Fatal("attached owner session was never active")
	}
	first := lifecycleMintAttachedBootstrap(t, privateURL)
	second := lifecycleMintAttachedBootstrap(t, privateURL)
	if first == second || first == ownerToken || second == ownerToken || first == original || second == original {
		t.Fatal("reopening did not mint independent fresh bootstrap capabilities")
	}
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", session).Code != http.StatusOK {
		t.Fatal("minting pending bootstraps unexpectedly replaced the current owner")
	}
	return session, []string{original, first, second}
}

func lifecycleMintAttachedBootstrap(t *testing.T, privateURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bootstrap, err := Attach(ctx, privateURL, false)
	if err != nil {
		t.Fatal("healthy owner could not mint a fresh reopen bootstrap")
	}
	return lifecycleBootstrapToken(t, bootstrap)
}

func lifecycleBootstrapToken(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal("owner bootstrap URL is invalid")
	}
	fields, err := url.ParseQuery(parsed.Fragment)
	if err != nil || len(fields) != 1 || fields.Get("bootstrap") == "" {
		t.Fatal("owner bootstrap URL has no browser capability")
	}
	return fields.Get("bootstrap")
}

func lifecycleOwnerRequest(owner *Server, method, path, body string, session testSession) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, owner.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", owner.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(session.cookie)
	r.Header.Set("X-CSRF-Token", session.csrf)
	reply := httptest.NewRecorder()
	owner.Handler().ServeHTTP(reply, r)
	return reply
}

func lifecycleAssertCapabilitiesBurned(t *testing.T, owner *Server, peer *httptest.Server, privateURL string, session testSession, pending []string) {
	t.Helper()
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", session).Code != http.StatusUnauthorized {
		t.Fatal("lifecycle action left the previous owner session active")
	}
	for _, token := range pending {
		reply := consumeAttachedBootstrap(owner, token)
		if reply.Code != http.StatusUnauthorized || len(reply.Result().Cookies()) != 0 {
			t.Fatal("lifecycle action left a pending browser bootstrap usable")
		}
	}

	// Sign a fresh, unexpired nonce with the captured pre-action capability, not
	// a cached request whose expiry or replay guard could explain rejection.
	captured := &Server{origin: owner.origin, attachURL: privateURL}
	nonce := make([]byte, 32)
	nonce[0] = 71
	proof := attachTestRequest(t, captured, false, base64.RawURLEncoding.EncodeToString(nonce), strconv.FormatInt(time.Now().Unix()+5, 10))
	denial := attachTestHTTP(owner, proof)
	if denial.Code != http.StatusUnauthorized || len(denial.Result().Cookies()) != 0 {
		t.Fatal("lifecycle action left the captured persistent attach capability usable")
	}

	// An unauthenticated shell request must still succeed at the same live
	// endpoint before helper denial can count as authentication evidence.
	healthy, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	check, err := http.NewRequestWithContext(healthy, http.MethodGet, peer.URL+"/", nil)
	if err != nil {
		cancel()
		t.Fatal("healthy peer probe could not be constructed")
	}
	response, err := peer.Client().Do(check)
	if err != nil {
		cancel()
		t.Fatal("owner peer stopped before capability denial could be checked")
	}
	_ = response.Body.Close()
	cancel()
	if response.StatusCode != http.StatusOK {
		t.Fatal("owner peer did not remain healthy after lifecycle action")
	}

	reopen, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	browserURL, err := Attach(reopen, privateURL, false)
	if err != ErrOwnerAttach || browserURL != "" {
		t.Fatal("reopen helper returned browser authority after lifecycle action")
	}
}

type lifecycleChannelListener struct {
	address               net.Addr
	accepting, closed     chan struct{}
	acceptOnce, closeOnce sync.Once
}

func (listener *lifecycleChannelListener) Addr() net.Addr { return listener.address }
func (listener *lifecycleChannelListener) Accept() (net.Conn, error) {
	listener.acceptOnce.Do(func() { close(listener.accepting) })
	<-listener.closed
	return nil, net.ErrClosed
}
func (listener *lifecycleChannelListener) Close() error {
	listener.closeOnce.Do(func() { close(listener.closed) })
	return nil
}
