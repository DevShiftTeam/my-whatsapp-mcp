//go:build !darwin && !linux

package appdata

import "os"

func platformSupported() bool                           { return false }
func noFollowFlag() int                                 { return 0 }
func ownedByCurrentUser(info os.FileInfo) bool          { return false }
func singleLink(info os.FileInfo) bool                  { return false }
func lockFile(file *os.File) error                      { return ErrUnsupportedPlatform }
func duplicateLockFile(file *os.File) (*os.File, error) { return nil, ErrUnsupportedPlatform }
