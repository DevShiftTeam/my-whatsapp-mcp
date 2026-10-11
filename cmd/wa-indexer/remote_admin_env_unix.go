//go:build darwin || linux

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func adminEnvOpenFlags() int { return os.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW }
