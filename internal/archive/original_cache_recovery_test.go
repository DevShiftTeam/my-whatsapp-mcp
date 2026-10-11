//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type originalCacheRecoveryTestPort interface {
	RecoverOriginalCache(context.Context, OriginalCacheWitness) (bool, error)
}

func originalCacheRecoveryPort(t *testing.T, s *Store) originalCacheRecoveryTestPort {
	t.Helper()
	p, ok := any(s).(originalCacheRecoveryTestPort)
	if !ok {
		t.Fatal("archive does not implement offline original-cache recovery")
	}
	return p
}

func originalCacheRecoveryFixture(t *testing.T) (*Store, OriginalCacheWitness, ArchivePolicy) {
	t.Helper()
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	e, ref, media := originalDownloadTestSource(t, s, p, "cache-witness", MessageVoice)
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatalf("fixture publication: %v %v", ok, err)
	}
	w := OriginalCacheWitness{Source: ref, Key: e.Key, MediaID: media, ArtifactID: r.ArtifactID, Ordinal: 0, Kind: MessageVoice, MIMEType: e.Media[0].MimeType, DeclaredBytes: 64, ProtocolReference: bytes.Clone(e.Media[0].ProtocolReference), SHA256: strings.Repeat("a", 64), Bytes: 64, Format: "ogg"}
	if err := s.readDB.QueryRow(`SELECT m.chat_id,m.extension_media_identity,d.descriptor_sha256,d.protocol_sha256,d.legacy_source_media_identity FROM messages m JOIN extension_original_downloads d ON d.message_id=m.id WHERE d.media_id=?`, media).Scan(&w.ChatID, &w.MediaIdentity, &w.DescriptorSHA256, &w.ProtocolSHA256, &w.LegacyIdentity); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE extension_media_associations SET active=0,protocol_reference=X'',artifact_id='',availability='not_retained' WHERE id=?`,
		`UPDATE extension_media_artifacts SET state='evicting' WHERE media_id=?`,
	} {
		if _, err := s.writeDB.Exec(statement, media); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.writeDB.Exec(`UPDATE messages SET media_availability='not_retained' WHERE id=?`, ref.Version.MessageID); err != nil {
		t.Fatal(err)
	}
	return s, w, p
}

func originalCacheRecoveryState(t *testing.T, s *Store, w OriginalCacheWitness) string {
	t.Helper()
	var state string
	if err := s.readDB.QueryRow(`SELECT a.active||':'||hex(a.protocol_reference)||':'||a.artifact_id||':'||a.availability||':'||a.last_original_sha256||':'||o.state||':'||o.charged_bytes||':'||o.cleanup_ack||':'||d.completed_sha256||':'||d.state||':'||m.local_revision||':'||m.content_hash FROM extension_media_associations a JOIN extension_media_artifacts o ON o.media_id=a.id JOIN extension_original_downloads d ON d.media_id=a.id JOIN messages m ON m.id=a.message_id WHERE a.id=? AND o.id=?`, w.MediaID, w.ArtifactID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestOriginalCacheRecoveryReattachesOnlyVerifiedOriginal(t *testing.T) {
	s, w, p := originalCacheRecoveryFixture(t)
	port := originalCacheRecoveryPort(t, s)
	before := extensionMediaCharge(t, s, BudgetOriginals)
	var revision ArchiveRevision
	if err := s.readDB.QueryRow(`SELECT archive_revision FROM archive_meta`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || !ok {
		t.Fatalf("verified recovery: %v %v", ok, err)
	}
	if charge := extensionMediaCharge(t, s, BudgetOriginals); charge != before {
		t.Fatalf("physical charge changed: %d != %d", charge, before)
	}
	ref, err := s.CurrentSource(context.Background(), w.Key)
	if err != nil || ref != w.Source {
		t.Fatalf("source authority changed: %v", err)
	}
	a, err := s.AuthorizeOriginal(context.Background(), ReadScope{Account: w.Source.Account, Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, w.MediaID)
	if err != nil || a.ID != w.ArtifactID || a.SHA256 != w.SHA256 || a.Bytes != w.Bytes {
		t.Fatalf("reattached original unavailable: %v", err)
	}
	var marker, completed, downloadState string
	var after ArchiveRevision
	var roots int
	if err := s.readDB.QueryRow(`SELECT a.last_original_sha256,d.completed_sha256,d.state FROM extension_media_associations a JOIN extension_original_downloads d ON d.media_id=a.id WHERE a.id=?`, w.MediaID).Scan(&marker, &completed, &downloadState); err != nil {
		t.Fatal(err)
	}
	if marker != w.SHA256 || completed != w.SHA256 || downloadState != "complete" {
		t.Fatal("durable original completion evidence changed")
	}
	if err := s.readDB.QueryRow(`SELECT archive_revision,(SELECT count(*) FROM extension_roots) FROM archive_meta`).Scan(&after, &roots); err != nil || after != revision+1 || roots != 0 {
		t.Fatalf("repair invented roots or wrong revision: %d %d %v", after, roots, err)
	}
	if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || !ok {
		t.Fatalf("exact replay: %v %v", ok, err)
	}
	if err := s.readDB.QueryRow(`SELECT archive_revision FROM archive_meta`).Scan(&revision); err != nil || revision != after {
		t.Fatalf("idempotent repair bumped revision: %v", err)
	}
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: w.Source.Account, Version: extractionTestVersion(JobTranscription), TTL: time.Minute})
	if err != nil || work.MediaID != w.MediaID || work.ArtifactID != w.ArtifactID || work.Source != w.Source || work.OriginalSHA256 != w.SHA256 {
		t.Fatalf("ordinary extraction not eligible after repair: %v", err)
	}
}

func TestOriginalCacheRecoveryRejectsChangedWitness(t *testing.T) {
	mutations := map[string]func(*OriginalCacheWitness){
		"account":  func(w *OriginalCacheWitness) { w.Source.Account = "other" },
		"key":      func(w *OriginalCacheWitness) { w.Key.Message = "other" },
		"chat":     func(w *OriginalCacheWitness) { w.ChatID = "other" },
		"message":  func(w *OriginalCacheWitness) { w.Source.Version.MessageID = "other" },
		"revision": func(w *OriginalCacheWitness) { w.Source.Version.Revision++ },
		"policy":   func(w *OriginalCacheWitness) { w.Source.Version.PolicyGeneration++ },
		"hash":     func(w *OriginalCacheWitness) { w.Source.ContentHash = strings.Repeat("b", 64) },
		"expiry": func(w *OriginalCacheWitness) {
			w.Source.Version.ExpiresAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		},
		"media":            func(w *OriginalCacheWitness) { w.MediaID = "other" },
		"artifact":         func(w *OriginalCacheWitness) { w.ArtifactID = "other" },
		"ordinal":          func(w *OriginalCacheWitness) { w.Ordinal = 1 },
		"kind":             func(w *OriginalCacheWitness) { w.Kind = MessageImage },
		"mime":             func(w *OriginalCacheWitness) { w.MIMEType = "audio/mpeg" },
		"declared":         func(w *OriginalCacheWitness) { w.DeclaredBytes++ },
		"reference":        func(w *OriginalCacheWitness) { w.ProtocolReference = []byte("different") },
		"ordered identity": func(w *OriginalCacheWitness) { w.MediaIdentity = strings.Repeat("b", 64) },
		"sha":              func(w *OriginalCacheWitness) { w.SHA256 = strings.Repeat("b", 64) },
		"size":             func(w *OriginalCacheWitness) { w.Bytes++ },
		"format":           func(w *OriginalCacheWitness) { w.Format = "mp3" },
		"descriptor":       func(w *OriginalCacheWitness) { w.DescriptorSHA256 = strings.Repeat("b", 64) },
		"protocol":         func(w *OriginalCacheWitness) { w.ProtocolSHA256 = strings.Repeat("b", 64) },
		"legacy":           func(w *OriginalCacheWitness) { w.LegacyIdentity = strings.Repeat("b", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s, w, _ := originalCacheRecoveryFixture(t)
			port := originalCacheRecoveryPort(t, s)
			before := originalCacheRecoveryState(t, s, w)
			changed := w
			mutate(&changed)
			if ok, err := port.RecoverOriginalCache(context.Background(), changed); err != nil || ok {
				t.Fatalf("changed witness accepted: %v %v", ok, err)
			}
			if originalCacheRecoveryState(t, s, w) != before {
				t.Fatal("denied recovery mutated original/source state")
			}
		})
	}
}

func TestOriginalCacheRecoveryAcceptsInactiveRetainedAvailability(t *testing.T) {
	s, w, _ := originalCacheRecoveryFixture(t)
	port := originalCacheRecoveryPort(t, s)
	// Account invalidation clears the reference and artifact link without
	// changing this historical availability label. It is not read authority.
	if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET availability='retained' WHERE id=?`, w.MediaID); err != nil {
		t.Fatal(err)
	}
	if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || !ok {
		t.Fatalf("inactive completed original rejected: %v %v", ok, err)
	}
}

