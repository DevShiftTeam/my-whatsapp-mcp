//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotRecoveryPreservesCommittedArchiveAndClient(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	kept := apply(t, s, event("kept", "saved snapshot canary", 1, EventOriginal), p.Generation)
	candidateRoot(t, s, p, "preserved-transcript", "שלום preserved transcript", []MessageID{kept.MessageID}, DerivedProvenance{Kind: DocumentTranscript, OffsetKind: "slice", StartMS: 0, EndMS: 1000})
	apply(t, s, event("deleted", "deleted canary", 1, EventOriginal), p.Generation)
	apply(t, s, event("deleted", "", 2, EventDelete), p.Generation)
	apply(t, s, event("conflict", "first content", 1, EventOriginal), p.Generation)
	if _, err := s.ApplySourceEvent(ctx, event("conflict", "conflicting content", 1, EventOriginal), p.Generation); !errors.Is(err, ErrSourceRevisionConflict) {
		t.Fatalf("conflict fixture: %v", err)
	}
	credential := createCredential(t, s, "snapshot-recovery", CredentialPolicy{Account: "account", AllChats: true})
	before := snapshotRecoveryRows(t, s)
	if _, err := s.BeginObservation(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnsafe(ctx, "account", "drain_failed"); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.readSafety("account"); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("ordinary open cleared denial: %v", err)
	}
	receipt, err := s.RestoreCommittedSnapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RecoveryEpoch != 1 || receipt.ArchiveRevision <= kept.ArchiveRevision || receipt.Mode != "committed_snapshot" || receipt.Code != "owner_restored_snapshot" {
		t.Fatalf("invalid fixed receipt: %#v", receipt)
	}
	if got := snapshotRecoveryRows(t, s); !reflect.DeepEqual(got, before) {
		t.Fatal("snapshot recovery changed source, tombstones, policy, or credentials")
	}
	grant, err := s.ResolveCredential(ctx, sha256.Sum256([]byte("synthetic-bearer-snapshot-recovery")))
	if err != nil || grant.CredentialID != credential.ID {
		t.Fatalf("saved client unavailable: %v", err)
	}
	page, err := s.Search(ctx, grant.Scope, SearchRequest{Query: "canary"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != kept.MessageID {
		t.Fatalf("snapshot visibility changed: %#v %v", page, err)
	}
	for _, name := range []string{"observation.marker", "unsafe.marker"} {
		if _, err = os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old witness remained: %s %v", name, err)
		}
	}
}

func snapshotRecoveryRows(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	for _, query := range []string{
		`SELECT id||':'||body||':'||state||':'||local_revision||':'||protocol_revision||':'||revision_barrier FROM messages ORDER BY id`,
		`SELECT account||':'||chat||':'||sender||':'||message||':'||protocol_revision FROM source_tombstones ORDER BY account,chat,sender,message`,
		`SELECT generation||':'||date_mode||':'||chat_mode||':'||rolling_retention FROM archive_policy`,
		`SELECT id||':'||hex(digest)||':'||revision||':'||revoked FROM credentials ORDER BY id`,
		`SELECT id||':'||text||':'||text_sha256||':'||media_id||':'||provenance_json FROM extension_roots ORDER BY id`,
		`SELECT root_id||':'||message_id||':'||local_revision||':'||policy_generation||':'||content_hash FROM extension_root_sources ORDER BY root_id,message_id`,
		`SELECT id||':'||hex(protocol_reference)||':'||last_original_sha256||':'||availability||':'||active||':'||artifact_id FROM extension_media_associations ORDER BY id`,
		`SELECT id||':'||sha256||':'||state||':'||bytes||':'||charged_bytes FROM extension_media_artifacts ORDER BY id`,
	} {
		rows, err := s.readDB.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var row string
			if err = rows.Scan(&row); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result[query] = append(result[query], row)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	return result
}

func snapshotRecoveryOfflineFixture(t *testing.T) *Store {
	t.Helper()
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("recovery-fixture", "committed recovery canary", 1, EventOriginal), p.Generation)
	if _, err := s.BeginObservation(context.Background(), "account"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnsafe(context.Background(), "account", "drain_failed"); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSnapshotRecoveryCrashBoundariesRemainDeniedAndExplicitRetryCompletes(t *testing.T) {
	for _, stage := range []string{"after_prepare", "before_observation_remove", "before_unsafe_remove", "before_directory_sync", "before_complete"} {
		t.Run(stage, func(t *testing.T) {
			s := snapshotRecoveryOfflineFixture(t)
			s.snapshotRecoveryFault = func(current string) error {
				if current == stage {
					return ErrArchiveUnavailable
				}
				return nil
			}
			if _, err := s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, ErrArchiveUnavailable) {
				t.Fatalf("fault was ignored: %v", err)
			}
			if _, err := s.readSafety("account"); !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("incomplete recovery permitted reads: %v", err)
			}
			dir := s.privateDir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(context.Background(), dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.readSafety("account"); !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("ordinary reopen completed prepared audit: %v", err)
			}
			receipt, err := s.RestoreCommittedSnapshot(context.Background(), "")
			if err != nil || receipt.RecoveryEpoch != 1 {
				t.Fatalf("explicit retry did not finish own audit: %#v %v", receipt, err)
			}
			again, err := s.RestoreCommittedSnapshot(context.Background(), "")
			if err != nil || again != receipt {
				t.Fatalf("completed recovery not idempotent: %#v %v", again, err)
			}
		})
	}
}

