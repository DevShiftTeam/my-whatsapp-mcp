package media

import (
	"context"
	"errors"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// cleanupArtifact runs only with stopped ownership and an exclusive artifact
// guard. Directory durability precedes the durable quota acknowledgement.
func (s *Store) cleanupArtifact(ctx context.Context, a archive.MediaArtifact, code string) error {
	if a.ID == "" || a.ReservationID == "" || a.CleanupLease == "" || a.State == "retained" {
		return ErrInvalidRequest
	}
	for _, suffix := range []string{".part", ".original"} {
		if err := s.media.remove(artifactName(a.ID, suffix)); err != nil {
			return err
		}
	}
	if err := s.working.remove(artifactName(a.ID, ".decoded")); err != nil {
		return err
	}
	if err := s.media.sync(); err != nil {
		return err
	}
	if err := s.working.sync(); err != nil {
		return err
	}
	s.transition(a.ID, "removed")
	if err := s.repo.FinishMediaCleanup(ctx, archive.MediaCleanup{ArtifactID: a.ID, ReservationID: a.ReservationID, LeaseID: a.CleanupLease, Code: code}); err != nil {
		return err
	}
	s.transition(a.ID, "cleanup_acknowledged")
	return nil
}

func (s *Store) Evict(ctx context.Context, id archive.MediaID) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.end()
	return s.evict(ctx, id)
}
func (s *Store) evict(ctx context.Context, id archive.MediaID) error {
	// Exclude eviction while any original reader is alive, before SQL can change
	// retained state. The cleanup port exposes no unauthorised media-ID lookup.
	readers, err := artifactLock(ctx, s.media, "media-readers.lock", true, false)
	if err != nil {
		return err
	}
	defer releaseLock(readers)
	a, err := s.repo.BeginEviction(ctx, id)
	if err != nil {
		return err
	}
	guard, err := artifactLock(ctx, s.media, artifactName(a.ID, ".lock"), true, false)
	if err != nil {
		return err
	}
	defer releaseLock(guard)
	return s.cleanupArtifact(ctx, a, "evicted")
}

// Reconcile removes only unfinished artifacts identified by the durable
// repository. It never adopts bytes from hashes or guesses unknown ownership.
// The lifetime instance lock plus operation gate make reservation-before-file
// transitions safe; active artifact guards are skipped without freeing quota.
func (s *Store) Reconcile(ctx context.Context, limit int) (ReconcileReport, error) {
	var report ReconcileReport
	if limit < 1 || limit > 256 {
		return report, ErrInvalidRequest
	}
	if err := s.beginRecovery(); err != nil {
		return report, err
	}
	defer s.end()
	if err := s.recoverStopped(); err != nil {
		return report, err
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	// Reject unsafe private-directory entries even when no database record names
	// them. Safe unknown files are preserved; retained inventory is private DB.
	for _, dir := range []*directory{s.media, s.working} {
		if err := dir.walkNames(func(name string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// The application executable namespace owns this one working child.
			// Validate its retained directory without treating it as a media file.
			if dir == s.working && name == "native-exec" {
				child, err := dir.subdirectory(name)
				if err != nil {
					return err
				}
				return child.close()
			}
			// A crash may leave the transport-owned socket before its next
			// lease-guarded startup. Preserve only this validated fixed entry.
			if dir == s.working && name == "native.sock" {
				info, err := dir.file.Stat()
				if err != nil || appdata.VerifyNativeSocketEntry(dir.path, info) != nil {
					return ErrUnsafeFile
				}
				return nil
			}
			f, err := dir.open(name, 0)
			if err != nil {
				return err
			}
			return f.Close()
		}); err != nil {
			return report, err
		}
	}
	records, err := s.repo.ReconcileMedia(ctx, limit)
	if err != nil {
		return report, err
	}
	if len(records) > limit {
		return report, ErrUnavailable
	}
	for _, a := range records {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if a.State != "reserved" && a.State != "staged" && a.State != "evicting" {
			return report, ErrUnavailable
		}
		guard, err := artifactLock(ctx, s.media, artifactName(a.ID, ".lock"), true, false)
		if errors.Is(err, ErrActiveUse) {
			report.Active++
			continue
		}
		if err != nil {
			return report, err
		}
		err = s.cleanupArtifact(ctx, a, "orphan_removed")
		_ = releaseLock(guard)
		if err != nil {
			return report, err
		}
		s.mu.Lock()
		p := s.pending[a.ID]
		if p != nil {
			p.cleaned = true
		}
		s.mu.Unlock()
		if p != nil {
			if err := p.lease.Release(); err != nil {
				return report, err
			}
			s.mu.Lock()
			delete(s.pending, a.ID)
			s.mu.Unlock()
		}
		report.Cleaned++
		report.Codes = append(report.Codes, "orphan_removed")
	}
	// Physical release can itself fail after the durable cleanup acknowledgement;
	// retain and retry those already-cleaned leases even though SQL no longer
	// returns their artifacts in the unfinished batch.
	s.mu.Lock()
	ready := map[archive.ArtifactID]*pendingSpace{}
	for id, p := range s.pending {
		if p.cleaned {
			ready[id] = p
		}
	}
	s.mu.Unlock()
	for id, p := range ready {
		if err := p.lease.Release(); err != nil {
			return report, err
		}
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}
	return report, nil
}
