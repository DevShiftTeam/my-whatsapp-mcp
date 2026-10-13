package archive

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const extensionDecodedQuota int64 = 128 << 20
const extensionMediaBatch = 256

type extensionMediaOperation uint8

const (
	extensionMediaAdmit extensionMediaOperation = iota
	extensionMediaRenew
	extensionMediaPublish
	extensionMediaCandidates
	extensionMediaEvict
	extensionMediaCleanup
	extensionMediaReconcile
	extensionMediaInspect
)

// Background command priority never changes its failure sensitivity. No
// command accepts a path, source bytes, caller transaction or safety switch.
type extensionMediaCommand struct {
	op          extensionMediaOperation
	admission   MediaAdmissionRequest
	reservation MediaReservation
	publication OriginalPublication
	cleanup     MediaCleanup
	media       MediaID
	ttl         time.Duration
	limit       int
}

func (c extensionMediaCommand) account() string {
	switch c.op {
	case extensionMediaAdmit:
		return c.admission.Source.Account
	case extensionMediaRenew:
		return c.reservation.Source.Account
	case extensionMediaPublish:
		return c.publication.Reservation.Source.Account
	}
	return ""
}
func (c extensionMediaCommand) priority() bool {
	return c.op == extensionMediaRenew || c.op == extensionMediaCleanup || c.op == extensionMediaInspect
}
func (c extensionMediaCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c extensionMediaCommand) run(s *Store, ctx context.Context) (any, error) {
	var value any
	apply := func(tx *sql.Tx) error {
		var err error
		switch c.op {
		case extensionMediaAdmit:
			value, err = s.extensionAdmitMedia(ctx, tx, c.admission)
		case extensionMediaRenew:
			value, err = s.extensionRenewMedia(ctx, tx, c.reservation, c.ttl)
		case extensionMediaPublish:
			value, err = s.extensionPublishOriginal(ctx, tx, c.publication)
		case extensionMediaCandidates:
			value, err = s.extensionEvictionCandidates(ctx, tx, c.limit)
		case extensionMediaEvict:
			value, err = s.extensionBeginEviction(ctx, tx, c.media)
		case extensionMediaCleanup:
			err = s.extensionFinishMediaCleanup(ctx, tx, c.cleanup)
		case extensionMediaReconcile:
			value, err = s.extensionReconcileMedia(ctx, tx, c.limit)
		case extensionMediaInspect:
			value, err = s.extensionInspectMedia(ctx, tx, c.reservation)
		default:
			err = ErrInvalidExtension
		}
		return err
	}
	var err error
	if c.op == extensionMediaCleanup || c.op == extensionMediaReconcile || c.op == extensionMediaInspect {
		// Recovery removes owned artifacts and exposes no source body. It must
		// remain possible while the source archive is unsafe or unclean.
		err = s.transaction(ctx, apply)
	} else {
		err = s.extensionTransaction(ctx, c.account(), apply)
	}
	if c.op == extensionMediaPublish && (errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged)) {
		return false, nil
	}
	return value, err
}
func validExtensionMediaTTL(ttl time.Duration) bool { return ttl >= time.Second && ttl <= 24*time.Hour }
func validExtensionMediaSource(ref SourceRef) bool {
	return validString(ref.Account, 512) && validReadID(string(ref.Version.MessageID)) && ref.Version.Revision > 0 && ref.Version.PolicyGeneration > 0 && !ref.Version.Deleted && !ref.Version.Excluded && validReadTime(ref.Version.ExpiresAt) && validString(ref.ContentHash, 128)
}
func validExtensionReservation(r MediaReservation) bool {
	return validExtensionMediaSource(r.Source) && validReadID(string(r.ID)) && validReadID(string(r.MediaID)) && validReadID(string(r.ArtifactID)) && validReadID(string(r.LeaseID)) && !r.LeaseUntil.IsZero() && validReadTime(r.LeaseUntil) && r.MaximumBytes > 0 && (r.Budget == BudgetOriginals || r.Budget == BudgetDecoded)
}
func extensionMediaLimit(limit int) error {
	if limit <= 0 {
		return ErrInvalidExtension
	}
	if limit > extensionMediaBatch {
		return ErrLimitExceeded
	}
	return nil
}
func (s *Store) AdmitMedia(ctx context.Context, request MediaAdmissionRequest) (MediaReservation, error) {
	if request.OwnerDownloadLease != (OriginalDownloadLease{}) && (request.OwnerDownloadLease.IntentID == "" || !extensionValidDownloadLease(request.OwnerDownloadLease)) {
		return MediaReservation{}, ErrInvalidExtension
	}
	if !validExtensionMediaSource(request.Source) || !validReadID(string(request.MediaID)) || (request.Budget != BudgetOriginals && request.Budget != BudgetDecoded) || request.MaximumBytes <= 0 || !validExtensionMediaTTL(request.TTL) {
		return MediaReservation{}, ErrInvalidExtension
	}
	value, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaAdmit, admission: request}, 1024)
	if err != nil {
		return MediaReservation{}, err
	}
	return value.(MediaReservation), nil
}
func (s *Store) RenewMedia(ctx context.Context, r MediaReservation, ttl time.Duration) (time.Time, error) {
	if !validExtensionReservation(r) || !validExtensionMediaTTL(ttl) {
		return time.Time{}, ErrInvalidExtension
	}
	value, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaRenew, reservation: r, ttl: ttl}, 1024)
	if err != nil {
		return time.Time{}, err
	}
	return value.(time.Time), nil
}
func (s *Store) PublishOriginal(ctx context.Context, p OriginalPublication) (bool, error) {
	if !validExtensionReservation(p.Reservation) || p.Reservation.Budget != BudgetOriginals || p.Bytes <= 0 || !extensionMediaDigest(p.SHA256) || !extensionMediaFormat(p.ActualFormat) {
		return false, ErrInvalidExtension
	}
	if p.Bytes > p.Reservation.MaximumBytes {
		return false, ErrQuotaExceeded
	}
	value, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaPublish, publication: p}, 1152)
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}
func (s *Store) EvictionCandidates(ctx context.Context, limit int) ([]MediaID, error) {
	if err := extensionMediaLimit(limit); err != nil {
		return nil, err
	}
	value, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaCandidates, limit: limit}, 0)
	if err != nil {
		return nil, err
	}
	return value.([]MediaID), nil
}
func (s *Store) BeginEviction(ctx context.Context, id MediaID) (MediaArtifact, error) {
	if !validReadID(string(id)) {
		return MediaArtifact{}, ErrInvalidExtension
	}
	value, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaEvict, media: id}, int64(len(id)))
	if err != nil {
		return MediaArtifact{}, err
	}
	return value.(MediaArtifact), nil
}
func (s *Store) FinishMediaCleanup(ctx context.Context, c MediaCleanup) error {
	if !validReadID(string(c.ReservationID)) || !validReadID(string(c.ArtifactID)) || !validReadID(string(c.LeaseID)) || !extensionMediaCleanupCode(c.Code) {
		return ErrInvalidExtension
	}
	_, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaCleanup, cleanup: c}, 512)
	return err
}
func (s *Store) ReconcileMedia(ctx context.Context, limit int) ([]MediaArtifact, error) {
	if err := extensionMediaLimit(limit); err != nil {
		return nil, err
	}
	value, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaReconcile, limit: limit}, 0)
	if err != nil {
		return nil, err
	}
	return value.([]MediaArtifact), nil
}

