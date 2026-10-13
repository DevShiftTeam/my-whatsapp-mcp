package media

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"go.mau.fi/whatsmeow"
	"golang.org/x/sys/unix"
)

// Download owns exactly one capped inode, including ciphertext padding/MAC.
// Complete may only be called after the synchronous authenticated downloader
// has returned. Close may only be called after all writers have stopped.
type Download struct {
	mu                                    sync.Mutex
	store                                 *Store
	request                               DownloadRequest
	reservation                           archive.MediaReservation
	physical                              diskspace.Lease
	guard                                 *os.File
	file                                  *boundedFile
	maximum                               int64
	published, uncertain, stopped, closed bool
	cleanup                               archive.MediaArtifact
}

var _ ingestion.DownloadAdmission = (*Download)(nil)

// MediaAdmitter adapts the neutral authenticated-connector port. No client URL,
// path, media key, or arbitrary filesystem name enters this store.
func (s *Store) MediaAdmitter(ctx context.Context, q ingestion.MediaDownloadRequest) (ingestion.DownloadAdmission, error) {
	return s.AdmitDownload(ctx, DownloadRequest{OwnerDownloadLease: q.OwnerDownloadLease, Key: q.Key, Version: q.Version, MediaID: q.MediaID, Kind: q.Kind, DeclaredBytes: q.DeclaredBytes, CiphertextMaximum: q.CiphertextMaximum, ProtocolSHA256: q.ProtocolSHA256})
}

