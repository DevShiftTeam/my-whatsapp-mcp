//go:build darwin || linux

package media

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"golang.org/x/sys/unix"
)

// These tests catch writes through path replacements, accepting shared or
// special files, overwriting publication destinations, and unbounded inventory.
func TestDirectoryMissingDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "media")
	d, err := openDirectory(path)
	if d != nil {
		_ = d.close()
		t.Fatal("opened a missing directory")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing ancestor was created: %v", err)
	}
}

func TestBoundDirectoryDeviceCheckedBeforeFileCreation(t *testing.T) {
	directory, path := privateTestDirectory(t)
	// Canonicalize only this trusted fixture, preserving the broker's path gate.
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := diskspace.New(path, diskspace.Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	snapshot, err := broker.Snapshot()
	if err != nil || snapshot.Device == 0 {
		t.Fatal("filesystem device unavailable", err)
	}
	directory.expectedDevice = snapshot.Device + 1
	file, err := directory.open("new-file", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, ErrFilesystemMismatch) {
		t.Fatal("bound destination device mismatch accepted", err)
	}
	names, err := os.ReadDir(path)
	if err != nil || len(names) != 0 {
		t.Fatal("bound media directory wrote before checking its device", err)
	}
}

func privateTestDirectory(t *testing.T) (*directory, string) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	d, err := openDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.close() })
	return d, path
}

func writePrivateTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryCreatesPrivateFilesExclusively(t *testing.T) {
	d, path := privateTestDirectory(t)
	f, err := d.open("record.part", unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("preserved"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(path, "record.part"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("private file mode: %v %v", st, err)
	}
	if f, err := d.open("record.part", unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC); err == nil {
		_ = f.Close()
		t.Fatal("create reopened an existing file")
	}
	if contents, err := os.ReadFile(filepath.Join(path, "record.part")); err != nil || string(contents) != "preserved" {
		t.Fatalf("existing content changed: %q %v", contents, err)
	}
	if err := d.sync(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryRejectsTruncateBeforeOpeningAnUnsafeInode(t *testing.T) {
	d, path := privateTestDirectory(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writePrivateTestFile(t, outside, "preserved outside contents")
	if err := os.Link(outside, filepath.Join(path, "record.media")); err != nil {
		t.Fatal(err)
	}
	if file, err := d.open("record.media", unix.O_WRONLY|unix.O_TRUNC); !errors.Is(err, ErrUnsafeFile) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("truncation allowed: %v", err)
	}
	if contents, err := os.ReadFile(outside); err != nil || string(contents) != "preserved outside contents" {
		t.Fatalf("outside inode changed: %q %v", contents, err)
	}
}

func TestDirectoryRechecksPrivateModeBeforeCreating(t *testing.T) {
	d, path := privateTestDirectory(t)
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0700)
	if file, err := d.open("record.part", unix.O_CREAT|unix.O_WRONLY); !errors.Is(err, ErrUnsafeFile) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("changed directory mode accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(path, "record.part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file created under unsafe directory: %v", err)
	}
}

func TestDirectoryRejectsUnsafeFileTypes(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "public", "readonly", "special-mode"} {
		t.Run(kind, func(t *testing.T) {
			d, path := privateTestDirectory(t)
			child := filepath.Join(path, "record.media")
			switch kind {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				writePrivateTestFile(t, outside, "outside")
				if err := os.Symlink(outside, child); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				outside := filepath.Join(t.TempDir(), "outside")
				writePrivateTestFile(t, outside, "outside")
				if err := os.Link(outside, child); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(child, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(child, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				writePrivateTestFile(t, child, "contents")
				mode := os.FileMode(0640)
				if kind == "readonly" {
					mode = 0400
				}
				if kind == "special-mode" {
					mode = 0600 | os.ModeSetuid
				}
				if err := os.Chmod(child, mode); err != nil {
					t.Fatal(err)
				}
			}
			if f, err := d.open("record.media", unix.O_RDONLY); !errors.Is(err, ErrUnsafeFile) {
				if f != nil {
					_ = f.Close()
				}
				t.Fatalf("unsafe file accepted: %v", err)
			}
			if err := d.remove("record.media"); !errors.Is(err, ErrUnsafeFile) {
				t.Fatalf("unsafe removal accepted: %v", err)
			}
			if _, err := os.Lstat(child); err != nil {
				t.Fatalf("unsafe entry removed: %v", err)
			}
		})
	}
}

func TestDirectoryRejectsUnsafeDirectories(t *testing.T) {
	for _, kind := range []string{"public", "readonly", "symlink", "mutable-ancestor", "symlink-ancestor"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			path := filepath.Join(parent, "media")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "public":
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			case "readonly":
				if err := os.Chmod(path, 0500); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := filepath.Join(parent, "linked")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "mutable-ancestor":
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(parent, 0700)
			case "symlink-ancestor":
				link := filepath.Join(t.TempDir(), "linked")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "media")
			}
			if d, err := openDirectory(path); !errors.Is(err, ErrUnsafeFile) {
				if d != nil {
					_ = d.close()
				}
				t.Fatalf("unsafe directory accepted: %v", err)
			}
		})
	}
}

