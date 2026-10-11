//go:build darwin || linux

package modelstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// directory anchors every operation to an already opened private directory.
// All children are single validated names; O_NOFOLLOW rejects link races.
type directory struct {
	file           *os.File
	path           string
	expectedDevice uint64
}

func checkFilesystem(file *os.File, expected uint64) error {
	if expected == 0 {
		return nil
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return ErrFilesystemMismatch
	}
	device := uint64(stat.Dev)
	// Darwin dev_t is signed 32-bit; match the broker's unsigned identity.
	if runtime.GOOS == "darwin" {
		device = uint64(uint32(stat.Dev))
	}
	if device != expected {
		return ErrFilesystemMismatch
	}
	return nil
}

func openDirectory(path string, create bool, private bool) (*directory, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// macOS's standard /var and /tmp aliases are fixed OS-owned anchors. Every
	// application-controlled ancestor below them is walked with O_NOFOLLOW.
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp"} {
			if absolute == alias || strings.HasPrefix(absolute, alias+"/") {
				absolute = "/private" + absolute
				break
			}
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(absolute, "/"), "/")
	for i, name := range parts {
		if name == "" {
			continue
		}
		if create && i == len(parts)-1 {
			if err := unix.Mkdirat(fd, name, 0700); err != nil && err != unix.EEXIST {
				_ = unix.Close(fd)
				return nil, err
			}
		}
		next, e := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, fmt.Errorf("%w: directory ancestor", ErrUnsafeFile)
		}
		fd = next
		if i < len(parts)-1 {
			f := os.NewFile(uintptr(fd), name)
			var stat unix.Stat_t
			statErr := unix.Fstat(fd, &stat)
			safe := statErr == nil && (uint32(stat.Uid) == uint32(os.Geteuid()) || stat.Uid == 0) && (stat.Mode&0022 == 0 || (stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0)) && safeAncestorACL(f)
			if !safe {
				_ = f.Close()
				return nil, ErrUnsafeFile
			}
			// Retain the descriptor; f is a temporary wrapper only. Clearing its
			// finalizer requires KeepAlive through the next Openat and ownership
			// transfer, so duplicate atomically rather than retain two wrappers.
			duplicate, e := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
			_ = f.Close()
			if e != nil {
				return nil, e
			}
			fd = duplicate
		}
	}
	d := &directory{file: os.NewFile(uintptr(fd), absolute), path: absolute}
	if err := checkFD(d.file, true, private, false); err != nil {
		_ = d.file.Close()
		return nil, err
	}
	return d, nil
}

func checkFD(f *os.File, dir, private, readonly bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	typeBits := st.Mode & unix.S_IFMT
	if uint32(st.Uid) != uint32(os.Geteuid()) || (dir && typeBits != unix.S_IFDIR) || (!dir && (typeBits != unix.S_IFREG || st.Nlink != 1)) {
		return ErrUnsafeFile
	}
	if private && st.Mode&0077 != 0 {
		return ErrUnsafeFile
	}
	if private && !noExtendedACL(f) {
		return ErrUnsafeFile
	}
	if readonly && st.Mode&0222 != 0 {
		return ErrUnsafeFile
	}
	return nil
}

func (d *directory) child(name string, create bool) (*directory, error) {
	if !safeName(name) {
		return nil, ErrUnsafeFile
	}
	if err := checkFilesystem(d.file, d.expectedDevice); err != nil {
		return nil, err
	}
	if create {
		if err := unix.Mkdirat(int(d.file.Fd()), name, 0700); err != nil && err != unix.EEXIST {
			return nil, err
		}
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("%w: directory", ErrUnsafeFile)
	}
	c := &directory{file: os.NewFile(uintptr(fd), name), path: filepath.Join(d.path, name), expectedDevice: d.expectedDevice}
	if err := checkFD(c.file, true, true, false); err != nil {
		_ = c.close()
		return nil, err
	}
	if err := checkFilesystem(c.file, c.expectedDevice); err != nil {
		_ = c.close()
		return nil, err
	}
	return c, nil
}

