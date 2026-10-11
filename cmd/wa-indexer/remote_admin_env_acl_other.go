//go:build !darwin && !linux

package main

import "os"

func adminEnvNoACL(*os.File) bool { return false }

func adminEnvOpenFlags() int { return os.O_RDONLY }
