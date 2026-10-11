//go:build !darwin

package modelstore

import (
	"os"
	"runtime"
)

// Linux POSIX ACL grants obey the group-class mask represented in 0700/0600
// mode bits. Fail closed on other platforms whose ACL behavior is unsupported.
func noExtendedACL(*os.File) bool { return runtime.GOOS == "linux" }

func safeAncestorACL(*os.File) bool { return runtime.GOOS == "linux" }
