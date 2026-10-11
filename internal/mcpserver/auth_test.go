package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sync"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

type fakeCredentials struct {
	mu               sync.Mutex
	digest           [32]byte
	grant            archive.CredentialGrant
	resolveErr       error
	checkErr         error
	resolves, checks int
	onCheck          func(int)
}

func (f *fakeCredentials) CreateCredential(context.Context, archive.CredentialCreate) (archive.Credential, error) {
	return archive.Credential{}, ErrUnavailable
}
func (f *fakeCredentials) ResolveCredential(_ context.Context, d [32]byte) (archive.CredentialGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.resolveErr != nil {
		return archive.CredentialGrant{}, f.resolveErr
	}
	if d != f.digest {
		return archive.CredentialGrant{}, ErrUnauthorized
	}
	return f.grant, nil
}
func (f *fakeCredentials) ResolveCredentialID(_ context.Context, id archive.CredentialID) (archive.CredentialGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.resolveErr != nil {
		return archive.CredentialGrant{}, f.resolveErr
	}
	if id != f.grant.CredentialID {
		return archive.CredentialGrant{}, ErrUnauthorized
	}
	return f.grant, nil
}
func (f *fakeCredentials) SetCredentialPolicy(context.Context, archive.CredentialID, archive.CredentialPolicy, uint64) (archive.Credential, error) {
	return archive.Credential{}, ErrUnavailable
}
func (f *fakeCredentials) RevokeCredential(context.Context, archive.CredentialID, uint64) error {
	return ErrUnavailable
}
func (f *fakeCredentials) CheckCredentialGrant(context.Context, archive.CredentialGrant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.onCheck != nil {
		f.onCheck(f.checks)
	}
	return f.checkErr
}

func fakeToken() string { return base64.RawURLEncoding.EncodeToString(make([]byte, 32)) }
func credentialsFixture() *fakeCredentials {
	return &fakeCredentials{digest: sha256.Sum256([]byte(fakeToken())), grant: archive.CredentialGrant{CredentialID: "credential-fixture", CredentialRevision: 1, Scope: archive.ReadScope{Account: "source-fixture", Principal: "client-fixture", PolicyGeneration: 1, AllChats: true}}}
}

const nativeFixturePrincipal = "wa-indexer/native-client/v1/00000000000000000000000000000000"

type fakeNativeRegistry struct {
	mu       sync.Mutex
	bindings map[NativeClientBinding]bool
	checks   int
}

func nativeBinding(grant archive.CredentialGrant) NativeClientBinding {
	return NativeClientBinding{Account: grant.Scope.Account, Principal: grant.Scope.Principal, CredentialID: grant.CredentialID}
}

func nativeRegistryFixture(grants ...archive.CredentialGrant) *fakeNativeRegistry {
	r := &fakeNativeRegistry{bindings: make(map[NativeClientBinding]bool)}
	for _, grant := range grants {
		r.bindings[nativeBinding(grant)] = true
	}
	return r
}

func (r *fakeNativeRegistry) PublishedNativeClient(binding NativeClientBinding) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	return r.bindings[binding]
}

func (r *fakeNativeRegistry) remove(binding NativeClientBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bindings, binding)
}

func (r *fakeNativeRegistry) publish(binding NativeClientBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings[binding] = true
}

func nativeCredentialsFixture() *fakeCredentials {
	f := credentialsFixture()
	f.grant.Scope.Principal = nativeFixturePrincipal
	return f
}

func TestAuthenticationModesAreExclusive(t *testing.T) {
	for _, mode := range []AuthMode{LocalAdapter, Bearer} {
		t.Run(string(mode), func(t *testing.T) {
			for _, kind := range []string{"missing", "duplicate", "mixed", "malformed", "valid"} {
				t.Run(kind, func(t *testing.T) {
					f := credentialsFixture()
					if mode == LocalAdapter {
						f = nativeCredentialsFixture()
					}
					r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
					name, value := localAdapterHeader, fakeToken()
					if mode == Bearer {
						name, value = "Authorization", "Bearer "+fakeToken()
					}
					switch kind {
					case "duplicate":
						r.Header.Add(name, value)
						r.Header.Add(name, value)
					case "mixed":
						r.Header.Set(name, value)
						r.Header.Set(accessHeader, "fixture-assertion")
					case "malformed":
						r.Header.Set(name, value+" ")
					case "valid":
						r.Header.Set(name, value)
					}
					_, err := authenticate(r, Config{Mode: mode, NativeClients: nativeRegistryFixture(f.grant)}, f, nil)
					if (err == nil) != (kind == "valid") {
						t.Fatalf("authentication result for %s: %v", kind, err)
					}
					if kind != "valid" && f.resolves != 0 {
						t.Fatal("invalid header reached credential persistence")
					}
				})
			}
		})
	}
}

