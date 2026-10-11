package workerlife

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

// GrowthLease retains admitted storage until all owned bytes are removed.
type GrowthLease interface{ Release() error }

// ExecutableLease owns a private executable snapshot through helper cleanup.
// A nonnil lease returned with an error must also be retained and closed.
type ExecutableLease interface {
	Path() string
	Close() error
}

type ExecutableProvider interface {
	Prepare(context.Context, *os.File, int64) (ExecutableLease, error)
}

// NamespaceConfig borrows an existing app-verified private cache directory.
// The app's existing instance lock and extraction-recovery EOF must precede
// Recover and new launches. This namespace creates no additional lock or DB.
type NamespaceConfig struct {
	Directory *os.File
	Path      string
	Admit     func(context.Context, int64) (GrowthLease, error)
}

const (
	namespaceMaximumExecutable = int64(256 << 20)
	namespaceOverhead          = int64(64 << 10)
	namespaceManifestMaximum   = int64(1024)
	namespaceEntryMaximum      = 1024
)

var errNamespaceCloneUnsupported = errors.New("worker_lifetime_clone_unsupported")

type namespaceIdentity struct {
	Device, Inode                                      uint64
	Mode, Owner                                        uint32
	Links                                              uint64
	Size, Allocated                                    int64
	ModifiedSec, ModifiedNano, ChangedSec, ChangedNano int64
}

func (identity namespaceIdentity) stable(other namespaceIdentity) bool { return identity == other }

type namespaceAnchor struct {
	file     *os.File
	path     string
	identity namespaceIdentity
}

// ExecutableNamespace serializes its own preparation and cleanup operations.
// Active/uncertain leases keep both the directory anchor and growth ownership.
type ExecutableNamespace struct {
	mu                                sync.Mutex
	anchor                            *namespaceAnchor
	admit                             func(context.Context, int64) (GrowthLease, error)
	leases                            map[*namespaceExecutableLease]struct{}
	pendingRecovery                   *namespaceExecutableLease
	recovered, recoveryFailed, closed bool
	clone                             func(*os.File, *os.File, string) error
	copy                              func(context.Context, *os.File, *os.File, int64) error
	unlinkFile                        func(*os.File, string) error
	syncDirectory                     func(*os.File) error
	closeFile                         func(*os.File) error
	removeDirectory                   func(*os.File, string) error
}

type namespaceManifest struct {
	Version         int    `json:"version"`
	Token           string `json:"token"`
	Bytes           int64  `json:"bytes"`
	SHA256          string `json:"sha256"`
	DirectoryDevice uint64 `json:"directory_device"`
	DirectoryInode  uint64 `json:"directory_inode"`
	FileDevice      uint64 `json:"file_device"`
	FileInode       uint64 `json:"file_inode"`
}

type namespaceExecutableLease struct {
	namespace                                                            *ExecutableNamespace
	growth                                                               GrowthLease
	name                                                                 string
	directory, manifestFile, executable                                  *os.File
	handoff                                                              *os.File
	handoffCloseUnknown                                                  bool
	directoryIdentity                                                    namespaceIdentity
	manifestIdentity, executableIdentity                                 namespaceIdentity
	record                                                               namespaceManifest
	ready, removed, synced, directoryClosed, closed                      bool
	directoryCreated                                                     bool
	manifestRemoved, executableRemoved, manifestClosed, executableClosed bool
	helperSynced, manifestSynced                                         bool
}

func NewExecutableNamespace(config NamespaceConfig) (*ExecutableNamespace, error) {
	if config.Directory == nil || config.Admit == nil || !filepath.IsAbs(config.Path) || filepath.Clean(config.Path) != config.Path {
		return nil, ErrProtocol
	}
	anchor, err := namespaceNewAnchor(config.Directory, config.Path)
	if err != nil {
		return nil, err
	}
	return &ExecutableNamespace{anchor: anchor, admit: config.Admit, leases: make(map[*namespaceExecutableLease]struct{}), clone: namespaceClone, copy: namespaceCopy,
		unlinkFile: namespaceUnlinkFile, syncDirectory: func(file *os.File) error { return file.Sync() }, closeFile: func(file *os.File) error { return file.Close() }, removeDirectory: namespaceRemoveDirectory}, nil
}

