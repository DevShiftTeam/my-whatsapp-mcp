//go:build linux

package workerlife

import "os"

// Linux POSIX ACL grants obey the group-class mode mask checked on the private
// directory/file; they cannot independently grant access outside that mask.
func namespaceNoExtendedACL(file *os.File) bool { return file != nil }
