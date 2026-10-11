package ingestion

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type handoffConnection struct {
	*refreshConnection
	activity chan string
	stopHook func()
}

func (c *handoffConnection) ConnectContext(ctx context.Context) error {
	if err := c.refreshConnection.ConnectContext(ctx); err != nil {
		return err
	}
	c.activity <- "connect"
	return nil
}
func (c *handoffConnection) Disconnect() {
	c.refreshConnection.Disconnect()
	if c.stopHook != nil {
		c.stopHook()
	}
	c.activity <- "disconnect"
}
func (c *handoffConnection) currentHandler(t *testing.T) whatsmeow.EventHandler {
	t.Helper()
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	if h == nil {
		t.Fatal("owned event handler was not installed")
	}
	return h
}
func waitHandoffActivity(t *testing.T, c *handoffConnection, want string) {
	t.Helper()
	select {
	case got := <-c.activity:
		if got != want {
			t.Fatalf("handoff operation = %q; want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("owned handoff operation did not finish")
	}
}
func noHandoffActivity(t *testing.T, c *handoffConnection) {
	t.Helper()
	select {
	case <-c.activity:
		t.Fatal("pairing reconnected without both current handoff signals")
	case <-time.After(50 * time.Millisecond):
	}
}
func unlinkedHandoffRuntime(t *testing.T) (*Runtime, *handoffConnection) {
	t.Helper()
	r := testRuntime(fixtureStore())
	r.client.Store.ID = nil
	c := &handoffConnection{refreshConnection: &refreshConnection{fakeConnection: &fakeConnection{}}, activity: make(chan string, 16)}
	r = newRuntime(r.config, r.store, Callbacks{}, r.client, c)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Pair(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitHandoffActivity(t, c, "connect")
	return r, c
}
func acceptHandoffPairing(t *testing.T, r *Runtime, c *handoffConnection) {
	t.Helper()
	jid := types.NewJID("100", types.DefaultUserServer)
	r.client.Store.ID = &jid
	c.mu.Lock()
	qr := c.channels[len(c.channels)-1]
	c.mu.Unlock()
	qr <- whatsmeow.QRChannelItem{Event: "success"}
	waitQRWorker(t, r)
}

func TestPairingHandoffWaitsForSuccessAnd515(t *testing.T) {
	for _, reconnectFirst := range []bool{false, true} {
		r, c := unlinkedHandoffRuntime(t)
		h := c.currentHandler(t)
		if reconnectFirst {
			h(&events.ManualLoginReconnect{})
			h(&events.ManualLoginReconnect{})
		} else {
			acceptHandoffPairing(t, r, c)
		}
		noHandoffActivity(t, c)
		if reconnectFirst {
			acceptHandoffPairing(t, r, c)
		} else {
			h(&events.ManualLoginReconnect{})
			h(&events.ManualLoginReconnect{})
		}
		waitHandoffActivity(t, c, "disconnect")
		waitHandoffActivity(t, c, "connect")
		c.currentHandler(t)(&events.ManualLoginReconnect{})
		noHandoffActivity(t, c)
		c.mu.Lock()
		count, disconnects := len(c.contexts), c.disconnects
		oldCanceled, currentAlive := c.contexts[0].Err() == context.Canceled, c.contexts[1].Err() == nil
		c.mu.Unlock()
		if count != 2 || disconnects != 1 || !oldCanceled || !currentAlive {
			t.Fatal("handoff was not exactly one joined stop followed by an owned reconnect")
		}
		if err := r.Close(); err != nil || !r.Quiesced() {
			t.Fatal("handoff cleanup did not join owned workers")
		}
	}
	// An explicit stop retires both its QR signal and its captured SDK handler.
	r, c := unlinkedHandoffRuntime(t)
	oldHandler := c.currentHandler(t)
	acceptHandoffPairing(t, r, c)
	noHandoffActivity(t, c)
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitHandoffActivity(t, c, "disconnect")
	oldHandler(&events.ManualLoginReconnect{})
	noHandoffActivity(t, c)
	r.client.Store.ID = nil
	if err := r.Pair(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitHandoffActivity(t, c, "connect")
	acceptHandoffPairing(t, r, c)
	oldHandler(&events.ManualLoginReconnect{})
	noHandoffActivity(t, c)
	c.currentHandler(t)(&events.ManualLoginReconnect{})
	waitHandoffActivity(t, c, "disconnect")
	waitHandoffActivity(t, c, "connect")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Canceling the owned run before the second signal retires the handoff.
	r, c = unlinkedHandoffRuntime(t)
	h := c.currentHandler(t)
	acceptHandoffPairing(t, r, c)
	noHandoffActivity(t, c)
	r.cancel()
	h(&events.ManualLoginReconnect{})
	noHandoffActivity(t, c)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Model an already-admitted terminal event finishing during the owner join.
	// Its fixed diagnostic must be resampled after stop, before any reconnect.
	r, c = unlinkedHandoffRuntime(t)
	acceptHandoffPairing(t, r, c)
	noHandoffActivity(t, c)
	c.stopHook = func() { r.connectionDiagnostic("event", "connect_failure", nil, 400, false) }
	c.currentHandler(t)(&events.ManualLoginReconnect{})
	waitHandoffActivity(t, c, "disconnect")
	noHandoffActivity(t, c)
	if d := r.Status().ConnectionDiagnostic; d.Event != "connect_failure" || d.Reason != 400 {
		t.Fatal("owner join discarded the admitted terminal cause")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// A logout must keep the deleted session refused instead of handing it off.
	r, c = unlinkedHandoffRuntime(t)
	h = c.currentHandler(t)
	acceptHandoffPairing(t, r, c)
	noHandoffActivity(t, c)
	h(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	h(&events.ManualLoginReconnect{})
	noHandoffActivity(t, c)
	if r.Status().HasSession {
		t.Fatal("logged-out pairing retained a usable session")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Saved-session Connect never enters a registration handoff.
	r = testRuntime(fixtureStore())
	c = &handoffConnection{refreshConnection: &refreshConnection{fakeConnection: &fakeConnection{}}, activity: make(chan string, 16)}
	r.connection = c
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitHandoffActivity(t, c, "connect")
	c.currentHandler(t)(&events.ManualLoginReconnect{})
	noHandoffActivity(t, c)
}
