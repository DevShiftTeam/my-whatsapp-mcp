package retrieval

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"sort"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

const cursorLifetime = 5 * time.Minute

type cursorPayload struct {
	Schema              int
	Offset              int
	Identity            [32]byte
	Ranking             [32]byte
	Snapshot            archive.ReadSnapshot
	IssuedAt, ExpiresAt int64
}

func staticCursorIdentity(scope archive.ReadScope, request archive.SearchRequest, query archive.CandidatePairRequest) ([32]byte, error) {
	scope.Chats = sortedChats(scope.Chats)
	request.Chats = sortedChats(request.Chats)
	request.Cursor = ""
	scope.From, scope.Until = scope.From.UTC(), scope.Until.UTC()
	request.From, request.Until = request.From.UTC(), request.Until.UTC()
	if request.Limit == 0 {
		request.Limit = 20
	}
	encoded, err := json.Marshal(struct {
		Scope       archive.ReadScope
		Request     archive.SearchRequest
		Generation  archive.IndexGenerationID
		Version     archive.IndexVersion
		KeywordOnly bool
	}{scope, request, query.Generation, query.Version, query.KeywordOnly})
	if err != nil {
		return [32]byte{}, archive.ErrCursorStale
	}
	return sha256.Sum256(encoded), nil
}
func sortedChats(input []archive.ChatID) []archive.ChatID {
	result := append([]archive.ChatID(nil), input...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	unique := result[:0]
	for _, id := range result {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	return unique
}
func rankingIdentity(pair archive.CandidatePair) ([32]byte, error) {
	// The candidate refs include every remembered source date/expiry and canonical
	// metadata version. Clock-only losses change this bounded ranking identity;
	// a harmless nanosecond tick does not change any effective-cutoff token.
	encoded, err := json.Marshal(struct{ Keyword, Semantic []archive.Candidate }{pair.Keyword, pair.Semantic})
	if err != nil {
		return [32]byte{}, archive.ErrCursorStale
	}
	return sha256.Sum256(encoded), nil
}
func (s *Service) cursorCipher() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.cursorKey[:])
	if err != nil {
		return nil, archive.ErrCursorStale
	}
	return cipher.NewGCM(block)
}
func (s *Service) encodeCursor(payload cursorPayload) (string, error) {
	aead, err := s.cursorCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", archive.ErrArchiveUnavailable
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", archive.ErrCursorStale
	}
	sealed := aead.Seal(nonce, nonce, encoded, []byte("wa-indexer-retrieval-cursor-v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (s *Service) decodeCursor(token string) (cursorPayload, error) {
	var payload cursorPayload
	if len(token) == 0 || len(token) > 1024 {
		return payload, archive.ErrCursorStale
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || base64.RawURLEncoding.EncodeToString(sealed) != token {
		return payload, archive.ErrCursorStale
	}
	aead, err := s.cursorCipher()
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return payload, archive.ErrCursorStale
	}
	encoded, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte("wa-indexer-retrieval-cursor-v1"))
	if err != nil {
		return payload, archive.ErrCursorStale
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&payload); err != nil {
		return cursorPayload{}, archive.ErrCursorStale
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return cursorPayload{}, archive.ErrCursorStale
	}
	now := time.Now().UnixMilli()
	if payload.Schema != 1 || payload.Offset < 1 || payload.Offset >= 200 || payload.IssuedAt <= 0 || payload.ExpiresAt-payload.IssuedAt != cursorLifetime.Milliseconds() || now < payload.IssuedAt || now >= payload.ExpiresAt {
		return cursorPayload{}, archive.ErrCursorStale
	}
	return payload, nil
}
