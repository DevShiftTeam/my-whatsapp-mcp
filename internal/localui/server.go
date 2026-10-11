package localui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

//go:embed assets/index.html assets/app.css assets/app.js assets/theme.js assets/selects.js assets/logo.png
var assets embed.FS

const maxBodyBytes = 32768

type session struct {
	digest  [32]byte
	token   string
	csrf    string
	expires time.Time
}

type Server struct {
	app                                    Application
	origin, host, cookieName, bootstrapURL string
	attachURL, instance                    string
	attachKey                              [32]byte
	demo, attachRevoked                    bool
	bootstraps                             map[[32]byte]time.Time
	nonces                                 map[string]time.Time
	clock                                  ownerClock
	sessionTTL                             time.Duration
	mu                                     sync.Mutex
	owner                                  *session
	// The independent remote interface supplies its own session revocation on
	// archive deletion; the local owner capability flow remains unchanged.
	deleteOwnerSessions  func(http.ResponseWriter)
	peerDeleteRevocation func()
}

// New generates one server-lifetime attach capability and in-memory browser
// bootstraps. Composition writes AttachURL only to its protected owner file.
func New(cfg Config, app Application) (*Server, error) {
	u, err := url.Parse(cfg.Origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.Host == "" || app == nil {
		return nil, errors.New("invalid_local_origin")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 || u.Host != net.JoinHostPort(ip.String(), strconv.Itoa(port)) || cfg.Origin != "http://"+u.Host {
		return nil, errors.New("invalid_local_origin")
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.BootstrapTTL == 0 {
		cfg.BootstrapTTL = 30 * time.Second
	}
	if cfg.SessionTTL < 0 || cfg.SessionTTL > 24*time.Hour || cfg.BootstrapTTL < 0 || cfg.BootstrapTTL > 30*time.Second {
		return nil, errors.New("invalid_session_duration")
	}
	token, err := randomToken()
	if err != nil {
		return nil, errors.New("owner_auth_unavailable")
	}
	var instance [16]byte
	var key [32]byte
	if _, err := rand.Read(instance[:]); err != nil {
		return nil, errors.New("owner_auth_unavailable")
	}
	if _, err := rand.Read(key[:]); err != nil {
		return nil, errors.New("owner_auth_unavailable")
	}
	now := time.Now()
	s := &Server{app: app, origin: cfg.Origin, host: u.Host, cookieName: "owner_session_" + strconv.Itoa(port), bootstrapURL: cfg.Origin + "/#bootstrap=" + token,
		instance: hex.EncodeToString(instance[:]), attachKey: key, demo: cfg.Demo,
		bootstraps: map[[32]byte]time.Time{sha256.Sum256([]byte(token)): now.Add(cfg.BootstrapTTL)},
		nonces:     make(map[string]time.Time), sessionTTL: cfg.SessionTTL,
		clock: ownerClock{base: now, last: now, wall: time.Now, elapsed: func() time.Duration { return time.Since(now) }}}
	s.attachURL = cfg.Origin + "/#attach=" + s.instance + "." + base64.RawURLEncoding.EncodeToString(key[:])
	return s, nil
}

func randomToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func (s *Server) BootstrapURL() string  { return s.bootstrapURL }
func (s *Server) AttachURL() string     { return s.attachURL }
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }

// RevokeSessions also burns the bootstrap capability. Restart generates a fresh
// capability; revocation cannot accidentally reopen authentication.
func (s *Server) RevokeSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner = nil
	s.attachRevoked = true
	clear(s.bootstraps)
	clear(s.nonces)
	clear(s.attachKey[:])
}

// Serve enforces listener safety as well as request authority. It deliberately
// configures no request or body logging.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() || net.JoinHostPort(address.IP.String(), strconv.Itoa(address.Port)) != s.host {
		return errors.New("invalid_local_listener")
	}
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		select {
		case <-ctx.Done():
		case <-done:
		}
		s.RevokeSessions()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
	}()
	err := server.Serve(listener)
	close(done)
	<-quiesced
	s.RevokeSessions()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if r.Host != s.host || (r.URL.Host != "" && r.URL.Host != s.host) {
		fail(w, http.StatusForbidden, "host_denied")
		return
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 || (len(origins) == 1 && origins[0] != s.origin) {
		fail(w, http.StatusForbidden, "origin_denied")
		return
	}
	if r.URL.RawQuery != "" && r.URL.Path != "/api/chats" {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/assets/app.css" || r.URL.Path == "/assets/app.js" || r.URL.Path == "/assets/theme.js" || r.URL.Path == "/assets/selects.js" || r.URL.Path == "/assets/logo.png") {
		s.static(w, r)
		return
	}
	if r.URL.Path == "/api/bootstrap" || r.URL.Path == "/api/owner/attach" {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_denied")
			return
		}
		if r.Header.Get("Origin") != s.origin {
			fail(w, http.StatusForbidden, "origin_denied")
			return
		}
		if r.URL.Path == "/api/owner/attach" {
			s.ownerAttach(w, r)
		} else {
			s.bootstrap(w, r)
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		fail(w, http.StatusNotFound, "route_unavailable")
		return
	}
	csrf, ok := s.authenticated(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Cookies are host scoped, not port scoped. Requiring this second capability
	// for every read prevents a different localhost service from replaying a
	// captured cookie to recover archive data or the per-origin capability.
	if len(r.Header.Values("X-CSRF-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
		fail(w, http.StatusForbidden, "csrf_denied")
		return
	}
	method, exists := routeMethods[r.URL.Path]
	if !exists {
		fail(w, http.StatusNotFound, "route_unavailable")
		return
	}
	if r.URL.Path == "/api/original-budget" && r.Method == http.MethodPut {
		method = http.MethodPut
	}
	if r.Method != method {
		fail(w, http.StatusMethodNotAllowed, "method_denied")
		return
	}
	if method != http.MethodGet {
		if r.Header.Get("Origin") != s.origin {
			fail(w, http.StatusForbidden, "origin_denied")
			return
		}
	}
	s.api(w, r, csrf)
}

var routeMethods = map[string]string{
	"/api/session": http.MethodGet, "/api/setup": http.MethodGet, "/api/policy": http.MethodPut,
	"/api/pairing/start": http.MethodPost, "/api/pairing": http.MethodGet, "/api/pairing/qr": http.MethodGet,
	"/api/status": http.MethodGet, "/api/chats": http.MethodGet, "/api/search": http.MethodPost,
	"/api/stats":   http.MethodPost,
	"/api/context": http.MethodPost, "/api/pause": http.MethodPost, "/api/resume": http.MethodPost,
	"/api/disconnect": http.MethodPost, "/api/data/delete": http.MethodPost,
	"/api/models": http.MethodGet, "/api/models/state": http.MethodGet,
	"/api/models/operations": http.MethodPost, "/api/models/operations/cancel": http.MethodPost,
	"/api/local-ai": http.MethodGet, "/api/local-ai/select": http.MethodPost,
	"/api/connections": http.MethodGet, "/api/connections/create": http.MethodPost,
	"/api/connections/guide": http.MethodPost, "/api/connections/revoke": http.MethodPost,
	"/api/connections/reveal":          http.MethodPost,
	"/api/connections/send-permission": http.MethodPost,
	"/api/original-budget":             http.MethodGet,
	"/api/voice/retry":                 http.MethodPost,
	"/api/documents/retry":             http.MethodPost,
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := "assets/index.html"
	contentType := "text/html; charset=utf-8"
	if r.URL.Path == "/assets/app.css" {
		name = "assets/app.css"
		contentType = "text/css; charset=utf-8"
	}
	if r.URL.Path == "/assets/app.js" {
		name = "assets/app.js"
		contentType = "text/javascript; charset=utf-8"
	}
	if r.URL.Path == "/assets/theme.js" {
		name = "assets/theme.js"
		contentType = "text/javascript; charset=utf-8"
	}
	if r.URL.Path == "/assets/selects.js" {
		name = "assets/selects.js"
		contentType = "text/javascript; charset=utf-8"
	}
	if r.URL.Path == "/assets/logo.png" {
		name = "assets/logo.png"
		contentType = "image/png"
	}
	data, err := assets.ReadFile(name)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "interface_unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &request) {
		return
	}
	token, err := randomToken()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "owner_auth_unavailable")
		return
	}
	csrf, err := randomToken()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "owner_auth_unavailable")
		return
	}
	digest := sha256.Sum256([]byte(request.Token))
	s.mu.Lock()
	now := s.clock.nowLocked()
	s.expireCapabilitiesLocked(now)
	var matched [32]byte
	found := false
	for pending := range s.bootstraps {
		if subtle.ConstantTimeCompare(digest[:], pending[:]) == 1 {
			matched = pending
			found = true
		}
	}
	if s.attachRevoked || !found {
		s.mu.Unlock()
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	delete(s.bootstraps, matched)
	// Reopening the app must not invalidate another owner window. Only a fresh,
	// validated one-use bootstrap may reuse these in-memory credentials, and it
	// retains the original session deadline rather than extending it.
	if s.owner == nil || !now.Before(s.owner.expires) {
		s.owner = &session{digest: sha256.Sum256([]byte(token)), token: token, csrf: csrf, expires: now.Add(s.sessionTTL)}
	} else {
		token, csrf = s.owner.token, s.owner.csrf
	}
	expires := s.owner.expires
	remaining := expires.Sub(now)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: s.cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int((remaining + time.Second - 1) / time.Second), Expires: expires})
	respond(w, map[string]string{"csrf_token": csrf})
}

