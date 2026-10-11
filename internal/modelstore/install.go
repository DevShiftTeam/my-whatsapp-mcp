package modelstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Options struct {
	Client *http.Client
	// ExpectedDevice binds every retained directory to the shared app broker.
	// Zero permits standalone use; a nonzero device requires a prepared root.
	ExpectedDevice uint64
	// FreeBytes is an optional destination-filesystem probe (also a test seam).
	FreeBytes       func() (int64, error)
	HeadroomBytes   int64
	MaxDownloadTime time.Duration
	// ReserveSpace connects the app's shared physical-space admission. Growth
	// excludes already allocated files and the separately tested headroom.
	ReserveSpace func(context.Context, int64) (SpaceReservation, error)
}

type SpaceReservation interface{ Release() error }

type Store struct {
	mu                              sync.RWMutex
	root, versions, staging, leases *directory
	catalog                         Catalog
	profiles                        map[string]Profile
	options                         Options
	client                          *http.Client
	closed                          bool
}

func Open(path string, c Catalog, opts Options) (*Store, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	// Freeze caller-owned slices so clients cannot alter pinned identities/URLs.
	b, _ := json.Marshal(c)
	var frozen Catalog
	_ = json.Unmarshal(b, &frozen)
	root, err := openDirectory(path, opts.ExpectedDevice == 0, true)
	if err != nil {
		return nil, err
	}
	if err := checkFilesystem(root.file, opts.ExpectedDevice); err != nil {
		_ = root.close()
		return nil, err
	}
	root.expectedDevice = opts.ExpectedDevice
	s := &Store{root: root, catalog: frozen, profiles: make(map[string]Profile), options: opts}
	for _, p := range frozen.Profiles {
		s.profiles[p.ID] = p
	}
	if opts.HeadroomBytes < 0 || opts.HeadroomBytes > 64<<30 || opts.MaxDownloadTime < 0 || opts.MaxDownloadTime > 24*time.Hour {
		_ = root.close()
		return nil, ErrInvalidCatalog
	}
	if s.options.HeadroomBytes == 0 {
		s.options.HeadroomBytes = 1 << 30
	}
	if s.options.MaxDownloadTime == 0 {
		s.options.MaxDownloadTime = 30 * time.Minute
	}
	entries := []struct {
		name   string
		target **directory
	}{{"versions", &s.versions}, {"staging", &s.staging}, {"leases", &s.leases}}
	var missing []int
	for i, entry := range entries {
		d, err := root.child(entry.name, opts.ExpectedDevice == 0)
		if opts.ExpectedDevice != 0 && errors.Is(err, os.ErrNotExist) {
			missing = append(missing, i)
			continue
		}
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		if err := checkFilesystem(d.file, opts.ExpectedDevice); err != nil {
			_ = d.close()
			_ = s.Close()
			return nil, err
		}
		*entry.target = d
	}
	// Validate all existing child mounts before creating missing children.
	for _, i := range missing {
		entry := entries[i]
		d, err := root.child(entry.name, true)
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		if err := checkFilesystem(d.file, opts.ExpectedDevice); err != nil {
			_ = d.close()
			_ = s.Close()
			return nil, err
		}
		*entry.target = d
	}
	// A successful constructor acknowledges the child-directory entries created
	// above. App composition holds its bootstrap growth reservation until this
	// child-first/root-last durability barrier completes.
	for _, d := range []*directory{s.versions, s.staging, s.leases, s.root} {
		if err := d.file.Sync(); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	base := opts.Client
	if base == nil {
		base = &http.Client{Timeout: s.options.MaxDownloadTime}
	}
	copyClient := *base
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return ErrURLPolicy
		}
		return frozen.checkURL(req.URL.String())
	}
	s.client = &copyClient
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var all []error
	for _, d := range []*directory{s.leases, s.staging, s.versions, s.root} {
		if d != nil {
			all = append(all, d.close())
		}
	}
	return errors.Join(all...)
}

func (s *Store) profile(id string) (Profile, error) {
	p, ok := s.profiles[id]
	if !ok {
		return p, ErrUnknownProfile
	}
	if !p.compatible() {
		return p, ErrPlatform
	}
	return p, nil
}