// InspectMediaReservation resolves an uncertain publication using the exact
// private reservation capability. It returns cleanup metadata, never current
// source access or an arbitrary artifact-ID lookup.
func (s *Store) InspectMediaReservation(ctx context.Context, r MediaReservation) (MediaArtifact, error) {
	if !validExtensionReservation(r) {
		return MediaArtifact{}, ErrInvalidExtension
	}
	value, err := s.submitExtension(ctx, extensionMediaCommand{op: extensionMediaInspect, reservation: r}, 1024)
	if err != nil {
		return MediaArtifact{}, err
	}
	return value.(MediaArtifact), nil
}
func extensionMediaDigest(v string) bool {
	if len(v) != 64 || strings.ToLower(v) != v {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}
func extensionMediaFormat(v string) bool {
	switch v {
	case "png", "jpeg", "webp", "ogg", "opus", "wav", "mp3", "flac", "m4a", "pdf", "document":
		return true
	}
	return false
}
func extensionMediaKindFormat(kind, format string) bool {
	switch kind {
	case "image":
		return format == "png" || format == "jpeg" || format == "webp"
	case "voice":
		return format == "ogg" || format == "opus" || format == "wav" || format == "mp3" || format == "flac" || format == "m4a"
	case "document":
		return format == "pdf" || format == "document"
	}
	return false
}
func extensionMediaCleanupCode(v string) bool {
	switch v {
	case "removed", "cancelled", "failed", "orphan_removed", "evicted", "source_invalidated", "not_found", "cleanup_complete":
		return true
	}
	return false
}

// Source transactions retain only permitted protocol references, never keys
// from view-once/excluded sources. Shared hashes grant no association authority.
func (s *Store) extensionAssociateMedia(ctx context.Context, tx *sql.Tx, ref SourceRef, refs []MediaReference) ([]MediaID, error) {
	if len(refs) > 8 {
		return nil, ErrLimitExceeded
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if _, err := s.extensionCheckSources(ctx, tx, []SourceRef{ref}, true); err != nil {
		if errors.Is(err, ErrSourceUnavailable) {
			return nil, nil
		}
		return nil, err
	}
	ids := make([]MediaID, 0, len(refs))
	for ordinal, m := range refs {
		if m.ViewOnce || (m.Kind != "image" && m.Kind != "voice" && m.Kind != "document") || (len(m.ProtocolReference) == 0 && m.Kind != "document") {
			continue
		}
		var id MediaID
		err := tx.QueryRowContext(ctx, `SELECT id FROM extension_media_associations WHERE message_id=? AND local_revision=? AND ordinal=?`, ref.Version.MessageID, ref.Version.Revision, ordinal).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			created, err := randomID()
			if err != nil {
				return nil, err
			}
			id = MediaID(created)
			protocol := m.ProtocolReference
			if protocol == nil {
				protocol = []byte{}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,ordinal,kind,protocol_reference,mime_type,declared_bytes,filename,view_once,availability,active,artifact_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,1,'')`, id, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt), ordinal, m.Kind, protocol, m.MimeType, m.DeclaredBytes, m.Filename, m.Availability)
			if err != nil {
				return nil, ErrArchiveUnavailable
			}
		} else if err != nil {
			return nil, ErrArchiveUnavailable
		} else {
			protocol := m.ProtocolReference
			if protocol == nil {
				protocol = []byte{}
			}
			_, err = tx.ExecContext(ctx, `UPDATE extension_media_associations SET artifact_id=CASE WHEN active=1 AND policy_generation=? AND content_hash=? THEN artifact_id ELSE '' END,account=?,policy_generation=?,content_hash=?,source_expires_at=?,kind=?,protocol_reference=?,mime_type=?,declared_bytes=?,filename=?,availability=?,view_once=0,active=1 WHERE id=?`, ref.Version.PolicyGeneration, ref.ContentHash, ref.Account, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt), m.Kind, protocol, m.MimeType, m.DeclaredBytes, m.Filename, m.Availability, id)
			if err != nil {
				return nil, ErrArchiveUnavailable
			}
		}
		if err = extensionEnqueueOriginalDownload(ctx, tx, ref, id, m); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
func (s *Store) extensionMediaAssociation(ctx context.Context, q sqlQuerier, ref SourceRef, id MediaID) (string, ArtifactID, error) {
	if _, err := s.extensionCheckSources(ctx, q, []SourceRef{ref}, true); err != nil {
		return "", "", err
	}
	var kind string
	var artifact ArtifactID
	err := q.QueryRowContext(ctx, `SELECT kind,artifact_id FROM extension_media_associations WHERE id=? AND account=? AND message_id=? AND local_revision=? AND policy_generation=? AND content_hash=? AND source_expires_at=? AND active=1 AND view_once=0`, id, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt)).Scan(&kind, &artifact)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrSourceUnavailable
	}
	if err != nil {
		return "", "", readError(ctx, err)
	}
	return kind, artifact, nil
}

const extensionMediaArtifactColumns = `a.id,a.media_id,a.account,a.message_id,a.local_revision,a.policy_generation,a.content_hash,a.source_expires_at,a.sha256,a.bytes,a.reservation_id,a.cleanup_lease,a.budget,a.charged_bytes,a.state,a.actual_format`

type extensionStoredMedia struct {
	reservation MediaReservation
	artifact    MediaArtifact
	format      string
	cleanupAck  bool
}

func extensionLoadMedia(ctx context.Context, q sqlQuerier, id ArtifactID) (extensionStoredMedia, error) {
	var v extensionStoredMedia
	var expiry, deadline int64
	var ack int
	err := q.QueryRowContext(ctx, `SELECT `+extensionMediaArtifactColumns+`,a.maximum_bytes,a.lease_id,a.lease_until,a.actual_format,a.cleanup_ack FROM extension_media_artifacts a WHERE a.id=?`, id).Scan(&v.artifact.ID, &v.artifact.MediaID, &v.artifact.Source.Account, &v.artifact.Source.Version.MessageID, &v.artifact.Source.Version.Revision, &v.artifact.Source.Version.PolicyGeneration, &v.artifact.Source.ContentHash, &expiry, &v.artifact.SHA256, &v.artifact.Bytes, &v.artifact.ReservationID, &v.artifact.CleanupLease, &v.artifact.Budget, &v.artifact.ReservedBytes, &v.artifact.State, &v.artifact.ActualFormat, &v.reservation.MaximumBytes, &v.reservation.LeaseID, &deadline, &v.format, &ack)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNoWork
	}
	if err != nil {
		return v, readError(ctx, err)
	}
	v.artifact.Source.Version.ExpiresAt = fromMillis(expiry)
	v.reservation.ID = v.artifact.ReservationID
	v.reservation.ArtifactID = v.artifact.ID
	v.reservation.MediaID = v.artifact.MediaID
	v.reservation.Source = v.artifact.Source
	v.reservation.Budget = v.artifact.Budget
	v.reservation.LeaseUntil = fromMillis(deadline)
	v.cleanupAck = ack != 0
	return v, nil
}
func extensionMediaReservationMatches(a, b MediaReservation) bool {
	return a.ID == b.ID && a.ArtifactID == b.ArtifactID && a.MediaID == b.MediaID && a.Source.Account == b.Source.Account && a.Source.ContentHash == b.Source.ContentHash && a.Source.Version.MessageID == b.Source.Version.MessageID && a.Source.Version.Revision == b.Source.Version.Revision && a.Source.Version.PolicyGeneration == b.Source.Version.PolicyGeneration && a.Source.Version.ExpiresAt.Equal(b.Source.Version.ExpiresAt) && a.Source.Version.Deleted == b.Source.Version.Deleted && a.Source.Version.Excluded == b.Source.Version.Excluded && a.Budget == b.Budget && a.MaximumBytes == b.MaximumBytes && a.LeaseID == b.LeaseID && a.LeaseUntil.Equal(b.LeaseUntil)
}
func extensionMediaLeaseMatches(stored, caller MediaReservation) bool {
	// The lease identity grants authority, never the caller's deadline. A
	// renewal whose reply was lost remains recoverable with the prior DTO.
	// Reject a caller deadline beyond durable authority; use the stored deadline
	// and Store.now for every actual currentness decision.
	if caller.LeaseUntil.After(stored.LeaseUntil) {
		return false
	}
	stored.LeaseUntil = caller.LeaseUntil
	return extensionMediaReservationMatches(stored, caller)
}

func (s *Store) extensionAdmitMedia(ctx context.Context, tx *sql.Tx, request MediaAdmissionRequest) (MediaReservation, error) {
	if err := s.ownerVoiceAdmit(ctx, tx, request); err != nil {
		return MediaReservation{}, err
	}
	kind, inputArtifact, err := s.extensionMediaAssociation(ctx, tx, request.Source, request.MediaID)
	if err != nil {
		return MediaReservation{}, err
	}
	if kind == "document" && (request.Budget != BudgetOriginals || request.MaximumBytes > MaximumDocumentBytes+26) {
		return MediaReservation{}, ErrInvalidExtension
	}
	if request.Budget == BudgetDecoded && inputArtifact != "" {
		var available int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts WHERE id=? AND budget='originals' AND state='retained' AND cleanup_ack=0`, inputArtifact).Scan(&available); err != nil {
			return MediaReservation{}, ErrArchiveUnavailable
		}
		if available != 1 {
			return MediaReservation{}, ErrNoWork
		}
	}
	if request.Budget == BudgetDecoded && inputArtifact == "" {
		// A decoded prepublication reservation pins the already admitted original
		// even before its association becomes available for authorized retrieval.
		err = tx.QueryRowContext(ctx, `SELECT id FROM extension_media_artifacts WHERE media_id=? AND account=? AND message_id=? AND local_revision=? AND policy_generation=? AND content_hash=? AND budget='originals' AND cleanup_ack=0 AND state IN ('reserved','staged') ORDER BY created_at,id LIMIT 1`, request.MediaID, request.Source.Account, request.Source.Version.MessageID, request.Source.Version.Revision, request.Source.Version.PolicyGeneration, request.Source.ContentHash).Scan(&inputArtifact)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return MediaReservation{}, ErrArchiveUnavailable
		}
	}
	var quota, used int64
	if request.Budget == BudgetDecoded {
		quota = extensionDecodedQuota
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(charged_bytes),0) FROM extension_media_artifacts WHERE budget=? AND cleanup_ack=0`, request.Budget).Scan(&used); err != nil || used < 0 {
			return MediaReservation{}, ErrArchiveUnavailable
		}
	} else {
		budget, err := extensionLoadOriginalBudget(ctx, tx)
		if err != nil {
			return MediaReservation{}, err
		}
		quota, used = budget.MaximumBytes, budget.ChargedBytes
	}
	if request.MaximumBytes > quota {
		return MediaReservation{}, ErrQuotaExceeded
	}
	if used > quota-request.MaximumBytes {
		return MediaReservation{}, ErrQuotaExceeded
	}
	if request.Budget == BudgetOriginals {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts WHERE (media_id=? OR id=?) AND budget='originals' AND cleanup_ack=0`, request.MediaID, inputArtifact).Scan(&n); err != nil {
			return MediaReservation{}, ErrArchiveUnavailable
		}
		if n > 0 {
			return MediaReservation{}, ErrNoWork
		}
		inputArtifact = ""
	}
	r := MediaReservation{Source: request.Source, MediaID: request.MediaID, Budget: request.Budget, MaximumBytes: request.MaximumBytes}
	for _, dst := range []*string{(*string)(&r.ID), (*string)(&r.ArtifactID), (*string)(&r.LeaseID)} {
		v, err := randomID()
		if err != nil {
			return MediaReservation{}, err
		}
		*dst = v
	}
	now := s.now()
	r.LeaseUntil = fromMillis(unixms(now.Add(request.TTL)))
	_, err = tx.ExecContext(ctx, `INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,created_at,updated_at,input_artifact_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'reserved',?,?,?)`, r.ArtifactID, r.ID, r.MediaID, r.Source.Account, r.Source.Version.MessageID, r.Source.Version.Revision, r.Source.Version.PolicyGeneration, r.Source.ContentHash, unixms(r.Source.Version.ExpiresAt), r.Budget, r.MaximumBytes, r.MaximumBytes, r.LeaseID, unixms(r.LeaseUntil), unixms(now), unixms(now), inputArtifact)
	if err != nil {
		return MediaReservation{}, ErrArchiveUnavailable
	}
	if r.Budget == BudgetOriginals {
		_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET input_artifact_id=? WHERE media_id=? AND account=? AND message_id=? AND local_revision=? AND policy_generation=? AND content_hash=? AND budget='decoded_work' AND cleanup_ack=0 AND input_artifact_id=''`, r.ArtifactID, r.MediaID, r.Source.Account, r.Source.Version.MessageID, r.Source.Version.Revision, r.Source.Version.PolicyGeneration, r.Source.ContentHash)
		if err != nil {
			return MediaReservation{}, ErrArchiveUnavailable
		}
	}
	if request.OwnerDownloadLease.IntentID != "" {
		_, err = tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET reservation_id=?,artifact_id=? WHERE id=? AND lease_id=?`, r.ID, r.ArtifactID, request.OwnerDownloadLease.IntentID, request.OwnerDownloadLease.LeaseID)
		if err != nil {
			return MediaReservation{}, fixedSQLError(err)
		}
	}
	return r, nil
}
func (s *Store) extensionRenewMedia(ctx context.Context, tx *sql.Tx, r MediaReservation, ttl time.Duration) (time.Time, error) {
	if _, _, err := s.ownerVoiceBound(ctx, tx, r); err != nil {
		return time.Time{}, err
	}
	v, err := extensionLoadMedia(ctx, tx, r.ArtifactID)
	if err != nil {
		return time.Time{}, err
	}
	if !extensionMediaLeaseMatches(v.reservation, r) || v.cleanupAck || (v.artifact.State != "reserved" && v.artifact.State != "staged") || !s.now().Before(v.reservation.LeaseUntil) {
		return time.Time{}, ErrNoWork
	}
	if _, _, err = s.extensionMediaAssociation(ctx, tx, r.Source, r.MediaID); err != nil {
		return time.Time{}, err
	}
	deadline := fromMillis(unixms(s.now().Add(ttl)))
	if !deadline.After(v.reservation.LeaseUntil) {
		deadline = v.reservation.LeaseUntil
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET lease_until=?,updated_at=? WHERE id=?`, unixms(deadline), unixms(s.now()), r.ArtifactID)
	if err != nil {
		return time.Time{}, ErrArchiveUnavailable
	}
	return deadline, nil
}
func (s *Store) extensionPublishOriginal(ctx context.Context, tx *sql.Tx, p OriginalPublication) (bool, error) {
	r := p.Reservation
	owner, owned, ownerErr := s.ownerVoiceBound(ctx, tx, r)
	if ownerErr != nil {
		return false, ownerErr
	}
	if owned && p.SHA256 != owner.snapshot.ExpectedSHA {
		return false, ErrInvalidExtension
	}
	v, err := extensionLoadMedia(ctx, tx, r.ArtifactID)
	if errors.Is(err, ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !extensionMediaLeaseMatches(v.reservation, r) || v.cleanupAck {
		return false, nil
	}
	if v.artifact.State != "reserved" && v.artifact.State != "staged" && v.artifact.State != "retained" {
		return false, nil
	}
	kind, _, current := s.extensionMediaAssociation(ctx, tx, r.Source, r.MediaID)
	if current != nil && !errors.Is(current, ErrSourceUnavailable) && !errors.Is(current, ErrPolicyChanged) {
		return false, current
	}
	if current != nil || (v.artifact.State != "retained" && !s.now().Before(v.reservation.LeaseUntil)) {
		// A completed shared file can outlive its original admitting source.
		// A late retry by that source cannot cancel other current associations.
		if v.artifact.State == "retained" {
			return false, nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET state='evicting',cleanup_lease=CASE WHEN cleanup_lease='' THEN lease_id ELSE cleanup_lease END,updated_at=? WHERE id=?`, unixms(s.now()), r.ArtifactID)
		if err != nil {
			return false, ErrArchiveUnavailable
		}
		return false, nil
	}
	if !extensionMediaKindFormat(kind, p.ActualFormat) || (kind == "document" && p.Bytes > MaximumDocumentBytes) {
		return false, ErrInvalidExtension
	}
	if v.artifact.State == "retained" {
		return v.artifact.SHA256 == p.SHA256 && v.artifact.Bytes == p.Bytes && v.format == p.ActualFormat, nil
	}
	var previousSHA string
	if err = tx.QueryRowContext(ctx, `SELECT last_original_sha256 FROM extension_media_associations WHERE id=?`, r.MediaID).Scan(&previousSHA); err != nil {
		return false, readError(ctx, err)
	}
	if previousSHA != "" && previousSHA != p.SHA256 {
		if err = s.extensionInvalidateMedia(ctx, tx, r.MediaID); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET state='retained',sha256=?,bytes=?,charged_bytes=?,actual_format=?,updated_at=? WHERE id=?`, p.SHA256, p.Bytes, p.Bytes, p.ActualFormat, unixms(s.now()), r.ArtifactID)
	if err != nil {
		return false, ErrArchiveUnavailable
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_media_associations SET artifact_id=?,availability='retained',last_original_sha256=? WHERE id=? AND active=1`, r.ArtifactID, p.SHA256, r.MediaID)
	if err != nil {
		return false, ErrArchiveUnavailable
	}
	if owned {
		_, err = tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state='complete',completed_sha256=?,protocol_reference=X'' WHERE id=? AND artifact_id=? AND reservation_id=?`, p.SHA256, owner.snapshot.Intent.ID, r.ArtifactID, r.ID)
		if err != nil {
			return false, fixedSQLError(err)
		}
	}
	if err = extensionCompleteOriginalDownloadPublication(ctx, tx, r.MediaID, p.SHA256); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET input_artifact_id=? WHERE media_id=? AND budget='decoded_work' AND cleanup_ack=0 AND input_artifact_id=''`, r.ArtifactID, r.MediaID)
	if err != nil {
		return false, ErrArchiveUnavailable
	}
	_, err = tx.ExecContext(ctx, `UPDATE messages SET media_availability='retained' WHERE id=?`, r.Source.Version.MessageID)
	if err != nil {
		return false, ErrArchiveUnavailable
	}
	if _, err = bumpRevision(ctx, tx); err != nil {
		return false, ErrArchiveUnavailable
	}
	return true, nil
}

