package workerlife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
)

// closeLeaseLocked keeps every partial checkpoint in memory. A successful
// unlink/close is never repeated against a replacement inode on a retry.
func (namespace *ExecutableNamespace) closeLeaseLocked(lease *namespaceExecutableLease) error {
	if lease.closed {
		return nil
	}
	lease.ready = false
	if err := namespaceVerifyAnchor(namespace.anchor); err != nil {
		return ErrCleanup
	}
	if lease.name != "" && !lease.removed {
		if lease.directory == nil {
			file, err := namespaceOpenDirectory(namespace.anchor.file, lease.name)
			if err != nil {
				return ErrCleanup
			}
			lease.directory = file
		}
		if lease.directoryIdentity.Inode == 0 {
			identity, err := namespaceStat(lease.directory)
			if err != nil {
				return ErrCleanup
			}
			lease.directoryIdentity = identity
		}
		if lease.directoryIdentity.Device != namespace.anchor.identity.Device || !namespaceNoExtendedACL(lease.directory) {
			return ErrCleanup
		}
		if err := namespaceSameDirectory(namespace.anchor.file, lease.name, lease.directoryIdentity); err != nil {
			return ErrCleanup
		}
		names, err := namespaceNames(lease.directory, 2)
		if err != nil {
			return ErrCleanup
		}
		present := make(map[string]bool, len(names))
		for _, name := range names {
			if name != "manifest.json" && name != "helper" {
				return ErrCleanup
			}
			present[name] = true
		}
		if err := namespace.bindCleanupFile(lease, "manifest.json", present["manifest.json"]); err != nil {
			return ErrCleanup
		}
		if err := namespace.bindCleanupFile(lease, "helper", present["helper"]); err != nil {
			return ErrCleanup
		}
		// Validate every remaining inode before deleting any of the known set.
		if lease.manifestFile != nil && !lease.manifestRemoved && namespaceSameFile(lease.directory, "manifest.json", lease.manifestIdentity) != nil {
			return ErrCleanup
		}
		if lease.executable != nil && !lease.executableRemoved && namespaceSameFile(lease.directory, "helper", lease.executableIdentity) != nil {
			return ErrCleanup
		}
		if lease.executable != nil && !lease.executableRemoved {
			if err := namespace.unlinkFile(lease.directory, "helper"); err != nil {
				return ErrCleanup
			}
			lease.executableRemoved = true
		}
		// Keep the ownership record until helper absence is durable. Each
		// checkpoint survives a live retry independently of later cleanup steps.
		if !lease.helperSynced {
			if err := namespace.syncDirectory(lease.directory); err != nil {
				return ErrCleanup
			}
			lease.helperSynced = true
		}
		if lease.manifestFile != nil && !lease.manifestRemoved {
			if err := namespace.unlinkFile(lease.directory, "manifest.json"); err != nil {
				return ErrCleanup
			}
			lease.manifestRemoved = true
		}
		if !lease.manifestSynced {
			if err := namespace.syncDirectory(lease.directory); err != nil {
				return ErrCleanup
			}
			lease.manifestSynced = true
		}
		if err := namespace.closeFileLocked(lease.executable, &lease.executableClosed); err != nil {
			return ErrCleanup
		}
		if err := namespace.closeFileLocked(lease.manifestFile, &lease.manifestClosed); err != nil {
			return ErrCleanup
		}
		if err := namespaceSameDirectory(namespace.anchor.file, lease.name, lease.directoryIdentity); err != nil {
			return ErrCleanup
		}
		if err := namespace.removeDirectory(namespace.anchor.file, lease.name); err != nil {
			return ErrCleanup
		}
		lease.removed = true
	}
	if !lease.synced {
		if err := namespace.syncDirectory(namespace.anchor.file); err != nil {
			return ErrCleanup
		}
		lease.synced = true
	}
	if err := namespace.closeFileLocked(lease.executable, &lease.executableClosed); err != nil {
		return ErrCleanup
	}
	if err := namespace.closeFileLocked(lease.manifestFile, &lease.manifestClosed); err != nil {
		return ErrCleanup
	}
	if err := namespace.closeFileLocked(lease.directory, &lease.directoryClosed); err != nil {
		return ErrCleanup
	}
	if !namespaceNil(lease.growth) {
		if err := lease.growth.Release(); err != nil {
			return ErrCleanup
		}
		lease.growth = nil
	}
	lease.closed = true
	delete(namespace.leases, lease)
	return nil
}

