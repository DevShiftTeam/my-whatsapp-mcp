package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerPasswordResetLifetime(t *testing.T) {
	a := managedTestApp(t)
	ctx := context.Background()
	auth, e := a.RemoteOwnerAuthenticator()
	if e != nil {
		t.Fatal(e)
	}
	if auth.Generation() != 0 {
		t.Fatal("unexpected initial password")
	}
	if e = a.SetRemoteOwnerPassword(ctx, []byte("old owner passphrase")); e != nil {
		t.Fatal(e)
	}
	if auth.Generation() == 0 {
		t.Fatal("not shared")
	}
	if e = a.DeleteLocalData(ctx, true); e != nil {
		t.Fatal(e)
	}
	if auth.Generation() != 0 {
		t.Fatal("retained old realm")
	}
	if e = auth.SetPassword(ctx, []byte("old owner passphrase")); e == nil {
		t.Fatal("stale commit survived reset")
	}
	if _, e = os.Stat(filepath.Join(a.paths.RemoteDir, localauth.RecordFilename)); !os.IsNotExist(e) {
		t.Fatal("password persisted through reset", e)
	}
	next, e := a.RemoteOwnerAuthenticator()
	if e != nil || next == auth || next.Generation() != 0 {
		t.Fatal("fresh realm", e)
	}
	if e = a.SetRemoteOwnerPassword(ctx, []byte("new owner passphrase")); e != nil {
		t.Fatal(e)
	}
	auth.Close()
	if next.Generation() == 0 {
		t.Fatal("old close cleared new realm")
	}
	if e = a.Close(ctx); e != nil {
		t.Fatal(e)
	}
	if next.Generation() != 0 {
		t.Fatal("close failed to revoke")
	}
}

func TestOwnerPasswordIsLazyAndOptionalCorruptionKeepsArchiveAvailable(t *testing.T) {
	a := managedTestApp(t)
	if a.ownerAuth.auth != nil {
		t.Fatal("disabled startup constructed owner authentication")
	}
	if e := os.MkdirAll(a.paths.RemoteDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(a.paths.RemoteDir, localauth.RecordFilename), []byte("corrupt private password record"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := a.RemoteOwnerAuthenticator(); e == nil {
		t.Fatal("corrupt record silently replaced")
	}
	if a.ownerAuth.auth != nil {
		t.Fatal("failed authentication published")
	}
	if _, e := a.Status(context.Background()); e != nil {
		t.Fatal("optional auth failure broke archive", e)
	}
}
