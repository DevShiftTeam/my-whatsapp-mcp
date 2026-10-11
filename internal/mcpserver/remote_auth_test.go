package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

const remoteFixturePrincipal = "wa-indexer/remote-client/v1/00000000000000000000000000000000"

type fakeRemoteRegistry struct {
	mu       sync.Mutex
	bindings map[RemoteClientBinding]bool
	checks   int
}

func remoteBinding(grant archive.CredentialGrant) RemoteClientBinding {
	return RemoteClientBinding{Account: grant.Scope.Account, Principal: grant.Scope.Principal, CredentialID: grant.CredentialID}
}

func remoteRegistryFixture(grants ...archive.CredentialGrant) *fakeRemoteRegistry {
	r := &fakeRemoteRegistry{bindings: make(map[RemoteClientBinding]bool)}
	for _, grant := range grants {
		r.bindings[remoteBinding(grant)] = true
	}
	return r
}

func (r *fakeRemoteRegistry) PublishedRemoteClient(binding RemoteClientBinding) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	return r.bindings[binding]
}

func (r *fakeRemoteRegistry) remove(binding RemoteClientBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bindings, binding)
}

func (r *fakeRemoteRegistry) publish(binding RemoteClientBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings[binding] = true
}

func remoteCredentialsFixture() *fakeCredentials {
	f := credentialsFixture()
	f.grant.Scope.Principal = remoteFixturePrincipal
	return f
}

func TestRemoteFamilyRequiresCanonicalReadyBinding(t *testing.T) {
	for _, kind := range []string{"ready", "nil", "typed_nil", "pending", "account", "principal", "id", "uppercase", "short", "future", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			credentials := remoteCredentialsFixture()
			binding := remoteBinding(credentials.grant)
			registry := remoteRegistryFixture(credentials.grant)
			var configured RemoteClientRegistry = registry
			switch kind {
			case "nil":
				configured = nil
			case "typed_nil":
				configured = (*fakeRemoteRegistry)(nil)
			case "pending":
				registry.remove(binding)
			case "account":
				registry.remove(binding)
				binding.Account = "other-account"
				registry.bindings[binding] = true
			case "principal":
				registry.remove(binding)
				binding.Principal = "other-principal"
				registry.bindings[binding] = true
			case "id":
				registry.remove(binding)
				binding.CredentialID = "other-id"
				registry.bindings[binding] = true
			case "uppercase":
				credentials.grant.Scope.Principal = archive.RemoteClientPrincipalPrefix + "v1/" + strings.Repeat("A", 32)
			case "short":
				credentials.grant.Scope.Principal = archive.RemoteClientPrincipalPrefix + "v1/0"
			case "future":
				credentials.grant.Scope.Principal = archive.RemoteClientPrincipalPrefix + "v2/" + strings.Repeat("0", 32)
			case "malformed":
				credentials.grant.Scope.Principal = archive.RemoteClientPrincipalPrefix
			}
			if kind == "uppercase" || kind == "short" || kind == "future" || kind == "malformed" {
				// Supply the exact mutated binding so canonical validation is the
				// only reason this reserved principal can fail authorization.
				configured = remoteRegistryFixture(credentials.grant)
			}
			request, _ := http.NewRequest(http.MethodPost, "https://mcp.example.com/mcp", nil)
			request.Header.Set("Authorization", "Bearer "+fakeToken())
			_, err := authenticate(request, Config{Mode: Bearer, RemoteClients: configured}, credentials, nil)
			if (err == nil) != (kind == "ready") {
				t.Fatal("reserved remote grant escaped canonical ready binding")
			}
		})
	}
}

func TestRemoteFamilyCannotReplayIntoNativeOrAccess(t *testing.T) {
	for _, principalName := range []string{remoteFixturePrincipal, archive.RemoteClientPrincipalPrefix, archive.RemoteClientPrincipalPrefix + "v9/not-canonical"} {
		for _, mode := range []AuthMode{LocalAdapter, Access} {
			credentials := remoteCredentialsFixture()
			credentials.grant.Scope.Principal = principalName
			request, _ := http.NewRequest(http.MethodPost, "http://localhost/mcp", nil)
			if mode == LocalAdapter {
				request.Header.Set(localAdapterHeader, fakeToken())
			} else {
				request.Header.Set(accessHeader, "synthetic-verified-assertion")
			}
			_, err := authenticate(request, Config{Mode: mode, NativeClients: nativeRegistryFixture(credentials.grant), RemoteClients: remoteRegistryFixture(credentials.grant)}, credentials, &fixtureAccessVerifier{id: credentials.grant.CredentialID})
			if err != ErrUnauthorized {
				t.Fatal("remote family crossed listener mode")
			}
		}
	}
}

func TestRemotePrincipalRechecksReadyRegistryAndCredential(t *testing.T) {
	for _, operation := range []string{"unpublish", "revoke_or_expire"} {
		t.Run(operation, func(t *testing.T) {
			credentials := remoteCredentialsFixture()
			registry := remoteRegistryFixture(credentials.grant)
			request, _ := http.NewRequest(http.MethodPost, "https://mcp.example.com/mcp", nil)
			request.Header.Set("Authorization", "Bearer "+fakeToken())
			principal, err := authenticate(request, Config{Mode: Bearer, RemoteClients: registry}, credentials, nil)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "unpublish" {
				registry.remove(remoteBinding(credentials.grant))
			} else {
				credentials.resolveErr = archive.ErrUnauthorized
			}
			if _, err := principal.resolve(context.Background(), credentials); err != ErrUnauthorized {
				t.Fatal("cached remote authority survived invalidation")
			}
		})
	}
}

func TestRemoteRegistryRecheckedBeforeHTTPResponse(t *testing.T) {
	credentials := remoteCredentialsFixture()
	registry := remoteRegistryFixture(credentials.grant)
	reader := &fakeReader{onCheck: func(count int) {
		if count == 1 {
			registry.remove(remoteBinding(credentials.grant))
		}
	}}
	server, _ := httpFixture(t, reader, credentials, func(config *Config) { config.RemoteClients = registry })
	status, body := rpc(t, server, statusRPC, nil)
	if status != http.StatusUnauthorized || body != `{"error":"mcp_unauthorized"}` {
		t.Fatal("unpublished remote binding escaped buffered response")
	}
	if reader.reads != 1 || registry.checks != 3 {
		t.Fatal("initial, tool and final remote binding checks were skipped")
	}
}
