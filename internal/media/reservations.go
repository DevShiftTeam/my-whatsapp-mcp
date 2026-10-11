package media

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"go.mau.fi/whatsmeow"
	"golang.org/x/sys/unix"
)

const metadataGrowth int64 = 64 << 10
const maximumMediaBytes int64 = 128 << 20

func sameVersion(a, b archive.SourceVersion) bool {
	return a.MessageID == b.MessageID && a.Revision == b.Revision && a.PolicyGeneration == b.PolicyGeneration && a.Deleted == b.Deleted && a.Excluded == b.Excluded && a.ExpiresAt.Equal(b.ExpiresAt)
}
func sameSource(a, b archive.SourceRef) bool {
	return a.Account == b.Account && a.ContentHash == b.ContentHash && sameVersion(a.Version, b.Version)
}
func wireMaximum(n int64) int64 { return (n/16+1)*16 + 10 }

func (s *Store) bind(ctx context.Context, q DownloadRequest) (archive.SourceRef, int64, int64, error) {
	if q.MediaID == "" || q.Key.Account == "" || q.Version.MessageID == "" || q.Version.Deleted || q.Version.Excluded || q.ProtocolSHA256 == ([32]byte{}) {
		return archive.SourceRef{}, 0, 0, ErrInvalidRequest
	}
	maximum := s.options.ImageMaximumBytes
	if q.Kind == archive.MessageVoice {
		maximum = s.options.VoiceMaximumBytes
	} else if q.Kind != archive.MessageImage {
		return archive.SourceRef{}, 0, 0, ErrFormat
	}
	if q.DeclaredBytes < 0 || q.DeclaredBytes > maximum {
		return archive.SourceRef{}, 0, 0, ErrLimit
	}
	plain := q.DeclaredBytes
	if plain == 0 {
		plain = maximum
	}
	wire := wireMaximum(plain)
	if q.CiphertextMaximum != 0 {
		if q.CiphertextMaximum < 26 || q.CiphertextMaximum > wireMaximum(maximumMediaBytes) || (q.CiphertextMaximum-10)%16 != 0 {
			return archive.SourceRef{}, 0, 0, ErrInvalidRequest
		}
		if q.DeclaredBytes != 0 && q.CiphertextMaximum != wire {
			return archive.SourceRef{}, 0, 0, ErrInvalidRequest
		}
		// A connector can supply a generic unknown-size wire ceiling. Apply the
		// smaller kind-specific ceiling and reserve only that actual extent.
		if q.DeclaredBytes == 0 && q.CiphertextMaximum < wire {
			wire = q.CiphertextMaximum
			if plain > wire-11 {
				plain = wire - 11
			}
		}
	}
	ref, err := s.repo.CurrentSource(ctx, q.Key)
	if err != nil {
		return ref, 0, 0, err
	}
	if ref.Account != q.Key.Account || ref.ContentHash == "" || !sameVersion(ref.Version, q.Version) || ref.Version.Deleted || ref.Version.Excluded {
		return ref, 0, 0, ErrSourceChanged
	}
	return ref, plain, wire, nil
}

