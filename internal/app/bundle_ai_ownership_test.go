package app

import (
	"context"
	"os"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

func TestEarlyResourceFailureReturnsBundleCleanupOwner(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	assets := &bundledAIAssets{root: root, files: make(map[string]*os.File)}
	bundle := &bundledAI{assets: assets}
	resources, err := openResources(context.Background(), appdata.Paths{Root: dir + "/missing"}, nil, Config{Native: &NativeConfig{bundle: bundle}}, false)
	if err == nil || resources == nil || len(resources.bundles) != 1 || resources.bundles[0] != bundle {
		t.Fatal("early broker failure lost captured bundle owner")
	}
	if err := resources.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if assets.root != nil {
		t.Fatal("acknowledged partial cleanup retained bundle root")
	}
	if err := resources.close(context.Background()); err != nil {
		t.Fatal("cleanup retry is not idempotent")
	}
}
