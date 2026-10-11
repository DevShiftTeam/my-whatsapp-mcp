package modelstore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMutableAncestorRejected(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	if s, err := Open(filepath.Join(parent, "models"), catalogFor(component("model", []byte("x"), "")), Options{}); !errors.Is(err, ErrUnsafeFile) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("mutable ancestor allowed: %v", err)
	}
}

func TestDarwinGrantingACLRejected(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin extended ACL")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "models")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/bin/chmod", "+a", "everyone allow read,write,execute", root).Run(); err != nil {
		t.Fatal(err)
	}
	defer exec.Command("/bin/chmod", "-N", root).Run()
	if s, err := Open(root, catalogFor(component("model", []byte("x"), "")), Options{}); !errors.Is(err, ErrUnsafeFile) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("granting ACL allowed: %v", err)
	}
}

func TestDarwinGrantingAncestorACLRejected(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin extended ACL")
	}
	parent := t.TempDir()
	if err := exec.Command("/bin/chmod", "+a", "everyone allow write", parent).Run(); err != nil {
		t.Fatal(err)
	}
	defer exec.Command("/bin/chmod", "-N", parent).Run()
	if s, err := Open(filepath.Join(parent, "models"), catalogFor(component("model", []byte("x"), "")), Options{}); !errors.Is(err, ErrUnsafeFile) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("granting ancestor ACL allowed: %v", err)
	}
}

func TestDarwinDenyOnlyAncestorACLAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin extended ACL")
	}
	parent := t.TempDir()
	if err := exec.Command("/bin/chmod", "+a", "everyone deny delete", parent).Run(); err != nil {
		t.Fatal(err)
	}
	defer exec.Command("/bin/chmod", "-N", parent).Run()
	s, err := Open(filepath.Join(parent, "models"), catalogFor(component("model", []byte("x"), "")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}