func TestSnapshotRecoveryNewIdenticalWitnessSurvivesPreparedRetry(t *testing.T) {
	s := snapshotRecoveryOfflineFixture(t)
	s.snapshotRecoveryFault = func(stage string) error {
		if stage == "before_directory_sync" {
			return ErrArchiveUnavailable
		}
		return nil
	}
	if _, err := s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A later immediate denial has the same fixed body but a new incarnation.
	if err := os.WriteFile(filepath.Join(dir, "unsafe.marker"), []byte("unsafe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("new same-content witness accepted as old: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "unsafe.marker")); err != nil {
		t.Fatal("new witness removed")
	}
}

func TestSnapshotRecoveryNewUnsafeEpochCannotBeOverwritten(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "persisted"}[persisted], func(t *testing.T) {
			s := snapshotRecoveryOfflineFixture(t)
			s.snapshotRecoveryFault = func(stage string) error {
				if stage != "after_prepare" {
					return nil
				}
				if persisted {
					_, err := s.writeDB.Exec(`UPDATE account_safety SET epoch=epoch+1,reason='source_revision_conflict' WHERE account='account'`)
					return err
				}
				return s.denyImmediate("account", "queue_limit_exceeded")
			}
			if _, err := s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, ErrPolicyChanged) {
				t.Fatalf("competing denial overwritten: %v", err)
			}
			dir := s.privateDir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(context.Background(), dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, ErrPolicyChanged) {
				t.Fatalf("restart forgot new unsafe epoch: %v", err)
			}
		})
	}
}

