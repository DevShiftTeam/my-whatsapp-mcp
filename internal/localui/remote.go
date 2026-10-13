package localui

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
)

const remoteOwnerCookie = "__Host-wa_owner"
const remoteSessionLimit = 32
const remoteLoginBurst = 5
const remoteLoginWindow = time.Minute

// RemoteConfig is trusted backend configuration. Forwarded request headers are
// never configuration and cannot select an origin, account or owner capability.
type RemoteConfig struct {
	Origin        string
	ListenAddress string
	PasswordHash  string
	SessionTTL    time.Duration
	MCPHandler    http.Handler
}

type RemoteServer struct {
	dispatch                                  *Server
	origin, host, listenAddress, passwordHash string
	password                                  adminPasswordHash
	mcpHandler                                http.Handler
	clock                                     ownerClock
	sessionTTL                                time.Duration
	mu                                        sync.Mutex
	handlers                                  sync.WaitGroup
	sessions                                  map[[32]byte]session
	closed                                    bool
	loginWindowStart                          time.Time
	loginAttempts                             int
	kdfSlot                                   chan struct{}
}

func ValidateRemoteConfig(cfg RemoteConfig) error {
	u, err := url.Parse(cfg.Origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || cfg.Origin != "https://"+u.Host || !remotePublicHost(u) {
		return errors.New("invalid_remote_admin_origin")
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:6969"
	}
	host, portText, err := net.SplitHostPort(cfg.ListenAddress)
	port, parseErr := strconv.Atoi(portText)
	if err != nil || parseErr != nil || host != "127.0.0.1" || port < 1 || port > 65535 || cfg.ListenAddress != net.JoinHostPort(host, strconv.Itoa(port)) {
		return errors.New("invalid_remote_admin_listener")
	}
	if cfg.SessionTTL < 0 || cfg.SessionTTL > 24*time.Hour {
		return errors.New("invalid_session_duration")
	}
	if _, ok := parseAdminPasswordHash(cfg.PasswordHash); !ok {
		return errors.New("invalid_admin_password_hash")
	}
	return nil
}

func remotePublicHost(u *url.URL) bool {
	host := u.Hostname()
	if len(host) == 0 || len(host) > 253 || strings.ToLower(host) != host || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	if portText := u.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		return err == nil && port >= 1 && port <= 65535 && portText == strconv.Itoa(port) && u.Host == net.JoinHostPort(host, portText)
	}
	return u.Host == host
}

func NewRemote(cfg RemoteConfig, app Application) (*RemoteServer, error) {
	if err := ValidateRemoteConfig(cfg); err != nil {
		return nil, err
	}
	if app == nil {
		return nil, errors.New("invalid_remote_admin_application")
	}
	u, _ := url.Parse(cfg.Origin)
	password, _ := parseAdminPasswordHash(cfg.PasswordHash)
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:6969"
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	now := time.Now()
	s := &RemoteServer{dispatch: &Server{realm: realmLegacy, app: app}, origin: cfg.Origin, host: u.Host, listenAddress: cfg.ListenAddress, passwordHash: cfg.PasswordHash,
		password: password, mcpHandler: cfg.MCPHandler, sessionTTL: cfg.SessionTTL, sessions: make(map[[32]byte]session), kdfSlot: make(chan struct{}, 1),
		clock: ownerClock{base: now, last: now, wall: time.Now, elapsed: func() time.Duration { return time.Since(now) }}}
	s.dispatch.deleteOwnerSessions = func(w http.ResponseWriter) {
		s.RevokeSessions()
		s.clearCookie(w)
	}
	return s, nil
}

func (s *RemoteServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Add and shutdown admission share one lock. Serve can join every
		// accepted request before the application and its archive are closed.
		s.mu.Lock()
		closed := s.closed
		if !closed {
			s.handlers.Add(1)
		}
		s.mu.Unlock()
		if closed {
			securityHeaders(w)
			if r.URL.Path == "/api/auth/login" {
				remoteFail(w, 503, "auth_unavailable")
			} else {
				remoteFail(w, 401, "unauthorized")
			}
			return
		}
		defer s.handlers.Done()
		s.handle(w, r)
	})
}

func (s *RemoteServer) RevokeSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	clear(s.sessions)
}

func (s *RemoteServer) Serve(ctx context.Context, listener net.Listener) error {
	var once sync.Once
	var shutdown context.Context
	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	return s.ServeWithShutdown(ctx, listener, func() context.Context {
		once.Do(func() { shutdown, cancel = context.WithTimeout(context.Background(), 5*time.Second) })
		return shutdown
	})
}