func TestOriginalCacheRecoveryAcceptsExactAuthenticatedReplay(t *testing.T) {
	s, w, _ := originalCacheRecoveryFixture(t)
	port := originalCacheRecoveryPort(t, s)
	if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET active=1,protocol_reference=? WHERE id=?`, w.ProtocolReference, w.MediaID); err != nil {
		t.Fatal(err)
	}
	if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || !ok {
		t.Fatalf("exact replay rejected: %v %v", ok, err)
	}
}

func TestOriginalCacheRecoveryRejectsCurrentBarriers(t *testing.T) {
	mutations := map[string]func(*testing.T, *Store, OriginalCacheWitness){
		"deleted": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`UPDATE messages SET state='deleted' WHERE id=?`, w.Source.Version.MessageID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"view once": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`UPDATE messages SET view_once=1 WHERE id=?`, w.Source.Version.MessageID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"conflict barrier": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`UPDATE messages SET revision_barrier='source_revision_conflict' WHERE id=?`, w.Source.Version.MessageID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"source tombstone": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`INSERT INTO source_tombstones(account,chat,sender,message,from_me,protocol_revision) VALUES(?,?,?,?,?,1)`, w.Key.Account, w.Key.Chat, w.Key.Sender, w.Key.Message, w.Key.FromMe)
			if err != nil {
				t.Fatal(err)
			}
		},
		"chat tombstone": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`INSERT INTO chat_tombstones(account,chat,cutoff) VALUES(?,?,?)`, w.Key.Account, w.Key.Chat, unixms(s.now()))
			if err != nil {
				t.Fatal(err)
			}
		},
		"identity conflict": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`INSERT INTO sync_gaps(account,chat,code) VALUES(?,?,'identity_alias_conflict')`, w.Key.Account, w.Key.Chat)
			if err != nil {
				t.Fatal(err)
			}
		},
		"cleanup acknowledged": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`UPDATE extension_media_artifacts SET cleanup_ack=1 WHERE id=?`, w.ArtifactID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"missing charge": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`UPDATE extension_media_artifacts SET charged_bytes=0 WHERE id=?`, w.ArtifactID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"changed completion": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET completed_sha256=? WHERE media_id=?`, strings.Repeat("b", 64), w.MediaID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"download owner": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET state='leased',lease_id='current-owner',lease_until=? WHERE media_id=?`, unixms(s.now().Add(time.Minute)), w.MediaID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"decoded owner": func(t *testing.T, s *Store, w OriginalCacheWitness) {
			_, err := s.writeDB.Exec(`INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,created_at,updated_at,input_artifact_id) SELECT 'decoded-original-recovery','decoded-reservation',media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,'decoded_work',128,128,'decoded-lease',lease_until,'reserved',created_at,updated_at,id FROM extension_media_artifacts WHERE id=?`, w.ArtifactID)
			if err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s, w, _ := originalCacheRecoveryFixture(t)
			port := originalCacheRecoveryPort(t, s)
			mutate(t, s, w)
			before := originalCacheRecoveryState(t, s, w)
			if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || ok {
				t.Fatalf("barrier accepted: %v %v", ok, err)
			}
			if originalCacheRecoveryState(t, s, w) != before {
				t.Fatal("denied repair mutated metadata")
			}
		})
	}
}

func TestOriginalCacheRecoveryRequiresDurableLegacyIdentity(t *testing.T) {
	s, w, _ := originalCacheRecoveryFixture(t)
	port := originalCacheRecoveryPort(t, s)
	w.DescriptorSHA256, w.ProtocolSHA256, w.LegacyIdentity = "", "", w.MediaIdentity
	if _, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET descriptor_sha256='',protocol_sha256='',legacy_source_media_identity=? WHERE media_id=?`, w.LegacyIdentity, w.MediaID); err != nil {
		t.Fatal(err)
	}
	if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || !ok {
		t.Fatalf("valid legacy evidence rejected: %v %v", ok, err)
	}
	s, w, _ = originalCacheRecoveryFixture(t)
	port = originalCacheRecoveryPort(t, s)
	w.DescriptorSHA256, w.ProtocolSHA256, w.LegacyIdentity = "", "", ""
	if _, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET descriptor_sha256='',protocol_sha256='',legacy_source_media_identity='' WHERE media_id=?`, w.MediaID); err != nil {
		t.Fatal(err)
	}
	if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || ok {
		t.Fatalf("missing legacy evidence accepted: %v %v", ok, err)
	}
}

func TestOriginalCacheRecoveryDoesNotClearSafety(t *testing.T) {
	s, w, _ := originalCacheRecoveryFixture(t)
	port := originalCacheRecoveryPort(t, s)
	s.denyImmediate(w.Source.Account, "drain_failed")
	if ok, err := port.RecoverOriginalCache(context.Background(), w); ok || !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("unsafe recovery accepted: %v %v", ok, err)
	}
}

type originalCacheBlockingTestCommand struct{ entered, release chan struct{} }

func (c originalCacheBlockingTestCommand) priority() bool { return false }
func (c originalCacheBlockingTestCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}
func (c originalCacheBlockingTestCommand) account() string { return "" }
func (c originalCacheBlockingTestCommand) run(_ *Store, _ context.Context) (any, error) {
	close(c.entered)
	<-c.release
	return nil, nil
}

func TestOriginalCacheRecoveryCancellationRetainsQueuedReference(t *testing.T) {
	s, w, _ := originalCacheRecoveryFixture(t)
	port := originalCacheRecoveryPort(t, s)
	block := originalCacheBlockingTestCommand{entered: make(chan struct{}), release: make(chan struct{})}
	blocked := make(chan error, 1)
	go func() { _, err := s.submitExtension(context.Background(), block, 64); blocked <- err }()
	select {
	case <-block.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not enter barrier")
	}
	released := false
	defer func() {
		if !released {
			close(block.release)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := port.RecoverOriginalCache(ctx, w); result <- err }()
	var queued command
	select {
	case queued = <-s.bodies:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery was not admitted")
	}
	// Hold the actual admitted command while the submitting caller returns.
	// Caller cleanup must not overwrite queue-owned protobuf input.
	defer func() { s.bodies <- queued }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	owned, ok := queued.payload.(originalCacheRecoveryCommand)
	if !ok || !bytes.Equal(owned.witness.ProtocolReference, w.ProtocolReference) {
		t.Fatal("cancelled caller erased admitted reference")
	}
	close(block.release)
	released = true
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
}

func TestOriginalCacheRecoveryRejectsDeletionAliasAndExtractionPins(t *testing.T) {
	t.Run("unresolved deletion", func(t *testing.T) {
		s, w, _ := originalCacheRecoveryFixture(t)
		port := originalCacheRecoveryPort(t, s)
		if _, err := s.writeDB.Exec(`INSERT INTO unresolved_deletions(account,chat,message) VALUES(?,?,?)`, w.Key.Account, w.Key.Chat, w.Key.Message); err != nil {
			t.Fatal(err)
		}
		if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || ok {
			t.Fatalf("unresolved deletion accepted: %v %v", ok, err)
		}
	})
	t.Run("canonical source changed", func(t *testing.T) {
		s, w, _ := originalCacheRecoveryFixture(t)
		port := originalCacheRecoveryPort(t, s)
		if _, err := s.writeDB.Exec(`INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES(?,?,?,'connector_mapping')`, w.Key.Account, w.Key.Chat, "other-canonical-chat"); err != nil {
			t.Fatal(err)
		}
		if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || ok {
			t.Fatalf("changed canonical source accepted: %v %v", ok, err)
		}
	})
	t.Run("extraction pinned", func(t *testing.T) {
		s, w, _ := originalCacheRecoveryFixture(t)
		port := originalCacheRecoveryPort(t, s)
		if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET active=1,protocol_reference=?,artifact_id=?,availability='retained' WHERE id=?`, w.ProtocolReference, w.ArtifactID, w.MediaID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec(`UPDATE extension_media_artifacts SET state='retained' WHERE id=?`, w.ArtifactID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: w.Source.Account, Version: extractionTestVersion(JobTranscription), TTL: time.Minute}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET active=0,protocol_reference=X'',artifact_id='' WHERE id=?`, w.MediaID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec(`UPDATE extension_media_artifacts SET state='evicting' WHERE id=?`, w.ArtifactID); err != nil {
			t.Fatal(err)
		}
		if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || ok {
			t.Fatalf("pinned original accepted: %v %v", ok, err)
		}
	})
}

func TestReadOriginalCacheWitnessesKeepsCompatibleStoppedBackupUnchanged(t *testing.T) {
	for _, version := range []int{10, 11, 12, 13, 14} {
		t.Run(fmt.Sprintf("version %d", version), func(t *testing.T) {
			s, _, dir := originalDownloadTestArchive(t)
			p := testPolicy(t, s)
			e, ref, media := originalDownloadTestSource(t, s, p, "stopped-backup", MessageVoice)
			r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
			if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
				t.Fatalf("publication: %v %v", ok, err)
			}
			// Remove the later empty schema as well as its migration records. Reading
			// an older stopped backup must not invoke Open or recreate these tables.
			originalCacheTestEarlierSchema(t, s, version)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "archive.sqlite")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			witnesses, err := ReadOriginalCacheWitnesses(context.Background(), dir)
			if err != nil || len(witnesses) != 1 {
				t.Fatalf("stopped backup: %d %v", len(witnesses), err)
			}
			w := witnesses[0]
			if w.Source != ref || w.Key != e.Key || w.MediaID != media || w.ArtifactID != r.ArtifactID || !bytes.Equal(w.ProtocolReference, e.Media[0].ProtocolReference) {
				t.Fatal("backup witness rebound source")
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatalf("backup bytes changed: %v", err)
			}
			if _, err := os.Lstat(path + "-wal"); !os.IsNotExist(err) {
				t.Fatalf("reader created WAL: %v", err)
			}
			if _, err := os.Lstat(path + "-shm"); !os.IsNotExist(err) {
				t.Fatalf("reader created shared memory: %v", err)
			}
		})
	}
}

func originalCacheTestEarlierSchema(t *testing.T, s *Store, version int) {
	t.Helper()
	var statements []string
	if version < 14 {
		statements = append(statements, `DROP TABLE extension_document_promotions`, `ALTER TABLE extension_media_associations DROP COLUMN filename`)
	}
	if version < 13 {
		statements = append(statements, `DROP TABLE send_permission_modes`)
	}
	if version < 12 {
		statements = append(statements, `DROP TABLE send_requests`, `DROP TABLE send_permissions`)
	}
	if version < 11 {
		statements = append(statements, `DROP TABLE snapshot_recovery_audit`, `ALTER TABLE archive_meta DROP COLUMN recovery_epoch`, `ALTER TABLE archive_meta DROP COLUMN recovery_code`)
	}
	if version < 10 {
		statements = append(statements, `DROP TABLE unresolved_deletions`)
	}
	for _, statement := range statements {
		if _, err := s.writeDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.writeDB.Exec(`DELETE FROM schema_migrations WHERE version>?`, version); err != nil {
		t.Fatal(err)
	}
}

func TestReadOriginalCacheWitnessesRefusesUnqualifiedSchemaWithoutMutation(t *testing.T) {
	for _, name := range []string{"too old", "gap", "altered checksum", "future"} {
		t.Run(name, func(t *testing.T) {
			s, _, dir := originalDownloadTestArchive(t)
			testPolicy(t, s)
			want := ErrSchemaChanged
			var statement string
			switch name {
			case "too old":
				originalCacheTestEarlierSchema(t, s, 9)
			case "gap":
				statement = `DELETE FROM schema_migrations WHERE version=3`
			case "altered checksum":
				statement = `UPDATE schema_migrations SET checksum='changed' WHERE version=1`
			case "future":
				statement = `INSERT INTO schema_migrations(version,checksum) SELECT max(version)+1,'future' FROM schema_migrations`
				want = ErrSchemaTooNew
			}
			if statement != "" {
				if _, err := s.writeDB.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "archive.sqlite")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if witnesses, err := ReadOriginalCacheWitnesses(context.Background(), dir); !errors.Is(err, want) || len(witnesses) != 0 {
				t.Fatalf("unqualified backup accepted: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatalf("refused backup bytes changed: %v", err)
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
					t.Fatalf("refused backup created sidecar %s: %v", suffix, err)
				}
			}
		})
	}
}

func TestReadOriginalCacheWitnessesRejectsUnsafeAndUncheckpointedBackup(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = ReadOriginalCacheWitnesses(context.Background(), filepath.Join(dir, "absent")); !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("missing backup initialized: %v", err)
		}
	})
	t.Run("nonempty WAL", func(t *testing.T) {
		s, _, dir := originalDownloadTestArchive(t)
		testPolicy(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		wal := filepath.Join(dir, "archive.sqlite-wal")
		if err := os.WriteFile(wal, []byte("uncheckpointed committed evidence"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadOriginalCacheWitnesses(context.Background(), dir); !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("nonempty WAL ignored: %v", err)
		}
		body, err := os.ReadFile(wal)
		if err != nil || string(body) != "uncheckpointed committed evidence" {
			t.Fatalf("WAL changed: %v", err)
		}
	})
}

func TestOriginalCacheRecoveryOpenPreservesExpiredCommittedRows(t *testing.T) {
	s, clock, dir := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	e := event("expired-recovery-original", "expiredcachepreservation", 1, EventOriginal)
	e.ExpiresAt = fromMillis(clock.Load()).Add(time.Minute)
	r := apply(t, s, e, p.Generation)
	// Make the persisted row expired against the preservation factory's actual
	// clock, independently of the fixed synthetic ingestion clock.
	if _, err := s.writeDB.Exec(`UPDATE messages SET expires_at=? WHERE id=?`, unixms(time.Now().UTC().Add(-time.Minute)), r.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	recovery, err := OpenOriginalCacheRecovery(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	var body, state string
	var fts int
	if err := recovery.readDB.QueryRow(`SELECT body,state,(SELECT count(*) FROM message_fts WHERE rowid=messages.rowid) FROM messages WHERE id=?`, r.MessageID).Scan(&body, &state, &fts); err != nil || body != e.Body || state != "active" || fts != 1 {
		t.Fatalf("offline open changed expired committed row: %v", err)
	}
	// Deferred physical expiry does not grant expired source authority.
	if _, err := recovery.CurrentSource(context.Background(), e.Key); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("expired row authorized: %v", err)
	}
	if pending, err := recovery.sweepExpired(context.Background()); err != nil || pending {
		t.Fatalf("maintenance expiry changed preservation session: %v %v", pending, err)
	}
	if err := recovery.readDB.QueryRow(`SELECT body,state FROM messages WHERE id=?`, r.MessageID).Scan(&body, &state); err != nil || body != e.Body || state != "active" {
		t.Fatalf("maintenance expired preserved row: %v", err)
	}
}

func TestOriginalCacheRecoveryOpenRefusesMissingOldOrChangedSchema(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(dir, "missing")
		if s, err := OpenOriginalCacheRecovery(context.Background(), missing); err == nil || s != nil {
			if s != nil {
				s.Close()
			}
			t.Fatal("missing recovery profile initialized")
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Fatalf("missing root created: %v", err)
		}
	})
	for _, name := range []string{"old schema", "migration gap", "altered checksum", "future"} {
		t.Run(name, func(t *testing.T) {
			s, _, dir := originalDownloadTestArchive(t)
			testPolicy(t, s)
			want := ErrSchemaChanged
			statement := `DELETE FROM schema_migrations WHERE version=(SELECT max(version) FROM schema_migrations)`
			if name == "migration gap" {
				statement = `DELETE FROM schema_migrations WHERE version=11`
			} else if name == "altered checksum" {
				statement = `UPDATE schema_migrations SET checksum='changed' WHERE version=1`
			} else if name == "future" {
				statement = `INSERT INTO schema_migrations(version,checksum) SELECT max(version)+1,'future' FROM schema_migrations`
				want = ErrSchemaTooNew
			}
			if _, err := s.writeDB.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "archive.sqlite")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if recovery, err := OpenOriginalCacheRecovery(context.Background(), dir); !errors.Is(err, want) || recovery != nil {
				if recovery != nil {
					recovery.Close()
				}
				t.Fatalf("unqualified schema opened for writes: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatalf("refused profile bytes changed: %v", err)
			}
		})
	}
}

func TestOriginalCacheRecoveryOpenRefusesSafetyWithoutMutation(t *testing.T) {
	s, _, dir := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("unsafe-offline-cache", "synthetic unsafe preserved body", 1, EventOriginal), p.Generation)
	if _, err := s.writeDB.Exec(`UPDATE account_safety SET unsafe=1,reason='drain_failed',epoch=epoch+1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive.sqlite")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if recovery, err := OpenOriginalCacheRecovery(context.Background(), dir); !errors.Is(err, ErrReconciliationRequired) || recovery != nil {
		if recovery != nil {
			recovery.Close()
		}
		t.Fatalf("unsafe preservation open: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatalf("unsafe archive mutated: %v", err)
	}
}