func (namespace *ExecutableNamespace) Prepare(ctx context.Context, source *os.File, maximumBytes int64) (ExecutableLease, error) {
	if namespace == nil || ctx == nil || source == nil {
		return nil, ErrProtocol
	}
	if maximumBytes <= 0 || maximumBytes > namespaceMaximumExecutable {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	namespace.mu.Lock()
	defer namespace.mu.Unlock()
	if namespace.closed || !namespace.recovered || namespace.recoveryFailed || namespace.pendingRecovery != nil {
		return nil, ErrCleanup
	}
	if err := namespaceVerifyAnchor(namespace.anchor); err != nil {
		return nil, ErrCleanup
	}
	input, err := namespaceDuplicate(source)
	if err != nil {
		return nil, ErrProtocol
	}
	defer input.Close()
	before, err := namespaceStat(input)
	if err != nil || before.Mode&0170000 != 0100000 || before.Size <= 0 || before.Size > maximumBytes {
		return nil, ErrLimit
	}
	if before.Owner != 0 && before.Owner != uint32(os.Geteuid()) {
		return nil, ErrProtocol
	}
	if before.Mode&0022 != 0 || before.Allocated < 0 || before.Allocated > before.Size+namespaceOverhead {
		return nil, ErrProtocol
	}
	if !namespaceNoExtendedACL(input) {
		return nil, ErrProtocol
	}
	growth, admissionErr := namespace.admit(ctx, before.Size+namespaceOverhead)
	if namespaceNil(growth) {
		if admissionErr != nil {
			return nil, admissionErr
		}
		return nil, ErrProtocol
	}
	lease := &namespaceExecutableLease{namespace: namespace, growth: growth, record: namespaceManifest{Version: 1, Bytes: before.Size}}
	namespace.leases[lease] = struct{}{}
	fail := func(cause error) (ExecutableLease, error) {
		if cleanupErr := namespace.closeHandoffLocked(lease); cleanupErr != nil {
			return lease, errors.Join(cause, ErrCleanup)
		}
		if cleanupErr := namespace.closeLeaseLocked(lease); cleanupErr != nil {
			return lease, errors.Join(cause, ErrCleanup)
		}
		return nil, cause
	}
	if admissionErr != nil {
		return fail(admissionErr)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	digest, err := namespaceHash(ctx, input, before.Size)
	if err != nil {
		return fail(err)
	}
	after, err := namespaceStat(input)
	if err != nil || !before.stable(after) {
		return fail(ErrProtocol)
	}
	lease.record.SHA256 = digest
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fail(ErrUnavailable)
	}
	lease.record.Token = hex.EncodeToString(token[:])
	lease.name = "workerlife-stage-" + lease.record.Token
	lease.directory, lease.directoryCreated, err = namespaceCreateDirectory(namespace.anchor.file, lease.name)
	if err != nil {
		if !lease.directoryCreated {
			lease.name = ""
		}
		return fail(ErrUnavailable)
	}
	lease.directoryIdentity, err = namespaceStat(lease.directory)
	if err != nil || lease.directoryIdentity.Device != namespace.anchor.identity.Device {
		return fail(ErrProtocol)
	}
	lease.record.DirectoryDevice, lease.record.DirectoryInode = lease.directoryIdentity.Device, lease.directoryIdentity.Inode
	lease.manifestFile, err = namespaceOpenFile(lease.directory, "manifest.json", true)
	if err != nil {
		return fail(ErrUnavailable)
	}
	lease.manifestIdentity, err = namespaceStat(lease.manifestFile)
	if err != nil || lease.manifestIdentity.Device != namespace.anchor.identity.Device {
		return fail(ErrProtocol)
	}
	if err := namespace.writeManifest(lease); err != nil {
		return fail(err)
	}
	if err := lease.directory.Sync(); err != nil {
		return fail(ErrCleanup)
	}
	if err := namespace.anchor.file.Sync(); err != nil {
		return fail(ErrCleanup)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := namespaceVerifyAnchor(namespace.anchor); err != nil {
		return fail(ErrCleanup)
	}
	if err := namespaceSameDirectory(namespace.anchor.file, lease.name, lease.directoryIdentity); err != nil {
		return fail(ErrCleanup)
	}
	err = namespace.clone(input, lease.directory, "helper")
	copied := false
	if errors.Is(err, errNamespaceCloneUnsupported) {
		copied = true
		lease.executable, err = namespaceOpenFile(lease.directory, "helper", true)
		if err == nil {
			err = namespace.copy(ctx, input, lease.executable, before.Size)
		}
	} else if err == nil {
		lease.executable, err = namespaceOpenFile(lease.directory, "helper", false)
	}
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fail(contextErr)
		}
		return fail(ErrUnavailable)
	}
	if err := lease.executable.Chmod(0500); err != nil {
		return fail(ErrUnavailable)
	}
	if err := lease.executable.Sync(); err != nil {
		return fail(ErrCleanup)
	}
	lease.executableIdentity, err = namespaceStat(lease.executable)
	if err != nil || lease.executableIdentity.Device != namespace.anchor.identity.Device || !namespaceOwnedFile(lease.executableIdentity, before.Size) || lease.executableIdentity.Size != before.Size || lease.executableIdentity.Mode&07777 != 0500 || !namespaceNoExtendedACL(lease.executable) {
		return fail(ErrProtocol)
	}
	copyDigest, err := namespaceHash(ctx, lease.executable, before.Size)
	if err != nil {
		return fail(err)
	}
	if copyDigest != digest {
		return fail(ErrProtocol)
	}
	if err := auditExecutableMachO(ctx, lease.executable, before.Size); err != nil {
		return fail(err)
	}
	copyAfter, err := namespaceStat(lease.executable)
	if err != nil || !lease.executableIdentity.stable(copyAfter) || !namespaceNoExtendedACL(lease.executable) {
		return fail(ErrProtocol)
	}
	after, err = namespaceStat(input)
	if err != nil || !before.stable(after) {
		return fail(ErrProtocol)
	}
	lease.record.FileDevice, lease.record.FileInode = copyAfter.Device, copyAfter.Inode
	if err := namespace.writeManifest(lease); err != nil {
		return fail(err)
	}
	if err := lease.executable.Sync(); err != nil {
		return fail(ErrCleanup)
	}
	if err := lease.directory.Sync(); err != nil {
		return fail(ErrCleanup)
	}
	if copied {
		if err := namespace.readOnlyHandoffLocked(lease); err != nil {
			return fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := namespaceVerifyAnchor(namespace.anchor); err != nil {
		return fail(ErrCleanup)
	}
	if err := namespaceSameDirectory(namespace.anchor.file, lease.name, lease.directoryIdentity); err != nil {
		return fail(ErrCleanup)
	}
	published := "workerlife-exec-" + lease.record.Token
	if err := namespaceRenameDirectory(namespace.anchor.file, lease.name, published); err != nil {
		return fail(ErrCleanup)
	}
	lease.name = published
	if err := namespace.anchor.file.Sync(); err != nil {
		return fail(ErrCleanup)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	lease.ready = true
	return lease, nil
}

// readOnlyHandoffLocked replaces only an owned COPY writer after all writes,
// hashes, and syncs. Both descriptors remain owned until its first Close ACK.
func (namespace *ExecutableNamespace) readOnlyHandoffLocked(lease *namespaceExecutableLease) error {
	expected := lease.executableIdentity
	if namespaceVerifyAnchor(namespace.anchor) != nil || namespaceSameDirectory(namespace.anchor.file, lease.name, lease.directoryIdentity) != nil || namespaceSameFile(lease.directory, "helper", expected) != nil {
		return ErrCleanup
	}
	readOnly, err := namespaceOpenFile(lease.directory, "helper", false)
	if err != nil {
		return ErrUnavailable
	}
	lease.handoff = readOnly
	current, err := namespaceStat(readOnly)
	if err != nil || !expected.stable(current) || !namespaceNoExtendedACL(readOnly) || namespaceSameFile(lease.directory, "helper", expected) != nil {
		return ErrProtocol
	}
	if err := namespace.closeFile(lease.executable); err != nil {
		// An unknown first-close result never authorizes retrying this writer or
		// releasing growth. The partial lease retains its readonly descriptor.
		lease.handoffCloseUnknown = true
		return ErrCleanup
	}
	lease.executable = readOnly
	lease.handoff = nil
	current, err = namespaceStat(readOnly)
	if err != nil || !expected.stable(current) || !namespaceNoExtendedACL(readOnly) || namespaceSameFile(lease.directory, "helper", expected) != nil {
		return ErrProtocol
	}
	return nil
}

func (namespace *ExecutableNamespace) closeHandoffLocked(lease *namespaceExecutableLease) error {
	if lease.handoffCloseUnknown {
		return ErrCleanup
	}
	if lease.handoff != nil {
		if err := namespace.closeFile(lease.handoff); err != nil {
			lease.handoffCloseUnknown = true
			return ErrCleanup
		}
		lease.handoff = nil
	}
	return nil
}

func (namespace *ExecutableNamespace) writeManifest(lease *namespaceExecutableLease) error {
	if lease.directoryIdentity.Device != namespace.anchor.identity.Device || lease.manifestIdentity.Device != namespace.anchor.identity.Device {
		return ErrCleanup
	}
	if namespaceVerifyAnchor(namespace.anchor) != nil || namespaceSameDirectory(namespace.anchor.file, lease.name, lease.directoryIdentity) != nil || namespaceSameFile(lease.directory, "manifest.json", lease.manifestIdentity) != nil || !namespaceNoExtendedACL(lease.manifestFile) {
		return ErrCleanup
	}
	// Even a partial own write changes size/timestamps. Retain the held inode's
	// new bounded metadata so cleanup can retry without accepting a replacement.
	defer func() {
		current, err := namespaceStat(lease.manifestFile)
		if err == nil && current.Device == lease.manifestIdentity.Device && current.Inode == lease.manifestIdentity.Inode {
			lease.manifestIdentity = current
		}
	}()
	data, err := json.Marshal(lease.record)
	if err != nil || int64(len(data)) > namespaceManifestMaximum {
		return ErrLimit
	}
	n, err := lease.manifestFile.WriteAt(data, 0)
	if err != nil || n != len(data) {
		return ErrUnavailable
	}
	if err := lease.manifestFile.Truncate(int64(len(data))); err != nil {
		return ErrUnavailable
	}
	if err := lease.manifestFile.Sync(); err != nil {
		return ErrCleanup
	}
	lease.manifestIdentity, err = namespaceStat(lease.manifestFile)
	if err != nil {
		return ErrProtocol
	}
	return nil
}

func (lease *namespaceExecutableLease) Path() string {
	if lease == nil || lease.namespace == nil {
		return ""
	}
	namespace := lease.namespace
	namespace.mu.Lock()
	defer namespace.mu.Unlock()
	if !lease.ready || lease.closed || lease.removed || namespace.closed || namespaceVerifyAnchor(namespace.anchor) != nil {
		return ""
	}
	if lease.directoryIdentity.Device != namespace.anchor.identity.Device || lease.manifestIdentity.Device != namespace.anchor.identity.Device || lease.executableIdentity.Device != namespace.anchor.identity.Device || namespaceSameDirectory(namespace.anchor.file, lease.name, lease.directoryIdentity) != nil || namespaceSameFile(lease.directory, "manifest.json", lease.manifestIdentity) != nil || namespaceSameFile(lease.directory, "helper", lease.executableIdentity) != nil {
		return ""
	}
	return filepath.Join(namespace.anchor.path, lease.name, "helper")
}

func (lease *namespaceExecutableLease) Close() error {
	if lease == nil || lease.namespace == nil {
		return ErrProtocol
	}
	namespace := lease.namespace
	namespace.mu.Lock()
	defer namespace.mu.Unlock()
	if lease.closed {
		return nil
	}
	lease.ready = false
	if err := namespace.closeHandoffLocked(lease); err != nil {
		return err
	}
	return namespace.closeLeaseLocked(lease)
}

func (namespace *ExecutableNamespace) Close() error {
	if namespace == nil {
		return ErrProtocol
	}
	namespace.mu.Lock()
	defer namespace.mu.Unlock()
	if namespace.closed {
		return nil
	}
	if len(namespace.leases) != 0 || namespace.pendingRecovery != nil || namespace.recoveryFailed {
		return ErrCleanup
	}
	if err := namespaceVerifyAnchor(namespace.anchor); err != nil {
		return ErrCleanup
	}
	if err := namespace.anchor.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return ErrCleanup
	}
	namespace.closed = true
	return nil
}

func namespaceNil(value any) bool {
	if value == nil {
		return true
	}
	reflectValue := reflect.ValueOf(value)
	switch reflectValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflectValue.IsNil()
	default:
		return false
	}
}

func namespaceOwnedFile(identity namespaceIdentity, maximum int64) bool {
	return identity.Mode&0170000 == 0100000 && identity.Owner == uint32(os.Geteuid()) && identity.Links == 1 && identity.Mode&0022 == 0 && identity.Size >= 0 && identity.Size <= maximum && identity.Allocated >= 0 && identity.Allocated <= maximum+namespaceOverhead
}

func namespaceAllocated(blocks int64) int64 {
	if blocks < 0 || blocks > (1<<63-1)/512 {
		return -1
	}
	return blocks * 512
}

func namespaceHash(ctx context.Context, file io.ReaderAt, bytes int64) (string, error) {
	hash := sha256.New()
	if err := namespaceReadExtent(ctx, file, bytes, func(data []byte) error { _, err := hash.Write(data); return err }); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func namespaceCopy(ctx context.Context, source, destination *os.File, bytes int64) error {
	if !namespaceNoExtendedACL(destination) {
		return ErrCleanup
	}
	return namespaceReadExtent(ctx, source, bytes, func(data []byte) error {
		for len(data) != 0 {
			n, err := destination.Write(data)
			if err != nil || n <= 0 || n > len(data) {
				return ErrUnavailable
			}
			data = data[n:]
		}
		return nil
	})
}

func namespaceReadExtent(ctx context.Context, source io.ReaderAt, bytes int64, consume func([]byte) error) error {
	if bytes < 0 || bytes > namespaceMaximumExecutable {
		return ErrLimit
	}
	reader := io.NewSectionReader(source, 0, bytes)
	var buffer [64 << 10]byte
	for remaining := bytes; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		length := int64(len(buffer))
		if remaining < length {
			length = remaining
		}
		n, err := reader.Read(buffer[:int(length)])
		if n <= 0 || int64(n) > remaining {
			return ErrProtocol
		}
		if err := consume(buffer[:n]); err != nil {
			return err
		}
		remaining -= int64(n)
		if err != nil && (err != io.EOF || remaining != 0) {
			return ErrProtocol
		}
	}
	return ctx.Err()
}

func namespaceToken(name string) (string, bool) {
	for _, prefix := range []string{"workerlife-stage-", "workerlife-exec-"} {
		if strings.HasPrefix(name, prefix) {
			token := strings.TrimPrefix(name, prefix)
			return token, namespaceHex(token, 32)
		}
	}
	return "", false
}

func namespaceHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
