//go:build darwin

package app

import "syscall"

func namespaceStatDevice(stat *syscall.Stat_t) uint64 { return uint64(uint32(stat.Dev)) }
