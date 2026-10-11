package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

const remoteFileFixtureID archive.CredentialID = "0123456789abcdef0123456789abcdef"

type remoteFileCredentialStore struct {
	fileCredentialStore
	afterCreate        func()
	uncertainCreateErr error
}

func (s *remoteFileCredentialStore) CreateCredential(_ context.Context, request archive.CredentialCreate) (archive.Credential, error) {
	if s.createErr != nil {
		return archive.Credential{}, s.createErr
	}
	s.created = append(s.created, request)
	if s.afterCreate != nil {
		s.afterCreate()
	}
	return archive.Credential{ID: remoteFileFixtureID, Revision: 1, Label: request.Label, Principal: request.Principal, Policy: request.Policy}, s.uncertainCreateErr
}

func remoteFileRequest() archive.CredentialCreate {
	return archive.CredentialCreate{Label: "synthetic remote bearer", Principal: remoteFixturePrincipal, Policy: archive.CredentialPolicy{Account: "private-source-account-canary", AllChats: true}}
}

func remoteFileEndpoint() RemoteEndpoint {
	return RemoteEndpoint{PublicURL: "https://mcp.example.com/mcp"}
}

func TestRemoteCredentialFilePublishesPrivateRandomDigestOnly(t *testing.T) {
	root, dir := privateTestRoot(t)
	store := new(remoteFileCredentialStore)
	request := remoteFileRequest()
	request.Digest = [32]byte{7}
	credential, err := IssueRemoteCredentialFile(context.Background(), store, request, root, "remote-one.json", remoteFileEndpoint())
	if err != nil || credential.ID != remoteFileFixtureID || len(store.created) != 1 {
		t.Fatal("remote credential was not published")
	}
	config, id, err := ReadRemoteCredentialFileRoot(root, "remote-one.json")
	if err != nil || id != credential.ID || config.PublicURL != remoteFileEndpoint().PublicURL {
		t.Fatal("protected remote handoff lost identity")
	}
	raw, decodeErr := base64.RawURLEncoding.DecodeString(config.Token)
	if decodeErr != nil || len(raw) != 32 || store.created[0].Digest != sha256.Sum256([]byte(config.Token)) || store.created[0].Digest == request.Digest {
		t.Fatal("raw token did not produce a fresh 256-bit digest")
	}
	info, statErr := root.Stat("remote-one.json")
	if statErr != nil || info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatal("remote handoff is not private")
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "remote-one.json"))
	if readErr != nil || strings.Contains(string(data), request.Principal) || strings.Contains(string(data), request.Policy.Account) {
		t.Fatal("remote file exposed archive authority")
	}
	var shape map[string]any
	if json.Unmarshal(data, &shape) != nil || len(shape) != 5 || shape["version"] != float64(1) || shape["mode"] != "remote_bearer" || shape["endpoint"] != nil || shape["native_socket_path"] != nil {
		t.Fatal("remote file widened native format")
	}
	if _, _, err := ReadCredentialFileRoot(root, "remote-one.json"); err != ErrCredentialFile {
		t.Fatal("native decoder accepted remote file")
	}
	if _, err := IssueRemoteCredentialFile(context.Background(), store, request, root, "remote-two.json", remoteFileEndpoint()); err != nil {
		t.Fatal(err)
	}
	second, _, err := ReadRemoteCredentialFileRoot(root, "remote-two.json")
	if err != nil || second.Token == config.Token || store.created[1].Digest == store.created[0].Digest {
		t.Fatal("independent remote grants reused token")
	}
}

func TestRemoteCredentialFilePublicationFailurePreservesIdentityAndCleanupError(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		root, dir := privateTestRoot(t)
		path := filepath.Join(dir, "remote.json")
		if err := os.WriteFile(path, []byte("existing-owner-canary"), 0600); err != nil {
			t.Fatal(err)
		}
		store := new(remoteFileCredentialStore)
		want := ErrCredentialFile
		if cleanupFails {
			store.revokeErr = errors.New("private-revoke-error-canary")
			want = ErrCredentialCleanup
		}
		credential, err := IssueRemoteCredentialFile(context.Background(), store, remoteFileRequest(), root, "remote.json", remoteFileEndpoint())
		if err != want || credential.ID != remoteFileFixtureID || credential.Revision != 1 || len(store.revoked) != 1 || store.revoked[0] != credential.ID {
			t.Fatal("post-create uncertainty lost recovery identity")
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != "existing-owner-canary" {
			t.Fatal("publication changed existing owner file")
		}
		if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "canary") {
			t.Fatal("private failure detail escaped")
		}
	}
}

