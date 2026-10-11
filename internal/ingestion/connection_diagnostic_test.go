package ingestion

import (
	"context"
	"errors"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/socket"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
)

type diagnosticConnection struct {
	*fakeConnection
	err                      error
	connected, authenticated bool
}

func (c *diagnosticConnection) ConnectContext(ctx context.Context) error {
	_ = c.fakeConnection.ConnectContext(ctx)
	return c.err
}
func (c *diagnosticConnection) IsConnected() bool { return c.connected }
func (c *diagnosticConnection) IsLoggedIn() bool  { return c.authenticated }

type opaqueConnectionError struct{}

func (opaqueConnectionError) Error() string { panic("diagnostics must not stringify connector errors") }

func TestConnectionDiagnosticTypedFailureAndEarlyBranches(t *testing.T) {
	for _, item := range []struct {
		err    error
		class  string
		reason int
	}{
		{context.Canceled, "canceled", 0},
		{context.DeadlineExceeded, "deadline", 0},
		{socket.ErrDialFailed, "dial", 0},
		{socket.ErrWithStatusCode{StatusCode: 403}, "http_status", 403},
		{whatsmeow.ErrAlreadyConnected, "already_connected", 0},
		{wmstore.ErrDeviceDeleted, "device_deleted", 0},
		{opaqueConnectionError{}, "unknown", 0},
	} {
		r := testRuntime(fixtureStore())
		t.Cleanup(func() { _ = r.Close() })
		r.connection = &diagnosticConnection{fakeConnection: &fakeConnection{}, err: item.err}
		if err := r.Connect(context.Background()); !errors.Is(err, archive.ErrSourceUnavailable) {
			t.Fatal("startup public error criterion changed")
		}
		d := r.Status().ConnectionDiagnostic
		if d.Phase != "connect" || d.ErrorClass != item.class || d.Reason != item.reason || !r.Quiesced() || r.Status().ReconciliationRequired {
			t.Fatalf("startup diagnostic lost or quarantined: %+v", d)
		}
		_ = r.Close()
	}
	for _, phase := range []string{"runtime_closed", "session_missing", "session_close_failed", "session_deleted"} {
		r := testRuntime(fixtureStore())
		t.Cleanup(func() { _ = r.Close() })
		switch phase {
		case "runtime_closed":
			r.closed = true
		case "session_missing":
			r.client.Store.ID = nil
		case "session_close_failed":
			r.sessionCloseFailed = true
		case "session_deleted":
			r.client.Store.Deleted = true
			// Model an already-deleted device loaded before any SDK workers start.
			r = newRuntime(r.config, r.store, r.callbacks, r.client, &fakeConnection{})
		}
		if err := r.Connect(context.Background()); !errors.Is(err, archive.ErrSourceUnavailable) {
			t.Fatal("early unavailable branch changed")
		}
		class := "source_unavailable"
		if phase == "session_deleted" {
			class = "device_deleted"
		}
		if d := r.Status().ConnectionDiagnostic; d.Phase != phase || d.ErrorClass != class {
			t.Fatalf("early reason was discarded: %+v", d)
		}
		if phase == "session_deleted" && r.Status().HasSession {
			t.Fatal("deleted in-memory device still claimed a usable linked session")
		}
	}
}

func TestConnectionDiagnosticEventsAndSocketFlags(t *testing.T) {
	r := testRuntime(fixtureStore())
	r.connection = &diagnosticConnection{fakeConnection: &fakeConnection{}, connected: true, authenticated: true}
	defer r.Close()
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := r.Status().ConnectionDiagnostic
	if d.Phase != "awaiting_authentication" || !d.SocketStateKnown || !d.SocketConnected || !d.SocketAuthenticated || r.Status().Connection == "connected" {
		t.Fatalf("socket snapshot invented final Connected or was missing: %+v", d)
	}
	// Model the already-live state at the existing observation no-op guard.
	r.mu.Lock()
	r.status.Connection = "connected"
	r.mu.Unlock()
	if err := r.Connect(context.Background()); err != nil || r.Status().ConnectionDiagnostic != d {
		t.Fatal("already-live connect erased its diagnostic without a new attempt")
	}
	for _, item := range []struct {
		event  any
		name   string
		reason int
	}{
		{&events.ManualLoginReconnect{}, "manual_login_reconnect", 0},
		{&events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable, Message: "PRIVATE MUST NOT BE RETAINED"}, "connect_failure", 503},
		{&events.ConnectFailure{Reason: events.ConnectFailureReason(123456), Message: "PRIVATE"}, "connect_failure", 0},
		{&events.ClientOutdated{}, "client_outdated", 0},
		{&events.StreamError{Code: "PRIVATE UNKNOWN"}, "stream_error", 0},
	} {
		current := testRuntime(fixtureStore())
		t.Cleanup(func() { _ = current.Close() })
		if err := current.Connect(context.Background()); err != nil {
			t.Fatal(err)
		}
		current.deliver(item.event)
		d := current.Status().ConnectionDiagnostic
		if d.Event != item.name || d.Reason != item.reason || d.ErrorClass != "none" || current.Status().ReconciliationRequired {
			t.Fatalf("event diagnostic missing or widened: %+v", d)
		}
		if item.name == "stream_error" {
			if err := current.Reconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if next := current.Status().ConnectionDiagnostic; next.Phase != "awaiting_authentication" || next.Event != "none" || next.ErrorClass != "none" || next.Reason != 0 {
				t.Fatalf("explicit fresh attempt retained a stale terminal cause: %+v", next)
			}
		}
		if err := current.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r.deliver(&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut})
	if d := r.Status().ConnectionDiagnostic; d.Event != "logged_out" || d.Reason != 401 || !d.OnConnect {
		t.Fatalf("typed logout witness lost: %+v", d)
	}
	first := r.Status().ConnectionDiagnostic
	r.deliver(&events.Disconnected{})
	if d := r.Status().ConnectionDiagnostic; d != first || r.Status().HasSession {
		t.Fatal("disconnection erased the first logout cause or restored a deleted session")
	}
	if err := r.Connect(context.Background()); !errors.Is(err, archive.ErrSourceUnavailable) || r.Status().ConnectionDiagnostic != first || r.Status().HasSession {
		t.Fatal("explicit connect erased the deleted session's first logout cause")
	}
}

func TestConnectionDiagnosticPostPairFailureSurvivesCleanup(t *testing.T) {
	r := testRuntime(fixtureStore())
	defer r.Close()
	c := &diagnosticConnection{fakeConnection: &fakeConnection{}}
	r.connection = c
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.err = socket.ErrDialFailed
	r.completePairing(r.observation)
	d := r.Status().ConnectionDiagnostic
	if d.Phase != "post_pair_start" || d.ErrorClass != "dial" || !r.Quiesced() || r.Status().ReconciliationRequired {
		t.Fatalf("post-pair first cause was discarded by cleanup: %+v", d)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if after := r.Status().ConnectionDiagnostic; after != d {
		t.Fatal("cleanup overwrote connection failure")
	}
}
