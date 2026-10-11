//go:build !darwin && !linux

package appdata

import "os"

func noExtendedACL(file *os.File) bool { return false }

func noExtendedACLAt(*os.File, string, os.FileInfo) bool { return false }