func (namespace *ExecutableNamespace) bindCleanupFile(lease *namespaceExecutableLease, name string, present bool) error {
	file, identity, removed := &lease.executable, &lease.executableIdentity, lease.executableRemoved
	maximum := lease.record.Bytes
	if name == "manifest.json" {
		file, identity, removed = &lease.manifestFile, &lease.manifestIdentity, lease.manifestRemoved
		maximum = namespaceManifestMaximum
	}
	if removed {
		if present {
			return ErrCleanup
		}
		return nil
	}
	if !present {
		if *file != nil {
			return ErrCleanup
		}
		return nil
	}
	if *file == nil {
		opened, err := namespaceOpenFile(lease.directory, name, false)
		if err != nil {
			return ErrCleanup
		}
		*file = opened
	}
	current, err := namespaceStat(*file)
	if err != nil || current.Device != namespace.anchor.identity.Device || current.Device != lease.directoryIdentity.Device || !namespaceOwnedFile(current, maximum) || !namespaceNoExtendedACL(*file) {
		return ErrCleanup
	}
	if name == "manifest.json" && current.Mode&07777 != 0600 {
		return ErrCleanup
	}
	if identity.Inode == 0 {
		*identity = current
	} else if !identity.stable(current) {
		return ErrCleanup
	}
	return nil
}

func (namespace *ExecutableNamespace) closeFileLocked(file *os.File, closed *bool) error {
	if file == nil || *closed {
		return nil
	}
	if err := namespace.closeFile(file); err != nil && !errors.Is(err, os.ErrClosed) {
		return ErrCleanup
	}
	*closed = true
	return nil
}

// Recover performs bounded reads, known-entry deletion and fsync only. It never
// admits new growth or writes a journal. The app must already own its instance
// lock and have observed extraction recovery EOF before calling it.
func (namespace *ExecutableNamespace) Recover(ctx context.Context) error {
	if namespace == nil || ctx == nil {
		return ErrProtocol
	}
	namespace.mu.Lock()
	defer namespace.mu.Unlock()
	if namespace.closed || len(namespace.leases) != 0 {
		return ErrCleanup
	}
	namespace.recovered = false
	namespace.recoveryFailed = true
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := namespaceVerifyAnchor(namespace.anchor); err != nil {
		return ErrCleanup
	}
	// A previous live cleanup may have removed its manifest before a later
	// fsync/rmdir/close failure. Retry its retained descriptor checkpoints before
	// interpreting the now manifest-less directory as a new on-disk record.
	if namespace.pendingRecovery != nil {
		if err := namespace.closeLeaseLocked(namespace.pendingRecovery); err != nil {
			return ErrCleanup
		}
		namespace.pendingRecovery = nil
	}
	names, err := namespaceNames(namespace.anchor.file, namespaceEntryMaximum)
	if err != nil {
		return ErrCleanup
	}
	sort.Strings(names)
	for _, name := range names {
		token, owned := namespaceToken(name)
		if !owned {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := namespace.recoverDirectory(ctx, name, token); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return ErrCleanup
		}
	}
	if err := namespace.anchor.file.Sync(); err != nil {
		return ErrCleanup
	}
	namespace.recovered = true
	namespace.recoveryFailed = false
	return nil
}