func lock(ctx context.Context, d *directory, name string, exclusive, nonblock bool) (*os.File, error) {
	f, err := d.open(name, unix.O_RDWR|unix.O_CREAT, 0600, true)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = f.Close()
			return nil, err
		}
		if nonblock {
			_ = f.Close()
			return nil, ErrActiveLease
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func unlock(f *os.File) error {
	if f == nil {
		return nil
	}
	return errors.Join(unix.Flock(int(f.Fd()), unix.LOCK_UN), f.Close())
}

func hashFile(ctx context.Context, f *os.File, a Component) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != a.Bytes {
		return ErrSizeMismatch
	}
	h := sha256.New()
	buf := make([]byte, 128<<10)
	var off int64
	for off < a.Bytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int64(len(buf))
		if a.Bytes-off < n {
			n = a.Bytes - off
		}
		read, err := f.ReadAt(buf[:n], off)
		if err != nil {
			return err
		}
		_, _ = h.Write(buf[:read])
		off += int64(read)
	}
	if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return ErrHashMismatch
	}
	return nil
}

func (s *Store) installed(ctx context.Context, p Profile) (InstalledProfile, *directory, map[string]*os.File, error) {
	var installed InstalledProfile
	b, err := s.root.read(p.ID+".active", 64)
	if errors.Is(err, os.ErrNotExist) {
		return installed, nil, nil, ErrNotInstalled
	}
	if err != nil {
		return installed, nil, nil, err
	}
	if string(b) != p.identity() {
		return installed, nil, nil, ErrNotInstalled
	}
	d, err := s.versions.child(p.identity(), false)
	if errors.Is(err, os.ErrNotExist) {
		return installed, nil, nil, ErrNotInstalled
	}
	if err != nil {
		return installed, nil, nil, err
	}
	files := make(map[string]*os.File)
	fail := func(e error) (InstalledProfile, *directory, map[string]*os.File, error) {
		for _, f := range files {
			_ = f.Close()
		}
		_ = d.close()
		return InstalledProfile{}, nil, nil, e
	}
	installed = InstalledProfile{ID: p.ID, Identity: p.identity(), Revision: p.Revision, Platform: p.Platform, Selection: p.Selection, GateStatus: p.GateStatus, RuntimeRequirements: append([]string(nil), p.RuntimeRequirements...)}
	for _, a := range p.Components {
		f, err := d.open(a.Filename, unix.O_RDONLY, 0, true)
		if err != nil {
			return fail(err)
		}
		files[a.ID] = f
		if err := checkFD(f, false, true, true); err != nil {
			return fail(err)
		}
		if err := hashFile(ctx, f, a); err != nil {
			return fail(err)
		}
		installed.Components = append(installed.Components, InstalledComponent{ID: a.ID, Role: a.Role, SHA256: a.SHA256, Bytes: a.Bytes, Path: filepath.Join(d.path, a.Filename)})
	}
	return installed, d, files, nil
}

func (s *Store) Verify(ctx context.Context, id string) (InstalledProfile, error) {
	lease, err := s.Acquire(ctx, id)
	if err != nil {
		return InstalledProfile{}, err
	}
	p := lease.Profile()
	return p, lease.Release()
}

type Lease struct {
	mu      sync.Mutex
	profile InstalledProfile
	lock    *os.File
	dir     *directory
	anchor  *directory
	files   map[string]*os.File
	closed  bool
}

func (s *Store) Acquire(ctx context.Context, id string) (*Lease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	p, err := s.profile(id)
	if err != nil {
		return nil, err
	}
	op, err := lock(ctx, s.root, "operation.lock", false, false)
	if err != nil {
		return nil, err
	}
	defer unlock(op)
	guard, err := lock(ctx, s.leases, id+".lease", false, false)
	if err != nil {
		return nil, err
	}
	profile, dir, files, err := s.installed(ctx, p)
	if err != nil {
		_ = unlock(guard)
		return nil, err
	}
	// Keep the lock namespace anchored independently of Store.Close. This is a
	// directory descriptor duplicate, not a new file open or lock acquisition.
	fd, err := unix.FcntlInt(s.leases.file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		for _, f := range files {
			_ = f.Close()
		}
		_ = dir.close()
		_ = unlock(guard)
		return nil, err
	}
	anchor := &directory{file: os.NewFile(uintptr(fd), "lease-anchor"), path: s.leases.path, expectedDevice: s.leases.expectedDevice}
	return &Lease{profile: profile, lock: guard, dir: dir, anchor: anchor, files: files}, nil
}

func (l *Lease) Profile() InstalledProfile {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.profile
	p.Components = append([]InstalledComponent(nil), p.Components...)
	p.RuntimeRequirements = append([]string(nil), p.RuntimeRequirements...)
	return p
}