func (s *Store) AdmitDownload(ctx context.Context, q DownloadRequest) (*Download, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			s.end()
		}
	}()
	s.operation.RLock()
	defer s.operation.RUnlock()
	ref, plain, wire, err := s.bind(ctx, q)
	if err != nil {
		return nil, err
	}
	r, p, err := s.reserveOwner(ctx, ref, q.MediaID, archive.BudgetOriginals, wire, q.OwnerDownloadLease)
	if err != nil {
		return nil, err
	}
	guard, err := artifactLock(ctx, s.media, artifactName(r.ArtifactID, ".lock"), true, true)
	if err != nil {
		return nil, s.failedAdmission(ctx, r, p, err)
	}
	f, err := s.media.open(artifactName(r.ArtifactID, ".part"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		_ = releaseLock(guard)
		return nil, s.failedAdmission(ctx, r, p, err)
	}
	if err := s.media.sync(); err != nil {
		_ = f.Close()
		_ = s.media.remove(artifactName(r.ArtifactID, ".part"))
		_ = releaseLock(guard)
		return nil, s.failedAdmission(ctx, r, p, err)
	}
	s.transition(r.ArtifactID, "created")
	failed = false
	d := &Download{store: s, request: q, reservation: r, physical: p, guard: guard, file: &boundedFile{ctx: ctx, raw: f, maximum: wire}, maximum: plain}
	s.register(r.ArtifactID, d)
	return d, nil
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func (s *Store) failedAdmission(ctx context.Context, r archive.MediaReservation, p diskspace.Lease, cause error) error {
	cc, cancel := cleanupContext(ctx)
	defer cancel()
	err := s.cleanupArtifact(cc, archive.MediaArtifact{ID: r.ArtifactID, ReservationID: r.ID, CleanupLease: r.LeaseID, Budget: r.Budget}, "failed")
	if err != nil {
		s.mu.Lock()
		if s.pending == nil {
			s.pending = map[archive.ArtifactID]*pendingSpace{}
		}
		s.pending[r.ArtifactID] = &pendingSpace{lease: p}
		s.mu.Unlock()
		return errors.Join(cause, err)
	}
	if p != nil {
		err = p.Release()
		if err != nil {
			s.mu.Lock()
			if s.pending == nil {
				s.pending = map[archive.ArtifactID]*pendingSpace{}
			}
			s.pending[r.ArtifactID] = &pendingSpace{lease: p, cleaned: true}
			s.mu.Unlock()
		}
	}
	return errors.Join(cause, err)
}

func (d *Download) File() whatsmeow.File { return d.file }
func (d *Download) Renew(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.stopped || d.published {
		return ErrClosed
	}
	deadline, err := d.store.repo.RenewMedia(ctx, d.reservation, d.store.options.LeaseTTL)
	if err == nil {
		d.reservation.LeaseUntil = deadline
	}
	return err
}

func (d *Download) Complete(ctx context.Context, v ingestion.VerifiedMedia) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.stopped {
		return ErrClosed
	}
	if d.published {
		return nil
	}
	if d.uncertain {
		if err := d.resolve(ctx); err != nil {
			return err
		}
		if d.published {
			return nil
		}
		return ErrSourceChanged
	}
	if err := d.file.freeze(); err != nil {
		return err
	}
	if v.Key != d.request.Key || !sameVersion(v.Version, d.request.Version) || v.Kind != d.request.Kind || v.SHA256 != d.request.ProtocolSHA256 || v.Bytes <= 0 || v.Bytes > d.maximum || d.request.DeclaredBytes != 0 && v.Bytes != d.request.DeclaredBytes {
		return ErrAuthentication
	}
	digest, size, format, err := verifyMediaFile(ctx, d.file.raw, d.request.Kind, d.maximum)
	if err != nil {
		return err
	}
	if size != v.Bytes || digest != hex.EncodeToString(v.SHA256[:]) {
		return ErrAuthentication
	}
	ref, err := d.store.repo.CurrentSource(ctx, d.request.Key)
	if err != nil {
		return err
	}
	if !sameSource(ref, d.reservation.Source) {
		return ErrSourceChanged
	}
	if err := d.file.raw.Sync(); err != nil {
		return err
	}
	if err := d.store.media.same(artifactName(d.reservation.ArtifactID, ".part"), d.file.raw); err != nil {
		return err
	}
	d.store.transition(d.reservation.ArtifactID, "synced")
	if err := d.store.media.rename(artifactName(d.reservation.ArtifactID, ".part"), artifactName(d.reservation.ArtifactID, ".original")); err != nil {
		return err
	}
	if err := d.store.media.sync(); err != nil {
		return err
	}
	d.store.transition(d.reservation.ArtifactID, "renamed")
	// Mark uncertain before entering the durable publication operation. Even a
	// cancelled/error response can follow a successful commit.
	d.uncertain = true
	accepted, err := d.store.repo.PublishOriginal(ctx, archive.OriginalPublication{Reservation: d.reservation, SHA256: digest, Bytes: size, ActualFormat: format})
	if err == nil && accepted {
		d.published = true
		d.uncertain = false
		d.store.transition(d.reservation.ArtifactID, "published")
		return nil
	}
	resolved := d.resolve(ctx)
	if resolved != nil {
		return errors.Join(err, resolved)
	}
	if d.published {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrSourceChanged
}

func (d *Download) resolve(ctx context.Context) error {
	inspector, ok := d.store.repo.(artifactInspector)
	if !ok {
		return ErrPublicationUncertain
	}
	cc, cancel := cleanupContext(ctx)
	defer cancel()
	a, err := inspector.InspectMediaReservation(cc, d.reservation)
	if err != nil || a.ID != d.reservation.ArtifactID || a.ReservationID != d.reservation.ID {
		return errors.Join(ErrPublicationUncertain, err)
	}
	if a.State == "retained" {
		d.published = true
		d.uncertain = false
		return nil
	}
	if a.State != "reserved" && a.State != "staged" && a.State != "evicting" {
		return ErrPublicationUncertain
	}
	d.cleanup = a
	d.uncertain = false
	return nil
}

func (d *Download) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	var stopErr error
	if !d.stopped {
		stopErr = d.file.stop()
		d.stopped = true
	}
	if d.uncertain {
		if err := d.resolve(context.Background()); err != nil {
			return errors.Join(stopErr, err)
		}
	}
	if !d.published {
		a := d.cleanup
		if a.ID == "" {
			a = archive.MediaArtifact{ID: d.reservation.ArtifactID, ReservationID: d.reservation.ID, CleanupLease: d.reservation.LeaseID, Budget: d.reservation.Budget}
		}
		cc, cancel := cleanupContext(context.Background())
		err := d.store.cleanupArtifact(cc, a, "cancelled")
		cancel()
		if err != nil {
			return errors.Join(stopErr, err)
		}
	}
	if err := d.physical.Release(); err != nil {
		return errors.Join(stopErr, err)
	}
	d.closed = true
	err := releaseLock(d.guard)
	d.store.unregister(d.reservation.ArtifactID)
	d.store.end()
	return errors.Join(stopErr, err)
}
func (d *Download) recoverStopped() error {
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	if !stopped {
		return ErrActiveUse
	}
	return d.Close()
}

// Decoded is a charged temporary extent. Seal freezes its Go writer and fsyncs
// before native input descriptors may be opened. Hold it through full child
// process-group reap; TTL expiry never releases files or reservations.
type Decoded struct {
	mu                      sync.Mutex
	store                   *Store
	reservation             archive.MediaReservation
	physical                diskspace.Lease
	guard                   *os.File
	guardIdentity           os.FileInfo
	file                    *boundedFile
	children                []*os.File
	sealed, stopped, closed bool
	exported, cleanupGuard  bool
}