func TestDirectoryOperationsStayAnchoredAfterPathReplacement(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "media")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	d, err := openDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	writePrivateTestFile(t, filepath.Join(path, "record.part"), "original")
	original := filepath.Join(parent, "original")
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	writePrivateTestFile(t, filepath.Join(path, "record.part"), "replacement")
	if err := d.rename("record.part", "record.media"); err != nil {
		t.Fatal(err)
	}
	f, err := d.open("record.media", unix.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(contents) != "original" {
		t.Fatalf("read wrong inode: %q %v", contents, err)
	}
	if err := d.remove("record.media"); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(path, "record.part")); err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement directory was mutated: %q %v", contents, err)
	}
}

func TestDirectorySubdirectoryStaysAnchoredAfterRootReplacement(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			parent := t.TempDir()
			rootPath := filepath.Join(parent, "root")
			if err := os.Mkdir(rootPath, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(rootPath, "media"), 0700); err != nil {
				t.Fatal(err)
			}
			writePrivateTestFile(t, filepath.Join(rootPath, "media", "record.media"), "original")
			root, err := openDirectory(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer root.close()
			originalPath := filepath.Join(parent, "original")
			if err := os.Rename(rootPath, originalPath); err != nil {
				t.Fatal(err)
			}
			replacementPath := rootPath
			if replacement == "symlink" {
				replacementPath = filepath.Join(parent, "replacement")
			}
			if err := os.Mkdir(replacementPath, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(replacementPath, "media"), 0700); err != nil {
				t.Fatal(err)
			}
			writePrivateTestFile(t, filepath.Join(replacementPath, "media", "record.media"), "replacement")
			if replacement == "symlink" {
				if err := os.Symlink(replacementPath, rootPath); err != nil {
					t.Fatal(err)
				}
			}
			child, err := root.subdirectory("media")
			if err != nil {
				t.Fatal(err)
			}
			defer child.close()
			file, err := child.open("record.media", unix.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil || string(contents) != "original" {
				t.Fatalf("subdirectory read replacement: %q %v", contents, err)
			}
			if err := child.remove("record.media"); err != nil {
				t.Fatal(err)
			}
			if contents, err := os.ReadFile(filepath.Join(replacementPath, "media", "record.media")); err != nil || string(contents) != "replacement" {
				t.Fatalf("replacement child changed: %q %v", contents, err)
			}
			if _, err := os.Lstat(filepath.Join(originalPath, "media", "record.media")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("original child was not used: %v", err)
			}
		})
	}
}

func TestDirectorySubdirectoryRejectsUnsafeChildren(t *testing.T) {
	d, path := privateTestDirectory(t)
	if err := os.Mkdir(filepath.Join(path, "public"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(path, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"public", "linked", "../outside", "child/name"} {
		if child, err := d.subdirectory(name); !errors.Is(err, ErrUnsafeFile) {
			if child != nil {
				_ = child.close()
			}
			t.Fatalf("unsafe child %q accepted: %v", name, err)
		}
	}
	if child, err := d.subdirectory("missing"); !errors.Is(err, os.ErrNotExist) {
		if child != nil {
			_ = child.close()
		}
		t.Fatalf("missing child: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(path, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing child created: %v", err)
	}
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0700)
	if child, err := d.subdirectory("public"); !errors.Is(err, ErrUnsafeFile) {
		if child != nil {
			_ = child.close()
		}
		t.Fatalf("mutable parent accepted: %v", err)
	}
}

func TestDirectorySameDetectsReplacedName(t *testing.T) {
	d, path := privateTestDirectory(t)
	child := filepath.Join(path, "record.part")
	writePrivateTestFile(t, child, "original")
	f, err := d.open("record.part", unix.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := d.same("record.part", f); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := d.same("record.part", f); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing name: %v", err)
	}
	writePrivateTestFile(t, child, "replacement")
	if err := d.same("record.part", f); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("replacement accepted: %v", err)
	}
}

