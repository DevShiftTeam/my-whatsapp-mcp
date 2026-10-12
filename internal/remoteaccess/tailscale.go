package remoteaccess

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

var tailscaleForbiddenEnv = []string{"TS_AUTHKEY", "TS_AUTH_KEY", "TS_CLIENT_SECRET", "TS_CLIENT_ID", "TS_ID_TOKEN", "TS_AUDIENCE", "TSNET_FORCE_LOGIN"}

type tailscaleOptions struct{ dir, hostname string }
type tailscaleStatus struct {
	state, auth, dns, cert                              string
	hasIP, hasTailnet, expired, funnelAllowed, magicDNS bool
}
type tailscaleNode interface {
	Start() error
	Status(context.Context) (tailscaleStatus, error)
	Up(context.Context) error
	Listen(Provider) (net.Listener, error)
	Close() error
}
type tailscaleAdapter struct {
	paths   appdata.Paths
	lease   *appdata.InstanceLock
	newNode func(tailscaleOptions) tailscaleNode
	poll    time.Duration
}

// NewTailscaleAdapter is lazy: disabled remote access opens no provider state and
// starts no SDK. Private and Funnel use the same persistent node identity.
func NewTailscaleAdapter(paths appdata.Paths, lease *appdata.InstanceLock) Adapter {
	return &tailscaleAdapter{paths: paths, lease: lease, newNode: newSDKNode, poll: time.Second}
}
func (a *tailscaleAdapter) Open(ctx context.Context, i Intent, secret Secret, notice func(Notice)) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if validateIntent(i) != nil || !i.Enabled || i.NodeName == "" || len(secret) != 0 || (i.Provider != TailscalePrivate && i.Provider != TailscaleFunnel) {
		return nil, ErrInvalidIntent
	}
	// Host invariant: other application code must not mutate these variables while
	// the SDK is running. Never consume inherited credentials or alter process env.
	for _, name := range tailscaleForbiddenEnv {
		if os.Getenv(name) != "" {
			return nil, ErrCredentialsRejected
		}
	}
	dir, err := appdata.OpenRemoteProviderDirectory(a.paths, a.lease, appdata.TailscaleProvider)
	if err != nil {
		return nil, ErrState
	}
	path, err := dir.Path()
	if err != nil {
		dir.Close()
		return nil, ErrState
	}
	runCtx, cancel := context.WithCancel(ctx)
	s := &tailscaleSession{node: a.newNode(tailscaleOptions{path, i.NodeName}), dir: dir, cancel: cancel, started: make(chan struct{}), joined: make(chan struct{}), done: make(chan error, 1), ready: make(chan error, 1)}
	go s.run(runCtx, i, notice, a.poll)
	select {
	case err := <-s.ready:
		return s, err
	case <-ctx.Done():
		return s, ctx.Err()
	}
}
func validTailscaleAuthURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "login.tailscale.com" && u.User == nil && u.Opaque == "" && u.Fragment == "" && strings.HasPrefix(raw, "https://login.tailscale.com/")
}
func tailscaleOrigin(st tailscaleStatus, saved string) (string, error) {
	if st.expired {
		return "", ErrCredentialsRejected
	}
	if st.state != "Running" || !st.hasIP || !st.hasTailnet {
		return "", ErrTransient
	}
	dns := strings.TrimSuffix(st.dns, ".")
	if st.cert == "" {
		return "", ErrPermissionRequired
	}
	if dns != st.cert {
		return "", ErrIdentityChanged
	}
	return PinOrigin(saved, "https://"+dns)
}

type tailscaleSession struct {
	node            tailscaleNode
	dir             *appdata.RemoteProviderDirectory
	cancel          context.CancelFunc
	started, joined chan struct{}
	ready, done     chan error

	mu                      sync.Mutex
	origin                  string
	listener                net.Listener
	closeAttempt            *tailscaleCloseAttempt
	nodeClosed, fullyClosed bool
}

