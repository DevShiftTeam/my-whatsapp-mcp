package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const ClassifiedHistoryGroupParticipantChangeNumber = "v1_group_participant_change_number"
const ClassifiedHistoryChangeUsername = "v1_noncontent_history_notice"

// ClassifiedHistoryDiagnosticFor retains the original diagnostic constructors.
// Authorization uses ClassifiedHistoryDiagnosticAllowed for exact membership.
func ClassifiedHistoryDiagnosticFor(classification string) (ClassifiedHistoryDiagnostic, bool) {
	if classification == ClassifiedHistoryGroupParticipantChangeNumber {
		return ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 33, StubPresent: true, StubKnown: true, Presence: 475}, true
	}
	if classification == ClassifiedHistoryChangeUsername {
		return ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 193, StubPresent: true, StubKnown: true, Presence: 411}, true
	}
	return ClassifiedHistoryDiagnostic{}, false
}

// ClassifiedHistoryDiagnosticAllowed admits only individually reviewed tuples.
// The notice classification does not authorize arbitrary known protocol stubs.
func ClassifiedHistoryDiagnosticAllowed(classification string, diagnostic ClassifiedHistoryDiagnostic) bool {
	expected, known := ClassifiedHistoryDiagnosticFor(classification)
	if !known {
		return false
	}
	if diagnostic == expected {
		return true
	}
	if classification == ClassifiedHistoryChangeUsername {
		expected.RawStub = 217
		return diagnostic == expected
	}
	return false
}

// ClassifiedHistoryConflictProducerSupported limits the reviewed conflict evidence
// grammar to immutable producers qualified for this recovery.
func ClassifiedHistoryConflictProducerSupported(buildSHA string) bool {
	return buildSHA == "ff0a3bd06a7f921e50d9b5c6484c481066070db3" || buildSHA == "7a7ca4a2ec0f619c0aff0a25839abcba68805a0a" || buildSHA == "028633b7af17c13f016b4b0e95ad3769c3b8dcb1" || buildSHA == "0e4dd9b2643ad3a4263333a4878f747190853c41"
}

type ClassifiedHistoryDiagnostic struct {
	Event       string `json:"event"`
	Stage       string `json:"stage"`
	RawStub     int    `json:"raw_stub"`
	StubPresent bool   `json:"stub_present"`
	StubKnown   bool   `json:"stub_known"`
	Presence    uint64 `json:"presence"`
}

// These metadata readers support exactly the last pre-classification schema
// and its additive audit migration. Writable recovery still requires latest.
func validateCompatibleSnapshotMigrations(ctx context.Context, tx *sql.Tx) (int, error) {
	if err := validateOriginalCacheMigrationHistory(ctx, tx, false); err != nil {
		return 0, err
	}
	var version int
	if err := tx.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		return 0, fixedSQLError(err)
	}
	if version != 14 && version != 15 && version != 16 {
		return 0, ErrSchemaChanged
	}
	return version, nil
}

