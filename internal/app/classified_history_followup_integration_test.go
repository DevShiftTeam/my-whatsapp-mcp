package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func classifiedFollowupFixture(t *testing.T) (classifiedIntegrationFixture, ClassifiedHistoryRecoveryConfig, ClassifiedHistoryEvidence) {
	t.Helper()
	ctx := context.Background()
	f := classifiedIntegrationProfile(t, false)
	first, err := RecoverClassifiedHistorySnapshot(ctx, f.config)
	if err != nil {
		t.Fatal("initial classified33 recovery", err)
	}
	// Model the actual previous release: its completed binding occupied one
	// fixed filename. Preserve those exact bytes across the next acceptance.
	legacy := filepath.Join(f.paths.WorkingDir, classifiedHistoryBindingFile)
	if _, err := os.Stat(legacy); os.IsNotExist(err) {
		bindings := classifiedFollowupBindings(t, f.paths.WorkingDir)
		if len(bindings) != 1 {
			t.Fatal("initial recovery did not create exactly one binding")
		}
		for path := range bindings {
			if err := os.Rename(path, legacy); err != nil {
				t.Fatal(err)
			}
		}
	}
	legacyBytes, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	f.preserved[legacy] = legacyBytes
	if again, err := RecoverClassifiedHistorySnapshot(ctx, f.config); err != nil || again != first {
		t.Fatal("legacy fixed binding cannot replay its exact completed33 witness", again, err)
	}
	// Evolve only the stopped synthetic session. Follow-up recovery must retain
	// this latest file, rather than restoring the original bootstrap backup.
	sessionPath := filepath.Join(f.paths.SessionDir, "session.sqlite")
	address := url.URL{Scheme: "file", Path: sessionPath}
	address.RawQuery = url.Values{"mode": {"rw"}, "_foreign_keys": {"on"}}.Encode()
	session, err := sql.Open("sqlite3", address.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Exec("CREATE TABLE synthetic_followup_session(epoch INTEGER NOT NULL); INSERT INTO synthetic_followup_session VALUES(2)"); err != nil {
		session.Close()
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	evolved, err := os.ReadFile(sessionPath)
	if err != nil || bytes.Equal(evolved, f.preserved[sessionPath]) {
		t.Fatal("fixture did not evolve the saved session", err)
	}
	f.preserved[sessionPath] = evolved
	store, err := archive.Open(ctx, f.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginObservation(ctx, f.bootstrap.ExpectedAccountID); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.MarkUnsafe(ctx, f.bootstrap.ExpectedAccountID, "history_corrupt"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, e := syntheticClassifiedEvidence(t)
	cfg.DataDir = f.paths.Root
	e.ReadySHA256 = f.evidence.ReadySHA256
	e.Witness.RebuildGeneration = f.evidence.Witness.RebuildGeneration
	e.Witness.Classification = archive.ClassifiedHistoryChangeUsername
	e.Witness.Diagnostic = archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 193, StubPresent: true, StubKnown: true, Presence: 411}
	log := []byte(`{"event":"history_failure","stage":"record_body_missing","stub":"other_stub","stub_type":193,"raw_stub":193,"stub_present":true,"stub_known":true,"presence":411}` + "\n")
	if err := os.WriteFile(e.DiagnosticLogPath, log, 0600); err != nil {
		t.Fatal(err)
	}
	e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(log)
	db := classifiedIntegrationDB(t, f.paths)
	if err := db.QueryRow("SELECT epoch FROM account_safety WHERE account=?", f.bootstrap.ExpectedAccountID).Scan(&e.Witness.UnsafeEpoch); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1").Scan(&e.Witness.ArchiveRevision, &e.Witness.RecoveryEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if e.Witness.UnsafeEpoch <= f.evidence.Witness.UnsafeEpoch || e.Witness.RecoveryEpoch != first.RecoveryEpoch || e.Witness.ArchiveRevision != uint64(first.ArchiveRevision) {
		t.Fatal("fixture did not create a new fence after completed recovery")
	}
	e.Witness.OriginalArchiveSHA256, err = classifiedArchiveHash(ctx, filepath.Join(f.paths.ArchiveDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	e.Witness.UnsafeMarkerSHA256, e.Witness.UnsafeMarkerIdentity = classifiedIntegrationMarker(t, filepath.Join(f.paths.ArchiveDir, "unsafe.marker"))
	e.Witness.ObservationMarkerSHA256, e.Witness.ObservationMarkerIdentity = classifiedIntegrationMarker(t, filepath.Join(f.paths.ArchiveDir, "observation.marker"))
	classifiedIntegrationResign(t, &cfg, &e)
	return f, cfg, e
}

func classifiedFollowupFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = raw
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func classifiedFollowupBindings(t *testing.T, working string) map[string][]byte {
	t.Helper()
	bindings := map[string][]byte{}
	for path, raw := range classifiedFollowupFiles(t, working) {
		var binding classifiedHistoryBinding
		if json.Unmarshal(raw, &binding) == nil && binding.Version == 1 && binding.Witness.EvidenceSHA256 != "" {
			bindings[path] = raw
		}
	}
	return bindings
}

func classifiedFollowupAssertExistingFiles(t *testing.T, before map[string][]byte) {
	t.Helper()
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("rejected witness changed a preexisting file %s: %v", path, err)
		}
	}
}

func TestClassifiedHistoryFollowupPreservesLegacyBindingAndLatestSession(t *testing.T) {
	f, cfg, e := classifiedFollowupFixture(t)
	ctx := context.Background()
	before := classifiedFollowupFiles(t, f.paths.Root)
	if _, err := RecoverClassifiedHistorySnapshot(ctx, f.config); err == nil {
		t.Fatal("old33 evidence accepted the newer193 fence")
	}
	classifiedFollowupAssertExistingFiles(t, before)
	got, err := RecoverClassifiedHistorySnapshot(ctx, cfg)
	if err != nil {
		t.Fatal("fresh193 follow-up recovery", err)
	}
	if got.Mode != "committed_snapshot" || got.Code != "owner_restored_snapshot" || got.RecoveryEpoch != e.Witness.RecoveryEpoch+1 || uint64(got.ArchiveRevision) != e.Witness.ArchiveRevision+1 {
		t.Fatal("incorrect second partial acceptance", got)
	}
	f.assertPreserved(t)
	canonical, err := readClassifiedHistoryEvidence(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bindings := classifiedFollowupBindings(t, f.paths.WorkingDir)
	if len(bindings) != 2 {
		t.Fatal("follow-up did not retain two independent evidence bindings", len(bindings))
	}
	found := false
	for path, raw := range bindings {
		var binding classifiedHistoryBinding
		if err := json.Unmarshal(raw, &binding); err != nil {
			t.Fatal(err)
		}
		if binding.Witness == canonical.Witness {
			found = strings.Contains(filepath.Base(path), canonical.Witness.EvidenceSHA256)
		}
	}
	if !found {
		t.Fatal("follow-up binding filename does not include canonical evidence digest")
	}
	if again, err := RecoverClassifiedHistorySnapshot(ctx, cfg); err != nil || again != got {
		t.Fatal("identical193 retry failed", again, err)
	}
	if !reflect.DeepEqual(bindings, classifiedFollowupBindings(t, f.paths.WorkingDir)) {
		t.Fatal("identical retry changed append-only evidence bindings")
	}
	if _, err := RecoverClassifiedHistorySnapshot(ctx, f.config); err == nil {
		t.Fatal("completed second recovery accepted the old first witness")
	}
	f.assertPreserved(t)
}

func TestClassifiedHistoryFollowupRejectsMismatchedCurrentWitnessWithoutMutation(t *testing.T) {
	for _, kind := range []string{"unsafe_epoch", "archive_revision", "recovery_epoch", "current_archive", "diagnostic"} {
		t.Run(kind, func(t *testing.T) {
			f, cfg, e := classifiedFollowupFixture(t)
			currentWitness := e
			switch kind {
			case "unsafe_epoch":
				e.Witness.UnsafeEpoch--
			case "archive_revision":
				e.Witness.ArchiveRevision--
			case "recovery_epoch":
				e.Witness.RecoveryEpoch--
			case "current_archive":
				e.Witness.OriginalArchiveSHA256 = strings.Repeat("0", 64)
			case "diagnostic":
				e.Witness.Diagnostic.Presence = 475
			}
			classifiedIntegrationResign(t, &cfg, &e)
			before := classifiedFollowupFiles(t, f.paths.Root)
			_, markerBefore := classifiedIntegrationMarker(t, filepath.Join(f.paths.ArchiveDir, "unsafe.marker"))
			if _, err := RecoverClassifiedHistorySnapshot(context.Background(), cfg); err == nil {
				t.Fatal("mismatched current witness accepted", kind)
			}
			// A new durable seal may precede archive CAS rejection. Every
			// preexisting byte, including archive and old seals, must survive.
			classifiedFollowupAssertExistingFiles(t, before)
			_, markerAfter := classifiedIntegrationMarker(t, filepath.Join(f.paths.ArchiveDir, "unsafe.marker"))
			if markerAfter != markerBefore {
				t.Fatal("rejected witness replaced the unsafe marker")
			}
			if kind == "unsafe_epoch" {
				// Change only this stopped synthetic archive's physical bytes.
				// The stale witness's seal must not grant another witness a
				// waiver of its original-archive digest check.
				db := classifiedIntegrationDB(t, f.paths)
				if _, err := db.Exec("CREATE TABLE synthetic_post_rejection_change(value INTEGER); INSERT INTO synthetic_post_rejection_change VALUES(1)"); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				digest, err := classifiedArchiveHash(context.Background(), filepath.Join(f.paths.ArchiveDir, "archive.sqlite"))
				if err != nil || digest == currentWitness.Witness.OriginalArchiveSHA256 {
					t.Fatal("fixture did not change physical archive bytes", err)
				}
				changedBefore := classifiedFollowupFiles(t, f.paths.Root)
				if _, err := RecoverClassifiedHistorySnapshot(context.Background(), cfg); err == nil {
					t.Fatal("rejected stale seal became authoritative after physical archive change")
				}
				classifiedFollowupAssertExistingFiles(t, changedBefore)
				classifiedIntegrationResign(t, &cfg, &currentWitness)
				if _, err := RecoverClassifiedHistorySnapshot(context.Background(), cfg); err == nil {
					t.Fatal("different witness reused a rejected seal to waive its archive digest")
				}
				classifiedFollowupAssertExistingFiles(t, changedBefore)
			}
			f.assertPreserved(t)
		})
	}
}
