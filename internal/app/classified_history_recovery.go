package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrClassifiedHistoryRecovery = errors.New("classified_history_recovery_unavailable")

type ClassifiedHistoryRecoveryConfig struct {
	DataDir                string `json:"data_dir"`
	EvidencePath           string `json:"evidence_path"`
	ExpectedEvidenceSHA256 string `json:"expected_evidence_sha256"`
	SourceLockFD           *int   `json:"source_lock_fd,omitempty"`
	AncestorLockFDs        []int  `json:"ancestor_lock_fds,omitempty"`
}
type ClassifiedHistoryEvidence struct {
	Witness             archive.ClassifiedHistoryWitness `json:"witness"`
	DiagnosticLogPath   string                           `json:"diagnostic_log_path"`
	LaunchEvidencePath  string                           `json:"launch_evidence_path"`
	OperatorReceiptPath string                           `json:"operator_receipt_path"`
	ReadySHA256         string                           `json:"ready_sha256"`
}
type ClassifiedHistoryLaunchArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type ClassifiedHistoryLaunchEvidence struct {
	Artifacts              map[string]ClassifiedHistoryLaunchArtifact `json:"artifacts"`
	Version                int                                        `json:"version"`
	DataDir                string                                     `json:"data_dir"`
	ReadySHA256            string                                     `json:"ready_sha256"`
	RuntimeBuildSHA        string                                     `json:"runtime_build_sha"`
	RuntimeBinarySHA256    string                                     `json:"runtime_binary_sha256"`
	RuntimePID             int                                        `json:"runtime_pid"`
	LaunchEvidenceKind     string                                     `json:"launch_evidence_kind"`
	LaunchActionRecordedAt string                                     `json:"launch_action_recorded_at"`
	ProcessObservedAt      string                                     `json:"process_observed_at"`
	StoppedAt              string                                     `json:"stopped_at"`
	ProcessReadReceiptPath string                                     `json:"process_read_receipt_path"`
	JoinedExitReceiptPath  string                                     `json:"joined_exit_receipt_path"`
}
type ClassifiedHistoryOperatorReceipt struct {
	Version               int    `json:"version"`
	AcceptedPartial       bool   `json:"accepted_partial"`
	AttestedCurrentFence  bool   `json:"attested_current_fence"`
	LaunchSHA256          string `json:"launch_sha256"`
	DiagnosticLogSHA256   string `json:"diagnostic_log_sha256"`
	OriginalArchiveSHA256 string `json:"original_archive_sha256"`
	RebuildGeneration     string `json:"rebuild_generation"`
	UnsafeEpoch           uint64 `json:"unsafe_epoch"`
	ArchiveRevision       uint64 `json:"archive_revision"`
	RecoveryEpoch         uint64 `json:"recovery_epoch"`
}

