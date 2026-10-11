package localui

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const remoteAuthBodyBytes = 4096

func remoteDecode(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, remoteAuthBodyBytes)
	return decode(w, r, into)
}

func (s *RemoteServer) login(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
	}
	if !remoteDecode(w, r, &request) {
		return
	}
	if len(request.Password) == 0 || len(request.Password) > 1024 || !utf8.ValidString(request.Password) || strings.ContainsRune(request.Password, 0) {
		remoteFail(w, 400, "invalid_request")
		return
	}
	// The throttle is global; untrusted tunnel/forwarded client address headers
	// cannot buy extra attempts. No request waits in a KDF queue.
	s.mu.Lock()
	now := s.clock.nowLocked()
	s.expireSessionsLocked(now)
	if s.closed {
		s.mu.Unlock()
		remoteFail(w, 503, "auth_unavailable")
		return
	}
	if s.loginWindowStart.IsZero() || !now.Before(s.loginWindowStart.Add(remoteLoginWindow)) {
		s.loginWindowStart, s.loginAttempts = now, 0
	}
	if s.loginAttempts >= remoteLoginBurst || len(s.sessions) >= remoteSessionLimit {
		retry := s.loginWindowStart.Add(remoteLoginWindow).Sub(now)
		s.mu.Unlock()
		remoteThrottled(w, retry)
		return
	}
	s.loginAttempts++
	s.mu.Unlock()
	select {
	case s.kdfSlot <- struct{}{}:
		defer func() { <-s.kdfSlot }()
	default:
		remoteThrottled(w, time.Second)
		return
	}
	if r.Context().Err() != nil {
		remoteFail(w, 503, "request_interrupted")
		return
	}
	matched := s.password.matches(request.Password)
	request.Password = ""
	if r.Context().Err() != nil {
		remoteFail(w, 503, "request_interrupted")
		return
	}
	if !matched {
		remoteFail(w, 401, "unauthorized")
		return
	}
	token, tokenErr := randomToken()
	csrf, csrfErr := randomToken()
	if tokenErr != nil || csrfErr != nil {
		remoteFail(w, 503, "auth_unavailable")
		return
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	now = s.clock.nowLocked()
	s.expireSessionsLocked(now)
	if s.closed {
		s.mu.Unlock()
		remoteFail(w, 503, "auth_unavailable")
		return
	}
	if len(s.sessions) >= remoteSessionLimit {
		s.mu.Unlock()
		remoteThrottled(w, remoteLoginWindow)
		return
	}
	expires := now.Add(s.sessionTTL)
	s.sessions[digest] = session{digest: digest, csrf: csrf, expires: expires}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: remoteOwnerCookie, Value: token, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int((s.sessionTTL + time.Second - 1) / time.Second), Expires: expires})
	respond(w, map[string]string{"csrf_token": csrf})
}

func (s *RemoteServer) authenticated(r *http.Request) (string, [32]byte, bool) {
	var none [32]byte
	// One Cookie header and one owner cookie prevent parser ambiguity between
	// gateways and Go. Unrelated browser cookies may share the single header.
	if len(r.Header.Values("Cookie")) != 1 {
		return "", none, false
	}
	var token string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == remoteOwnerCookie {
			count++
			token = cookie.Value
		}
	}
	if count != 1 {
		return "", none, false
	}
	if _, ok := canonicalToken(token); !ok {
		return "", none, false
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.nowLocked()
	s.expireSessionsLocked(now)
	owner, ok := s.sessions[digest]
	if s.closed || !ok || !now.Before(owner.expires) {
		return "", none, false
	}
	return owner.csrf, digest, true
}

func (s *RemoteServer) expireSessionsLocked(now time.Time) {
	for digest, owner := range s.sessions {
		if !now.Before(owner.expires) {
			delete(s.sessions, digest)
		}
	}
}

func (s *RemoteServer) restoreSession(w http.ResponseWriter, r *http.Request) {
	csrf, _, ok := s.authenticated(r)
	if !ok {
		remoteFail(w, 401, "unauthorized")
		return
	}
	var request struct{}
	if !remoteDecode(w, r, &request) {
		return
	}
	respond(w, map[string]string{"csrf_token": csrf})
}

func (s *RemoteServer) logout(w http.ResponseWriter, r *http.Request) {
	csrf, digest, ok := s.authenticated(r)
	if !ok {
		remoteFail(w, 401, "unauthorized")
		return
	}
	if len(r.Header.Values("X-CSRF-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
		remoteFail(w, 403, "csrf_denied")
		return
	}
	var request struct{}
	if !remoteDecode(w, r, &request) {
		return
	}
	s.mu.Lock()
	delete(s.sessions, digest)
	s.mu.Unlock()
	s.clearCookie(w)
	respond(w, map[string]bool{"ok": true})
}

func (s *RemoteServer) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: remoteOwnerCookie, Value: "", Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}

func remoteThrottled(w http.ResponseWriter, retry time.Duration) {
	seconds := int((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 60 {
		seconds = 60
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	remoteFail(w, 429, "login_throttled")
}

func remoteFail(w http.ResponseWriter, status int, code string) {
	message := publicMessages[code]
	switch code {
	case "unauthorized":
		message = "Sign in to access this archive."
	case "csrf_denied":
		message = "Sign in again to continue."
	case "host_denied":
		message = "Open the configured archive address."
	case "origin_denied":
		message = "Open the archive directly to make this change."
	case "login_throttled":
		message = "Too many sign-in attempts. Wait briefly and try again."
	case "auth_unavailable":
		message = "Sign-in is unavailable. Restart the app and try again."
	}
	if message == "" {
		message = "This request is unavailable."
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
