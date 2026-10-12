package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteDispatchDefaultOffResetAndPasswordFirst(t *testing.T) {
	a := unixNativeTestApp(t)
	ctx := context.Background()
	ad := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 4)}
	old, e := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: ad}, nil)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-ad.sessions:
		t.Fatal("disabled provider opened")
	default:
	}
	v := localui.RemoteAccessSubmission{Intent: appRemoteIntent(true), Secret: []byte("synthetic-token")}
	if e = a.ConfigureRemoteAccess(ctx, v); !errors.Is(e, localui.ErrRemotePasswordRequired) {
		t.Fatal("browser saved before password", e)
	}
	if old.Status().SavedOrigin != "" {
		t.Fatal("rejected save mutated identity")
	}
	v.Intent.ExposeBrowser = false
	if e = a.ConfigureRemoteAccess(ctx, v); e != nil {
		t.Fatal(e)
	}
	select {
	case s := <-ad.sessions:
		appRemoteRequest(t, s, "/mcp")
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not open")
	}
	if e = a.DisableRemoteAccess(ctx); e != nil {
		t.Fatal(e)
	}
	snap, e := a.RemoteAccessStatus(ctx)
	if e != nil || snap.SavedOrigin != "https://mcp.example.com" || snap.State != "disabled" {
		t.Fatal("disabled identity lost", e)
	}
	if e = a.DeleteLocalData(ctx, true); e != nil {
		t.Fatal(e)
	}
	snap, e = a.RemoteAccessStatus(ctx)
	if e != nil || snap.SavedOrigin != "" || snap.PasswordConfigured {
		t.Fatal("reset status", e)
	}
	if a.remoteLifecycle.manager == old {
		t.Fatal("retained stale manager")
	}
	if e = a.ConfigureRemoteAccess(ctx, v); e != nil {
		t.Fatal(e)
	}
	select {
	case s := <-ad.sessions:
		appRemoteRequest(t, s, "/mcp")
	case <-time.After(3 * time.Second):
		t.Fatal("reset provider did not open")
	}
	if e = a.RemoveRemoteAccess(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestRemoteDispatchManualDeniesManaged(t *testing.T) {
	a := remoteConnectionTestApp(t)
	ctx := context.Background()
	s, e := a.RemoteAccessStatus(ctx)
	if e != nil || !s.Manual {
		t.Fatal("manual status", e)
	}
	if _, e = a.NewDefaultRemoteAccessManager(nil); e == nil {
		t.Fatal("manual started manager")
	}
	for _, e := range []error{a.ConfigureRemoteAccess(ctx, localui.RemoteAccessSubmission{Intent: appRemoteIntent(false), Secret: []byte("token")}), a.DisableRemoteAccess(ctx), a.RemoveRemoteAccess(ctx)} {
		if !errors.Is(e, localui.ErrRemoteManual) {
			t.Fatal("manual mutation", e)
		}
	}
}

func TestRemoteDispatchReleasesAdmissionBeforeSynchronousPrepare(t *testing.T) {
	a := unixNativeTestApp(t)
	ctx := context.Background()
	entered := make(chan struct{}, 1)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 2)}
	_, e := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, func(context.Context, remoteaccess.Intent, string, uint64) (http.Handler, error) {
		entered <- struct{}{}
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil
	})
	if e != nil {
		t.Fatal(e)
	}
	// Keep the dispatch call on its stack until Prepare runs. Holding app
	// admission around the action deterministically turns this into needs_setup.
	e = a.withRemoteManager(ctx, func(m *remoteaccess.Manager) error {
		if e := m.Configure(ctx, appRemoteIntent(true), []byte("synthetic")); e != nil {
			return e
		}
		select {
		case <-entered:
			return nil
		case <-time.After(time.Second):
			return errors.New("prepare was blocked by its own dispatch")
		}
	})
	if e != nil {
		t.Fatal(e)
	}
	s := <-adapter.sessions
	appRemoteRequest(t, s, "/mcp")
}
func TestRemoteEnabledRestartAndStaleDispatchAfterReset(t *testing.T) {
	a := unixNativeTestApp(t)
	ctx := context.Background()
	paths := a.paths
	root := paths.Root
	// Persist enabled intent before manager construction. A synchronous provider
	// can Open immediately; constructor ready must protect Prepare installation.
	if e := remoteaccess.NewStore(paths, a.lease).Save(ctx, appRemoteIntent(false), []byte("synthetic")); e != nil {
		t.Fatal(e)
	}
	if e := a.Close(ctx); e != nil {
		t.Fatal(e)
	}
	b, e := New(ctx, Config{DataDir: root, Demo: true, Offline: true})
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close(ctx)
	b.mcpEnabled = true
	if e = b.startMCP(ctx); e != nil {
		t.Fatal(e)
	}
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 3)}
	old, e := b.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, nil)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case s := <-adapter.sessions:
		appRemoteRequest(t, s, "/mcp")
	case <-time.After(time.Second):
		t.Fatal("enabled restart did not open")
	}
	if old.Status().State != "online" {
		t.Fatal("constructor raced Prepare", old.Status().State)
	}
	if e = b.DeleteLocalData(ctx, true); e != nil {
		t.Fatal(e)
	}
	if e = old.Configure(ctx, appRemoteIntent(false), []byte("STALE_SECRET")); !errors.Is(e, remoteaccess.ErrClosed) {
		t.Fatal("old manager revived", e)
	}
	v := localui.RemoteAccessSubmission{Intent: appRemoteIntent(false), Secret: []byte("fresh")}
	if e = b.ConfigureRemoteAccess(ctx, v); e != nil {
		t.Fatal(e)
	}
	select {
	case s := <-adapter.sessions:
		appRemoteRequest(t, s, "/mcp")
	case <-time.After(time.Second):
		t.Fatal("reset reconstruction did not open")
	}
	if b.remoteLifecycle.manager == old {
		t.Fatal("reused stale manager")
	}
	if e = old.Remove(ctx); !errors.Is(e, remoteaccess.ErrClosed) {
		t.Fatal("old removal touched new state", e)
	}
	intent, secret, e := remoteaccess.NewStore(b.paths, b.lease).Load(ctx)
	defer clear(secret)
	if e != nil || intent.SavedOrigin != "https://mcp.example.com" || string(secret) != "fresh" {
		t.Fatal("stale manager mutated fresh store", e)
	}
}

