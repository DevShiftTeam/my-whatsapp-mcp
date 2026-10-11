package localui

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A browser uses one host-wide cookie jar even when these local owner servers
// have distinct ports. Exercise that behavior rather than hand-picking a cookie.
func TestOwnerWindowsKeepSeparateLocalPortCookies(t *testing.T) {
	first, _ := attachedTestServer(t, false)
	second, _ := attachedTestServer(t, false)
	firstSession := ownerWindowLogin(t, first, lifecycleBootstrapToken(t, first.BootstrapURL()))
	secondSession := ownerWindowLogin(t, second, lifecycleBootstrapToken(t, second.BootstrapURL()))
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("browser cookie jar unavailable")
	}
	for _, item := range []struct {
		owner   *Server
		session testSession
	}{{first, firstSession}, {second, secondSession}} {
		origin, _ := url.Parse(item.owner.origin)
		jar.SetCookies(origin, []*http.Cookie{item.session.cookie})
	}
	for _, item := range []struct {
		owner   *Server
		session testSession
	}{{first, firstSession}, {second, secondSession}} {
		origin, _ := url.Parse(item.owner.origin)
		if item.session.cookie.Name != "owner_session_"+origin.Port() {
			t.Error("owner cookie does not include its canonical local port")
		}
		r := httptest.NewRequest(http.MethodGet, item.owner.origin+"/api/session", nil)
		for _, cookie := range jar.Cookies(origin) {
			r.AddCookie(cookie)
		}
		r.Header.Set("X-CSRF-Token", item.session.csrf)
		rr := httptest.NewRecorder()
		item.owner.Handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Errorf("opening another local port invalidated an owner window: status %d", rr.Code)
		}
	}
}

func TestOwnerWindowsReuseUnexpiredCredentialsWithoutExtendingExpiry(t *testing.T) {
	owner, _ := attachedTestServer(t, false)
	first := ownerWindowLogin(t, owner, lifecycleBootstrapToken(t, owner.BootstrapURL()))
	owner.mu.Lock()
	originalExpiry := owner.owner.expires
	owner.clock.last = owner.clock.nowLocked().Add(time.Second)
	owner.mu.Unlock()
	secondToken := lifecycleMintAttachedBootstrap(t, owner.AttachURL())
	second := ownerWindowLogin(t, owner, secondToken)
	if first.cookie.Value != second.cookie.Value || first.csrf != second.csrf {
		t.Error("reopening owner window rotated credentials used by another tab")
	}
	owner.mu.Lock()
	expiry := owner.owner.expires
	owner.mu.Unlock()
	if !expiry.Equal(originalExpiry) || second.cookie.MaxAge >= first.cookie.MaxAge || second.cookie.Expires.After(first.cookie.Expires) {
		t.Error("reopening owner window extended the original session expiry")
	}
	for _, session := range []testSession{first, second, {cookie: second.cookie, csrf: first.csrf}} {
		if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", session).Code != http.StatusOK {
			t.Error("previous or new owner tab lost authorization after reopening")
		}
	}
	if replay := consumeAttachedBootstrap(owner, secondToken); replay.Code != http.StatusUnauthorized || len(replay.Result().Cookies()) != 0 {
		t.Error("reused owner credentials made the bootstrap reusable")
	}
	for _, csrf := range []string{"", "incorrect-owner-capability"} {
		captured := testSession{cookie: second.cookie, csrf: csrf}
		if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", captured).Code != http.StatusForbidden {
			t.Error("reopening permitted cookie-only or wrong-CSRF capability recovery")
		}
	}
	owner.mu.Lock()
	owner.clock.last = originalExpiry
	owner.mu.Unlock()
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", second).Code != http.StatusUnauthorized {
		t.Error("reopened owner window outlived the original session deadline")
	}
}

func TestOwnerWindowsRenewExpiredCredentialsAndRetainExplicitRevocation(t *testing.T) {
	owner := newTestServer(t, &fakeApplication{}, Config{Origin: testOrigin, SessionTTL: time.Second})
	first := ownerWindowLogin(t, owner, lifecycleBootstrapToken(t, owner.BootstrapURL()))
	freshToken := ownerWindowAttachedToken(t, owner, 1)
	pending := ownerWindowAttachedToken(t, owner, 2)
	owner.mu.Lock()
	owner.clock.last = owner.owner.expires
	owner.mu.Unlock()
	second := ownerWindowLogin(t, owner, freshToken)
	if first.cookie.Value == second.cookie.Value || first.csrf == second.csrf {
		t.Error("new owner window revived expired credentials")
	}
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", first).Code != http.StatusUnauthorized {
		t.Error("expired credentials recovered access after a new owner opened")
	}
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", second).Code != http.StatusOK {
		t.Error("valid fresh bootstrap did not establish a new session after expiry")
	}
	owner.RevokeSessions()
	if lifecycleOwnerRequest(owner, http.MethodGet, "/api/session", "", second).Code != http.StatusUnauthorized {
		t.Error("explicit revocation left reopened owner credentials active")
	}
	if replay := consumeAttachedBootstrap(owner, pending); replay.Code != http.StatusUnauthorized || len(replay.Result().Cookies()) != 0 {
		t.Error("explicit revocation left a pending fresh-window bootstrap active")
	}
}

func ownerWindowAttachedToken(t *testing.T, owner *Server, nonceID byte) string {
	t.Helper()
	nonce := make([]byte, 32)
	nonce[0] = nonceID
	attached := attachTestHTTP(owner, attachTestRequest(t, owner, false, base64.RawURLEncoding.EncodeToString(nonce), strconv.FormatInt(time.Now().Unix()+5, 10)))
	var attachment struct {
		Token string `json:"bootstrap_token"`
	}
	if attached.Code != http.StatusOK || json.Unmarshal(attached.Body.Bytes(), &attachment) != nil || attachment.Token == "" {
		t.Fatal("could not stage a pending bootstrap before owner expiry")
	}
	return attachment.Token
}

func ownerWindowLogin(t *testing.T, owner *Server, token string) testSession {
	t.Helper()
	reply := consumeAttachedBootstrap(owner, token)
	var fields struct {
		CSRF string `json:"csrf_token"`
	}
	cookies := reply.Result().Cookies()
	if reply.Code != http.StatusOK || len(cookies) != 1 || json.Unmarshal(reply.Body.Bytes(), &fields) != nil || fields.CSRF == "" {
		t.Fatal("valid owner bootstrap has no usable credentials")
	}
	if cookies[0].Value == "" || cookies[0].Path != "/" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || strings.Contains(reply.Body.String(), cookies[0].Value) {
		t.Fatal("owner bootstrap weakened cookie protection or disclosed its token")
	}
	return testSession{cookie: cookies[0], csrf: fields.CSRF}
}
