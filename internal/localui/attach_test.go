package localui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests are source-authored during the root's Go execution hold. No server is
// launched until the root grants the serialized validation lane.
func attachedTestServer(t *testing.T, demo bool) (*Server, *httptest.Server) {
	t.Helper()
	var owner *Server
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { owner.Handler().ServeHTTP(w, r) }))
	var err error
	owner, err = New(Config{Origin: "http://" + peer.Listener.Addr().String(), Demo: demo}, &fakeApplication{})
	if err != nil {
		t.Fatal("owner construction failed")
	}
	peer.Start()
	t.Cleanup(peer.Close)
	return owner, peer
}

func independentAttachMAC(key []byte, fields ...string) string {
	mac := hmac.New(sha256.New, key)
	for _, field := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(field))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func independentAttachReply(r *http.Request, key []byte, instance string) []byte {
	var request struct {
		Nonce   string `json:"nonce"`
		Demo    bool   `json:"demo"`
		Expires string `json:"expires_unix"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		return nil
	}
	tokenBytes := make([]byte, 32)
	tokenBytes[0] = 1
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expiry := strconv.FormatInt(time.Now().Unix()+30, 10)
	mode := "0"
	if request.Demo {
		mode = "1"
	}
	reply := map[string]any{"instance": instance, "nonce": request.Nonce, "demo": request.Demo,
		"bootstrap_token": token, "expires_unix": expiry,
		"proof": independentAttachMAC(key, "wa-indexer-owner-attach/response/v1", r.Header.Get("Origin"), instance, request.Nonce, mode, request.Expires, token, expiry)}
	data, _ := json.Marshal(reply)
	return data
}

func attachTestRequest(t *testing.T, s *Server, demo bool, nonce, expiry string) map[string]any {
	t.Helper()
	u, err := url.Parse(s.AttachURL())
	if err != nil {
		t.Fatal("private capability invalid")
	}
	parts := strings.Split(strings.TrimPrefix(u.Fragment, "attach="), ".")
	if len(parts) != 2 {
		t.Fatal("private capability format invalid")
	}
	key, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal("private capability encoding invalid")
	}
	mode := "0"
	if demo {
		mode = "1"
	}
	proof := independentAttachMAC(key, "wa-indexer-owner-attach/request/v1", s.origin, parts[0], nonce, mode, expiry)
	return map[string]any{"instance": parts[0], "nonce": nonce, "demo": demo, "expires_unix": expiry, "proof": proof}
}

func attachTestHTTP(s *Server, payload any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	r := httptest.NewRequest(http.MethodPost, s.origin+"/api/owner/attach", strings.NewReader(string(body)))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, r)
	return rr
}

func consumeAttachedBootstrap(s *Server, token string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"token": token})
	r := httptest.NewRequest(http.MethodPost, s.origin+"/api/bootstrap", strings.NewReader(string(body)))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, r)
	return rr
}

func TestFreshAttachReturnsOneUseBootstrapAndPreservesOwnerSession(t *testing.T) {
	s, _ := attachedTestServer(t, true)
	first, err := Attach(context.Background(), s.AttachURL(), true)
	if err != nil {
		t.Fatal("fresh attach failed")
	}
	u, _ := url.Parse(first)
	firstToken := strings.TrimPrefix(u.Fragment, "bootstrap=")
	firstSession := consumeAttachedBootstrap(s, firstToken)
	if firstSession.Code != 200 {
		t.Fatal("attached bootstrap rejected")
	}
	probe := func(rr *httptest.ResponseRecorder) int {
		r := httptest.NewRequest(http.MethodGet, s.origin+"/api/session", nil)
		r.AddCookie(rr.Result().Cookies()[0])
		var reply struct {
			CSRF string `json:"csrf_token"`
		}
		if json.Unmarshal(rr.Body.Bytes(), &reply) != nil {
			t.Fatal("session reply invalid")
		}
		r.Header.Set("X-CSRF-Token", reply.CSRF)
		result := httptest.NewRecorder()
		s.Handler().ServeHTTP(result, r)
		return result.Code
	}
	if probe(firstSession) != 200 {
		t.Fatal("first owner session was never usable")
	}
	if consumeAttachedBootstrap(s, firstToken).Code != 401 {
		t.Fatal("attached bootstrap replay accepted")
	}
	second, err := Attach(context.Background(), s.AttachURL(), true)
	if err != nil || second == first {
		t.Fatal("reopening did not produce a fresh bootstrap")
	}
	u, _ = url.Parse(second)
	secondSession := consumeAttachedBootstrap(s, strings.TrimPrefix(u.Fragment, "bootstrap="))
	if secondSession.Code != 200 {
		t.Fatal("replacement bootstrap rejected")
	}
	if probe(secondSession) != 200 {
		t.Fatal("replacement owner session was not usable")
	}
	// Both tabs retain the same unexpired owner credentials. Reopening with a
	// fresh one-use bootstrap must leave the previous tab usable as well.
	mixed := httptest.NewRequest(http.MethodGet, s.origin+"/api/session", nil)
	mixed.AddCookie(secondSession.Result().Cookies()[0])
	var firstFields map[string]string
	if json.Unmarshal(firstSession.Body.Bytes(), &firstFields) != nil {
		t.Fatal("first owner reply invalid")
	}
	mixed.Header.Set("X-CSRF-Token", firstFields["csrf_token"])
	mixedReply := httptest.NewRecorder()
	s.Handler().ServeHTTP(mixedReply, mixed)
	if mixedReply.Code != 200 {
		t.Fatal("reopening invalidated an existing owner's capability")
	}
	request := httptest.NewRequest(http.MethodGet, s.origin+"/api/session", nil)
	request.AddCookie(firstSession.Result().Cookies()[0])
	var sessionReply struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(firstSession.Body.Bytes(), &sessionReply)
	request.Header.Set("X-CSRF-Token", sessionReply.CSRF)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, request)
	if rr.Code != 200 {
		t.Fatal("previous owner session lost access after reopening")
	}
}

func TestAttachRejectsModeTamperingReplayAndExpiredProofAfterNonceEviction(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	nonce := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	expiry := strconv.FormatInt(time.Now().Unix()+5, 10)
	good := attachTestRequest(t, s, false, nonce, expiry)
	tampered := attachTestRequest(t, s, false, nonce, expiry)
	tampered["demo"] = true
	if attachTestHTTP(s, tampered).Code != 401 {
		t.Fatal("mode tampering accepted")
	}
	if attachTestHTTP(s, good).Code != 200 {
		t.Fatal("valid proof rejected")
	}
	if attachTestHTTP(s, good).Code != 401 {
		t.Fatal("nonce replay accepted")
	}
	s.mu.Lock()
	s.clock.last = time.Now().Add(61 * time.Second)
	s.mu.Unlock()
	if attachTestHTTP(s, good).Code != 401 {
		t.Fatal("old signed request revived after nonce eviction")
	}
}

func TestAttachRevocationBurnsPersistentCapabilityAndPendingBootstrap(t *testing.T) {
	s, _ := attachedTestServer(t, false)
	bootstrap, err := Attach(context.Background(), s.AttachURL(), false)
	if err != nil {
		t.Fatal("attach failed")
	}
	s.RevokeSessions()
	if _, err = Attach(context.Background(), s.AttachURL(), false); err == nil {
		t.Fatal("revoked attach capability minted a bootstrap")
	}
	u, _ := url.Parse(bootstrap)
	if consumeAttachedBootstrap(s, strings.TrimPrefix(u.Fragment, "bootstrap=")).Code != 401 {
		t.Fatal("revoked pending bootstrap accepted")
	}
}

func TestAttachAdmissionBoundsDoNotEvictLivePendingCapabilitiesOrNonces(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	s.mu.Lock()
	s.bootstraps = make(map[[32]byte]time.Time)
	s.mu.Unlock()
	var oldestToken string
	var oldestRequest map[string]any
	for i := 0; i < 8; i++ {
		nonceBytes := make([]byte, 32)
		nonceBytes[0] = byte(i)
		payload := attachTestRequest(t, s, false, base64.RawURLEncoding.EncodeToString(nonceBytes), strconv.FormatInt(time.Now().Unix()+5, 10))
		rr := attachTestHTTP(s, payload)
		if rr.Code != 200 {
			t.Fatal("pending capacity rejected early")
		}
		if i == 0 {
			var reply struct {
				Token string `json:"bootstrap_token"`
			}
			if json.Unmarshal(rr.Body.Bytes(), &reply) != nil {
				t.Fatal("attach response invalid")
			}
			oldestToken, oldestRequest = reply.Token, payload
		}
	}
	nonceBytes := make([]byte, 32)
	nonceBytes[0] = 9
	if attachTestHTTP(s, attachTestRequest(t, s, false, base64.RawURLEncoding.EncodeToString(nonceBytes), strconv.FormatInt(time.Now().Unix()+5, 10))).Code != 429 {
		t.Fatal("pending capacity overflow accepted")
	}
	if consumeAttachedBootstrap(s, oldestToken).Code != 200 {
		t.Fatal("pending overflow evicted a live bootstrap")
	}
	if attachTestHTTP(s, oldestRequest).Code != 401 {
		t.Fatal("pending overflow evicted a live replay guard")
	}
	s.mu.Lock()
	s.bootstraps = make(map[[32]byte]time.Time)
	s.nonces = make(map[string]time.Time)
	for i := 0; i < 64; i++ {
		s.nonces[strconv.Itoa(i)] = time.Now().Add(time.Minute)
	}
	s.mu.Unlock()
	if attachTestHTTP(s, attachTestRequest(t, s, false, base64.RawURLEncoding.EncodeToString(nonceBytes), strconv.FormatInt(time.Now().Unix()+5, 10))).Code != 429 {
		t.Fatal("nonce capacity overflow accepted")
	}
	s.mu.Lock()
	_, oldestPresent := s.nonces["0"]
	count := len(s.nonces)
	s.mu.Unlock()
	if !oldestPresent || count != 64 {
		t.Fatal("nonce overflow evicted a live replay guard")
	}
}

func TestStalePortNeverReceivesAttachSecretAndCannotSupplyUnverifiedBrowserURL(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var leaked atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		leaked.Store(strings.Contains(string(body), key) || strings.Contains(r.RequestURI, key))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance":"00000000000000000000000000000000","demo":false,"nonce":"bogus","bootstrap_token":"bogus","expires_unix":"1","proof":"bogus"}`))
	}))
	defer peer.Close()
	if _, err := Attach(context.Background(), peer.URL+"/#attach="+strings.Repeat("0", 32)+"."+key, false); err == nil {
		t.Fatal("stale port supplied browser authority")
	}
	if leaked.Load() {
		t.Fatal("helper sent raw attach secret to stale port")
	}
}

