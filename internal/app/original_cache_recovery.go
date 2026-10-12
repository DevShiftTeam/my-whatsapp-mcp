package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
)

// OriginalCacheRecoveryReceipt contains aggregate results only. Recovery never
// exposes source identifiers, download references, or recording contents.
type OriginalCacheRecoveryReceipt struct {
	Restored int `json:"restored"`
	Skipped  int `json:"skipped"`
}

// RecoverOriginalCache verifies already-present originals against a stopped
// backup and the current source authority. It acquires both profile leases and
// normal archive/media ownership without constructing the app, connecting the
// SDK, reconciling cache files, or starting workers. No original is copied.
func RecoverOriginalCache(ctx context.Context, dataDir, backupDir string) (receipt OriginalCacheRecoveryReceipt, result error) {
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) != dataDir ||
		!filepath.IsAbs(backupDir) || filepath.Clean(backupDir) != backupDir || dataDir == backupDir {
		return receipt, appdata.ErrUnsafeDataPath
	}
	current, err := appdata.ExistingPaths(dataDir)
	if err != nil {
		return receipt, err
	}
	backup, err := appdata.ExistingPaths(backupDir)
	if err != nil {
		return receipt, err
	}
	if err := archive.RefuseHistoricalSnapshot(current.ArchiveDir); err != nil {
		return receipt, err
	}
	ordered := []appdata.Paths{current, backup}
	for _, paths := range ordered {
		// Acquire normally creates a missing lease file. Explicit recovery uses
		// stopped existing profiles and must not create files in the backup.
		info, err := os.Lstat(filepath.Join(paths.Root, ".instance.lock"))
		if errors.Is(err, os.ErrNotExist) {
			return receipt, appdata.ErrDataUnavailable
		}
		if err != nil || !privateStateFile(info) {
			return receipt, appdata.ErrUnsafeDataPath
		}
	}
	if ordered[1].Root < ordered[0].Root {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}
	leases := make([]*appdata.InstanceLock, 0, len(ordered))
	defer func() {
		for i := len(leases) - 1; i >= 0; i-- {
			if err := leases[i].Close(); err != nil {
				result = errors.Join(result, err)
				receipt = OriginalCacheRecoveryReceipt{}
			}
		}
	}()
	for _, paths := range ordered {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		lease, err := appdata.Acquire(paths)
		if err != nil {
			return receipt, err
		}
		leases = append(leases, lease)
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
		for _, path := range []string{filepath.Join(paths.ArchiveDir, "archive.sqlite"), filepath.Join(paths.SessionDir, "session.sqlite")} {
			if err := existingRecoverySQLite(ctx, path); err != nil {
				return receipt, err
			}
		}
	}
	// Export is query-only: an older compatible backup must not be migrated or
	// opened through the ordinary archive writer.
	witnesses, err := archive.ReadOriginalCacheWitnesses(ctx, backup.ArchiveDir)
	if err != nil {
		return receipt, err
	}
	if len(witnesses) > 256 {
		return receipt, archive.ErrLimitExceeded
	}
	store, err := archive.OpenOriginalCacheRecovery(ctx, current.ArchiveDir)
	if err != nil {
		return receipt, err
	}
	defer func() {
		if err := store.Close(); err != nil {
			result = errors.Join(result, err)
			receipt = OriginalCacheRecoveryReceipt{}
		}
	}()
	account, err := store.BoundAccount(ctx)
	if err != nil {
		return receipt, err
	}
	if account == "" || account == demoAccount {
		return receipt, archive.ErrUnauthorized
	}
	for _, witness := range witnesses {
		if witness.Source.Account != account || witness.Key.Account != account {
			return receipt, archive.ErrUnauthorized
		}
	}
	cache, err := media.Open(current, store, media.Options{Admission: originalCacheAdmission{}})
	if err != nil {
		return receipt, err
	}
	defer func() {
		if err := cache.Close(); err != nil {
			result = errors.Join(result, err)
			receipt = OriginalCacheRecoveryReceipt{}
		}
	}()
	for _, witness := range witnesses {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		restored, err := cache.RecoverOriginalCache(ctx, witness)
		if err != nil {
			// A missing, changed, or invalid individual original does not grant
			// authority and stays untouched. Ownership and structural failures
			// stop the operation instead of being reduced to a skipped count.
			if originalCacheRefusal(err) {
				receipt.Skipped++
				continue
			}
			return receipt, err
		}
		if restored {
			receipt.Restored++
		} else {
			receipt.Skipped++
		}
	}
	return receipt, nil
}

func originalCacheRefusal(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !originalCacheRefusal(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return originalCacheRefusal(wrapped.Unwrap())
	}
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, media.ErrSourceChanged) ||
		errors.Is(err, media.ErrAuthentication) || errors.Is(err, media.ErrFormat) || errors.Is(err, media.ErrLimit)
}

// In-place recovery admits no new physical growth. Accidentally invoking a
// download or decoded-file operation through this composition fails closed.
type originalCacheAdmission struct{}

func (originalCacheAdmission) Reserve(context.Context, diskspace.Request) (diskspace.Lease, error) {
	return nil, diskspace.ErrInvalidRequest
}