func TestPrincipalResolvesAgainAndDeniesRevocation(t *testing.T) {
	f := credentialsFixture()
	r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+fakeToken())
	p, err := authenticate(r, Config{Mode: Bearer}, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.resolveErr = archive.ErrUnauthorized
	if _, err = p.resolve(context.Background(), f); err != ErrUnauthorized {
		t.Fatalf("revoked authority reused: %v", err)
	}
	if f.resolves != 2 {
		t.Fatalf("expected fresh resolve: %d", f.resolves)
	}
}

type fixtureAccessVerifier struct {
	id    archive.CredentialID
	err   error
	calls int
}

func (v *fixtureAccessVerifier) Verify(context.Context, string) (archive.CredentialID, error) {
	v.calls++
	return v.id, v.err
}

func TestNativeCredentialCannotReplayAcrossModes(t *testing.T) {
	f := nativeCredentialsFixture()
	native := f.grant.CredentialID
	for _, mode := range []AuthMode{LocalAdapter, Bearer, Access} {
		t.Run(string(mode), func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
			v := &fixtureAccessVerifier{id: native}
			switch mode {
			case LocalAdapter:
				r.Header.Set(localAdapterHeader, fakeToken())
			case Bearer:
				r.Header.Set("Authorization", "Bearer "+fakeToken())
			case Access:
				r.Header.Set(accessHeader, "synthetic-verified-assertion")
			}
			_, err := authenticate(r, Config{Mode: mode, NativeClients: nativeRegistryFixture(f.grant)}, f, v)
			if (err == nil) != (mode == LocalAdapter) {
				t.Fatal("native grant escaped its listener mode")
			}
		})
	}
	r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
	r.Header.Set(localAdapterHeader, fakeToken())
	if _, err := authenticate(r, Config{Mode: LocalAdapter, LocalCredentialID: "another-native-grant", NativeClients: nativeRegistryFixture(f.grant)}, f, nil); err != ErrUnauthorized {
		t.Fatal("unbound token reached local adapter")
	}
	if _, err := normalizeConfig(Config{Mode: LocalAdapter, Hosts: []string{"localhost"}}); err != ErrConfiguration {
		t.Fatal("missing native binding did not fail configuration")
	}
}

func TestLocalAdapterRequiresCanonicalPublishedBindingOnEveryResolve(t *testing.T) {
	for _, kind := range []string{"no_registry", "not_published", "wrong_account", "wrong_principal", "wrong_id", "remote_principal", "uppercase_nonce", "short_nonce", "future_version", "legacy_unpublished", "valid", "legacy_migrated"} {
		t.Run(kind, func(t *testing.T) {
			f := nativeCredentialsFixture()
			binding := nativeBinding(f.grant)
			switch kind {
			case "wrong_account":
				binding.Account = "different-account"
			case "wrong_principal":
				binding.Principal = "wa-indexer/native-client/v1/11111111111111111111111111111111"
			case "wrong_id":
				binding.CredentialID = "different-credential"
			case "remote_principal":
				f.grant.Scope.Principal = "client:remote"
				binding = nativeBinding(f.grant)
			case "uppercase_nonce":
				f.grant.Scope.Principal = "wa-indexer/native-client/v1/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
				binding = nativeBinding(f.grant)
			case "short_nonce":
				f.grant.Scope.Principal = "wa-indexer/native-client/v1/0000"
				binding = nativeBinding(f.grant)
			case "future_version":
				f.grant.Scope.Principal = "wa-indexer/native-client/v2/00000000000000000000000000000000"
				binding = nativeBinding(f.grant)
			case "legacy_unpublished", "legacy_migrated":
				f.grant.Scope.Principal = "native-local-client"
				binding = nativeBinding(f.grant)
			}
			registry := &fakeNativeRegistry{bindings: map[NativeClientBinding]bool{binding: true}}
			if kind == "not_published" || kind == "legacy_unpublished" {
				registry.remove(binding)
			}
			var nativeClients NativeClientRegistry = registry
			if kind == "no_registry" {
				nativeClients = nil
			}
			r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
			r.Header.Set(localAdapterHeader, fakeToken())
			p, err := authenticate(r, Config{Mode: LocalAdapter, NativeClients: nativeClients}, f, nil)
			allowed := kind == "valid" || kind == "legacy_migrated"
			if (err == nil) != allowed {
				t.Fatalf("published native binding decision for %s: %v", kind, err)
			}
			if allowed {
				registry.remove(binding)
				if _, err := p.resolve(context.Background(), f); err != ErrUnauthorized {
					t.Fatal("removed native binding reused old local authority")
				}
				if registry.checks != 2 {
					t.Fatal("native registry was not checked on each fresh resolve")
				}
			}
		})
	}
}

func TestRemoteRejectsEveryReservedNativePrincipalWithoutKnownID(t *testing.T) {
	for _, value := range []string{nativeFixturePrincipal, "wa-indexer/native-client/", "wa-indexer/native-client/v1/bad", "wa-indexer/native-client/v1/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "wa-indexer/native-client/v9/future", "native-local-client"} {
		for _, mode := range []AuthMode{Bearer, Access} {
			t.Run(string(mode)+"/"+value, func(t *testing.T) {
				f := credentialsFixture()
				f.grant.Scope.Principal = value
				r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
				if mode == Bearer {
					r.Header.Set("Authorization", "Bearer "+fakeToken())
				} else {
					r.Header.Set(accessHeader, "synthetic-verified-assertion")
				}
				v := &fixtureAccessVerifier{id: f.grant.CredentialID}
				if _, err := authenticate(r, Config{Mode: mode}, f, v); err != ErrUnauthorized {
					t.Fatal("reserved native principal replayed remotely")
				}
			})
		}
	}
}

func TestLocalAdapterSupportsMultiplePublishedClientsWithoutCompatibilityPin(t *testing.T) {
	f := nativeCredentialsFixture()
	first := f.grant
	second := first
	second.CredentialID = "second-native-client"
	second.Scope.Principal = "wa-indexer/native-client/v1/11111111111111111111111111111111"
	registry := nativeRegistryFixture(first, second)
	config := Config{Mode: LocalAdapter, NativeClients: registry}
	for _, grant := range []archive.CredentialGrant{first, second} {
		f.grant = grant
		r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
		r.Header.Set(localAdapterHeader, fakeToken())
		if _, err := authenticate(r, config, f, nil); err != nil {
			t.Fatal("independently published native client required a singular pin")
		}
	}
	var absent *fakeNativeRegistry
	if _, err := normalizeConfig(Config{Mode: LocalAdapter, NativeClients: absent, Hosts: []string{"localhost"}}); err != ErrConfiguration {
		t.Fatal("typed nil registry did not fail closed")
	}
}

func TestLocalAdapterHostMustMatchFixedNativeAuthority(t *testing.T) {
	credentials := nativeCredentialsFixture()
	for _, hosts := range [][]string{nil, {"localhost:80"}, {"127.0.0.1:1234"}, {"localhost", "localhost"}, {"localhost"}} {
		_, err := normalizeConfig(Config{Mode: LocalAdapter, NativeClients: nativeRegistryFixture(credentials.grant), Hosts: hosts})
		allowed := len(hosts) == 1 && hosts[0] == "localhost"
		if (err == nil) != allowed {
			t.Fatal("local handler accepted a different logical authority")
		}
	}
}

func TestAccessHeadersHaveNoBearerFallback(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "bearer", "adapter", "unmapped", "valid"} {
		t.Run(kind, func(t *testing.T) {
			f := credentialsFixture()
			r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1234/mcp", nil)
			v := &fixtureAccessVerifier{id: f.grant.CredentialID}
			if kind != "missing" {
				r.Header.Set(accessHeader, "synthetic-verified-assertion")
			}
			switch kind {
			case "duplicate":
				r.Header.Add(accessHeader, "synthetic-verified-assertion")
			case "bearer":
				r.Header.Set("Authorization", "Bearer "+fakeToken())
			case "adapter":
				r.Header.Set(localAdapterHeader, fakeToken())
			case "unmapped":
				v.id = "unknown-grant"
			}
			_, err := authenticate(r, Config{Mode: Access}, f, v)
			if (err == nil) != (kind == "valid") {
				t.Fatal("Access assertion boundary accepted fallback")
			}
			if kind != "valid" && kind != "unmapped" && v.calls != 0 {
				t.Fatal("mixed header reached verifier")
			}
		})
	}
}
