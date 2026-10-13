package archive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
	"strings"
	"time"
)

type OwnerVoiceIntent struct {
	ID               string
	Key              SourceKey
	Source           SourceRef
	MediaID          MediaID
	OriginalSentAt   time.Time
	SourceRevision   int64
	RequestID        string
	RequestExpiresAt time.Time
}
type OwnerVoiceRequestRegistration struct {
	IntentID  string
	RequestID string
	Source    SourceRef
}
type OwnerVoiceDescriptorReply struct {
	RequestID      string
	Key            SourceKey
	OriginalSentAt time.Time
	SourceRevision int64
	ExpiresAt      time.Time
	Media          MediaReference
}
type OwnerVoiceRecoveryStatus struct {
	AwaitingPhone  int    `json:"awaiting_phone"`
	WaitingCleanup int    `json:"waiting_cleanup"`
	Queued         int    `json:"queued"`
	Downloading    int    `json:"downloading"`
	Completed      int    `json:"completed"`
	Failed         int    `json:"failed"`
	Code           string `json:"code,omitempty"`
}
type OwnerVoiceRecoveryStore interface {
	RecordOwnerVoiceReplyDiagnostic(context.Context, string, SourceRef, string) (bool, error)
	RecordOwnerVoiceRestriction(context.Context, string, SourceKey, time.Time, bool) error
	PrepareOwnerVoiceRedownloads(context.Context, ReadScope, int) ([]OwnerVoiceIntent, error)
	RegisterOwnerVoiceRequest(context.Context, OwnerVoiceRequestRegistration) error
	LookupOwnerVoiceRequest(context.Context, string) (OwnerVoiceIntent, bool, error)
	AcceptOwnerVoiceDescriptor(context.Context, OwnerVoiceDescriptorReply) (bool, error)
	OwnerVoiceRecoveryStatus(context.Context, ReadScope) (OwnerVoiceRecoveryStatus, error)
}

// The ledger is separate from initial-download completion evidence. Its bounded
// snapshot contains no protocol bytes and cannot restore source message bodies.
type ownerVoiceSnapshot struct {
	Intent                                    OwnerVoiceIntent
	Scope                                     ReadScope
	ChatID                                    ChatID
	Identity, ExpectedSHA, Descriptor, Legacy string
	CurrentDescriptor                         string
	PriorPolicy                               Generation
	Ordinal                                   int
	Declared                                  int64
	Mime                                      string
}
type ownerVoiceRow struct {
	snapshot                                                                ownerVoiceSnapshot
	state, request, lease, protocol, reservation, artifact, completed, code string
	expires, until, due                                                     int64
	attempts                                                                int
}
type ownerVoiceCommand struct {
	op           string
	scope        ReadScope
	limit        int
	registration OwnerVoiceRequestRegistration
	reply        OwnerVoiceDescriptorReply
	request      string
	diagnostic   string
	source       SourceRef
}

func (c ownerVoiceCommand) priority() bool                    { return false }
func (c ownerVoiceCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c ownerVoiceCommand) account() string                   { return c.scope.Account }
func (c ownerVoiceCommand) run(s *Store, ctx context.Context) (any, error) {
	defer clear(c.reply.Media.ProtocolReference)
	var v any
	err := s.extensionTransaction(ctx, c.account(), func(tx *sql.Tx) error {
		var err error
		switch c.op {
		case "reply_diagnostic":
			v, err = s.ownerVoiceReplyDiagnostic(ctx, tx, c.request, c.source, c.diagnostic)
		case "prepare":
			v, err = s.ownerVoicePrepare(ctx, tx, c.scope, c.limit)
		case "register":
			err = s.ownerVoiceRegister(ctx, tx, c.registration)
		case "lookup":
			var r ownerVoiceRow
			r, err = ownerVoiceLoad(ctx, tx, "request_id", c.request)
			if errors.Is(err, ErrNoWork) {
				v = ownerVoiceLookup{}
				return nil
			}
			if err == nil {
				v = ownerVoiceLookup{r.intent(), true}
			}
		case "accept":
			v, err = s.ownerVoiceAccept(ctx, tx, c.reply)
		case "status":
			v, err = s.ownerVoiceStatus(ctx, tx, c.scope)
		default:
			err = ErrInvalidExtension
		}
		return err
	})
	if c.op == "accept" && (errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrNoWork) || errors.Is(err, ErrPolicyChanged)) {
		return false, nil
	}
	return v, err
}

// These codes are private attempt evidence, never response content or authority.
const ownerVoiceReplyDiagnosticSQL = `'voice_reply_invalid_envelope','voice_reply_key_mismatch','voice_reply_original_mismatch','voice_reply_no_audio','voice_reply_invalid_descriptor','voice_reply_not_accepted','voice_reply_bodyless','voice_reply_mutation','voice_reply_restricted'`

func validOwnerVoiceReplyDiagnostic(code string) bool {
	switch code {
	case "voice_reply_invalid_envelope", "voice_reply_key_mismatch", "voice_reply_original_mismatch", "voice_reply_no_audio", "voice_reply_invalid_descriptor", "voice_reply_not_accepted", "voice_reply_bodyless", "voice_reply_mutation", "voice_reply_restricted":
		return true
	}
	return false
}

