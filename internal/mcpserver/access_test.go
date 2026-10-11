package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/golang-jwt/jwt/v5"
)

type accessRoundTripper func(*http.Request) (*http.Response, error)

func (f accessRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type accessFixture struct {
	t   *testing.T
	key *rsa.PrivateKey
	cfg AccessConfig
	now time.Time
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &accessFixture{t: t, key: key, now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC), cfg: AccessConfig{
		Issuer: "https://synthetic-access.invalid", Audience: "synthetic-client-a", JWKSURL: "https://synthetic-access.invalid/cdn-cgi/access/certs",
		Identities: map[string]archive.CredentialID{"authenticated-subject-a": "credential-a"},
	}}
}

func (f *accessFixture) token(edit func(*jwt.RegisteredClaims, *jwt.Token)) string {
	f.t.Helper()
	claims := &jwt.RegisteredClaims{Issuer: f.cfg.Issuer, Subject: "authenticated-subject-a", Audience: jwt.ClaimStrings{f.cfg.Audience}, ExpiresAt: jwt.NewNumericDate(f.now.Add(time.Hour)), NotBefore: jwt.NewNumericDate(f.now.Add(-time.Minute)), IssuedAt: jwt.NewNumericDate(f.now.Add(-time.Minute))}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "synthetic-key"
	if edit != nil {
		edit(claims, token)
	}
	var signingKey any = f.key
	if token.Method == jwt.SigningMethodHS256 {
		signingKey = []byte("synthetic-hmac-secret")
	}
	if token.Method == jwt.SigningMethodNone {
		signingKey = jwt.UnsafeAllowNoneSignatureType
	}
	raw, err := token.SignedString(signingKey)
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *accessFixture) jwks() string {
	f.t.Helper()
	e := big.NewInt(int64(f.key.E)).Bytes()
	data, err := json.Marshal(map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": "synthetic-key", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)}}})
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func accessResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAccessSignedAssertionValidation(t *testing.T) {
	f := newAccessFixture(t)
	var calls atomic.Int32
	v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != f.cfg.JWKSURL || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
			t.Error("JWKS request changed the pinned endpoint or sent credentials")
		}
		return accessResponse(http.StatusOK, f.jwks()), nil
	}), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	good := f.token(nil)
	id, err := v.Verify(context.Background(), good)
	if err != nil || id != "credential-a" {
		t.Fatalf("valid signed assertion: id=%q err=%v", id, err)
	}
	f.cfg.Identities["authenticated-subject-a"] = "mutated-credential"
	id, err = v.Verify(context.Background(), good)
	if err != nil || id != "credential-a" {
		t.Fatal("mutable config changed the verified mapping")
	}
	if calls.Load() != 1 {
		t.Fatalf("cached key fetched %d times", calls.Load())
	}

	tests := map[string]string{
		"empty": "", "opaque": "not-a-jwt", "too_large": strings.Repeat("a", 8193),
		"tampered":          good[:len(good)-8] + "tampered",
		"wrong_issuer":      f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.Issuer = "https://wrong.invalid" }),
		"wrong_audience":    f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.Audience = jwt.ClaimStrings{"wrong-client"} }),
		"expired":           f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.ExpiresAt = jwt.NewNumericDate(f.now.Add(-time.Second)) }),
		"missing_exp":       f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.ExpiresAt = nil }),
		"not_yet_valid":     f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.NotBefore = jwt.NewNumericDate(f.now.Add(time.Second)) }),
		"future_iat":        f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.IssuedAt = jwt.NewNumericDate(f.now.Add(time.Second)) }),
		"missing_subject":   f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.Subject = "" }),
		"unmapped_subject":  f.token(func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.Subject = "authenticated-subject-b" }),
		"missing_kid":       f.token(func(_ *jwt.RegisteredClaims, token *jwt.Token) { delete(token.Header, "kid") }),
		"unknown_kid":       f.token(func(_ *jwt.RegisteredClaims, token *jwt.Token) { token.Header["kid"] = "unknown-key" }),
		"critical_header":   f.token(func(_ *jwt.RegisteredClaims, token *jwt.Token) { token.Header["crit"] = []string{"private-extension"} }),
		"remote_key_header": f.token(func(_ *jwt.RegisteredClaims, token *jwt.Token) { token.Header["jku"] = "https://attacker.invalid/keys" }),
		"hmac": f.token(func(_ *jwt.RegisteredClaims, token *jwt.Token) {
			token.Method = jwt.SigningMethodHS256
			token.Header["alg"] = "HS256"
		}),
		"unsigned": f.token(func(_ *jwt.RegisteredClaims, token *jwt.Token) {
			token.Method = jwt.SigningMethodNone
			token.Header["alg"] = "none"
		}),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			id, err := v.Verify(context.Background(), raw)
			if id != "" || !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("bad assertion accepted: id=%q err=%v", id, err)
			}
			if err.Error() != ErrUnauthorized.Error() {
				t.Fatal("failure exposed assertion details")
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid assertions caused %d network fetches", calls.Load())
	}
}