type remoteFailureAdapter struct {
	failure error
	pending bool
	opened  chan struct{}
	joined  chan struct{}
	calls   atomic.Int32
}

func (a *remoteFailureAdapter) Open(ctx context.Context, _ remoteaccess.Intent, _ remoteaccess.Secret, notice func(remoteaccess.Notice)) (remoteaccess.Session, error) {
	a.calls.Add(1)
	select {
	case a.opened <- struct{}{}:
	default:
	}
	if a.pending {
		notice(remoteaccess.Notice{State: "needs_setup", AuthURL: "https://login.tailscale.com/a/synthetic-secret"})
		<-ctx.Done()
		close(a.joined)
	}
	return nil, a.failure
}
func TestRemoteOutageStatesAndSigninCancellationKeepLocalSearch(t *testing.T) {
	for _, failure := range []error{remoteaccess.ErrPermissionRequired, remoteaccess.ErrCredentialsRejected, remoteaccess.ErrServiceLimit, errors.New("SYNTHETIC_SECRET /private/archive")} {
		t.Run(failure.Error(), func(t *testing.T) {
			a := unixNativeTestApp(t)
			ctx := context.Background()
			ad := &remoteFailureAdapter{failure: failure, opened: make(chan struct{}, 1)}
			m, e := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: ad}, nil)
			if e != nil {
				t.Fatal(e)
			}
			if e = a.ConfigureRemoteAccess(ctx, localui.RemoteAccessSubmission{Intent: appRemoteIntent(false), Secret: []byte("synthetic")}); e != nil {
				t.Fatal(e)
			}
			<-ad.opened
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for m.Status().State == "connecting" {
				select {
				case <-deadline.C:
					t.Fatal("failure not observed")
				case <-tick.C:
				}
			}
			snap, e := a.RemoteAccessStatus(ctx)
			if e != nil || strings.Contains(snap.Code, "SYNTHETIC") || snap.SavedOrigin != "https://mcp.example.com" {
				t.Fatal("unsafe outage status", e)
			}
			if _, e = a.Search(ctx, archive.SearchRequest{Query: "פגישה", Limit: 1}); e != nil {
				t.Fatal("outage stopped local search", e)
			}
			if e = a.DisableRemoteAccess(ctx); e != nil {
				t.Fatal(e)
			}
			count := ad.calls.Load()
			if m.Status().State != "disabled" || ad.calls.Load() != count {
				t.Fatal("disabled worker revived")
			}
		})
	}
	a := unixNativeTestApp(t)
	ctx := context.Background()
	ad := &remoteFailureAdapter{pending: true, failure: remoteaccess.ErrTransient, opened: make(chan struct{}, 1), joined: make(chan struct{})}
	m, e := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.TailscalePrivate: ad}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.ConfigureRemoteAccess(ctx, localui.RemoteAccessSubmission{Intent: remoteaccess.Intent{Version: 1, Provider: remoteaccess.TailscalePrivate, Enabled: true, NodeName: "saved-node"}}); e != nil {
		t.Fatal(e)
	}
	<-ad.opened
	if e = a.DisableRemoteAccess(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ad.joined:
	default:
		t.Fatal("signin worker not joined")
	}
	if m.OwnerNotice().AuthURL != "" {
		t.Fatal("signin capability retained")
	}
	if e = a.RemoveRemoteAccess(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestRemoteDefaultConstructorDoesNotStartSDKOrCreateProviderState(t *testing.T) {
	a := unixNativeTestApp(t)
	if _, e := a.NewDefaultRemoteAccessManager(nil); e != nil {
		t.Fatal(e)
	}
	if entries, e := os.ReadDir(a.paths.RemoteDir); e != nil || len(entries) != 0 {
		t.Fatal("disabled construction created provider state", e)
	}
	if a.ownerAuth.auth != nil {
		t.Fatal("disabled construction initialized auth")
	}
}

func TestManagedRemoteDoesNotDeclareClaudeAccountCapability(t *testing.T) {
	a := unixNativeTestApp(t)
	ctx := context.Background()
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	if _, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureRemoteAccess(ctx, localui.RemoteAccessSubmission{Intent: appRemoteIntent(false), Secret: []byte("synthetic")}); err != nil {
		t.Fatal(err)
	}
	select {
	case session := <-adapter.sessions:
		appRemoteRequest(t, session, "/mcp")
	case <-time.After(time.Second):
		t.Fatal("managed provider did not prepare")
	}
	expiry := time.Now().Add(time.Hour)
	if _, err := a.CreateConnection(ctx, localui.ConnectionRequest{Client: "claude_connectors", Label: "unconfirmed account", Mode: "remote", AllChats: true, ExpiresAt: &expiry}); !errors.Is(err, localui.ErrConnectionUnsupported) || len(a.authState.RemoteClients) != 0 {
		t.Fatal("managed transport asserted account capability or issued a grant", err)
	}
	for _, client := range []string{"codex_cli", "claude_code"} {
		connection, err := a.CreateConnection(ctx, localui.ConnectionRequest{Client: client, Label: "supported remote", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
		if err != nil {
			t.Fatal(client, err)
		}
		if _, err = a.ConnectionGuide(ctx, localui.ConnectionGuideRequest{ID: connection.ID, Client: client, Mode: "remote"}); err != nil {
			t.Fatal(client, err)
		}
	}
	// A previously admitted connector record must not turn managed transport into
	// a new account declaration when its guide is requested later.
	a.ops.Lock()
	a.data.Lock()
	_, err := a.issueRemoteClient(ctx, archive.CredentialCreate{Label: "previous connector", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: expiry}, "claude_connectors")
	var id string
	if err == nil {
		id = a.authState.RemoteClients[len(a.authState.RemoteClients)-1].IntentID
	}
	a.data.Unlock()
	a.ops.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ConnectionGuide(ctx, localui.ConnectionGuideRequest{ID: id, Client: "claude_connectors", Mode: "remote"}); !errors.Is(err, localui.ErrConnectionUnsupported) {
		t.Fatal("managed guide asserted account capability", err)
	}
}

func TestManualOwnerHeaderDeclarationPreservesEligibleConnectorPath(t *testing.T) {
	a := remoteConnectionTestApp(t)
	a.mcpConfig.Remote.HeaderAuthAvailable = true // Existing explicit owner launch declaration.
	expiry := time.Now().Add(time.Hour)
	connection, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "claude_connectors", Label: "declared eligible", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ConnectionGuide(context.Background(), localui.ConnectionGuideRequest{ID: connection.ID, Client: "claude_connectors", Mode: "remote"}); err != nil {
		t.Fatal(err)
	}
}