func TestOriginalCacheRecoveryRejectsMalformedClearedReference(t *testing.T) {
	for _, reference := range []any{"uncleared text reference", bytes.Repeat([]byte{'x'}, 8193)} {
		s, w, _ := originalCacheRecoveryFixture(t)
		port := originalCacheRecoveryPort(t, s)
		if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET protocol_reference=? WHERE id=?`, reference, w.MediaID); err != nil {
			t.Fatal(err)
		}
		before := originalCacheRecoveryState(t, s, w)
		if ok, err := port.RecoverOriginalCache(context.Background(), w); err != nil || ok {
			t.Fatalf("malformed cleared reference accepted: %v %v", ok, err)
		}
		if originalCacheRecoveryState(t, s, w) != before {
			t.Fatal("malformed denial changed state")
		}
	}
}

func TestOriginalCacheRecoveryOpenRefusesMarkersBeforeCheckpoint(t *testing.T) {
	for _, marker := range []string{"observation.marker", "unsafe.marker"} {
		t.Run(marker, func(t *testing.T) {
			s, _, dir := originalDownloadTestArchive(t)
			p := testPolicy(t, s)
			apply(t, s, event("marker-preservation", "synthetic committed marker body", 1, EventOriginal), p.Generation)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "archive.sqlite")
			uri := url.URL{Scheme: "file", Path: path}
			db, err := sql.Open("sqlite3", uri.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE archive_meta SET archive_revision=archive_revision+1`); err != nil {
				t.Fatal(err)
			}
			wal := path + "-wal"
			beforeDB, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeWAL, err := os.ReadFile(wal)
			if err != nil || len(beforeWAL) == 0 {
				t.Fatalf("fixture has no committed WAL: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, marker), []byte("synthetic owner safety evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			if recovery, err := OpenOriginalCacheRecovery(context.Background(), dir); !errors.Is(err, ErrReconciliationRequired) || recovery != nil {
				if recovery != nil {
					recovery.Close()
				}
				t.Fatalf("marker preflight refusal: %v", err)
			}
			afterDB, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(beforeDB) != sha256.Sum256(afterDB) {
				t.Fatalf("refused marker checkpointed database: %v", err)
			}
			afterWAL, err := os.ReadFile(wal)
			if err != nil || sha256.Sum256(beforeWAL) != sha256.Sum256(afterWAL) {
				t.Fatalf("refused marker changed WAL: %v", err)
			}
		})
	}
}

