package ingestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type refreshConnection struct {
	*fakeConnection
	channels      []chan whatsmeow.QRChannelItem
	contexts      []context.Context
	disconnects   int
	authenticated bool
	loginHook     func() bool
}

func (c *refreshConnection) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	qr := make(chan whatsmeow.QRChannelItem, 4)
	c.channels = append(c.channels, qr)
	return qr, nil
}
func (c *refreshConnection) ConnectContext(ctx context.Context) error {
	if err := c.fakeConnection.ConnectContext(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	c.contexts = append(c.contexts, ctx)
	c.mu.Unlock()
	return nil
}
func (c *refreshConnection) Disconnect() {
	c.mu.Lock()
	c.disconnects++
	c.mu.Unlock()
}
func (c *refreshConnection) IsLoggedIn() bool {
	c.mu.Lock()
	hook, authenticated := c.loginHook, c.authenticated
	c.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return authenticated
}

func unlinkedRefreshRuntime(t *testing.T) (*Runtime, *refreshConnection, chan QRNotification) {
	t.Helper()
	r := testRuntime(fixtureStore())
	r.client.Store.ID = nil
	c := &refreshConnection{fakeConnection: &fakeConnection{}}
	notifications := make(chan QRNotification, 8)
	r = newRuntime(r.config, r.store, Callbacks{QRChanged: func(n QRNotification) { notifications <- n }}, r.client, c)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Pair(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r, c, notifications
}

func waitQRWorker(t *testing.T, r *Runtime) {
	t.Helper()
	select {
	case <-r.qrDone:
	case <-time.After(time.Second):
		t.Fatal("owned QR worker did not finish")
	}
}

func waitRefreshConnection(t *testing.T, r *Runtime, state archive.ConnectionState) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for r.Status().Connection != state {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("owned connector did not consume the synthetic connection event")
		}
	}
}

func TestQRExpiredAttemptCanRefresh(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		r, c, _ := unlinkedRefreshRuntime(t)
		oldContext := c.contexts[0]
		if terminal {
			c.channels[0] <- whatsmeow.QRChannelItem{Event: "timeout"}
		}
		close(c.channels[0]) // Also models SDK terminal close without a final item.
		waitQRWorker(t, r)
		if r.Status().Connection != "disconnected" {
			t.Fatal("expired unlinked QR still claimed a live pairing attempt")
		}
		if err := r.Pair(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(c.channels) != 2 || len(c.contexts) != 2 || c.disconnects != 1 || oldContext.Err() != context.Canceled || c.contexts[1].Err() != nil {
			t.Fatal("explicit refresh did not stop/join the old attempt before creating a fresh QR connection")
		}
		if r.Status().ReconciliationRequired {
			t.Fatal("ordinary QR expiry quarantined the archive")
		}
		if err := r.Close(); err != nil || !r.Quiesced() {
			t.Fatal("refresh cleanup did not join the owned workers")
		}
	}
}

func TestQRLateExpiryPreservesLinkedSession(t *testing.T) {
	for _, storedID := range []bool{true, false} {
		for _, afterEnd := range []bool{false, true} {
			r, c, _ := unlinkedRefreshRuntime(t)
			link := func() {
				if storedID {
					jid := types.NewJID("100", types.DefaultUserServer)
					r.client.Store.ID = &jid
				} else {
					c.authenticated = true
				}
			}
			if !afterEnd {
				link()
			}
			c.channels[0] <- whatsmeow.QRChannelItem{Event: "timeout"}
			close(c.channels[0])
			waitQRWorker(t, r)
			if afterEnd {
				link()
			}
			start := r.Pair
			if storedID {
				start = r.Connect // Actual application route, before Pair can restore state.
			}
			if err := start(context.Background()); err != nil {
				t.Fatal(err)
			}
			// The application selects Connect as soon as the stored ID appears.
			if err := r.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.channels) != 1 || len(c.contexts) != 1 || c.disconnects != 0 || c.contexts[0].Err() != nil {
				t.Fatal("late QR expiry refreshed or canceled a linked/authenticated session")
			}
			for _, start := range []func(context.Context) error{r.Pair, r.Connect} {
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				if err := start(canceled); !errors.Is(err, context.Canceled) {
					t.Fatal("late-session no-op bypassed caller cancellation")
				}
				r.mu.Lock()
				r.status.ReconciliationRequired = true
				r.mu.Unlock()
				err := start(context.Background())
				r.mu.Lock()
				r.status.ReconciliationRequired = false
				r.mu.Unlock()
				if !errors.Is(err, archive.ErrReconciliationRequired) || len(c.contexts) != 1 || c.disconnects != 0 || c.contexts[0].Err() != nil {
					t.Fatal("late-session no-op bypassed unsafe admission or tore down its socket")
				}
			}
			if storedID {
				r.deliver(&events.Connected{})
				waitRefreshConnection(t, r, "connected")
				r.deliver(&events.Disconnected{})
				waitRefreshConnection(t, r, "disconnected")
				oldContext := c.contexts[0]
				if err := r.Connect(context.Background()); err != nil || len(c.contexts) != 2 || c.disconnects != 1 || oldContext.Err() != context.Canceled {
					t.Fatal("old QR expiry suppressed a later explicit saved-session reconnect")
				}
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Inject accepted Connected between the watcher's live check and transition.
	r, c, _ := unlinkedRefreshRuntime(t)
	healthy := ConnectionDiagnostic{Phase: "connected", Event: "connected", ErrorClass: "none", SocketStateKnown: true, SocketConnected: true, SocketAuthenticated: true}
	c.loginHook = func() bool {
		r.mu.Lock()
		r.status.Connection, r.status.HasSession, r.qrEnded = "connected", true, false
		r.status.ConnectionDiagnostic = healthy
		r.mu.Unlock()
		return false
	}
	c.channels[0] <- whatsmeow.QRChannelItem{Event: "timeout"}
	close(c.channels[0])
	waitQRWorker(t, r)
	if got := r.Status(); got.Connection != "connected" || got.ConnectionDiagnostic != healthy {
		t.Fatal("late QR end overwrote an accepted connected state or diagnostic")
	}
}

func TestQRNonterminalErrorKeepsCodeDelivery(t *testing.T) {
	r, c, notifications := unlinkedRefreshRuntime(t)
	c.channels[0] <- whatsmeow.QRChannelItem{Event: "error"}
	select {
	case n := <-notifications:
		if n.Event != "error" {
			t.Fatal("nonterminal warning was not reported")
		}
	case <-time.After(time.Second):
		t.Fatal("nonterminal warning was lost")
	}
	c.channels[0] <- whatsmeow.QRChannelItem{Event: "code", Code: "SYNTHETIC_QR", Timeout: time.Minute}
	select {
	case n := <-notifications:
		if n.Event != "code" || n.Code != "SYNTHETIC_QR" || r.Status().Connection != "pairing" {
			t.Fatal("nonterminal warning prevented the next valid linking code")
		}
	case <-time.After(time.Second):
		t.Fatal("QR watcher stopped on a nonterminal SDK error")
	}
	if err := r.Close(); err != nil || !r.Quiesced() {
		t.Fatal("nonterminal QR worker cleanup failed")
	}
}
