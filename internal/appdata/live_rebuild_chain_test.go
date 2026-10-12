package appdata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireInheritedExistingRejectsUnlockedDescriptor(t *testing.T) {
	paths, err := Prepare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	f, err := os.OpenFile(filepath.Join(paths.Root, instanceLockFile), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	held, err := AcquireInheritedExisting(paths, int(f.Fd()))
	if err == nil {
		held.Close()
		t.Fatal("unlocked descriptor accepted as inherited authority")
	}
}