// Extraction jobs require an exact authorized source association and retained
// bytes. Derived embedding does not pin originals; valid derivatives survive
// ordinary cache eviction.
func (s *Store) extensionMediaJobEligible(ctx context.Context, q sqlQuerier, j Job) (bool, error) {
	if j.Kind != JobOCR && j.Kind != JobCaption && j.Kind != JobTranscription {
		return true, nil
	}
	if len(j.Sources) != 1 || j.MediaID == "" {
		return false, nil
	}
	_, artifact, err := s.extensionMediaAssociation(ctx, q, j.Sources[0], j.MediaID)
	if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if artifact == "" {
		return false, nil
	}
	var n int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts WHERE id=? AND state='retained' AND budget='originals' AND cleanup_ack=0`, artifact).Scan(&n)
	if err != nil {
		return false, readError(ctx, err)
	}
	return n == 1, nil
}

type extensionMediaLink struct {
	source SourceRef
	media  MediaID
	kind   string
}

func extensionMediaLinks(ctx context.Context, q sqlQuerier, where string, args ...any) ([]extensionMediaLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.id,l.account,l.message_id,l.local_revision,l.policy_generation,l.content_hash,l.source_expires_at,l.kind FROM extension_media_associations l `+where, args...)
	if err != nil {
		return nil, readError(ctx, err)
	}
	defer rows.Close()
	var links []extensionMediaLink
	for rows.Next() {
		var link extensionMediaLink
		var expiry int64
		if err = rows.Scan(&link.media, &link.source.Account, &link.source.Version.MessageID, &link.source.Version.Revision, &link.source.Version.PolicyGeneration, &link.source.ContentHash, &expiry, &link.kind); err != nil {
			return nil, readError(ctx, err)
		}
		link.source.Version.ExpiresAt = fromMillis(expiry)
		links = append(links, link)
		if len(links) > 2048 {
			return nil, ErrLimitExceeded
		}
	}
	if err = rows.Err(); err != nil {
		return nil, readError(ctx, err)
	}
	return links, nil
}