// ClassifiedHistoryWitness records explicit owner acceptance of a committed
// partial snapshot. Hashes bind reviewed evidence; they do not prove causality.
type ClassifiedHistoryWitness struct {
	Version                   int                         `json:"version"`
	Classification            string                      `json:"classification"`
	RebuildGeneration         string                      `json:"rebuild_generation"`
	UnsafeEpoch               uint64                      `json:"unsafe_epoch"`
	ArchiveRevision           uint64                      `json:"archive_revision"`
	RecoveryEpoch             uint64                      `json:"recovery_epoch"`
	OriginalArchiveSHA256     string                      `json:"original_archive_sha256"`
	EvidenceSHA256            string                      `json:"evidence_sha256"`
	OperatorReceiptSHA256     string                      `json:"operator_receipt_sha256"`
	DiagnosticLogSHA256       string                      `json:"diagnostic_log_sha256"`
	LaunchEvidenceSHA256      string                      `json:"launch_evidence_sha256"`
	RuntimeBuildSHA           string                      `json:"runtime_build_sha"`
	RuntimeBinarySHA256       string                      `json:"runtime_binary_sha256"`
	RuntimePID                int                         `json:"runtime_pid"`
	LaunchEvidenceKind        string                      `json:"launch_evidence_kind"`
	LaunchActionRecordedAt    time.Time                   `json:"launch_action_recorded_at"`
	ProcessObservedAt         time.Time                   `json:"process_observed_at"`
	StoppedAt                 time.Time                   `json:"stopped_at"`
	ProcessReadReceiptSHA256  string                      `json:"process_read_receipt_sha256"`
	JoinedExitReceiptSHA256   string                      `json:"joined_exit_receipt_sha256"`
	Diagnostic                ClassifiedHistoryDiagnostic `json:"diagnostic"`
	ObservationMarkerSHA256   string                      `json:"observation_marker_sha256"`
	ObservationMarkerIdentity string                      `json:"observation_marker_identity"`
	UnsafeMarkerSHA256        string                      `json:"unsafe_marker_sha256"`
	UnsafeMarkerIdentity      string                      `json:"unsafe_marker_identity"`
	SourceRevisionConflicts   uint32                      `json:"source_revision_conflicts,omitempty"`
}

// OpenClassifiedHistoryRecovery seals the caller's validated evidence while
// holding the archive lease, before SQLite can modify physical archive bytes.
// The caller also holds the profile and retained ancestor instance leases.
func OpenClassifiedHistoryRecovery(ctx context.Context, dir string, w ClassifiedHistoryWitness, beforeMigration func() error) (*Store, error) {
	if _, _, err := canonicalClassifiedHistoryWitness(w); err != nil {
		return nil, err
	}
	if beforeMigration == nil {
		return nil, ErrInvalidEvent
	}
	if _, err := originalCacheExistingArchive(dir, true); err != nil {
		return nil, err
	}
	return openArchive(ctx, dir, Options{}, beforeMigration)
}

func (s *Store) RestoreClassifiedHistorySnapshot(ctx context.Context, w ClassifiedHistoryWitness) (SnapshotRecoveryReceipt, error) {
	if _, _, err := canonicalClassifiedHistoryWitness(w); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	return s.restoreCommittedSnapshot(ctx, "", &w)
}

func canonicalClassifiedHistoryWitness(w ClassifiedHistoryWitness) (string, string, error) {
	validHex := func(value string, bytes int) bool {
		v, e := hex.DecodeString(value)
		return e == nil && len(v) == bytes && strings.ToLower(value) == value
	}
	if w.Version != 1 || !validHex(w.RebuildGeneration, 16) || w.UnsafeEpoch == 0 || w.UnsafeEpoch >= 1<<63-1 || w.ArchiveRevision >= 1<<63-1 || w.RecoveryEpoch >= 1<<63-1 || w.RuntimePID <= 0 {
		return "", "", ErrInvalidEvent
	}
	if w.LaunchEvidenceKind != "owner_attested_launch_interval_v1" || w.ProcessObservedAt.Before(w.LaunchActionRecordedAt) || w.StoppedAt.Before(w.ProcessObservedAt) {
		return "", "", ErrInvalidEvent
	}
	for _, stamp := range []time.Time{w.LaunchActionRecordedAt, w.ProcessObservedAt, w.StoppedAt} {
		if stamp.IsZero() || !validReadTime(stamp) || stamp.Location() != time.UTC {
			return "", "", ErrInvalidEvent
		}
	}
	for _, hash := range []string{w.OriginalArchiveSHA256, w.EvidenceSHA256, w.OperatorReceiptSHA256, w.DiagnosticLogSHA256, w.LaunchEvidenceSHA256, w.RuntimeBinarySHA256, w.UnsafeMarkerSHA256, w.ProcessReadReceiptSHA256, w.JoinedExitReceiptSHA256} {
		if !validHex(hash, 32) {
			return "", "", ErrInvalidEvent
		}
	}
	if !validHex(w.RuntimeBuildSHA, 20) || !validString(w.UnsafeMarkerIdentity, 256) {
		return "", "", ErrInvalidEvent
	}
	if (w.ObservationMarkerSHA256 == "") != (w.ObservationMarkerIdentity == "") || (w.ObservationMarkerSHA256 != "" && (!validHex(w.ObservationMarkerSHA256, 32) || !validString(w.ObservationMarkerIdentity, 256))) {
		return "", "", ErrInvalidEvent
	}
	if !ClassifiedHistoryDiagnosticAllowed(w.Classification, w.Diagnostic) {
		return "", "", ErrInvalidEvent
	}
	if w.SourceRevisionConflicts > 32 || (w.SourceRevisionConflicts != 0 && (w.Classification != ClassifiedHistoryChangeUsername || !ClassifiedHistoryConflictProducerSupported(w.RuntimeBuildSHA))) {
		return "", "", ErrInvalidEvent
	}
	b, err := json.Marshal(w)
	if err != nil || len(b) > 8192 {
		return "", "", ErrInvalidEvent
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:]), nil
}

