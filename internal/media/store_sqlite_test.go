//go:build sqlite_fts5

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

var crashNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func sqliteMediaFixture(t *testing.T, root string, stage string) (*Store, *archive.Store, appdata.Paths, DownloadRequest, []byte) {
	t.Helper()
	ctx := context.Background()
	paths, err := appdata.Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	repo, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{Now: func() time.Time { return crashNow }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	policy, err := repo.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Generation == 0 {
		policy, err = repo.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	data := pngData(t)
	event := archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "media-crash-fixture"}, Kind: archive.EventOriginal, OriginalSentAt: crashNow.Add(-time.Hour), OriginalDateKnown: true, SourceRevision: 1, Caption: "synthetic media", BodyKind: archive.MessageImage, Media: []archive.MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic authenticated protocol reference"), MimeType: "image/png", DeclaredBytes: int64(len(data)), Availability: "not_retained"}}}
	applied, err := repo.ApplySourceEvent(ctx, event, policy.Generation)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatalf("source association %#v %v", applied, err)
	}
	admission, err := diskspace.New(paths.Root, diskspace.Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admission.Close() })
	s, err := Open(paths, repo, Options{Admission: admission, OnTransition: func(v Transition) {
		if stage != "" && v.Phase == stage {
			os.Exit(72)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	q := DownloadRequest{Key: event.Key, Version: applied.Version, MediaID: applied.MediaIDs[0], Kind: archive.MessageImage, DeclaredBytes: int64(len(data)), ProtocolSHA256: sha256.Sum256(data)}
	return s, repo, paths, q, data
}

func TestMediaCrashChild(t *testing.T) {
	root := os.Getenv("WA_INDEXER_MEDIA_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess fixture")
	}
	stage := os.Getenv("WA_INDEXER_MEDIA_CRASH_STAGE")
	s, _, _, q, data := sqliteMediaFixture(t, root, stage)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.File().Write(data); err != nil {
		t.Fatal(err)
	}
	if stage == "removed" || stage == "cleanup_acknowledged" {
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := d.Complete(context.Background(), ingestion.VerifiedMedia{Key: q.Key, Version: q.Version, Kind: q.Kind, SHA256: q.ProtocolSHA256, Bytes: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("crash checkpoint not reached")
}

func TestCrashAtEveryPublishBoundary(t *testing.T) {
	for _, stage := range []string{"reserved", "created", "synced", "renamed", "published", "removed", "cleanup_acknowledged"} {
		t.Run(stage, func(t *testing.T) {
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(parent, "owned")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestMediaCrashChild$")
			cmd.Env = append(os.Environ(), "WA_INDEXER_MEDIA_CRASH_ROOT="+root, "WA_INDEXER_MEDIA_CRASH_STAGE="+stage)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 72 {
				t.Fatalf("checkpoint %s failed: %v %s", stage, err, output)
			}
			s, repo, paths, q, data := sqliteMediaFixture(t, root, "")
			scope := archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: q.Version.PolicyGeneration}
			records, err := repo.ReconcileMedia(context.Background(), 256)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "published" {
				if len(records) != 0 {
					t.Fatal("retained record offered for crash cleanup")
				}
				o, err := s.OpenOriginal(context.Background(), scope, q.MediaID)
				if err != nil {
					t.Fatal(err)
				}
				fd, err := o.Open()
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(fd)
				_ = fd.Close()
				_ = o.Close()
				if err != nil || !bytes.Equal(got, data) {
					t.Fatal("published bytes changed after crash")
				}
			}
			if stage != "published" && stage != "cleanup_acknowledged" {
				if len(records) != 1 || records[0].ReservedBytes != wireMaximum(int64(len(data))) {
					t.Fatalf("unacknowledged crash charge lost: %#v", records)
				}
				if _, err := s.OpenOriginal(context.Background(), scope, q.MediaID); err == nil {
					t.Fatal("unpublished crash file authorized")
				}
			}
			report, err := s.Reconcile(context.Background(), 256)
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if stage == "published" || stage == "cleanup_acknowledged" {
				expected = 0
			}
			if report.Cleaned != expected {
				t.Fatalf("wrong crash cleanup %+v", report)
			}
			again, err := s.Reconcile(context.Background(), 256)
			if err != nil || again.Cleaned != 0 {
				t.Fatal("cleanup not idempotent", again, err)
			}
			remaining, err := repo.ReconcileMedia(context.Background(), 256)
			if err != nil || len(remaining) != 0 {
				t.Fatal("cleanup charge remains", remaining, err)
			}
			entries, err := os.ReadDir(paths.MediaDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				ext := filepath.Ext(e.Name())
				if ext == ".part" || ext == ".original" && stage != "published" {
					t.Fatal("crash bytes remain after acknowledged cleanup")
				}
			}
		})
	}
}
