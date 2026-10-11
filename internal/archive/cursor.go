package archive

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"slices"
	"time"
)

const (
	cursorVersion   = 1
	maxCursorBytes  = 1024
	maxCursorOffset = 1_000_000
)

// Search cursors bind the server's lexical ranking semantics.
const searchLexicalRankVersion = "doc-local-fts-token-v1"

type pageCursor struct {
	Identity string `json:"i"`
	Offset   int    `json:"o"`
}

func encodeCursor(key []byte, identity string, offset int) (string, error) {
	if identity == "" || offset < 0 || offset > maxCursorOffset {
		return "", ErrArchiveUnavailable
	}
	aead, err := cursorAEAD(key)
	if err != nil {
		return "", ErrArchiveUnavailable
	}
	plaintext, err := json.Marshal(pageCursor{Identity: identity, Offset: offset})
	if err != nil {
		return "", ErrArchiveUnavailable
	}
	if base64.RawURLEncoding.EncodedLen(1+aead.NonceSize()+len(plaintext)+aead.Overhead()) > maxCursorBytes {
		return "", ErrArchiveUnavailable
	}
	wire := make([]byte, 1+aead.NonceSize())
	wire[0] = cursorVersion
	nonce := wire[1:]
	if _, err := rand.Read(nonce); err != nil {
		return "", ErrArchiveUnavailable
	}
	wire = aead.Seal(wire, nonce, plaintext, cursorAAD(identity))
	return base64.RawURLEncoding.EncodeToString(wire), nil
}

func decodeCursor(key []byte, raw, identity string) (int, error) {
	if raw == "" || len(raw) > maxCursorBytes || identity == "" {
		return 0, ErrCursorStale
	}
	aead, err := cursorAEAD(key)
	if err != nil {
		return 0, ErrCursorStale
	}
	wire, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || base64.RawURLEncoding.EncodeToString(wire) != raw || len(wire) < 1+aead.NonceSize()+aead.Overhead() || wire[0] != cursorVersion {
		return 0, ErrCursorStale
	}
	nonce := wire[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, wire[1+aead.NonceSize():], cursorAAD(identity))
	if err != nil {
		return 0, ErrCursorStale
	}
	// Pointers distinguish missing fields from a legitimate zero offset.
	var cursor struct {
		Identity *string `json:"i"`
		Offset   *int    `json:"o"`
	}
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Identity == nil || cursor.Offset == nil {
		return 0, ErrCursorStale
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return 0, ErrCursorStale
	}
	if *cursor.Identity != identity || *cursor.Offset < 0 || *cursor.Offset > maxCursorOffset {
		return 0, ErrCursorStale
	}
	return *cursor.Offset, nil
}

func cursorAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrArchiveUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	return cipher.NewGCM(block)
}

func cursorAAD(identity string) []byte {
	return []byte("wa-indexer/page-cursor/v1\x00" + identity)
}

// cursorIdentity binds a page to the server's grant and its current read view.
// Request must be a sanitized request value that excludes the cursor itself.
func cursorIdentity(kind string, scope ReadScope, request any, snapshot ReadSnapshot, from, until time.Time, validUntil time.Time) string {
	if kind == "search" {
		kind += "/" + searchLexicalRankVersion
	}
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
