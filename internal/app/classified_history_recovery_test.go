package app

import (
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func syntheticClassifiedEvidence(t *testing.T) (ClassifiedHistoryRecoveryConfig, ClassifiedHistoryEvidence) {
	t.Helper()
	root := t.TempDir()
	os.Chmod(root, 0700)
	cfg := ClassifiedHistoryRecoveryConfig{DataDir: filepath.Join(root, "profile"), EvidencePath: filepath.Join(root, "evidence.json")}
	e := ClassifiedHistoryEvidence{ReadySHA256: strings.Repeat("1", 64), DiagnosticLogPath: filepath.Join(root, "diagnostic.log"), LaunchEvidencePath: filepath.Join(root, "launch.json"), OperatorReceiptPath: filepath.Join(root, "operator.json")}
	w := archive.ClassifiedHistoryWitness{Version: 1, Classification: archive.ClassifiedHistoryGroupParticipantChangeNumber, RebuildGeneration: strings.Repeat("a", 32), UnsafeEpoch: 1, ArchiveRevision: 4, OriginalArchiveSHA256: strings.Repeat("2", 64), RuntimeBuildSHA: strings.Repeat("3", 40), RuntimeBinarySHA256: strings.Repeat("4", 64), RuntimePID: 123, LaunchEvidenceKind: "owner_attested_launch_interval_v1", LaunchActionRecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ProcessObservedAt: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), StoppedAt: time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC), UnsafeMarkerSHA256: strings.Repeat("5", 64), UnsafeMarkerIdentity: "synthetic", Diagnostic: archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 33, StubPresent: true, StubKnown: true, Presence: 475}}
	write := func(path string, value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return liveRebuildDigest(raw)
	}
	log := []byte(`{"event":"history_failure","stage":"record_body_missing","stub":"other_stub","stub_type":33,"raw_stub":33,"stub_present":true,"stub_known":true,"presence":475}` + "\n")
	os.WriteFile(e.DiagnosticLogPath, log, 0600)
	w.DiagnosticLogSHA256 = liveRebuildDigest(log)
	readPath, exitPath := filepath.Join(root, "process-read.json"), filepath.Join(root, "joined-exit.json")
	w.ProcessReadReceiptSHA256 = write(readPath, map[string]any{"pid": w.RuntimePID, "observed_at": w.ProcessObservedAt})
	w.JoinedExitReceiptSHA256 = write(exitPath, map[string]any{"pid": w.RuntimePID, "stopped_at": w.StoppedAt})

	artifacts := map[string]ClassifiedHistoryLaunchArtifact{}
	for _, role := range []string{"supervisor", "launch_manifest", "plist", "launch_action", "binary"} {
		path := filepath.Join(root, role)
		raw := []byte("synthetic-" + role)
		os.WriteFile(path, raw, 0600)
		artifacts[role] = ClassifiedHistoryLaunchArtifact{path, liveRebuildDigest(raw)}
	}
	w.RuntimeBinarySHA256 = artifacts["binary"].SHA256
	w.LaunchEvidenceSHA256 = write(e.LaunchEvidencePath, ClassifiedHistoryLaunchEvidence{Artifacts: artifacts, Version: 1, DataDir: cfg.DataDir, ReadySHA256: e.ReadySHA256, RuntimeBuildSHA: w.RuntimeBuildSHA, RuntimeBinarySHA256: w.RuntimeBinarySHA256, RuntimePID: w.RuntimePID, LaunchEvidenceKind: w.LaunchEvidenceKind, LaunchActionRecordedAt: w.LaunchActionRecordedAt.Format(time.RFC3339Nano), ProcessObservedAt: w.ProcessObservedAt.Format(time.RFC3339Nano), StoppedAt: w.StoppedAt.Format(time.RFC3339Nano), ProcessReadReceiptPath: readPath, JoinedExitReceiptPath: exitPath})
	w.OperatorReceiptSHA256 = write(e.OperatorReceiptPath, ClassifiedHistoryOperatorReceipt{1, true, true, w.LaunchEvidenceSHA256, w.DiagnosticLogSHA256, w.OriginalArchiveSHA256, w.RebuildGeneration, w.UnsafeEpoch, w.ArchiveRevision, w.RecoveryEpoch, w.AcceptUnknownHistoryGaps})
	e.Witness = w
	cfg.ExpectedEvidenceSHA256 = write(cfg.EvidencePath, e)
	return cfg, e
}
func TestClassifiedHistoryEvidenceRequiresExactAttestedDiagnostic(t *testing.T) {
	cfg, _ := syntheticClassifiedEvidence(t)
	e, err := readClassifiedHistoryEvidence(cfg)
	if err != nil || e.Witness.Diagnostic.RawStub != 33 {
		t.Fatal("exact owner evidence refused")
	}
	for _, kind := range []string{"digest", "duplicate", "unknown", "unknown_enum", "safety", "ambiguous_log", "launch_mismatch", "operator_denial", "permissions", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg, e := syntheticClassifiedEvidence(t)
			switch kind {
			case "digest":
				cfg.ExpectedEvidenceSHA256 = strings.Repeat("0", 64)
			case "duplicate":
				raw, _ := os.ReadFile(cfg.EvidencePath)
				raw = append([]byte(`{"ready_sha256":"wrong",`), raw[1:]...)
				os.WriteFile(cfg.EvidencePath, raw, 0600)
				cfg.ExpectedEvidenceSHA256 = liveRebuildDigest(raw)
			case "unknown":
				raw, _ := os.ReadFile(cfg.EvidencePath)
				raw = append([]byte(`{"force":true,`), raw[1:]...)
				os.WriteFile(cfg.EvidencePath, raw, 0600)
				cfg.ExpectedEvidenceSHA256 = liveRebuildDigest(raw)
			case "permissions":
				os.Chmod(cfg.EvidencePath, 0644)
			case "symlink":
				os.Rename(cfg.EvidencePath, cfg.EvidencePath+".target")
				os.Symlink(cfg.EvidencePath+".target", cfg.EvidencePath)
			default:
				switch kind {
				case "unknown_enum":
					e.Witness.Diagnostic.RawStub = -1
				case "safety":
					e.Witness.Diagnostic.Presence |= 512
				case "ambiguous_log":
					raw, _ := os.ReadFile(e.DiagnosticLogPath)
					raw = append(raw, raw...)
					os.WriteFile(e.DiagnosticLogPath, raw, 0600)
					e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(raw)
				case "launch_mismatch":
					os.WriteFile(e.LaunchEvidencePath, []byte(`{}`), 0600)
				case "operator_denial":
					os.WriteFile(e.OperatorReceiptPath, []byte(`{}`), 0600)
				}
				raw, _ := json.Marshal(e)
				os.WriteFile(cfg.EvidencePath, raw, 0600)
				cfg.ExpectedEvidenceSHA256 = liveRebuildDigest(raw)
			}
			if _, err := readClassifiedHistoryEvidence(cfg); err == nil {
				t.Fatal("unsafe evidence admitted")
			}
		})
	}
}

