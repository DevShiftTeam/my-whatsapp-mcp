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

type fileCredentialStore struct {
	created   []archive.CredentialCreate
	revoked   []archive.CredentialID
	createErr error
	revokeErr error
}

func (s *fileCredentialStore) CreateCredential(_ context.Context, c archive.CredentialCreate) (archive.Credential, error) {
	if s.createErr != nil {
		return archive.Credential{}, s.createErr
	}
	s.created = append(s.created, c)
	return archive.Credential{ID: "credential-one", Revision: 1, Label: c.Label, Principal: c.Principal, Policy: c.Policy}, nil
}
func (*fileCredentialStore) ResolveCredential(context.Context, [32]byte) (archive.CredentialGrant, error) {
	return archive.CredentialGrant{}, archive.ErrUnauthorized
}
func (*fileCredentialStore) ResolveCredentialID(context.Context, archive.CredentialID) (archive.CredentialGrant, error) {
	return archive.CredentialGrant{}, archive.ErrUnauthorized
}
func (*fileCredentialStore) SetCredentialPolicy(context.Context, archive.CredentialID, archive.CredentialPolicy, uint64) (archive.Credential, error) {
	return archive.Credential{}, archive.ErrUnauthorized
}
func (s *fileCredentialStore) RevokeCredential(_ context.Context, id archive.CredentialID, revision uint64) error {
	if revision != 1 {
		return archive.ErrUnauthorized
	}
	s.revoked = append(s.revoked, id)
	return s.revokeErr
}
func (*fileCredentialStore) CheckCredentialGrant(context.Context, archive.CredentialGrant) error {
	return archive.ErrUnauthorized
}

func privateTestRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func fileNativeEndpoint() NativeEndpoint {
	return NativeEndpoint{SocketPath: "/private/tmp/wa-indexer-synthetic/working/native.sock"}
}