func (s *Server) authenticated(r *http.Request) (string, bool) {
	var token string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == s.cookieName {
			count++
			if count > 1 {
				return "", false
			}
			token = cookie.Value
		}
	}
	if token == "" || len(token) > 128 {
		return "", false
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == nil || !s.clock.nowLocked().Before(s.owner.expires) || subtle.ConstantTimeCompare(digest[:], s.owner.digest[:]) != 1 {
		return "", false
	}
	return s.owner.csrf, true
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if len(r.Header.Values("Content-Type")) != 1 {
		fail(w, http.StatusUnsupportedMediaType, "invalid_request")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "invalid_request")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			fail(w, http.StatusBadRequest, "invalid_request")
		}
		return false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		fail(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(into); err == nil {
		err = decoder.Decode(new(any))
		if errors.Is(err, io.EOF) {
			if uniqueFields(data) {
				return true
			}
			fail(w, http.StatusBadRequest, "invalid_request")
			return false
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, http.StatusRequestEntityTooLarge, "request_too_large")
	} else {
		fail(w, http.StatusBadRequest, "invalid_request")
	}
	return false
}

// Visit every object, including nested DTOs and array elements. Rejecting
// repeated decoded field names also rejects escaped names and case aliases.
func uniqueFields(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !uniqueJSONValue(decoder, 0) {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func uniqueJSONValue(decoder *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return true
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return false
			}
			// The public schema has ASCII names only; encoding/json otherwise
			// accepts Unicode case-fold aliases such as a long s.
			for _, char := range key {
				if char > 127 {
					return false
				}
			}
			key = strings.ToLower(key)
			if seen[key] || !uniqueJSONValue(decoder, depth+1) {
				return false
			}
			seen[key] = true
		}
	case '[':
		for decoder.More() {
			if !uniqueJSONValue(decoder, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	closing, err := decoder.Token()
	return err == nil && (delimiter == '{' && closing == json.Delim('}') || delimiter == '[' && closing == json.Delim(']'))
}

func (s *Server) api(w http.ResponseWriter, r *http.Request, csrf string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	switch r.URL.Path {
	case "/api/connections/send-permission":
		s.sendPermission(w, r, ctx)
	case "/api/connections/reveal":
		s.revealRemote(w, r, ctx)
	case "/api/original-budget":
		s.originalBudget(w, r, ctx)
	case "/api/voice/retry":
		s.voiceRetry(w, r, ctx)
	case "/api/documents/retry":
		s.documentRetry(w, r, ctx)
	case "/api/connections", "/api/connections/create", "/api/connections/guide", "/api/connections/revoke":
		s.connections(w, r, ctx)
	case "/api/models", "/api/models/state", "/api/models/operations", "/api/models/operations/cancel":
		s.models(w, r, ctx)
	case "/api/local-ai", "/api/local-ai/select":
		s.localAI(w, r, ctx)
	case "/api/session":
		respond(w, map[string]string{"csrf_token": csrf})
	case "/api/setup":
		value, err := s.app.Setup(ctx)
		result(w, value, err)
	case "/api/status":
		value, err := s.app.Status(ctx)
		result(w, value, err)
	case "/api/pairing":
		value, err := s.app.PairingState(ctx)
		result(w, value, err)
	case "/api/pairing/qr":
		state, err := s.app.PairingState(ctx)
		if err != nil {
			publicError(w, err)
			return
		}
		if !state.QRAvailable {
			fail(w, http.StatusNotFound, "source_unavailable")
			return
		}
		data, err := s.app.QRPNG(ctx)
		if err != nil {
			publicError(w, err)
			return
		}
		if len(data) < 8 || len(data) > 1<<20 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			fail(w, http.StatusServiceUnavailable, "source_unavailable")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	case "/api/policy":
		s.policy(w, r, ctx)
	case "/api/search":
		s.search(w, r, ctx)
	case "/api/stats":
		s.stats(w, r, ctx)
	case "/api/context":
		s.context(w, r, ctx)
	case "/api/chats":
		s.chats(w, r, ctx)
	case "/api/data/delete":
		var request struct {
			Confirmation string `json:"confirmation"`
		}
		if !decode(w, r, &request) {
			return
		}
		if request.Confirmation != DeleteConfirmation {
			fail(w, http.StatusBadRequest, "confirmation_required")
			return
		}
		if err := s.app.DeleteLocalData(ctx, true); err != nil {
			publicError(w, err)
			return
		}
		if s.deleteOwnerSessions != nil {
			s.deleteOwnerSessions(w)
		} else {
			s.RevokeSessions()
			http.SetCookie(w, &http.Cookie{Name: s.cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		}
		s.mu.Lock()
		revokePeer := s.peerDeleteRevocation
		s.mu.Unlock()
		if revokePeer != nil {
			revokePeer()
		}
		respond(w, map[string]bool{"ok": true})
	default:
		var request struct{}
		if !decode(w, r, &request) {
			return
		}
		var err error
		switch r.URL.Path {
		case "/api/pairing/start":
			setup, setupErr := s.app.Setup(ctx)
			if setupErr != nil {
				publicError(w, setupErr)
				return
			}
			if !setup.Configured {
				fail(w, http.StatusConflict, "setup_required")
				return
			}
			err = s.app.StartPairing(ctx)
		case "/api/pause":
			err = s.app.Pause(ctx)
		case "/api/resume":
			err = s.app.Resume(ctx)
		case "/api/disconnect":
			err = s.app.Disconnect(ctx)
		}
		result(w, map[string]bool{"ok": true}, err)
	}
}

func (s *Server) policy(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	var request struct {
		ExpectedGeneration   archive.Generation `json:"expected_generation"`
		DateMode             archive.DateMode   `json:"date_mode"`
		ImportStart          time.Time          `json:"import_start"`
		ChatMode             archive.ChatMode   `json:"chat_mode"`
		IncludedChats        []archive.ChatID   `json:"included_chats"`
		RollingRetentionDays int                `json:"rolling_retention_days"`
	}
	if !decode(w, r, &request) {
		return
	}
	if (request.DateMode != archive.DateSince && request.DateMode != archive.DateAllAvailable) ||
		(request.DateMode == archive.DateSince && request.ImportStart.IsZero()) ||
		(request.DateMode == archive.DateAllAvailable && !request.ImportStart.IsZero()) ||
		(request.ChatMode != archive.ChatsAll && request.ChatMode != archive.ChatsSelective) ||
		(request.ChatMode == archive.ChatsAll && len(request.IncludedChats) != 0) ||
		request.RollingRetentionDays < 0 || request.RollingRetentionDays > 36500 || !validIDs(request.IncludedChats, 1000) {
		fail(w, http.StatusBadRequest, "invalid_policy")
		return
	}
	policy := archive.ArchivePolicy{DateMode: request.DateMode, ImportStart: request.ImportStart,
		ChatMode: request.ChatMode, IncludedChats: request.IncludedChats, RollingRetention: time.Duration(request.RollingRetentionDays) * 24 * time.Hour}
	value, err := s.app.SetPolicy(ctx, policy, request.ExpectedGeneration)
	result(w, value, err)
}

func validIDs(ids []archive.ChatID, max int) bool {
	if len(ids) > max {
		return false
	}
	seen := make(map[archive.ChatID]bool, len(ids))
	for _, id := range ids {
		if !validOpaque(string(id)) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func validOpaque(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	var request struct {
		Query  string           `json:"query"`
		Chats  []archive.ChatID `json:"chats"`
		From   time.Time        `json:"from"`
		Until  time.Time        `json:"until"`
		Limit  int              `json:"limit"`
		Cursor string           `json:"cursor"`
	}
	if !decode(w, r, &request) {
		return
	}
	if len(request.Query) > 4096 || !utf8.ValidString(request.Query) || strings.TrimSpace(request.Query) == "" ||
		request.Limit < 0 || request.Limit > 100 || len(request.Cursor) > 8192 || !validIDs(request.Chats, 100) ||
		(!request.From.IsZero() && !request.Until.IsZero() && !request.From.Before(request.Until)) {
		fail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	if request.Limit == 0 {
		request.Limit = 30
	}
	value, err := s.app.Search(ctx, archive.SearchRequest{Query: request.Query, Chats: request.Chats, From: request.From, Until: request.Until, Limit: request.Limit, Cursor: request.Cursor})
	result(w, value, err)
}

func (s *Server) context(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	var request struct {
		MessageID archive.MessageID `json:"message_id"`
		Before    int               `json:"before"`
		After     int               `json:"after"`
	}
	if !decode(w, r, &request) {
		return
	}
	if !validOpaque(string(request.MessageID)) || request.Before < 0 || request.After < 0 || request.Before > 50 || request.After > 50 {
		fail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	value, err := s.app.Context(ctx, archive.ContextRequest{MessageID: request.MessageID, Before: request.Before, After: request.After})
	result(w, value, err)
}

func (s *Server) chats(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			fail(w, http.StatusBadRequest, "invalid_query")
			return
		}
	}
	limit := 100
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
	}
	if err != nil || limit < 1 || limit > 100 || len(query.Get("cursor")) > 8192 {
		fail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	value, err := s.app.Chats(ctx, archive.ChatListRequest{Limit: limit, Cursor: query.Get("cursor")})
	result(w, value, err)
}

func result(w http.ResponseWriter, value any, err error) {
	if err != nil {
		publicError(w, err)
		return
	}
	respond(w, value)
}

func respond(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "archive_unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(append(data, '\n'))
}

func publicError(w http.ResponseWriter, err error) {
	var code archive.Error
	if errors.As(err, &code) {
		switch code {
		case archive.ErrInvalidPolicy:
			fail(w, 400, "invalid_policy")
		case archive.ErrInvalidQuery:
			fail(w, 400, "invalid_query")
		case archive.ErrPolicyChanged:
			fail(w, 409, "policy_changed")
		case archive.ErrCursorStale:
			fail(w, 409, "cursor_stale")
		case archive.ErrReconciliationRequired:
			fail(w, 409, "reconciliation_required")
		case archive.ErrSourceUnavailable:
			fail(w, 404, "source_unavailable")
		case archive.ErrUnauthorized:
			fail(w, 403, "unauthorized")
		case archive.ErrFTS5Unavailable:
			fail(w, 503, "fts5_unavailable")
		default:
			fail(w, 503, "archive_unavailable")
		}
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		fail(w, 503, "request_interrupted")
		return
	}
	fail(w, http.StatusServiceUnavailable, "archive_unavailable")
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": publicMessages[code]})
}

var publicMessages = map[string]string{
	"local_ai_unavailable": "Local AI is unavailable. Keyword search remains usable.",
	"local_ai_conflict":    "Local AI settings changed. Review the current choice and try again.",
	"local_ai_invalid":     "Choose eligible local AI components or turn local AI off.",
	"host_denied":          "Open the private owner link on this computer.", "origin_denied": "Open the owner interface directly to make this change.",
	"unauthorized": "The owner session has ended. Open the app again to request a fresh private owner window.", "csrf_denied": "Open the app again to request a fresh private owner window.",
	"invalid_request": "Check the request and try again.", "request_too_large": "The request is too large. Use a smaller selection.",
	"method_denied": "This action is unavailable with this request.", "route_unavailable": "This page is unavailable.",
	"interface_unavailable": "Restart the app to reopen the interface.", "owner_auth_unavailable": "Restart the app to open an owner session.",
	"owner_attach_unavailable":     "The running app could not verify this launch. Restart the app and try again.",
	"owner_attach_limit":           "Too many owner windows were requested. Wait briefly and try again.",
	"model_management_unavailable": "Model management is unavailable in this app. Keyword search remains available.",
	"model_operation_busy":         "A model task is already running. Wait for it to finish or cancel it.",
	"model_profile_unknown":        "This model profile is unavailable. Reopen Models and media.",
	"model_not_installed":          "This model is not installed. Download the pinned profile first.",
	"model_hash_mismatch":          "Model verification failed. Remove and download the pinned profile again.",
	"model_size_mismatch":          "The model is incomplete. Retry its download or remove it.",
	"model_platform_unavailable":   "This model does not support this computer.",
	"model_offline_required":       "This profile requires a verified offline import. A download is unavailable.",
	"model_active":                 "This model is in use. Pause its work before removing it.",
	"model_operation_failed":       "The model task did not finish. Check its state and try again.",
	"confirmation_required":        "Type DELETE LOCAL DATA to confirm deletion.", "setup_required": "Choose an import date and chat policy before linking WhatsApp.",
	"invalid_policy": "Choose one import date range and one chat policy.", "policy_changed": "The archive settings changed. Reload them before saving.",
	"invalid_query": "Use a shorter search or adjust its filters.", "cursor_stale": "The archive changed. Run the search again.",
	"reconciliation_required": "Import needs attention. Search is paused until the archive is safe to read.",
	"source_unavailable":      "This message or pairing code is no longer available.", "archive_unavailable": "The archive is unavailable. Try again or restart the app.",
	"fts5_unavailable": "Keyword search is unavailable in this build.", "request_interrupted": "The request stopped. Try again.",
}
