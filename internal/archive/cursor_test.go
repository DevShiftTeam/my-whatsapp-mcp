package archive

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCursorRoundTripAndOpacity(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	identity := "private-principal/account/query/snapshot"
	for _, offset := range []int{0, 17, 1_000_000} {
		raw, err := encodeCursor(key, identity, offset)
		if err != nil {
			t.Fatalf("encode offset %d: %v", offset, err)
		}
		got, err := decodeCursor(key, raw, identity)
		if err != nil || got != offset {
			t.Fatalf("decode offset %d: got %d, %v", offset, got, err)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			t.Fatalf("cursor is not URL-safe: %v", err)
		}
		if bytes.Contains(decoded, []byte(identity)) || strings.Contains(raw, identity) {
			t.Fatal("cursor exposes reader identity")
		}
		second, err := encodeCursor(key, identity, offset)
		if err != nil || second == raw {
			t.Fatalf("repeated cursor encryption must use a fresh nonce: %v", err)
		}
	}
}

func TestCursorRejectsTamperingAndInvalidInputs(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	raw, err := encodeCursor(key, "reader-identity", 17)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wire {
		changed := bytes.Clone(wire)
		changed[i] ^= 1
		if _, err := decodeCursor(key, base64.RawURLEncoding.EncodeToString(changed), "reader-identity"); !errors.Is(err, ErrCursorStale) {
			t.Fatalf("tampered byte %d returned %v", i, err)
		}
	}
	tests := []struct {
		name, raw, identity string
		key                 []byte
	}{
		{"missing", "", "reader-identity", key},
		{"malformed", "!not-a-cursor", "reader-identity", key},
		{"noncanonical-newline", raw + "\n", "reader-identity", key},
		{"truncated", raw[:len(raw)/2], "reader-identity", key},
		{"oversized", strings.Repeat("A", 1025), "reader-identity", key},
		{"identity-changed", raw, "another-reader", key},
		{"identity-missing", raw, "", key},
		{"key-changed", raw, "reader-identity", bytes.Repeat([]byte{0x43}, 32)},
		{"key-missing", raw, "reader-identity", nil},
		{"key-too-short", raw, "reader-identity", bytes.Repeat([]byte{0x42}, 16)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeCursor(tt.key, tt.raw, tt.identity); !errors.Is(err, ErrCursorStale) {
				t.Fatalf("decode returned %v", err)
			}
		})
	}
}

func TestCursorEncodingFailsClosed(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	for _, tt := range []struct {
		name, identity string
		key            []byte
		offset         int
	}{
		{"negative-offset", "identity", key, -1},
		{"large-offset", "identity", key, 1_000_001},
		{"missing-key", "identity", nil, 1},
		{"short-key", "identity", bytes.Repeat([]byte{0x42}, 16), 1},
		{"missing-identity", "", key, 1},
		{"large-identity", strings.Repeat("x", 1024), key, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := encodeCursor(tt.key, tt.identity, tt.offset); !errors.Is(err, ErrArchiveUnavailable) {
				t.Fatalf("encode returned %v", err)
			}
		})
	}
}

