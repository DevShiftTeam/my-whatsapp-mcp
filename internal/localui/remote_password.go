package localui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const adminHashPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"

type adminPasswordHash struct {
	salt   [16]byte
	digest [32]byte
}

// HashAdminPassword is for an owner-run private setup step. The encoded hash,
// never the password, is the server configuration. Runtime verification accepts
// only this fixed Argon2id profile, so configuration cannot request unbounded
// memory, CPU or goroutines.
func HashAdminPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 || !utf8.ValidString(password) || strings.ContainsRune(password, 0) {
		return "", errors.New("invalid_admin_password")
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", errors.New("admin_auth_unavailable")
	}
	digest := argon2.IDKey([]byte(password), salt[:], 2, 19456, 1, 32)
	defer clear(digest)
	return adminHashPrefix + base64.RawStdEncoding.EncodeToString(salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(digest), nil
}

func parseAdminPasswordHash(encoded string) (adminPasswordHash, bool) {
	var parsed adminPasswordHash
	if !strings.HasPrefix(encoded, adminHashPrefix) || len(encoded) != len(adminHashPrefix)+22+1+43 {
		return parsed, false
	}
	parts := strings.Split(strings.TrimPrefix(encoded, adminHashPrefix), "$")
	if len(parts) != 2 {
		return parsed, false
	}
	salt, saltErr := base64.RawStdEncoding.Strict().DecodeString(parts[0])
	digest, digestErr := base64.RawStdEncoding.Strict().DecodeString(parts[1])
	if saltErr != nil || digestErr != nil || len(salt) != 16 || len(digest) != 32 || base64.RawStdEncoding.EncodeToString(salt) != parts[0] || base64.RawStdEncoding.EncodeToString(digest) != parts[1] {
		return parsed, false
	}
	copy(parsed.salt[:], salt)
	copy(parsed.digest[:], digest)
	clear(digest)
	return parsed, true
}

func (hash adminPasswordHash) matches(password string) bool {
	digest := argon2.IDKey([]byte(password), hash.salt[:], 2, 19456, 1, 32)
	defer clear(digest)
	return subtle.ConstantTimeCompare(digest, hash.digest[:]) == 1
}
