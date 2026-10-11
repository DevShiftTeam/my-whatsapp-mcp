//go:build darwin || linux

package diskspace

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func privateRoot(t *testing.T) string {
	t.Helper()
	// TempDir can itself be reached through /var on Darwin. Resolve only the
	// trusted test fixture; production constructors must reject symlink paths.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func testController(t *testing.T, free int64) *Controller {
	t.Helper()
	c, err := New(privateRoot(t), Options{HeadroomBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	c.query = func(*os.File) (filesystemSpace, error) {
		return filesystemSpace{AvailableBlocks: uint64(free), BlockSize: 1}, nil
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("controller cleanup: %v", err)
		}
	})
	return c
}

func TestConcurrentReservationsSharePhysicalCapacity(t *testing.T) {
	// Missing global serialization or separate per-class pools admits >9 writers.
	c := testController(t, 100)
	var wg sync.WaitGroup
	var leasesMu sync.Mutex
	var leases []Lease
	var denied atomic.Int64
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			class := []Class{Original, Decoded, ModelInstall}[i%3]
			lease, err := c.Reserve(context.Background(), Request{Class: class, MaximumGrowthBytes: 10})
			if errors.Is(err, ErrInsufficientSpace) {
				denied.Add(1)
				return
			}
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			leasesMu.Lock()
			leases = append(leases, lease)
			leasesMu.Unlock()
		}(i)
	}
	wg.Wait()
	s, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 9 || denied.Load() != 21 || s.ReservedBytes != 90 || s.ActiveLeases != 9 || s.AvailableBytes != 0 {
		t.Fatalf("over-admission: admitted=%d denied=%d snapshot=%+v", len(leases), denied.Load(), s)
	}
	if s.OriginalBytes+s.DecodedBytes+s.ModelInstallBytes != 90 {
		t.Fatalf("class accounting: %+v", s)
	}
	for _, lease := range leases {
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	lease, err := c.Reserve(context.Background(), Request{Class: ModelInstall, MaximumGrowthBytes: 90})
	if err != nil {
		t.Fatalf("released capacity not reusable: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestControllerCopiesShareCapacityAndLifetime(t *testing.T) {
	// A by-value copy must not create another mutex/accounting pool over one fd.
	c := testController(t, 100)
	copy := *c
	lease, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 90})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if admitted, err := copy.Reserve(context.Background(), Request{Class: Decoded, MaximumGrowthBytes: 1}); !errors.Is(err, ErrInsufficientSpace) {
		if admitted != nil {
			admitted.Release()
		}
		t.Fatalf("controller copy created a separate capacity pool: %v", err)
	}
	if err := copy.Close(); !errors.Is(err, ErrLeasesActive) {
		t.Fatalf("copy closed descriptor owned by active lease: %v", err)
	}
	if _, err := c.Reserve(context.Background(), Request{Class: ModelInstall, MaximumGrowthBytes: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("copy shutdown did not stop original: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := copy.Snapshot(); !errors.Is(err, ErrClosed) {
		t.Fatalf("copy did not observe descriptor closure: %v", err)
	}
}

func TestLeasePinsAcquiredStateAcrossControllerAssignment(t *testing.T) {
	// Reassigning the public wrapper must not redirect an existing cleanup lease.
	c := testController(t, 100)
	original := *c
	t.Cleanup(func() {
		if err := original.Close(); err != nil {
			t.Errorf("original anchor cleanup: %v", err)
		}
	})
	lease, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 90})
	if err != nil {
		t.Fatal(err)
	}
	other := testController(t, 100)
	otherLease, err := other.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 90})
	if err != nil {
		t.Fatal(err)
	}
	defer otherLease.Release()
	*c = *other
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	s, err := original.Snapshot()
	if err != nil || s.ReservedBytes != 0 || s.ActiveLeases != 0 {
		t.Fatalf("release abandoned the acquired state: %+v %v", s, err)
	}
	s, err = other.Snapshot()
	if err != nil || s.ReservedBytes != 90 || s.ActiveLeases != 1 {
		t.Fatalf("release changed the replacement state: %+v %v", s, err)
	}
	if _, err := other.Reserve(context.Background(), Request{Class: Decoded, MaximumGrowthBytes: 1}); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("redirected release freed another live writer's capacity: %v", err)
	}
}

