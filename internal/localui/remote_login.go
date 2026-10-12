package localui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
)

const remoteCookie = "__Host-wa_managed_owner"
const challengeCookie = "__Host-wa_managed_challenge"

type ManagedRemoteConfig struct {
	Context       context.Context
	Origin        string
	Generation    uint64
	Authenticator *localauth.Authenticator
	SessionTTL    time.Duration
}
type remoteSession struct {
	session
	generation uint64
}
type loginChallenge struct {
	nonce      [32]byte
	generation uint64
	expires    time.Time
}
type remoteRealm struct {
	ctx        context.Context
	auth       *localauth.Authenticator
	generation uint64
	revoked    bool
	sessions   map[[32]byte]remoteSession
	challenges map[[32]byte]loginChallenge
}

// NewRemote creates no attach or bootstrap capability. The app/provider context
// fences this realm; password generation independently fences every login.
func NewManagedRemote(cfg ManagedRemoteConfig, app Application) (*Server, error) {
	origin, err := remoteaccess.PinOrigin("", cfg.Origin)
	if err != nil || origin != cfg.Origin || app == nil || cfg.Context == nil || cfg.Context.Err() != nil || cfg.Generation == 0 || cfg.Authenticator == nil || cfg.Authenticator.Generation() == 0 {
		return nil, errors.New("remote_owner_unavailable")
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.SessionTTL < time.Second || cfg.SessionTTL > 24*time.Hour {
		return nil, errors.New("invalid_session_duration")
	}
	u, _ := url.Parse(origin)
	now := time.Now()
	return &Server{realm: realmManaged, app: app, origin: origin, host: u.Host, sessionTTL: cfg.SessionTTL, attachRevoked: true, clock: ownerClock{base: now, last: now, wall: time.Now, elapsed: func() time.Duration { return time.Since(now) }}, remote: &remoteRealm{ctx: cfg.Context, auth: cfg.Authenticator, generation: cfg.Generation, sessions: make(map[[32]byte]remoteSession), challenges: make(map[[32]byte]loginChallenge)}}, nil
}

// Explicit principal capabilities: new native routes cannot become remote APIs.
var remoteProtected = map[string]bool{
	"/api/session": true, "/api/setup": true, "/api/policy": true,
	"/api/pairing/start": true, "/api/pairing": true, "/api/pairing/qr": true,
	"/api/status": true, "/api/chats": true, "/api/search": true, "/api/context": true,
	"/api/pause": true, "/api/resume": true, "/api/disconnect": true,
	"/api/models": true, "/api/models/state": true, "/api/models/operations": true, "/api/models/operations/cancel": true,
	"/api/local-ai": true, "/api/local-ai/select": true,
	"/api/connections": true, "/api/connections/create": true, "/api/connections/guide": true, "/api/connections/revoke": true,
	"/api/original-budget": true, "/api/remote/logout": true,
	"/api/stats": true, "/api/voice/retry": true, "/api/documents/retry": true, "/api/connections/send-permission": true,
}

func (s *Server) interfaceMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "method_denied")
		return
	}
	if !bodyFree(r) || r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, 400, "invalid_request")
		return
	}
	realm := ""
	switch s.realm {
	case realmNative:
		realm = "local"
	case realmLegacy:
		realm = "legacy"
	case realmManaged:
		realm = "remote"
	default:
		fail(w, 503, "interface_unavailable")
		return
	}
	respond(w, map[string]string{"realm": realm})
}
func bodyFree(r *http.Request) bool {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return false
	}
	if r.Body != nil {
		var b [1]byte
		n, err := r.Body.Read(b[:])
		return n == 0 && err == io.EOF
	}
	return true
}
func boundedRemoteHeaders(r *http.Request) bool {
	n := len(r.Host) + len(r.RequestURI)
	for k, values := range r.Header {
		n += len(k)
		for _, v := range values {
			n += len(v)
			if n > 16384 {
				return false
			}
		}
	}
	return n <= 16384
}
func (s *Server) handleRemote(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	if !boundedRemoteHeaders(r) {
		fail(w, 431, "invalid_request")
		return
	}
	if r.Host != s.host || r.URL.User != nil || r.URL.Opaque != "" || (r.URL.Host != "" && r.URL.Host != s.host) || (r.URL.Scheme != "" && r.URL.Scheme != "https") {
		fail(w, 403, "host_denied")
		return
	}
	// Forwarding metadata is never an authority source. Conflicts fail closed.
	if len(r.Header.Values("Forwarded")) != 0 || len(r.Header.Values("X-Forwarded-Host")) > 1 || (r.Header.Get("X-Forwarded-Host") != "" && r.Header.Get("X-Forwarded-Host") != s.host) || len(r.Header.Values("X-Forwarded-Proto")) > 1 || (r.Header.Get("X-Forwarded-Proto") != "" && r.Header.Get("X-Forwarded-Proto") != "https") {
		fail(w, 403, "host_denied")
		return
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (len(origins) == 1 && origins[0] != s.origin) {
		fail(w, 403, "origin_denied")
		return
	}
	if r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path || r.URL.Fragment != "" || r.URL.ForceQuery || r.URL.RawQuery != "" && r.URL.Path != "/api/chats" {
		fail(w, 400, "invalid_request")
		return
	}
	path := r.URL.Path
	if path == "/api/interface" {
		s.interfaceMode(w, r)
		return
	}
	if path == "/" || path == "/assets/app.css" || path == "/assets/app.js" || path == "/assets/theme.js" || path == "/assets/selects.js" || path == "/assets/logo.png" {
		if r.Method != "GET" {
			fail(w, 405, "method_denied")
			return
		}
		if !bodyFree(r) {
			fail(w, 400, "invalid_request")
			return
		}
		s.static(w, r)
		return
	}
	if path != "/api/remote/login" && !remoteProtected[path] {
		fail(w, 404, "route_unavailable")
		return
	}
	if path == "/api/remote/login" {
		s.remoteLogin(w, r)
		return
	}
	csrf, ok := s.remoteAuthenticated(r)
	if !ok {
		fail(w, 401, "unauthorized")
		return
	}
	if len(r.Header.Values("X-CSRF-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
		fail(w, 403, "csrf_denied")
		return
	}
	method := routeMethods[path]
	if path == "/api/remote/logout" {
		method = "POST"
	}
	if path == "/api/original-budget" && r.Method == "PUT" {
		method = "PUT"
	}
	if r.Method != method {
		fail(w, 405, "method_denied")
		return
	}
	if method == "GET" {
		if !bodyFree(r) {
			fail(w, 400, "invalid_request")
			return
		}
	} else if len(origins) != 1 {
		fail(w, 403, "origin_denied")
		return
	}
	if path == "/api/remote/logout" {
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
		key, _ := cookieDigest(r, remoteCookie)
		s.mu.Lock()
		delete(s.remote.sessions, key)
		s.mu.Unlock()
		setRemoteCookie(w, remoteCookie, "", -1)
		respond(w, map[string]bool{"ok": true})
		return
	}
	s.api(w, r, csrf)
}
func cookieDigest(r *http.Request, name string) ([32]byte, bool) {
	value := ""
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			count++
			value = c.Value
		}
	}
	if count != 1 || len(value) != 43 {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(value)), true
}
func (s *Server) expireRemoteLocked() uint64 {
	generation := s.remote.auth.Generation()
	now := s.clock.nowLocked()
	if s.remote.revoked || s.remote.ctx.Err() != nil {
		generation = 0
	}
	for key, v := range s.remote.sessions {
		if generation == 0 || v.generation != generation || !now.Before(v.expires) {
			delete(s.remote.sessions, key)
		}
	}
	for key, v := range s.remote.challenges {
		if generation == 0 || v.generation != generation || !now.Before(v.expires) {
			delete(s.remote.challenges, key)
		}
	}
	return generation
}
func (s *Server) remoteAuthenticated(r *http.Request) (string, bool) {
	key, ok := cookieDigest(r, remoteCookie)
	if !ok {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireRemoteLocked()
	v, ok := s.remote.sessions[key]
	return v.csrf, ok
}
func setRemoteCookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: seconds})
}
func (s *Server) remoteLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		fail(w, 405, "method_denied")
		return
	}
	if r.Method == "GET" {
		if !bodyFree(r) {
			fail(w, 400, "invalid_request")
			return
		}
		cookie, e := randomToken()
		nonce, e2 := randomToken()
		if e != nil || e2 != nil {
			fail(w, 503, "owner_auth_unavailable")
			return
		}
		s.mu.Lock()
		generation := s.expireRemoteLocked()
		if old, ok := cookieDigest(r, challengeCookie); ok {
			delete(s.remote.challenges, old)
		}
		if generation == 0 || len(s.remote.challenges) >= 64 {
			s.mu.Unlock()
			fail(w, 429, "owner_attach_limit")
			return
		}
		s.remote.challenges[sha256.Sum256([]byte(cookie))] = loginChallenge{nonce: sha256.Sum256([]byte(nonce)), generation: generation, expires: s.clock.nowLocked().Add(30 * time.Second)}
		s.mu.Unlock()
		setRemoteCookie(w, challengeCookie, cookie, 30)
		respond(w, map[string]string{"challenge": nonce})
		return
	}
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != s.origin {
		fail(w, 403, "origin_denied")
		return
	}
	password, nonce, ok := decodePassword(w, r, true)
	if !ok {
		return
	}
	defer clear(password)
	key, ok := cookieDigest(r, challengeCookie)
	s.mu.Lock()
	s.expireRemoteLocked()
	challenge, exists := s.remote.challenges[key]
	delete(s.remote.challenges, key)
	s.mu.Unlock()
	setRemoteCookie(w, challengeCookie, "", -1)
	digest := sha256.Sum256([]byte(nonce))
	if !ok || !exists || subtle.ConstantTimeCompare(digest[:], challenge.nonce[:]) != 1 {
		fail(w, 401, "unauthorized")
		return
	}
	verified, err := s.remote.auth.Verify(r.Context(), password, challenge.generation)
	if err != nil || !verified {
		if errors.Is(err, localauth.ErrBusy) {
			fail(w, 429, "owner_attach_limit")
		} else {
			fail(w, 401, "unauthorized")
		}
		return
	}
	token, e := randomToken()
	csrf, e2 := randomToken()
	if e != nil || e2 != nil {
		fail(w, 503, "owner_auth_unavailable")
		return
	}
	s.mu.Lock()
	generation := s.expireRemoteLocked()
	old, hasCookie := cookieDigest(r, remoteCookie)
	_, replacing := s.remote.sessions[old]
	replacing = hasCookie && replacing
	// A live presented session already occupies a slot. Successful re-login
	// replaces only that entry; arbitrary cookies cannot bypass the bound.
	if generation != challenge.generation || r.Context().Err() != nil || len(s.remote.sessions) >= 16 && !replacing {
		s.mu.Unlock()
		fail(w, 401, "unauthorized")
		return
	}
	if replacing {
		delete(s.remote.sessions, old)
	}
	s.remote.sessions[sha256.Sum256([]byte(token))] = remoteSession{session: session{digest: sha256.Sum256([]byte(token)), csrf: csrf, expires: s.clock.nowLocked().Add(s.sessionTTL)}, generation: generation}
	s.mu.Unlock()
	setRemoteCookie(w, remoteCookie, token, int(s.sessionTTL.Seconds()))
	respond(w, map[string]string{"csrf_token": csrf})
}

