// Package diskspace coordinates outstanding physical growth on one filesystem.
// Durable media quotas and model ownership remain the responsibility of callers.
package diskspace

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Class string

const (
	Original     Class = "original"
	Decoded      Class = "decoded"
	ModelInstall Class = "model_install"
)

type Request struct {
	Class              Class
	MaximumGrowthBytes int64
}

// Lease remains charged until the caller confirms all writers have stopped and
// completion or cleanup has finished. Context cancellation never releases it.
type Lease interface {
	Release() error
}

type Admission interface {
	Reserve(context.Context, Request) (Lease, error)
}

var (
	ErrInvalidRequest      = errors.New("invalid_diskspace_request")
	ErrInvalidOptions      = errors.New("invalid_diskspace_options")
	ErrInsufficientSpace   = errors.New("insufficient_disk_space")
	ErrUnsafeRoot          = errors.New("unsafe_diskspace_root")
	ErrUnavailable         = errors.New("diskspace_unavailable")
	ErrUnsupportedPlatform = errors.New("unsupported_diskspace_platform")
	ErrClosed              = errors.New("diskspace_closed")
	ErrLeasesActive        = errors.New("diskspace_leases_active")
)

const DefaultHeadroomBytes int64 = 256 << 20

type Options struct {
	// Zero selects DefaultHeadroomBytes. Negative values are rejected.
	HeadroomBytes int64
}

// Snapshot contains aggregate accounting only; paths and artifact identities
// are never retained or exposed. AvailableBytes is clamped to zero on pressure.
type Snapshot struct {
	FreeBytes, ReservedBytes, HeadroomBytes, AvailableBytes int64
	OriginalBytes, DecodedBytes, ModelInstallBytes          int64
	ActiveLeases                                            int
	Device                                                  uint64
}

type filesystemSpace struct {
	AvailableBlocks uint64
	BlockSize       uint64
}

// Controller owns an existing, verified private directory descriptor. One
// mutex covers fresh free-space measurement and every outstanding reservation.
// App composition creates one controller for all writers on this filesystem;
// startup reconciliation must finish before any new write is admitted.
type Controller struct {
	*controllerState
}

// Mutable ownership is shared even if an exported Controller is copied by
// value. A copy cannot invent another capacity pool or close another's fd.
type controllerState struct {
	mu       sync.Mutex
	root     *os.File
	device   uint64
	headroom int64
	reserved int64
	byClass  [3]int64
	active   int
	stopping bool
	closed   bool
	closeErr error
	query    func(*os.File) (filesystemSpace, error)
}

var _ Admission = (*Controller)(nil)

