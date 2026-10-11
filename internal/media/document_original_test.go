package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// The ordinary fake predates persisted formats. Keep its existing image and
// voice fixtures unchanged, while these tests retain the publication format.
type documentRepository struct {
	*fakeRepository
	onAuthorize func()
}

func (f *documentRepository) AuthorizeOriginal(ctx context.Context, scope archive.ReadScope, id archive.MediaID) (archive.MediaArtifact, error) {
	if f.onAuthorize != nil {
		f.onAuthorize()
	}
	return f.fakeRepository.AuthorizeOriginal(ctx, scope, id)
}

func (f *documentRepository) PublishOriginal(ctx context.Context, publication archive.OriginalPublication) (bool, error) {
	accepted, err := f.fakeRepository.PublishOriginal(ctx, publication)
	if accepted {
		f.mu.Lock()
		artifact := f.artifacts[publication.Reservation.ArtifactID]
		artifact.ActualFormat = publication.ActualFormat
		f.artifacts[artifact.ID] = artifact
		f.mu.Unlock()
	}
	return accepted, err
}

func retainedDocument(t *testing.T, data []byte) (*Store, *documentRepository, *Original) {
	t.Helper()
	f := &documentRepository{fakeRepository: newRepository()}
	s, _, _ := testStore(t, f.fakeRepository, Options{})
	s.repo = f
	q := documentRequest(f.fakeRepository, data)
	download, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = download.Close() })
	if _, err := download.File().Write(data); err != nil {
		t.Fatal(err)
	}
	if err := download.Complete(context.Background(), receipt(q, data)); err != nil {
		t.Fatal(err)
	}
	if err := download.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := s.OpenOriginal(context.Background(), documentOwnerScope(), q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Close() })
	return s, f, original
}

func documentOwnerScope() archive.ReadScope {
	return archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}
}

func TestDocumentOriginalKeepsVerifiedBytesMetadataAndCleanupGuards(t *testing.T) {
	for _, data := range [][]byte{documentPDFFixture(), []byte("opaque synthetic document")} {
		s, f, original := retainedDocument(t, data)
		metadata := original.Metadata()
		wantHash := sha256.Sum256(data)
		wantFormat := "document"
		if bytes.HasPrefix(data, []byte("%PDF-")) {
			wantFormat = "pdf"
		}
		if metadata.Bytes != int64(len(data)) || metadata.SHA256 != hex.EncodeToString(wantHash[:]) || metadata.Format != wantFormat {
			t.Fatal("retained document metadata does not describe the authenticated original")
		}
		file, err := original.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(file)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("retained document bytes changed", err)
		}
		if err := original.Close(); !errors.Is(err, ErrActiveUse) {
			t.Fatal("live document descriptor lost its cleanup guard", err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := original.file.Seek(1, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if err := original.Recheck(context.Background(), documentOwnerScope()); err != nil {
			t.Fatal("unchanged source-bound document rejected", err)
		}
		if position, err := original.file.Seek(0, io.SeekCurrent); err != nil || position != 1 {
			t.Fatal("recheck changed the retained descriptor offset", err)
		}
		f.processed = true
		if err := s.Evict(context.Background(), metadata.MediaID); !errors.Is(err, ErrActiveUse) {
			t.Fatal("guarded document evicted during read", err)
		}
		if err := original.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Evict(context.Background(), metadata.MediaID); err != nil {
			t.Fatal("closed document could not be evicted", err)
		}
	}
}

func TestDocumentOriginalRecheckRejectsChangedAuthority(t *testing.T) {
	for _, change := range []string{"denied", "artifact", "source revision", "policy", "hash", "size", "format", "budget"} {
		t.Run(change, func(t *testing.T) {
			_, f, original := retainedDocument(t, documentPDFFixture())
			initial := original.Metadata()
			f.mu.Lock()
			artifact := f.artifacts[initial.ArtifactID]
			switch change {
			case "denied":
				f.denied[initial.MediaID] = true
			case "artifact":
				artifact.ID = "different-artifact"
			case "source revision":
				artifact.Source.Version.Revision++
			case "policy":
				artifact.Source.Version.PolicyGeneration++
			case "hash":
				artifact.SHA256 = hex.EncodeToString(make([]byte, sha256.Size))
			case "size":
				artifact.Bytes++
			case "format":
				artifact.ActualFormat = "document"
			case "budget":
				artifact.Budget = archive.BudgetDecoded
			}
			f.artifacts[initial.ArtifactID] = artifact
			f.mu.Unlock()
			if err := original.Recheck(context.Background(), documentOwnerScope()); err == nil {
				t.Fatal("changed document authority accepted")
			}
			if original.Metadata() != initial {
				t.Fatal("recheck replaced captured metadata with new authority")
			}
		})
	}
}

func TestDocumentOriginalRecheckRejectsAuthorityChangesDuringVerification(t *testing.T) {
	for _, change := range []string{"revoked", "source changed"} {
		t.Run(change, func(t *testing.T) {
			_, f, original := retainedDocument(t, bytes.Repeat([]byte("document block\n"), 10000))
			ctx := &verificationMutationContext{Context: context.Background(), at: 3, mutate: func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				if change == "revoked" {
					f.denied[original.Metadata().MediaID] = true
				} else {
					artifact := f.artifacts[original.Metadata().ArtifactID]
					artifact.Source.Version.Revision++
					f.artifacts[artifact.ID] = artifact
				}
			}}
			if err := original.Recheck(ctx, documentOwnerScope()); err == nil {
				t.Fatal("authority changed during document verification was accepted")
			}
		})
	}
}

func TestDocumentOriginalRecheckRejectsFileAndGuardReplacement(t *testing.T) {
	for _, change := range []string{"same inode bytes", "original replacement", "artifact guard replacement", "reader guard replacement", "permissions", "hardlink"} {
		t.Run(change, func(t *testing.T) {
			s, _, original := retainedDocument(t, []byte("opaque synthetic document"))
			name := artifactName(original.Metadata().ArtifactID, ".original")
			path := filepath.Join(s.media.path, name)
			switch change {
			case "same inode bytes":
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte("O"), 0)
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
				if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "original replacement", "artifact guard replacement", "reader guard replacement":
				if change == "artifact guard replacement" {
					path = filepath.Join(s.media.path, artifactName(original.Metadata().ArtifactID, ".lock"))
				} else if change == "reader guard replacement" {
					path = filepath.Join(s.media.path, "media-readers.lock")
				}
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("opaque synthetic document"), 0600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".linked"); err != nil {
					t.Fatal(err)
				}
			}
			if err := original.Recheck(context.Background(), documentOwnerScope()); err == nil {
				t.Fatal("changed document file or cleanup guard accepted")
			}
		})
	}
}

