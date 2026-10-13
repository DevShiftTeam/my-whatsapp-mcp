package archive

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
)

type savedCursor struct {
	LiveOffset    int    `json:"l"`
	HistoryOffset int    `json:"h"`
	LiveCursor    string `json:"c,omitempty"`
	LiveSkip      int    `json:"s"`
	LiveRank      int    `json:"r"`
	HistoryRank   int    `json:"q"`
	LiveDone      bool   `json:"d"`
	HistoryDone   bool   `json:"e"`
}

func (r *savedOwnerRead) encode(identity string, c savedCursor) (string, error) {
	aead, err := cursorAEAD(r.live.cursorKey)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return "", ErrSavedReadLimit
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", ErrArchiveUnavailable
	}
	wire := aead.Seal(nonce, nonce, plain, []byte("saved/v1/"+identity))
	encoded := "s1." + base64.RawURLEncoding.EncodeToString(wire)
	if len(encoded) > 8192 {
		return "", ErrSavedReadLimit
	}
	return encoded, nil
}
func (r *savedOwnerRead) decode(identity, raw string) (savedCursor, error) {
	var c savedCursor
	if raw == "" {
		return c, nil
	}
	if len(raw) > 8192 || !strings.HasPrefix(raw, "s1.") {
		return c, ErrSavedCursorStale
	}
	aead, err := cursorAEAD(r.live.cursorKey)
	if err != nil {
		return c, ErrSavedCursorStale
	}
	wire, err := base64.RawURLEncoding.Strict().DecodeString(raw[3:])
	if err != nil || len(wire) < aead.NonceSize()+aead.Overhead() {
		return c, ErrSavedCursorStale
	}
	plain, err := aead.Open(nil, wire[:aead.NonceSize()], wire[aead.NonceSize():], []byte("saved/v1/"+identity))
	if err != nil || json.Unmarshal(plain, &c) != nil {
		return c, ErrSavedCursorStale
	}
	if c.LiveOffset < 0 || c.HistoryOffset < 0 || c.LiveSkip < 0 || c.LiveSkip > 100 || c.LiveRank < 0 || c.HistoryRank < 0 || c.LiveOffset > maxCursorOffset || c.HistoryOffset > maxCursorOffset {
		return savedCursor{}, ErrSavedCursorStale
	}
	return c, nil
}