func strictClassifiedJSON(raw []byte, into any) error {
	if len(raw) == 0 || len(raw) > 65536 {
		return ErrClassifiedHistoryRecovery
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 12 {
			return ErrClassifiedHistoryRecovery
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return ErrClassifiedHistoryRecovery
				}
				seen[key] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrClassifiedHistoryRecovery
		}
		_, err = d.Token()
		return err
	}
	if walk(0) != nil {
		return ErrClassifiedHistoryRecovery
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrClassifiedHistoryRecovery
	}
	return liveRebuildDecode(raw, into)
}
func readClassifiedHistoryEvidence(cfg ClassifiedHistoryRecoveryConfig) (ClassifiedHistoryEvidence, error) {
	var e ClassifiedHistoryEvidence
	if !filepath.IsAbs(cfg.DataDir) || filepath.Clean(cfg.DataDir) != cfg.DataDir || !filepath.IsAbs(cfg.EvidencePath) || filepath.Clean(cfg.EvidencePath) != cfg.EvidencePath || !liveRebuildValidDigest(cfg.ExpectedEvidenceSHA256) {
		return e, ErrClassifiedHistoryRecovery
	}
	raw, err := liveRebuildReadPrivate(cfg.EvidencePath, 65536)
	if err != nil || liveRebuildDigest(raw) != cfg.ExpectedEvidenceSHA256 || strictClassifiedJSON(raw, &e) != nil {
		return e, ErrClassifiedHistoryRecovery
	}
	w := &e.Witness
	if w.EvidenceSHA256 != "" || !liveRebuildValidDigest(e.ReadySHA256) || w.Version != 1 || w.Classification != archive.ClassifiedHistoryGroupParticipantChangeNumber || len(w.RebuildGeneration) != 32 || strings.Trim(w.RebuildGeneration, "0123456789abcdef") != "" || w.UnsafeEpoch == 0 || w.RuntimePID <= 0 || w.LaunchEvidenceKind != "owner_attested_launch_interval_v1" || w.LaunchActionRecordedAt.IsZero() || w.LaunchActionRecordedAt.Location() != time.UTC || w.ProcessObservedAt.Location() != time.UTC || w.StoppedAt.Location() != time.UTC || w.ProcessObservedAt.Before(w.LaunchActionRecordedAt) || w.StoppedAt.Before(w.ProcessObservedAt) || len(w.RuntimeBuildSHA) != 40 || strings.Trim(w.RuntimeBuildSHA, "0123456789abcdef") != "" {
		return e, ErrClassifiedHistoryRecovery
	}
	if w.Diagnostic != (archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 33, StubPresent: true, StubKnown: true, Presence: 475}) {
		return e, ErrClassifiedHistoryRecovery
	}
	for _, h := range []string{w.OriginalArchiveSHA256, w.OperatorReceiptSHA256, w.DiagnosticLogSHA256, w.LaunchEvidenceSHA256, w.RuntimeBinarySHA256, w.UnsafeMarkerSHA256, w.ProcessReadReceiptSHA256, w.JoinedExitReceiptSHA256} {
		if !liveRebuildValidDigest(h) {
			return e, ErrClassifiedHistoryRecovery
		}
	}
	for _, path := range []string{e.DiagnosticLogPath, e.LaunchEvidencePath, e.OperatorReceiptPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return e, ErrClassifiedHistoryRecovery
		}
	}
	launchRaw, err := liveRebuildReadPrivate(e.LaunchEvidencePath, 65536)
	var launch ClassifiedHistoryLaunchEvidence
	if err != nil || liveRebuildDigest(launchRaw) != w.LaunchEvidenceSHA256 || strictClassifiedJSON(launchRaw, &launch) != nil || launch.Version != 1 || launch.DataDir != cfg.DataDir || launch.ReadySHA256 != e.ReadySHA256 || launch.RuntimeBuildSHA != w.RuntimeBuildSHA || launch.RuntimeBinarySHA256 != w.RuntimeBinarySHA256 || launch.RuntimePID != w.RuntimePID || launch.LaunchEvidenceKind != w.LaunchEvidenceKind || launch.LaunchActionRecordedAt != w.LaunchActionRecordedAt.Format(time.RFC3339Nano) || launch.ProcessObservedAt != w.ProcessObservedAt.Format(time.RFC3339Nano) || launch.StoppedAt != w.StoppedAt.Format(time.RFC3339Nano) {
		return e, ErrClassifiedHistoryRecovery
	}

	for _, artifact := range []struct{ path, digest string }{{launch.ProcessReadReceiptPath, w.ProcessReadReceiptSHA256}, {launch.JoinedExitReceiptPath, w.JoinedExitReceiptSHA256}} {
		if !filepath.IsAbs(artifact.path) || filepath.Clean(artifact.path) != artifact.path {
			return e, ErrClassifiedHistoryRecovery
		}
		raw, err := liveRebuildReadPrivate(artifact.path, 16<<20)
		if err != nil || liveRebuildDigest(raw) != artifact.digest {
			return e, ErrClassifiedHistoryRecovery
		}
		clear(raw)
	}

	if len(launch.Artifacts) != 5 {
		return e, ErrClassifiedHistoryRecovery
	}
	for _, role := range []string{"supervisor", "launch_manifest", "plist", "launch_action", "binary"} {
		artifact, ok := launch.Artifacts[role]
		if !ok || !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path || !liveRebuildValidDigest(artifact.SHA256) {
			return e, ErrClassifiedHistoryRecovery
		}
		if role == "binary" {
			if artifact.SHA256 != w.RuntimeBinarySHA256 {
				return e, ErrClassifiedHistoryRecovery
			}
			digest, err := classifiedArtifactHash(artifact.Path)
			if err != nil || digest != artifact.SHA256 {
				return e, ErrClassifiedHistoryRecovery
			}
		} else {
			raw, err := liveRebuildReadPrivate(artifact.Path, 16<<20)
			if err != nil || liveRebuildDigest(raw) != artifact.SHA256 {
				return e, ErrClassifiedHistoryRecovery
			}
			clear(raw)
		}
	}
	operatorRaw, err := liveRebuildReadPrivate(e.OperatorReceiptPath, 65536)
	var operator ClassifiedHistoryOperatorReceipt
	expected := ClassifiedHistoryOperatorReceipt{1, true, true, w.LaunchEvidenceSHA256, w.DiagnosticLogSHA256, w.OriginalArchiveSHA256, w.RebuildGeneration, w.UnsafeEpoch, w.ArchiveRevision, w.RecoveryEpoch}
	if err != nil || liveRebuildDigest(operatorRaw) != w.OperatorReceiptSHA256 || strictClassifiedJSON(operatorRaw, &operator) != nil || operator != expected {
		return e, ErrClassifiedHistoryRecovery
	}
	log, err := liveRebuildReadPrivate(e.DiagnosticLogPath, 32<<20)
	if err != nil || liveRebuildDigest(log) != w.DiagnosticLogSHA256 {
		return e, ErrClassifiedHistoryRecovery
	}
	defer clear(log)
	count := 0
	for _, line := range bytes.Split(log, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' {
			if bytes.Contains(line, []byte("history_failure")) {
				return e, ErrClassifiedHistoryRecovery
			}
			continue
		}
		var fields map[string]json.RawMessage
		if strictClassifiedJSON(line, &fields) != nil {
			return e, ErrClassifiedHistoryRecovery
		}
		var event string
		if raw, ok := fields["event"]; ok && json.Unmarshal(raw, &event) != nil {
			return e, ErrClassifiedHistoryRecovery
		}
		if event == "runtime_failure" {
			var code string
			if json.Unmarshal(fields["code"], &code) != nil || (code != "history_corrupt" && code != "reconciliation_required") {
				return e, ErrClassifiedHistoryRecovery
			}
		}
		if event != "history_failure" {
			continue
		}
		var diagnostic struct {
			Event       string `json:"event"`
			Stage       string `json:"stage"`
			Stub        string `json:"stub"`
			StubType    int    `json:"stub_type"`
			RawStub     int    `json:"raw_stub"`
			StubPresent bool   `json:"stub_present"`
			StubKnown   bool   `json:"stub_known"`
			Presence    uint64 `json:"presence"`
		}
		if strictClassifiedJSON(line, &diagnostic) != nil || diagnostic.Event != "history_failure" || diagnostic.Stage != "record_body_missing" || diagnostic.Stub != "other_stub" || diagnostic.StubType != 33 || diagnostic.RawStub != 33 || !diagnostic.StubPresent || !diagnostic.StubKnown || diagnostic.Presence != 475 {
			return e, ErrClassifiedHistoryRecovery
		}
		count++
	}
	if count != 1 {
		return e, ErrClassifiedHistoryRecovery
	}
	canonical, err := json.Marshal(e)
	if err != nil {
		return e, ErrClassifiedHistoryRecovery
	}
	w.EvidenceSHA256 = liveRebuildDigest(canonical)
	return e, nil
}