func TestClassifiedHistoryEvidenceRejectsAmbiguousPinnedLog(t *testing.T) {
	cfg, e := syntheticClassifiedEvidence(t)
	raw, _ := os.ReadFile(e.DiagnosticLogPath)
	raw = append(raw, raw...)
	os.WriteFile(e.DiagnosticLogPath, raw, 0600)
	e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(raw)
	w := e.Witness
	op := ClassifiedHistoryOperatorReceipt{1, true, true, w.LaunchEvidenceSHA256, w.DiagnosticLogSHA256, w.OriginalArchiveSHA256, w.RebuildGeneration, w.UnsafeEpoch, w.ArchiveRevision, w.RecoveryEpoch, w.AcceptUnknownHistoryGaps}
	raw, _ = json.Marshal(op)
	os.WriteFile(e.OperatorReceiptPath, raw, 0600)
	e.Witness.OperatorReceiptSHA256 = liveRebuildDigest(raw)
	raw, _ = json.Marshal(e)
	os.WriteFile(cfg.EvidencePath, raw, 0600)
	cfg.ExpectedEvidenceSHA256 = liveRebuildDigest(raw)
	if _, err := readClassifiedHistoryEvidence(cfg); err == nil {
		t.Fatal("two fully hash-bound occurrences accepted as one current fault")
	}
}
func TestClassifiedHistoryArtifactOwnerOnlySingleLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(path, []byte("synthetic-binary"), 0700)
	if _, err := classifiedArtifactHash(path); err != nil {
		t.Fatal("private executable refused")
	}
	os.Link(path, path+".hardlink")
	if _, err := classifiedArtifactHash(path); err == nil {
		t.Fatal("aliased binary evidence accepted")
	}
	os.Remove(path + ".hardlink")
	os.Chmod(path, 0755)
	if _, err := classifiedArtifactHash(path); err == nil {
		t.Fatal("shared binary evidence accepted")
	}
}

func TestClassifiedHistoryBindingRetryRequiresExactSyncableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding.json")
	raw := []byte(`{"version":1}`)
	os.WriteFile(path, raw, 0600)
	if err := syncClassifiedHistoryBinding(path, raw); err != nil {
		t.Fatal("existing exact receipt could not become durable")
	}
	if err := syncClassifiedHistoryBinding(path, []byte(`{"version":2}`)); err == nil {
		t.Fatal("changed binding synchronized as trusted")
	}
	os.Rename(path, path+".target")
	os.Symlink(path+".target", path)
	if err := syncClassifiedHistoryBinding(path, raw); err == nil {
		t.Fatal("symlink receipt became durable authority")
	}
}

func TestClassifiedHistoryBindingRetryFailsClosedOnSyncFault(t *testing.T) {
	for _, stage := range []string{"file", "directory"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "binding.json")
			raw := []byte(`{"version":1}`)
			os.WriteFile(path, raw, 0600)
			fault := errors.New("synthetic_sync_failure")
			fileSynced := false
			err := syncClassifiedHistoryBindingWith(path, raw, func(f *os.File) error {
				if stage == "file" {
					return fault
				}
				fileSynced = true
				return f.Sync()
			}, func(dir string) error {
				if !fileSynced {
					t.Fatal("directory sync preceded file durability")
				}
				return fault
			})
			if err == nil {
				t.Fatal("matching receipt admitted migration after failed durability")
			}
		})
	}
}