func TestCancellationDoesNotReleaseActiveLease(t *testing.T) {
	// A cancellation callback must never free capacity while a writer survives.
	c := testController(t, 100)
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := c.Reserve(ctx, Request{Class: Original, MaximumGrowthBytes: 90})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := c.Reserve(context.Background(), Request{Class: Decoded, MaximumGrowthBytes: 1}); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("cancellation freed active writer capacity: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("repeated release: %v", err)
	}
	if _, err := c.Reserve(ctx, Request{Class: Original, MaximumGrowthBytes: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled admission: %v", err)
	}
	s, err := c.Snapshot()
	if err != nil || s.ActiveLeases != 0 || s.ReservedBytes != 0 {
		t.Fatalf("release exactly once: %+v %v", s, err)
	}
}

func TestConcurrentReleaseAcknowledgesExactlyOnce(t *testing.T) {
	// Racing cleanup acknowledgements cannot create negative/reusable capacity.
	c := testController(t, 100)
	lease, err := c.Reserve(context.Background(), Request{Class: ModelInstall, MaximumGrowthBytes: 90})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if err := lease.Release(); err != nil {
				t.Errorf("release: %v", err)
			}
		})
	}
	wg.Wait()
	s, err := c.Snapshot()
	if err != nil || s.ReservedBytes != 0 || s.ActiveLeases != 0 || s.ModelInstallBytes != 0 || s.AvailableBytes != 90 {
		t.Fatalf("release decremented more than once: %+v %v", s, err)
	}
	if _, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 91}); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("double release invented capacity: %v", err)
	}
}

func TestCancellationDuringMeasurementDoesNotAdmit(t *testing.T) {
	// A context lost while awaiting a filesystem query must not acquire a lease.
	c := testController(t, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.query = func(*os.File) (filesystemSpace, error) {
		cancel()
		return filesystemSpace{AvailableBlocks: 100, BlockSize: 1}, nil
	}
	if _, err := c.Reserve(ctx, Request{Class: Original, MaximumGrowthBytes: 90}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled during query acquired lease: %v", err)
	}
	s, err := c.Snapshot()
	if err != nil || s.ActiveLeases != 0 || s.ReservedBytes != 0 {
		t.Fatalf("canceled query changed accounting: %+v %v", s, err)
	}
}

func TestMeasurementFailurePreservesOutstandingLease(t *testing.T) {
	// A failed probe cannot erase capacity held by an existing writer.
	c := testController(t, 100)
	lease, err := c.Reserve(context.Background(), Request{Class: Decoded, MaximumGrowthBytes: 40})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	c.query = func(*os.File) (filesystemSpace, error) {
		return filesystemSpace{}, errors.New("private path must not escape")
	}
	if _, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 1}); err != ErrUnavailable {
		t.Fatalf("probe did not fail closed with a fixed error: %v", err)
	}
	c.query = func(*os.File) (filesystemSpace, error) {
		return filesystemSpace{AvailableBlocks: 100, BlockSize: 1}, nil
	}
	s, err := c.Snapshot()
	if err != nil || s.ActiveLeases != 1 || s.ReservedBytes != 40 || s.DecodedBytes != 40 {
		t.Fatalf("failed measurement dropped ownership: %+v %v", s, err)
	}
}

func TestFreshFreeSpaceDeclineDeniesNewWriter(t *testing.T) {
	// Reusing constructor/preflight free space misses unrelated physical writes.
	c := testController(t, 100)
	var free atomic.Uint64
	free.Store(100)
	c.query = func(*os.File) (filesystemSpace, error) {
		return filesystemSpace{AvailableBlocks: free.Load(), BlockSize: 1}, nil
	}
	lease, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	free.Store(45)
	if _, err := c.Reserve(context.Background(), Request{Class: ModelInstall, MaximumGrowthBytes: 6}); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("did not remeasure decline: %v", err)
	}
	s, err := c.Snapshot()
	if err != nil || s.FreeBytes != 45 || s.AvailableBytes != 5 || s.ReservedBytes != 30 {
		t.Fatalf("decline accounting: %+v %v", s, err)
	}
	free.Store(5)
	s, err = c.Snapshot()
	if err != nil || s.AvailableBytes != 0 || s.ReservedBytes != 30 {
		t.Fatalf("low free must retain reservations: %+v %v", s, err)
	}
}

