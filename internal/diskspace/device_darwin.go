//go:build darwin

package diskspace

import "golang.org/x/sys/unix"

// dev_t is signed on Darwin; preserve its 32-bit identity without sign extension.
func deviceID(stat unix.Stat_t) uint64 { return uint64(uint32(stat.Dev)) }

func filesystemSample(stat unix.Statfs_t) (filesystemSpace, error) {
	return spaceFromUnits(stat.Bavail, int64(stat.Bsize), 0)
}