// Open duplicates a verified descriptor for inheritance by a worker. Duplicates
// share an offset; consumers should not perform concurrent streaming reads.
// The lifetime lease must remain held even after transferring the descriptor.
func (l *Lease) Open(id string) (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrLeaseClosed
	}
	f, ok := l.files[id]
	if !ok {
		return nil, ErrUnknownProfile
	}
	if err := checkFD(f, false, true, true); err != nil {
		return nil, err
	}
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dup := os.NewFile(uintptr(fd), id)
	if _, err := dup.Seek(0, io.SeekStart); err != nil {
		_ = dup.Close()
		return nil, err
	}
	return dup, nil
}

// WorkerGuard duplicates the same shared flock open-file description. A
// trusted independent holder keeps this guard through the worker's entire
// process-group lifetime, including after the application is killed. The
// recipient owns the returned descriptor and must close it without LOCK_UN.
func (l *Lease) WorkerGuard() (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrLeaseClosed
	}
	name := l.profile.ID + ".lease"
	if err := l.anchor.same(name, l.lock); err != nil {
		return nil, err
	}
	fd, err := unix.FcntlInt(l.lock.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	guard := os.NewFile(uintptr(fd), "model-worker-guard")
	if err := l.anchor.same(name, guard); err != nil {
		_ = guard.Close()
		return nil, err
	}
	return guard, nil
}

func (l *Lease) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	var all []error
	for _, f := range l.files {
		all = append(all, f.Close())
	}
	// LOCK_UN would release the lock for every exported duplicate of this OFD.
	// Closing alone releases it only after the final holder descriptor closes.
	all = append(all, l.dir.close(), l.anchor.close(), l.lock.Close())
	return errors.Join(all...)
}
func (l *Lease) Close() error { return l.Release() }

func (s *Store) Space(ctx context.Context, id string) (SpacePlan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return SpacePlan{}, ErrClosed
	}
	p, err := s.profile(id)
	if err != nil {
		return SpacePlan{}, err
	}
	op, err := lock(ctx, s.root, "operation.lock", false, false)
	if err != nil {
		return SpacePlan{}, err
	}
	defer unlock(op)
	return s.space(ctx, p)
}

func (s *Store) space(ctx context.Context, p Profile) (SpacePlan, error) {
	free := s.options.FreeBytes
	if free == nil {
		free = s.root.freeBytes
	}
	available, err := free()
	if err != nil {
		return SpacePlan{}, err
	}
	// Renaming staging into versions consumes no second copy. Old versions are
	// already included in actual filesystem free space and reported separately.
	plan := SpacePlan{DownloadBytes: p.totalBytes(), InstalledBytes: p.totalBytes(), TemporaryBytes: 1 << 20, FreeBytes: available, HeadroomBytes: s.options.HeadroomBytes}
	for _, parent := range []*directory{s.versions, s.staging} {
		n, physical, e := allocatedBytes(parent)
		if e != nil {
			return SpacePlan{}, e
		}
		plan.ExistingBytes += n
		plan.ExistingAllocatedBytes += physical
	}
	var errReuse error
	var growthCredit int64
	plan.ReusableBytes, growthCredit, errReuse = s.reusable(ctx, p)
	if errReuse != nil {
		return SpacePlan{}, errReuse
	}
	plan.DownloadBytes -= plan.ReusableBytes
	plan.GrowthBytes = p.totalBytes() - growthCredit
	plan.RequiredBytes = plan.GrowthBytes + plan.TemporaryBytes + plan.HeadroomBytes
	if plan.RequiredBytes > available {
		plan.AdditionalBytes = plan.RequiredBytes - available
	}
	return plan, nil
}

