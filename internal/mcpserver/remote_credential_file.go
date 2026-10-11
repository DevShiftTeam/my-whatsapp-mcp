package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
)

// RemoteEndpoint is trusted route metadata. The server never dials PublicURL.
type RemoteEndpoint struct{ PublicURL string }

// RemoteCredentialConfig is a protected bearer handoff, never a native adapter.
// Token must not appear in arguments, logs or public connection metadata.
type RemoteCredentialConfig struct{ PublicURL, Token string }

// NormalizeRemoteURL validates trusted public route metadata without network IO.
func NormalizeRemoteURL(value string) (string, error) {
	endpoint, err := clientsetup.NormalizeRemoteURL(value)
	if err != nil {
		return "", ErrConfiguration
	}
	return endpoint, nil
}

type remoteCredentialFile struct {
	Version      int                  `json:"version"`
	Mode         string               `json:"mode"`
	CredentialID archive.CredentialID `json:"credential_id"`
	PublicURL    string               `json:"public_url"`
	Token        string               `json:"token"`
}

const remoteCredentialMode = "remote_bearer"

// IssueRemoteCredentialFile requires a durable app-owned pending intent before
// entry. Post-create failures preserve a known credential identity for recovery.
func IssueRemoteCredentialFile(ctx context.Context, store Credentials, request archive.CredentialCreate, root *os.Root, name string, endpoint RemoteEndpoint) (createdCredential archive.Credential, resultErr error) {
	if !registryConfigured(store) || !validCredentialFileName(name) || !canonicalRemotePrincipal(request.Principal) || !canonicalRemoteURL(endpoint.PublicURL) || !remotePrivateDirectory(root, false) {
		return archive.Credential{}, ErrCredentialFile
	}
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return archive.Credential{}, ErrCredentialFile
	}
	token := base64.RawURLEncoding.EncodeToString(entropy[:])
	request.Digest = sha256.Sum256([]byte(token))
	credential, err := store.CreateCredential(ctx, request)
	if err != nil {
		// A known identity remains useful even if the store reports an uncertain
		// result. The durable app intent performs ordered metadata recovery.
		return credential, ErrCredentialFile
	}
	compensate := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := store.RevokeCredential(cleanupCtx, credential.ID, credential.Revision); err != nil {
			return ErrCredentialCleanup
		}
		return ErrCredentialFile
	}
	if !canonicalClientNonce(string(credential.ID)) || credential.Revision == 0 {
		return credential, compensate()
	}
	data, err := json.Marshal(remoteCredentialFile{Version: 1, Mode: remoteCredentialMode, CredentialID: credential.ID, PublicURL: endpoint.PublicURL, Token: token})
	if err != nil || len(data) > maxCredentialFileBytes {
		return credential, compensate()
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return credential, compensate()
	}
	captured, statErr := file.Stat()
	complete, closed := false, false
	defer func() {
		cleanupFailed := false
		if !closed {
			cleanupFailed = file.Close() != nil
		}
		if !complete {
			current, err := root.Lstat(name)
			if err != nil || captured == nil || !os.SameFile(captured, current) || !privateOwner(current, false) || !remoteSingleLink(current) {
				cleanupFailed = true
			} else if root.Remove(name) != nil || !remotePrivateDirectory(root, true) {
				cleanupFailed = true
			}
			cleanupFailed = compensate() == ErrCredentialCleanup || cleanupFailed
			if cleanupFailed {
				resultErr = ErrCredentialCleanup
			}
		}
	}()
	if statErr != nil || file.Chmod(0600) != nil {
		return credential, ErrCredentialFile
	}
	created, err := file.Stat()
	if err != nil || !privateOwner(created, false) || !remoteSingleLink(created) || !os.SameFile(captured, created) || !credentialNoExtendedACL(file) {
		return credential, ErrCredentialFile
	}
	if count, err := file.Write(data); err != nil || count != len(data) {
		return credential, ErrCredentialFile
	}
	if err := file.Sync(); err != nil {
		return credential, ErrCredentialFile
	}
	finalInfo, err := file.Stat()
	current, currentErr := root.Lstat(name)
	if err != nil || currentErr != nil || !privateOwner(finalInfo, false) || !remoteSingleLink(finalInfo) || !os.SameFile(captured, finalInfo) || !os.SameFile(finalInfo, current) || !credentialNoExtendedACL(file) {
		return credential, ErrCredentialFile
	}
	err = file.Close()
	closed = true
	if err != nil || !remotePrivateDirectory(root, true) {
		return credential, ErrCredentialFile
	}
	complete = true
	return credential, nil
}

