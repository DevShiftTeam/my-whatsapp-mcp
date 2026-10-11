//go:build !darwin && !linux

package archive

import "os"

func openSnapshotMarker(root *os.Root, name string) (*os.File, error) {
	return nil, ErrUnsafeDataPath
}

func snapshotMarkerFileIdentity(info os.FileInfo) (string, error) {
	return "", ErrUnsafeDataPath
}