// Dedicated password decoder: no password-bearing exported/generic DTO exists.
// Bound raw bytes, exact keys and Unicode; clear raw/intermediate byte buffers.
func decodePassword(w http.ResponseWriter, r *http.Request, login bool) ([]byte, string, bool) {
	if len(r.Header.Values("Content-Type")) != 1 {
		fail(w, 415, "invalid_request")
		return nil, "", false
	}
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		fail(w, 415, "invalid_request")
		return nil, "", false
	}
	data, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
	defer clear(data)
	if e != nil || !utf8.Valid(data) || !uniqueFields(data) {
		fail(w, 400, "invalid_request")
		return nil, "", false
	}
	var fields map[string]json.RawMessage
	expectedKeys := 1
	if login {
		expectedKeys = 2
	}
	if json.Unmarshal(data, &fields) != nil || len(fields) != expectedKeys {
		fail(w, 400, "invalid_request")
		return nil, "", false
	}
	defer func() {
		for _, v := range fields {
			clear(v)
		}
	}()
	raw, ok := fields["password"]
	var value string
	if !ok || !validJSONString(raw) || json.Unmarshal(raw, &value) != nil {
		fail(w, 400, "invalid_request")
		return nil, "", false
	}
	password := []byte(value)
	value = ""
	nonce := ""
	if login {
		raw, ok = fields["challenge"]
		if !ok || json.Unmarshal(raw, &nonce) != nil || len(nonce) != 43 {
			clear(password)
			fail(w, 400, "invalid_request")
			return nil, "", false
		}
	}
	return password, nonce, true
}
func validJSONString(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw)-1 {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		v, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if v >= 0xdc00 && v <= 0xdfff {
			return false
		}
		if v >= 0xd800 && v <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// OwnerPasswordApplication is optional and is used only after native owner+CSRF.
type OwnerPasswordApplication interface {
	SetRemoteOwnerPassword(context.Context, []byte) error
}

func (s *Server) setOwnerPassword(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if s.realm != realmNative {
		fail(w, 404, "route_unavailable")
		return
	}
	app, ok := s.app.(OwnerPasswordApplication)
	if !ok {
		fail(w, 503, "owner_auth_unavailable")
		return
	}
	password, _, ok := decodePassword(w, r, false)
	if !ok {
		return
	}
	defer clear(password)
	if e := app.SetRemoteOwnerPassword(ctx, password); e != nil {
		switch {
		case errors.Is(e, localauth.ErrBusy), errors.Is(e, remoteaccess.ErrBusy):
			fail(w, 409, "owner_auth_busy")
		case errors.Is(e, localauth.ErrPassword):
			fail(w, 400, "invalid_request")
		default:
			fail(w, 503, "owner_auth_unavailable")
		}
		return
	}
	respond(w, map[string]bool{"ok": true})
}