// ReadRemoteCredentialFileRoot borrows the already anchored private clients
// root and accepts only the distinct remote bearer format.
func ReadRemoteCredentialFileRoot(root *os.Root, name string) (RemoteCredentialConfig, archive.CredentialID, error) {
	if !validCredentialFileName(name) || !remotePrivateDirectory(root, false) {
		return RemoteCredentialConfig{}, "", ErrCredentialFile
	}
	before, err := root.Lstat(name)
	if err != nil || !privateOwner(before, false) || !remoteSingleLink(before) {
		return RemoteCredentialConfig{}, "", ErrCredentialFile
	}
	file, err := root.Open(name)
	if err != nil {
		return RemoteCredentialConfig{}, "", ErrCredentialFile
	}
	opened, statErr := file.Stat()
	if statErr != nil || !privateOwner(opened, false) || !remoteSingleLink(opened) || !os.SameFile(before, opened) || !credentialNoExtendedACL(file) {
		file.Close()
		return RemoteCredentialConfig{}, "", ErrCredentialFile
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	finalInfo, finalStatErr := file.Stat()
	current, currentErr := root.Lstat(name)
	finalACLPrivate := credentialNoExtendedACL(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > maxCredentialFileBytes || finalStatErr != nil || currentErr != nil || !privateOwner(finalInfo, false) || !remoteSingleLink(finalInfo) || !privateOwner(current, false) || !remoteSingleLink(current) || !os.SameFile(opened, finalInfo) || !os.SameFile(finalInfo, current) || !finalACLPrivate || !remotePrivateDirectory(root, false) {
		return RemoteCredentialConfig{}, "", ErrCredentialFile
	}
	stored, err := decodeRemoteCredential(data)
	if err != nil || stored.Version != 1 || stored.Mode != remoteCredentialMode || !canonicalClientNonce(string(stored.CredentialID)) || !validToken(stored.Token) || !canonicalRemoteURL(stored.PublicURL) {
		return RemoteCredentialConfig{}, "", ErrCredentialFile
	}
	return RemoteCredentialConfig{PublicURL: stored.PublicURL, Token: stored.Token}, stored.CredentialID, nil
}

func canonicalRemoteURL(value string) bool {
	canonical, err := NormalizeRemoteURL(value)
	return err == nil && canonical == value
}

// A borrowed root is checked through its descriptor, never its former pathname.
func remotePrivateDirectory(root *os.Root, syncDirectory bool) bool {
	if root == nil {
		return false
	}
	before, err := root.Stat(".")
	if err != nil || !privateOwner(before, true) {
		return false
	}
	file, err := root.Open(".")
	if err != nil {
		return false
	}
	opened, statErr := file.Stat()
	valid := statErr == nil && privateOwner(opened, true) && os.SameFile(before, opened) && credentialNoExtendedACL(file)
	if valid && syncDirectory {
		valid = file.Sync() == nil
	}
	closeErr := file.Close()
	return valid && closeErr == nil
}

func remoteSingleLink(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	links, owner := value.FieldByName("Nlink"), value.FieldByName("Uid")
	uid := os.Getuid()
	return uid >= 0 && links.IsValid() && links.CanUint() && links.Uint() == 1 && owner.IsValid() && owner.CanUint() && owner.Uint() == uint64(uid)
}

// Exact object keys and duplicates are checked separately from the native
// decoder, so case-insensitive struct matching cannot widen this file format.
func decodeRemoteCredential(data []byte) (remoteCredentialFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return remoteCredentialFile{}, ErrCredentialFile
	}
	var stored remoteCredentialFile
	seen := make(map[string]bool, 5)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return remoteCredentialFile{}, ErrCredentialFile
		}
		seen[name] = true
		switch name {
		case "version":
			err = decoder.Decode(&stored.Version)
		case "mode":
			err = decoder.Decode(&stored.Mode)
		case "credential_id":
			err = decoder.Decode(&stored.CredentialID)
		case "public_url":
			err = decoder.Decode(&stored.PublicURL)
		case "token":
			err = decoder.Decode(&stored.Token)
		default:
			return remoteCredentialFile{}, ErrCredentialFile
		}
		if err != nil {
			return remoteCredentialFile{}, ErrCredentialFile
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 5 || decoder.Decode(new(any)) != io.EOF {
		return remoteCredentialFile{}, ErrCredentialFile
	}
	return stored, nil
}
