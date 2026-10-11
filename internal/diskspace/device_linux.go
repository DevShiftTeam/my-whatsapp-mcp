//go:build linux

package diskspace

import "golang.org/x/sys/unix"

func deviceID(stat unix.Stat_t) uint64 { return uint64(stat.Dev) }

func filesystemSample(stat unix.Statfs_t) (filesystemSpace, error) {
	return spaceFromUnits(stat.Bavail, int64(stat.Bsize), int64(stat.Frsize))
}
