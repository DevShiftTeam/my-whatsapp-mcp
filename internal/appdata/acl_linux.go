//go:build linux

package appdata

import "os"

// Linux POSIX ACL grants obey the group-class mask encoded in the validated
// 0700/0600 modes; unlike Darwin, they cannot independently grant access here.
func noExtendedACL(file *os.File) bool { return true }

func noExtendedACLAt(*os.File, string, os.FileInfo) bool { return true }