func TestSnapshotRecoveryRejectsCorruptKeywordIndex(t *testing.T) {
	s := snapshotRecoveryOfflineFixture(t)
	if _, err := s.writeDB.Exec(`DELETE FROM message_fts`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("missing committed keyword row accepted: %v", err)
	}
	var prepared int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM snapshot_recovery_audit`).Scan(&prepared); err != nil || prepared != 0 {
		t.Fatalf("corrupt snapshot prepared: %d %v", prepared, err)
	}
}

func TestSnapshotRecoveryRevokesOldDownloadAndReservationLeases(t *testing.T) {
	s, _, dir := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	_, source, media := originalDownloadTestSource(t, s, p, "old-voice-lease", MessageVoice)
	work := originalDownloadTestClaim(t, s)
	reservation := extensionAdmit(t, s, source, media, BudgetOriginals, 128)
	if err := s.MarkUnsafe(context.Background(), "account", "drain_failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.RestoreCommittedSnapshot(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadOriginalDownload(context.Background(), work.Lease); !errors.Is(err, ErrNoWork) {
		t.Fatalf("old download token still authoritative: %v", err)
	}
	if _, err = s.RenewMedia(context.Background(), reservation, time.Minute); !errors.Is(err, ErrNoWork) {
		t.Fatalf("old reservation can renew: %v", err)
	}
	var protocol []byte
	var attempts int
	if err = s.readDB.QueryRow(`SELECT a.protocol_reference,d.attempts FROM extension_media_associations a JOIN extension_original_downloads d ON d.media_id=a.id WHERE a.id=?`, media).Scan(&protocol, &attempts); err != nil || len(protocol) == 0 || attempts != work.Attempts {
		t.Fatalf("revocation destroyed download evidence: %v", err)
	}
}

func TestSnapshotRecoveryPreparedExpiryRemainsFinishableAndUnreadable(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(unixms(time.Now().UTC()))
	now := func() time.Time { return fromMillis(clock.Load()) }
	s, err := Open(ctx, privateTestDir(t), Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	p := testPolicy(t, s)
	e := event("expiring-recovery", "expiry recovery canary", 1, EventOriginal)
	e.OriginalSentAt, e.ExpiresAt = now(), now().Add(time.Minute)
	id := apply(t, s, e, p.Generation).MessageID
	if err = s.MarkUnsafe(ctx, "account", "drain_failed"); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	s.snapshotRecoveryFault = func(stage string) error {
		if stage == "after_prepare" {
			return ErrArchiveUnavailable
		}
		return nil
	}
	if _, err = s.RestoreCommittedSnapshot(ctx, ""); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64((2 * time.Minute) / time.Millisecond))
	s, err = Open(ctx, dir, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SweepExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RestoreCommittedSnapshot(ctx, ""); err != nil {
		t.Fatalf("expiry stranded explicit prepared recovery: %v", err)
	}
	page, err := s.Search(ctx, ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}, SearchRequest{Query: "canary"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("expired source became visible: %#v %v", page, err)
	}
	if err = s.SweepExpired(ctx); err != nil {
		t.Fatal(err)
	}
	var state, body string
	if err = s.readDB.QueryRow(`SELECT state,body FROM messages WHERE id=?`, id).Scan(&state, &body); err != nil || state != "expired" || body != "" {
		t.Fatalf("physical expiry cleanup did not resume: %s %v", state, err)
	}
}

func TestSnapshotRecoveryPreservesPublicationReceiptsAndAcceptedPrefix(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "complete"}[complete], func(t *testing.T) {
			ctx := context.Background()
			s := testArchive(t)
			p := testPolicy(t, s)
			apply(t, s, event("publication-source", "abcdefgh", 1, EventOriginal), p.Generation)
			v := indexTestVersion("snapshot-publication")
			v.MaxPassageTokens = 1
			generation, err := s.EnsureIndex(ctx, v)
			if err != nil {
				t.Fatal(err)
			}
			work, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: generation.ID, TTL: time.Minute, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			input := publicationTestReady(t, s, work)
			bytes := 2
			if complete {
				bytes = 8
			}
			batch := publicationTestPrefix(input, generation.Version, 1, bytes)
			progress, err := s.PublishWindowBatch(ctx, batch)
			if err != nil || !progress.Accepted || progress.Complete != complete {
				t.Fatalf("publication fixture: %#v %v", progress, err)
			}
			var unacceptedBatch WindowBatchPublication
			if !complete {
				// Freeze the next, unpublished prefix under the old lease. Replaying
				// an already accepted batch is not proof of a new publication.
				unacceptedBatch = publicationTestPrefix(publicationTestReady(t, s, work), generation.Version, 1, 2)
			}
			before, err := extensionLoadPublication(ctx, s.readDB, "window", work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.MarkUnsafe(ctx, "account", "drain_failed"); err != nil {
				t.Fatal(err)
			}
			dir := s.privateDir
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.RestoreCommittedSnapshot(ctx, ""); err != nil {
				t.Fatal(err)
			}
			after, err := extensionLoadPublication(ctx, s.readDB, "window", work.ID)
			if !complete {
				before.leaseUntil = 0
			}
			if err != nil || after != before {
				t.Fatalf("durable receipt/prefix changed: %v", err)
			}
			if complete {
				replay, err := s.PublishWindowBatch(ctx, batch)
				if err != nil || !replay.Accepted || !replay.Complete {
					t.Fatalf("completed replay receipt lost: %#v %v", replay, err)
				}
				return
			}
			stale, err := s.PublishWindowBatch(ctx, unacceptedBatch)
			if stale.Accepted || (err != nil && !errors.Is(err, ErrNoWork)) {
				t.Fatalf("old partial lease publishes new content: %#v %v", stale, err)
			}
			newWork, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: generation.ID, TTL: time.Minute, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			resumed := publicationTestReady(t, s, newWork)
			if resumed.Start.SourceOrdinal != 0 || resumed.Start.Field != "body" || resumed.Start.StartByte != 2 {
				t.Fatalf("accepted prefix restarted: %#v", resumed.Start)
			}
			var count int
			if err = s.readDB.QueryRow(`SELECT count(*) FROM extension_documents WHERE generation=?`, generation.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("accepted document removed: %d %v", count, err)
			}
		})
	}
}

func TestSnapshotRecoveryRefusesRunningObservationAndUnrelatedReasons(t *testing.T) {
	for _, reason := range []UnsafeReason{"drain_failed", "identity_alias_conflict", "source_revision_conflict", "queue_limit_exceeded"} {
		t.Run(string(reason), func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			apply(t, s, event("message", "committed text", 1, EventOriginal), p.Generation)
			if _, err := s.BeginObservation(context.Background(), "account"); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkUnsafe(context.Background(), "account", reason); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("running observation restored: %v", err)
			}
			dir := s.privateDir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(context.Background(), dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if reason != "drain_failed" {
				expected := ErrReconciliationRequired
				if reason == "identity_alias_conflict" {
					expected = ErrIdentityAliasConflict
				}
				if _, err = s.RestoreCommittedSnapshot(context.Background(), ""); !errors.Is(err, expected) {
					t.Fatalf("unrelated unsafe reason restored: %v", err)
				}
			}
		})
	}
}
