package remoteaccess

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
)

func TestProviderRemovePreservesIndependentOwnerPassword(t *testing.T) {
	s, p := testDiskStore(t)
	ctx := context.Background()
	a, e := localauth.NewAuthenticator(p, s.lease)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.SetPassword(ctx, []byte("independent owner password")); e != nil {
		t.Fatal(e)
	}
	if e = s.Save(ctx, storedIntent(), Secret("provider-secret")); e != nil {
		t.Fatal(e)
	}
	name := filepath.Join(p.RemoteDir, localauth.RecordFilename)
	before, _ := os.ReadFile(name)
	if e = s.Remove(ctx); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(name)
	if !bytes.Equal(before, after) {
		t.Fatal("provider removal changed password")
	}
	if ok, e := a.Verify(ctx, []byte("independent owner password"), a.Generation()); e != nil || !ok {
		t.Fatal("owner login removed", e)
	}
	entries, _ := os.ReadDir(p.RemoteDir)
	if len(entries) != 1 || entries[0].Name() != localauth.RecordFilename {
		t.Fatal("provider leftovers")
	}
}
func TestProviderRemoveDoesNotExemptUnsafeOwnerRecord(t *testing.T) {
	for _, kind := range []string{"corrupt", "permissions", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, p := testDiskStore(t)
			if e := s.Save(context.Background(), storedIntent(), Secret("provider-secret")); e != nil {
				t.Fatal(e)
			}
			name := filepath.Join(p.RemoteDir, localauth.RecordFilename)
			switch kind {
			case "corrupt":
				os.WriteFile(name, []byte("unrecognized"), 0600)
			case "permissions":
				os.WriteFile(name, []byte("unrecognized"), 0644)
			case "symlink":
				os.Symlink("../outside", name)
			}
			if e := s.Remove(context.Background()); e == nil {
				t.Fatal("invalid independently owned record exempted")
			}
		})
	}
}

func TestProviderRemoveSerializesWithOwnerPasswordCommit(t *testing.T) {
	s, p := testDiskStore(t)
	ctx := context.Background()
	a, e := localauth.NewAuthenticator(p, s.lease)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.SetPassword(ctx, []byte("initial owner password")); e != nil {
		t.Fatal(e)
	}
	if e = s.Save(ctx, storedIntent(), Secret("provider-secret")); e != nil {
		t.Fatal(e)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	go func() { <-start; done <- s.Remove(ctx) }()
	go func() { <-start; done <- a.SetPassword(ctx, []byte("replacement owner password")) }()
	close(start)
	for i := 0; i < 2; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	b, e := localauth.NewAuthenticator(p, s.lease)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if b.Generation() != 2 {
		t.Fatal("concurrent provider removal erased password commit")
	}
	if ok, e := b.Verify(ctx, []byte("replacement owner password"), 2); e != nil || !ok {
		t.Fatal("replacement password", e)
	}
}
