//go:build darwin

package appdata

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func grantTestACL(t *testing.T, path string, inherit bool) {
	t.Helper()
	grant := "everyone allow read,write,execute"
	if inherit {
		grant += ",file_inherit,directory_inherit"
	}
	if err := exec.Command("/bin/chmod", "+a", grant, path).Run(); err != nil {
		t.Fatal("could not create synthetic granting ACL")
	}
}

func TestPrepareRefusesGrantingACLEmptyRootWithoutClaiming(t *testing.T) {
	root := filepath.Join(syntheticParent(t), "empty-acl-root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	grantTestACL(t, root, false)
	if _, err := Prepare(root); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("error = %v", err)
	}
	assertMode(t, root, 0755)
	if _, err := os.Lstat(filepath.Join(root, ".wa-indexer-owner")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ACL-bearing empty root was claimed")
	}
}

func TestPrepareRefusesInheritedGrantingACL(t *testing.T) {
	parent := syntheticParent(t)
	grantTestACL(t, parent, true)
	root := filepath.Join(parent, "new-root")
	if _, err := Prepare(root); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".wa-indexer-owner")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inherited ACL root was claimed")
	}
}

func TestPrepareRefusesACLOnOwnedObjects(t *testing.T) {
	for _, kind := range []string{"root", "marker", "directory", "file", "read-only-model",
		"archive.sqlite", "archive.sqlite-wal", "archive.sqlite-shm",
		"session.sqlite", "session.sqlite-wal", "session.sqlite-shm"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			var path string
			switch kind {
			case "root":
				path = paths.Root
			case "marker":
				path = filepath.Join(paths.Root, ".wa-indexer-owner")
			case "directory":
				path = paths.SessionDir
			case "file":
				path = filepath.Join(paths.ArchiveDir, "synthetic-data")
				if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
					t.Fatal(err)
				}
			case "read-only-model":
				path = filepath.Join(paths.ModelDir, "synthetic-model.onnx")
				if err := os.WriteFile(path, []byte("synthetic"), 0400); err != nil {
					t.Fatal(err)
				}
			case "archive.sqlite", "archive.sqlite-wal", "archive.sqlite-shm":
				path = filepath.Join(paths.ArchiveDir, kind)
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "session.sqlite", "session.sqlite-wal", "session.sqlite-shm":
				path = filepath.Join(paths.SessionDir, kind)
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			grantTestACL(t, path, false)
			if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPrepareRejectsUnsafeManagedSQLiteFiles(t *testing.T) {
	for _, name := range []string{"archive/archive.sqlite", "archive/archive.sqlite-wal", "archive/archive.sqlite-shm",
		"session/session.sqlite", "session/session.sqlite-wal", "session/session.sqlite-shm"} {
		for _, kind := range []string{"public", "symlink", "hardlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				paths := syntheticPaths(t)
				path := filepath.Join(paths.Root, name)
				if kind == "public" {
					if err := os.WriteFile(path, nil, 0644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				} else {
					target := filepath.Join(syntheticParent(t), "target")
					if err := os.WriteFile(target, nil, 0600); err != nil {
						t.Fatal(err)
					}
					var err error
					if kind == "symlink" {
						err = os.Symlink(target, path)
					} else {
						err = os.Link(target, path)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
					t.Fatalf("unsafe managed SQLite file error = %v", err)
				}
			})
		}
	}
}

func TestWithClientsRefusesGrantingACLBeforeCallback(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	grantTestACL(t, paths.ClientDir, false)
	called := false
	if err := WithClients(paths, lease, func(root *os.Root) error { called = true; return nil }); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("clients ACL fixed error = %v", err)
	}
	if called {
		t.Fatal("client callback ran with granting ACL")
	}
}

func TestReadOwnerURLRefusesGrantingACL(t *testing.T) {
	for _, kind := range []string{"root", "marker", "owner"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			if err := os.WriteFile(paths.OwnerURLFile, []byte(syntheticAttachURL()+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			path := paths.OwnerURLFile
			if kind == "root" {
				path = paths.Root
			}
			if kind == "marker" {
				path = filepath.Join(paths.Root, ownerMarker)
			}
			grantTestACL(t, path, false)
			if _, err := ExistingPaths(paths.Root); kind != "owner" && !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("existing ACL fixed error = %v", err)
			}
			if got, err := ReadOwnerURL(paths); got != "" || !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("read ACL fixed error = %v", err)
			}
		})
	}
}

func TestNativeSocketRefusesGrantingACL(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	socket, err := ListenNativeSocket(context.Background(), paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	grantTestACL(t, socket.Path(), false)
	if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("socket ACL preflight=%v", err)
	}
	if conn, err := DialNativeSocket(context.Background(), socket.Path()); conn != nil || !errors.Is(err, ErrUnsafeDataPath) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("socket ACL dial=%v", err)
	}
}