func TestOriginalCacheRecoveryOpenPreservesMissingAndPendingFence(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(map[bool]string{true: "missing final", false: "pending preserved"}[missing], func(t *testing.T) {
			s, _, dir := originalDownloadTestArchive(t)
			p := testPolicy(t, s)
			apply(t, s, event("fence-preservation", "synthetic client fence body", 1, EventOriginal), p.Generation)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			final := filepath.Join(dir, credentialFenceMarker)
			pending := final + ".pending"
			if err := os.WriteFile(pending, []byte("v1\n*\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := os.Remove(final); err != nil {
					t.Fatal(err)
				}
			}
			beforePending, err := os.ReadFile(pending)
			if err != nil {
				t.Fatal(err)
			}
			var beforeFinal []byte
			if !missing {
				beforeFinal, err = os.ReadFile(final)
				if err != nil {
					t.Fatal(err)
				}
			}
			recovery, err := OpenOriginalCacheRecovery(context.Background(), dir)
			if missing {
				if !errors.Is(err, ErrUnsafeDataPath) || recovery != nil {
					if recovery != nil {
						recovery.Close()
					}
					t.Fatalf("missing final fence refusal: %v", err)
				}
				if _, err := os.Lstat(final); !os.IsNotExist(err) {
					t.Fatalf("missing final fence was allocated: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !recovery.credentialFence.allDenied {
					t.Fatal("pending client denial was lost")
				}
				if err := recovery.Close(); err != nil {
					t.Fatal(err)
				}
				afterFinal, err := os.ReadFile(final)
				if err != nil || !bytes.Equal(beforeFinal, afterFinal) {
					t.Fatalf("final fence changed: %v", err)
				}
			}
			afterPending, err := os.ReadFile(pending)
			if err != nil || !bytes.Equal(beforePending, afterPending) {
				t.Fatalf("pending fence changed: %v", err)
			}
		})
	}
}