func TestDirectoryRenameDoesNotOverwrite(t *testing.T) {
	d, path := privateTestDirectory(t)
	writePrivateTestFile(t, filepath.Join(path, "record.part"), "source")
	writePrivateTestFile(t, filepath.Join(path, "record.media"), "destination")
	if err := d.rename("record.part", "record.media"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite: %v", err)
	}
	for name, expected := range map[string]string{"record.part": "source", "record.media": "destination"} {
		contents, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(contents) != expected {
			t.Fatalf("%s changed: %q %v", name, contents, err)
		}
	}
	if err := d.remove("missing.media"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
}

func TestDirectoryInventoryIsBoundedAndRepeatable(t *testing.T) {
	d, path := privateTestDirectory(t)
	writePrivateTestFile(t, filepath.Join(path, "record.part"), "contents")
	for i := 0; i < 2; i++ {
		names, err := d.names()
		if err != nil || len(names) != 1 || names[0] != "record.part" {
			t.Fatalf("inventory: %v %v", names, err)
		}
	}
	for i := 0; i < 4096; i++ {
		writePrivateTestFile(t, filepath.Join(path, fmt.Sprintf("entry-%04d", i)), "")
	}
	if names, err := d.names(); !errors.Is(err, ErrUnsafeFile) || names != nil {
		t.Fatalf("unbounded inventory: %d %v", len(names), err)
	}
	count := 0
	if err := d.walkNames(func(string) error { count++; return nil }); err != nil || count != 4097 {
		t.Fatalf("streaming inventory: %d %v", count, err)
	}
	stop := errors.New("stop enumeration")
	count = 0
	if err := d.walkNames(func(string) error { count++; return stop }); !errors.Is(err, stop) || count != 1 {
		t.Fatalf("callback cancellation: %d %v", count, err)
	}
}

func TestDirectoryRejectsTraversalNames(t *testing.T) {
	d, _ := privateTestDirectory(t)
	for _, name := range []string{"", ".", "..", "../outside", "/absolute", "child/name", "child\\name", "bad\x00name", "bad name"} {
		if f, err := d.open(name, unix.O_RDONLY); !errors.Is(err, ErrUnsafeFile) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("unsafe name %q accepted: %v", name, err)
		}
		if err := d.remove(name); !errors.Is(err, ErrUnsafeFile) {
			t.Fatalf("unsafe remove %q: %v", name, err)
		}
		if err := d.rename("record.part", name); !errors.Is(err, ErrUnsafeFile) {
			t.Fatalf("unsafe rename %q: %v", name, err)
		}
	}
}

func TestDirectorySupportsOnlyFixedOwnerMarker(t *testing.T) {
	d, path := privateTestDirectory(t)
	writePrivateTestFile(t, filepath.Join(path, ".wa-indexer-owner"), "owner marker")
	file, err := d.open(".wa-indexer-owner", unix.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(contents) != "owner marker" {
		t.Fatalf("owner marker read: %q %v", contents, err)
	}
	names, err := d.names()
	if err != nil || len(names) != 1 || names[0] != ".wa-indexer-owner" {
		t.Fatalf("owner marker inventory: %v %v", names, err)
	}
	for _, name := range []string{".other", ".wa-indexer-owner.tmp", "..wa-indexer-owner", "./.wa-indexer-owner", ".wa-indexer-owner/child", ".wa-indexer-owner\x00"} {
		if file, err := d.open(name, unix.O_RDONLY); !errors.Is(err, ErrUnsafeFile) {
			if file != nil {
				_ = file.Close()
			}
			t.Fatalf("non-marker dot name %q accepted: %v", name, err)
		}
	}
}

func TestDirectoryDarwinACLPolicy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin extended ACL")
	}
	for _, test := range []struct {
		name, target, acl string
		allowed           bool
	}{
		{"file-grant", "record.media", "everyone allow read", false},
		{"directory-grant", "media", "everyone allow read,write,execute", false},
		{"ancestor-grant", "ancestor", "everyone allow write", false},
		{"ancestor-read-grant", "ancestor", "everyone allow read", false},
		{"ancestor-deny", "ancestor", "everyone deny delete", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			path := filepath.Join(parent, "media")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			filePath := filepath.Join(path, "record.media")
			writePrivateTestFile(t, filePath, "contents")
			target := parent
			if test.target == "media" {
				target = path
			}
			if test.target == "record.media" {
				target = filePath
			}
			if err := exec.Command("/bin/chmod", "+a", test.acl, target).Run(); err != nil {
				t.Fatal(err)
			}
			defer exec.Command("/bin/chmod", "-N", target).Run()
			d, err := openDirectory(path)
			if test.target == "record.media" && err == nil {
				defer d.close()
				f, e := d.open("record.media", unix.O_RDONLY)
				if f != nil {
					_ = f.Close()
				}
				err = e
			} else if d != nil {
				_ = d.close()
			}
			if test.allowed && err != nil {
				t.Fatalf("deny-only ACL rejected: %v", err)
			}
			if !test.allowed && !errors.Is(err, ErrUnsafeFile) {
				t.Fatalf("extended ACL accepted: %v", err)
			}
		})
	}
}