// Timestamp storage has millisecond precision, while retention and the clock
// retain nanoseconds. This expression is exactly ceil(now-retention): the
// quotient removes whole milliseconds, then the remainder decides the ceiling.
const extensionMediaRollingSQL = `(ap.rolling_retention=0 OR m.original_sent_at>=?-ap.rolling_retention/1000000+CASE WHEN ap.rolling_retention%1000000<? THEN 1 ELSE 0 END)`

func extensionMediaClockRemainder(now time.Time) int64 {
	return int64(now.Nanosecond()) % int64(time.Millisecond)
}

const extensionMediaCurrentRetainedSQL = `JOIN extension_media_artifacts a ON a.id=l.artifact_id WHERE l.active=1 AND l.view_once=0 AND a.state='retained' AND a.budget='originals' AND a.cleanup_ack=0 AND EXISTS(SELECT 1 FROM ` + authorizedTables + ` WHERE m.id=l.message_id AND m.account=l.account AND m.state='active' AND m.original_date_known=1 AND m.view_once=0 AND m.local_revision=l.local_revision AND m.content_hash=l.content_hash AND m.expires_at=l.source_expires_at AND ap.generation=l.policy_generation AND (m.expires_at=0 OR m.expires_at>?) AND (ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id)) AND (ap.date_mode='all_available' OR m.original_sent_at>=ap.import_start) AND ` + extensionMediaRollingSQL + `)`
const extensionMediaJobSourceSQL = `js.account=l.account AND js.message_id=l.message_id AND js.local_revision=l.local_revision AND js.policy_generation=l.policy_generation AND js.content_hash=l.content_hash AND js.expires_at=l.source_expires_at AND (SELECT count(*) FROM extension_job_sources extra WHERE extra.job_id=j.id)=1`
const extensionMediaMissingJobsSQL = `((l.kind='image' AND (NOT EXISTS(SELECT 1 FROM extension_jobs j JOIN extension_job_sources js ON js.job_id=j.id WHERE j.generation=? AND j.media_id=l.id AND j.kind='ocr' AND j.state<>'cancelled' AND ` + extensionMediaJobSourceSQL + `) OR NOT EXISTS(SELECT 1 FROM extension_jobs j JOIN extension_job_sources js ON js.job_id=j.id WHERE j.generation=? AND j.media_id=l.id AND j.kind='caption' AND j.state<>'cancelled' AND ` + extensionMediaJobSourceSQL + `))) OR (l.kind='voice' AND NOT EXISTS(SELECT 1 FROM extension_jobs j JOIN extension_job_sources js ON js.job_id=j.id WHERE j.generation=? AND j.media_id=l.id AND j.kind='transcription' AND j.state<>'cancelled' AND ` + extensionMediaJobSourceSQL + `)))`

