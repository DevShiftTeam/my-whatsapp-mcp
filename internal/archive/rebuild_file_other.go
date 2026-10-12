//go:build !darwin && !linux

package archive

func rebuildOwned(string, bool) error { return ErrUnsafeDataPath }