func TestAttachRouteRequiresExactAuthorityAndStrictBoundedJSON(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	payload := attachTestRequest(t, s, false, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), strconv.FormatInt(time.Now().Unix()+5, 10))
	body, _ := json.Marshal(payload)
	for _, test := range []struct {
		name   string
		status int
		change func(*http.Request)
	}{
		{"missing_origin", 403, func(r *http.Request) { r.Header.Del("Origin") }},
		{"wrong_origin", 403, func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:1") }},
		{"multiple_origins", 403, func(r *http.Request) { r.Header.Add("Origin", s.origin) }},
		{"wrong_host", 403, func(r *http.Request) { r.Host = "localhost:1" }},
		{"missing_content_type", 415, func(r *http.Request) { r.Header.Del("Content-Type") }},
		{"multiple_content_types", 415, func(r *http.Request) { r.Header.Add("Content-Type", "application/json") }},
		{"get", 405, func(r *http.Request) { r.Method = http.MethodGet }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, s.origin+"/api/owner/attach", strings.NewReader(string(body)))
			r.Header.Set("Origin", s.origin)
			r.Header.Set("Content-Type", "application/json")
			test.change(r)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != test.status {
				t.Fatal("attach authority guard returned an unexpected status")
			}
		})
	}
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"unknown", `{"unused":"private"}`, 400},
		{"duplicate_alias", `{"instance":"a","INSTANCE":"b"}`, 400},
		{"duplicate_escaped", `{"nonce":"a","\u006eonce":"b"}`, 400},
		{"non_object", `null`, 400},
		{"trailing_object", `{} {}`, 400},
		{"large", strings.Repeat(" ", maxBodyBytes+1), 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, s.origin+"/api/owner/attach", strings.NewReader(test.body))
			r.Header.Set("Origin", s.origin)
			r.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != test.status {
				t.Fatal("attach body guard returned an unexpected status")
			}
		})
	}
	// Guards must not consume the valid capability or require browser cookies.
	if attachTestHTTP(s, payload).Code != 200 {
		t.Fatal("rejected request consumed the attach nonce")
	}
}

