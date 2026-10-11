//go:build darwin || linux

package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type originalCacheRepository struct {
	*fakeRepository
	calls  int
	accept bool
	err    error
}

func (f *originalCacheRepository) RecoverOriginalCache(_ context.Context, w archive.OriginalCacheWitness) (bool, error) {
	f.calls++
	if !f.accept || f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.artifacts[w.ArtifactID]
	a.State = "retained"
	f.artifacts[a.ID] = a
	f.media[w.MediaID] = a.ID
	return true, nil
}

func originalCacheFixture(t *testing.T) (*Store, *originalCacheRepository, archive.OriginalCacheWitness, string) {
	t.Helper()
	f := newRepository()
	f.source.ContentHash = strings.Repeat("a", 64)
	s, paths, _ := testStore(t, f, Options{})
	q := retainTestOriginal(t, s, f)
	a := f.artifacts[f.media[q.MediaID]]
	a.State = "evicting"
	f.artifacts[a.ID] = a
	repo := &originalCacheRepository{fakeRepository: f, accept: true}
	s.repo = repo
	w := archive.OriginalCacheWitness{Source: f.source, Key: q.Key, MediaID: q.MediaID, ArtifactID: a.ID, Kind: q.Kind, DeclaredBytes: a.Bytes, SHA256: a.SHA256, Bytes: a.Bytes, Format: "png"}
	return s, repo, w, filepath.Join(paths.MediaDir, artifactName(a.ID, ".original"))
}

func TestOriginalCacheRecoveryVerifiesExistingBytesBeforeMetadata(t *testing.T) {
	s, repo, w, path := originalCacheFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	charge := repo.charge(archive.BudgetOriginals)
	accepted, err := s.RecoverOriginalCache(context.Background(), w)
	if err != nil || !accepted || repo.calls != 1 {
		t.Fatalf("repair result accepted=%v calls=%d err=%v", accepted, repo.calls, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) || repo.charge(archive.BudgetOriginals) != charge {
		t.Fatal("in-place repair changed bytes or accounting")
	}
	if repo.artifacts[w.ArtifactID].State != "retained" {
		t.Fatal("verified original was not restored")
	}
}

func TestOriginalCacheRecoveryRejectsMissingOrUnverifiedBytes(t *testing.T) {
	for _, attack := range []string{"missing", "digest", "size", "format", "symlink", "hardlink", "source"} {
		t.Run(attack, func(t *testing.T) {
			s, repo, w, path := originalCacheFixture(t)
			switch attack {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "digest":
				w.SHA256 = strings.Repeat("b", 64)
			case "size":
				w.Bytes++
			case "format":
				w.Format = "jpeg"
			case "source":
				repo.source.Version.Revision++
			case "symlink", "hardlink":
				outside := filepath.Join(filepath.Dir(filepath.Dir(path)), "private-witness")
				bytes, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(outside, bytes, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if attack == "symlink" {
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Link(outside, path); err != nil {
						t.Fatal(err)
					}
				}
			}
			accepted, err := s.RecoverOriginalCache(context.Background(), w)
			if accepted || err == nil || repo.calls != 0 {
				t.Fatalf("unverified original reached metadata: accepted=%v calls=%d err=%v", accepted, repo.calls, err)
			}
			if repo.artifacts[w.ArtifactID].State != "evicting" {
				t.Fatal("denied repair changed artifact state")
			}
		})
	}
}

func TestOriginalCacheRecoveryRejectsBusyOwnerAndGuard(t *testing.T) {
	for _, holder := range []string{"operation", "artifact", "readers"} {
		t.Run(holder, func(t *testing.T) {
			s, repo, w, _ := originalCacheFixture(t)
			var release func()
			if holder == "operation" {
				if err := s.begin(); err != nil {
					t.Fatal(err)
				}
				release = s.end
			} else {
				name := artifactName(w.ArtifactID, ".lock")
				if holder == "readers" {
					name = "media-readers.lock"
				}
				guard, err := artifactLock(context.Background(), s.media, name, false, false)
				if err != nil {
					t.Fatal(err)
				}
				release = func() { _ = releaseLock(guard) }
			}
			accepted, err := s.RecoverOriginalCache(context.Background(), w)
			release()
			if accepted || !errors.Is(err, ErrActiveUse) || repo.calls != 0 {
				t.Fatalf("busy owner admitted accepted=%v calls=%d err=%v", accepted, repo.calls, err)
			}
		})
	}
}

func TestOriginalCacheRecoveryCoreRefusalPreservesOriginal(t *testing.T) {
	s, repo, w, path := originalCacheFixture(t)
	repo.accept = false
	accepted, err := s.RecoverOriginalCache(context.Background(), w)
	if err != nil || accepted || repo.calls != 1 {
		t.Fatalf("unexpected refusal accepted=%v calls=%d err=%v", accepted, repo.calls, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("refused recovery deleted original")
	}
	if repo.artifacts[w.ArtifactID].State != "evicting" {
		t.Fatal("refused recovery changed artifact")
	}
}

func TestOriginalCacheRecoveryCancellationDoesNotPublish(t *testing.T) {
	s, repo, w, _ := originalCacheFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	accepted, err := s.RecoverOriginalCache(ctx, w)
	if accepted || !errors.Is(err, context.Canceled) || repo.calls != 0 {
		t.Fatalf("cancelled recovery accepted=%v calls=%d err=%v", accepted, repo.calls, err)
	}
}
