//go:build !darwin

package ingestion

import (
	"os"
	"runtime"
)

// Linux POSIX ACL grants obey the group-class mask represented in 0700/0600
// mode bits. Fail closed on other platforms whose ACL behavior is unsupported.
func sessionNoExtendedACL(*os.File) bool { return runtime.GOOS == "linux" }

func sessionNoExtendedACLAt(*os.File, string, os.FileInfo) bool { return runtime.GOOS == "linux" }

func sessionSafeAncestorACL(*os.File) bool { return runtime.GOOS == "linux" }
