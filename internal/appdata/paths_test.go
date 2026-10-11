package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func syntheticParent(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return parent
}

func syntheticPaths(t *testing.T) Paths {
	t.Helper()
	paths, err := Prepare(filepath.Join(syntheticParent(t), "synthetic-app"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("mode = %o, want %o", info.Mode().Perm(), want)
	}
}

func TestPrepareCreatesPrivateSeparatedPathsAndReopens(t *testing.T) {
	parent := syntheticParent(t)
	root := filepath.Join(parent, "nested", "synthetic-app")
	paths, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != root {
		t.Fatal("root was not canonical")
	}
	for _, child := range []string{paths.ArchiveDir, paths.SessionDir, paths.MediaDir, paths.WorkingDir, paths.ModelDir, paths.ClientDir} {
		if filepath.Dir(child) != root {
			t.Fatal("child escaped root")
		}
		assertMode(t, child, 0700)
	}
	if paths.ArchiveDir == paths.SessionDir {
		t.Fatal("archive and session share a directory")
	}
	assertMode(t, root, 0700)
	assertMode(t, filepath.Join(root, ".wa-indexer-owner"), 0600)
	again, err := Prepare(root)
	if err != nil || again != paths {
		t.Fatalf("reopen = %v", err)
	}
}

func TestDefaultRootUsesOSApplicationDirectory(t *testing.T) {
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(base, "WA Indexer") {
		t.Fatal("default root is outside application data directory")
	}
	if runtime.GOOS == "darwin" && filepath.Base(filepath.Dir(got)) != "Application Support" {
		t.Fatal("macOS default is not Application Support")
	}
}

func TestPrepareRefusesUnownedNonemptyRootWithoutModifyingIt(t *testing.T) {
	root := filepath.Join(syntheticParent(t), "unowned")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "unrelated.txt")
	if err := os.WriteFile(file, []byte("preserve me"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Prepare(root)
	if !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("error = %v", err)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "preserve me" {
		t.Fatal("unowned file changed")
	}
	if _, err := os.Lstat(filepath.Join(root, ".wa-indexer-owner")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unowned directory was claimed")
	}
	assertMode(t, root, 0755)
}

func TestPrepareClaimsExplicitEmptyRootAndRestrictsPermissions(t *testing.T) {
	root := filepath.Join(syntheticParent(t), "empty-explicit-root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	paths, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, paths.Root, 0700)
	assertMode(t, filepath.Join(paths.Root, ".wa-indexer-owner"), 0600)
}

func TestPrepareRejectsDangerousRootAndDotDot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", home, filepath.Join(syntheticParent(t), "child") + "/../other"} {
		_, err := Prepare(path)
		if !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestPrepareRejectsSymlinkComponentsAndOwnedChildren(t *testing.T) {
	parent := syntheticParent(t)
	out := filepath.Join(parent, "outside")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "app")} {
		if _, err := Prepare(path); !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("error = %v", err)
		}
	}
	paths := syntheticPaths(t)
	if err := os.Remove(paths.SessionDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, paths.SessionDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareRejectsChangedOwnershipMarkerAndPrivateFileSymlinks(t *testing.T) {
	for _, kind := range []string{"changed-marker", "marker-symlink", "owner-url-symlink", "lock-symlink"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			marker := filepath.Join(paths.Root, ".wa-indexer-owner")
			switch kind {
			case "changed-marker":
				if err := os.WriteFile(marker, []byte("another-application-v1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "marker-symlink":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(syntheticParent(t), "missing"), marker); err != nil {
					t.Fatal(err)
				}
			case "owner-url-symlink":
				if err := os.Symlink(filepath.Join(syntheticParent(t), "missing"), paths.OwnerURLFile); err != nil {
					t.Fatal(err)
				}
			case "lock-symlink":
				if err := os.Symlink(filepath.Join(syntheticParent(t), "missing"), filepath.Join(paths.Root, ".instance.lock")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
