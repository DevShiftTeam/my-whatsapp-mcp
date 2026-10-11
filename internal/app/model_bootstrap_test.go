package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"os"
	"path/filepath"
	"testing"
)

func TestModelControlDirectoriesWaitForOrderedRecoveryEOF(t *testing.T) {
	paths, err := appdata.Prepare(privateTestRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	disk, err := diskspace.New(paths.Root, diskspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &resources{paths: paths, instanceGuard: instance, disk: disk, recovery: &extractionRecovery{complete: false}}
	defer r.close(context.Background())
	r.recoveryPending.Store(true)
	if err = r.initializeModels(context.Background()); err == nil || r.models != nil || r.modelBootstrap != nil {
		t.Fatal("model metadata admitted before input ownership recovery")
	}
	entries, err := os.ReadDir(paths.ModelDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("pending recovery created model metadata")
	}
	r.recovery.complete = true
	if err = r.initializeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.models == nil || r.modelBootstrap != nil {
		t.Fatal("EOF did not activate model store with acknowledged bootstrap")
	}
	if _, err = r.Reserve(context.Background(), diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: 1}); err == nil {
		t.Fatal("constructor bootstrap bypassed ordinary payload gate")
	}
	if err = r.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUncertainModelControlInitializationRetainsGrowthUntilAcknowledgedRetry(t *testing.T) {
	paths, err := appdata.Prepare(privateTestRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	disk, err := diskspace.New(paths.Root, diskspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &resources{paths: paths, instanceGuard: instance, disk: disk, recovery: &extractionRecovery{complete: true}}
	unsafe := filepath.Join(paths.ModelDir, "versions")
	if err = os.Symlink(t.TempDir(), unsafe); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(unsafe); _ = r.close(context.Background()) }()
	if err = r.initializeModels(context.Background()); err == nil || r.modelBootstrap == nil || r.modelMetadataReady {
		t.Fatal("uncertain initialization lost growth fence")
	}
	owner := r.modelBootstrap
	if err = r.close(context.Background()); err == nil || r.modelBootstrap != owner {
		t.Fatal("failed cleanup forgot metadata growth owner")
	}
	if err = os.Remove(unsafe); err != nil {
		t.Fatal(err)
	}
	if err = r.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.modelBootstrap != nil || r.models != nil || r.disk != nil {
		t.Fatal("acknowledged retry retained resource ownership")
	}
}
