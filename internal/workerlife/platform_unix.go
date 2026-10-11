//go:build darwin || linux

package workerlife

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func duplicateRegular(file *os.File) (*os.File, error) {
	if file == nil {
		return nil, ErrProtocol
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrProtocol
	}
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	return os.NewFile(uintptr(fd), "workerlife-duplicate"), nil
}

func executableFile(config Config) (*os.File, error) {
	if config.ExecutableFile != nil {
		file, err := duplicateRegular(config.ExecutableFile)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || info.Mode().Perm()&0111 == 0 {
			file.Close()
			return nil, ErrUnavailable
		}
		return file, nil
	}
	if !filepath.IsAbs(config.Executable) {
		return nil, ErrUnavailable
	}
	before, err := os.Lstat(config.Executable)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0111 == 0 {
		return nil, ErrUnavailable
	}
	fd, err := unix.Open(config.Executable, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(fd), "workerlife-executable")
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		file.Close()
		return nil, ErrUnavailable
	}
	return file, nil
}

// Both probes must positively report ESRCH. EPERM or any other uncertainty
// retains ownership. Trusted runtimes must not escape or daemonize their group.
func probeNative(pid int) (bool, error) {
	if pid <= 0 {
		return false, ErrProtocol
	}
	group := syscall.Kill(-pid, 0)
	leader := syscall.Kill(pid, 0)
	if errors.Is(group, syscall.ESRCH) && errors.Is(leader, syscall.ESRCH) {
		return true, nil
	}
	if group != nil && !errors.Is(group, syscall.ESRCH) {
		return false, ErrCleanup
	}
	if leader != nil && !errors.Is(leader, syscall.ESRCH) {
		return false, ErrCleanup
	}
	return false, nil
}
