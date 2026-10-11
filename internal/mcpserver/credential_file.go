package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

// AdapterConfig is the private native-client handoff to an existing daemon.
// Token must stay in an owner-protected file, never process arguments or logs.
type AdapterConfig struct {
	Endpoint, NativeSocketPath string
	Token                      string
}

const NativeAdapterEndpoint = "http://localhost/mcp"

// NativeEndpoint is selected by the owner daemon, never an HTTP/tool argument.
// Authentication bytes travel only after appdata verifies the Unix peer UID.
type NativeEndpoint struct {
	SocketPath string
}

type credentialFile struct {
	Version          int                  `json:"version"`
	CredentialID     archive.CredentialID `json:"credential_id"`
	Endpoint         string               `json:"endpoint"`
	NativeSocketPath string               `json:"native_socket_path,omitempty"`
	Token            string               `json:"token"`
}

const maxCredentialFileBytes = 4096

// IssueCredentialFile creates one independent named grant and writes its raw
// 256-bit token exactly once. Only the digest crosses the credential-store port.
// A file cannot replace any existing entry, including a symlink. Private roots
// and files refuse every extended ACL, including otherwise harmless deny ACLs.
// After a successful store creation, failures return the created credential so
// the owner can retain its recovery identity. ErrCredentialCleanup means local
// cleanup or revocation failed and requires an explicit owner recovery attempt.
// This helper alone does not provide durable issuance intent across a crash.
func IssueCredentialFile(ctx context.Context, store Credentials, request archive.CredentialCreate, root *os.Root, name string, endpoint NativeEndpoint) (createdCredential archive.Credential, resultErr error) {
	if store == nil || root == nil || !validCredentialFileName(name) || !validNativeSocketPath(endpoint.SocketPath) {
		return archive.Credential{}, ErrCredentialFile
	}
	info, err := root.Stat(".")
	if err != nil || !privateOwner(info, true) {
		return archive.Credential{}, ErrCredentialFile
	}
	directory, err := root.Open(".")
	if err != nil {
		return archive.Credential{}, ErrCredentialFile
	}
	openedRoot, statErr := directory.Stat()
	privateDirectory := statErr == nil && os.SameFile(info, openedRoot) && credentialNoExtendedACL(directory)
	closeErr := directory.Close()
	if !privateDirectory || closeErr != nil {
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
		return archive.Credential{}, ErrCredentialFile
	}
	compensate := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := store.RevokeCredential(cleanupCtx, credential.ID, credential.Revision); err != nil {
			return ErrCredentialCleanup
		}
		return ErrCredentialFile
	}
	data, err := json.Marshal(credentialFile{Version: 2, CredentialID: credential.ID, Endpoint: NativeAdapterEndpoint, NativeSocketPath: endpoint.SocketPath, Token: token})
	if err != nil || len(data) > maxCredentialFileBytes {
		return credential, compensate()
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return credential, compensate()
	}
	complete := false
	closed := false
	defer func() {
		cleanupFailed := false
		if !closed {
			cleanupFailed = file.Close() != nil
		}
		if !complete {
			cleanupFailed = root.Remove(name) != nil || cleanupFailed
			cleanupFailed = compensate() == ErrCredentialCleanup || cleanupFailed
			if cleanupFailed {
				resultErr = ErrCredentialCleanup
			}
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return credential, ErrCredentialFile
	}
	created, err := file.Stat()
	if err != nil || !privateOwner(created, false) || !credentialNoExtendedACL(file) {
		return credential, ErrCredentialFile
	}
	if _, err := file.Write(data); err != nil {
		return credential, ErrCredentialFile
	}
	if err := file.Sync(); err != nil {
		return credential, ErrCredentialFile
	}
	err = file.Close()
	closed = true
	if err != nil {
		return credential, ErrCredentialFile
	}
	directory, err = root.Open(".")
	if err != nil {
		return credential, ErrCredentialFile
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err != nil || closeErr != nil {
		return credential, ErrCredentialFile
	}
	complete = true
	return credential, nil
}

// ReadCredentialFile accepts only a bounded owner-only regular file in a private
// owner directory. Lstat plus same-inode validation rejects symlinks and races
// without relying on platform-specific open flags. Paths must be absolute;
// any extended ACL is refused rather than claiming a general ACL interpreter.
func ReadCredentialFile(path string) (AdapterConfig, error) {
	if !filepath.IsAbs(path) {
		return AdapterConfig{}, ErrCredentialFile
	}
	directory := filepath.Dir(path)
	name := filepath.Base(path)
	if !validCredentialFileName(name) {
		return AdapterConfig{}, ErrCredentialFile
	}
	parent, err := os.Lstat(directory)
	if err != nil || !privateOwner(parent, true) {
		return AdapterConfig{}, ErrCredentialFile
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return AdapterConfig{}, ErrCredentialFile
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil || !privateOwner(rootInfo, true) || !os.SameFile(parent, rootInfo) {
		return AdapterConfig{}, ErrCredentialFile
	}
	config, _, err := ReadCredentialFileRoot(root, name)
	return config, err
}

// ReadCredentialFileRoot uses a borrowed, already anchored owner root. It never
// reopens a parent pathname or closes the caller's root. The identity is for
// private durable publication/recovery checks, never public tool output.
func ReadCredentialFileRoot(root *os.Root, name string) (AdapterConfig, archive.CredentialID, error) {
	config, id, _, err := readCredentialFileRoot(root, name)
	return config, id, err
}

func readCredentialFileRoot(root *os.Root, name string) (AdapterConfig, archive.CredentialID, os.FileInfo, error) {
	if root == nil || !validCredentialFileName(name) {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !privateOwner(rootInfo, true) {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	openedDirectory, err := root.Open(".")
	if err != nil {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	openedInfo, statErr := openedDirectory.Stat()
	privateDirectory := statErr == nil && os.SameFile(rootInfo, openedInfo) && credentialNoExtendedACL(openedDirectory)
	closeErr := openedDirectory.Close()
	if !privateDirectory || closeErr != nil {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	before, err := root.Lstat(name)
	if err != nil || !privateOwner(before, false) {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	file, err := root.Open(name)
	if err != nil {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !privateOwner(opened, false) || !os.SameFile(before, opened) || !credentialNoExtendedACL(file) {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil || len(data) > maxCredentialFileBytes {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored credentialFile
	if err := decoder.Decode(&stored); err != nil || decoder.Decode(new(any)) != io.EOF || stored.CredentialID == "" || len(stored.CredentialID) > 128 || !validAdapterToken(stored.Token) {
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	config := AdapterConfig{Endpoint: stored.Endpoint, NativeSocketPath: stored.NativeSocketPath, Token: stored.Token}
	switch stored.Version {
	case 1: // Accepted only for private recovery/token verification, never dialing.
		if stored.NativeSocketPath != "" || !validAdapterEndpoint(stored.Endpoint) {
			return AdapterConfig{}, "", nil, ErrCredentialFile
		}
	case 2:
		if !validNativeAdapterConfig(config) {
			return AdapterConfig{}, "", nil, ErrCredentialFile
		}
	default:
		return AdapterConfig{}, "", nil, ErrCredentialFile
	}
	return config, stored.CredentialID, opened, nil
}

// RewriteCredentialFileRoot updates only an owner-verified transport handoff.
// The app must durably enter pending state first and serialize this operation
// under its held instance/root lease. It preserves the existing grant and token,
// captures the opened inode, and refuses replacement if that inode changed.
// A failure after rename is uncertain publication; the app keeps its pending
// intent for recovery/revocation instead of treating the grant as ready.
func RewriteCredentialFileRoot(root *os.Root, name string, expectedID archive.CredentialID, expected AdapterConfig, endpoint NativeEndpoint) (resultErr error) {
	if root == nil || !validCredentialFileName(name) || expectedID == "" || !validNativeSocketPath(endpoint.SocketPath) {
		return ErrCredentialFile
	}
	current, id, captured, err := readCredentialFileRoot(root, name)
	if err != nil || id != expectedID || current != expected {
		return ErrCredentialFile
	}
	data, err := json.Marshal(credentialFile{Version: 2, CredentialID: id, Endpoint: NativeAdapterEndpoint, NativeSocketPath: endpoint.SocketPath, Token: current.Token})
	if err != nil || len(data) > maxCredentialFileBytes {
		return ErrCredentialFile
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrCredentialFile
	}
	temporary := ".native-rewrite-" + base64.RawURLEncoding.EncodeToString(nonce[:]) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrCredentialFile
	}
	closed, renamed := false, false
	defer func() {
		cleanupFailed := false
		if !closed {
			cleanupFailed = file.Close() != nil
		}
		if !renamed {
			cleanupFailed = root.Remove(temporary) != nil || cleanupFailed
		}
		if cleanupFailed {
			resultErr = ErrCredentialCleanup
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return ErrCredentialFile
	}
	created, err := file.Stat()
	if err != nil || !privateOwner(created, false) || !credentialNoExtendedACL(file) {
		return ErrCredentialFile
	}
	if _, err := file.Write(data); err != nil {
		return ErrCredentialFile
	}
	if err := file.Sync(); err != nil {
		return ErrCredentialFile
	}
	err = file.Close()
	closed = true
	if err != nil {
		return ErrCredentialFile
	}
	beforeRename, err := root.Lstat(name)
	if err != nil || !privateOwner(beforeRename, false) || !os.SameFile(captured, beforeRename) {
		return ErrCredentialFile
	}
	if err := root.Rename(temporary, name); err != nil {
		return ErrCredentialFile
	}
	renamed = true
	directory, err := root.Open(".")
	if err != nil {
		return ErrCredentialFile
	}
	syncErr, closeErr := directory.Sync(), directory.Close()
	if syncErr != nil || closeErr != nil {
		return ErrCredentialFile
	}
	return nil
}

func validNativeSocketPath(path string) bool {
	return path != "" && len(path) <= 103 && filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Base(path) == "native.sock" && !strings.ContainsRune(path, '\x00')
}

func validNativeAdapterConfig(config AdapterConfig) bool {
	return config.Endpoint == NativeAdapterEndpoint && validNativeSocketPath(config.NativeSocketPath) && validAdapterToken(config.Token)
}

func validCredentialFileName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\\\x00") && filepath.Base(name) == name
}

func privateOwner(info os.FileInfo, directory bool) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if directory {
		if !info.IsDir() || info.Mode().Perm() != 0700 {
			return false
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return false
	}
	// Unix FileInfo exposes Uid; platforms without a numeric owner cannot add
	// this check and retain the strict mode, rooted-file and same-inode checks.
	if uid := os.Getuid(); uid >= 0 {
		value := reflect.ValueOf(info.Sys())
		if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
			value = value.Elem()
		}
		if value.IsValid() && value.Kind() == reflect.Struct {
			field := value.FieldByName("Uid")
			if field.IsValid() && field.CanUint() && field.Uint() != uint64(uid) {
				return false
			}
		}
	}
	return true
}

func validAdapterToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == token
}

func validAdapterEndpoint(endpoint string) bool {
	if len(endpoint) > 512 {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "/mcp" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" || u.ForceQuery || u.Opaque != "" || u.String() != endpoint {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	_, portString, err := net.SplitHostPort(u.Host)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portString)
	return err == nil && port > 0 && port <= 65535 && strconv.Itoa(port) == portString
}
