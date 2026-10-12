//go:build linux

package remoteaccess

import "os"

// Linux POSIX access ACL writers are bounded by the group-class write mask,
// which cloudflarePath rejects. Unlike Darwin they cannot override that mask.
func cloudflareSafeACL(*os.File) bool { return true }