// Lazy seeding must not make an old generation's completed jobs sufficient to
// evict bytes before a replacement generation has received its required jobs.
func extensionMediaUnseededGenerationSQL() string {
	return `EXISTS(SELECT 1 FROM extension_generations g WHERE g.state IN ('active','building') AND ` + strings.ReplaceAll(extensionMediaMissingJobsSQL, "j.generation=?", "j.generation=g.id") + `)`
}

// Verified primary outcomes are independent of every encoder generation.
// Failures and legacy job state alone never manufacture successful coverage.
func extensionMediaExtractionDoneSQL(kind string) string {
	return `EXISTS(SELECT 1 FROM extension_extraction_jobs x JOIN extension_extraction_versions ev ON ev.id=x.version_id WHERE x.media_id=l.id AND x.kind=` + kind + ` AND x.account=l.account AND x.message_id=l.message_id AND x.local_revision=l.local_revision AND x.policy_generation=l.policy_generation AND x.content_hash=l.content_hash AND x.source_expires_at=l.source_expires_at AND x.original_sha256=l.last_original_sha256 AND x.original_sha256=(SELECT original.sha256 FROM extension_media_artifacts original WHERE original.id=l.artifact_id) AND x.input_pinned=0 AND x.state='complete' AND (
	 (x.outcome='text' AND (SELECT count(*) FROM extension_extraction_outputs o WHERE o.job_id=x.id) BETWEEN 1 AND 256
	 AND (SELECT min(ordinal) FROM extension_extraction_outputs o WHERE o.job_id=x.id)=0
	 AND (SELECT max(ordinal)+1 FROM extension_extraction_outputs o WHERE o.job_id=x.id)=(SELECT count(*) FROM extension_extraction_outputs o WHERE o.job_id=x.id)
	 AND NOT EXISTS(SELECT 1 FROM extension_extraction_outputs o LEFT JOIN extension_roots r ON r.id=o.root_id WHERE o.job_id=x.id AND (
	 r.id IS NULL OR r.extraction_job_id IS NOT x.id OR r.media_id<>l.id OR r.source_count<>1 OR r.kind<>CASE x.kind WHEN 'transcription' THEN 'transcript' ELSE x.kind END
	 OR (SELECT count(*) FROM extension_root_sources rs WHERE rs.root_id=r.id)<>1
	 OR NOT EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND rs.ordinal=0 AND rs.account=l.account AND rs.message_id=l.message_id AND rs.local_revision=l.local_revision AND rs.policy_generation=l.policy_generation AND rs.content_hash=l.content_hash AND rs.expires_at=l.source_expires_at)
	 OR json_extract(r.provenance_json,'$.Kind') IS NOT r.kind OR json_extract(r.provenance_json,'$.MediaID') IS NOT l.id
	 OR json_extract(r.provenance_json,'$.ProfileID') IS NOT json_extract(ev.version_json,'$.ProfileID') OR json_extract(r.provenance_json,'$.ExtractorVersion') IS NOT json_extract(ev.version_json,'$.ExtractorVersion') OR json_extract(r.provenance_json,'$.PromptVersion') IS NOT json_extract(ev.version_json,'$.PromptVersion'))))
	 OR (x.outcome='no_text' AND x.kind IN ('ocr','caption') AND NOT EXISTS(SELECT 1 FROM extension_extraction_outputs o WHERE o.job_id=x.id))
	 OR (x.outcome='silent' AND x.kind='transcription' AND NOT EXISTS(SELECT 1 FROM extension_extraction_outputs o WHERE o.job_id=x.id))))`
}
func extensionMediaIndependentProcessedSQL() string {
	return `(l.kind='document' OR (l.kind='image' AND ` + extensionMediaExtractionDoneSQL(`'ocr'`) + ` AND ` + extensionMediaExtractionDoneSQL(`'caption'`) + `) OR (l.kind='voice' AND ` + extensionMediaExtractionDoneSQL(`'transcription'`) + `))`
}
func extensionMediaReadyForEvictionSQL() string {
	return `NOT EXISTS(SELECT 1 FROM extension_extraction_jobs x WHERE x.media_id=l.id AND x.state IN ('pending','leased'))
	 AND NOT EXISTS(SELECT 1 FROM extension_media_artifacts d WHERE d.media_id=l.id AND d.budget='decoded_work' AND d.cleanup_ack=0)
	 AND ` + extensionMediaIndependentProcessedSQL()
}
func extensionExtractionArtifactPinned(ctx context.Context, q sqlQuerier, id ArtifactID) (bool, error) {
	var pins int
	// Malformed byte-equal references still deny physical removal. They do not
	// authorize input loading or settlement of the unresolved owner's lease.
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM extension_extraction_jobs WHERE (artifact_id=? OR artifact_id=CAST(? AS BLOB)) AND input_pinned=1`, id, id).Scan(&pins)
	if err != nil {
		return false, readError(ctx, err)
	}
	return pins != 0, nil
}

// Generation creation and media claims each seed one bounded batch. Missing
// coverage remains visible to activation until subsequent claims finish seeding.
func (s *Store) extensionSeedMediaJobs(ctx context.Context, tx *sql.Tx, g IndexGenerationID) error {
	now := s.now()
	links, err := extensionMediaLinks(ctx, tx, extensionMediaCurrentRetainedSQL+` AND `+extensionMediaMissingJobsSQL+` ORDER BY l.id LIMIT ?`, unixms(now), unixms(now), extensionMediaClockRemainder(now), g, g, g, extensionMediaBatch)
	if err != nil {
		return err
	}
	for _, l := range links {
		if _, _, err = s.extensionMediaAssociation(ctx, tx, l.source, l.media); errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
			continue
		} else if err != nil {
			return err
		}
		if err = s.extensionEnqueueMediaJobsForGeneration(ctx, tx, l.source, l.media, l.kind, g); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) extensionMediaCoverageAt(ctx context.Context, q sqlQuerier, g IndexGenerationID, now time.Time) error {
	var missing int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM extension_media_associations l `+extensionMediaCurrentRetainedSQL+` AND `+extensionMediaMissingJobsSQL+` LIMIT 1`, unixms(now), unixms(now), extensionMediaClockRemainder(now), g, g, g).Scan(&missing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return readError(ctx, err)
	}
	return ErrNoWork
}
func (s *Store) extensionSeedNextMediaJobs(ctx context.Context, tx *sql.Tx, claim JobClaim) error {
	kind := "image"
	switch claim.Kind {
	case JobOCR, JobCaption:
	case JobTranscription:
		kind = "voice"
	default:
		return nil
	}
	now := s.now()
	// Scope each batch to useful progress for the requested account and worker.
	missing := `NOT EXISTS(SELECT 1 FROM extension_jobs j JOIN extension_job_sources js ON js.job_id=j.id WHERE j.generation=g.id AND j.media_id=l.id AND j.kind=? AND j.state<>'cancelled' AND ` + extensionMediaJobSourceSQL + `)`
	var generation IndexGenerationID
	err := tx.QueryRowContext(ctx, `SELECT g.id FROM extension_generations g WHERE g.state IN ('active','building') AND EXISTS(SELECT 1 FROM extension_media_associations l `+extensionMediaCurrentRetainedSQL+` AND l.account=? AND l.kind=? AND `+missing+`) ORDER BY g.created_at,g.id LIMIT 1`, unixms(now), unixms(now), extensionMediaClockRemainder(now), claim.Account, kind, claim.Kind).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return readError(ctx, err)
	}
	missing = `NOT EXISTS(SELECT 1 FROM extension_jobs j JOIN extension_job_sources js ON js.job_id=j.id WHERE j.generation=? AND j.media_id=l.id AND j.kind=? AND j.state<>'cancelled' AND ` + extensionMediaJobSourceSQL + `)`
	links, err := extensionMediaLinks(ctx, tx, extensionMediaCurrentRetainedSQL+` AND l.account=? AND l.kind=? AND `+missing+` ORDER BY l.id LIMIT ?`, unixms(now), unixms(now), extensionMediaClockRemainder(now), claim.Account, kind, generation, claim.Kind, extensionMediaBatch)
	if err != nil {
		return err
	}
	for _, link := range links {
		if _, _, err = s.extensionMediaAssociation(ctx, tx, link.source, link.media); errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
			continue
		} else if err != nil {
			return err
		}
		if err = s.extensionEnqueueMediaJobsForGeneration(ctx, tx, link.source, link.media, link.kind, generation); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) extensionMediaEvictable(ctx context.Context, q sqlQuerier, id ArtifactID) (bool, MediaID, error) {
	if pinned, err := extensionExtractionArtifactPinned(ctx, q, id); err != nil {
		return false, "", err
	} else if pinned {
		return false, "", nil
	}
	var pins int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts WHERE budget='decoded_work' AND cleanup_ack=0 AND (input_artifact_id=? OR input_artifact_id=CAST(? AS BLOB))`, id, id).Scan(&pins); err != nil {
		return false, "", readError(ctx, err)
	}
	if pins != 0 {
		return false, "", nil
	}
	links, err := extensionMediaLinks(ctx, q, `WHERE l.artifact_id=? AND l.active=1 ORDER BY l.id`, id)
	if err != nil {
		return false, "", err
	}
	if len(links) == 0 {
		return false, "", nil
	}
	for _, l := range links {
		if _, err = s.extensionCheckSources(ctx, q, []SourceRef{l.source}, true); errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) || errors.Is(err, ErrReconciliationRequired) {
			return false, "", nil
		} else if err != nil {
			return false, "", err
		}
		var n int
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_associations l WHERE l.id=? AND `+extensionMediaReadyForEvictionSQL(), l.media).Scan(&n); err != nil {
			return false, "", readError(ctx, err)
		}
		if n != 1 {
			return false, "", nil
		}
	}
	return true, links[0].media, nil
}