func (s *Store) reserve(ctx context.Context, ref archive.SourceRef, id archive.MediaID, budget archive.BudgetKind, max int64) (archive.MediaReservation, diskspace.Lease, error) {
	r, err := s.repo.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: ref, MediaID: id, Budget: budget, MaximumBytes: max, TTL: s.options.LeaseTTL})
	if errors.Is(err, archive.ErrQuotaExceeded) && budget == archive.BudgetOriginals {
		ids, e := s.repo.EvictionCandidates(ctx, 256)
		if e != nil {
			return r, nil, e
		}
		for _, victim := range ids {
			if e := s.evict(ctx, victim); e != nil && !errors.Is(e, ErrActiveUse) && !errors.Is(e, archive.ErrNoWork) {
				return r, nil, e
			}
			r, err = s.repo.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: ref, MediaID: id, Budget: budget, MaximumBytes: max, TTL: s.options.LeaseTTL})
			if err == nil {
				break
			}
			if !errors.Is(err, archive.ErrQuotaExceeded) {
				return r, nil, err
			}
		}
		if err != nil {
			return r, nil, errors.Join(ErrWaitingForDiskBudget, err)
		}
	}
	if err != nil {
		if errors.Is(err, archive.ErrQuotaExceeded) {
			return r, nil, errors.Join(ErrWaitingForDiskBudget, err)
		}
		return r, nil, err
	}
	if r.ID == "" || r.ArtifactID == "" || r.LeaseID == "" || r.MediaID != id || r.Budget != budget || r.MaximumBytes != max || !sameSource(r.Source, ref) {
		return r, nil, ErrUnavailable
	}
	s.transition(r.ArtifactID, "reserved")
	class := diskspace.Original
	if budget == archive.BudgetDecoded {
		class = diskspace.Decoded
	}
	lease, err := s.options.Admission.Reserve(ctx, diskspace.Request{Class: class, MaximumGrowthBytes: max + metadataGrowth})
	if err != nil {
		if errors.Is(err, diskspace.ErrInsufficientSpace) {
			err = errors.Join(ErrWaitingForDiskBudget, err)
		}
		cc, cancel := cleanupContext(ctx)
		defer cancel()
		cleanup := s.repo.FinishMediaCleanup(cc, archive.MediaCleanup{ArtifactID: r.ArtifactID, ReservationID: r.ID, LeaseID: r.LeaseID, Code: "not_found"})
		return r, nil, errors.Join(err, cleanup)
	}
	if lease == nil {
		cc, cancel := cleanupContext(ctx)
		defer cancel()
		cleanup := s.repo.FinishMediaCleanup(cc, archive.MediaCleanup{ArtifactID: r.ArtifactID, ReservationID: r.ID, LeaseID: r.LeaseID, Code: "not_found"})
		return r, nil, errors.Join(ErrUnavailable, cleanup)
	}
	return r, lease, nil
}

func artifactLock(ctx context.Context, dir *directory, name string, exclusive, wait bool) (*os.File, error) {
	f, err := dir.open(name, unix.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		f, err = dir.open(name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
		if errors.Is(err, os.ErrExist) {
			f, err = dir.open(name, unix.O_RDWR)
		}
	}
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
		if !wait {
			_ = f.Close()
			return nil, ErrActiveUse
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
func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return errors.Join(unix.Flock(int(f.Fd()), unix.LOCK_UN), f.Close())
}

type boundedFile struct {
	mu             sync.Mutex
	ctx            context.Context
	raw            *os.File
	maximum        int64
	sealed, closed bool
}

var _ whatsmeow.File = (*boundedFile)(nil)

func (f *boundedFile) check(write bool) error {
	if f.closed || write && f.sealed {
		return os.ErrClosed
	}
	return f.ctx.Err()
}
func (f *boundedFile) extent(offset int64, n int) error {
	if offset < 0 || offset > f.maximum || int64(n) > f.maximum-offset {
		return ErrLimit
	}
	return nil
}
func (f *boundedFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(false); err != nil {
		return 0, err
	}
	return f.raw.Read(p)
}
func (f *boundedFile) ReadAt(p []byte, n int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(false); err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, ErrInvalidRequest
	}
	return f.raw.ReadAt(p, n)
}
func (f *boundedFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(true); err != nil {
		return 0, err
	}
	offset, err := f.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if err := f.extent(offset, len(p)); err != nil {
		return 0, err
	}
	return f.raw.Write(p)
}
func (f *boundedFile) WriteAt(p []byte, n int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(true); err != nil {
		return 0, err
	}
	if err := f.extent(n, len(p)); err != nil {
		return 0, err
	}
	return f.raw.WriteAt(p, n)
}
func (f *boundedFile) Seek(n int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(false); err != nil {
		return 0, err
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		v, e := f.raw.Seek(0, io.SeekCurrent)
		if e != nil {
			return 0, e
		}
		base = v
	case io.SeekEnd:
		st, e := f.raw.Stat()
		if e != nil {
			return 0, e
		}
		base = st.Size()
	default:
		return 0, ErrInvalidRequest
	}
	if n < -base || n > f.maximum-base {
		return 0, ErrLimit
	}
	return f.raw.Seek(base+n, io.SeekStart)
}
func (f *boundedFile) Truncate(n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(true); err != nil {
		return err
	}
	if err := f.extent(n, 0); err != nil {
		return err
	}
	return f.raw.Truncate(n)
}
func (f *boundedFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(false); err != nil {
		return nil, err
	}
	st, err := f.raw.Stat()
	if err == nil && st.Size() > f.maximum {
		return nil, ErrLimit
	}
	return st, err
}
func (f *boundedFile) freeze() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(false); err != nil {
		return err
	}
	f.sealed = true
	return nil
}
func (f *boundedFile) stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return errors.Join(f.raw.Sync(), f.raw.Close())
}
