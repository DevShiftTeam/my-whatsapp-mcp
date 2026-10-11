//go:build !darwin && !linux

package app

import "os"

// appdata rejects these platforms before any state operation.
func stateNoFollow() int { return 0 }

func namespaceDeviceMatches(os.FileInfo, uint64) bool { return false }

func privateStateFile(os.FileInfo) bool { return false }
func stateNoExtendedACL(*os.File) bool  { return false }