func (s *tailscaleSession) Origin() string { s.mu.Lock(); defer s.mu.Unlock(); return s.origin }
func (s *tailscaleSession) Listener() net.Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listener
}
func (s *tailscaleSession) Done() <-chan error { return s.done }
func (s *tailscaleSession) run(ctx context.Context, i Intent, notice func(Notice), interval time.Duration) {
	defer close(s.joined)
	err := s.node.Start()
	close(s.started)
	if err != nil {
		s.ready <- ErrTransient
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastAuth := ""
	emitAuth := func(st tailscaleStatus) {
		if notice != nil && st.auth != lastAuth && validTailscaleAuthURL(st.auth) {
			lastAuth = st.auth
			notice(Notice{State: "needs_setup", Code: "tailscale_sign_in", AuthURL: st.auth})
		}
	}
	for {
		if ctx.Err() != nil {
			s.ready <- ctx.Err()
			return
		}
		st, err := s.node.Status(ctx)
		if err != nil {
			s.ready <- ErrTransient
			return
		}
		emitAuth(st)
		if st.state == "NeedsMachineAuth" {
			if notice != nil {
				notice(Notice{State: "needs_setup", Code: "tailscale_device_approval"})
			}
		}
		if st.state == "Running" {
			origin, err := tailscaleOrigin(st, i.SavedOrigin)
			if err != nil {
				s.ready <- err
				return
			}
			if i.Provider == TailscalePrivate && !st.magicDNS {
				s.ready <- ErrPermissionRequired
				return
			}
			if i.Provider == TailscaleFunnel && !st.funnelAllowed {
				s.ready <- ErrPermissionRequired
				return
			}
			if s.dir.Validate() != nil {
				s.ready <- ErrState
				return
			}
			if s.node.Up(ctx) != nil {
				s.ready <- ErrTransient
				return
			}
			// Context-free SDK Listen calls are unblocked by Close, then joined before
			// releasing the directory registration. No handler is exposed by the adapter.
			ln, err := s.node.Listen(i.Provider)
			if err != nil {
				s.ready <- ErrTransient
				return
			}
			if ctx.Err() != nil {
				ln.Close()
				s.ready <- ctx.Err()
				return
			}
			// Recheck authenticated identity after a potentially blocking listener open.
			st, err = s.node.Status(ctx)
			if err != nil {
				err = ErrTransient
			}
			if err == nil {
				_, err = tailscaleOrigin(st, origin)
			}
			if err != nil {
				ln.Close()
				s.ready <- err
				return
			}
			s.mu.Lock()
			s.origin = origin
			s.listener = ln
			s.mu.Unlock()
			s.ready <- nil
			break
		}
		select {
		case <-ctx.Done():
			s.ready <- ctx.Err()
			return
		case <-ticker.C:
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st, err := s.node.Status(ctx)
			if err != nil {
				s.done <- ErrTransient
				return
			}
			emitAuth(st)
			if st.state == "NeedsLogin" {
				s.done <- ErrCredentialsRejected
				return
			}
			if _, err := tailscaleOrigin(st, s.Origin()); err != nil {
				s.done <- err
				return
			}
			if s.dir.Validate() != nil {
				s.done <- ErrState
				return
			}
		}
	}
}

type tailscaleCloseAttempt struct {
	done chan struct{}
	err  error
}

// Close permits deadline-bounded retries. A timeout never releases the directory:
// wait for completed Start before SDK Close, then join the context-free listener
// and status worker before releasing owned anchors. SDK errors retain ownership.
func (s *tailscaleSession) Close(ctx context.Context) error {
	s.cancel()
	s.mu.Lock()
	if s.fullyClosed {
		s.mu.Unlock()
		return nil
	}
	attempt := s.closeAttempt
	if attempt != nil {
		select {
		case <-attempt.done:
			attempt = nil
		default:
		}
	}
	if attempt == nil {
		attempt = &tailscaleCloseAttempt{done: make(chan struct{})}
		s.closeAttempt = attempt
		go s.drain(attempt)
	}
	s.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *tailscaleSession) drain(attempt *tailscaleCloseAttempt) {
	defer close(attempt.done)
	<-s.started
	s.mu.Lock()
	nodeClosed := s.nodeClosed
	s.mu.Unlock()
	if !nodeClosed {
		if s.node.Close() != nil {
			attempt.err = ErrState
			return
		}
		s.mu.Lock()
		s.nodeClosed = true
		s.mu.Unlock()
	}
	<-s.joined
	if ln := s.Listener(); ln != nil {
		ln.Close()
	}
	if s.dir.Close() != nil {
		attempt.err = ErrState
		return
	}
	s.mu.Lock()
	s.fullyClosed = true
	s.mu.Unlock()
}
