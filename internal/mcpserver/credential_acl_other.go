//go:build !darwin && !linux

package mcpserver

import "os"

func credentialNoExtendedACL(*os.File) bool { return false }