func TestAccessUnknownKidBoundedRefresh(t *testing.T) {
	f := newAccessFixture(t)
	var calls atomic.Int32
	var clockMu sync.Mutex
	now := f.now
	v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return accessResponse(http.StatusOK, f.jwks()), nil
	}), func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), f.token(nil)); err != nil {
		t.Fatal(err)
	}
	unknown := f.token(func(_ *jwt.RegisteredClaims, token *jwt.Token) { token.Header["kid"] = "unknown" })
	check := func() {
		t.Helper()
		var group sync.WaitGroup
		for range 20 {
			group.Add(1)
			go func() {
				defer group.Done()
				if _, err := v.Verify(context.Background(), unknown); !errors.Is(err, ErrUnauthorized) {
					t.Errorf("unknown key: %v", err)
				}
			}()
		}
		group.Wait()
	}
	check()
	if calls.Load() != 1 {
		t.Fatalf("cooldown made %d fetches", calls.Load())
	}
	clockMu.Lock()
	now = now.Add(31 * time.Second)
	clockMu.Unlock()
	check()
	if calls.Load() != 2 {
		t.Fatalf("unknown-key refresh was not coalesced: %d fetches", calls.Load())
	}
	clockMu.Lock()
	now = now.Add(5 * time.Minute)
	clockMu.Unlock()
	if _, err := v.Verify(context.Background(), f.token(nil)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expired JWKS cache did not refresh: %d fetches", calls.Load())
	}
}

func TestAccessInitialJWKSFetchIsCoalesced(t *testing.T) {
	f := newAccessFixture(t)
	const callers = 20
	var calls atomic.Int32
	var clockCalls atomic.Int32
	entered := make(chan struct{})
	lookupsReady := make(chan struct{})
	release := make(chan struct{})
	v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return accessResponse(http.StatusOK, f.jwks()), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}), func() time.Time {
		// Before the blocked fetch returns, its caller reads the clock four
		// times; every other caller reads it for claims and the cache lookup.
		// The barrier holds the response until all cold lookups have begun.
		if clockCalls.Add(1) == 2*callers+2 {
			close(lookupsReady)
		}
		return f.now
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := f.token(nil)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			id, err := v.Verify(context.Background(), raw)
			if id != "credential-a" || err != nil {
				t.Errorf("concurrent valid assertion: id=%q err=%v", id, err)
			}
		}()
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		group.Wait()
		t.Fatal("JWKS fetch did not begin")
	}
	select {
	case <-lookupsReady:
	case <-time.After(3 * time.Second):
		close(release)
		group.Wait()
		t.Fatal("concurrent cold lookups did not reach the fetch barrier")
	}
	close(release)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("initial key lookup made %d fetches", calls.Load())
	}
}