func (s *Store) RecordOwnerVoiceReplyDiagnostic(ctx context.Context, request string, source SourceRef, code string) (bool, error) {
	if !validString(request, 128) || !validExtensionMediaSource(source) || !validOwnerVoiceReplyDiagnostic(code) {
		return false, ErrInvalidExtension
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	value, err := s.submitExtension(ctx, ownerVoiceCommand{op: "reply_diagnostic", scope: ReadScope{Account: source.Account}, request: request, source: source, diagnostic: code}, 2048)
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}

func (s *Store) ownerVoiceReplyDiagnostic(ctx context.Context, tx *sql.Tx, request string, source SourceRef, code string) (bool, error) {
	r, err := ownerVoiceLoad(ctx, tx, "request_id", request)
	if errors.Is(err, ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.state != "awaiting_phone" || unixms(s.now()) >= r.expires || r.snapshot.Intent.Source != source {
		return false, nil
	}
	if _, err = s.readSafety(source.Account); err != nil {
		return false, err
	}
	if err = s.ownerVoiceCurrent(ctx, tx, r); err != nil {
		if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
			return false, nil
		}
		return false, err
	}
	s.extensionRegisterDeadline(tx, fromMillis(r.expires))
	_, err = tx.ExecContext(ctx, "UPDATE owner_voice_redownload_intents SET last_code=? WHERE id=?", code, r.snapshot.Intent.ID)
	return err == nil, fixedSQLError(err)
}

type ownerVoiceLookup struct {
	intent OwnerVoiceIntent
	known  bool
}

func (s *Store) PrepareOwnerVoiceRedownloads(ctx context.Context, scope ReadScope, limit int) ([]OwnerVoiceIntent, error) {
	if _, _, err := ownerVoiceScope(scope, s.now()); err != nil {
		return nil, err
	}
	scope.Chats = append([]ChatID(nil), scope.Chats...)
	if limit < 1 || limit > 8 {
		return nil, ErrInvalidExtension
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := s.submitExtension(ctx, ownerVoiceCommand{op: "prepare", scope: scope, limit: limit}, 2048)
	if err != nil {
		return nil, err
	}
	return v.([]OwnerVoiceIntent), nil
}
func (s *Store) RegisterOwnerVoiceRequest(ctx context.Context, r OwnerVoiceRequestRegistration) error {
	if !validString(r.IntentID, 128) || !validString(r.RequestID, 128) || !validExtensionMediaSource(r.Source) {
		return ErrInvalidExtension
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.submitExtension(ctx, ownerVoiceCommand{op: "register", registration: r}, 2048)
	return err
}
func (s *Store) LookupOwnerVoiceRequest(ctx context.Context, id string) (OwnerVoiceIntent, bool, error) {
	if !validString(id, 128) {
		return OwnerVoiceIntent{}, false, ErrInvalidExtension
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := s.submitExtension(ctx, ownerVoiceCommand{op: "lookup", request: id}, 512)
	if err != nil {
		return OwnerVoiceIntent{}, false, err
	}
	r := v.(ownerVoiceLookup)
	return r.intent, r.known, nil
}
func (s *Store) AcceptOwnerVoiceDescriptor(ctx context.Context, r OwnerVoiceDescriptorReply) (bool, error) {
	if !validString(r.RequestID, 128) || !validKey(r.Key) || !validReadTime(r.OriginalSentAt) || !validReadTime(r.ExpiresAt) || r.Media.Kind != "voice" || r.Media.ViewOnce || len(r.Media.MimeType) > 256 || len(r.Media.ProtocolReference) > 8192 || r.Media.Filename != "" {
		return false, nil
	}
	if _, _, ok := extensionDownloadDescriptor(MessageVoice, r.Media.ProtocolReference, r.Media.DeclaredBytes); !ok {
		return false, nil
	}
	r.Media.ProtocolReference = bytes.Clone(r.Media.ProtocolReference)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := s.submitExtension(ctx, ownerVoiceCommand{op: "accept", reply: r}, int64(2048+len(r.Media.ProtocolReference)))
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}
func (s *Store) OwnerVoiceRecoveryStatus(ctx context.Context, scope ReadScope) (OwnerVoiceRecoveryStatus, error) {
	if _, _, err := ownerVoiceScope(scope, s.now()); err != nil {
		return OwnerVoiceRecoveryStatus{}, err
	}
	scope.Chats = append([]ChatID(nil), scope.Chats...)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := s.submitExtension(ctx, ownerVoiceCommand{op: "status", scope: scope}, 2048)
	if err != nil {
		return OwnerVoiceRecoveryStatus{}, err
	}
	return v.(OwnerVoiceRecoveryStatus), nil
}
func (r ownerVoiceRow) intent() OwnerVoiceIntent {
	i := r.snapshot.Intent
	i.RequestID = r.request
	i.RequestExpiresAt = fromMillis(r.expires)
	return i
}
func ownerVoiceLoad(ctx context.Context, q sqlQuerier, column, id string) (ownerVoiceRow, error) {
	var r ownerVoiceRow
	var b []byte
	if column != "id" && column != "request_id" && column != "artifact_id" {
		return r, ErrInvalidExtension
	}
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(snapshot)='blob' AND length(snapshot)<=8192 THEN snapshot ELSE NULL END,state,request_id,request_expires_at,lease_id,lease_until,next_due,attempts,protocol_sha256,reservation_id,artifact_id,completed_sha256,last_code FROM owner_voice_redownload_intents WHERE `+column+`=?`, id).Scan(&b, &r.state, &r.request, &r.expires, &r.lease, &r.until, &r.due, &r.attempts, &r.protocol, &r.reservation, &r.artifact, &r.completed, &r.code)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoWork
	}
	if err != nil {
		return r, fixedSQLError(err)
	}
	if len(b) == 0 || json.Unmarshal(b, &r.snapshot) != nil || !validString(r.snapshot.Intent.ID, 128) || !validExtensionMediaSource(r.snapshot.Intent.Source) || !extensionDownloadDigest(r.snapshot.ExpectedSHA) || !extensionDownloadDigest(r.snapshot.Identity) {
		return r, ErrSourceUnavailable
	}
	return r, nil
}
func ownerVoiceScope(scope ReadScope, now time.Time) (string, []any, error) {
	if !validString(scope.Account, 512) || !validString(scope.Principal, 256) || scope.PolicyGeneration == 0 || scope.Rolling < 0 || !validReadTime(scope.From) || !validReadTime(scope.Until) || (!scope.AllChats && len(scope.Chats) == 0) {
		return "", nil, ErrUnauthorized
	}
	chats, err := canonicalChats(scope.Chats)
	if err != nil {
		return "", nil, err
	}
	where := `m.account=? AND ap.generation=?`
	args := []any{scope.Account, scope.PolicyGeneration}
	if !scope.AllChats {
		marks := make([]string, len(chats))
		for n, c := range chats {
			marks[n] = "?"
			args = append(args, c)
		}
		where += " AND m.chat_id IN (" + strings.Join(marks, ",") + ")"
	}
	from := scope.From
	if scope.Rolling > 0 {
		from = later(from, now.Add(-scope.Rolling))
	}
	if !from.IsZero() {
		where += " AND m.original_sent_at>=?"
		args = append(args, ceilMillis(from))
	}
	if !scope.Until.IsZero() {
		where += " AND m.original_sent_at<?"
		args = append(args, ceilMillis(scope.Until))
	}
	return where, args, nil
}
func (s *Store) ownerVoiceCurrent(ctx context.Context, q sqlQuerier, r ownerVoiceRow) error {
	x := r.snapshot
	i := x.Intent
	if _, err := s.extensionCheckSources(ctx, q, []SourceRef{i.Source}, true); err != nil {
		return err
	}
	ok, err := originalCacheSourceMatches(ctx, q, OriginalCacheWitness{Source: i.Source, Key: i.Key, ChatID: x.ChatID, MediaIdentity: x.Identity})
	if err != nil {
		return err
	}
	if !ok {
		return ErrSourceUnavailable
	}
	where, args, err := ownerVoiceScope(x.Scope, s.now())
	if err != nil {
		return err
	}
	args = append(args, i.Source.Version.MessageID, i.SourceRevision, unixms(i.OriginalSentAt))
	var n int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM `+authorizedTables+` WHERE `+where+` AND m.id=? AND m.protocol_revision=? AND m.original_sent_at=? AND m.body_kind='voice' AND m.view_once=0`, args...).Scan(&n)
	if err != nil {
		return fixedSQLError(err)
	}
	if n != 1 {
		return ErrSourceUnavailable
	}
	if s.rebuild != nil {
		g, err := rebuildGuardQuery(ctx, q, i.Key)
		if err != nil {
			return err
		}
		if g.Deleted || g.ViewOnce || g.RevisionBarrier != "" || g.ProtocolRevision > i.SourceRevision || (!g.ExpiresAt.IsZero() && !s.now().Before(g.ExpiresAt)) || (!g.ChatClearCutoff.IsZero() && !i.OriginalSentAt.After(g.ChatClearCutoff)) {
			return ErrSourceUnavailable
		}
	}
	return nil
}
func (s *Store) ownerVoiceCheckScope(ctx context.Context, q sqlQuerier, scope ReadScope) error {
	if _, err := s.readSafety(scope.Account); err != nil {
		return err
	}
	var generation Generation
	if err := q.QueryRowContext(ctx, "SELECT generation FROM archive_policy WHERE id=1").Scan(&generation); err != nil {
		return fixedSQLError(err)
	}
	if generation != scope.PolicyGeneration {
		return ErrPolicyChanged
	}
	return nil
}
func (s *Store) ownerVoiceExpire(ctx context.Context, tx *sql.Tx, account string) error {
	_, err := tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state='expired',last_code=CASE WHEN last_code IN (`+ownerVoiceReplyDiagnosticSQL+`) THEN last_code ELSE 'media_deadline_exceeded' END WHERE account=? AND state IN ('prepared','awaiting_phone') AND request_expires_at<=?`, account, unixms(s.now()))
	return fixedSQLError(err)
}
func (s *Store) ownerVoicePrepare(ctx context.Context, tx *sql.Tx, scope ReadScope, limit int) ([]OwnerVoiceIntent, error) {
	where, args, err := ownerVoiceScope(scope, s.now())
	if err != nil {
		return nil, err
	}
	if err = s.ownerVoiceCheckScope(ctx, tx, scope); err != nil {
		return nil, err
	}
	if err = s.ownerVoiceExpire(ctx, tx, scope.Account); err != nil {
		return nil, err
	}
	var active int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM owner_voice_redownload_intents WHERE account=? AND state IN ('prepared','awaiting_phone','ready','leased')`, scope.Account).Scan(&active)
	if err != nil {
		return nil, fixedSQLError(err)
	}
	if limit > 32-active {
		limit = 32 - active
	}
	if limit <= 0 {
		return []OwnerVoiceIntent{}, nil
	}
	if s.rebuild != nil {
		where += ` AND NOT EXISTS(SELECT 1 FROM rebuild_guards g WHERE g.account=m.account AND COALESCE((SELECT canonical FROM identity_aliases al WHERE al.account=g.account AND al.alias=g.chat),g.chat)=m.source_chat AND COALESCE((SELECT canonical FROM identity_aliases al WHERE al.account=g.account AND al.alias=g.sender),g.sender)=m.source_sender AND g.message=m.source_message AND g.from_me=m.from_me AND (g.deleted<>0 OR g.view_once<>0 OR g.revision_barrier<>'' OR g.protocol_revision>m.protocol_revision OR (g.expires_at<>0 AND (m.expires_at=0 OR g.expires_at<m.expires_at))))`
	}
	// The parent is already imported; no source body is read or admitted here.
	args = append(args, unixms(s.now()), unixms(s.now()), extensionMediaClockRemainder(s.now()), limit)
	rows, err := tx.QueryContext(ctx, `SELECT a.id,m.id FROM `+authorizedTables+` JOIN extension_media_associations a ON a.message_id=m.id AND a.account=m.account
 JOIN extension_original_downloads d ON d.media_id=a.id
 WHERE `+where+` AND m.state='active' AND m.body_kind='voice' AND m.original_date_known=1 AND m.view_once=0 AND m.revision_barrier=''
 AND (m.expires_at=0 OR m.expires_at>?)
 AND (ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id))
 AND (ap.date_mode='all_available' OR m.original_sent_at>=ap.import_start) AND `+extensionMediaRollingSQL+`
 AND c.source_chat=m.source_chat AND i.source_identity=m.source_sender
 AND NOT EXISTS(SELECT 1 FROM source_tombstones t WHERE t.account=m.account AND t.chat=m.source_chat AND t.sender=m.source_sender AND t.message=m.source_message AND t.from_me=m.from_me)
 AND NOT EXISTS(SELECT 1 FROM chat_tombstones t WHERE t.account=m.account AND t.chat=m.source_chat AND t.cutoff>=m.original_sent_at)
 AND NOT EXISTS(SELECT 1 FROM unresolved_deletions u WHERE u.account=m.account AND u.chat=m.source_chat AND u.message=m.source_message)
 AND NOT EXISTS(SELECT 1 FROM sync_gaps g WHERE g.account=m.account AND g.code='identity_alias_conflict')
 AND NOT EXISTS(SELECT 1 FROM identity_aliases al WHERE al.account=m.account AND al.alias IN (m.source_chat,m.source_sender) AND al.canonical<>al.alias)
 AND NOT EXISTS(SELECT 1 FROM chat_aliases al WHERE al.account=m.account AND al.alias=m.source_chat AND al.canonical<>al.alias)
 AND a.kind='voice' AND a.view_once=0 AND a.ordinal=0 AND a.local_revision=m.local_revision AND a.content_hash=m.content_hash AND a.source_expires_at=m.expires_at
 AND length(a.last_original_sha256)=64 AND a.last_original_sha256=d.completed_sha256
 AND NOT EXISTS(SELECT 1 FROM extension_media_associations sibling WHERE sibling.message_id=m.id AND sibling.local_revision=m.local_revision AND sibling.ordinal<>0)
 AND NOT EXISTS(SELECT 1 FROM owner_voice_redownload_intents r WHERE r.media_id=a.id AND r.local_revision=m.local_revision AND r.policy_generation=ap.generation AND r.state IN ('prepared','awaiting_phone','ready','leased'))
 AND NOT EXISTS(SELECT 1 FROM extension_media_artifacts physical WHERE physical.id=a.artifact_id AND physical.cleanup_ack=0 AND physical.state='retained')
 ORDER BY m.original_sent_at DESC,m.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fixedSQLError(err)
	}
	type candidate struct {
		media   MediaID
		message MessageID
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.media, &c.message); err != nil {
			break
		}
		candidates = append(candidates, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, fixedSQLError(err)
	}
	out := []OwnerVoiceIntent{}
	for _, c := range candidates {
		current, err := s.extensionLookupSourceAt(ctx, tx, scope.Account, c.message, true, s.now())
		if errors.Is(err, ErrSourceUnavailable) {
			continue
		}
		if err != nil {
			return nil, err
		}
		x := ownerVoiceSnapshot{Scope: scope, ChatID: current.ChatID}
		x.Scope.AllChats = false
		x.Scope.Chats = []ChatID{current.ChatID}
		i := &x.Intent
		i.Source = current.Ref
		i.MediaID = c.media
		i.OriginalSentAt = current.OriginalSentAt
		var sent int64
		err = tx.QueryRowContext(ctx, `SELECT m.account,m.source_chat,m.source_sender,m.source_message,m.from_me,m.protocol_revision,m.extension_media_identity,a.ordinal,a.declared_bytes,a.mime_type,a.last_original_sha256,a.policy_generation,d.descriptor_sha256,d.legacy_source_media_identity,m.original_sent_at FROM messages m JOIN extension_media_associations a ON a.message_id=m.id JOIN extension_original_downloads d ON d.media_id=a.id WHERE a.id=? AND length(CAST(m.account AS BLOB))<=512 AND length(CAST(m.source_chat AS BLOB))<=512 AND length(CAST(m.source_sender AS BLOB))<=512 AND length(CAST(m.source_message AS BLOB))<=512 AND length(m.extension_media_identity)=64 AND length(a.mime_type)<=256 AND length(d.descriptor_sha256)<=64 AND length(d.legacy_source_media_identity)<=64`, c.media).Scan(&i.Key.Account, &i.Key.Chat, &i.Key.Sender, &i.Key.Message, &i.Key.FromMe, &i.SourceRevision, &x.Identity, &x.Ordinal, &x.Declared, &x.Mime, &x.ExpectedSHA, &x.PriorPolicy, &x.Descriptor, &x.Legacy, &sent)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fixedSQLError(err)
		}
		if !validKey(i.Key) || !extensionDownloadDigest(x.ExpectedSHA) || !extensionDownloadDigest(x.Identity) {
			continue
		}
		if err = s.ownerVoiceCurrent(ctx, tx, ownerVoiceRow{snapshot: x}); errors.Is(err, ErrSourceUnavailable) {
			continue
		} else if err != nil {
			return nil, err
		}
		i.ID, err = randomID()
		if err != nil {
			return nil, err
		}
		i.RequestExpiresAt = fromMillis(unixms(s.now().Add(5 * time.Minute)))
		b, err := json.Marshal(x)
		if err != nil || len(b) > 8192 {
			return nil, ErrInvalidExtension
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO owner_voice_redownload_intents(id,account,message_id,media_id,local_revision,policy_generation,snapshot,created_at,request_expires_at,state) VALUES(?,?,?,?,?,?,?,?,?,'prepared')`, i.ID, i.Source.Account, i.Source.Version.MessageID, i.MediaID, i.Source.Version.Revision, i.Source.Version.PolicyGeneration, b, unixms(s.now()), unixms(i.RequestExpiresAt))
		if err != nil {
			return nil, fixedSQLError(err)
		}
		out = append(out, *i)
	}
	return out, nil
}
func (s *Store) ownerVoiceRegister(ctx context.Context, tx *sql.Tx, reg OwnerVoiceRequestRegistration) error {
	r, err := ownerVoiceLoad(ctx, tx, "id", reg.IntentID)
	if err != nil {
		return err
	}
	if r.state != "prepared" || r.request != "" || r.snapshot.Intent.Source != reg.Source || unixms(s.now()) >= r.expires {
		return ErrNoWork
	}
	if err = s.ownerVoiceCurrent(ctx, tx, r); err != nil {
		return err
	}
	until := s.now().Add(5 * time.Minute)
	s.extensionRegisterDeadline(tx, until)
	_, err = tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET request_id=?,request_expires_at=?,state='awaiting_phone' WHERE id=?`, reg.RequestID, unixms(until), reg.IntentID)
	return fixedSQLError(err)
}
func (s *Store) ownerVoiceAccept(ctx context.Context, tx *sql.Tx, reply OwnerVoiceDescriptorReply) (bool, error) {
	r, err := ownerVoiceLoad(ctx, tx, "request_id", reply.RequestID)
	if err != nil {
		return false, err
	}
	i := r.snapshot.Intent
	if r.state != "awaiting_phone" || unixms(s.now()) >= r.expires || i.Key != reply.Key || !i.OriginalSentAt.Equal(reply.OriginalSentAt) || i.SourceRevision != reply.SourceRevision || !i.Source.Version.ExpiresAt.Equal(reply.ExpiresAt) {
		return false, nil
	}
	if err = s.ownerVoiceCurrent(ctx, tx, r); err != nil {
		return false, err
	}
	reject := func() (bool, error) {
		_, e := tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state='failed',protocol_reference=X'',last_code='media_invalid_admission' WHERE id=?`, i.ID)
		return false, fixedSQLError(e)
	}
	x := r.snapshot
	// A fresh authenticated transport may rotate its path, key and encrypted
	// digest. Its exact reduced bytes are pinned separately; plaintext identity
	// remains the previously verified SHA, and parent media identity is immutable.
	if reply.Media.DeclaredBytes != x.Declared || reply.Media.MimeType != x.Mime {
		return reject()
	}
	descriptor, raw, ok := extensionDownloadDescriptor(MessageVoice, reply.Media.ProtocolReference, reply.Media.DeclaredBytes)
	if !ok {
		return reject()
	}
	var audio waE2E.AudioMessage
	if (proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 8}).Unmarshal(reply.Media.ProtocolReference, &audio) != nil || hex.EncodeToString(audio.GetFileSHA256()) != x.ExpectedSHA {
		return reject()
	}
	s.extensionRegisterDeadline(tx, fromMillis(r.expires))
	result, err := tx.ExecContext(ctx, `UPDATE extension_media_associations SET policy_generation=?,protocol_reference=?,active=1,availability='not_retained' WHERE id=? AND account=? AND message_id=? AND local_revision=? AND content_hash=? AND source_expires_at=? AND kind='voice' AND view_once=0 AND last_original_sha256=?`, i.Source.Version.PolicyGeneration, reply.Media.ProtocolReference, i.MediaID, i.Source.Account, i.Source.Version.MessageID, i.Source.Version.Revision, i.Source.ContentHash, unixms(i.Source.Version.ExpiresAt), x.ExpectedSHA)
	if err != nil {
		return false, fixedSQLError(err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return false, ErrSourceUnavailable
	}
	x.CurrentDescriptor = descriptor
	snapshot, err := json.Marshal(x)
	if err != nil || len(snapshot) > 8192 {
		return false, ErrInvalidExtension
	}
	_, err = tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state='ready',last_code='',protocol_sha256=?,snapshot=?,protocol_reference=? WHERE id=?`, raw, snapshot, reply.Media.ProtocolReference, i.ID)
	return err == nil, fixedSQLError(err)
}
func (s *Store) ownerVoiceStatus(ctx context.Context, tx *sql.Tx, scope ReadScope) (OwnerVoiceRecoveryStatus, error) {
	var status OwnerVoiceRecoveryStatus
	where, args, err := ownerVoiceScope(scope, s.now())
	if err != nil {
		return status, err
	}
	if err = s.ownerVoiceCheckScope(ctx, tx, scope); err != nil {
		return status, err
	}
	if err = s.ownerVoiceExpire(ctx, tx, scope.Account); err != nil {
		return status, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id FROM `+authorizedTables+` JOIN owner_voice_redownload_intents r ON m.id=r.message_id WHERE `+where+` AND r.policy_generation=ap.generation ORDER BY r.created_at DESC,r.id LIMIT 32`, args...)

	if err != nil {
		return status, fixedSQLError(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return status, fixedSQLError(err)
	}
	for _, id := range ids {
		r, err := ownerVoiceLoad(ctx, tx, "id", id)
		if err != nil {
			return status, err
		}
		if err = s.ownerVoiceCurrent(ctx, tx, r); err != nil {
			if errors.Is(err, ErrSourceUnavailable) {
				continue
			}
			return status, err
		}
		switch r.state {
		case "prepared", "awaiting_phone":
			status.AwaitingPhone++
		case "leased":
			status.Downloading++
		case "complete":
			status.Completed++
		case "failed", "cancelled", "expired":
			status.Failed++
			status.Code = r.code
			if validOwnerVoiceReplyDiagnostic(r.code) {
				switch r.state {
				case "expired":
					status.Code = "media_deadline_exceeded"
				case "cancelled":
					status.Code = "media_canceled"
				default:
					status.Code = "media_invalid_admission"
				}
			}
		case "ready":
			var clean bool
			err = tx.QueryRowContext(ctx, `SELECT `+extensionDownloadNoPhysicalSQL+` FROM extension_media_associations a WHERE a.id=?`, r.snapshot.Intent.MediaID).Scan(&clean)
			if err != nil {
				return status, fixedSQLError(err)
			}
			if clean {
				status.Queued++
			} else {
				status.WaitingCleanup++
			}
		}
	}
	return status, nil
}

