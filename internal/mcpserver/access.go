package mcpserver

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/golang-jwt/jwt/v5"
)

const (
	maxAccessAssertionBytes   = 4 << 10
	maxAccessJWKSBytes        = 128 << 10
	maxAccessJWKSKeys         = 16
	accessJWKSCacheTTL        = 5 * time.Minute
	accessJWKSRefreshInterval = 30 * time.Second
	accessJWKSFetchTimeout    = 3 * time.Second
)

type signedAccessVerifier struct {
	config      AccessConfig
	client      *http.Client
	now         func() time.Time
	refresh     chan struct{}
	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	expiresAt   time.Time
	lastAttempt time.Time
	lastFailure bool
	refreshing  bool
}

func newAccessVerifier(config AccessConfig) (accessVerifier, error) {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    time.Second,
		ResponseHeaderTimeout:  2 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		MaxIdleConns:           1,
		MaxIdleConnsPerHost:    1,
		MaxConnsPerHost:        1,
		IdleConnTimeout:        30 * time.Second,
	}
	return newAccessVerifierWithTransport(config, transport, time.Now)
}

// This private constructor allows synthetic transports and clocks in tests.
// Production configuration exposes neither transport nor URL overrides.
func newAccessVerifierWithTransport(config AccessConfig, transport http.RoundTripper, now func() time.Time) (accessVerifier, error) {
	if !validAccessConfiguration(config) || transport == nil || now == nil {
		return nil, ErrConfiguration
	}
	identities := make(map[string]archive.CredentialID, len(config.Identities))
	for subject, id := range config.Identities {
		identities[subject] = id
	}
	config.Identities = identities
	return &signedAccessVerifier{
		config:  config,
		client:  &http.Client{Transport: transport, Timeout: accessJWKSFetchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now:     now,
		refresh: make(chan struct{}, 1),
	}, nil
}

func validAccessConfiguration(config AccessConfig) bool {
	issuer, err := url.Parse(config.Issuer)
	if err != nil || !validAccessURL(issuer, config.Issuer) || (issuer.Path != "" && issuer.Path != "/") {
		return false
	}
	jwks, err := url.Parse(config.JWKSURL)
	if err != nil || !validAccessURL(jwks, config.JWKSURL) || jwks.Host != issuer.Host || jwks.Path == "" || jwks.Path == "/" {
		return false
	}
	if !boundedAccessIdentifier(config.Audience, 256) || len(config.Identities) == 0 || len(config.Identities) > 256 {
		return false
	}
	for subject, id := range config.Identities {
		if !boundedAccessIdentifier(subject, 256) || !boundedAccessIdentifier(string(id), 256) {
			return false
		}
	}
	return true
}

func validAccessURL(parsed *url.URL, raw string) bool {
	return len(raw) <= 2048 && strings.TrimSpace(raw) == raw && parsed.Scheme == "https" && parsed.Host != "" && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.RawFragment == "" && parsed.RawPath == "" && parsed.Opaque == "" && parsed.String() == raw
}

func boundedAccessIdentifier(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func (v *signedAccessVerifier) Verify(ctx context.Context, raw string) (archive.CredentialID, error) {
	if ctx == nil || ctx.Err() != nil || len(raw) == 0 || len(raw) > maxAccessAssertionBytes {
		return "", ErrUnauthorized
	}
	options := []jwt.ParserOption{jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.config.Issuer), jwt.WithAudience(v.config.Audience), jwt.WithExpirationRequired(), jwt.WithTimeFunc(v.now), jwt.WithIssuedAt(), jwt.WithStrictDecoding()}
	claims := &jwt.RegisteredClaims{}
	token, _, err := jwt.NewParser(options...).ParseUnverified(raw, claims)
	if err != nil || token.Method != jwt.SigningMethodRS256 || token.Header["alg"] != "RS256" {
		return "", ErrUnauthorized
	}
	kid, ok := token.Header["kid"].(string)
	if !ok || !boundedAccessIdentifier(kid, 128) {
		return "", ErrUnauthorized
	}
	for _, unsupported := range []string{"crit", "jku", "jwk", "x5u", "x5c"} {
		if _, exists := token.Header[unsupported]; exists {
			return "", ErrUnauthorized
		}
	}
	credentialID, mapped := v.config.Identities[claims.Subject]
	// This preliminary check only suppresses useless network work. No authority
	// is returned until ParseWithClaims has also verified the signature below.
	if !mapped || jwt.NewValidator(options...).Validate(claims) != nil {
		return "", ErrUnauthorized
	}
	key, err := v.publicKey(ctx, kid)
	if err != nil {
		return "", err
	}
	verified, err := jwt.ParseWithClaims(raw, &jwt.RegisteredClaims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodRS256 {
			return nil, ErrUnauthorized
		}
		return key, nil
	}, options...)
	if err != nil || !verified.Valid || ctx.Err() != nil {
		return "", ErrUnauthorized
	}
	return credentialID, nil
}

