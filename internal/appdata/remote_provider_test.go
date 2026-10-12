package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderDirectoryLifetime(t *testing.T) {
	p := syntheticPaths(t)
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	d, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider)
	if err != nil {
		t.Fatal(err)
	}
	path, err := d.Path()
	if err != nil || path != filepath.Join(p.RemoteDir, "tailscale") {
		t.Fatal(path, err)
	}
	if _, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider); !errors.Is(err, ErrRemoteProviderActive) {
		t.Fatal(err)
	}
	if err := l.Close(); !errors.Is(err, ErrRemoteProviderActive) {
		t.Fatal(err)
	}
	if err := ResetOwned(p, l); !errors.Is(err, ErrRemoteProviderActive) {
		t.Fatal(err)
	}
	if err := WithRemoteAccessRemoval(p, l, func(*os.Root) error { return nil }); !errors.Is(err, ErrRemoteProviderActive) {
		t.Fatal(err)
	}
	f, err := d.OpenFile("config", true)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := d.OpenFile("../escape", true); err == nil {
		t.Fatal("escape")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d.Validate() == nil {
		t.Fatal("closed admitted")
	}
	if err := ResetOwned(p, l); err != nil {
		t.Fatal(err)
	}
}
func TestProviderDirectoryReplacementAndScopedValidation(t *testing.T) {
	p := syntheticPaths(t)
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	d, err := OpenRemoteProviderDirectory(p, l, CloudflareProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	path, _ := d.Path()
	// Live SDK children are mutable; direct credentials must remain strict.
	if err := os.WriteFile(filepath.Join(path, "transient"), []byte("sdk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WithRemoteAccess(p, l, func(*os.Root) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.RemoteDir, "credential"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if WithRemoteAccess(p, l, func(*os.Root) error { return nil }) == nil {
		t.Fatal("credential relaxed")
	}
	os.Remove(filepath.Join(p.RemoteDir, "credential"))
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if d.Validate() == nil {
		t.Fatal("replacement accepted")
	}
}
func TestInactiveProviderDirectoryRemainsStrict(t *testing.T) {
	p := syntheticPaths(t)
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	d, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := d.Path()
	d.Close()
	os.WriteFile(filepath.Join(path, "unsafe"), []byte("x"), 0644)
	if _, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider); err == nil {
		t.Fatal("unsafe inactive tree")
	}
}