func (d *directory) open(name string, flags int, mode uint32, private bool) (*os.File, error) {
	if !safeName(name) {
		return nil, ErrUnsafeFile
	}
	if err := checkFilesystem(d.file, d.expectedDevice); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, mode)
	if err != nil {
		if err == unix.ENOENT {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("%w: file", ErrUnsafeFile)
	}
	f := os.NewFile(uintptr(fd), name)
	if err := checkFD(f, false, private, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := checkFilesystem(f, d.expectedDevice); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (d *directory) read(name string, cap int64) ([]byte, error) {
	f, err := d.open(name, unix.O_RDONLY, 0, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, cap+1))
	if int64(len(b)) > cap {
		return nil, ErrUnsafeFile
	}
	return b, err
}

// same verifies a held private lock still identifies its child in the anchored
// namespace. It never reopens the lock file or changes its flock ownership.
func (d *directory) same(name string, f *os.File) error {
	if !safeName(name) || f == nil {
		return ErrUnsafeFile
	}
	if err := checkFD(d.file, true, true, false); err != nil {
		return err
	}
	if err := checkFD(f, false, true, false); err != nil {
		return err
	}
	var opened, named unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &opened); err != nil {
		return err
	}
	if err := unix.Fstatat(int(d.file.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("%w: lock identity", ErrUnsafeFile)
	}
	if opened.Dev != named.Dev || opened.Ino != named.Ino || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&07777 != 0600 || named.Nlink != 1 || uint32(named.Uid) != uint32(os.Geteuid()) {
		return ErrUnsafeFile
	}
	return nil
}

func (d *directory) atomicWrite(name string, data []byte) error {
	// The temporary name is reserved by the operation lock, never reused blindly.
	tmp := name + ".tmp"
	if err := d.unlink(tmp); err != nil {
		return err
	}
	f, err := d.open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600, true)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = d.safeTarget(name); err != nil {
		return err
	}
	if err = unix.Renameat(int(d.file.Fd()), tmp, int(d.file.Fd()), name); err != nil {
		return err
	}
	return d.file.Sync()
}

func (d *directory) safeTarget(name string) error {
	f, err := d.open(name, unix.O_RDONLY, 0, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func (d *directory) unlink(name string) error {
	if !safeName(name) {
		return ErrUnsafeFile
	}
	if err := d.safeTarget(name); err != nil {
		return err
	}
	err := unix.Unlinkat(int(d.file.Fd()), name, 0)
	if err == unix.ENOENT {
		return nil
	}
	return err
}

func (d *directory) renameTo(name string, to *directory, target string) error {
	if err := d.safeTarget(name); err != nil {
		return err
	}
	if err := to.safeTarget(target); err != nil {
		return err
	}
	if err := unix.Renameat(int(d.file.Fd()), name, int(to.file.Fd()), target); err != nil {
		return err
	}
	if err := to.file.Sync(); err != nil {
		return err
	}
	return d.file.Sync()
}

func (d *directory) close() error { return d.file.Close() }
func (d *directory) freeBytes() (int64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(d.file.Fd()), &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (d *directory) names() ([]string, error) {
	fd, err := unix.Openat(int(d.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), d.path)
	defer f.Close()
	names, err := f.Readdirnames(4097)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > 4096 {
		return nil, ErrUnsafeFile
	}
	return names, nil
}

func fileAllocatedBytes(f *os.File) (int64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	if st.Blocks < 0 || st.Blocks > 1<<50 {
		return 0, ErrUnsafeFile
	}
	return int64(st.Blocks) * 512, nil
}

func allocatedBytes(parent *directory) (int64, int64, error) {
	names, err := parent.names()
	if err != nil {
		return 0, 0, err
	}
	var total, physical int64
	for _, name := range names {
		if !shaPattern.MatchString(name) {
			return 0, 0, ErrUnsafeFile
		}
		d, err := parent.child(name, false)
		if err != nil {
			return 0, 0, err
		}
		files, err := d.names()
		if err != nil {
			_ = d.close()
			return 0, 0, err
		}
		for _, name := range files {
			f, e := d.open(name, unix.O_RDONLY, 0, true)
			if e != nil {
				_ = d.close()
				return 0, 0, e
			}
			st, e := f.Stat()
			blocks, allocationErr := fileAllocatedBytes(f)
			_ = f.Close()
			if e != nil {
				_ = d.close()
				return 0, 0, e
			}
			if allocationErr != nil {
				_ = d.close()
				return 0, 0, allocationErr
			}
			if st.Size() < 0 || st.Size() > 16<<30 {
				_ = d.close()
				return 0, 0, ErrUnsafeFile
			}
			total += st.Size()
			physical += blocks
		}
		_ = d.close()
	}
	return total, physical, nil
}

func removeDirectory(parent *directory, name string) error {
	d, err := parent.child(name, false)
	if err != nil {
		return err
	}
	defer d.close()
	names, err := d.names()
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := d.unlink(n); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(int(parent.file.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.file.Sync()
}
