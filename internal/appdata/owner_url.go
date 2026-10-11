package appdata

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// WriteOwnerURL writes the local authenticated attach handoff without printing it.
// The coordinator must hold its instance lease while using this private handoff.
func WriteOwnerURL(paths Paths, lease *InstanceLock, ownerURL string) error {
	if !validOwnerURL(ownerURL) {
		return ErrInvalidOwnerURL
	}
	if lease == nil {
		return ErrLockRequired
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	dir, err := openLeaseRoot(paths, lease)
	if err != nil {
		return err
	}
	defer dir.Close()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ErrDataUnavailable
	}
	// Temporary capabilities belong inside an owned data directory, so reset
	// also removes a handoff interrupted before rename.
	temporary := filepath.Join("working", ".owner-url-"+hex.EncodeToString(random[:]))
	file, err := dir.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag(), 0600)
	if err != nil {
		return ErrDataUnavailable
	}
	defer dir.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return ErrDataUnavailable
	}
	_, writeErr := file.WriteString(ownerURL + "\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrDataUnavailable
	}
	if err := dir.Rename(temporary, ownerURLFile); err != nil {
		return ErrDataUnavailable
	}
	working, err := dir.OpenRoot("working")
	if err != nil {
		return ErrDataUnavailable
	}
	defer working.Close()
	if err := syncDirectory(working); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// RemoveOwnerURL removes only the protected local browser handoff file.
func RemoveOwnerURL(paths Paths, lease *InstanceLock) error {
	if lease == nil {
		return ErrLockRequired
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	dir, err := openLeaseRoot(paths, lease)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Remove(ownerURLFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrDataUnavailable
	}
	return syncDirectory(dir)
}

// ReadOwnerURL reads only a private attach candidate, without acquiring a lease
// or opening archive data. The caller must authenticate the live service and its
// expected mode before using the candidate; a stale file is not proof of liveness.
func ReadOwnerURL(paths Paths) (string, error) {
	canonical, err := canonicalRoot(paths.Root)
	if err != nil {
		return "", err
	}
	if paths != pathsFor(canonical) {
		return "", ErrUnsafeDataPath
	}
	if _, err := ExistingPaths(canonical); err != nil {
		return "", err
	}
	dir, err := openNoSymlinkRoot(canonical, false)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := verifyOwnership(dir); err != nil {
		return "", err
	}
	rootInfo, err := dir.Stat(".")
	if err != nil {
		return "", ErrUnsafeDataPath
	}
	before, err := dir.Lstat(ownerURLFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrDataUnavailable
	}
	if err != nil || !privateFile(before) {
		return "", ErrUnsafeDataPath
	}
	if before.Size() > 4096 {
		return "", ErrInvalidOwnerURL
	}
	file, err := dir.OpenFile(ownerURLFile, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return "", ErrUnsafeDataPath
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !privateFile(opened) || !os.SameFile(before, opened) || !noExtendedACL(file) {
		return "", ErrUnsafeDataPath
	}
	content, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", ErrDataUnavailable
	}
	if len(content) > 4096 {
		return "", ErrInvalidOwnerURL
	}
	ownerURL := strings.TrimSuffix(string(content), "\n")
	if !validOwnerURL(ownerURL) {
		return "", ErrInvalidOwnerURL
	}
	after, err := dir.Lstat(ownerURLFile)
	final, statErr := file.Stat()
	if err != nil || statErr != nil || !privateFile(after) || !privateFile(final) || !os.SameFile(before, after) || !os.SameFile(before, final) || !noExtendedACL(file) {
		return "", ErrUnsafeDataPath
	}
	current, err := openNoSymlinkRoot(canonical, false)
	if err != nil {
		return "", err
	}
	defer current.Close()
	currentRoot, err := current.Stat(".")
	if err != nil || !os.SameFile(rootInfo, currentRoot) {
		return "", ErrUnsafeDataPath
	}
	if err := verifyOwnership(current); err != nil {
		return "", err
	}
	return ownerURL, nil
}

func validOwnerURL(input string) bool {
	if len(input) > 4096 {
		return false
	}
	parsed, err := url.Parse(input)
	if err != nil || parsed.Scheme != "http" || parsed.Opaque != "" || parsed.User != nil || parsed.Path != "/" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.RawFragment != "" {
		return false
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	if err != nil || !address.IsLoopback() || address.Zone() != "" {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	if parsed.Host != net.JoinHostPort(address.String(), strconv.Itoa(port)) || parsed.String() != input {
		return false
	}
	const prefix = "attach="
	if !strings.HasPrefix(parsed.Fragment, prefix) {
		return false
	}
	token := strings.TrimPrefix(parsed.Fragment, prefix)
	if len(token) != 32+1+43 || token[32] != '.' {
		return false
	}
	for _, char := range token[:32] {
		if !(char >= 'a' && char <= 'f' || char >= '0' && char <= '9') {
			return false
		}
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(token[33:])
	return err == nil && len(secret) == 32 && base64.RawURLEncoding.EncodeToString(secret) == token[33:]
}
