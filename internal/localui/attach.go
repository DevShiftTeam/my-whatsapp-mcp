package localui

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrOwnerAttach = errors.New("owner_attach_unavailable")

const (
	attachRequestDomain  = "wa-indexer-owner-attach/request/v1"
	attachResponseDomain = "wa-indexer-owner-attach/response/v1"
	attachReplyLimit     = 2048
)

type ownerClock struct {
	base, last time.Time
	wall       func() time.Time
	elapsed    func() time.Duration
}

// Called only under the server mutex. Neither wall-clock rollback nor cache
// cleanup can make an expired proof or browser capability valid again.
func (clock *ownerClock) nowLocked() time.Time {
	// Effective time compares wall timestamps. time.Now also carries a monotonic
	// reading; strip it here so a forward wall correction is observed immediately.
	now := clock.wall().Round(0)
	elapsed := clock.elapsed()
	if elapsed < 0 {
		elapsed = 0
	}
	monotonic := clock.base.Add(elapsed).Round(0)
	if now.Before(monotonic) {
		now = monotonic
	}
	if now.Before(clock.last.Round(0)) {
		now = clock.last.Round(0)
	}
	clock.last = now
	return now
}

type attachRequest struct {
	Instance    string `json:"instance"`
	Nonce       string `json:"nonce"`
	Demo        *bool  `json:"demo"`
	ExpiresUnix string `json:"expires_unix"`
	Proof       string `json:"proof"`
}

type attachResponse struct {
	Instance       string `json:"instance"`
	Nonce          string `json:"nonce"`
	Demo           *bool  `json:"demo"`
	BootstrapToken string `json:"bootstrap_token"`
	ExpiresUnix    string `json:"expires_unix"`
	Proof          string `json:"proof"`
}

func modeField(demo bool) string {
	if demo {
		return "1"
	}
	return "0"
}

