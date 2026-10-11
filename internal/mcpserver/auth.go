package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// A request retains an opaque lookup key, never cached authorization or raw auth.
type principal struct {
	digest              [32]byte
	id                  archive.CredentialID
	boundID, excludedID archive.CredentialID
	mode                AuthMode
	nativeClients       NativeClientRegistry
	remoteClients       RemoteClientRegistry
}

func nativeRegistryConfigured(registry NativeClientRegistry) bool {
	return registryConfigured(registry)
}

func registryConfigured(registry any) bool {
	if registry == nil {
		return false
	}
	value := reflect.ValueOf(registry)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

func canonicalNativePrincipal(value string) bool {
	return canonicalClientPrincipal(value, archive.NativeClientPrincipalPrefix)
}

func canonicalRemotePrincipal(value string) bool {
	return canonicalClientPrincipal(value, archive.RemoteClientPrincipalPrefix)
}

func canonicalClientPrincipal(value, family string) bool {
	prefix := family + "v1/"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	nonce := strings.TrimPrefix(value, prefix)
	return canonicalClientNonce(nonce)
}

func canonicalClientNonce(nonce string) bool {
	if len(nonce) != 32 {
		return false
	}
	for _, character := range nonce {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validToken(raw string) bool {
	if len(raw) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == raw
}

func (p principal) resolve(ctx context.Context, credentials Credentials) (archive.CredentialGrant, error) {
	var grant archive.CredentialGrant
	var err error
	if p.id != "" {
		grant, err = credentials.ResolveCredentialID(ctx, p.id)
	} else {
		grant, err = credentials.ResolveCredential(ctx, p.digest)
	}
	if err != nil || grant.CredentialID == "" || grant.CredentialRevision == 0 || grant.Scope.Account == "" || grant.Scope.Principal == "" || grant.Scope.PolicyGeneration == 0 || (!grant.Scope.AllChats && len(grant.Scope.Chats) == 0) || (p.boundID != "" && p.boundID != grant.CredentialID) || (p.excludedID != "" && p.excludedID == grant.CredentialID) {
		return archive.CredentialGrant{}, ErrUnauthorized
	}
	switch p.mode {
	case LocalAdapter:
		if (!canonicalNativePrincipal(grant.Scope.Principal) && grant.Scope.Principal != archive.LegacyNativeClientPrincipal) || !nativeRegistryConfigured(p.nativeClients) || !p.nativeClients.PublishedNativeClient(NativeClientBinding{Account: grant.Scope.Account, Principal: grant.Scope.Principal, CredentialID: grant.CredentialID}) {
			return archive.CredentialGrant{}, ErrUnauthorized
		}
	case Bearer, Access:
		if strings.HasPrefix(grant.Scope.Principal, archive.NativeClientPrincipalPrefix) || grant.Scope.Principal == archive.LegacyNativeClientPrincipal {
			return archive.CredentialGrant{}, ErrUnauthorized
		}
		if strings.HasPrefix(grant.Scope.Principal, archive.RemoteClientPrincipalPrefix) {
			if p.mode != Bearer || !canonicalRemotePrincipal(grant.Scope.Principal) || !registryConfigured(p.remoteClients) || !p.remoteClients.PublishedRemoteClient(RemoteClientBinding{Account: grant.Scope.Account, Principal: grant.Scope.Principal, CredentialID: grant.CredentialID}) {
				return archive.CredentialGrant{}, ErrUnauthorized
			}
		}
	default:
		return archive.CredentialGrant{}, ErrUnauthorized
	}
	return grant, nil
}

func authenticate(r *http.Request, config Config, credentials Credentials, access accessVerifier) (principal, error) {
	var p principal
	mode := config.Mode
	p.mode = mode
	a, b, c := r.Header.Values("Authorization"), r.Header.Values(localAdapterHeader), r.Header.Values(accessHeader)
	var raw string
	switch mode {
	case LocalAdapter:
		if !nativeRegistryConfigured(config.NativeClients) || len(a) != 0 || len(c) != 0 || len(b) != 1 {
			return p, ErrUnauthorized
		}
		p.boundID = config.LocalCredentialID
		p.nativeClients = config.NativeClients
		raw = b[0]
	case Bearer:
		if len(b) != 0 || len(c) != 0 || len(a) != 1 || !strings.HasPrefix(a[0], "Bearer ") {
			return p, ErrUnauthorized
		}
		p.excludedID = config.LocalCredentialID
		p.remoteClients = config.RemoteClients
		raw = strings.TrimPrefix(a[0], "Bearer ")
	case Access:
		if len(a) != 0 || len(b) != 0 || len(c) != 1 || len(c[0]) == 0 || len(c[0]) > 16<<10 || access == nil {
			return p, ErrUnauthorized
		}
		id, err := access.Verify(r.Context(), c[0])
		if err != nil || id == "" {
			return p, ErrUnauthorized
		}
		p.id = id
		p.excludedID = config.LocalCredentialID
	default:
		return p, ErrUnauthorized
	}
	if mode != Access {
		if !validToken(raw) {
			return p, ErrUnauthorized
		}
		p.digest = sha256.Sum256([]byte(raw))
	}
	if _, err := p.resolve(r.Context(), credentials); err != nil {
		return principal{}, ErrUnauthorized
	}
	return p, nil
}
