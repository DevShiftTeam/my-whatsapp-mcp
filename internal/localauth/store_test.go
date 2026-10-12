package localauth

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

func authFixture(t *testing.T) (*Authenticator, appdata.Paths, *appdata.InstanceLock) {
	t.Helper()
	p, e := appdata.Prepare(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	l, e := appdata.Acquire(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	a, e := NewAuthenticator(p, l)
	if e != nil {
		t.Fatal(e)
	}
	return a, p, l
}
func TestRecordPersistenceGenerationAndClose(t *testing.T) {
	a, p, l := authFixture(t)
	ctx := context.Background()
	password := []byte("first exact passphrase")
	if a.Generation() != 0 {
		t.Fatal("configured before setup")
	}
	if e := a.SetPassword(ctx, password); e != nil {
		t.Fatal(e)
	}
	old := a.Generation()
	name := filepath.Join(p.RemoteDir, RecordFilename)
	info, e := os.Stat(name)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private record", e)
	}
	b, e := NewAuthenticator(p, l)
	if e != nil {
		t.Fatal(e)
	}
	if ok, e := b.Verify(ctx, password, old); e != nil || !ok {
		t.Fatal("reload", e)
	}
	b.Close()
	if e := a.SetPassword(ctx, []byte("new exact passphrase")); e != nil {
		t.Fatal(e)
	}
	if a.Generation() != old+1 {
		t.Fatal("generation not advanced")
	}
	if ok, _ := a.Verify(ctx, password, old); ok {
		t.Fatal("old generation survived")
	}
	a.Close()
	if a.Generation() != 0 {
		t.Fatal("closed remains active")
	}
	if e := a.SetPassword(ctx, password); e == nil {
		t.Fatal("closed write accepted")
	}
}
func TestRecordRejectsUnsafeAndUnknown(t *testing.T) {
	for _, kind := range []string{"permissions", "symlink", "oversize", "version"} {
		t.Run(kind, func(t *testing.T) {
			a, p, l := authFixture(t)
			if e := a.SetPassword(context.Background(), []byte("long enough passphrase")); e != nil {
				t.Fatal(e)
			}
			name := filepath.Join(p.RemoteDir, RecordFilename)
			switch kind {
			case "permissions":
				os.Chmod(name, 0644)
			case "symlink":
				os.Remove(name)
				os.Symlink("../outside", name)
			case "oversize":
				os.WriteFile(name, make([]byte, 1000), 0600)
			case "version":
				data, _ := os.ReadFile(name)
				data[8] = 255
				os.WriteFile(name, data, 0600)
			}
			if _, e := NewAuthenticator(p, l); e == nil {
				t.Fatal("unsafe record accepted")
			}
		})
	}
}

func TestInFlightVerificationAndPasswordWriteCannotSurviveClose(t *testing.T) {
	for _, operation := range []string{"verify", "write"} {
		t.Run(operation, func(t *testing.T) {
			a, p, _ := authFixture(t)
			ctx := context.Background()
			password := []byte("in flight passphrase")
			if e := a.SetPassword(ctx, password); e != nil {
				t.Fatal(e)
			}
			before, _ := os.ReadFile(filepath.Join(p.RemoteDir, RecordFilename))
			generation := a.Generation()
			hashed, release := make(chan struct{}), make(chan struct{})
			a.afterHash = func() { close(hashed); <-release }
			done := make(chan bool, 1)
			go func() {
				if operation == "verify" {
					ok, _ := a.Verify(ctx, password, generation)
					done <- ok
				} else {
					done <- a.SetPassword(ctx, []byte("new in flight phrase")) == nil
				}
			}()
			<-hashed
			a.Close()
			close(release)
			if <-done {
				t.Fatal("closed realm admitted in-flight result")
			}
			after, _ := os.ReadFile(filepath.Join(p.RemoteDir, RecordFilename))
			if string(before) != string(after) {
				t.Fatal("stale password committed")
			}
		})
	}
}

func TestUncertainPasswordCommitFencesOldAndNewGeneration(t *testing.T) {
	a, p, l := authFixture(t)
	ctx := context.Background()
	old := []byte("old owner passphrase")
	if e := a.SetPassword(ctx, old); e != nil {
		t.Fatal(e)
	}
	generation := a.Generation()
	a.afterCommit = func() error { return ErrState }
	if e := a.SetPassword(ctx, []byte("new owner passphrase")); e == nil {
		t.Fatal("uncertain commit reported successful")
	}
	if a.Generation() != 0 {
		t.Fatal("uncertain commit remains authorized")
	}
	if ok, _ := a.Verify(ctx, old, generation); ok {
		t.Fatal("old password authorized")
	}
	if ok, _ := a.Verify(ctx, []byte("new owner passphrase"), generation+1); ok {
		t.Fatal("uncertain new generation authorized")
	}
	reopened, e := NewAuthenticator(p, l)
	if e != nil {
		t.Fatal(e)
	}
	if reopened.Generation() != generation+1 {
		t.Fatal("committed record lost")
	}
	if ok, e := reopened.Verify(ctx, []byte("new owner passphrase"), generation+1); e != nil || !ok {
		t.Fatal("explicit reconstruction failed", e)
	}
}

func TestPasswordResetRejectsInFlightVerification(t *testing.T) {
	a, _, _ := authFixture(t)
	ctx := context.Background()
	p := []byte("unchanged owner phrase")
	if e := a.SetPassword(ctx, p); e != nil {
		t.Fatal(e)
	}
	generation := a.Generation()
	hashed, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	a.afterHash = func() {
		if first.Swap(false) {
			close(hashed)
			<-release
		}
	}
	done := make(chan bool, 1)
	go func() { ok, _ := a.Verify(ctx, p, generation); done <- ok }()
	<-hashed
	if e := a.SetPassword(ctx, p); e != nil {
		t.Fatal(e)
	}
	close(release)
	if <-done {
		t.Fatal("in-flight verification survived generation change")
	}
	if a.Generation() != generation+1 {
		t.Fatal("password reset did not advance")
	}
}

func TestTryGenerationNeverWaitsOrAuthorizesClosedState(t *testing.T) {
	a := &Authenticator{record: Record{generation: 7}}
	reader, ok := any(a).(interface{ TryGeneration() (uint64, bool) })
	if !ok {
		t.Fatal("nonblocking generation admission unavailable")
	}
	if g, ok := reader.TryGeneration(); !ok || g != 7 {
		t.Fatal("current generation rejected")
	}
	a.mu.Lock()
	if g, ok := reader.TryGeneration(); ok || g != 0 {
		a.mu.Unlock()
		t.Fatal("contended authority admitted")
	}
	a.mu.Unlock()
	a.Close()
	if g, ok := reader.TryGeneration(); ok || g != 0 {
		t.Fatal("closed authority admitted")
	}
}
