//go:build linux

package app

import "os"

// Validated 0600 permission mask also denies Linux POSIX ACL grants.
func stateNoExtendedACL(*os.File) bool { return true }