func TestCloseRetainsActiveReservationsUntilAcknowledged(t *testing.T) {
	// Close must not erase outstanding ownership or allow admission while draining.
	c := testController(t, 100)
	lease, err := c.Reserve(context.Background(), Request{Class: Decoded, MaximumGrowthBytes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); !errors.Is(err, ErrLeasesActive) {
		t.Fatalf("close freed active reservations: %v", err)
	}
	if _, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("draining accepted writer: %v", err)
	}
	s, err := c.Snapshot()
	if err != nil || s.ReservedBytes != 30 || s.ActiveLeases != 1 {
		t.Fatalf("close lost held ownership: %+v %v", s, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed descriptor remained usable: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}

func TestCloseRacingReserveAndReleasePreservesOwnership(t *testing.T) {
	// Shutdown cannot close a descriptor that a racing admitted lease still owns.
	c := testController(t, 100)
	lease, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 90})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		if err := lease.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})
	wg.Go(func() {
		<-start
		if err := c.Close(); err != nil && !errors.Is(err, ErrLeasesActive) {
			t.Errorf("racing close: %v", err)
		}
	})
	for range 30 {
		wg.Go(func() {
			<-start
			admitted, err := c.Reserve(context.Background(), Request{Class: Decoded, MaximumGrowthBytes: 1})
			if errors.Is(err, ErrClosed) || errors.Is(err, ErrInsufficientSpace) {
				return
			}
			if err != nil {
				t.Errorf("racing reserve: %v", err)
				return
			}
			if err := admitted.Release(); err != nil {
				t.Errorf("racing admitted lease release: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if err := c.Close(); err != nil {
		t.Fatalf("close after all acknowledgements: %v", err)
	}
	if _, err := c.Snapshot(); !errors.Is(err, ErrClosed) {
		t.Fatalf("descriptor ownership survived successful close: %v", err)
	}
}

func TestCheckedSpaceArithmeticRejectsOverflow(t *testing.T) {
	// Wrapped multiplication/addition must never create artificial free capacity.
	c := testController(t, math.MaxInt64)
	for _, space := range []filesystemSpace{
		{AvailableBlocks: math.MaxUint64, BlockSize: 2},
		{AvailableBlocks: uint64(math.MaxInt64) + 1, BlockSize: 1},
		{AvailableBlocks: 1, BlockSize: 0},
	} {
		c.query = func(*os.File) (filesystemSpace, error) { return space, nil }
		if _, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 1}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unsafe measured space %+v: %v", space, err)
		}
	}
	c.query = func(*os.File) (filesystemSpace, error) {
		return filesystemSpace{AvailableBlocks: math.MaxInt64, BlockSize: 1}, nil
	}
	lease, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: math.MaxInt64 - 10})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := c.Reserve(context.Background(), Request{Class: ModelInstall, MaximumGrowthBytes: 1}); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("addition overflow admitted: %v", err)
	}
}

func TestInvalidRequestNeverCreatesReservation(t *testing.T) {
	c := testController(t, 100)
	for _, request := range []Request{
		{Class: "unknown", MaximumGrowthBytes: 1},
		{Class: Original, MaximumGrowthBytes: -1},
		{Class: Original, MaximumGrowthBytes: 0},
	} {
		if _, err := c.Reserve(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid request %+v: %v", request, err)
		}
	}
	if _, err := c.Reserve(nil, Request{Class: Original, MaximumGrowthBytes: 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil context: %v", err)
	}
	s, err := c.Snapshot()
	if err != nil || s.ActiveLeases != 0 || s.ReservedBytes != 0 {
		t.Fatalf("invalid request changed accounting: %+v %v", s, err)
	}
}

func TestRealFilesystemSnapshotUsesRetainedDirectory(t *testing.T) {
	// An absolute-path reopen would follow the replacement or fail after rename.
	root := privateRoot(t)
	c, err := New(root, Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	before, err := c.Snapshot()
	if err != nil || before.FreeBytes <= 0 || before.Device == 0 {
		t.Fatalf("anchored filesystem measurement: %+v %v", before, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	relocated := root + "-original"
	if err := os.Rename(root, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(root), root); err != nil {
		t.Fatal(err)
	}
	// Keep the real filesystem query, while asserting which descriptor it sees.
	query := c.query
	c.query = func(file *os.File) (filesystemSpace, error) {
		anchored, err := file.Stat()
		if err != nil || !os.SameFile(info, anchored) {
			t.Errorf("filesystem query followed replaced root: %v", err)
		}
		return query(file)
	}
	after, err := c.Snapshot()
	if err != nil || after.Device != before.Device || after.FreeBytes <= 0 {
		t.Fatalf("replacement detached anchor: %+v %v", after, err)
	}
	lease, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 1})
	if err != nil {
		t.Fatalf("retained root reservation: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestRootValidationRejectsSymlinkAndNonPrivateDirectory(t *testing.T) {
	root := privateRoot(t)
	link := root + "-link"
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "child"), root + "/../private", "", "/", root + "\x00", root + "-missing"} {
		if c, err := New(path, Options{}); !errors.Is(err, ErrUnsafeRoot) {
			if c != nil {
				c.Close()
			}
			t.Fatalf("unsafe root %q: %v", path, err)
		}
	}
	if err := os.Chmod(root, 0750); err != nil {
		t.Fatal(err)
	}
	if c, err := New(root, Options{}); !errors.Is(err, ErrUnsafeRoot) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("nonprivate root: %v", err)
	}
}

func TestAnchoredRootPermissionsRemainValidated(t *testing.T) {
	// Permission mutation after construction must deny subsequent allocations.
	root := privateRoot(t)
	c, err := New(root, Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := os.Chmod(root, 0750); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 1}); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatalf("mutated permissions accepted: %v", err)
	}
}

func TestDefaultHeadroomAndInvalidOptions(t *testing.T) {
	root := privateRoot(t)
	c, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.query = func(*os.File) (filesystemSpace, error) {
		return filesystemSpace{AvailableBlocks: 256 * 1024 * 1024, BlockSize: 1}, nil
	}
	if _, err := c.Reserve(context.Background(), Request{Class: Original, MaximumGrowthBytes: 1}); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("default safety headroom absent: %v", err)
	}
	if c, err := New(root, Options{HeadroomBytes: -1}); !errors.Is(err, ErrInvalidOptions) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("negative headroom: %v", err)
	}
}