func (r ownerVoiceRow) downloadRow() extensionDownloadRow {
	i := r.snapshot.Intent
	return extensionDownloadRow{work: OriginalDownloadWork{Lease: OriginalDownloadLease{IntentID: i.ID, MediaID: i.MediaID, LeaseID: LeaseID(r.lease)}, Source: i.Source, Kind: MessageVoice, DeclaredBytes: r.snapshot.Declared, Attempts: r.attempts, LeaseUntil: fromMillis(r.until)}, state: r.state, protocol: r.protocol, descriptor: r.snapshot.CurrentDescriptor, completed: r.completed, due: r.due}
}
func (s *Store) ownerVoiceLease(ctx context.Context, q sqlQuerier, l OriginalDownloadLease) (ownerVoiceRow, error) {
	r, err := ownerVoiceLoad(ctx, q, "id", l.IntentID)
	if err != nil {
		return r, err
	}
	if r.downloadRow().work.Lease != l || (r.state != "leased" && r.state != "complete") || !s.now().Before(fromMillis(r.until)) {
		return r, ErrNoWork
	}
	if err = s.ownerVoiceCurrent(ctx, q, r); err != nil {
		return r, err
	}
	in, err := s.extensionReadDownloadReference(ctx, q, r.downloadRow())
	defer clear(in.ProtocolReference)
	if err != nil {
		return r, err
	}
	if in.Work.Kind != MessageVoice || in.Work.DeclaredBytes != r.snapshot.Declared {
		return r, ErrSourceUnavailable
	}
	s.extensionRegisterDeadline(q, fromMillis(r.until))
	return r, nil
}
func (s *Store) ownerVoiceClaim(ctx context.Context, tx *sql.Tx, c OriginalDownloadClaim) (OriginalDownloadWork, error) {
	now := s.now()
	_, err := tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state='failed',protocol_reference=X'',last_code='media_unavailable' WHERE account=? AND attempts>=8 AND (state='ready' OR (state='leased' AND lease_until<=?))`, c.Account, unixms(now))
	if err != nil {
		return OriginalDownloadWork{}, fixedSQLError(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id FROM owner_voice_redownload_intents r JOIN extension_media_associations a ON a.id=r.media_id WHERE r.account=? AND r.attempts<8 AND r.next_due<=? AND (r.state='ready' OR (r.state='leased' AND r.lease_until<=?)) AND `+extensionDownloadNoPhysicalSQL+` ORDER BY r.next_due,r.created_at,r.id LIMIT 32`, c.Account, unixms(now), unixms(now))
	if err != nil {
		return OriginalDownloadWork{}, fixedSQLError(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return OriginalDownloadWork{}, fixedSQLError(err)
	}
	for _, id := range ids {
		r, err := ownerVoiceLoad(ctx, tx, "id", id)
		if err != nil {
			return OriginalDownloadWork{}, err
		}
		err = s.ownerVoiceCurrent(ctx, tx, r)
		if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
			_, err = tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state='cancelled',protocol_reference=X'',last_code='media_source_changed' WHERE id=?`, id)
			if err != nil {
				return OriginalDownloadWork{}, fixedSQLError(err)
			}
			continue
		}
		if err != nil {
			return OriginalDownloadWork{}, err
		}
		in, err := s.extensionReadDownloadReference(ctx, tx, r.downloadRow())
		clear(in.ProtocolReference)
		if err != nil {
			return OriginalDownloadWork{}, err
		}
		lease, err := randomID()
		if err != nil {
			return OriginalDownloadWork{}, err
		}
		r.lease = lease
		r.until = unixms(now.Add(c.TTL))
		r.attempts++
		r.state = "leased"
		_, err = tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state='leased',lease_id=?,lease_until=?,attempts=?,reservation_id='',artifact_id='' WHERE id=?`, lease, r.until, r.attempts, id)
		if err != nil {
			return OriginalDownloadWork{}, fixedSQLError(err)
		}
		s.extensionRegisterDeadline(tx, fromMillis(r.until))
		return r.downloadRow().work, nil
	}
	return OriginalDownloadWork{}, nil
}
func (s *Store) ownerVoiceFinish(ctx context.Context, tx *sql.Tx, l OriginalDownloadLease) (bool, error) {
	r, err := s.ownerVoiceLease(ctx, tx, l)
	if err != nil {
		return false, err
	}
	if r.state != "complete" || r.completed != r.snapshot.ExpectedSHA || r.artifact == "" || r.reservation == "" {
		return false, nil
	}
	var n int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_artifacts a JOIN extension_media_associations m ON m.artifact_id=a.id WHERE a.id=? AND a.reservation_id=? AND a.media_id=? AND a.state='retained' AND a.cleanup_ack=0 AND a.sha256=? AND m.active=1 AND m.last_original_sha256=a.sha256`, r.artifact, r.reservation, l.MediaID, r.completed).Scan(&n)
	return n == 1, fixedSQLError(err)
}
func (s *Store) ownerVoiceRetry(ctx context.Context, tx *sql.Tx, retry OriginalDownloadRetry) error {
	r, err := s.ownerVoiceLease(ctx, tx, retry.Lease)
	if err != nil {
		return err
	}
	if r.state == "complete" {
		return nil
	}
	var clean bool
	err = tx.QueryRowContext(ctx, `SELECT `+extensionDownloadNoPhysicalSQL+` FROM extension_media_associations a WHERE a.id=?`, retry.Lease.MediaID).Scan(&clean)
	if err != nil {
		return fixedSQLError(err)
	}
	if !clean {
		return extensionDownloadCleanupIncomplete
	}
	attempts := r.attempts
	if retry.Code == "waiting_for_disk_budget" && attempts > 0 {
		attempts--
	}
	state := "ready"
	if retry.Terminal || attempts >= 8 {
		state = "failed"
	}
	after := retry.After
	if after < time.Second {
		after = time.Second
	}
	if retry.Code == "waiting_for_disk_budget" && after < time.Minute {
		after = time.Minute
	}
	_, err = tx.ExecContext(ctx, `UPDATE owner_voice_redownload_intents SET state=?,protocol_reference=CASE WHEN ?='failed' THEN X'' ELSE protocol_reference END,attempts=?,next_due=?,lease_id='',lease_until=0,last_code=? WHERE id=?`, state, state, attempts, unixms(s.now().Add(after)), retry.Code, retry.Lease.IntentID)
	return fixedSQLError(err)
}

// Bound original admissions never borrow completion receipts or reservations
// from an older policy. Failed physical attempts retain their binding until
// the existing cleanup acknowledgement permits another claim/admission.
func (s *Store) ownerVoiceAdmit(ctx context.Context, tx *sql.Tx, request MediaAdmissionRequest) error {
	l := request.OwnerDownloadLease
	if l.IntentID == "" {
		if l.MediaID != "" || l.LeaseID != "" {
			return ErrInvalidExtension
		}
		if request.Budget == BudgetOriginals {
			var protected bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owner_voice_redownload_intents WHERE media_id=?)`, request.MediaID).Scan(&protected)
			if err != nil {
				return fixedSQLError(err)
			}
			if protected {
				return ErrNoWork
			}
		}
		return nil
	}
	if request.Budget != BudgetOriginals || l.MediaID != request.MediaID {
		return ErrInvalidExtension
	}
	r, err := s.ownerVoiceLease(ctx, tx, l)
	if err != nil {
		return err
	}
	if r.state != "leased" || r.snapshot.Intent.Source != request.Source {
		return ErrNoWork
	}
	var clean bool
	err = tx.QueryRowContext(ctx, `SELECT `+extensionDownloadNoPhysicalSQL+` FROM extension_media_associations a WHERE a.id=?`, l.MediaID).Scan(&clean)
	if err != nil {
		return fixedSQLError(err)
	}
	if !clean {
		return extensionDownloadCleanupIncomplete
	}
	return nil
}
func (s *Store) ownerVoiceBound(ctx context.Context, q sqlQuerier, res MediaReservation) (ownerVoiceRow, bool, error) {
	r, err := ownerVoiceLoad(ctx, q, "artifact_id", string(res.ArtifactID))
	if errors.Is(err, ErrNoWork) {
		return r, false, nil
	}
	if err != nil {
		return r, true, err
	}
	if r.reservation != string(res.ID) || r.snapshot.Intent.Source != res.Source || r.snapshot.Intent.MediaID != res.MediaID {
		return r, true, ErrNoWork
	}
	_, err = s.ownerVoiceLease(ctx, q, r.downloadRow().work.Lease)
	return r, true, err
}

