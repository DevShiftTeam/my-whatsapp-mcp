package remoteaccess

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"tailscale.com/atomicfile"
	"tailscale.com/tka"
)

type fakeTS struct {
	mu                    sync.Mutex
	status                tailscaleStatus
	opts                  tailscaleOptions
	started, closed       chan struct{}
	startGate, listenGate <-chan struct{}
	closeOnce             sync.Once
	mode                  Provider
	listenErr             error
	closeFailures         int
}

func (f *fakeTS) Start() error {
	if f.startGate != nil {
		<-f.startGate
	}
	close(f.started)
	return nil
}
func (f *fakeTS) Status(context.Context) (tailscaleStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, nil
}
func (f *fakeTS) Up(context.Context) error { return nil }
func (f *fakeTS) Listen(p Provider) (net.Listener, error) {
	f.mode = p
	if f.listenGate != nil {
		select {
		case <-f.listenGate:
		case <-f.closed:
			return nil, errors.New("secret")
		}
	}
	if f.listenErr != nil {
		return nil, f.listenErr
	}
	return net.Listen("tcp", "127.0.0.1:0")
}
func (f *fakeTS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeFailures > 0 {
		f.closeFailures--
		return errors.New("synthetic close failure")
	}
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}
func runningTS() tailscaleStatus {
	return tailscaleStatus{state: "Running", dns: "archive.tail123.ts.net.", cert: "archive.tail123.ts.net", hasIP: true, hasTailnet: true, funnelAllowed: true, magicDNS: true}
}
func tsFixture(t *testing.T) (*tailscaleAdapter, *fakeTS, *FileStore) {
	t.Helper()
	store, p := testDiskStore(t)
	f := &fakeTS{status: runningTS(), started: make(chan struct{}), closed: make(chan struct{})}
	a := NewTailscaleAdapter(p, store.lease).(*tailscaleAdapter)
	a.newNode = func(o tailscaleOptions) tailscaleNode { f.opts = o; return f }
	a.poll = time.Millisecond
	return a, f, store
}
func tsIntent(p Provider) Intent {
	return Intent{Version: 1, Provider: p, Enabled: true, NodeName: "archive"}
}
func TestTailscaleGoalsAndPersistence(t *testing.T) {
	for _, p := range []Provider{TailscalePrivate, TailscaleFunnel} {
		t.Run(string(p), func(t *testing.T) {
			a, f, store := tsFixture(t)
			s, err := a.Open(context.Background(), tsIntent(p), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if s.Origin() != "https://archive.tail123.ts.net" || f.mode != p || f.opts.hostname != "archive" || f.opts.dir != store.paths.RemoteDir+"/tailscale" {
				t.Fatal("wrong identity/mode")
			}
			if err := store.Save(context.Background(), tsIntent(p), nil); err != nil {
				t.Fatal(err)
			}
			if store.Remove(context.Background()) == nil {
				t.Fatal("live removal")
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := store.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestTailscaleIdentityPermissionsAndNotices(t *testing.T) {
	for _, mode := range []string{"identity", "permission", "auth"} {
		t.Run(mode, func(t *testing.T) {
			a, f, _ := tsFixture(t)
			i := tsIntent(TailscaleFunnel)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if mode == "identity" {
				i.SavedOrigin = "https://other.tail123.ts.net"
			}
			if mode == "permission" {
				f.status.cert = ""
			}
			if mode == "auth" {
				f.status.state = "NeedsLogin"
				f.status.auth = "https://login.tailscale.com/a/synthetic"
			}
			notices := make(chan Notice, 20)
			s, err := a.Open(ctx, i, nil, func(n Notice) {
				notices <- n
				if mode == "auth" {
					cancel()
				}
			})
			if s != nil {
				s.Close(context.Background())
			}
			switch mode {
			case "identity":
				if !errors.Is(err, ErrIdentityChanged) {
					t.Fatal(err)
				}
			case "permission":
				if !errors.Is(err, ErrPermissionRequired) {
					t.Fatal(err)
				}
			case "auth":
				select {
				case n := <-notices:
					if n.AuthURL != f.status.auth {
						t.Fatal("lost auth")
					}
				default:
					t.Fatal("missing owner notice")
				}
			}
		})
	}
}
func TestTailscaleAuthURLValidation(t *testing.T) {
	for _, u := range []string{"http://login.tailscale.com/a", "https://login.tailscale.com.evil/a", "https://user@login.tailscale.com/a", "https://login.tailscale.com:443/a", "https://login.tailscale.com/a#fragment"} {
		if validTailscaleAuthURL(u) {
			t.Fatal(u)
		}
	}
	if !validTailscaleAuthURL("https://login.tailscale.com/a/synthetic") {
		t.Fatal("official login")
	}
}
func TestTailscaleInheritedCredentialsRefused(t *testing.T) {
	for _, name := range tailscaleForbiddenEnv {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "synthetic-private")
			a, f, _ := tsFixture(t)
			s, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
			if s != nil || !errors.Is(err, ErrCredentialsRejected) {
				t.Fatal(err)
			}
			select {
			case <-f.started:
				t.Fatal("SDK started")
			default:
			}
		})
	}
}
func TestTailscaleCancellationJoinsStartAndListen(t *testing.T) {
	for _, phase := range []string{"start", "listen"} {
		t.Run(phase, func(t *testing.T) {
			a, f, store := tsFixture(t)
			gate := make(chan struct{})
			if phase == "start" {
				f.startGate = gate
			} else {
				f.listenGate = gate
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			s, err := a.Open(ctx, tsIntent(TailscalePrivate), nil, nil)
			if err == nil || s == nil {
				t.Fatal("cancel lost ownership")
			}
			closeCtx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer stop()
			err = s.Close(closeCtx)
			if phase == "start" {
				if err == nil {
					t.Fatal("unjoined start")
				}
				select {
				case <-f.closed:
					t.Fatal("close raced start")
				default:
				}
				if appdata.ResetOwned(store.paths, store.lease) == nil {
					t.Fatal("reset while starting")
				}
				close(gate)
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := appdata.ResetOwned(store.paths, store.lease); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestTailscaleOfflineAndExpiredSession(t *testing.T) {
	for _, state := range []string{"Stopped", "NeedsLogin"} {
		t.Run(state, func(t *testing.T) {
			a, f, _ := tsFixture(t)
			s, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			f.mu.Lock()
			f.status.state = state
			f.mu.Unlock()
			select {
			case err := <-s.Done():
				if err == nil {
					t.Fatal("no failure")
				}
			case <-time.After(time.Second):
				t.Fatal("offline not observed")
			}
		})
	}
}

func TestTailscaleStoreDuringSDKAtomicWrites(t *testing.T) {
	a, _, store := tsFixture(t)
	session, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	stop := make(chan struct{})
	joined := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				joined <- nil
				return
			default:
			}
			tmp := filepath.Join(store.paths.RemoteDir, "tailscale", "state.tmp")
			dst := filepath.Join(store.paths.RemoteDir, "tailscale", "state")
			if err := os.WriteFile(tmp, []byte("synthetic-sdk-state"), 0600); err != nil {
				joined <- err
				return
			}
			if err := os.Rename(tmp, dst); err != nil {
				joined <- err
				return
			}
		}
	}()
	for n := 0; n < 30; n++ {
		if err := store.Save(context.Background(), tsIntent(TailscalePrivate), nil); err != nil {
			close(stop)
			<-joined
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reopening preserves private state and cannot accept a replacement hostname.
	d, err := appdata.OpenRemoteProviderDirectory(store.paths, store.lease, appdata.TailscaleProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f, err := d.OpenFile("state", false)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
func TestTailscaleSDKConfiguration(t *testing.T) {
	n := newSDKNode(tailscaleOptions{dir: "synthetic-path", hostname: "saved-node"}).(*sdkNode)
	s := n.server
	if s.Dir != "synthetic-path" || s.Hostname != "saved-node" || s.Ephemeral || s.RunWebClient || s.ControlURL != "https://controlplane.tailscale.com" || s.Logf == nil || s.UserLogf == nil {
		t.Fatal("unsafe SDK configuration")
	}
	s.Logf("synthetic-secret %s", "private")
	s.UserLogf("synthetic-auth %s", "private")
}

func TestTailscaleExpiredRunningIdentity(t *testing.T) {
	a, f, _ := tsFixture(t)
	f.status.expired = true
	s, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
	if s != nil {
		s.Close(context.Background())
	}
	if !errors.Is(err, ErrCredentialsRejected) {
		t.Fatal(err)
	}
}

func TestTailscaleFunnelPermissionDenied(t *testing.T) {
	a, f, _ := tsFixture(t)
	f.status.funnelAllowed = false
	s, err := a.Open(context.Background(), tsIntent(TailscaleFunnel), nil, nil)
	if s != nil {
		s.Close(context.Background())
	}
	if !errors.Is(err, ErrPermissionRequired) || f.mode != "" {
		t.Fatal("permission not gated", err)
	}
}
func TestTailscaleUnknownSDKErrorIsFixedTransient(t *testing.T) {
	a, f, _ := tsFixture(t)
	f.listenErr = errors.New("secret synthetic provider failure")
	s, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
	if s != nil {
		s.Close(context.Background())
	}
	if err != ErrTransient {
		t.Fatal("raw error escaped", err)
	}
}

func TestTailscaleCloseFailureRetainsAndRetriesOwnership(t *testing.T) {
	a, f, store := tsFixture(t)
	f.closeFailures = 1
	s, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != ErrState {
		t.Fatal(err)
	}
	if appdata.ResetOwned(store.paths, store.lease) == nil {
		t.Fatal("failed Close released state")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := appdata.ResetOwned(store.paths, store.lease); err != nil {
		t.Fatal(err)
	}
}
func TestTailscaleUnsafeStateNeverStartsSDK(t *testing.T) {
	a, f, store := tsFixture(t)
	if err := os.Chmod(store.paths.RemoteDir, 0755); err != nil {
		t.Fatal(err)
	}
	s, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
	if s != nil || err != ErrState {
		t.Fatal(err)
	}
	select {
	case <-f.started:
		t.Fatal("unsafe state used")
	default:
	}
}

func TestTailscaleReconnectRetainsSavedIdentity(t *testing.T) {
	a, first, store := tsFixture(t)
	i := tsIntent(TailscalePrivate)
	s, err := a.Open(context.Background(), i, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	i.SavedOrigin = s.Origin()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := &fakeTS{status: runningTS(), started: make(chan struct{}), closed: make(chan struct{})}
	a.newNode = func(o tailscaleOptions) tailscaleNode { next.opts = o; return next }
	s, err = a.Open(context.Background(), i, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.opts.dir != first.opts.dir || s.Origin() != i.SavedOrigin {
		t.Fatal("reconnect identity changed")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed := &fakeTS{status: runningTS(), started: make(chan struct{}), closed: make(chan struct{})}
	changed.status.dns = "moved.tail456.ts.net."
	changed.status.cert = "moved.tail456.ts.net"
	a.newNode = func(tailscaleOptions) tailscaleNode { return changed }
	s, err = a.Open(context.Background(), i, nil, nil)
	if s != nil {
		s.Close(context.Background())
	}
	if err != ErrIdentityChanged || changed.mode != "" {
		t.Fatal("migrated node accepted", err)
	}
	if err := store.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTailscalePrivateMagicDNSRequiredBeforeListen(t *testing.T) {
	a, f, _ := tsFixture(t)
	f.status.magicDNS = false
	s, err := a.Open(context.Background(), tsIntent(TailscalePrivate), nil, nil)
	if s != nil {
		s.Close(context.Background())
	}
	if err != ErrPermissionRequired || f.mode != "" {
		t.Fatal("MagicDNS setup was not gated", err, f.mode)
	}
}

func TestTailscaleSDKCacheLoadReconnectRemoveReset(t *testing.T) {
	for _, action := range []string{"remove", "reset"} {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/crash-%v", action, crash), func(t *testing.T) {
				a, _, store := tsFixture(t)
				i := tsIntent(TailscalePrivate)
				i.SavedOrigin = "https://archive.tail123.ts.net"
				if err := store.Save(context.Background(), i, nil); err != nil {
					t.Fatal(err)
				}
				var session Session
				var err error
				if !crash {
					session, err = a.Open(context.Background(), i, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(filepath.Join(store.paths.RemoteDir, "tailscale"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				path := filepath.Join(store.paths.RemoteDir, "tailscale")
				certDir := filepath.Join(path, "certs")
				if err := os.Mkdir(certDir, 0700); err != nil {
					t.Fatal(err)
				}
				pub, priv, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"archive.tail123.ts.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
				der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
				if err != nil {
					t.Fatal(err)
				}
				certPath := filepath.Join(certDir, "archive.tail123.ts.net.crt")
				if err := atomicfile.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
					t.Fatal(err)
				}
				st, err := os.Stat(certPath)
				if err != nil || st.Mode().Perm() != 0644 {
					t.Fatal("fixture did not exercise real SDK mode", err)
				}
				fs, err := tka.ChonkDir(filepath.Join(path, "tka-profiles", "abcd"))
				if err != nil {
					t.Fatal(err)
				}
				aum := tka.AUM{MessageKind: tka.AUMNoOp}
				if err := fs.CommitVerifiedAUMs([]tka.AUM{aum}); err != nil {
					t.Fatal(err)
				}
				if session != nil {
					if err := session.Close(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				saved, _, err := store.Load(context.Background())
				if err != nil || saved != i {
					t.Fatal("inactive cache lost saved intent", err)
				}
				fresh := &fakeTS{status: runningTS(), started: make(chan struct{}), closed: make(chan struct{})}
				a.newNode = func(tailscaleOptions) tailscaleNode { return fresh }
				session, err = a.Open(context.Background(), saved, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if session.Origin() != i.SavedOrigin {
					t.Fatal("changed saved origin")
				}
				if err := session.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				if action == "remove" {
					err = store.Remove(context.Background())
				} else {
					err = appdata.ResetOwned(store.paths, store.lease)
				}
				if err != nil {
					t.Fatal(err)
				}
				saved, _, err = store.Load(context.Background())
				if err != nil || saved.Enabled {
					t.Fatal("state remained", err)
				}
			})
		}
	}
}
