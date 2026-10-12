// Package localauth owns the remote browser password, never native or MCP credentials.
package localauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

var (
	ErrPassword = errors.New("invalid_owner_password")
	ErrState    = errors.New("owner_password_unavailable")
	ErrBusy     = errors.New("owner_auth_busy")
)

const recordSize = 74

// Record is opaque outside this package. Only the private store encodes it.
type Record struct {
	generation uint64
	salt       [16]byte
	digest     [32]byte
}

func (r Record) Generation() uint64           { return r.generation }
func (r Record) String() string               { return "[private owner password record]" }
func (r Record) GoString() string             { return r.String() }
func (r Record) MarshalJSON() ([]byte, error) { return nil, ErrState }
func validPassword(p []byte) bool {
	if len(p) < 12 || len(p) > 128 || !utf8.Valid(p) {
		return false
	}
	for _, c := range string(p) {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func HashPassword(ctx context.Context, password []byte, generation uint64) (Record, error) {
	if !validPassword(password) || generation == 0 {
		return Record{}, ErrPassword
	}
	if err := hashSlots.acquire(ctx); err != nil {
		return Record{}, err
	}
	defer hashSlots.release()
	r := Record{generation: generation}
	if _, err := rand.Read(r.salt[:]); err != nil {
		return Record{}, ErrState
	}
	value := argon2.IDKey(password, r.salt[:], 3, 32*1024, 1, 32)
	copy(r.digest[:], value)
	clear(value)
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	return r, nil
}
func VerifyPassword(ctx context.Context, password []byte, r Record) (bool, error) {
	if !validPassword(password) || r.generation == 0 {
		return false, ErrPassword
	}
	if err := hashSlots.acquire(ctx); err != nil {
		return false, err
	}
	defer hashSlots.release()
	value := argon2.IDKey(password, r.salt[:], 3, 32*1024, 1, 32)
	defer clear(value)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(value, r.digest[:]) == 1, nil
}
func encodeRecord(r Record) []byte {
	b := make([]byte, recordSize)
	copy(b, "WAOWNER1")
	b[8] = 1
	binary.BigEndian.PutUint32(b[9:13], 32*1024)
	binary.BigEndian.PutUint32(b[13:17], 3)
	b[17] = 1
	binary.BigEndian.PutUint64(b[18:26], r.generation)
	copy(b[26:42], r.salt[:])
	copy(b[42:], r.digest[:])
	return b
}
func decodeRecord(b []byte) (Record, error) {
	if len(b) != recordSize || string(b[:8]) != "WAOWNER1" || b[8] != 1 || binary.BigEndian.Uint32(b[9:13]) != 32*1024 || binary.BigEndian.Uint32(b[13:17]) != 3 || b[17] != 1 {
		return Record{}, ErrState
	}
	r := Record{generation: binary.BigEndian.Uint64(b[18:26])}
	if r.generation == 0 {
		return Record{}, ErrState
	}
	copy(r.salt[:], b[26:42])
	copy(r.digest[:], b[42:])
	return r, nil
}
