//go:build darwin || linux

package app

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"
)

func TestNativeNamespaceRejectsForeignPhysicalDevice(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	device := namespaceStatDevice(info.Sys().(*syscall.Stat_t))
	if !namespaceDeviceMatches(info, device) || namespaceDeviceMatches(info, device+1) || namespaceDeviceMatches(info, 0) {
		t.Fatal("namespace physical reservation device binding failed")
	}
}

type retryNamespace struct {
	calls   int
	failure error
}

func (n *retryNamespace) Recover(context.Context) error { n.calls++; return n.failure }
func (*retryNamespace) Close() error                    { return nil }
func (*retryNamespace) Prepare(context.Context, *os.File, int64) (workerlife.ExecutableLease, error) {
	return nil, errNativeNamespace
}

func TestNativeNamespaceRequiresAcquiredInstanceBeforeRecovery(t *testing.T) {
	n := &retryNamespace{failure: errors.New("cleanup uncertainty")}
	r := &resources{namespace: n, recovery: &extractionRecovery{complete: true}}
	// Missing instance authority must fail before any namespace cleanup call.
	if err := r.initializeExecutableNamespace(context.Background(), r.paths); err == nil || n.calls != 0 || r.namespace != n {
		t.Fatal("namespace escaped acquired-instance recovery boundary")
	}
}

func TestFailedNamespaceMkdirRetryRetainsOneGrowthOwner(t *testing.T) {
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
	defer disk.Close()
	r := &resources{paths: paths, instanceGuard: instance, disk: disk, recovery: &extractionRecovery{complete: true}}
	calls := 0
	r.namespaceMkdir = func(root *os.Root) error {
		calls++
		if calls <= 2 {
			return errors.New("injected mkdir failure")
		}
		return root.Mkdir("native-exec", 0700)
	}
	if err = r.initializeExecutableNamespace(context.Background(), paths); err == nil {
		t.Fatal("failed mkdir accepted")
	}
	first := r.namespaceBootstrap
	before, err := disk.Snapshot()
	if err != nil || before.ActiveLeases != 1 || first == nil {
		t.Fatal("failed mkdir lost finite growth ownership")
	}
	if err = r.initializeExecutableNamespace(context.Background(), paths); err == nil {
		t.Fatal("second failed mkdir accepted")
	}
	middle, err := disk.Snapshot()
	if err != nil || middle.ActiveLeases != 1 || r.namespaceBootstrap != first {
		t.Fatal("retry overwrote retained growth owner")
	}
	if err = r.initializeExecutableNamespace(context.Background(), paths); err != nil {
		t.Fatal(err)
	}
	after, err := disk.Snapshot()
	if err != nil || after.ActiveLeases != 0 || r.namespaceBootstrap != nil || calls != 3 {
		t.Fatal("mkdir retry leaked or replaced growth ownership")
	}
	if err = r.namespace.Close(); err != nil {
		t.Fatal(err)
	}
	if err = disk.Close(); err != nil {
		t.Fatal("broker still has active bootstrap ownership")
	}
}
