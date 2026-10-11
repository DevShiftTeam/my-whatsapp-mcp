//go:build !darwin && !linux

package diskspace

import "os"

func openVerifiedRoot(string) (*os.File, error) { return nil, ErrUnsupportedPlatform }
func verifyRoot(*os.File) (uint64, error)       { return 0, ErrUnsupportedPlatform }
func queryFilesystem(*os.File) (filesystemSpace, error) {
	return filesystemSpace{}, ErrUnsupportedPlatform
}