func (namespace *ExecutableNamespace) recoverDirectory(ctx context.Context, name, token string) error {
	directory, err := namespaceOpenDirectory(namespace.anchor.file, name)
	if err != nil {
		return ErrCleanup
	}
	lease := &namespaceExecutableLease{namespace: namespace, name: name, directory: directory}
	// Recovery read descriptors are not guard ownership. Close them if the
	// on-disk record is unknown. Once destructive cleanup starts, keep the whole
	// lease in memory so unlink/close checkpoints remain retryable.
	defer func() {
		if namespace.pendingRecovery == lease {
			return
		}
		if !lease.directoryClosed {
			_ = directory.Close()
		}
		if lease.manifestFile != nil && !lease.manifestClosed {
			_ = lease.manifestFile.Close()
		}
		if lease.executable != nil && !lease.executableClosed {
			_ = lease.executable.Close()
		}
	}()
	lease.directoryIdentity, err = namespaceStat(directory)
	if err != nil || lease.directoryIdentity.Device != namespace.anchor.identity.Device {
		return ErrCleanup
	}
	names, err := namespaceNames(directory, 2)
	if err != nil {
		return ErrCleanup
	}
	manifestPresent, helperPresent := false, false
	for _, entry := range names {
		switch entry {
		case "manifest.json":
			manifestPresent = true
		case "helper":
			helperPresent = true
		default:
			return ErrCleanup
		}
	}
	if !manifestPresent {
		// The instance-owner/recovery-EOF caller proof permits deleting only an
		// empty, strictly named, verified private directory without a manifest.
		if len(names) != 0 {
			return ErrCleanup
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		namespace.pendingRecovery = lease
		if err := namespace.closeLeaseLocked(lease); err != nil {
			return err
		}
		namespace.pendingRecovery = nil
		return nil
	}
	lease.manifestFile, err = namespaceOpenFile(directory, "manifest.json", false)
	if err != nil {
		return ErrCleanup
	}
	lease.manifestIdentity, err = namespaceStat(lease.manifestFile)
	if err != nil || lease.manifestIdentity.Device != namespace.anchor.identity.Device || lease.manifestIdentity.Size > namespaceManifestMaximum {
		return ErrCleanup
	}
	lease.record, err = decodeNamespaceManifest(io.NewSectionReader(lease.manifestFile, 0, lease.manifestIdentity.Size))
	if err != nil {
		return ErrCleanup
	}
	if lease.record.Version != 1 || lease.record.Token != token || !namespaceHex(lease.record.SHA256, 64) || lease.record.Bytes <= 0 || lease.record.Bytes > namespaceMaximumExecutable || lease.record.DirectoryDevice != lease.directoryIdentity.Device || lease.record.DirectoryInode != lease.directoryIdentity.Inode {
		return ErrCleanup
	}
	published := strings.HasPrefix(name, "workerlife-exec-")
	if lease.record.FileInode == 0 {
		if lease.record.FileDevice != 0 || published {
			return ErrCleanup
		}
	} else if lease.record.FileDevice != namespace.anchor.identity.Device {
		return ErrCleanup
	}
	if helperPresent {
		lease.executable, err = namespaceOpenFile(directory, "helper", false)
		if err != nil {
			return ErrCleanup
		}
		lease.executableIdentity, err = namespaceStat(lease.executable)
		if err != nil || lease.executableIdentity.Device != namespace.anchor.identity.Device || !namespaceOwnedFile(lease.executableIdentity, lease.record.Bytes) {
			return ErrCleanup
		}
		if lease.record.FileInode != 0 && (lease.record.FileDevice != lease.executableIdentity.Device || lease.record.FileInode != lease.executableIdentity.Inode) {
			return ErrCleanup
		}
		if published {
			if lease.executableIdentity.Size != lease.record.Bytes || lease.executableIdentity.Mode&07777 != 0500 {
				return ErrCleanup
			}
			digest, err := namespaceHash(ctx, lease.executable, lease.record.Bytes)
			if err != nil {
				return err
			}
			if digest != lease.record.SHA256 {
				return ErrCleanup
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	namespace.pendingRecovery = lease
	if err := namespace.closeLeaseLocked(lease); err != nil {
		return err
	}
	namespace.pendingRecovery = nil
	return nil
}

func decodeNamespaceManifest(reader io.Reader) (namespaceManifest, error) {
	var record namespaceManifest
	data, err := io.ReadAll(io.LimitReader(reader, namespaceManifestMaximum+1))
	if err != nil || int64(len(data)) > namespaceManifestMaximum {
		return record, ErrCleanup
	}
	// Reject duplicate keys before decoding the fixed schema. A corrupt record
	// must not silently acquire the last value and authorize destructive recovery.
	keys := json.NewDecoder(bytes.NewReader(data))
	opening, err := keys.Token()
	if err != nil || opening != json.Delim('{') {
		return record, ErrCleanup
	}
	seen := make(map[string]bool)
	for keys.More() {
		token, err := keys.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return record, ErrCleanup
		}
		switch key {
		case "version", "token", "bytes", "sha256", "directory_device", "directory_inode", "file_device", "file_inode":
		default:
			return record, ErrCleanup
		}
		seen[key] = true
		var value json.RawMessage
		if err := keys.Decode(&value); err != nil {
			return record, ErrCleanup
		}
	}
	closing, err := keys.Token()
	if err != nil || closing != json.Delim('}') {
		return record, ErrCleanup
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, ErrCleanup
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return record, ErrCleanup
	}
	return record, nil
}