func (s *Store) ReserveDecoded(ctx context.Context, source archive.SourceRef, id archive.MediaID, maximum int64) (*Decoded, error) {
	if source.Account == "" || source.ContentHash == "" || source.Version.MessageID == "" || source.Version.Deleted || source.Version.Excluded || id == "" || maximum <= 0 || maximum > maximumMediaBytes {
		return nil, ErrInvalidRequest
	}
	if err := s.begin(); err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			s.end()
		}
	}()
	s.operation.RLock()
	defer s.operation.RUnlock()
	r, p, err := s.reserve(ctx, source, id, archive.BudgetDecoded, maximum)
	if err != nil {
		return nil, err
	}
	guard, err := artifactLock(ctx, s.media, artifactName(r.ArtifactID, ".lock"), true, true)
	if err != nil {
		return nil, s.failedAdmission(ctx, r, p, err)
	}
	f, err := s.working.open(artifactName(r.ArtifactID, ".decoded"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		_ = releaseLock(guard)
		return nil, s.failedAdmission(ctx, r, p, err)
	}
	if err := s.working.sync(); err != nil {
		_ = f.Close()
		_ = s.working.remove(artifactName(r.ArtifactID, ".decoded"))
		_ = releaseLock(guard)
		return nil, s.failedAdmission(ctx, r, p, err)
	}
	failed = false
	s.transition(r.ArtifactID, "decoded_created")
	d := &Decoded{store: s, reservation: r, physical: p, guard: guard, file: &boundedFile{ctx: ctx, raw: f, maximum: maximum}}
	s.register(r.ArtifactID, d)
	return d, nil
}
func (d *Decoded) File() whatsmeow.File { return d.file }
func (d *Decoded) Renew(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.stopped {
		return ErrClosed
	}
	deadline, err := d.store.repo.RenewMedia(ctx, d.reservation, d.store.options.LeaseTTL)
	if err == nil {
		d.reservation.LeaseUntil = deadline
	}
	return err
}
func (d *Decoded) Seal(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.stopped {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := d.store.repo.RenewMedia(ctx, d.reservation, d.store.options.LeaseTTL); err != nil {
		return err
	}
	if err := d.file.freeze(); err != nil {
		return err
	}
	if err := d.file.raw.Sync(); err != nil {
		return err
	}
	d.sealed = true
	return nil
}
func (d *Decoded) Open() (*os.File, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.sealed || d.closed || d.stopped {
		return nil, ErrClosed
	}
	f, err := d.store.working.open(artifactName(d.reservation.ArtifactID, ".decoded"), unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	if err := sameInode(f, d.file.raw); err != nil {
		_ = f.Close()
		return nil, err
	}
	d.children = append(d.children, f)
	return f, nil
}

// WorkerGuard duplicates the existing sealed artifact's exclusive flock OFD.
// The trusted holder retains it through the full native process-group lifetime.
// Recipients own the returned descriptor and must close it without LOCK_UN.
func (d *Decoded) WorkerGuard() (*os.File, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.sealed || d.closed || d.stopped {
		return nil, ErrClosed
	}
	if err := d.store.working.same(artifactName(d.reservation.ArtifactID, ".decoded"), d.file.raw); err != nil {
		return nil, err
	}
	name := artifactName(d.reservation.ArtifactID, ".lock")
	if err := d.store.media.same(name, d.guard); err != nil {
		return nil, err
	}
	identity, err := d.guard.Stat()
	if err != nil {
		return nil, err
	}
	fd, err := unix.FcntlInt(d.guard.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	guard := os.NewFile(uintptr(fd), "decoded-worker-guard")
	if err := d.store.media.same(name, guard); err != nil {
		_ = guard.Close()
		return nil, err
	}
	d.guardIdentity = identity
	d.exported = true
	d.children = append(d.children, guard)
	return guard, nil
}

func (d *Decoded) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	if liveChildren(d.children) {
		return ErrActiveUse
	}
	var stopErr error
	if !d.stopped {
		stopErr = d.file.stop()
		d.stopped = true
	}
	cc, cancel := cleanupContext(context.Background())
	defer cancel()
	if d.exported && !d.cleanupGuard {
		// A holder's inherited duplicate is not visible in our local child list.
		// Drop only our exported OFD copy, then require a new exclusive cleanup
		// acquisition of the same inode. The holder's existing EX lock keeps this
		// attempt busy until the complete native group and holder have exited.
		name := artifactName(d.reservation.ArtifactID, ".lock")
		if d.guard != nil {
			if err := d.store.media.same(name, d.guard); err != nil {
				return errors.Join(stopErr, err)
			}
			err := d.guard.Close()
			d.guard = nil
			if err != nil {
				return errors.Join(stopErr, err)
			}
		}
		guard, err := artifactLock(cc, d.store.media, name, true, false)
		if err != nil {
			return errors.Join(stopErr, err)
		}
		identity, err := guard.Stat()
		if err != nil || d.guardIdentity == nil || !os.SameFile(identity, d.guardIdentity) {
			_ = guard.Close()
			return errors.Join(stopErr, ErrUnsafeFile, err)
		}
		d.guard = guard
		d.cleanupGuard = true
	}
	err := d.store.cleanupArtifact(cc, archive.MediaArtifact{ID: d.reservation.ArtifactID, ReservationID: d.reservation.ID, CleanupLease: d.reservation.LeaseID, Budget: d.reservation.Budget}, "cleanup_complete")
	if err != nil {
		return errors.Join(stopErr, err)
	}
	if err := d.physical.Release(); err != nil {
		return errors.Join(stopErr, err)
	}
	d.closed = true
	if d.exported {
		err = d.guard.Close()
	} else {
		err = releaseLock(d.guard)
	}
	d.store.unregister(d.reservation.ArtifactID)
	d.store.end()
	return errors.Join(stopErr, err)
}
func (d *Decoded) recoverStopped() error {
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	if !stopped {
		return ErrActiveUse
	}
	return d.Close()
}