func TestIssueCredentialFileRandomPrivateDigestOnly(t *testing.T) {
	root, dir := privateTestRoot(t)
	store := new(fileCredentialStore)
	request := archive.CredentialCreate{Label: "native adapter", Principal: "adapter-owner", Digest: [32]byte{7}, Policy: archive.CredentialPolicy{Account: "private-account", AllChats: true}}
	credential, err := IssueCredentialFile(context.Background(), store, request, root, "adapter.json", fileNativeEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	if credential.ID != "credential-one" || len(store.created) != 1 {
		t.Fatalf("credential not persisted: %#v", credential)
	}
	config, err := ReadCredentialFile(filepath.Join(dir, "adapter.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(config.Token)
	if err != nil || len(raw) != 32 {
		t.Fatal("token is not 256 bits")
	}
	if store.created[0].Digest != sha256.Sum256([]byte(config.Token)) || store.created[0].Digest == request.Digest {
		t.Fatal("stored digest does not match freshly issued token")
	}
	info, err := os.Stat(filepath.Join(dir, "adapter.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file is not owner-only")
	}
	data, err := os.ReadFile(filepath.Join(dir, "adapter.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-account") || strings.Contains(string(data), "adapter-owner") {
		t.Fatal("file leaks archive authority")
	}
	var persisted credentialFile
	if json.Unmarshal(data, &persisted) != nil || persisted.Version != 2 {
		t.Fatal("issuance did not write native format v2")
	}
	if config.Endpoint != NativeAdapterEndpoint || config.NativeSocketPath != fileNativeEndpoint().SocketPath {
		t.Fatal("issued native file does not bind the Unix endpoint")
	}
	if _, err := IssueCredentialFile(context.Background(), store, request, root, "second.json", fileNativeEndpoint()); err != nil {
		t.Fatal(err)
	}
	second, err := ReadCredentialFile(filepath.Join(dir, "second.json"))
	if err != nil || second.Token == config.Token || store.created[1].Digest == store.created[0].Digest {
		t.Fatal("independent issuance reused a token")
	}
}

func TestCredentialFileRefusesReplacementAndRevokesUnwrittenGrant(t *testing.T) {
	root, dir := privateTestRoot(t)
	file := filepath.Join(dir, "adapter.json")
	if err := os.WriteFile(file, []byte("owner-existing-file"), 0600); err != nil {
		t.Fatal(err)
	}
	store := new(fileCredentialStore)
	credential, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{}, root, "adapter.json", fileNativeEndpoint())
	if !errors.Is(err, ErrCredentialFile) || len(store.revoked) != 1 || credential.ID != "credential-one" || credential.Revision != 1 {
		t.Fatalf("failed write left grant active: %v", err)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "owner-existing-file" {
		t.Fatal("existing credential file was changed")
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatal("failure exposed owner path")
	}
}

func TestCredentialFilePublicationFailurePreservesUnrevokedGrantForRecovery(t *testing.T) {
	root, dir := privateTestRoot(t)
	file := filepath.Join(dir, "adapter.json")
	if err := os.WriteFile(file, []byte("owner-existing-file"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &fileCredentialStore{revokeErr: errors.New("private-cleanup-error-canary")}
	credential, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{}, root, "adapter.json", fileNativeEndpoint())
	if !errors.Is(err, ErrCredentialCleanup) || credential.ID != "credential-one" || credential.Revision != 1 || len(store.revoked) != 1 {
		t.Fatalf("cleanup failure lost recovery identity: %v", err)
	}
	if err.Error() != "mcp_credential_cleanup_required" {
		t.Fatal("cleanup failure exposed private diagnostics")
	}
	data, readErr := os.ReadFile(file)
	if readErr != nil || string(data) != "owner-existing-file" {
		t.Fatal("cleanup changed a pre-existing owner file")
	}
}

func TestCredentialFileRejectsHostileEndpointsAndPathsBeforeIssuance(t *testing.T) {
	root, _ := privateTestRoot(t)
	for _, path := range []string{"", "native.sock", "../native.sock", "/private/tmp/not-native.sock", "/private/tmp//native.sock", "/private/tmp/../native.sock", "/private/tmp/native.sock\x00", "/" + strings.Repeat("x", 103) + "/native.sock"} {
		store := new(fileCredentialStore)
		_, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{}, root, "adapter.json", NativeEndpoint{SocketPath: path})
		if !errors.Is(err, ErrCredentialFile) || len(store.created) != 0 {
			t.Fatal("accepted hostile native socket endpoint")
		}
	}
	for _, name := range []string{"", ".", "..", "../escape.json", "child/adapter.json", "child\\adapter.json", "/adapter.json"} {
		store := new(fileCredentialStore)
		_, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{}, root, name, fileNativeEndpoint())
		if !errors.Is(err, ErrCredentialFile) || len(store.created) != 0 {
			t.Fatalf("accepted unsafe file name %q", name)
		}
	}
}

func TestReadCredentialFileRejectsPermissionsSymlinksAndMalformedJSON(t *testing.T) {
	_, dir := privateTestRoot(t)
	file := filepath.Join(dir, "adapter.json")
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	valid := map[string]any{"version": 1, "credential_id": "credential-one", "endpoint": "http://127.0.0.1:9876/mcp", "token": token}
	data, _ := json.Marshal(valid)
	if err := os.WriteFile(file, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialFile(file); !errors.Is(err, ErrCredentialFile) {
		t.Fatal("read world-readable token")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialFile(link); !errors.Is(err, ErrCredentialFile) {
		t.Fatal("followed credential symlink")
	}
	for _, bad := range []string{string(data) + " {}", strings.Replace(string(data), "\"version\":1", "\"version\":2", 1), strings.Replace(string(data), "\"version\":1", "\"unknown\":1,\"version\":1", 1), strings.Replace(string(data), token, "raw-secret", 1), strings.Repeat(" ", 4097)} {
		if err := os.WriteFile(file, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadCredentialFile(file); !errors.Is(err, ErrCredentialFile) {
			t.Fatal("accepted malformed or oversized credential JSON")
		}
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialFile(file); !errors.Is(err, ErrCredentialFile) {
		t.Fatal("read token in non-private parent")
	}
}

func TestReadCredentialFileRequiresAbsoluteOwnerConfigPath(t *testing.T) {
	for _, path := range []string{"", "adapter.json", "./adapter.json", "../adapter.json"} {
		if _, err := ReadCredentialFile(path); !errors.Is(err, ErrCredentialFile) {
			t.Fatal("accepted relative native configuration path")
		}
	}
}

func TestReadCredentialFileRootKeepsAnchoredFileAndReturnsIdentity(t *testing.T) {
	root, dir := privateTestRoot(t)
	store := new(fileCredentialStore)
	credential, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{}, root, "adapter.json", fileNativeEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	config, id, err := ReadCredentialFileRoot(root, "adapter.json")
	if err != nil || id != credential.ID || !validAdapterToken(config.Token) {
		t.Fatal("rooted credential read lost published identity")
	}
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir + "-moved") })
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "adapter.json"), []byte("replacement-private-file"), 0600); err != nil {
		t.Fatal(err)
	}
	anchored, anchoredID, err := ReadCredentialFileRoot(root, "adapter.json")
	if err != nil || anchoredID != id || anchored != config {
		t.Fatal("rooted credential read followed a replaced parent pathname")
	}
}

func TestReadCredentialFileRootRejectsEscapesAndUntrustedEntries(t *testing.T) {
	root, dir := privateTestRoot(t)
	for _, name := range []string{"", ".", "..", "../adapter.json", "/adapter.json", "child/adapter.json"} {
		if _, id, err := ReadCredentialFileRoot(root, name); err != ErrCredentialFile || id != "" {
			t.Fatal("rooted reader accepted an unsafe name")
		}
	}
	if _, _, err := ReadCredentialFileRoot(nil, "adapter.json"); err != ErrCredentialFile {
		t.Fatal("rooted reader accepted missing authority")
	}
	file := filepath.Join(dir, "adapter.json")
	data, err := json.Marshal(credentialFile{Version: 1, CredentialID: "credential-one", Endpoint: "http://127.0.0.1:9876/mcp", Token: fakeToken()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCredentialFileRoot(root, "adapter.json"); err != ErrCredentialFile {
		t.Fatal("rooted reader accepted nonprivate token")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("adapter.json", filepath.Join(dir, "alias.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCredentialFileRoot(root, "alias.json"); err != ErrCredentialFile {
		t.Fatal("rooted reader followed a token symlink")
	}
}

func TestRewriteCredentialFileRootMigratesLegacyWithoutCreatingGrantOrToken(t *testing.T) {
	root, dir := privateTestRoot(t)
	legacy := credentialFile{Version: 1, CredentialID: "credential-one", Endpoint: "http://127.0.0.1:9876/mcp", Token: fakeToken()}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	name := "native-00000000000000000000000000000000.json"
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	expected, id, err := ReadCredentialFileRoot(root, name)
	if err != nil || expected.NativeSocketPath != "" {
		t.Fatal("legacy recovery read failed")
	}
	before, err := root.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := RewriteCredentialFileRoot(root, name, id, expected, fileNativeEndpoint()); err != nil {
		t.Fatal(err)
	}
	after, err := root.Lstat(name)
	if err != nil || os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatal("migration did not publish a new private inode")
	}
	current, currentID, err := ReadCredentialFileRoot(root, name)
	if err != nil || currentID != id || current.Token != expected.Token || current.Endpoint != NativeAdapterEndpoint || current.NativeSocketPath != fileNativeEndpoint().SocketPath {
		t.Fatal("migration changed native authority instead of transport")
	}
	data, err = os.ReadFile(filepath.Join(dir, name))
	var migrated credentialFile
	if err != nil || json.Unmarshal(data, &migrated) != nil || migrated.Version != 2 || strings.Contains(string(data), "9876") {
		t.Fatal("migration retained legacy TCP configuration")
	}
}

func TestRewriteCredentialFileRootRequiresVerifiedIdentityAndToken(t *testing.T) {
	root, dir := privateTestRoot(t)
	if _, err := IssueCredentialFile(context.Background(), new(fileCredentialStore), archive.CredentialCreate{}, root, "adapter.json", fileNativeEndpoint()); err != nil {
		t.Fatal(err)
	}
	expected, id, err := ReadCredentialFileRoot(root, "adapter.json")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "adapter.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"wrong_id", "wrong_token", "wrong_socket", "unsafe_name", "invalid_destination"} {
		wantID, wantConfig, name, endpoint := id, expected, "adapter.json", fileNativeEndpoint()
		switch kind {
		case "wrong_id":
			wantID = "different-credential"
		case "wrong_token":
			entropy, decodeErr := base64.RawURLEncoding.DecodeString(expected.Token)
			if decodeErr != nil || len(entropy) != 32 {
				t.Fatal("issued token invalid")
			}
			entropy[0] ^= 1
			wantConfig.Token = base64.RawURLEncoding.EncodeToString(entropy)
		case "wrong_socket":
			wantConfig.NativeSocketPath = "/private/tmp/different/working/native.sock"
		case "unsafe_name":
			name = "../adapter.json"
		case "invalid_destination":
			endpoint.SocketPath = "relative/native.sock"
		}
		if err := RewriteCredentialFileRoot(root, name, wantID, wantConfig, endpoint); err != ErrCredentialFile {
			t.Fatal("rewrite accepted unverified owner handoff")
		}
		after, err := os.ReadFile(filepath.Join(dir, "adapter.json"))
		if err != nil || string(after) != string(before) {
			t.Fatal("failed rewrite changed the existing credential file")
		}
	}
}