// New anchors to an existing owner-only directory without creating, repairing,
// or deleting files. Every path component must be a directory, never a symlink.
// The app's instance lock remains the cross-process writer authority.
func New(root string, options Options) (*Controller, error) {
	if options.HeadroomBytes < 0 {
		return nil, ErrInvalidOptions
	}
	headroom := options.HeadroomBytes
	if headroom == 0 {
		headroom = DefaultHeadroomBytes
	}
	file, err := openVerifiedRoot(root)
	if err != nil {
		return nil, err
	}
	device, err := verifyRoot(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	c := &Controller{controllerState: &controllerState{root: file, device: device, headroom: headroom, query: queryFilesystem}}
	// Verify the physical query and its arithmetic before handing out authority.
	if _, err := c.snapshotLocked(); err != nil {
		file.Close()
		return nil, err
	}
	return c, nil
}

func classIndex(class Class) (int, bool) {
	switch class {
	case Original:
		return 0, true
	case Decoded:
		return 1, true
	case ModelInstall:
		return 2, true
	default:
		return 0, false
	}
}

// Reserve measures available filesystem bytes while holding the same lock as
// outstanding peak-growth accounting. Already-written bytes may be counted
// conservatively again until the bounded operation completes and releases.
func (c *Controller) Reserve(ctx context.Context, request Request) (Lease, error) {
	index, valid := classIndex(request.Class)
	if ctx == nil || !valid || request.MaximumGrowthBytes <= 0 {
		return nil, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || c.controllerState == nil {
		return nil, ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.stopping || c.closed {
		return nil, ErrClosed
	}
	snapshot, err := c.snapshotLocked()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.MaximumGrowthBytes > snapshot.AvailableBytes {
		return nil, ErrInsufficientSpace
	}
	// Checked additions make the bounds independent of future accounting changes.
	reserved, ok := addBytes(c.reserved, request.MaximumGrowthBytes)
	classBytes, classOK := addBytes(c.byClass[index], request.MaximumGrowthBytes)
	if !ok || !classOK || c.active == math.MaxInt {
		return nil, ErrInsufficientSpace
	}
	c.reserved = reserved
	c.byClass[index] = classBytes
	c.active++
	return &reservation{state: c.controllerState, index: index, bytes: request.MaximumGrowthBytes}, nil
}

// Snapshot obtains a fresh anchored measurement. A draining controller remains
// observable until its last lease is acknowledged and Close succeeds.
func (c *Controller) Snapshot() (Snapshot, error) {
	if c == nil || c.controllerState == nil {
		return Snapshot{}, ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Controller) snapshotLocked() (Snapshot, error) {
	if c.closed || c.root == nil {
		return Snapshot{}, ErrClosed
	}
	device, err := verifyRoot(c.root)
	if err != nil {
		return Snapshot{}, err
	}
	if device != c.device {
		return Snapshot{}, ErrUnsafeRoot
	}
	space, err := c.query(c.root)
	if err != nil || space.BlockSize == 0 || space.BlockSize > math.MaxInt64 || space.AvailableBlocks > math.MaxInt64/space.BlockSize {
		return Snapshot{}, ErrUnavailable
	}
	free := int64(space.AvailableBlocks * space.BlockSize)
	charged, ok := addBytes(c.reserved, c.headroom)
	if !ok {
		return Snapshot{}, ErrUnavailable
	}
	var available int64
	if free > charged {
		available = free - charged
	}
	return Snapshot{
		FreeBytes: free, ReservedBytes: c.reserved, HeadroomBytes: c.headroom,
		AvailableBytes: available, ActiveLeases: c.active, Device: device,
		OriginalBytes: c.byClass[0], DecodedBytes: c.byClass[1], ModelInstallBytes: c.byClass[2],
	}, nil
}

func addBytes(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

// Close stops admissions immediately. Outstanding leases retain accounting and
// the filesystem descriptor; callers release them after cleanup, then retry
// Close. It never interprets shutdown or elapsed time as cleanup confirmation.
func (c *Controller) Close() error {
	if c == nil || c.controllerState == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.closeErr
	}
	c.stopping = true
	if c.active != 0 {
		return ErrLeasesActive
	}
	c.closed = true
	if err := c.root.Close(); err != nil {
		c.closeErr = ErrUnavailable
	}
	c.root = nil
	return c.closeErr
}

type reservation struct {
	state    *controllerState // immutable admitted ownership, not a mutable wrapper
	index    int
	bytes    int64
	released bool // guarded by state.mu, including concurrent Release calls
}

// Release is an idempotent caller acknowledgement, not an automatic lifetime.
func (lease *reservation) Release() error {
	c := lease.state
	c.mu.Lock()
	defer c.mu.Unlock()
	if lease.released {
		return nil
	}
	c.reserved -= lease.bytes
	c.byClass[lease.index] -= lease.bytes
	c.active--
	lease.released = true
	return nil
}

func canonicalRoot(root string) (string, error) {
	if root == "" || strings.IndexByte(root, 0) >= 0 {
		return "", ErrUnsafeRoot
	}
	for _, component := range strings.Split(root, string(filepath.Separator)) {
		if component == ".." {
			return "", ErrUnsafeRoot
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil || absolute == string(filepath.Separator) {
		return "", ErrUnsafeRoot
	}
	return absolute, nil
}

// Filesystem block counts use allocation fragments when supplied (Linux),
// otherwise the block size (Darwin and older Linux kernels).
func spaceFromUnits(availableBlocks uint64, blockSize, fragmentSize int64) (filesystemSpace, error) {
	if blockSize <= 0 || fragmentSize < 0 {
		return filesystemSpace{}, ErrUnavailable
	}
	unit := blockSize
	if fragmentSize != 0 {
		unit = fragmentSize
	}
	return filesystemSpace{AvailableBlocks: availableBlocks, BlockSize: uint64(unit)}, nil
}
