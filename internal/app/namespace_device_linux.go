//go:build linux

package app

import "syscall"

func namespaceStatDevice(stat *syscall.Stat_t) uint64 { return uint64(stat.Dev) }