func TestAccessInvalidClaimsDoNotFetchJWKS(t *testing.T) {
	f := newAccessFixture(t)
	for name, edit := range map[string]func(*jwt.RegisteredClaims, *jwt.Token){
		"issuer":   func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.Issuer = "https://wrong.invalid" },
		"audience": func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.Audience = jwt.ClaimStrings{"wrong-client"} },
		"subject":  func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.Subject = "unmapped" },
		"expired":  func(c *jwt.RegisteredClaims, _ *jwt.Token) { c.ExpiresAt = jwt.NewNumericDate(f.now.Add(-time.Second)) },
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return accessResponse(http.StatusOK, f.jwks()), nil
			}), func() time.Time { return f.now })
			if err != nil {
				t.Fatal(err)
			}
			raw := f.token(func(c *jwt.RegisteredClaims, token *jwt.Token) {
				token.Header["kid"] = "attacker-unknown-key"
				edit(c, token)
			})
			if _, err := v.Verify(context.Background(), raw); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("invalid claim: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("invalid claims caused a JWKS fetch")
			}
		})
	}
}

func TestAccessAssertionExpirationRecheckedAfterFetch(t *testing.T) {
	f := newAccessFixture(t)
	now := f.now
	v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(*http.Request) (*http.Response, error) {
		now = now.Add(2 * time.Hour)
		return accessResponse(http.StatusOK, f.jwks()), nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if id, err := v.Verify(context.Background(), f.token(nil)); id != "" || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("assertion expired while fetching keys: id=%q err=%v", id, err)
	}
}

func TestAccessRotatedJWKSKeyAndRemovedKey(t *testing.T) {
	f := newAccessFixture(t)
	firstRaw := f.token(nil)
	firstJWKS := f.jwks()
	rotated := newAccessFixture(t)
	var keyset atomic.Value
	keyset.Store(firstJWKS)
	var calls atomic.Int32
	now := f.now
	v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return accessResponse(http.StatusOK, keyset.Load().(string)), nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), firstRaw); err != nil {
		t.Fatal(err)
	}
	// The replacement key has the same kid, so expiry must replace cached keys.
	keyset.Store(rotated.jwks())
	now = now.Add(5*time.Minute + time.Second)
	if _, err := v.Verify(context.Background(), rotated.token(nil)); err != nil {
		t.Fatalf("rotated key rejected: %v", err)
	}
	if _, err := v.Verify(context.Background(), firstRaw); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("removed signing key remained valid: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("rotation made %d fetches", calls.Load())
	}
}

func TestAccessJWKSFetchFailClosedAndBounded(t *testing.T) {
	f := newAccessFixture(t)
	good := f.token(nil)
	for name, response := range map[string]func() (*http.Response, error){
		"redirect": func() (*http.Response, error) {
			r := accessResponse(http.StatusFound, "")
			r.Header.Set("Location", "https://attacker.invalid/keys")
			return r, nil
		},
		"server_error": func() (*http.Response, error) {
			return accessResponse(http.StatusInternalServerError, "secret-diagnostic"), nil
		},
		"large": func() (*http.Response, error) {
			return accessResponse(http.StatusOK, strings.Repeat(" ", 128<<10)+f.jwks()), nil
		},
		"invalid_json":  func() (*http.Response, error) { return accessResponse(http.StatusOK, "secret-not-json"), nil },
		"trailing_json": func() (*http.Response, error) { return accessResponse(http.StatusOK, f.jwks()+"{}"), nil },
		"duplicate_kid": func() (*http.Response, error) {
			var j map[string]any
			_ = json.Unmarshal([]byte(f.jwks()), &j)
			keys := j["keys"].([]any)
			j["keys"] = append(keys, keys[0])
			b, _ := json.Marshal(j)
			return accessResponse(http.StatusOK, string(b)), nil
		},
		"too_many_keys": func() (*http.Response, error) {
			var j map[string]any
			_ = json.Unmarshal([]byte(f.jwks()), &j)
			key := j["keys"].([]any)[0]
			keys := make([]any, 17)
			for i := range keys {
				keys[i] = key
			}
			j["keys"] = keys
			b, _ := json.Marshal(j)
			return accessResponse(http.StatusOK, string(b)), nil
		},
		"weak_rsa": func() (*http.Response, error) {
			var j map[string]any
			_ = json.Unmarshal([]byte(f.jwks()), &j)
			j["keys"].([]any)[0].(map[string]any)["n"] = "AQ"
			b, _ := json.Marshal(j)
			return accessResponse(http.StatusOK, string(b)), nil
		},
		"transport_error": func() (*http.Response, error) { return nil, errors.New("secret-url-and-token") },
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(*http.Request) (*http.Response, error) { calls.Add(1); return response() }), func() time.Time { return f.now })
			if err != nil {
				t.Fatal(err)
			}
			id, err := v.Verify(context.Background(), good)
			if id != "" || !errors.Is(err, ErrUnavailable) || err.Error() != ErrUnavailable.Error() {
				t.Fatalf("unsafe fetch result: id=%q err=%v", id, err)
			}
			if _, err := v.Verify(context.Background(), good); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("shared issuer failure did not retain its cooldown: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("fetch made %d attempts", calls.Load())
			}
		})
	}
}