func (s *Store) bindClassifiedHistoryAudit(w *ClassifiedHistoryWitness, current, prior *snapshotRecoveryAudit, hasPrior bool, unsafe int, markers []snapshotMarker) error {
	if w == nil {
		if hasPrior && (prior.classification != "" || prior.witnessJSON != "" || prior.witnessSHA256 != "") {
			// A completed typed audit cannot authorize ordinary replay. A later,
			// independently eligible ordinary failure can start its own audit.
			laterFault := unsafe == 1 && current.unsafeEpoch > prior.unsafeEpoch+1 && (current.reason == "drain_failed" || current.reason == "unclean_shutdown")
			newObservation := unsafe == 0 && current.unsafeEpoch == prior.unsafeEpoch+1 && markerDigest(markers, "unsafe.marker") == "" && markerDigest(markers, "observation.marker") != "" && (markerDigest(markers, "observation.marker") != prior.observationDigest || markerIdentity(markers, "observation.marker") != prior.observationIdentity)
			if prior.phase == "complete" && (laterFault || newObservation) {
				return nil
			}
			return ErrReconciliationRequired
		}
		return nil
	}
	canonical, digest, err := canonicalClassifiedHistoryWitness(*w)
	if err != nil {
		return err
	}
	if s.rebuild == nil || s.rebuild.Generation != w.RebuildGeneration || s.rebuild.Account != current.account || current.reason != "history_corrupt" {
		return ErrReconciliationRequired
	}
	if current.epoch == w.RecoveryEpoch+1 {
		if !hasPrior || prior.classification != w.Classification || prior.witnessJSON != canonical || prior.witnessSHA256 != digest || prior.reason != "history_corrupt" || prior.unsafeEpoch != w.UnsafeEpoch || prior.beforeRevision != w.ArchiveRevision || prior.epoch != w.RecoveryEpoch+1 || prior.revision != w.ArchiveRevision+1 || current.beforeRevision != prior.revision || prior.observationDigest != w.ObservationMarkerSHA256 || prior.observationIdentity != w.ObservationMarkerIdentity || prior.unsafeDigest != w.UnsafeMarkerSHA256 || prior.unsafeIdentity != w.UnsafeMarkerIdentity {
			return ErrPolicyChanged
		}
		return nil
	}
	if current.epoch != w.RecoveryEpoch || current.beforeRevision != w.ArchiveRevision || current.unsafeEpoch != w.UnsafeEpoch || unsafe != 1 || (hasPrior && prior.phase == "prepared") {
		return ErrPolicyChanged
	}
	if markerDigest(markers, "observation.marker") != w.ObservationMarkerSHA256 || markerIdentity(markers, "observation.marker") != w.ObservationMarkerIdentity || markerDigest(markers, "unsafe.marker") != w.UnsafeMarkerSHA256 || markerIdentity(markers, "unsafe.marker") != w.UnsafeMarkerIdentity {
		return ErrReconciliationRequired
	}
	current.classification = w.Classification
	current.witnessJSON = canonical
	current.witnessSHA256 = digest
	return nil
}
