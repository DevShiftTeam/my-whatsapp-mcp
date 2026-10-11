package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestSearchCursorLexicalRankingVersionBoundary(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	scope := ReadScope{
		Account: "cursor-account", Principal: "cursor-principal", PolicyGeneration: 3,
		AllChats: true, Chats: []ChatID{"b", "a", "b"},
		From: start, Until: start.Add(24 * time.Hour), Rolling: 7 * 24 * time.Hour,
	}
	request := struct {
		Expression     string
		Chats          []ChatID
		Limit          int
		RollingHorizon time.Time
	}{`"alpha"`, []ChatID{"a"}, 2, start}
	snapshot := ReadSnapshot{PolicyGeneration: 3, ArchiveRevision: 11, IndexGeneration: 5}
	from, until, validUntil := start, start.Add(time.Hour), start.Add(2*time.Hour)
	key := bytes.Repeat([]byte{0x42}, 32)
	currentIdentity := cursorIdentity("search", scope, request, snapshot, from, until, validUntil)
	// Pin the intended token-ranking domain independently of the new constant.
	expectedIdentity := legacyUnversionedCursorIdentity("search/doc-local-fts-token-v1", scope, request, snapshot, from, until, validUntil)
	if currentIdentity == "" || currentIdentity != expectedIdentity {
		t.Fatal("search cursor identity must bind the token-ranking version")
	}

	t.Run("authenticated_legacy_cursor_is_stale", func(t *testing.T) {
		legacyIdentity := legacyUnversionedCursorIdentity("search", scope, request, snapshot, from, until, validUntil)
		raw, err := encodeCursor(key, legacyIdentity, 2)
		if err != nil {
			t.Fatal(err)
		}
		// Establish that this is an authentic, previously usable cursor.
		if offset, err := decodeCursor(key, raw, legacyIdentity); err != nil || offset != 2 {
			t.Fatalf("legacy authentication: offset %d, %v", offset, err)
		}
		if _, err := decodeCursor(key, raw, currentIdentity); !errors.Is(err, ErrCursorStale) {
			t.Fatalf("legacy density-ranked cursor remained valid: %v", err)
		}
	})

	t.Run("token_rank_cursor_resumes_offsets", func(t *testing.T) {
		for _, offset := range []int{2, 4, 17} {
			raw, err := encodeCursor(key, currentIdentity, offset)
			if err != nil {
				t.Fatal(err)
			}
			// Search recomputes this identity from sanitized request metadata.
			resumeIdentity := cursorIdentity("search", scope, request, snapshot, from, until, validUntil)
			if got, err := decodeCursor(key, raw, resumeIdentity); err != nil || got != offset {
				t.Fatalf("token-ranked resume: got %d want %d, %v", got, offset, err)
			}
		}
	})

	t.Run("nonsearch_identity_and_cursor_are_unchanged", func(t *testing.T) {
		legacyIdentity := legacyUnversionedCursorIdentity("chats", scope, request, snapshot, from, until, validUntil)
		currentIdentity := cursorIdentity("chats", scope, request, snapshot, from, until, validUntil)
		if currentIdentity != legacyIdentity {
			t.Fatal("nonsearch cursor identity changed")
		}
		raw, err := encodeCursor(key, legacyIdentity, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := decodeCursor(key, raw, currentIdentity); err != nil || got != 2 {
			t.Fatalf("nonsearch legacy resume: offset %d, %v", got, err)
		}
	})
}

// This test-only helper is the prechange cursorIdentity serialization, kept
// independent of the ranking-version branch to authenticate legacy cursors.
func legacyUnversionedCursorIdentity(kind string, scope ReadScope, request any, snapshot ReadSnapshot, from, until time.Time, validUntil time.Time) string {
	chats := slices.Clone(scope.Chats)
	slices.Sort(chats)
	chats = slices.Compact(chats)
	if len(chats) == 0 {
		chats = []ChatID{}
	}
	identity := struct {
		Kind             string
		Account          string
		Principal        string
		PolicyGeneration Generation
		AllChats         bool
		Chats            []ChatID
		ScopeFrom        time.Time
		ScopeUntil       time.Time
		Rolling          time.Duration
		Request          any
		Snapshot         ReadSnapshot
		From             time.Time
		Until            time.Time
		ValidUntil       time.Time
	}{
		Kind: kind, Account: scope.Account, Principal: scope.Principal,
		PolicyGeneration: scope.PolicyGeneration, AllChats: scope.AllChats, Chats: chats,
		ScopeFrom: scope.From.UTC(), ScopeUntil: scope.Until.UTC(), Rolling: scope.Rolling,
		Request: request, Snapshot: snapshot,
		From: from.UTC(), Until: until.UTC(), ValidUntil: validUntil.UTC(),
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
