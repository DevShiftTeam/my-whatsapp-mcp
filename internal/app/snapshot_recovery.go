package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// RecoverCommittedSnapshot is an explicit offline owner operation. It acquires
// the same instance and archive leases as the app, without constructing an app,
// opening WhatsApp, loading models, or starting media and indexing workers.
func RecoverCommittedSnapshot(ctx context.Context, dataDir string) (receipt archive.SnapshotRecoveryReceipt, result error) {
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) != dataDir {
		return receipt, appdata.ErrUnsafeDataPath
	}
	paths, err := appdata.ExistingPaths(dataDir)
	if err != nil {
		return receipt, err
	}
	if err := archive.RefuseHistoricalSnapshot(paths.ArchiveDir); err != nil {
		return receipt, err
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		return receipt, err
	}
	defer func() {
		if err := lease.Close(); err != nil {
			result = errors.Join(result, err)
			receipt = archive.SnapshotRecoveryReceipt{}
		}
	}()
	state, exists, err := readState(paths, lease)
	if err != nil {
		return receipt, err
	}
	if !exists {
		return receipt, appdata.ErrDataUnavailable
	}
	if state.Mode != "real" {
		return receipt, appdata.ErrUnsafeDataPath
	}
	// Open must never initialize a new archive after a mistyped profile path.
	// Check only the fixed SQLite header; the linked-device database stays closed.
	for _, path := range []string{filepath.Join(paths.ArchiveDir, "archive.sqlite"), filepath.Join(paths.SessionDir, "session.sqlite")} {
		if err := existingRecoverySQLite(ctx, path); err != nil {
			return receipt, err
		}
	}
	store, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		return receipt, err
	}
	defer func() {
		if err := store.Close(); err != nil {
			result = errors.Join(result, err)
			receipt = archive.SnapshotRecoveryReceipt{}
		}
	}()
	account, err := store.BoundAccount(ctx)
	if err != nil {
		return receipt, err
	}
	if account == "" || account == demoAccount {
		return receipt, archive.ErrUnauthorized
	}
	// Exclusive instance ownership proves prior trusted worker holders are gone.
	// A lease TTL alone never authorizes releasing their retained-original pins.
	recovery, err := recoverExtractionPins(ctx, store, account)
	if err != nil {
		return receipt, err
	}
	for !recovery.complete {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		if _, err := recovery.step(ctx, store); err != nil {
			return receipt, err
		}
	}
	// The archive derives and validates the account, safety reason, and recovery
	// epoch internally; no caller-supplied account or weakened check is accepted.
	return store.RestoreCommittedSnapshot(ctx, "")
}

func existingRecoverySQLite(ctx context.Context, path string) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return appdata.ErrDataUnavailable
	}
	if err != nil || !privateStateFile(info) {
		return appdata.ErrUnsafeDataPath
	}
	if info.Size() < 100 {
		return appdata.ErrDataUnavailable
	}
	file, err := os.OpenFile(path, os.O_RDONLY|stateNoFollow()|syscall.O_NONBLOCK, 0)
	if err != nil {
		return appdata.ErrUnsafeDataPath
	}
	defer func() {
		if err := file.Close(); err != nil {
			result = errors.Join(result, appdata.ErrDataUnavailable)
		}
	}()
	opened, err := file.Stat()
	if err != nil || !privateStateFile(opened) || !os.SameFile(info, opened) || !stateNoExtendedACL(file) {
		return appdata.ErrUnsafeDataPath
	}
	var header [16]byte
	if _, err := io.ReadFull(file, header[:]); err != nil || !bytes.Equal(header[:], []byte("SQLite format 3\x00")) {
		return appdata.ErrDataUnavailable
	}
	after, err := os.Lstat(path)
	if err != nil || !privateStateFile(after) || !os.SameFile(opened, after) {
		return appdata.ErrUnsafeDataPath
	}
	return ctx.Err()
}
