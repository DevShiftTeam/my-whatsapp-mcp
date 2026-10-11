//go:build !darwin && !linux

package app

import "os"

func privateBundleEntry(os.FileInfo, bool, bool) bool { return false }
func duplicateBundleFile(*os.File) (*os.File, error)  { return nil, errBundledAI }
