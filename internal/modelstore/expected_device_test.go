package modelstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"golang.org/x/sys/unix"
)

func privateDeviceTestRoot(t *testing.T) string {
	t.Helper()
	// Resolve only the trusted fixture: the broker rejects symlink ancestors.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestExpectedDeviceMatchesSharedController(t *testing.T) {
	root := privateDeviceTestRoot(t)
	broker, err := diskspace.New(root, diskspace.Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	snapshot, err := broker.Snapshot()
	if err != nil || snapshot.Device == 0 {
		t.Fatal("shared controller device unavailable", err)
	}
	s, err := Open(root, catalogFor(component("model", []byte("abcdef"), "")), Options{ExpectedDevice: snapshot.Device})
	if err != nil {
		t.Fatal("matching retained filesystem rejected", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundDirectoryDeviceCheckedBeforeChildOrFileCreation(t *testing.T) {
	for _, kind := range []string{"child", "file"} {
		t.Run(kind, func(t *testing.T) {
			path := privateDeviceTestRoot(t)
			broker, err := diskspace.New(path, diskspace.Options{HeadroomBytes: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			snapshot, err := broker.Snapshot()
			if err != nil || snapshot.Device == 0 {
				t.Fatal("filesystem device unavailable", err)
			}
			directory, err := openDirectory(path, false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer directory.close()
			directory.expectedDevice = snapshot.Device + 1
			if kind == "child" {
				child, openErr := directory.child("new-child", true)
				if child != nil {
					_ = child.close()
				}
				err = openErr
			} else {
				file, openErr := directory.open("new-file", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600, true)
				if file != nil {
					_ = file.Close()
				}
				err = openErr
			}
			if !errors.Is(err, ErrFilesystemMismatch) {
				t.Fatal("bound destination device mismatch accepted", err)
			}
			names, err := os.ReadDir(path)
			if err != nil || len(names) != 0 {
				t.Fatal("bound directory wrote before checking its device", err)
			}
		})
	}
}

func TestExpectedDeviceMismatchCreatesNoModelChildren(t *testing.T) {
	root := privateDeviceTestRoot(t)
	broker, err := diskspace.New(root, diskspace.Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	snapshot, err := broker.Snapshot()
	if err != nil || snapshot.Device == 0 {
		t.Fatal("shared controller device unavailable", err)
	}
	if s, err := Open(root, catalogFor(component("model", []byte("abcdef"), "")), Options{ExpectedDevice: snapshot.Device + 1}); !errors.Is(err, ErrFilesystemMismatch) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("foreign device was accepted: %v", err)
	}
	names, err := os.ReadDir(root)
	if err != nil || len(names) != 0 {
		t.Fatalf("mismatch created model files before validation: %+v %v", names, err)
	}
}