func TestRemoteCredentialFileUncertainCreateRetainsKnownIdentityWithoutPublication(t *testing.T) {
	root, _ := privateTestRoot(t)
	store := &remoteFileCredentialStore{uncertainCreateErr: errors.New("private-create-uncertainty-canary")}
	credential, err := IssueRemoteCredentialFile(context.Background(), store, remoteFileRequest(), root, "remote.json", remoteFileEndpoint())
	if err != ErrCredentialFile || credential.ID != remoteFileFixtureID || credential.Revision != 1 || len(store.created) != 1 || len(store.revoked) != 0 {
		t.Fatal("uncertain Create lost known intent identity or issued twice")
	}
	if _, err := root.Lstat("remote.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uncertain Create published token file")
	}
}

func TestRemoteCredentialFileRejectsBeforeCreate(t *testing.T) {
	root, dir := privateTestRoot(t)
	for _, kind := range []string{"url", "noncanonical", "name", "native_principal", "future_principal", "create"} {
		store := new(remoteFileCredentialStore)
		endpoint := remoteFileEndpoint()
		request := remoteFileRequest()
		name := "remote.json"
		switch kind {
		case "url":
			endpoint.PublicURL = "http://mcp.example.com/mcp"
		case "noncanonical":
			endpoint.PublicURL = "https://MCP.EXAMPLE.COM:443/mcp"
		case "name":
			name = "../remote.json"
		case "native_principal":
			request.Principal = nativeFixturePrincipal
		case "future_principal":
			request.Principal = archive.RemoteClientPrincipalPrefix + "v2/" + strings.Repeat("0", 32)
		case "create":
			store.createErr = errors.New("private-create-error-canary")
		}
		credential, err := IssueRemoteCredentialFile(context.Background(), store, request, root, name, endpoint)
		if err != ErrCredentialFile || credential.ID != "" || len(store.created) != 0 || len(store.revoked) != 0 {
			t.Fatal("invalid remote issuance reached persistence/publication")
		}
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	store := new(remoteFileCredentialStore)
	if _, err := IssueRemoteCredentialFile(context.Background(), store, remoteFileRequest(), root, "remote.json", remoteFileEndpoint()); err != ErrCredentialFile || len(store.created) != 0 {
		t.Fatal("remote issuance accepted nonprivate root")
	}
}

func remoteFileJSON() string {
	return `{"version":1,"mode":"remote_bearer","credential_id":"` + string(remoteFileFixtureID) + `","public_url":"https://mcp.example.com/mcp","token":"` + fakeToken() + `"}`
}

func TestRemoteCredentialFileStrictDecoderRejectsMalformedAndNativeFormats(t *testing.T) {
	for _, kind := range []string{"valid", "unknown", "native_field", "native_mode", "native_v2", "duplicate", "wrong_case", "id", "id_uppercase", "token", "url", "noncanonical_url", "version", "trailing", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := privateTestRoot(t)
			data := remoteFileJSON()
			switch kind {
			case "unknown":
				data = strings.Replace(data, `"version":1`, `"unknown":0,"version":1`, 1)
			case "native_field":
				data = strings.Replace(data, `"version":1`, `"endpoint":"http://localhost/mcp","version":1`, 1)
			case "native_mode":
				data = strings.Replace(data, "remote_bearer", "local_adapter", 1)
			case "native_v2":
				data = `{"version":2,"credential_id":"` + string(remoteFileFixtureID) + `","endpoint":"http://localhost/mcp","native_socket_path":"/private/tmp/synthetic/working/native.sock","token":"` + fakeToken() + `"}`
			case "duplicate":
				data = strings.Replace(data, `"version":1`, `"version":1,"version":1`, 1)
			case "wrong_case":
				data = strings.Replace(data, `"mode"`, `"Mode"`, 1)
			case "id":
				data = strings.Replace(data, string(remoteFileFixtureID), "credential-one", 1)
			case "id_uppercase":
				data = strings.Replace(data, string(remoteFileFixtureID), strings.ToUpper(string(remoteFileFixtureID)), 1)
			case "token":
				data = strings.Replace(data, fakeToken(), "invalid-raw-token", 1)
			case "url":
				data = strings.Replace(data, "https://mcp.example.com/mcp", "https://127.0.0.1/mcp", 1)
			case "noncanonical_url":
				data = strings.Replace(data, "https://mcp.example.com/mcp", "https://MCP.EXAMPLE.COM:443/mcp", 1)
			case "version":
				data = strings.Replace(data, `"version":1`, `"version":2`, 1)
			case "trailing":
				data += " {}"
			case "oversize":
				data = strings.Repeat(" ", 4097)
			}
			if err := root.WriteFile("remote.json", []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			config, id, err := ReadRemoteCredentialFileRoot(root, "remote.json")
			if (err == nil) != (kind == "valid") {
				t.Fatal("remote decoder accepted invalid schema or rejected valid file")
			}
			if kind != "valid" && (config != (RemoteCredentialConfig{}) || id != "" || err != ErrCredentialFile) {
				t.Fatal("failed decode returned partial secret/authority")
			}
		})
	}
}