func (v *signedAccessVerifier) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if key, err, ready := v.cachedKey(kid); ready {
		return key, err
	}
	select {
	case v.refresh <- struct{}{}:
		defer func() { <-v.refresh }()
	case <-ctx.Done():
		return nil, ErrUnavailable
	}
	if key, err, ready := v.cachedKey(kid); ready {
		return key, err
	}
	v.mu.Lock()
	previousAttempt, previousFailure := v.lastAttempt, v.lastFailure
	v.lastAttempt = v.now()
	v.refreshing = true
	v.mu.Unlock()
	keys, err := v.fetchKeys(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refreshing = false
	if err != nil {
		if ctx.Err() != nil {
			// A caller abandoning its refresh is not evidence that the shared
			// issuer is unavailable. Let another live caller retry immediately.
			v.lastAttempt, v.lastFailure = previousAttempt, previousFailure
		} else {
			v.lastFailure = true
		}
		return nil, ErrUnavailable
	}
	v.keys, v.expiresAt, v.lastFailure = keys, v.now().Add(accessJWKSCacheTTL), false
	key := keys[kid]
	if key == nil {
		return nil, ErrUnauthorized
	}
	return key, nil
}

// Unknown keys and issuer failures share a refresh interval. Caller cancellation
// preserves prior shared state. Expired keys are never used after a failed refresh.
func (v *signedAccessVerifier) cachedKey(kid string) (*rsa.PublicKey, error, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if now.Before(v.expiresAt) {
		if key := v.keys[kid]; key != nil {
			return key, nil, true
		}
	}
	if v.refreshing {
		return nil, nil, false
	}
	if !v.lastAttempt.IsZero() && now.Before(v.lastAttempt.Add(accessJWKSRefreshInterval)) {
		if v.lastFailure || !now.Before(v.expiresAt) {
			return nil, ErrUnavailable, true
		}
		return nil, ErrUnauthorized, true
	}
	return nil, nil, false
}

func (v *signedAccessVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.config.JWKSURL, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	request.Header.Set("Accept", "application/json")
	response, err := v.client.Do(request)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > maxAccessJWKSBytes {
		return nil, ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAccessJWKSBytes+1))
	if err != nil || len(body) > maxAccessJWKSBytes {
		return nil, ErrUnavailable
	}
	return parseAccessJWKS(body)
}

func parseAccessJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	var jwks struct {
		Keys []struct {
			KeyType   string `json:"kty"`
			KeyID     string `json:"kid"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&jwks) != nil || len(jwks.Keys) == 0 || len(jwks.Keys) > maxAccessJWKSKeys {
		return nil, ErrUnavailable
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, ErrUnavailable
	}
	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	seen := make(map[string]bool, len(jwks.Keys))
	for _, jwk := range jwks.Keys {
		if !boundedAccessIdentifier(jwk.KeyID, 128) || seen[jwk.KeyID] {
			return nil, ErrUnavailable
		}
		seen[jwk.KeyID] = true
		if jwk.KeyType != "RSA" || (jwk.Use != "" && jwk.Use != "sig") || (jwk.Algorithm != "" && jwk.Algorithm != "RS256") {
			continue
		}
		if len(jwk.Modulus) > 684 || len(jwk.Exponent) > 6 {
			return nil, ErrUnavailable
		}
		n, err := base64.RawURLEncoding.Strict().DecodeString(jwk.Modulus)
		if err != nil || len(n) < 256 || len(n) > 512 || n[0] == 0 {
			return nil, ErrUnavailable
		}
		e, err := base64.RawURLEncoding.Strict().DecodeString(jwk.Exponent)
		if err != nil || len(e) == 0 || len(e) > 4 || e[0] == 0 {
			return nil, ErrUnavailable
		}
		exponent := uint64(0)
		for _, b := range e {
			exponent = exponent<<8 | uint64(b)
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 || modulus.BitLen() > 4096 || modulus.Bit(0) != 1 || exponent < 3 || exponent > 1<<31-1 || exponent&1 != 1 {
			return nil, ErrUnavailable
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: modulus, E: int(exponent)}
	}
	if len(keys) == 0 {
		return nil, ErrUnavailable
	}
	return keys, nil
}