const classifiedHistoryBindingFile = "classified-history-recovery.v1.json"

type classifiedHistoryBinding struct {
	Version     int                              `json:"version"`
	DataDir     string                           `json:"data_dir"`
	ReadySHA256 string                           `json:"ready_sha256"`
	Witness     archive.ClassifiedHistoryWitness `json:"witness"`
}

func classifiedArchiveHash(ctx context.Context, path string) (string, error) {
	if err := liveRebuildNoWAL(path); err != nil {
		return "", err
	}
	before, err := os.Lstat(path)
	if err != nil || !privateStateFile(before) || before.Size() > 8<<30 {
		return "", ErrClassifiedHistoryRecovery
	}
	f, err := os.OpenFile(path, os.O_RDONLY|stateNoFollow(), 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) || !stateNoExtendedACL(f) {
		return "", ErrClassifiedHistoryRecovery
	}
	h := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		n, e := f.Read(buf)
		h.Write(buf[:n])
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", e
		}
	}
	after, err := f.Stat()
	named, ne := os.Lstat(path)
	if err != nil || ne != nil || !os.SameFile(held, named) || held.Size() != after.Size() || !held.ModTime().Equal(after.ModTime()) {
		return "", ErrClassifiedHistoryRecovery
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func prepareClassifiedHistoryBinding(ctx context.Context, paths appdata.Paths, e ClassifiedHistoryEvidence) error {
	expected := classifiedHistoryBinding{1, paths.Root, e.ReadySHA256, e.Witness}
	raw, _ := json.Marshal(expected)
	path := filepath.Join(paths.WorkingDir, classifiedHistoryBindingFile)
	prior, err := liveRebuildReadPrivate(path, 65536)
	if err == nil {
		var binding classifiedHistoryBinding
		if strictClassifiedJSON(prior, &binding) != nil || binding != expected {
			return ErrClassifiedHistoryRecovery
		}
		return syncClassifiedHistoryBinding(path, prior)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return ErrClassifiedHistoryRecovery
	}
	digest, err := classifiedArchiveHash(ctx, filepath.Join(paths.ArchiveDir, "archive.sqlite"))
	if err != nil || digest != e.Witness.OriginalArchiveSHA256 {
		return ErrClassifiedHistoryRecovery
	}
	return liveRebuildWriteNew(path, raw)
}
func RecoverClassifiedHistorySnapshot(ctx context.Context, cfg ClassifiedHistoryRecoveryConfig) (receipt archive.SnapshotRecoveryReceipt, result error) {
	e, err := readClassifiedHistoryEvidence(cfg)
	if err != nil {
		return receipt, ErrClassifiedHistoryRecovery
	}
	paths, err := appdata.ExistingPaths(cfg.DataDir)
	if err != nil || paths.Root != cfg.DataDir {
		return receipt, ErrClassifiedHistoryRecovery
	}
	if err = archive.RefuseHistoricalSnapshot(paths.ArchiveDir); err != nil {
		return receipt, err
	}
	lease, err := appdata.AcquireExisting(paths)
	if err != nil {
		return receipt, err
	}
	defer func() {
		if err := lease.Close(); err != nil {
			result = errors.Join(result, err)
			receipt = archive.SnapshotRecoveryReceipt{}
		}
	}()
	guards, err := AcquireLiveRebuildSourceGuard(paths.Root, cfg.SourceLockFD, cfg.AncestorLockFDs...)
	if err != nil || guards == nil {
		return receipt, ErrClassifiedHistoryRecovery
	}
	defer func() {
		if err := guards.Close(); err != nil {
			result = errors.Join(result, err)
			receipt = archive.SnapshotRecoveryReceipt{}
		}
	}()
	ready, raw, err := liveRebuildLoadReady(paths.Root)
	if err != nil || ready == nil || liveRebuildDigest(raw) != e.ReadySHA256 || ready.Archive.Generation != e.Witness.RebuildGeneration {
		return receipt, ErrClassifiedHistoryRecovery
	}
	state, exists, err := readState(paths, lease)
	if err != nil || !exists || state.Mode != "real" {
		return receipt, ErrClassifiedHistoryRecovery
	}
	for _, path := range []string{filepath.Join(paths.ArchiveDir, "archive.sqlite"), filepath.Join(paths.SessionDir, "session.sqlite")} {
		if err = existingRecoverySQLite(ctx, path); err != nil {
			return receipt, err
		}
	}
	store, err := archive.OpenClassifiedHistoryRecovery(ctx, paths.ArchiveDir, e.Witness, func() error { return prepareClassifiedHistoryBinding(ctx, paths, e) })
	if err != nil {
		return receipt, err
	}
	defer func() {
		if err := store.Close(); err != nil {
			result = errors.Join(result, err)
			receipt = archive.SnapshotRecoveryReceipt{}
		}
	}()
	provenance, err := store.RebuildProvenance(ctx)
	if err != nil || provenance == nil || *provenance != guards.provenance {
		return receipt, ErrClassifiedHistoryRecovery
	}
	account, err := store.BoundAccount(ctx)
	if err != nil || account != ready.Archive.Account || account == demoAccount {
		return receipt, ErrClassifiedHistoryRecovery
	}
	recovery, err := recoverExtractionPins(ctx, store, account)
	if err != nil {
		return receipt, err
	}
	for !recovery.complete {
		if err = ctx.Err(); err != nil {
			return receipt, err
		}
		if _, err = recovery.step(ctx, store); err != nil {
			return receipt, err
		}
	}
	return store.RestoreClassifiedHistorySnapshot(ctx, e.Witness)
}

func ParseClassifiedHistoryRecoveryConfig(raw []byte) (ClassifiedHistoryRecoveryConfig, error) {
	var cfg ClassifiedHistoryRecoveryConfig
	if strictClassifiedJSON(raw, &cfg) != nil {
		return cfg, ErrClassifiedHistoryRecovery
	}
	return cfg, nil
}

func classifiedArtifactHash(path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil || (before.Mode().Perm() != 0600 && before.Mode().Perm() != 0700 && before.Mode().Perm() != 0500) || !privateStateFile(classifiedArtifactInfo{before}) || before.Size() > 512<<20 {
		return "", ErrClassifiedHistoryRecovery
	}
	f, err := os.OpenFile(path, os.O_RDONLY|stateNoFollow(), 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) || !stateNoExtendedACL(f) {
		return "", ErrClassifiedHistoryRecovery
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, (512<<20)+1)); err != nil {
		return "", err
	}
	after, err := f.Stat()
	named, ne := os.Lstat(path)
	if err != nil || ne != nil || !os.SameFile(held, named) || held.Size() != after.Size() || !held.ModTime().Equal(after.ModTime()) {
		return "", ErrClassifiedHistoryRecovery
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Executable evidence retains the same owner, single-link and regular-file
// requirements as private state; only its owner execute bit differs.
type classifiedArtifactInfo struct{ os.FileInfo }

func (i classifiedArtifactInfo) Mode() os.FileMode { return i.FileInfo.Mode()&^0777 | 0600 }

// A prior failed write/sync may leave matching bytes without a durable seal.
// Reestablish file and directory durability before allowing migration on retry.
func syncClassifiedHistoryBinding(path string, expected []byte) error {
	return syncClassifiedHistoryBindingWith(path, expected, (*os.File).Sync, liveRebuildSyncDir)
}
func syncClassifiedHistoryBindingWith(path string, expected []byte, syncFile func(*os.File) error, syncDirectory func(string) error) error {
	before, err := os.Lstat(path)
	if err != nil || !privateStateFile(before) {
		return ErrClassifiedHistoryRecovery
	}
	f, err := os.OpenFile(path, os.O_RDONLY|stateNoFollow(), 0)
	if err != nil {
		return ErrClassifiedHistoryRecovery
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) || !stateNoExtendedACL(f) {
		return ErrClassifiedHistoryRecovery
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || !bytes.Equal(raw, expected) {
		return ErrClassifiedHistoryRecovery
	}
	if err = syncFile(f); err != nil {
		return ErrClassifiedHistoryRecovery
	}
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(held, named) {
		return ErrClassifiedHistoryRecovery
	}
	return syncDirectory(filepath.Dir(path))
}