// Restrictions use the existing source-control safety transaction. Positive
// descriptor replies never mutate parent bodies; authenticated negative facts
// intentionally retain ordinary privacy erasure and revision semantics.
type ownerVoiceRestrictionCommand struct {
	request string
	key     SourceKey
	expires time.Time
	view    bool
}

func (c ownerVoiceRestrictionCommand) priority() bool                    { return true }
func (c ownerVoiceRestrictionCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c ownerVoiceRestrictionCommand) account() string                   { return c.key.Account }
func (c ownerVoiceRestrictionCommand) run(s *Store, ctx context.Context) (any, error) {
	invalidates := true
	err := s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		r, err := ownerVoiceLoad(ctx, tx, "request_id", c.request)
		if err != nil {
			return err
		}
		if r.snapshot.Intent.Key != c.key {
			return ErrSourceUnavailable
		}
		if _, err = s.readSafety(c.key.Account); err != nil {
			return err
		}
		canonical, err := canonicalKey(ctx, tx, c.key)
		if err != nil {
			return fixedSQLError(err)
		}
		if canonical != c.key {
			return ErrSourceUnavailable
		}
		// A later policy or edit cannot discard an authenticated privacy fact for
		// the same original. A different original with the same key cannot borrow it.
		var matches int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE id=? AND account=? AND source_chat=? AND source_sender=? AND source_message=? AND from_me=? AND original_date_known=1 AND original_sent_at=?`, r.snapshot.Intent.Source.Version.MessageID, c.key.Account, c.key.Chat, c.key.Sender, c.key.Message, c.key.FromMe, unixms(r.snapshot.Intent.OriginalSentAt)).Scan(&matches)
		if err != nil {
			return fixedSQLError(err)
		}
		if matches != 1 {
			return ErrSourceUnavailable
		}
		policy, err := s.loadPolicy(ctx, tx)
		if err != nil {
			return err
		}
		if s.rebuild != nil {
			return s.rebuildRestrict(ctx, tx, c.key, c.expires, c.view, policy.Generation)
		}
		return s.restrictCurrentSource(ctx, tx, c.key, c.expires, c.view, policy.Generation, RebuildGuard{})
	}, c.key.Account)
	return nil, err
}
func (s *Store) RecordOwnerVoiceRestriction(ctx context.Context, requestID string, key SourceKey, expires time.Time, viewOnce bool) error {
	if !validString(requestID, 128) || !validKey(key) || !validReadTime(expires) || (!expires.IsZero() && unixms(expires) <= 0) {
		return ErrInvalidExtension
	}
	if expires.IsZero() && !viewOnce {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.submitExtension(ctx, ownerVoiceRestrictionCommand{request: requestID, key: key, expires: expires, view: viewOnce}, 1024)
	return err
}