func (s *Store) extensionEvictionCandidates(ctx context.Context, tx *sql.Tx, limit int) ([]MediaID, error) {
	// Apply complete association/job eligibility before the bounded candidate
	// inventory. Pending originals must not hide later processed originals.
	query := `SELECT a.id FROM extension_media_artifacts a WHERE a.state='retained' AND a.budget='originals' AND a.cleanup_ack=0
	 AND NOT EXISTS(SELECT 1 FROM extension_extraction_jobs x WHERE (x.artifact_id=a.id OR x.artifact_id=CAST(a.id AS BLOB)) AND x.input_pinned=1)
	 AND NOT EXISTS(SELECT 1 FROM extension_media_artifacts d WHERE d.budget='decoded_work' AND d.cleanup_ack=0 AND (d.input_artifact_id=a.id OR d.input_artifact_id=CAST(a.id AS BLOB)))
	 AND EXISTS(SELECT 1 FROM extension_media_associations l WHERE l.artifact_id=a.id AND l.active=1)
	 AND NOT EXISTS(SELECT 1 FROM extension_media_associations l WHERE l.artifact_id=a.id AND l.active=1 AND (
	 l.view_once<>0 OR NOT EXISTS(SELECT 1 FROM ` + authorizedTables + ` WHERE m.id=l.message_id AND m.account=l.account AND m.state='active' AND m.original_date_known=1 AND m.view_once=0 AND m.local_revision=l.local_revision AND m.content_hash=l.content_hash AND m.expires_at=l.source_expires_at AND ap.generation=l.policy_generation AND (m.expires_at=0 OR m.expires_at>?) AND (ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id)) AND (ap.date_mode='all_available' OR m.original_sent_at>=ap.import_start) AND ` + extensionMediaRollingSQL + `)
	 OR NOT (` + extensionMediaReadyForEvictionSQL() + `)))
	 ORDER BY a.updated_at,a.id LIMIT ?`
	now := s.now()
	rows, err := tx.QueryContext(ctx, query, unixms(now), unixms(now), extensionMediaClockRemainder(now), extensionMediaBatch)
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	var ids []ArtifactID
	for rows.Next() {
		var id ArtifactID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, ErrArchiveUnavailable
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	result := make([]MediaID, 0, limit)
	for _, id := range ids {
		ok, media, err := s.extensionMediaEvictable(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, media)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}
func (s *Store) extensionBeginEviction(ctx context.Context, tx *sql.Tx, media MediaID) (MediaArtifact, error) {
	var id ArtifactID
	err := tx.QueryRowContext(ctx, `SELECT artifact_id FROM extension_media_associations WHERE id=? AND artifact_id<>''`, media).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaArtifact{}, ErrNoWork
	}
	if err != nil {
		return MediaArtifact{}, ErrArchiveUnavailable
	}
	v, err := extensionLoadMedia(ctx, tx, id)
	if err != nil {
		return MediaArtifact{}, err
	}
	if v.cleanupAck || v.artifact.Budget != BudgetOriginals {
		return MediaArtifact{}, ErrNoWork
	}
	if pinned, err := extensionExtractionArtifactPinned(ctx, tx, id); err != nil {
		return MediaArtifact{}, err
	} else if pinned {
		return MediaArtifact{}, ErrNoWork
	}
	var pins int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts WHERE budget='decoded_work' AND cleanup_ack=0 AND (input_artifact_id=? OR input_artifact_id=CAST(? AS BLOB))`, id, id).Scan(&pins); err != nil {
		return MediaArtifact{}, ErrArchiveUnavailable
	}
	if pins != 0 {
		return MediaArtifact{}, ErrNoWork
	}
	if v.artifact.State == "retained" {
		ok, _, err := s.extensionMediaEvictable(ctx, tx, id)
		if err != nil {
			return MediaArtifact{}, err
		}
		if !ok {
			return MediaArtifact{}, ErrNoWork
		}
	} else if v.artifact.State != "evicting" {
		return MediaArtifact{}, ErrNoWork
	}
	if v.artifact.CleanupLease == "" {
		lease, err := randomID()
		if err != nil {
			return MediaArtifact{}, err
		}
		v.artifact.CleanupLease = LeaseID(lease)
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET state='evicting',cleanup_lease=?,updated_at=? WHERE id=?`, v.artifact.CleanupLease, unixms(s.now()), id)
	if err != nil {
		return MediaArtifact{}, ErrArchiveUnavailable
	}
	v.artifact.State = "evicting"
	if _, err = bumpRevision(ctx, tx); err != nil {
		return MediaArtifact{}, ErrArchiveUnavailable
	}
	return v.artifact, nil
}
func (s *Store) extensionFinishMediaCleanup(ctx context.Context, tx *sql.Tx, c MediaCleanup) error {
	v, err := extensionLoadMedia(ctx, tx, c.ArtifactID)
	if err != nil {
		return err
	}
	lease := v.artifact.CleanupLease
	if lease == "" && (v.artifact.State == "reserved" || v.artifact.State == "staged" || (v.artifact.State == "evicting" && v.artifact.Bytes == 0)) {
		lease = v.reservation.LeaseID
	}
	if v.artifact.ReservationID != c.ReservationID || lease == "" || lease != c.LeaseID {
		return ErrNoWork
	}
	if v.cleanupAck {
		return nil
	}
	if v.artifact.State == "retained" {
		return ErrNoWork
	}
	if v.artifact.Budget == BudgetOriginals {
		if pinned, err := extensionExtractionArtifactPinned(ctx, tx, c.ArtifactID); err != nil {
			return err
		} else if pinned {
			return ErrNoWork
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts WHERE budget='decoded_work' AND cleanup_ack=0 AND (input_artifact_id=? OR input_artifact_id=CAST(? AS BLOB))`, c.ArtifactID, c.ArtifactID).Scan(&n); err != nil {
			return ErrArchiveUnavailable
		}
		if n > 0 {
			return ErrNoWork
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET state='evicting',charged_bytes=0,cleanup_ack=1,cleanup_lease=?,updated_at=? WHERE id=?`, lease, unixms(s.now()), c.ArtifactID)
	if err != nil {
		return ErrArchiveUnavailable
	}
	_, err = tx.ExecContext(ctx, `UPDATE messages SET media_availability='not_retained' WHERE id IN (SELECT message_id FROM extension_media_associations WHERE artifact_id=?) AND NOT EXISTS(SELECT 1 FROM extension_media_associations l JOIN extension_media_artifacts a ON a.id=l.artifact_id WHERE l.message_id=messages.id AND l.active=1 AND a.id<>? AND a.state='retained' AND a.cleanup_ack=0)`, c.ArtifactID, c.ArtifactID)
	if err != nil {
		return ErrArchiveUnavailable
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_media_associations SET artifact_id='',availability='not_retained' WHERE artifact_id=?`, c.ArtifactID)
	if err != nil {
		return ErrArchiveUnavailable
	}
	return nil
}
func (s *Store) extensionReconcileMedia(ctx context.Context, tx *sql.Tx, limit int) ([]MediaArtifact, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+extensionMediaArtifactColumns+`,a.lease_id FROM extension_media_artifacts a WHERE a.cleanup_ack=0 AND a.state IN ('reserved','staged','evicting')
	 AND (a.budget<>'originals' OR (
	 NOT EXISTS(SELECT 1 FROM extension_extraction_jobs x WHERE (x.artifact_id=a.id OR x.artifact_id=CAST(a.id AS BLOB)) AND x.input_pinned=1)
	 AND NOT EXISTS(SELECT 1 FROM extension_media_artifacts d WHERE d.budget='decoded_work' AND d.cleanup_ack=0 AND (d.input_artifact_id=a.id OR d.input_artifact_id=CAST(a.id AS BLOB)))))
	 ORDER BY CASE a.budget WHEN 'decoded_work' THEN 0 ELSE 1 END,a.created_at,a.id LIMIT ?`, limit)
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	var result []MediaArtifact
	for rows.Next() {
		var a MediaArtifact
		var expiry int64
		var downloadLease LeaseID
		if err = rows.Scan(&a.ID, &a.MediaID, &a.Source.Account, &a.Source.Version.MessageID, &a.Source.Version.Revision, &a.Source.Version.PolicyGeneration, &a.Source.ContentHash, &expiry, &a.SHA256, &a.Bytes, &a.ReservationID, &a.CleanupLease, &a.Budget, &a.ReservedBytes, &a.State, &a.ActualFormat, &downloadLease); err != nil {
			rows.Close()
			return nil, ErrArchiveUnavailable
		}
		a.Source.Version.ExpiresAt = fromMillis(expiry)
		if a.CleanupLease == "" {
			if a.State == "reserved" || a.State == "staged" || a.Bytes == 0 {
				a.CleanupLease = downloadLease
			} else {
				lease, err := randomID()
				if err != nil {
					rows.Close()
					return nil, err
				}
				a.CleanupLease = LeaseID(lease)
			}
		}
		result = append(result, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	for _, a := range result {
		_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET cleanup_lease=? WHERE id=? AND cleanup_lease=''`, a.CleanupLease, a.ID)
		if err != nil {
			return nil, ErrArchiveUnavailable
		}
	}
	return result, nil
}