func TestDocumentOriginalRecheckRejectsCancellationAndClosedHandle(t *testing.T) {
	_, _, original := retainedDocument(t, documentPDFFixture())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := original.Recheck(ctx, documentOwnerScope()); !errors.Is(err, context.Canceled) {
		t.Fatal("document recheck ignored cancellation", err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := original.Recheck(context.Background(), documentOwnerScope()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed document rechecked", err)
	}
}

func TestDocumentOriginalRecheckRejectsChangesAtFinalAuthorization(t *testing.T) {
	for _, change := range []string{"cancellation", "source", "guard"} {
		t.Run(change, func(t *testing.T) {
			s, f, original := retainedDocument(t, []byte("opaque synthetic document"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			f.onAuthorize = func() {
				calls++
				if calls != 2 {
					return
				}
				switch change {
				case "cancellation":
					cancel()
				case "source":
					f.mu.Lock()
					artifact := f.artifacts[original.Metadata().ArtifactID]
					artifact.Source.Version.Revision++
					f.artifacts[artifact.ID] = artifact
					f.mu.Unlock()
				case "guard":
					path := filepath.Join(s.media.path, "media-readers.lock")
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := original.Recheck(ctx, documentOwnerScope()); err == nil {
				t.Fatal("final authorization changed source, cancellation, or file guard was accepted")
			}
			if calls != 2 {
				t.Fatal("source was not reauthorized after descriptor verification")
			}
		})
	}
}

func TestDocumentOriginalRequiresPersistedDocumentFormat(t *testing.T) {
	s, f, original := retainedDocument(t, []byte("opaque synthetic document"))
	f.mu.Lock()
	artifact := f.artifacts[original.Metadata().ArtifactID]
	artifact.ActualFormat = ""
	f.artifacts[artifact.ID] = artifact
	f.mu.Unlock()
	if reopened, err := s.OpenOriginal(context.Background(), documentOwnerScope(), original.Metadata().MediaID); !errors.Is(err, ErrFormat) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatal("generic original accepted arbitrary bytes without a document record", err)
	}
}

func TestDocumentOriginalReopenEnforcesHardCap(t *testing.T) {
	s, f, original := retainedDocument(t, []byte("opaque synthetic document"))
	path := filepath.Join(s.media.path, artifactName(original.Metadata().ArtifactID, ".original"))
	if err := os.Truncate(path, (8<<20)+1); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	artifact := f.artifacts[original.Metadata().ArtifactID]
	artifact.Bytes = (8 << 20) + 1
	f.artifacts[artifact.ID] = artifact
	f.mu.Unlock()
	if reopened, err := s.OpenOriginal(context.Background(), documentOwnerScope(), original.Metadata().MediaID); !errors.Is(err, ErrLimit) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatal("retained document reopened beyond the hard cap", err)
	}
}