func TestCursorRejectsAuthenticatedInvalidPayload(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, payload string
	}{
		{"missing-identity", `{"o":1}`},
		{"missing-offset", `{"i":"reader"}`},
		{"null-identity", `{"i":null,"o":1}`},
		{"null-offset", `{"i":"reader","o":null}`},
		{"unknown-field", `{"i":"reader","o":1,"extra":true}`},
		{"trailing-json", `{"i":"reader","o":1}{}`},
		{"malformed-json", `{"i":"reader","o":`},
		{"inner-identity-changed", `{"i":"other","o":1}`},
		{"negative-offset", `{"i":"reader","o":-1}`},
		{"large-offset", `{"i":"reader","o":1000001}`},
		{"non-integer-offset", `{"i":"reader","o":1.5}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Build a valid encrypted envelope independently so the decoder's
			// payload checks are exercised after successful authentication.
			wire := make([]byte, 1+aead.NonceSize())
			wire[0] = 1
			wire = aead.Seal(wire, wire[1:], []byte(tt.payload), []byte("wa-indexer/page-cursor/v1\x00reader"))
			raw := base64.RawURLEncoding.EncodeToString(wire)
			if _, err := decodeCursor(key, raw, "reader"); !errors.Is(err, ErrCursorStale) {
				t.Fatalf("invalid authenticated payload returned %v", err)
			}
		})
	}
}

func TestCursorRawSizeBoundary(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	identity := strings.Repeat("x", 725)
	raw, err := encodeCursor(key, identity, 1)
	if err != nil || len(raw) != 1024 {
		t.Fatalf("maximum-size cursor: length %d, %v", len(raw), err)
	}
	if offset, err := decodeCursor(key, raw, identity); err != nil || offset != 1 {
		t.Fatalf("maximum-size cursor decode: %d, %v", offset, err)
	}
	if _, err := encodeCursor(key, identity+"x", 1); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("oversized encoded cursor returned %v", err)
	}
}

func TestCursorBindsScopeRequestSnapshotAndBounds(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	scope := ReadScope{
		Account: "private-account", Principal: "private-principal", PolicyGeneration: 3,
		Chats: []ChatID{"chat-a", "chat-b"}, From: start, Until: start.Add(24 * time.Hour),
		Rolling: 7 * 24 * time.Hour,
	}
	type request struct {
		Query string
		Limit int
	}
	req := request{Query: "needle", Limit: 25}
	snapshot := ReadSnapshot{PolicyGeneration: 3, ArchiveRevision: 11, IndexGeneration: 5}
	from, until, validUntil := start, start.Add(time.Hour), start.Add(2*time.Hour)
	base := cursorIdentity("search", scope, req, snapshot, from, until, validUntil)
	key := bytes.Repeat([]byte{0x42}, 32)
	raw, err := encodeCursor(key, base, 17)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*ReadScope, *request, *ReadSnapshot, *time.Time, *time.Time, *time.Time) string
	}{
		{"account", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.Account = "other"
			return "search"
		}},
		{"principal", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.Principal = "other"
			return "search"
		}},
		{"scope-generation", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.PolicyGeneration++
			return "search"
		}},
		{"scope-chats", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.Chats = []ChatID{"chat-a"}
			return "search"
		}},
		{"all-chats", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.AllChats = true
			return "search"
		}},
		{"scope-from", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.From = s.From.Add(time.Second)
			return "search"
		}},
		{"scope-until", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.Until = s.Until.Add(time.Second)
			return "search"
		}},
		{"rolling", func(s *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			s.Rolling++
			return "search"
		}},
		{"query", func(_ *ReadScope, r *request, _ *ReadSnapshot, _, _, _ *time.Time) string {
			r.Query = "changed"
			return "search"
		}},
		{"limit", func(_ *ReadScope, r *request, _ *ReadSnapshot, _, _, _ *time.Time) string { r.Limit++; return "search" }},
		{"snapshot-policy", func(_ *ReadScope, _ *request, s *ReadSnapshot, _, _, _ *time.Time) string {
			s.PolicyGeneration++
			return "search"
		}},
		{"snapshot-archive", func(_ *ReadScope, _ *request, s *ReadSnapshot, _, _, _ *time.Time) string {
			s.ArchiveRevision++
			return "search"
		}},
		{"snapshot-index", func(_ *ReadScope, _ *request, s *ReadSnapshot, _, _, _ *time.Time) string {
			s.IndexGeneration++
			return "search"
		}},
		{"effective-from", func(_ *ReadScope, _ *request, _ *ReadSnapshot, f, _, _ *time.Time) string {
			*f = f.Add(time.Second)
			return "search"
		}},
		{"effective-until", func(_ *ReadScope, _ *request, _ *ReadSnapshot, _, u, _ *time.Time) string {
			*u = u.Add(time.Second)
			return "search"
		}},
		{"valid-until", func(_ *ReadScope, _ *request, _ *ReadSnapshot, _, _, v *time.Time) string {
			*v = v.Add(time.Second)
			return "search"
		}},
		{"kind", func(_ *ReadScope, _ *request, _ *ReadSnapshot, _, _, _ *time.Time) string { return "chats" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, r, sn, f, u, v := scope, req, snapshot, from, until, validUntil
			kind := tt.edit(&s, &r, &sn, &f, &u, &v)
			identity := cursorIdentity(kind, s, r, sn, f, u, v)
			if _, err := decodeCursor(key, raw, identity); !errors.Is(err, ErrCursorStale) {
				t.Fatalf("changed %s remained valid: %v", tt.name, err)
			}
		})
	}
}

func TestCursorIdentityCanonicalizesScopeWithoutMutation(t *testing.T) {
	instant := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	originalChats := []ChatID{"b", "a", "b"}
	scope := ReadScope{Account: "account", Principal: "principal", Chats: originalChats, From: instant, Until: instant.Add(time.Hour)}
	canonical := scope
	canonical.Chats = []ChatID{"a", "b"}
	canonical.From = instant.In(time.FixedZone("same-instant", 3*3600))
	canonical.Until = scope.Until.In(time.FixedZone("same-instant", 3*3600))
	snapshot := ReadSnapshot{ArchiveRevision: 7}
	first := cursorIdentity("search", scope, map[string]any{"b": 2, "a": 1}, snapshot, instant, instant.Add(time.Hour), instant.Add(2*time.Hour))
	second := cursorIdentity("search", canonical, map[string]any{"a": 1, "b": 2}, snapshot, canonical.From, canonical.Until, instant.Add(2*time.Hour).In(time.FixedZone("same-instant", 3*3600)))
	if first != second {
		t.Fatal("equivalent grants, map keys, or time instants changed cursor identity")
	}
	if len(first) != 64 || strings.Contains(first, "principal") || strings.Contains(first, "account") {
		t.Fatal("identity must be a SHA256 digest")
	}
	if !slices.Equal(scope.Chats, []ChatID{"b", "a", "b"}) {
		t.Fatal("identity calculation mutated server-owned chat scope")
	}
	scope.Chats, canonical.Chats = nil, []ChatID{}
	if cursorIdentity("chats", scope, nil, snapshot, instant, instant, instant) != cursorIdentity("chats", canonical, nil, snapshot, instant, instant, instant) {
		t.Fatal("nil and empty chat grants must have the same cursor identity")
	}
}

func TestCursorIdentityFailsClosedForUnsupportedRequest(t *testing.T) {
	if identity := cursorIdentity("search", ReadScope{}, make(chan int), ReadSnapshot{}, time.Time{}, time.Time{}, time.Time{}); identity != "" {
		t.Fatal("unserializable request must not receive a valid identity")
	}
}