func (s *Store) extensionInspectMedia(ctx context.Context, tx *sql.Tx, r MediaReservation) (MediaArtifact, error) {
	v, err := extensionLoadMedia(ctx, tx, r.ArtifactID)
	if err != nil {
		return MediaArtifact{}, err
	}
	if !extensionMediaLeaseMatches(v.reservation, r) {
		return MediaArtifact{}, ErrNoWork
	}
	a := v.artifact
	if a.State != "retained" && a.Budget == BudgetOriginals {
		if pinned, err := extensionExtractionArtifactPinned(ctx, tx, a.ID); err != nil {
			return MediaArtifact{}, err
		} else if pinned {
			return MediaArtifact{}, ErrNoWork
		}
		var pins int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts WHERE budget='decoded_work' AND cleanup_ack=0 AND (input_artifact_id=? OR input_artifact_id=CAST(? AS BLOB))`, a.ID, a.ID).Scan(&pins); err != nil {
			return MediaArtifact{}, ErrArchiveUnavailable
		}
		if pins != 0 {
			return MediaArtifact{}, ErrNoWork
		}
	}
	if a.State != "retained" && a.CleanupLease == "" {
		if a.State == "reserved" || a.State == "staged" || a.Bytes == 0 {
			a.CleanupLease = v.reservation.LeaseID
		} else {
			lease, err := randomID()
			if err != nil {
				return MediaArtifact{}, err
			}
			a.CleanupLease = LeaseID(lease)
		}
		_, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET cleanup_lease=? WHERE id=? AND cleanup_lease=''`, a.CleanupLease, a.ID)
		if err != nil {
			return MediaArtifact{}, ErrArchiveUnavailable
		}
	}
	return a, nil
}
func (s *Store) AuthorizeOriginal(ctx context.Context, scope ReadScope, media MediaID) (MediaArtifact, error) {
	if !validReadID(string(media)) {
		return MediaArtifact{}, ErrInvalidExtension
	}
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return MediaArtifact{}, err
	}
	defer f.close()
	args := append(append([]any(nil), f.args...), media)
	query := `SELECT ` + extensionMediaArtifactColumns + `,m.id,m.content_hash,m.original_sent_at,m.expires_at,m.local_revision FROM ` + authorizedTables + `
	 JOIN extension_media_associations l ON l.message_id=m.id AND l.account=m.account AND l.local_revision=m.local_revision AND l.policy_generation=ap.generation AND l.content_hash=m.content_hash AND l.source_expires_at=m.expires_at
	 JOIN extension_media_artifacts a ON a.id=l.artifact_id WHERE ` + f.where + ` AND l.id=? AND l.active=1 AND l.view_once=0 AND m.view_once=0 AND a.budget='originals' AND a.state='retained' AND a.cleanup_ack=0`
	var a MediaArtifact
	var sourceExpiry, sent, expiry int64
	var revision uint64
	var message MessageID
	var hash string
	err = f.tx.QueryRowContext(f.ctx, query, args...).Scan(&a.ID, &a.MediaID, &a.Source.Account, &a.Source.Version.MessageID, &a.Source.Version.Revision, &a.Source.Version.PolicyGeneration, &a.Source.ContentHash, &sourceExpiry, &a.SHA256, &a.Bytes, &a.ReservationID, &a.CleanupLease, &a.Budget, &a.ReservedBytes, &a.State, &a.ActualFormat, &message, &hash, &sent, &expiry, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaArtifact{}, ErrSourceUnavailable
	}
	if err != nil {
		return MediaArtifact{}, readError(f.ctx, err)
	}
	// A true shared artifact retains its own cleanup identity, while authority
	// always comes from the requested association and current original source.
	a.MediaID = media
	a.Source = SourceRef{Account: scope.Account, ContentHash: hash, Version: SourceVersion{MessageID: message, Revision: revision, PolicyGeneration: scope.PolicyGeneration, ExpiresAt: fromMillis(expiry)}}
	f.pin(message, revision)
	f.note(fromMillis(sent), fromMillis(expiry))
	if err = f.finish(); err != nil {
		return MediaArtifact{}, err
	}
	return a, nil
}

var _ MediaStore = (*Store)(nil)
var _ MediaReservationInspector = (*Store)(nil)
