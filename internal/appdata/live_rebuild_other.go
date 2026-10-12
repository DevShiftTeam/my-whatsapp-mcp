//go:build !darwin && !linux

package appdata

import "os"

func duplicateRebuildLockFD(int) (*os.File, error) { return nil, ErrUnsupportedPlatform }