func attachMAC(key []byte, fields ...string) string {
	mac := hmac.New(sha256.New, key)
	for _, field := range fields {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write([]byte(field))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func canonicalToken(value string) ([]byte, bool) {
	if len(value) != 43 {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return decoded, err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func canonicalInstance(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == value
}

func canonicalExpiry(value string) (int64, bool) {
	if len(value) > 19 {
		return 0, false
	}
	expiry, err := strconv.ParseInt(value, 10, 64)
	return expiry, err == nil && expiry > 0 && strconv.FormatInt(expiry, 10) == value
}

func equalProof(expected, supplied string) bool {
	a, okA := canonicalToken(expected)
	b, okB := canonicalToken(supplied)
	return okA && okB && hmac.Equal(a, b)
}

func (s *Server) expireCapabilitiesLocked(now time.Time) {
	for digest, expiry := range s.bootstraps {
		if !now.Before(expiry) {
			delete(s.bootstraps, digest)
		}
	}
	for nonce, expiry := range s.nonces {
		if !now.Before(expiry) {
			delete(s.nonces, nonce)
		}
	}
}

func (s *Server) ownerAttach(w http.ResponseWriter, r *http.Request) {
	var request attachRequest
	if !decode(w, r, &request) {
		return
	}
	_, nonceOK := canonicalToken(request.Nonce)
	requestExpiry, expiryOK := canonicalExpiry(request.ExpiresUnix)
	if !canonicalInstance(request.Instance) || !nonceOK || !expiryOK || request.Demo == nil {
		fail(w, http.StatusUnauthorized, "owner_attach_unavailable")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.nowLocked()
	s.expireCapabilitiesLocked(now)
	expected := attachMAC(s.attachKey[:], attachRequestDomain, s.origin, request.Instance, request.Nonce, modeField(*request.Demo), request.ExpiresUnix)
	if s.attachRevoked || request.Instance != s.instance || *request.Demo != s.demo || requestExpiry <= now.Unix() || requestExpiry > now.Unix()+6 || !equalProof(expected, request.Proof) {
		fail(w, http.StatusUnauthorized, "owner_attach_unavailable")
		return
	}
	if _, replay := s.nonces[request.Nonce]; replay {
		fail(w, http.StatusUnauthorized, "owner_attach_unavailable")
		return
	}
	if len(s.nonces) >= 64 || len(s.bootstraps) >= 8 {
		fail(w, http.StatusTooManyRequests, "owner_attach_limit")
		return
	}
	// Burn the authenticated nonce before minting; entropy/transport failure must
	// never make the same request reusable. Live entries are never evicted early.
	s.nonces[request.Nonce] = now.Add(time.Minute)
	token, err := randomToken()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "owner_auth_unavailable")
		return
	}
	// Check again under the same mint lock; expiry cannot revive on wall rollback.
	now = s.clock.nowLocked()
	if requestExpiry <= now.Unix() {
		fail(w, http.StatusUnauthorized, "owner_attach_unavailable")
		return
	}
	expiry := now.Add(30 * time.Second).Unix()
	s.bootstraps[sha256.Sum256([]byte(token))] = time.Unix(expiry, 0)
	response := attachResponse{Instance: s.instance, Nonce: request.Nonce, Demo: request.Demo, BootstrapToken: token, ExpiresUnix: strconv.FormatInt(expiry, 10)}
	response.Proof = attachMAC(s.attachKey[:], attachResponseDomain, s.origin, response.Instance, response.Nonce, modeField(*response.Demo), request.ExpiresUnix, response.BootstrapToken, response.ExpiresUnix)
	respond(w, response)
}

func parseAttachURL(value string) (origin, instance string, key []byte, err error) {
	if len(value) > 512 || strings.Contains(value, "%") {
		return "", "", nil, ErrOwnerAttach
	}
	u, e := url.Parse(value)
	if e != nil || u.Scheme != "http" || u.User != nil || u.Path != "/" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" || u.RawFragment != "" || !strings.HasPrefix(u.Fragment, "attach=") {
		return "", "", nil, ErrOwnerAttach
	}
	ip := net.ParseIP(u.Hostname())
	port, e := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || e != nil || port < 1 || port > 65535 || u.Host != net.JoinHostPort(ip.String(), strconv.Itoa(port)) {
		return "", "", nil, ErrOwnerAttach
	}
	parts := strings.Split(strings.TrimPrefix(u.Fragment, "attach="), ".")
	if len(parts) != 2 || !canonicalInstance(parts[0]) {
		return "", "", nil, ErrOwnerAttach
	}
	key, ok := canonicalToken(parts[1])
	if !ok {
		return "", "", nil, ErrOwnerAttach
	}
	return "http://" + u.Host, parts[0], key, nil
}

// Attach authenticates the live owner server without sending the private attach
// secret to its port. The caller opens the returned URL only after verification.
// All failures are fixed and contain no URLs, secret, proof or response contents.
func Attach(ctx context.Context, privateURL string, expectedDemo bool) (string, error) {
	return attach(ctx, privateURL, expectedDemo, "")
}

// ValidOwnerConnect accepts only canonical literal loopback addresses with a
// positive port. DNS, IPv4-mapped IPv6 and scoped addresses are never dialed.
func ValidOwnerConnect(connect string) bool {
	address, err := netip.ParseAddrPort(connect)
	return err == nil && address.Port() != 0 && address.String() == connect && address.Addr().IsLoopback() && !address.Addr().Is4In6() && address.Addr().Zone() == ""
}

// AttachWithConnect dials an explicitly trusted local listener while retaining
// the private URL's approved browser Host and signed Origin. The override never
// expands authority, allows a proxy, or follows a redirect.
func AttachWithConnect(ctx context.Context, privateURL string, expectedDemo bool, connect string) (string, error) {
	if !ValidOwnerConnect(connect) {
		return "", ErrOwnerAttach
	}
	return attach(ctx, privateURL, expectedDemo, connect)
}

func attach(ctx context.Context, privateURL string, expectedDemo bool, connect string) (string, error) {
	origin, instance, key, err := parseAttachURL(privateURL)
	if err != nil {
		return "", ErrOwnerAttach
	}
	defer clear(key)
	nonce, err := randomToken()
	if err != nil {
		return "", ErrOwnerAttach
	}
	demo := expectedDemo
	request := attachRequest{Instance: instance, Nonce: nonce, Demo: &demo, ExpiresUnix: strconv.FormatInt(time.Now().Unix()+5, 10)}
	request.Proof = attachMAC(key, attachRequestDomain, origin, instance, nonce, modeField(demo), request.ExpiresUnix)
	body, err := json.Marshal(request)
	if err != nil {
		return "", ErrOwnerAttach
	}
	operation, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(operation, http.MethodPost, origin+"/api/owner/attach", bytes.NewReader(body))
	if err != nil {
		return "", ErrOwnerAttach
	}
	httpRequest.Header.Set("Origin", origin)
	httpRequest.Header.Set("Content-Type", "application/json")
	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	if connect != "" {
		dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, connect)
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: dial, DisableKeepAlives: true, ResponseHeaderTimeout: 2 * time.Second, MaxResponseHeaderBytes: attachReplyLimit}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrOwnerAttach }}
	response, err := client.Do(httpRequest)
	if err != nil {
		return "", ErrOwnerAttach
	}
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || err != nil || mediaType != "application/json" || len(response.Header.Values("Content-Type")) != 1 || response.ContentLength > attachReplyLimit {
		return "", ErrOwnerAttach
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, attachReplyLimit+1))
	if err != nil || len(data) > attachReplyLimit || !uniqueFields(data) {
		return "", ErrOwnerAttach
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var reply attachResponse
	if decoder.Decode(&reply) != nil || decoder.Decode(new(any)) != io.EOF {
		return "", ErrOwnerAttach
	}
	_, tokenOK := canonicalToken(reply.BootstrapToken)
	expiry, expiryOK := canonicalExpiry(reply.ExpiresUnix)
	now := time.Now()
	if reply.Instance != instance || reply.Nonce != nonce || reply.Demo == nil || *reply.Demo != expectedDemo || !tokenOK || !expiryOK || !time.Unix(expiry, 0).After(now) || time.Unix(expiry, 0).Sub(now) > 31*time.Second {
		return "", ErrOwnerAttach
	}
	expected := attachMAC(key, attachResponseDomain, origin, instance, nonce, modeField(expectedDemo), request.ExpiresUnix, reply.BootstrapToken, reply.ExpiresUnix)
	if !equalProof(expected, reply.Proof) {
		return "", ErrOwnerAttach
	}
	return origin + "/#bootstrap=" + reply.BootstrapToken, nil
}
