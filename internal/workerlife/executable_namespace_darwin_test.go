//go:build darwin

package workerlife

import (
	"context"
	"os"
	"runtime"
	"testing"
)

func TestExecutableNamespaceActualDarwinSelfExecutable(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("ARM64 namespace contract")
	}
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var growth namespaceTestGrowth
	ns, _ := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
	lease, err := ns.Prepare(context.Background(), source, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Path() == "" {
		t.Fatal("verified self executable not published")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}
