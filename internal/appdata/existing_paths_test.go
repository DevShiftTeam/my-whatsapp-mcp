//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingPathsSecondLaunchDoesNotMutate(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := WriteOwnerURL(paths, lease, syntheticAttachURL()); err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("second launch contention=%v", err)
	}
	// A read-only launch must not restore, traverse, or claim data directories.
	if err := os.Remove(paths.ArchiveDir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	markerBefore, err := os.Lstat(filepath.Join(paths.Root, ownerMarker))
	if err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.Lstat(filepath.Join(paths.Root, instanceLockFile))
	if err != nil {
		t.Fatal(err)
	}
	ownerBefore, err := os.Lstat(paths.OwnerURLFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExistingPaths(paths.Root)
	if err != nil || got != paths {
		t.Fatalf("existing paths fixed error=%v", err)
	}
	if candidate, err := ReadOwnerURL(got); err != nil || candidate != syntheticAttachURL() {
		t.Fatalf("read fixed error=%v", err)
	}
	after, err := os.ReadDir(paths.Root)
	if err != nil || len(before) != len(after) {
		t.Fatal("existing paths changed root entries")
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatal("existing paths changed root entries")
		}
	}
	for name, original := range map[string]os.FileInfo{ownerMarker: markerBefore, instanceLockFile: lockBefore, ownerURLFile: ownerBefore} {
		current, err := os.Lstat(filepath.Join(paths.Root, name))
		if err != nil || !os.SameFile(original, current) || !original.ModTime().Equal(current.ModTime()) {
			t.Fatal("read-only launch rewrote private control data")
		}
	}
}

func TestExistingPathsDoesNotCreateOrClaimRoot(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "malformed-marker", "root-symlink", "marker-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(syntheticParent(t), "read-only-root")
			if kind != "missing" {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "malformed-marker":
				if err := os.WriteFile(filepath.Join(root, ownerMarker), []byte("unowned\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "root-symlink":
				if err := os.WriteFile(filepath.Join(root, ownerMarker), []byte(ownerMarkerContent), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(root, root+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"-moved", root); err != nil {
					t.Fatal(err)
				}
			case "marker-symlink":
				outside := filepath.Join(syntheticParent(t), "outside-marker")
				if err := os.WriteFile(outside, []byte(ownerMarkerContent), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, ownerMarker)); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ExistingPaths(root)
			if got != (Paths{}) || !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("existing fixed error=%v", err)
			}
			if kind == "missing" {
				if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing root was created")
				}
			} else {
				for _, name := range dataDirectories {
					if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("read-only constructor created data directory")
					}
				}
				if _, err := os.Lstat(filepath.Join(root, instanceLockFile)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("read-only constructor created lock")
				}
			}
		})
	}
}

func TestExistingPathsDefaultsToSamePreparedRoot(t *testing.T) {
	home := syntheticParent(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "synthetic-config"))
	prepared, err := Prepare("")
	if err != nil {
		t.Fatal(err)
	}
	existing, err := ExistingPaths("")
	if err != nil || existing != prepared {
		t.Fatalf("default existing paths fixed error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(existing.Root, instanceLockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("default read-only constructor created lock")
	}
}