func TestAccessRejectsMalformedConfiguration(t *testing.T) {
	f := newAccessFixture(t)
	for name, edit := range map[string]func(*AccessConfig){
		"http_issuer":      func(c *AccessConfig) { c.Issuer = "http://synthetic-access.invalid" },
		"issuer_path":      func(c *AccessConfig) { c.Issuer += "/untrusted" },
		"http_jwks":        func(c *AccessConfig) { c.JWKSURL = "http://synthetic-access.invalid/keys" },
		"other_origin":     func(c *AccessConfig) { c.JWKSURL = "https://attacker.invalid/keys" },
		"userinfo":         func(c *AccessConfig) { c.JWKSURL = "https://secret@synthetic-access.invalid/keys" },
		"fragment":         func(c *AccessConfig) { c.JWKSURL += "#fragment" },
		"query":            func(c *AccessConfig) { c.JWKSURL += "?token=secret" },
		"empty_audience":   func(c *AccessConfig) { c.Audience = "" },
		"empty_identities": func(c *AccessConfig) { c.Identities = nil },
		"empty_credential": func(c *AccessConfig) { c.Identities = map[string]archive.CredentialID{"authenticated-subject-a": ""} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := f.cfg
			edit(&cfg)
			v, err := newAccessVerifier(cfg)
			if v != nil || !errors.Is(err, ErrConfiguration) {
				t.Fatalf("invalid configuration: verifier=%v err=%v", v, err)
			}
		})
	}
}

func TestAccessCanceledJWKSRequestAndCacheExpiry(t *testing.T) {
	f := newAccessFixture(t)
	var calls atomic.Int32
	now := f.now
	v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) != 2 {
			return accessResponse(http.StatusOK, f.jwks()), nil
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	good := f.token(nil)
	if _, err := v.Verify(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5*time.Minute + time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	id, err := v.Verify(ctx, good)
	if id != "" || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired key served after failed refresh: id=%q err=%v", id, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("fetch ignored request cancellation")
	}
	if id, err := v.Verify(context.Background(), good); id != "credential-a" || err != nil {
		t.Fatalf("caller deadline prevented a live client refreshing expired keys: id=%q err=%v", id, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected a live refresh after the abandoned request, got %d fetches", calls.Load())
	}
}

func TestAccessCanceledLeadingFetchDoesNotThrottleLiveClient(t *testing.T) {
	f := newAccessFixture(t)
	var calls atomic.Int32
	entered := make(chan struct{})
	v, err := newAccessVerifierWithTransport(f.cfg, accessRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return accessResponse(http.StatusOK, f.jwks()), nil
	}), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	good := f.token(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		id, err := v.Verify(ctx, good)
		if id != "" {
			result <- errors.New("canceled caller received a credential")
			return
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("leading JWKS fetch did not begin")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("canceled leading fetch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leading JWKS fetch ignored cancellation")
	}
	// The service clock has not advanced: this request must not inherit a
	// thirty-second issuer failure cooldown from the canceled caller.
	if id, err := v.Verify(context.Background(), good); id != "credential-a" || err != nil {
		t.Fatalf("canceled leading request blocked a live client: id=%q err=%v", id, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one canceled fetch and one live retry, got %d", calls.Load())
	}
}
