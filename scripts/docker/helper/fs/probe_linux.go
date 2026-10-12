//go:build linux

package main

import (
	"encoding/json"
	"golang.org/x/sys/unix"
	"os"
	"runtime"
)

func probe() error {
	runtime.LockOSThread()
	err := unix.Unshare(unix.CLONE_FS)
	errno := 0
	if err != nil {
		errno = int(err.(unix.Errno))
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"unshareCloneFS_errno": errno})
}