func TestRemoteCredentialFileRejectsUnsafeEntriesAndKeepsAnchoredRoot(t *testing.T) {
	root, dir := privateTestRoot(t)
	if err := root.WriteFile("remote.json", []byte(remoteFileJSON()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "remote.json"), filepath.Join(dir, "symlink.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRemoteCredentialFileRoot(root, "symlink.json"); err != ErrCredentialFile {
		t.Fatal("remote reader followed symlink")
	}
	if err := os.Link(filepath.Join(dir, "remote.json"), filepath.Join(dir, "hardlink.json")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"remote.json", "hardlink.json"} {
		if _, _, err := ReadRemoteCredentialFileRoot(root, name); err != ErrCredentialFile {
			t.Fatal("remote reader accepted multiply-linked secret")
		}
	}
	if err := root.Remove("hardlink.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "remote.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRemoteCredentialFileRoot(root, "remote.json"); err != ErrCredentialFile {
		t.Fatal("remote reader accepted public secret")
	}
	if err := os.Chmod(filepath.Join(dir, "remote.json"), 0600); err != nil {
		t.Fatal(err)
	}
	config, id, err := ReadRemoteCredentialFileRoot(root, "remote.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir + "-moved") })
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "remote.json"), []byte("replacement-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	anchored, anchoredID, err := ReadRemoteCredentialFileRoot(root, "remote.json")
	if err != nil || anchoredID != id || anchored != config {
		t.Fatal("remote reader followed replaced parent path")
	}
	for _, name := range []string{"", ".", "..", "../remote.json", "/remote.json", "child/remote.json"} {
		if _, id, err := ReadRemoteCredentialFileRoot(root, name); err != ErrCredentialFile || id != "" {
			t.Fatal("remote reader accepted unsafe basename")
		}
	}
}

func TestNormalizeRemoteURLUsesCanonicalPublicRouteRules(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://MCP.EXAMPLE.COM:443/mcp", "https://mcp.example.com/mcp"},
		{"https://mcp.example.com.:08443/mcp", "https://mcp.example.com:8443/mcp"},
		{"https://8.8.8.8/mcp", "https://8.8.8.8/mcp"},
		{"https://[2001:4860:4860::8888]/mcp", "https://[2001:4860:4860::8888]/mcp"},
	} {
		got, err := NormalizeRemoteURL(test.input)
		if err != nil || got != test.want {
			t.Fatal("shared public route normalization changed")
		}
	}
	for _, input := range []string{"http://mcp.example.com/mcp", "https://localhost/mcp", "https://private.local/mcp", "https://127.0.0.1/mcp", "https://192.168.1.1/mcp", "https://[::1]/mcp", "https://2130706433/mcp", "https://0x7f.1/mcp", "https://127.1/mcp", "https://example.123/mcp", "https://user:secret@mcp.example.com/mcp", "https://mcp.example.com/mcp?token=canary", "https://mcp.example.com/mcp#", "https://mcp.example.com/admin", "https://mcp.example.com/%6dcp", "https://mcp.example.com:0/mcp", "https://mcp.example.com:65536/mcp", "https://mcp.example.com:/mcp", " https://mcp.example.com/mcp", "https://mcp.example.com/mcp\n", strings.Repeat("x", 4097)} {
		got, err := NormalizeRemoteURL(input)
		if err != ErrConfiguration || got != "" {
			t.Fatal("invalid public route returned value or private error")
		}
	}
}