func verifyVersion(ctx context.Context, p Profile, d *directory) error {
	for _, a := range p.Components {
		f, err := d.open(a.Filename, unix.O_RDONLY, 0, true)
		if err != nil {
			return err
		}
		err = checkFD(f, false, true, true)
		if err == nil {
			err = hashFile(ctx, f, a)
		}
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reusable(ctx context.Context, p Profile) (int64, int64, error) {
	if d, err := s.versions.child(p.identity(), false); err == nil {
		err = verifyVersion(ctx, p, d)
		_ = d.close()
		if err == nil {
			return p.totalBytes(), p.totalBytes(), nil
		}
		if errors.Is(err, ErrUnsafeFile) || ctx.Err() != nil {
			return 0, 0, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, 0, err
	}
	d, err := s.staging.child(p.identity(), false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer d.close()
	var reusable, growthCredit int64
	for _, a := range p.Components {
		if f, err := d.open(a.Filename, unix.O_RDONLY, 0, true); err == nil {
			err = checkFD(f, false, true, true)
			if err == nil {
				err = hashFile(ctx, f, a)
			}
			_ = f.Close()
			if errors.Is(err, ErrUnsafeFile) {
				return 0, 0, err
			}
			if err == nil {
				reusable += a.Bytes
				growthCredit += a.Bytes
				continue
			}
			if ctx.Err() != nil {
				return 0, 0, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, 0, err
		}
		b, err := d.read(a.ID+".meta", 2048)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		var state partialState
		if json.Unmarshal(b, &state) != nil || state.URL != a.URL || state.SHA256 != a.SHA256 || state.Bytes != a.Bytes || !strongETag(state.ETag) {
			continue
		}
		f, err := d.open(a.ID+".part", unix.O_RDONLY, 0, true)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		st, err := f.Stat()
		physical, allocationErr := fileAllocatedBytes(f)
		_ = f.Close()
		if err != nil {
			return 0, 0, err
		}
		if allocationErr != nil {
			return 0, 0, allocationErr
		}
		if st.Size() >= 0 && st.Size() <= a.Bytes {
			reusable += st.Size()
			if physical > st.Size() {
				physical = st.Size()
			}
			growthCredit += physical
		}
	}
	return reusable, growthCredit, nil
}

func (s *Store) activate(ctx context.Context, p Profile, plan SpacePlan, progress func(Progress)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	emit := func(phase string) {
		if progress != nil {
			progress(Progress{ProfileID: p.ID, Phase: phase, CompletedBytes: p.totalBytes(), DownloadBytes: plan.DownloadBytes, InstalledBytes: plan.InstalledBytes, TemporaryBytes: plan.TemporaryBytes})
		}
	}
	emit("activating")
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.root.atomicWrite(p.ID+".active", []byte(p.identity())); err != nil {
		return err
	}
	emit("installed")
	return nil
}

func (s *Store) Install(ctx context.Context, id string, progress func(Progress)) error {
	return s.install(ctx, id, progress, nil)
}

func (s *Store) install(ctx context.Context, id string, progress func(Progress), source *directory) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.MaxDownloadTime)
	defer cancel()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	p, err := s.profile(id)
	if err != nil {
		return err
	}
	op, err := lock(ctx, s.root, "operation.lock", true, false)
	if err != nil {
		return err
	}
	defer unlock(op)
	if _, d, files, err := s.installed(ctx, p); err == nil {
		for _, f := range files {
			_ = f.Close()
		}
		_ = d.close()
		return nil
	}
	plan, err := s.space(ctx, p)
	if err != nil {
		return err
	}
	if plan.AdditionalBytes > 0 {
		return &SpaceError{plan, plan.AdditionalBytes}
	}
	heldGrowth := plan.RequiredBytes - plan.HeadroomBytes
	var reservations []SpaceReservation
	defer func() {
		for _, r := range reservations {
			resultErr = errors.Join(resultErr, r.Release())
		}
	}()
	if s.options.ReserveSpace != nil {
		reservation, e := s.options.ReserveSpace(ctx, plan.RequiredBytes-plan.HeadroomBytes)
		if e != nil {
			return e
		}
		if reservation == nil {
			return ErrUnsafeFile
		}
		reservations = append(reservations, reservation)
	}
	readmit := func() error {
		next, e := s.space(ctx, p)
		if e != nil {
			return e
		}
		if next.AdditionalBytes > 0 {
			return &SpaceError{next, next.AdditionalBytes}
		}
		growth := next.RequiredBytes - next.HeadroomBytes
		if s.options.ReserveSpace != nil && growth > heldGrowth {
			r, e := s.options.ReserveSpace(ctx, growth-heldGrowth)
			if e != nil {
				return e
			}
			if r == nil {
				return ErrUnsafeFile
			}
			reservations = append(reservations, r)
			heldGrowth = growth
		}
		return nil
	}
	if version, e := s.versions.child(p.identity(), false); e == nil {
		e = verifyVersion(ctx, p, version)
		_ = version.close()
		if e == nil {
			return s.activate(ctx, p, plan, progress)
		}
	}
	stage, err := s.staging.child(p.identity(), true)
	if err != nil {
		return err
	}
	defer stage.close()
	marker, _ := json.Marshal(p)
	if err := stage.atomicWrite("profile.json", marker); err != nil {
		return err
	}
	markerFile, err := stage.open("profile.json", unix.O_RDONLY, 0, true)
	if err != nil {
		return err
	}
	err = unix.Fchmod(int(markerFile.Fd()), 0400)
	if err == nil {
		err = markerFile.Sync()
	}
	_ = markerFile.Close()
	if err != nil {
		return err
	}
	var completed int64
	emit := func(a Component, phase string, n int64) {
		if progress != nil {
			progress(Progress{ProfileID: p.ID, ComponentID: a.ID, Phase: phase, CompletedBytes: completed + n, DownloadBytes: plan.DownloadBytes, InstalledBytes: plan.InstalledBytes, TemporaryBytes: plan.TemporaryBytes})
		}
	}
	for i, a := range p.Components {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 {
			remaining, e := s.space(ctx, p)
			if e != nil {
				return e
			}
			if remaining.AdditionalBytes > 0 {
				return &SpaceError{remaining, remaining.AdditionalBytes}
			}
		}
		if source != nil {
			err = s.copyOffline(ctx, source, stage, a, readmit)
		} else {
			err = s.download(ctx, stage, a, func(n int64) { emit(a, "downloading", n) }, readmit)
		}
		if err != nil {
			return err
		}
		completed += a.Bytes
		emit(a, "verified", 0)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A version is immutable and complete before it can be made active. Reuse a
	// crash-left complete version only after independently rechecking every byte.
	version, err := s.versions.child(p.identity(), false)
	if err == nil {
		var invalid error
		for _, a := range p.Components {
			f, e := version.open(a.Filename, unix.O_RDONLY, 0, true)
			if e == nil {
				e = checkFD(f, false, true, true)
				if e == nil {
					e = hashFile(ctx, f, a)
				}
				_ = f.Close()
			}
			if e != nil {
				invalid = e
				break
			}
		}
		_ = version.close()
		if invalid != nil {
			if errors.Is(invalid, ErrUnsafeFile) || ctx.Err() != nil {
				return invalid
			}
			guard, e := lock(ctx, s.leases, id+".lease", true, true)
			if e != nil {
				return e
			}
			defer unlock(guard)
			// Only a known corrupt app-owned version is replaceable, and never
			// while a worker holds it. The old good predecessor is another identity.
			if e := removeDirectory(s.versions, p.identity()); e != nil {
				return e
			}
			err = os.ErrNotExist
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := stage.file.Sync(); err != nil {
			return err
		}
		if err := unix.Renameat(int(s.staging.file.Fd()), p.identity(), int(s.versions.file.Fd()), p.identity()); err != nil {
			return err
		}
		if err := s.versions.file.Sync(); err != nil {
			return err
		}
		if err := s.staging.file.Sync(); err != nil {
			return err
		}
	}
	return s.activate(ctx, p, plan, progress)
}

func (s *Store) Remove(ctx context.Context, id string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	p, err := s.profile(id)
	if err != nil {
		return err
	}
	op, err := lock(ctx, s.root, "operation.lock", true, false)
	if err != nil {
		return err
	}
	defer unlock(op)
	guard, err := lock(ctx, s.leases, id+".lease", true, true)
	if err != nil {
		return err
	}
	defer unlock(guard)
	// Unpublish first. A crash can leave inert files, never a missing active file.
	if err := s.root.unlink(p.ID + ".active"); err != nil {
		return err
	}
	if err := s.root.file.Sync(); err != nil {
		return err
	}
	for _, parent := range []*directory{s.versions, s.staging} {
		names, err := parent.names()
		if err != nil {
			return err
		}
		for _, name := range names {
			if !shaPattern.MatchString(name) {
				return ErrUnsafeFile
			}
			d, err := parent.child(name, false)
			if err != nil {
				return err
			}
			b, err := d.read("profile.json", 1<<20)
			_ = d.close()
			if errors.Is(err, os.ErrNotExist) {
				if name == p.identity() {
					if err := removeDirectory(parent, name); err != nil {
						return err
					}
				}
				continue
			}
			if err != nil {
				return err
			}
			var prior Profile
			if json.Unmarshal(b, &prior) != nil || prior.identity() != name {
				return ErrUnsafeFile
			}
			if prior.ID == p.ID {
				if err := removeDirectory(parent, name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func strongETag(s string) bool {
	return len(s) >= 2 && len(s) <= 512 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && !strings.ContainsAny(s, "\r\n")
}