func TestAttachHelperAuthenticatesEveryReplyFieldBeforeConstructingURL(t *testing.T) {
	key := make([]byte, 32)
	instance := strings.Repeat("0", 32)
	for _, field := range []string{"unchanged", "expired", "too_far", "instance", "nonce", "demo", "bootstrap_token", "expires_unix", "proof", "unknown", "duplicate_alias", "request_expiry", "domain"} {
		t.Run(field, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Nonce, Proof string
					ExpiresUnix  string `json:"expires_unix"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					w.WriteHeader(400)
					return
				}
				tokenBytes := make([]byte, 32)
				tokenBytes[0] = 1
				token := base64.RawURLEncoding.EncodeToString(tokenBytes)
				expiry := strconv.FormatInt(time.Now().Unix()+30, 10)
				if field == "expired" {
					expiry = strconv.FormatInt(time.Now().Unix()-1, 10)
				}
				if field == "too_far" {
					expiry = strconv.FormatInt(time.Now().Unix()+120, 10)
				}
				origin := "http://" + r.Host
				domain, requestExpiry := "wa-indexer-owner-attach/response/v1", request.ExpiresUnix
				if field == "domain" {
					domain = "wa-indexer-owner-attach/request/v1"
				}
				if field == "request_expiry" {
					requestExpiry = "1"
				}
				reply := map[string]any{"instance": instance, "nonce": request.Nonce, "demo": false, "bootstrap_token": token, "expires_unix": expiry,
					"proof": independentAttachMAC(key, domain, origin, instance, request.Nonce, "0", requestExpiry, token, expiry)}
				switch field {
				case "instance":
					reply[field] = strings.Repeat("1", 32)
				case "nonce":
					reply[field] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				case "demo":
					reply[field] = true
				case "bootstrap_token":
					reply[field] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				case "expires_unix":
					reply[field] = strconv.FormatInt(time.Now().Unix()+25, 10)
				case "proof":
					reply[field] = request.Proof
				case "unknown":
					reply["url"] = "http://example.invalid/"
				}
				data, _ := json.Marshal(reply)
				if field == "duplicate_alias" {
					data = append(data[:len(data)-1], []byte(`,"NONCE":"ignored"}`)...)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(data)
			}))
			defer peer.Close()
			privateURL := peer.URL + "/#attach=" + instance + "." + base64.RawURLEncoding.EncodeToString(key)
			value, err := Attach(context.Background(), privateURL, false)
			if field == "unchanged" {
				tokenBytes := make([]byte, 32)
				tokenBytes[0] = 1
				if err != nil || value != peer.URL+"/#bootstrap="+base64.RawURLEncoding.EncodeToString(tokenBytes) {
					t.Fatal("independently signed valid response was rejected")
				}
			} else if err != ErrOwnerAttach || value != "" {
				t.Fatal("modified reply supplied browser authority")
			}
		})
	}
}

func TestAttachHelperRejectsUnsafeURLsRedirectsLargeRepliesAndWrongMode(t *testing.T) {
	s, peer := attachedTestServer(t, true)
	if _, err := Attach(context.Background(), s.AttachURL(), false); err == nil {
		t.Fatal("real/demo mismatch accepted")
	}
	key := make([]byte, 32)
	instance := strings.Repeat("0", 32)
	fragment := "#attach=" + instance + "." + base64.RawURLEncoding.EncodeToString(key)
	for _, value := range []string{"http://localhost:1234/" + fragment, "http://127.0.0.1:1234/?x=1" + fragment, "http://user@127.0.0.1:1234/" + fragment, "https://127.0.0.1:1234/" + fragment, peer.URL + "/#bootstrap=old", "http://192.0.2.1:1234/" + fragment, "http://127.0.0.1:1234/extra" + fragment, "http://127.0.0.1:01234/" + fragment} {
		if _, _, _, err := parseAttachURL(value); err != ErrOwnerAttach {
			t.Fatal("unsafe owner URL accepted")
		}
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		data := independentAttachReply(r, key, instance)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer target.Close()
	for _, mode := range []string{"redirect", "large", "stream_large", "headers", "content_type"} {
		t.Run(mode, func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, target.URL+"/api/owner/attach", http.StatusTemporaryRedirect)
					return
				}
				data := independentAttachReply(r, key, instance)
				w.Header().Set("Content-Type", "application/json")
				if mode == "headers" {
					w.Header().Set("X-Oversized-Test-Header", strings.Repeat("x", 2049))
				}
				if mode == "content_type" {
					w.Header().Set("Content-Type", "text/html")
				}
				if mode == "large" || mode == "stream_large" {
					data = append(data, []byte(strings.Repeat(" ", 2049))...)
				}
				if mode == "stream_large" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(data)
			}))
			defer fake.Close()
			value := fake.URL + "/" + fragment
			if _, err := Attach(context.Background(), value, false); err == nil {
				t.Fatal("untrusted attach response accepted")
			}
			if mode == "redirect" && redirected.Load() != 0 {
				t.Fatal("helper followed an attach redirect")
			}
		})
	}
}

func TestAttachEffectiveClockNeverRevivesExpiredProofAfterRollback(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	payload := attachTestRequest(t, s, false, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), strconv.FormatInt(time.Now().Unix()+5, 10))
	s.mu.Lock()
	s.clock.last = time.Now().Add(10 * time.Second)
	s.clock.wall = func() time.Time { return time.Now().Add(-time.Hour) }
	s.mu.Unlock()
	if attachTestHTTP(s, payload).Code != 401 {
		t.Fatal("wall clock rollback revived signed proof")
	}
}

func TestOwnerEffectiveTimeFollowsForwardWallChangesAndRetainsElapsedFloor(t *testing.T) {
	base := time.Now()
	wall := base
	elapsed := time.Duration(0)
	clock := ownerClock{base: base, last: base, wall: func() time.Time { return wall }, elapsed: func() time.Duration { return elapsed }}
	if effective := clock.nowLocked(); effective != effective.Round(0) {
		t.Fatal("effective clock retained monotonic metadata in wall comparisons")
	}
	wall = base.Add(time.Hour).Round(0)
	if effective := clock.nowLocked(); !effective.Equal(wall) {
		t.Fatal("forward wall correction was ignored")
	}
	wall = base.Add(-time.Hour).Round(0)
	elapsed = 2 * time.Hour
	want := base.Add(2 * time.Hour).Round(0)
	if effective := clock.nowLocked(); !effective.Equal(want) {
		t.Fatal("elapsed floor did not prevent rollback")
	}
	elapsed = 0
	if effective := clock.nowLocked(); !effective.Equal(want) {
		t.Fatal("effective clock went backwards after elapsed reset")
	}
}

func TestAttachSecretsNeverAppearInPublicShellOrRejectedReplies(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	private := strings.Split(strings.TrimPrefix(strings.Split(s.AttachURL(), "#")[1], "attach="), ".")[1]
	payload := attachTestRequest(t, s, false, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), strconv.FormatInt(time.Now().Unix()+5, 10))
	payload["proof"] = private
	rr := attachTestHTTP(s, payload)
	if rr.Code != 401 || strings.Contains(rr.Body.String(), private) {
		t.Fatal("rejected attach proof exposed private authority")
	}
	for _, path := range []string{"/", "/assets/app.js", "/assets/app.css", "/api/status"} {
		r := httptest.NewRequest(http.MethodGet, s.origin+path, nil)
		reply := httptest.NewRecorder()
		s.Handler().ServeHTTP(reply, r)
		if strings.Contains(reply.Body.String(), private) || strings.Contains(reply.Body.String(), s.AttachURL()) {
			t.Fatal("public response exposed persistent owner capability")
		}
		if reply.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("owner response became cacheable")
		}
	}
}
