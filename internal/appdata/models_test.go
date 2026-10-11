//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyModelComponentAllowsRestartAndPrivateWorkingAccess(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(paths.ModelDir, "synthetic-profile", "components")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	component := filepath.Join(profile, "encoder.onnx")
	if err := os.WriteFile(component, []byte("synthetic immutable model component"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "metadata.json"), []byte("synthetic model metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Prepare(paths.Root)
	if err != nil {
		t.Fatalf("model restart prepare = %v", err)
	}
	lease, err = Acquire(reopened)
	if err != nil {
		t.Fatalf("model restart acquire = %v", err)
	}
	defer lease.Close()
	if err := WriteOwnerURL(reopened, lease, syntheticAttachURL()); err != nil {
		t.Fatalf("model restart owner handoff = %v", err)
	}
	if err := WithWorking(reopened, lease, func(root *os.Root) error {
		return root.WriteFile("state.json", []byte("synthetic private working state"), 0600)
	}); err != nil {
		t.Fatalf("model restart working access = %v", err)
	}
	if err := RemoveOwnerURL(reopened, lease); err != nil {
		t.Fatal(err)
	}
	assertMode(t, component, 0400)
	assertMode(t, filepath.Join(reopened.WorkingDir, "state.json"), 0600)
}

func TestReadOnlyPermissionRemainsDeniedOutsideModelFiles(t *testing.T) {
	for _, location := range []string{"archive", "session", "media", "working", "clients", "marker", "lock", "owner-url", "model-directory"} {
		t.Run(location, func(t *testing.T) {
			paths := syntheticPaths(t)
			var target string
			switch location {
			case "marker":
				target = filepath.Join(paths.Root, ".wa-indexer-owner")
			case "lock":
				target = filepath.Join(paths.Root, ".instance.lock")
			case "owner-url":
				target = paths.OwnerURLFile
			case "model-directory":
				target = filepath.Join(paths.ModelDir, "read-only-directory")
				if err := os.Mkdir(target, 0400); err != nil {
					t.Fatal(err)
				}
			default:
				target = filepath.Join(paths.Root, location, "read-only-state")
			}
			if location != "model-directory" && location != "marker" {
				if err := os.WriteFile(target, []byte("synthetic protected data"), 0400); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(target, 0400); err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("prepare fixed error = %v", err)
			}
			if _, err := Acquire(paths); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("acquire fixed error = %v", err)
			}
			assertMode(t, target, 0400)
		})
	}
}

func TestReadOnlyModelFilesKeepLinkAndAccessRestrictions(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "group-readable", "world-readable"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			outside := filepath.Join(filepath.Dir(paths.Root), "outside-component")
			if err := os.WriteFile(outside, []byte("synthetic outside component"), 0400); err != nil {
				t.Fatal(err)
			}
			component := filepath.Join(paths.ModelDir, "unsafe-model.onnx")
			switch kind {
			case "symlink":
				if err := os.Symlink(outside, component); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(outside, component); err != nil {
					t.Fatal(err)
				}
			default:
				mode := os.FileMode(0440)
				if kind == "world-readable" {
					mode = 0444
				}
				if err := os.WriteFile(component, []byte("synthetic model component"), mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(component, mode); err != nil {
					t.Fatal(err)
				}
				assertMode(t, component, mode)
			}
			if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("unsafe model prepare = %v", err)
			}
			if _, err := Acquire(paths); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("unsafe model acquire = %v", err)
			}
			content, err := os.ReadFile(outside)
			if err != nil || string(content) != "synthetic outside component" {
				t.Fatal("outside component was changed")
			}
		})
	}
}
