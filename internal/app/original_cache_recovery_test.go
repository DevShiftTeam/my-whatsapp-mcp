package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func stoppedOriginalCacheProfile(t *testing.T) appdata.Paths {
	t.Helper()
	fixture := newSnapshotRecoveryFixture(t)
	if err := errors.Join(fixture.store.Close(), fixture.lease.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverCommittedSnapshot(context.Background(), fixture.paths.Root); err != nil {
		t.Fatal(err)
	}
	return fixture.paths
}

func TestRecoverOriginalCacheRejectsEitherLiveOwner(t *testing.T) {
	for _, live := range []string{"current", "backup"} {
		t.Run(live, func(t *testing.T) {
			current := stoppedOriginalCacheProfile(t)
			backup := stoppedOriginalCacheProfile(t)
			owned := current
			if live == "backup" {
				owned = backup
			}
			lease, err := appdata.Acquire(owned)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if _, err := RecoverOriginalCache(context.Background(), current.Root, backup.Root); !errors.Is(err, appdata.ErrInstanceAlreadyRunning) {
				t.Fatal("offline cache recovery bypassed a current or backup owner")
			}
			other := backup
			if live == "backup" {
				other = current
			}
			released, err := appdata.Acquire(other)
			if err != nil {
				t.Fatal("refused cache recovery retained the other profile lease")
			}
			if err := released.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoverOriginalCacheRejectsSameAndSymlinkProfiles(t *testing.T) {
	current := stoppedOriginalCacheProfile(t)
	if _, err := RecoverOriginalCache(context.Background(), current.Root, current.Root); !errors.Is(err, appdata.ErrUnsafeDataPath) {
		t.Fatal("same-profile recovery was not refused")
	}
	linkRoot := privateTestRoot(t)
	if err := os.Mkdir(linkRoot, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkRoot, "profile-link")
	if err := os.Symlink(current.Root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverOriginalCache(context.Background(), current.Root, link); !errors.Is(err, appdata.ErrUnsafeDataPath) {
		t.Fatal("symlinked backup recovery was not refused")
	}
}

func TestRecoverOriginalCacheRejectsCancelledAndNoncanonicalInputs(t *testing.T) {
	current := stoppedOriginalCacheProfile(t)
	backup := stoppedOriginalCacheProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RecoverOriginalCache(ctx, current.Root, backup.Root); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled cache recovery acquired profile authority")
	}
	for _, pair := range [][2]string{
		{"relative-current", backup.Root},
		{current.Root, "relative-backup"},
		{current.Root, backup.Root + string(filepath.Separator)},
	} {
		if _, err := RecoverOriginalCache(context.Background(), pair[0], pair[1]); !errors.Is(err, appdata.ErrUnsafeDataPath) {
			t.Fatal("cache recovery accepted a noncanonical profile path")
		}
	}
}

func TestRecoverOriginalCacheRefusesIncompleteProfileWithoutCreatingFiles(t *testing.T) {
	for _, which := range []string{"current", "backup"} {
		for _, missing := range []string{"archive", "session", "state", "lock", "empty_archive", "corrupt_session"} {
			t.Run(which+"_"+missing, func(t *testing.T) {
				current := stoppedOriginalCacheProfile(t)
				backup := stoppedOriginalCacheProfile(t)
				broken := current
				if which == "backup" {
					broken = backup
				}
				path := filepath.Join(broken.ArchiveDir, "archive.sqlite")
				if missing == "session" || missing == "corrupt_session" {
					path = filepath.Join(broken.SessionDir, "session.sqlite")
				} else if missing == "state" {
					path = filepath.Join(broken.WorkingDir, stateFile)
				} else if missing == "lock" {
					path = filepath.Join(broken.Root, ".instance.lock")
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				invalid := missing == "empty_archive" || missing == "corrupt_session"
				if invalid {
					var data []byte
					if missing == "corrupt_session" {
						data = make([]byte, 128)
					}
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				before := originalCacheProfileFiles(t, broken.Root)
				if _, err := RecoverOriginalCache(context.Background(), current.Root, backup.Root); err == nil {
					t.Fatal("cache recovery accepted an incomplete profile")
				}
				assertOriginalCacheProfileFiles(t, broken.Root, before)
			})
		}
	}
}

func TestRecoverOriginalCacheEmptyInventoryPreservesBackupAndPrivateState(t *testing.T) {
	current := stoppedOriginalCacheProfile(t)
	backup := stoppedOriginalCacheProfile(t)
	backupBefore := originalCacheProfileFiles(t, backup.Root)
	protected := make(map[string]string)
	for _, path := range []string{
		filepath.Join(current.SessionDir, "session.sqlite"),
		filepath.Join(current.WorkingDir, stateFile),
		filepath.Join(current.ClientDir, "synthetic-client.json"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		protected[path] = string(data)
	}
	receipt, err := RecoverOriginalCache(context.Background(), current.Root, backup.Root)
	if err != nil || receipt.Restored != 0 || receipt.Skipped != 0 {
		t.Fatalf("empty original-cache inventory failed: %v", err)
	}
	assertOriginalCacheProfileFiles(t, backup.Root, backupBefore)
	for path, before := range protected {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != before {
			t.Fatal("cache recovery changed private app state, session, or client configuration")
		}
	}
	for _, paths := range []appdata.Paths{current, backup} {
		lease, err := appdata.Acquire(paths)
		if err != nil {
			t.Fatal("cache recovery retained profile ownership after completion")
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOriginalCacheRefusalDoesNotHideCleanupFailure(t *testing.T) {
	if !originalCacheRefusal(errors.Join(media.ErrAuthentication, media.ErrFormat)) {
		t.Fatal("expected individual verification refusals were not skippable")
	}
	for _, unexpected := range []error{io.ErrUnexpectedEOF, media.ErrActiveUse, context.Canceled, media.ErrUnavailable} {
		if originalCacheRefusal(errors.Join(media.ErrFormat, unexpected)) {
			t.Fatal("verification refusal hid a cleanup, ownership, or composition failure")
		}
	}
}

func TestRecoverOriginalCacheWiresVerifiedBytesToCurrentArchive(t *testing.T) {
	for _, keepFile := range []bool{true, false} {
		name := "existing_original"
		if !keepFile {
			name = "missing_original"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			current := stoppedOriginalCacheProfile(t)
			lease, err := appdata.Acquire(current)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lease.Close() })
			store, err := archive.Open(ctx, current.ArchiveDir, archive.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			policy, err := store.Policy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			data := encoded.Bytes()
			digest := sha256.Sum256(data)
			reference, err := proto.Marshal(&waE2E.ImageMessage{
				DirectPath: proto.String("/synthetic-cache-original"), MediaKey: bytes.Repeat([]byte{0x11}, 32),
				FileSHA256: digest[:], FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(uint64(len(data))),
			})
			if err != nil {
				t.Fatal(err)
			}
			key := archive.SourceKey{Account: "synthetic-recovery@local.invalid", Chat: "synthetic-chat@local.invalid", Sender: "synthetic-sender@local.invalid", Message: "synthetic-cache-original"}
			result, err := store.ApplySourceEvent(ctx, archive.SourceEvent{
				Key: key, Kind: archive.EventOriginal, BodyKind: archive.MessageImage, OriginalSentAt: time.Now().UTC().Add(-time.Minute),
				OriginalDateKnown: true, SourceRevision: 1,
				Media: []archive.MediaReference{{Kind: string(archive.MessageImage), ProtocolReference: reference, MimeType: "image/png", DeclaredBytes: int64(len(data)), Availability: "not_retained"}},
			}, policy.Generation)
			if err != nil || len(result.MediaIDs) != 1 {
				t.Fatalf("synthetic original source failed: %v", err)
			}
			ref, err := store.CurrentSource(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := store.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: ref, MediaID: result.MediaIDs[0], Budget: archive.BudgetOriginals, MaximumBytes: int64(len(data)), TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if accepted, err := store.PublishOriginal(ctx, archive.OriginalPublication{Reservation: reservation, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(data)), ActualFormat: "png"}); err != nil || !accepted {
				t.Fatalf("synthetic original publication failed: %v", err)
			}
			artifactDigest := sha256.Sum256([]byte(reservation.ArtifactID))
			original := filepath.Join(current.MediaDir, hex.EncodeToString(artifactDigest[:])+".original")
			if err := os.WriteFile(original, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(store.Close(), lease.Close()); err != nil {
				t.Fatal(err)
			}
			backup := copyStoppedOriginalCacheProfile(t, current.Root)
			backupBefore := originalCacheProfileFiles(t, backup.Root)
			// Synthetic fixture-only invalidation models a cache association lost
			// without changing the source, completion record, or original bytes.
			mutationLease, err := appdata.Acquire(current)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { mutationLease.Close() })
			db, err := sql.Open("sqlite3", filepath.Join(current.ArchiveDir, "archive.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			_, updateErr := db.Exec(`UPDATE extension_media_associations SET active=0,protocol_reference=X'',artifact_id='',availability='not_retained'; UPDATE extension_media_artifacts SET state='evicting'; UPDATE messages SET media_availability='not_retained' WHERE id=?; UPDATE messages SET expires_at=? WHERE source_message='synthetic-message'`, result.MessageID, time.Now().UTC().Add(-time.Minute).UnixMilli())
			var unrelatedBefore string
			queryErr := db.QueryRow(`SELECT body||':'||normalized_body||':'||state||':'||local_revision FROM messages WHERE source_message='synthetic-message'`).Scan(&unrelatedBefore)
			if err := errors.Join(updateErr, queryErr, db.Close()); err != nil {
				t.Fatal(err)
			}
			if !keepFile {
				if err := os.Remove(original); err != nil {
					t.Fatal(err)
				}
			}
			if err := mutationLease.Close(); err != nil {
				t.Fatal(err)
			}
			receipt, err := RecoverOriginalCache(ctx, current.Root, backup.Root)
			if err != nil || keepFile && (receipt.Restored != 1 || receipt.Skipped != 0) || !keepFile && (receipt.Restored != 0 || receipt.Skipped != 1) {
				t.Fatalf("nonempty cache recovery returned wrong aggregate result: %v", err)
			}
			assertOriginalCacheProfileFiles(t, backup.Root, backupBefore)
			verificationLease, err := appdata.Acquire(current)
			if err != nil {
				t.Fatal(err)
			}
			defer verificationLease.Close()
			checkDB, err := sql.Open("sqlite3", filepath.Join(current.ArchiveDir, "archive.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			var unrelatedAfter string
			queryErr = checkDB.QueryRow(`SELECT body||':'||normalized_body||':'||state||':'||local_revision FROM messages WHERE source_message='synthetic-message'`).Scan(&unrelatedAfter)
			if err := errors.Join(queryErr, checkDB.Close()); err != nil || unrelatedAfter != unrelatedBefore {
				t.Fatal("offline cache recovery purged or changed unrelated expired text")
			}
			if keepFile {
				after, err := os.ReadFile(original)
				if err != nil || !bytes.Equal(data, after) {
					t.Fatal("in-place composition modified the verified original")
				}
				store, err := archive.Open(ctx, current.ArchiveDir, archive.Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				artifact, err := store.AuthorizeOriginal(ctx, archive.ReadScope{Account: key.Account, Principal: "owner", AllChats: true, PolicyGeneration: policy.Generation}, result.MediaIDs[0])
				if err != nil || artifact.ID != reservation.ArtifactID || artifact.SHA256 != hex.EncodeToString(digest[:]) {
					t.Fatal("verified cache was not reattached through ordinary original authorization")
				}
			} else if _, err := os.Lstat(original); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("missing original recovery copied or recreated recording bytes")
			}
		})
	}
}

func copyStoppedOriginalCacheProfile(t *testing.T, source string) appdata.Paths {
	t.Helper()
	backup, err := appdata.Prepare(privateTestRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(backup.Root, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	return backup
}

func originalCacheProfileFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func assertOriginalCacheProfileFiles(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := originalCacheProfileFiles(t, root)
	if len(after) != len(before) {
		t.Fatal("cache recovery changed the profile file inventory")
	}
	for path, data := range before {
		if current, ok := after[path]; !ok || current != data {
			t.Fatal("cache recovery modified a protected profile file")
		}
	}
}
