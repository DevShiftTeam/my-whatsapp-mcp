//go:build darwin || linux

package media

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

// directory anchors operations to an existing prepared private directory.
// Child names are internal basenames, never paths supplied by a client.
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
	if runtime.GOOS == "darwin" {
		device = uint64(uint32(stat.Dev))
	}
	if device != expected {
		return ErrFilesystemMismatch
	}
	return nil
}

func openDirectory(path string) (*directory, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return nil, ErrUnsafeFile
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Standard macOS /var and /tmp aliases refer to OS-owned /private paths.
	// All application-controlled ancestors are still opened with O_NOFOLLOW.
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp"} {
			if absolute == alias || strings.HasPrefix(absolute, alias+"/") {
				absolute = "/private" + absolute
				break
			}
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileError(err, "directory anchor")
	}
	file := os.NewFile(uintptr(fd), "/")
	parts := strings.Split(strings.TrimPrefix(absolute, "/"), "/")
	for _, name := range parts {
		if name == "" {
			continue
		}
		if err := checkAncestor(file); err != nil {
			_ = file.Close()
			return nil, err
		}
		nextFD, err := unix.Openat(int(file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		_ = file.Close()
		if err != nil {
			return nil, fileError(err, "directory ancestor")
		}
		file = os.NewFile(uintptr(nextFD), name)
	}
	d := &directory{file: file, path: absolute}
	if err := checkPrivateFD(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return d, nil
}

func checkAncestor(file *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return err
	}
	owned := uint32(st.Uid) == uint32(os.Geteuid()) || st.Uid == 0
	immutable := st.Mode&0022 == 0 || (st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || !owned || !immutable || !safeAncestorACL(file) {
		return ErrUnsafeFile
	}
	return nil
}

func checkPrivateFD(file *os.File, isDirectory bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return err
	}
	wantType, wantMode := uint32(unix.S_IFREG), uint32(0600)
	if isDirectory {
		wantType, wantMode = unix.S_IFDIR, 0700
	}
	if uint32(st.Uid) != uint32(os.Geteuid()) || uint32(st.Mode)&unix.S_IFMT != wantType || uint32(st.Mode)&07777 != wantMode || (!isDirectory && st.Nlink != 1) || !noExtendedACL(file) {
		return ErrUnsafeFile
	}
	return nil
}

func safeFilename(name string) bool {
	// The prepared appdata owner marker is the only reserved dot basename.
	if name == ".wa-indexer-owner" {
		return true
	}
	if len(name) == 0 || len(name) > 255 || !asciiAlphanumeric(name[0]) {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if !asciiAlphanumeric(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func asciiAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func fileError(err error, operation string) error {
	if errors.Is(err, unix.ENOENT) {
		return os.ErrNotExist
	}
	if errors.Is(err, unix.EEXIST) {
		return os.ErrExist
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ENXIO) {
		return fmt.Errorf("%w: %s", ErrUnsafeFile, operation)
	}
	return err
}

// subdirectory opens an existing private child through the verified parent
// descriptor, even if the parent's original pathname has been replaced.
func (d *directory) subdirectory(name string) (*directory, error) {
	if !safeFilename(name) {
		return nil, ErrUnsafeFile
	}
	if err := checkPrivateFD(d.file, true); err != nil {
		return nil, err
	}
	if err := checkFilesystem(d.file, d.expectedDevice); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileError(err, "subdirectory")
	}
	file := os.NewFile(uintptr(fd), name)
	if err := checkPrivateFD(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := checkFilesystem(file, d.expectedDevice); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &directory{file: file, path: filepath.Join(d.path, name), expectedDevice: d.expectedDevice}, nil
}

func (d *directory) open(name string, flags int) (*os.File, error) {
	if !safeFilename(name) || flags&unix.O_TRUNC != 0 && flags&unix.O_CREAT == 0 {
		return nil, ErrUnsafeFile
	}
	if err := checkPrivateFD(d.file, true); err != nil {
		return nil, err
	}
	if err := checkFilesystem(d.file, d.expectedDevice); err != nil {
		return nil, err
	}
	// Never truncate an existing inode before owner/link/mode/ACL validation.
	// Creation is exclusive even when a caller omits O_EXCL.
	if flags&unix.O_CREAT != 0 {
		flags |= unix.O_EXCL
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fileError(err, "file")
	}
	file := os.NewFile(uintptr(fd), name)
	if err := checkPrivateFD(file, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := checkFilesystem(file, d.expectedDevice); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// same confirms a validated descriptor still identifies the named child.
func (d *directory) same(name string, file *os.File) error {
	if !safeFilename(name) || file == nil {
		return ErrUnsafeFile
	}
	if err := checkPrivateFD(d.file, true); err != nil {
		return err
	}
	var opened, named unix.Stat_t
	if err := unix.Fstatat(int(d.file.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileError(err, "file identity")
	}
	if err := checkPrivateFD(file, false); err != nil {
		return err
	}
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return err
	}
	if opened.Dev != named.Dev || opened.Ino != named.Ino || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Nlink != 1 || uint32(named.Uid) != uint32(os.Geteuid()) || named.Mode&07777 != 0600 {
		return ErrUnsafeFile
	}
	return nil
}

func (d *directory) rename(old, new string) error {
	if !safeFilename(old) || !safeFilename(new) {
		return ErrUnsafeFile
	}
	file, err := d.open(old, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := d.same(old, file); err != nil {
		return err
	}
	return fileError(renameNoReplace(int(d.file.Fd()), old, new), "rename")
}

func (d *directory) remove(name string) error {
	file, err := d.open(name, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := d.same(name, file); err != nil {
		return err
	}
	err = unix.Unlinkat(int(d.file.Fd()), name, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return fileError(err, "remove")
}

func (d *directory) names() ([]string, error) {
	var names []string
	err := d.walkNames(func(name string) error {
		if len(names) == 4096 {
			return ErrUnsafeFile
		}
		names = append(names, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// walkNames uses an independent directory offset and bounded batches. The
// callback can stop enumeration, including cancellation, without a full census.
func (d *directory) walkNames(visit func(string) error) error {
	if visit == nil {
		return ErrUnsafeFile
	}
	if err := checkPrivateFD(d.file, true); err != nil {
		return err
	}
	fd, err := unix.Openat(int(d.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fileError(err, "inventory")
	}
	file := os.NewFile(uintptr(fd), d.path)
	defer file.Close()
	for {
		names, err := file.Readdirnames(256)
		if err != nil && err != io.EOF {
			return err
		}
		for _, name := range names {
			if !safeFilename(name) {
				return ErrUnsafeFile
			}
			if err := visit(name); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func (d *directory) sync() error {
	if err := checkPrivateFD(d.file, true); err != nil {
		return err
	}
	return d.file.Sync()
}

func (d *directory) close() error { return d.file.Close() }