// ServeWithShutdown shares the composition deadline and retains incomplete request ownership.
func (s *RemoteServer) ServeWithShutdown(ctx context.Context, listener net.Listener, stopContext func() context.Context) error {
	address, ok := listener.Addr().(*net.TCPAddr)
	if stopContext == nil || !ok || address.IP.String() != "127.0.0.1" || net.JoinHostPort(address.IP.String(), strconv.Itoa(address.Port)) != s.listenAddress {
		return errors.New("invalid_remote_admin_listener")
	}
	server := &http.Server{Handler: s.Handler(), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	quiesced := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		shutdown := stopContext()
		s.RevokeSessions()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
		joined := make(chan struct{})
		go func() { s.handlers.Wait(); close(joined) }()
		select {
		case <-joined:
			quiesced <- nil
		case <-shutdown.Done():
			select {
			case <-joined:
				quiesced <- nil
			default:
				quiesced <- ErrShutdownIncomplete
			}
		}
	}()
	err := server.Serve(listener)
	close(done)
	if joinErr := <-quiesced; joinErr != nil {
		return joinErr
	}
	s.RevokeSessions()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *RemoteServer) handle(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	if r.Host != s.host || (r.URL.Host != "" && r.URL.Host != s.host) {
		remoteFail(w, 403, "host_denied")
		return
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 || (len(origins) == 1 && origins[0] != s.origin) {
		remoteFail(w, 403, "origin_denied")
		return
	}
	// Private local owner capabilities are never exported by this listener.
	if r.URL.Path == "/api/bootstrap" || r.URL.Path == "/api/owner/attach" || nativeOnlyRoute(r.URL.Path) {
		remoteFail(w, 404, "route_unavailable")
		return
	}
	if r.URL.Path == "/api/interface" {
		if r.Method != http.MethodGet || !bodyFree(r) || r.URL.RawQuery != "" || r.URL.ForceQuery {
			remoteFail(w, 400, "invalid_request")
			return
		}
		respond(w, map[string]string{"realm": "legacy"})
		return
	}
	if r.URL.RawQuery != "" && r.URL.Path != "/api/chats" && r.URL.Path != "/api/status" {
		remoteFail(w, 400, "invalid_request")
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/assets/app.css" || r.URL.Path == "/assets/app.js" || r.URL.Path == "/assets/theme.js" || r.URL.Path == "/assets/selects.js" || r.URL.Path == "/assets/logo.png") {
		s.static(w, r)
		return
	}
	if r.URL.Path == "/mcp" || remoteSendRoute(r.URL.Path) || mcpserver.AttachmentDownloadRoute(r.URL.Path) {
		if r.URL.RawPath != "" || r.URL.ForceQuery {
			remoteFail(w, 404, "route_unavailable")
			return
		}
		if s.mcpHandler == nil {
			remoteFail(w, 404, "route_unavailable")
			return
		}
		// The separately constructed remote MCP handler performs scoped bearer
		// admission. Owner cookies/passwords cannot substitute for its token.
		s.mcpHandler.ServeHTTP(w, r)
		return
	}
	switch r.URL.Path {
	case "/api/auth/login", "/api/auth/session", "/api/auth/logout":
		if r.Method != http.MethodPost {
			remoteFail(w, 405, "method_denied")
			return
		}
		if !s.exactOrigin(r) {
			remoteFail(w, 403, "origin_denied")
			return
		}
		switch r.URL.Path {
		case "/api/auth/login":
			s.login(w, r)
		case "/api/auth/session":
			s.restoreSession(w, r)
		case "/api/auth/logout":
			s.logout(w, r)
		}
		return
	}
	method, exists := routeMethods[r.URL.Path]
	if !exists {
		remoteFail(w, 404, "route_unavailable")
		return
	}
	csrf, ownerDigest, ok := s.authenticated(r)
	if !ok {
		remoteFail(w, 401, "unauthorized")
		return
	}
	if len(r.Header.Values("X-CSRF-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
		remoteFail(w, 403, "csrf_denied")
		return
	}
	if r.URL.Path == "/api/original-budget" && r.Method == http.MethodPut {
		method = http.MethodPut
	}
	if r.Method != method {
		remoteFail(w, 405, "method_denied")
		return
	}
	if method != http.MethodGet && !s.exactOrigin(r) {
		remoteFail(w, 403, "origin_denied")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), ownerReadCheckKey{}, func() bool {
		current, _, valid := s.authenticated(r)
		return valid && subtle.ConstantTimeCompare([]byte(current), []byte(csrf)) == 1
	}))
	r = r.WithContext(context.WithValue(r.Context(), ownerReadAdmissionErrorKey{}, func() error { return s.historicalAdmissionError(r, ownerDigest, csrf) }))
	s.dispatch.api(w, r, csrf)
}

func remoteSendRoute(path string) bool {
	switch path {
	case "/api/send/message", "/api/send/media", "/api/send/voice", "/api/send/status":
		return true
	default:
		return false
	}
}

func (s *RemoteServer) exactOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	return len(values) == 1 && values[0] == s.origin
}

// LinkOwnerRevocations is a composition-only setup step before either Serve.
// Successful archive deletion must invalidate every interface to that archive.
func LinkOwnerRevocations(local *Server, remote *RemoteServer) error {
	if local == nil || remote == nil || remote.dispatch == nil {
		return errors.New("invalid_owner_revocation_link")
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.dispatch.mu.Lock()
	defer remote.dispatch.mu.Unlock()
	if local.peerDeleteRevocation != nil || remote.dispatch.peerDeleteRevocation != nil || local.owner != nil || local.attachRevoked || remote.closed || len(remote.sessions) != 0 {
		return errors.New("invalid_owner_revocation_link")
	}
	local.peerDeleteRevocation = remote.RevokeSessions
	remote.dispatch.peerDeleteRevocation = local.RevokeSessions
	return nil
}

func (s *RemoteServer) static(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.dispatch.static(w, r)
		return
	}
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		remoteFail(w, 503, "interface_unavailable")
		return
	}
	page := string(historicalSnapshotHTML(data, s.dispatch.app))
	if strings.Count(page, "<html ") != 1 {
		remoteFail(w, 503, "interface_unavailable")
		return
	}
	page = strings.Replace(page, "<html ", `<html data-remote-admin="true" `, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, page)
}
