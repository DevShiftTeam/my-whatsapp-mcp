//go:build !darwin && !linux

package archive

import (
	"context"
	"os"
)

// Native private-file/locking support must be verified before another OS ships.
func acquireArchiveLock(context.Context, string) (*os.File, error) { return nil, ErrArchiveUnavailable }
