package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type snapshotRecoveryFixture struct {
	paths      appdata.Paths
	lease      *appdata.InstanceLock
	store      *archive.Store
	credential archive.Credential
	digest     [32]byte
	messageID  archive.MessageID
}

func newSnapshotRecoveryFixture(t *testing.T) snapshotRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	paths, err := appdata.Prepare(privateTestRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	state := []byte(`{"version":1,"mode":"real","paused":true,"desired_connected":true,"remote_clients":{"synthetic_secret":"STATE-KEEP-SENTINEL"}}`)
	if err := appdata.WithWorking(paths, lease, func(root *os.Root) error {
		file, err := root.OpenFile(stateFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(state)
		return errors.Join(writeErr, file.Close())
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ClientDir, "synthetic-client.json"), []byte(`{"token":"CLIENT-KEEP-SENTINEL"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// This is an independent SQLite file. Recovery must not load its schema,
	// connect a device, or change any session bytes.
	sessionPath := filepath.Join(paths.SessionDir, "session.sqlite")
	if err := os.WriteFile(sessionPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sql.Open("sqlite3", sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	_, sessionErr := session.Exec("CREATE TABLE synthetic_session(secret TEXT); INSERT INTO synthetic_session VALUES('SESSION-KEEP-SENTINEL')")
	if err := errors.Join(sessionErr, session.Close()); err != nil {
		t.Fatal(err)
	}
	store, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	policy, err := store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplySourceEvent(ctx, archive.SourceEvent{
		Key:  archive.SourceKey{Account: "synthetic-recovery@local.invalid", Chat: "synthetic-chat@local.invalid", Sender: "synthetic-sender@local.invalid", Message: "synthetic-message"},
		Kind: archive.EventOriginal, Body: "snapshotkeep synthetic saved message", BodyKind: archive.MessageText,
		OriginalSentAt: time.Now().UTC().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1,
	}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("SYNTHETIC-CLIENT-TOKEN"))
	credential, err := store.CreateCredential(ctx, archive.CredentialCreate{
		Label: "Synthetic existing client", Principal: "client:synthetic-recovery", Digest: digest,
		Policy: archive.CredentialPolicy{Account: "synthetic-recovery@local.invalid", AllChats: true}, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnsafe(ctx, "synthetic-recovery@local.invalid", "drain_failed"); err != nil {
		t.Fatal(err)
	}
	return snapshotRecoveryFixture{paths: paths, lease: lease, store: store, credential: credential, digest: digest, messageID: result.MessageID}
}

func TestRecoverCommittedSnapshotRejectsLiveOwner(t *testing.T) {
	fixture := newSnapshotRecoveryFixture(t)
	if _, err := RecoverCommittedSnapshot(context.Background(), fixture.paths.Root); !errors.Is(err, appdata.ErrInstanceAlreadyRunning) {
		t.Fatal("offline recovery bypassed the application owner")
	}
}

func TestRecoverCommittedSnapshotPreservesSessionStateAndClient(t *testing.T) {
	ctx := context.Background()
	fixture := newSnapshotRecoveryFixture(t)
	if err := errors.Join(fixture.store.Close(), fixture.lease.Close()); err != nil {
		t.Fatal(err)
	}
	protected := []string{
		filepath.Join(fixture.paths.SessionDir, "session.sqlite"),
		filepath.Join(fixture.paths.WorkingDir, stateFile),
		filepath.Join(fixture.paths.ClientDir, "synthetic-client.json"),
	}
	before := make(map[string][]byte, len(protected))
	for _, path := range protected {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	receipt, err := RecoverCommittedSnapshot(ctx, fixture.paths.Root)
	if err != nil || receipt.Mode != "committed_snapshot" || receipt.Code != "owner_restored_snapshot" || receipt.RecoveryEpoch == 0 {
		t.Fatalf("explicit stopped-owner recovery failed: %v", err)
	}
	for _, path := range protected {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, before[path]) {
			t.Fatal("recovery changed the session or client configuration")
		}
	}
	paths, err := appdata.ExistingPaths(fixture.paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal("recovery retained application ownership after completion")
	}
	defer lease.Close()
	store, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	grant, err := store.ResolveCredential(ctx, fixture.digest)
	if err != nil || grant.CredentialID != fixture.credential.ID || grant.CredentialRevision != fixture.credential.Revision || !grant.Scope.AllChats {
		t.Fatal("recovery lost or broadened an existing client")
	}
	page, err := store.Search(ctx, grant.Scope, archive.SearchRequest{Query: "snapshotkeep"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != fixture.messageID {
		t.Fatal("recovery lost committed text or its existing client access")
	}
}

func TestRecoverCommittedSnapshotRefusesMissingArchiveBeforeCreation(t *testing.T) {
	for _, missing := range []string{"archive", "session", "state", "empty_archive", "empty_session", "corrupt_archive", "corrupt_session"} {
		t.Run(missing, func(t *testing.T) {
			fixture := newSnapshotRecoveryFixture(t)
			if err := errors.Join(fixture.store.Close(), fixture.lease.Close()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture.paths.ArchiveDir, "archive.sqlite")
			if missing == "session" || missing == "empty_session" || missing == "corrupt_session" {
				path = filepath.Join(fixture.paths.SessionDir, "session.sqlite")
			} else if missing == "state" {
				path = filepath.Join(fixture.paths.WorkingDir, stateFile)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			invalidFile := missing == "empty_archive" || missing == "empty_session" || missing == "corrupt_archive" || missing == "corrupt_session"
			var expected []byte
			if missing == "corrupt_archive" || missing == "corrupt_session" {
				expected = bytes.Repeat([]byte("CORRUPT-HEADER"), 16)
			}
			if invalidFile {
				if err := os.WriteFile(path, expected, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := RecoverCommittedSnapshot(context.Background(), fixture.paths.Root); err == nil {
				t.Fatal("recovery initialized incomplete existing data")
			}
			if invalidFile {
				if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, expected) {
					t.Fatal("recovery initialized an empty or corrupt SQLite file")
				}
			} else {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("recovery recreated missing application data")
				}
			}
		})
	}
}
