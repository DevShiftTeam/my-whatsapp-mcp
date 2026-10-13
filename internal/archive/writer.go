package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const maxEventTextBytes = 256 << 10
const maxProtocolReferenceBytes = 16 << 10
const maxAnchorsPerChat = 1000

type command struct {
	ctx             context.Context
	payload         any
	reply           chan commandResult
	bytes           int64
	account         string
	control         bool
	sourceSensitive bool
}
type commandResult struct {
	value any
	err   error
}
type policyCommand struct {
	policy   *ArchivePolicy
	expected Generation
}
type eventCommand struct {
	event    SourceEvent
	expected Generation
}
type aliasCommand struct{ alias SourceAlias }
type syncCommand struct{ update SyncUpdate }
type unsafeCommand struct {
	account string
	reason  UnsafeReason
}
type observationCommand struct {
	account string
	id      ObservationID
	begin   bool
}
type clearChatCommand struct {
	account, chat string
	cutoff        time.Time
}
type mediaCommand struct {
	key          SourceKey
	version      SourceVersion
	availability string
}
type drainCommand struct{}
type namesCommand struct{ names SourceNames }
type expiryCommand struct{}

func (s *Store) submit(ctx context.Context, payload any, account string, control bool, n int64) (any, error) {
	if s.historicalSnapshot {
		return nil, ErrHistoricalSnapshotReadOnly
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.lifeMu.RLock()
	if s.closed {
		s.lifeMu.RUnlock()
		return nil, ErrArchiveUnavailable
	}
	s.queuedCommands.Add(1)
	c := command{ctx: ctx, payload: payload, reply: make(chan commandResult, 1), bytes: n, account: account, control: control, sourceSensitive: commandSourceSensitive(payload, control)}
	if control {
		budget := s.options.ControlDeadline
		if _, alias := payload.(aliasCommand); alias {
			budget = s.options.AliasDeadline
		}
		deadline, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		c.ctx = deadline
		select {
		case s.controls <- c:
			s.lifeMu.RUnlock()
		case <-deadline.Done():
			s.queuedCommands.Add(-1)
			s.lifeMu.RUnlock()
			if c.sourceSensitive {
				s.denyImmediate(account, UnsafeReason("control_admission_failed"))
			}
			if _, expiry := c.payload.(expiryCommand); expiry {
				s.recordExpiryMaintenanceFailure("expiry_maintenance_admission", deadline.Err())
			}
			return nil, ErrQueueLimitExceeded
		}
	} else {
		if s.queuedCount.Add(1) > int64(s.options.BodyQueueCount) {
			s.queuedCommands.Add(-1)
			s.queuedCount.Add(-1)
			s.lifeMu.RUnlock()
			if c.sourceSensitive {
				s.denyImmediate(account, "queue_limit_exceeded")
			}
			return nil, ErrQueueLimitExceeded
		}
		for {
			v := s.queuedBytes.Load()
			if v+n > int64(s.options.BodyQueueBytes) {
				s.queuedCommands.Add(-1)
				s.queuedCount.Add(-1)
				s.lifeMu.RUnlock()
				if c.sourceSensitive {
					s.denyImmediate(account, UnsafeReason("queue_limit_exceeded"))
				}
				return nil, ErrQueueLimitExceeded
			}
			if s.queuedBytes.CompareAndSwap(v, v+n) {
				break
			}
		}
		select {
		case s.bodies <- c:
			s.lifeMu.RUnlock()
		default:
			s.queuedCommands.Add(-1)
			s.queuedBytes.Add(-n)
			s.queuedCount.Add(-1)
			s.lifeMu.RUnlock()
			if c.sourceSensitive {
				s.denyImmediate(account, UnsafeReason("queue_limit_exceeded"))
			}
			return nil, ErrQueueLimitExceeded
		}
	}
	return s.awaitCommandResult(c)
}
func (s *Store) awaitCommandResult(c command) (any, error) {
	select {
	case r := <-c.reply:
		return r.value, r.err
	case <-c.ctx.Done():
		// Cancellation does not turn an already authoritative, completed CAS
		// rejection into uncertainty about source or credential mutation.
		select {
		case r := <-c.reply:
			return r.value, r.err
		default:
		}
		if c.sourceSensitive {
			reason := UnsafeReason("control_admission_failed")
			if !c.control {
				reason = "drain_failed"
			}
			s.denyImmediate(c.account, reason)
		}
		if _, expiry := c.payload.(expiryCommand); expiry {
			s.recordExpiryMaintenanceFailure("expiry_maintenance_wait", c.ctx.Err())
		}
		s.extensionCommandFailure(c.payload)
		return nil, c.ctx.Err()
	}
}
func (s *Store) writerLoop() {
	defer close(s.done)
	maintenance := time.NewTicker(time.Minute)
	defer maintenance.Stop()
	execute := func(c command) {
		s.writerMu.Lock()
		defer s.writerMu.Unlock()
		defer s.queuedCommands.Add(-1)
		if !c.control {
			defer s.queuedBytes.Add(-c.bytes)
			defer s.queuedCount.Add(-1)
		}
		var v any
		var e error
		expiryRunnerEntered := false
		if e = c.ctx.Err(); e == nil {
			switch x := c.payload.(type) {
			case policyCommand:
				if x.policy == nil {
					v, e = s.loadPolicy(c.ctx, s.writeDB)
				} else {
					v, e = s.setPolicy(c.ctx, *x.policy, x.expected)
				}
			case eventCommand:
				v, e = s.applyEvent(c.ctx, x.event, x.expected)
			case aliasCommand:
				e = s.applyAlias(c.ctx, x.alias)
			case historyUncertaintyCommand:
				e = s.recordHistoryUncertainty(c.ctx, x)
			case unknownHistoryGapCommand:
				e = s.recordUnknownHistoryGap(c.ctx, x.account, x.rawStub)
			case syncCommand:
				e = s.setSync(c.ctx, x.update)
			case unsafeCommand:
				e = s.persistUnsafe(c.ctx, x.account, x.reason)
			case observationCommand:
				if x.begin {
					v, e = s.beginObservation(c.ctx, x.account)
				} else {
					e = s.endObservation(c.ctx, x.id)
				}
			case clearChatCommand:
				e = s.clearChat(c.ctx, x.account, x.chat, x.cutoff)
			case unresolvedDeletionCommand:
				e = s.recordUnresolvedDeletion(c.ctx, x.account, x.chat, x.message)
			case mediaCommand:
				e = s.updateMedia(c.ctx, x.key, x.version, x.availability)
			case expiryCommand:
				expiryRunnerEntered = true
				_, e = s.runExpiryMaintenance(c.ctx, "expiry_maintenance_result")
			case namesCommand:
				e = s.setSourceNames(c.ctx, x.names)
			case drainCommand:
			default:
				if extension, ok := c.payload.(extensionCommand); ok {
					v, e = extension.run(s, c.ctx)
				} else {
					e = ErrInvalidEvent
				}
			}
		}
		if e != nil && (errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) || errors.Is(e, ErrArchiveUnavailable)) {
			if c.sourceSensitive {
				s.denyImmediate(c.account, UnsafeReason("control_admission_failed"))
			}
			s.extensionCommandFailure(c.payload)
		}
		if _, expiry := c.payload.(expiryCommand); expiry && !expiryRunnerEntered && e != nil {
			// Expired commands may be rejected before the cleanup runner enters.
			s.recordExpiryMaintenanceFailure("expiry_maintenance_result", e)
		}
		if _, alias := c.payload.(aliasCommand); alias {
			recordAliasFailure(c.ctx, "alias_writer_result", e)
		}
		c.reply <- commandResult{v, e}
	}
	for {
		select {
		case c := <-s.controls:
			execute(c)
			continue
		default:
		}
		select {
		case c := <-s.controls:
			execute(c)
		case c := <-s.bodies:
			execute(c)
		case <-maintenance.C:
			s.writerMu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), s.options.ControlDeadline)
			pending, err := s.runExpiryMaintenance(ctx, "expiry_maintenance_timer")
			cancel()
			s.writerMu.Unlock()
			maintenance.Reset(expiryMaintenanceDelay(pending, err))
		case <-s.stop:
			for {
				select {
				case c := <-s.controls:
					execute(c)
					continue
				default:
				}
				select {
				case c := <-s.bodies:
					execute(c)
				case c := <-s.controls:
					execute(c)
				default:
					return
				}
			}
		}
	}
}

// Drain is a lifecycle barrier. The caller must first stop connector admission.
func (s *Store) Drain(ctx context.Context) error {
	_, e := s.submit(ctx, drainCommand{}, "", false, 0)
	return e
}
func (s *Store) Policy(ctx context.Context) (ArchivePolicy, error) {
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, e := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return ArchivePolicy{}, fixedSQLError(e)
	}
	defer tx.Rollback()
	return s.loadPolicy(ctx, tx)
}

func (s *Store) SetPolicy(ctx context.Context, p ArchivePolicy, expected Generation) (ArchivePolicy, error) {
	p.IncludedChats = append([]ChatID(nil), p.IncludedChats...)
	v, e := s.submit(ctx, policyCommand{&p, expected}, "", true, 0)
	if e != nil {
		return ArchivePolicy{}, e
	}
	return v.(ArchivePolicy), nil
}
func (s *Store) ApplySourceEvent(ctx context.Context, e SourceEvent, expected Generation) (ApplyResult, error) {
	if err := validateEvent(e); err != nil {
		return ApplyResult{}, err
	}
	e.Media = cloneMedia(e.Media)
	if e.DeliveredHistory != nil {
		admission := *e.DeliveredHistory
		e.DeliveredHistory = &admission
	}
	if e.ReplyTo != nil {
		k := *e.ReplyTo
		e.ReplyTo = &k
	}
	if e.Checkpoint != nil {
		c := *e.Checkpoint
		e.Checkpoint = &c
	}
	control := e.Kind == EventDelete || e.Kind == EventEditInvalidation
	n := int64(len(e.Body) + len(e.Caption) + len(e.Key.Account) + len(e.Key.Chat) + len(e.Key.Sender) + len(e.Key.Message) + 512)
	for _, m := range e.Media {
		n += int64(128 + len(m.ProtocolReference) + len(m.MimeType) + len(m.Filename) + len(m.Kind) + len(m.Availability))
	}
	if e.ReplyTo != nil {
		n += int64(len(e.ReplyTo.Account) + len(e.ReplyTo.Chat) + len(e.ReplyTo.Sender) + len(e.ReplyTo.Message) + 128)
	}
	if e.Checkpoint != nil {
		n += int64(len(e.Checkpoint.Kind) + len(e.Checkpoint.ID) + 64)
	}
	v, err := s.submit(ctx, eventCommand{e, expected}, e.Key.Account, control, n)
	if err != nil {
		if v != nil {
			return v.(ApplyResult), err
		}
		return ApplyResult{}, err
	}
	return v.(ApplyResult), nil
}
func (s *Store) ApplyAlias(ctx context.Context, a SourceAlias) error {
	if !validString(a.Account, 512) || !validString(a.PhoneJID, 512) || !validString(a.LID, 512) || !strings.HasSuffix(a.PhoneJID, "@s.whatsapp.net") || !strings.HasSuffix(a.LID, "@lid") || (a.Provenance != AliasConnectorMapping && a.Provenance != AliasSessionMetadata) {
		return ErrInvalidEvent
	}
	if committed, err := s.committedAlias(ctx, a); err != nil || committed {
		recordAliasFailure(ctx, "alias_lookup", err)
		return err
	}
	_, e := s.submit(ctx, aliasCommand{a}, a.Account, true, 0)
	recordAliasFailure(ctx, "alias_write", e)
	return e
}
func (s *Store) SetSyncStatus(ctx context.Context, u SyncUpdate) error {
	if !validString(u.Account, 512) || len(u.Chat) > 512 || u.Received < 0 || u.Applied < 0 || u.Dropped < 0 || !validConnection(u.Connection) || !validGap(u.GapCode) || !validCheckpoint(u.Checkpoint) {
		return ErrInvalidEvent
	}
	if u.Checkpoint != nil {
		c := *u.Checkpoint
		u.Checkpoint = &c
	}
	_, e := s.submit(ctx, syncCommand{u}, u.Account, false, int64(512+len(u.Account)+len(u.Chat)+len(u.GapCode)+checkpointBytes(u.Checkpoint)))
	return e
}
func (s *Store) ClearChat(ctx context.Context, a, c string, t time.Time) error {
	if !validString(a, 512) || !validString(c, 512) || t.IsZero() {
		return ErrInvalidEvent
	}
	_, e := s.submit(ctx, clearChatCommand{a, c, t}, a, true, 0)
	return e
}
func (s *Store) UpdateMedia(ctx context.Context, k SourceKey, v SourceVersion, a string) error {
	if !validKey(k) || !validMediaAvailability(a) {
		return ErrInvalidEvent
	}
	_, e := s.submit(ctx, mediaCommand{k, v, a}, k.Account, false, 512)
	return e
}
func validString(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func validKey(k SourceKey) bool {
	return validString(k.Account, 512) && validString(k.Chat, 512) && validString(k.Sender, 512) && validString(k.Message, 512)
}
func validConnection(s ConnectionState) bool {
	switch s {
	case "", "setup_required", "pairing", "connected", "disconnected", "paused", "reconciliation_required":
		return true
	}
	return false
}
func validGap(s string) bool {
	switch s {
	case UnknownHistoryGapCode, "", "backfill_requested_coverage_unknown", "backfill_request_failed", "mutation_metadata_missing", "source_revision_conflict", "history_limit_exceeded", "history_corrupt", "queue_limit_exceeded", "coverage_unknown", "offline_gap", "identity_alias_conflict", "control_admission_failed", "unsafe_mutation_projection", "drain_failed", "owner_restored_snapshot":
		return true
	case "app_state_sync_error", "undecryptable_message", "ambiguous_message_structure", "ambiguous_edit_structure", "unresolved_deletion":
		return true
	}
	return false
}
func validMediaAvailability(s string) bool {
	switch s {
	case "not_retained", "unavailable", "view_once", "expired", "unsupported", "limit_exceeded", "download_failed", "downloaded_not_retained", "retained":
		return true
	}
	return false
}
func validCheckpoint(c *SyncCheckpoint) bool {
	return c == nil || (validString(c.Kind, 64) && validString(c.ID, 512))
}
func validateEvent(e SourceEvent) error {
	if e.RebuildRequestID != "" && !validString(e.RebuildRequestID, 128) {
		return ErrInvalidEvent
	}
	if !validKey(e.Key) || !utf8.ValidString(e.Body) || !utf8.ValidString(e.Caption) || len(e.Body)+len(e.Caption) > maxEventTextBytes || len(e.Media) > 8 || e.SourceRevision < 0 || !validCheckpoint(e.Checkpoint) {
		return ErrInvalidEvent
	}
	switch e.Kind {
	case EventOriginal, EventEditInvalidation, EventEditContent, EventDelete:
	default:
		return ErrInvalidEvent
	}
	switch e.BodyKind {
	case "", MessageText, MessageImage, MessageVoice, MessageDocument, MessageUnsupported:
	default:
		return ErrInvalidEvent
	}
	if (e.Kind == EventDelete || e.Kind == EventEditInvalidation) && (e.Body != "" || e.Caption != "" || len(e.Media) > 0) {
		return ErrInvalidEvent
	}
	if e.OriginalDateKnown && e.OriginalSentAt.IsZero() {
		return ErrInvalidEvent
	}
	if e.ReplyTo != nil && !validKey(*e.ReplyTo) {
		return ErrInvalidEvent
	}
	for _, m := range e.Media {
		if len(m.ProtocolReference) > maxProtocolReferenceBytes || m.DeclaredBytes < 0 || len(m.MimeType) > 256 || len(m.Kind) > 64 || !utf8.ValidString(m.MimeType) || !validDocumentFilename(m.Filename) || !validMediaAvailability(m.Availability) {
			return ErrInvalidEvent
		}
	}
	return nil
}
func cloneMedia(in []MediaReference) []MediaReference {
	out := append([]MediaReference(nil), in...)
	for i := range out {
		out[i].ProtocolReference = append([]byte(nil), out[i].ProtocolReference...)
	}
	return out
}

type sqlQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) loadPolicy(ctx context.Context, q sqlQuerier) (ArchivePolicy, error) {
	var p ArchivePolicy
	var start, rolling int64
	e := q.QueryRowContext(ctx, "SELECT generation,date_mode,import_start,chat_mode,rolling_retention FROM archive_policy WHERE id=1").Scan(&p.Generation, &p.DateMode, &start, &p.ChatMode, &rolling)
	if errors.Is(e, sql.ErrNoRows) {
		return p, nil
	}
	if e != nil {
		return p, ErrArchiveUnavailable
	}
	p.ImportStart = fromMillis(start)
	p.RollingRetention = time.Duration(rolling)
	rows, e := q.QueryContext(ctx, "SELECT chat_id FROM policy_chats ORDER BY chat_id")
	if e != nil {
		return p, ErrArchiveUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var id ChatID
		if e = rows.Scan(&id); e != nil {
			return p, ErrArchiveUnavailable
		}
		p.IncludedChats = append(p.IncludedChats, id)
	}
	if rows.Err() != nil {
		return p, ErrArchiveUnavailable
	}
	return p, nil
}
func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	invalidates := true
	return s.transactionWithSafety(ctx, &invalidates, fn)
}
func (s *Store) transactionWithSafety(ctx context.Context, invalidates *bool, fn func(*sql.Tx) error, sourceAccount ...string) error {
	tx, e := s.writeDB.BeginTx(ctx, nil)
	if e != nil {
		return fixedSQLError(e)
	}
	defer tx.Rollback()
	var beforeRev, afterRev, beforePolicy, afterPolicy int64
	if e = tx.QueryRowContext(ctx, "SELECT archive_revision,COALESCE((SELECT generation FROM archive_policy WHERE id=1),0) FROM archive_meta WHERE id=1").Scan(&beforeRev, &beforePolicy); e != nil {
		return fixedSQLError(e)
	}
	if e = fn(tx); e != nil {
		return fixedSQLError(e)
	}
	if e = tx.QueryRowContext(ctx, "SELECT archive_revision,COALESCE((SELECT generation FROM archive_policy WHERE id=1),0) FROM archive_meta WHERE id=1").Scan(&afterRev, &afterPolicy); e != nil {
		return fixedSQLError(e)
	}
	// Final reader epoch validation and the commit share this lock. A source
	// invalidation cannot commit in the gap before its memory epoch advances.
	s.safetyMu.Lock()
	if len(sourceAccount) > 0 {
		if e = s.extensionSafetyLocked(sourceAccount[0]); e != nil {
			s.safetyMu.Unlock()
			return e
		}
	}
	e = tx.Commit()
	if e == nil && ((*invalidates && beforeRev != afterRev) || beforePolicy != afterPolicy) {
		s.safetyEpoch++
	}
	s.safetyMu.Unlock()
	return fixedSQLError(e)
}
func fixedSQLError(e error) error {
	if e == nil {
		return nil
	}
	var code Error
	if errors.As(e, &code) || errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	return ErrArchiveUnavailable
}
func bindAccount(ctx context.Context, tx *sql.Tx, a string) error {
	var existing string
	e := tx.QueryRowContext(ctx, "SELECT account FROM accounts LIMIT 1").Scan(&existing)
	if e == nil && existing != a {
		return ErrUnauthorized
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO accounts(account) VALUES(?)", a); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO account_safety(account) VALUES(?)", a)
	return e
}
func (s *Store) setPolicy(ctx context.Context, p ArchivePolicy, expected Generation) (ArchivePolicy, error) {
	if (p.DateMode != DateSince && p.DateMode != DateAllAvailable) || (p.DateMode == DateSince && p.ImportStart.IsZero()) || (p.DateMode == DateAllAvailable && !p.ImportStart.IsZero()) || (p.ChatMode != ChatsAll && p.ChatMode != ChatsSelective) || (p.ChatMode == ChatsAll && len(p.IncludedChats) > 0) || p.RollingRetention < 0 || len(p.IncludedChats) > 10000 {
		return ArchivePolicy{}, ErrInvalidPolicy
	}
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		old, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		if old.Generation != expected {
			return ErrPolicyChanged
		}
		p.Generation = old.Generation + 1
		if old.Generation != 0 {
			if e = extensionInvalidateAll(ctx, tx, ""); e != nil {
				return e
			}
		}
		seen := map[ChatID]bool{}
		for _, id := range p.IncludedChats {
			if !validString(string(id), 128) || seen[id] {
				return ErrInvalidPolicy
			}
			seen[id] = true
			var n int
			if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM chats WHERE id=?", id).Scan(&n); e != nil {
				return e
			}
			if n != 1 {
				return ErrInvalidPolicy
			}
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO archive_policy(id,generation,date_mode,import_start,chat_mode,rolling_retention) VALUES(1,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET generation=excluded.generation,date_mode=excluded.date_mode,import_start=excluded.import_start,chat_mode=excluded.chat_mode,rolling_retention=excluded.rolling_retention", p.Generation, p.DateMode, unixms(p.ImportStart), p.ChatMode, int64(p.RollingRetention)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM policy_chats"); e != nil {
			return e
		}
		for _, id := range p.IncludedChats {
			if _, e = tx.ExecContext(ctx, "INSERT INTO policy_chats(chat_id) VALUES(?)", id); e != nil {
				return e
			}
		}
		// Select metadata only; prohibited bodies never need to leave SQLite.
		lower := unixms(p.ImportStart)
		if p.DateMode == DateAllAvailable {
			lower = 0
		}
		if p.RollingRetention > 0 {
			r := unixms(s.now().Add(-p.RollingRetention))
			if r > lower {
				lower = r
			}
		}
		rows, e := tx.QueryContext(ctx, "SELECT rowid,id,chat_id,original_sent_at FROM messages WHERE state='active' AND (original_date_known=0 OR original_sent_at<? OR (?='selective' AND chat_id NOT IN(SELECT chat_id FROM policy_chats)))", lower, p.ChatMode)
		if e != nil {
			return e
		}
		type removed struct {
			row      int64
			id, chat string
			sent     int64
		}
		var list []removed
		for rows.Next() {
			var v removed
			if e = rows.Scan(&v.row, &v.id, &v.chat, &v.sent); e != nil {
				rows.Close()
				return e
			}
			list = append(list, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(list) > 0 {
			rev, e := bumpRevision(ctx, tx)
			if e != nil {
				return e
			}
			for _, v := range list {
				if _, e = tx.ExecContext(ctx, "DELETE FROM message_fts WHERE rowid=?", v.row); e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, "UPDATE messages SET state='excluded',body='',caption='',normalized_body='',normalized_caption='',content_hash='',extension_media_identity='',media_availability='not_retained',local_revision=local_revision+1,reply_account='',reply_chat='',reply_sender='',reply_message='',reply_from_me=0 WHERE id=?", v.id); e != nil {
					return e
				}
				if e = dirtyWindow(ctx, tx, v.chat, p.Generation, v.sent, rev); e != nil {
					return e
				}
			}
		} else if old.Generation != 0 {
			if _, e = bumpRevision(ctx, tx); e != nil {
				return e
			}
		}
		// Reconstruct pending work from CURRENT authorized source metadata, rather
		// than treating an older generation payload as new authorization.
		if e = reconstructDirty(ctx, tx, p.Generation); e != nil {
			return e
		}
		// Older-generation rebuild payloads carry no new permission.
		if e = s.reviseRebuildPolicy(ctx, tx, p); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "DELETE FROM dirty_windows WHERE generation<>?", p.Generation)
		return e
	})
	return p, e
}
func canonicalQuery(ctx context.Context, q sqlQuerier, account, identity string) (string, error) {
	var c string
	e := q.QueryRowContext(ctx, "SELECT canonical FROM identity_aliases WHERE account=? AND alias=?", account, identity).Scan(&c)
	if errors.Is(e, sql.ErrNoRows) {
		return identity, nil
	}
	if e != nil {
		return "", e
	}
	return c, nil
}
func canonicalKey(ctx context.Context, q sqlQuerier, k SourceKey) (SourceKey, error) {
	var e error
	if k.Chat, e = canonicalQuery(ctx, q, k.Account, k.Chat); e != nil {
		return k, e
	}
	k.Sender, e = canonicalQuery(ctx, q, k.Account, k.Sender)
	return k, e
}
func ensureChat(ctx context.Context, tx *sql.Tx, k SourceKey, sent int64) (string, error) {
	var id string
	e := tx.QueryRowContext(ctx, "SELECT id FROM chats WHERE account=? AND source_chat=?", k.Account, k.Chat).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		id, e = randomID()
		if e != nil {
			return "", e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO chats(id,account,source_chat,observed_oldest,observed_latest) VALUES(?,?,?,?,?)", id, k.Account, k.Chat, sent, sent)
	} else if e == nil && sent != 0 {
		_, e = tx.ExecContext(ctx, "UPDATE chats SET observed_oldest=CASE WHEN observed_oldest=0 OR observed_oldest>? THEN ? ELSE observed_oldest END,observed_latest=MAX(observed_latest,?) WHERE id=?", sent, sent, sent, id)
	}
	return id, e
}
func ensureIdentity(ctx context.Context, tx *sql.Tx, k SourceKey) (string, error) {
	var id string
	e := tx.QueryRowContext(ctx, "SELECT id FROM identities WHERE account=? AND source_identity=?", k.Account, k.Sender).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		id, e = randomID()
		if e != nil {
			return "", e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO identities(id,account,source_identity) VALUES(?,?,?)", id, k.Account, k.Sender)
	}
	return id, e
}
func saveAnchor(ctx context.Context, tx *sql.Tx, k SourceKey, sent int64, known bool) error {
	if _, e := tx.ExecContext(ctx, "INSERT INTO source_anchors(account,chat,sender,message,from_me,original_sent_at,date_known) VALUES(?,?,?,?,?,?,?) ON CONFLICT(account,chat,sender,message,from_me) DO UPDATE SET original_sent_at=CASE WHEN source_anchors.date_known=0 AND excluded.date_known=1 THEN excluded.original_sent_at ELSE source_anchors.original_sent_at END,date_known=MAX(source_anchors.date_known,excluded.date_known)", k.Account, k.Chat, k.Sender, k.Message, k.FromMe, sent, known); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, "DELETE FROM source_anchors WHERE rowid IN(SELECT rowid FROM source_anchors WHERE account=? AND chat=? ORDER BY original_sent_at DESC,rowid DESC LIMIT -1 OFFSET ?)", k.Account, k.Chat, maxAnchorsPerChat)
	return e
}
func bumpRevision(ctx context.Context, tx *sql.Tx) (ArchiveRevision, error) {
	_, e := tx.ExecContext(ctx, "UPDATE archive_meta SET archive_revision=archive_revision+1 WHERE id=1")
	if e != nil {
		return 0, e
	}
	var rev ArchiveRevision
	e = tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&rev)
	return rev, e
}
func dirtyWindow(ctx context.Context, tx *sql.Tx, chat string, g Generation, sent int64, rev ArchiveRevision) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO dirty_windows(chat_id,generation,from_time,until_time,archive_revision) VALUES(?,?,?,?,?) ON CONFLICT(chat_id,generation) DO UPDATE SET from_time=MIN(from_time,excluded.from_time),until_time=MAX(until_time,excluded.until_time),archive_revision=excluded.archive_revision", chat, g, sent, sent+1, rev)
	if e != nil {
		return e
	}
	return extensionDirty(ctx, tx, chat, g, sent, rev)
}
func saveCheckpoint(ctx context.Context, tx *sql.Tx, k SourceKey, c *SyncCheckpoint) error {
	if c == nil {
		return nil
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO sync_state(account,chat,checkpoint_kind,checkpoint_id) VALUES(?,?,?,?) ON CONFLICT(account,chat) DO UPDATE SET checkpoint_kind=excluded.checkpoint_kind,checkpoint_id=excluded.checkpoint_id", k.Account, k.Chat, c.Kind, c.ID)
	return e
}
func hashEvent(e SourceEvent) string {
	h := sha256.New()
	for _, s := range []string{e.Body, e.Caption, string(e.BodyKind)} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	if e.ReplyTo != nil {
		for _, s := range []string{e.ReplyTo.Account, e.ReplyTo.Chat, e.ReplyTo.Sender, e.ReplyTo.Message} {
			h.Write([]byte(s))
			h.Write([]byte{0})
		}
		if e.ReplyTo.FromMe {
			h.Write([]byte{1})
		}
	}
	for _, m := range e.Media {
		h.Write([]byte(m.Kind))
		h.Write([]byte{0})
		h.Write([]byte(m.MimeType))
		h.Write([]byte{0})
		h.Write(m.ProtocolReference)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

type sourceRow struct {
	rowid             int64
	id                MessageID
	chatID, senderID  string
	sent              int64
	known             bool
	protocol          int64
	local             uint64
	state, kind, hash string
	mediaIdentity     string
	expires           int64
	viewOnce          bool
	barrier           string
}

func loadSourceRow(ctx context.Context, q sqlQuerier, k SourceKey) (sourceRow, bool, error) {
	var v sourceRow
	e := q.QueryRowContext(ctx, "SELECT rowid,id,chat_id,sender_id,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,content_hash,expires_at,view_once,revision_barrier,extension_media_identity FROM messages WHERE account=? AND source_chat=? AND source_sender=? AND source_message=? AND from_me=?", k.Account, k.Chat, k.Sender, k.Message, k.FromMe).Scan(&v.rowid, &v.id, &v.chatID, &v.senderID, &v.sent, &v.known, &v.protocol, &v.local, &v.state, &v.kind, &v.hash, &v.expires, &v.viewOnce, &v.barrier, &v.mediaIdentity)
	if errors.Is(e, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, e == nil, e
}
func versionOf(v sourceRow, g Generation) SourceVersion {
	return SourceVersion{MessageID: v.id, Revision: v.local, PolicyGeneration: g, Deleted: v.state == "deleted", Excluded: v.state == "excluded", ExpiresAt: fromMillis(v.expires)}
}
func permittedPolicy(p ArchivePolicy, chat string, sent int64, known bool, now time.Time) bool {
	if p.Generation == 0 || !known {
		return false
	}
	if p.ChatMode == ChatsSelective {
		allowed := false
		for _, id := range p.IncludedChats {
			if string(id) == chat {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if p.DateMode == DateSince && sent < unixms(p.ImportStart) {
		return false
	}
	if p.RollingRetention > 0 && sent < unixms(now.Add(-p.RollingRetention)) {
		return false
	}
	return true
}
func (s *Store) applyEvent(ctx context.Context, e SourceEvent, expected Generation) (ApplyResult, error) {
	var result ApplyResult
	var boundaryErr error
	if _, err := s.readSafety(e.Key.Account); err != nil {
		return result, err
	}
	invalidates := true
	err := s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		p, err := s.loadPolicy(ctx, tx)
		if err != nil {
			return err
		}
		if p.Generation == 0 {
			return ErrInvalidPolicy
		}
		if p.Generation != expected && e.DeliveredHistory == nil {
			return ErrPolicyChanged
		}
		result.PolicyGeneration = p.Generation
		if _, err = s.readSafety(e.Key.Account); err != nil {
			return err
		}
		if err = bindAccount(ctx, tx, e.Key.Account); err != nil {
			return err
		}
		e.Key, err = canonicalKey(ctx, tx, e.Key)
		if err != nil {
			return err
		}
		k := e.Key
		admitted, err := s.rebuildAdmit(ctx, tx, &e, p.Generation)
		if err != nil {
			return err
		}
		if e.DeliveredHistory != nil && expected != p.Generation {
			admitted = false
		}
		if !admitted {
			result.Outcome = OutcomeFiltered
			return tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&result.ArchiveRevision)
		}
		guarded, err := hasUnresolvedDeletion(ctx, tx, k.Account, k.Chat, k.Message)
		if err != nil {
			return err
		}
		if guarded {
			// An unresolved group deletion is terminal across every sender and
			// direction. Do not turn a later original into a guessed target,
			// source anchor, or content-bearing SQL argument.
			v, exists, err := loadSourceRow(ctx, tx, k)
			if err != nil {
				return err
			}
			result.Outcome = OutcomeStale
			result.Version = versionOf(v, p.Generation)
			result.Version.Deleted = true
			if exists {
				result.MessageID = v.id
			}
			if err = saveCheckpoint(ctx, tx, k, e.Checkpoint); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&result.ArchiveRevision)
		}
		sent := int64(0)
		if e.OriginalDateKnown {
			sent = unixms(e.OriginalSentAt)
		}
		chatID, err := ensureChat(ctx, tx, k, sent)
		if err != nil {
			return err
		}
		if s.rebuild != nil {
			_, err = s.saveRebuildAnchor(ctx, tx, k, sent, e.OriginalDateKnown)
		} else {
			err = saveAnchor(ctx, tx, k, sent, e.OriginalDateKnown)
		}
		if err != nil {
			return err
		}
		v, exists, err := loadSourceRow(ctx, tx, k)
		if err != nil {
			return err
		}
		documentDateMatches := exists && v.known && e.OriginalDateKnown && unixms(e.OriginalSentAt) == v.sent
		metadataChanged := false
		if exists && v.known {
			sent = v.sent
			e.OriginalDateKnown = true
		} else if exists && e.OriginalDateKnown {
			v.sent = sent
			v.known = true
			metadataChanged = true
		}
		deleted := exists && v.state == "deleted"
		var tombRev int64
		err = tx.QueryRowContext(ctx, "SELECT protocol_revision FROM source_tombstones WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?", k.Account, k.Chat, k.Sender, k.Message, k.FromMe).Scan(&tombRev)
		if err == nil {
			deleted = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var cutoff int64
		err = tx.QueryRowContext(ctx, "SELECT cutoff FROM chat_tombstones WHERE account=? AND chat=?", k.Account, k.Chat).Scan(&cutoff)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		cleared := err == nil && e.OriginalDateKnown && sent <= cutoff
		allowed := permittedPolicy(p, chatID, sent, e.OriginalDateKnown, s.now())
		expires := unixms(e.ExpiresAt)
		if exists && v.expires > 0 && (expires == 0 || expires > v.expires) {
			expires = v.expires
		}
		viewOnce := v.viewOnce
		for _, media := range e.Media {
			viewOnce = viewOnce || media.ViewOnce
		}
		securityChanged := exists && (expires != v.expires || viewOnce != v.viewOnce)
		if expires > 0 && expires <= unixms(s.now()) {
			allowed = false
		}
		invalidates = exists || e.Kind != EventOriginal || !allowed || deleted || cleared
		if deleted {
			result.Outcome = OutcomeStale
			result.Version = versionOf(v, p.Generation)
			result.Version.Deleted = true
			result.MessageID = v.id
			if metadataChanged {
				if _, err = tx.ExecContext(ctx, "UPDATE messages SET original_sent_at=?,original_date_known=1 WHERE id=?", sent, v.id); err != nil {
					return err
				}
			}
			if err = saveCheckpoint(ctx, tx, k, e.Checkpoint); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&result.ArchiveRevision)
		}
		if (e.Kind == EventOriginal || e.Kind == EventEditContent) && !allowed && !exists && e.Kind == EventOriginal && !cleared {
			result.Outcome = OutcomeFiltered
			if !e.OriginalDateKnown {
				result.Outcome = OutcomeAwaitingMetadata
			}
			if err = saveCheckpoint(ctx, tx, k, e.Checkpoint); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&result.ArchiveRevision)
		}
		// A metadata-only mutation barrier exists even when its original has not
		// arrived. Incoming prohibited body/caption are never SQL arguments.
		if !exists {
			senderID, err := ensureIdentity(ctx, tx, k)
			if err != nil {
				return err
			}
			id, err := randomID()
			if err != nil {
				return err
			}
			kind := e.BodyKind
			if kind == "" {
				kind = MessageUnsupported
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,0,'invalidated',?,?,?)", id, k.Account, chatID, senderID, k.Chat, k.Sender, k.Message, k.FromMe, sent, e.OriginalDateKnown, e.SourceRevision, e.Kind, kind, expires)
			if err != nil {
				return err
			}
			v, _, err = loadSourceRow(ctx, tx, k)
			if err != nil {
				return err
			}
		}
		result.MessageID = v.id
		// Terminal control ordering never relies on receipt or wall-clock time.
		terminal := e.Kind == EventDelete || cleared
		missing := !terminal && (e.Kind == EventEditInvalidation || e.Kind == EventEditContent) && e.SourceRevision == 0
		oldOriginal := exists && e.Kind == EventOriginal && (v.kind == string(EventEditInvalidation) || v.kind == string(EventEditContent))
		stale := !terminal && !missing && exists && (e.SourceRevision < v.protocol || oldOriginal)
		if !terminal && exists && v.barrier != "" && e.SourceRevision <= v.protocol {
			boundaryErr = Error(v.barrier)
			stale = true
		}
		if stale {
			result.Outcome = OutcomeStale
			if metadataChanged || securityChanged {
				if err = extensionInvalidate(ctx, tx, v.id, 0); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, "UPDATE messages SET original_sent_at=?,original_date_known=?,expires_at=?,view_once=?,media_availability=CASE WHEN ? THEN 'view_once' ELSE media_availability END,local_revision=local_revision+1 WHERE id=?", sent, e.OriginalDateKnown, expires, viewOnce, viewOnce, v.id)
				if err != nil {
					return err
				}
				v.local++
				v.expires = expires
				v.viewOnce = viewOnce
				if expires > 0 && expires <= unixms(s.now()) {
					if err = extensionInvalidate(ctx, tx, v.id, 0); err != nil {
						return err
					}
					if _, err = tx.ExecContext(ctx, "DELETE FROM message_fts WHERE rowid=?", v.rowid); err != nil {
						return err
					}
					if _, err = tx.ExecContext(ctx, "UPDATE messages SET state='expired',body='',caption='',normalized_body='',normalized_caption='',content_hash='',extension_media_identity='',media_availability='expired',reply_account='',reply_chat='',reply_sender='',reply_message='',reply_from_me=0 WHERE id=?", v.id); err != nil {
						return err
					}
					v.state = "expired"
				}
				rev, err := bumpRevision(ctx, tx)
				if err != nil {
					return err
				}
				result.ArchiveRevision = rev
				if err = dirtyWindow(ctx, tx, chatID, p.Generation, sent, rev); err != nil {
					return err
				}
			}
			if err = saveCheckpoint(ctx, tx, k, e.Checkpoint); err != nil {
				return err
			}
			if result.ArchiveRevision == 0 {
				if err = tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&result.ArchiveRevision); err != nil {
					return err
				}
			}
			result.Version = versionOf(v, p.Generation)
			return nil
		}
		hash := ""
		mediaIdentity := ""
		if allowed && (e.Kind == EventOriginal || e.Kind == EventEditContent) {
			hash = hashEvent(e)
			mediaIdentity, err = sourceMediaIdentity(e.Media)
			if err != nil {
				return err
			}
		}
		same := exists && e.SourceRevision == v.protocol
		exactFields := false
		if exists && allowed && v.state == "active" && v.hash == hash && (e.Kind == EventOriginal || e.Kind == EventEditContent) {
			// Preserve the v1 content hash, whose delimiter encoding permits
			// UTF-8 NUL body/caption boundary collisions. Only currently allowed
			// bytes reach this exact-field comparison; stale inputs returned above.
			kind := e.BodyKind
			if kind == "" {
				kind = MessageUnsupported
			}
			if err = tx.QueryRowContext(ctx, "SELECT body=? AND caption=? AND body_kind=? FROM messages WHERE id=?", e.Body, e.Caption, kind, v.id).Scan(&exactFields); err != nil {
				return err
			}
		}
		duplicate := !terminal && !missing && !metadataChanged && !securityChanged && same && ((e.Kind == EventEditInvalidation && (v.kind == string(EventEditInvalidation) || v.kind == string(EventEditContent))) || ((e.Kind == EventOriginal || e.Kind == EventEditContent) && v.state == "active" && v.hash == hash && exactFields && v.mediaIdentity != "" && v.mediaIdentity == mediaIdentity && v.kind == string(e.Kind)))
		if duplicate {
			result.MediaIDs, err = s.extensionAssociateMedia(ctx, tx, SourceRef{Account: k.Account, Version: versionOf(v, p.Generation), ContentHash: v.hash}, e.Media)
			if err != nil {
				return err
			}
			result.Outcome = OutcomeDuplicate
			result.Version = versionOf(v, p.Generation)
			if err = saveCheckpoint(ctx, tx, k, e.Checkpoint); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&result.ArchiveRevision)
		}
		promotedDocument := false
		if !terminal && !missing && !metadataChanged && !securityChanged && same && exists && allowed {
			promotedDocument, err = documentPromotionEligible(ctx, tx, v, e, documentDateMatches)
			if err != nil {
				return err
			}
		}
		conflict := !promotedDocument && !terminal && !missing && same && exists && allowed && v.state == "active" && v.kind == string(e.Kind) && (v.hash != hash || !exactFields || v.mediaIdentity != "" && v.mediaIdentity != mediaIdentity) && (e.Kind == EventOriginal || e.Kind == EventEditContent)
		state := "invalidated"
		body, caption, normalBody, normalCaption := "", "", "", ""
		availability := "not_retained"
		outcome := OutcomeApplied
		if terminal {
			state = "deleted"
			if _, err = tx.ExecContext(ctx, "INSERT INTO source_tombstones(account,chat,sender,message,from_me,protocol_revision) VALUES(?,?,?,?,?,?) ON CONFLICT(account,chat,sender,message,from_me) DO UPDATE SET protocol_revision=MAX(protocol_revision,excluded.protocol_revision)", k.Account, k.Chat, k.Sender, k.Message, k.FromMe, e.SourceRevision); err != nil {
				return err
			}
		} else if !allowed {
			state = "excluded"
			outcome = OutcomeFiltered
			if !e.OriginalDateKnown {
				state = "invalidated"
				outcome = OutcomeAwaitingMetadata
			}
		} else if e.Kind == EventOriginal || e.Kind == EventEditContent {
			state = "active"
			body = e.Body
			caption = e.Caption
			normalBody = NormalizeLexical(body)
			normalCaption = NormalizeLexical(caption)
			if len(e.Media) > 0 {
				if e.Media[0].ViewOnce {
					availability = "view_once"
				} else if e.Media[0].Availability != "" && e.Media[0].Availability != "retained" {
					availability = e.Media[0].Availability
				}
			}
		}
		if conflict || missing {
			state = "invalidated"
			body = ""
			caption = ""
			normalBody = ""
			normalCaption = ""
			hash = ""
			code := "source_revision_conflict"
			boundaryErr = ErrSourceRevisionConflict
			if missing {
				code = "mutation_metadata_missing"
				boundaryErr = ErrMutationMetadataMissing
			}
			if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,?,?)", k.Account, k.Chat, code); err != nil {
				return err
			}
		}
		if !terminal && expires > 0 && expires <= unixms(s.now()) {
			state = "expired"
			availability = "expired"
		}
		// The service clock may cross expiry during hashing/normalization.
		// Classify and clear from the final state, not the earlier admission.
		if state != "active" {
			body, caption, normalBody, normalCaption, hash = "", "", "", "", ""
			mediaIdentity = ""
			invalidates = true
		}
		if viewOnce && state == "active" {
			availability = "view_once"
		}
		barrier := ""
		if conflict {
			barrier = "source_revision_conflict"
		}
		if missing {
			barrier = "mutation_metadata_missing"
		}
		protocol := e.SourceRevision
		if missing && v.protocol > protocol {
			protocol = v.protocol
		}
		reply := SourceKey{}
		if state == "active" && e.ReplyTo != nil && e.ReplyTo.Account == k.Account {
			reply, err = canonicalKey(ctx, tx, *e.ReplyTo)
			if err != nil {
				return err
			}
		}
		if err = extensionInvalidate(ctx, tx, v.id, 0); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM message_fts WHERE rowid=?", v.rowid); err != nil {
			return err
		}
		edited := e.Kind == EventEditInvalidation || e.Kind == EventEditContent || oldOriginal
		kind := e.BodyKind
		if kind == "" {
			kind = MessageUnsupported
		}
		sourceKind := e.Kind
		if missing {
			sourceKind = EventEditInvalidation
		}
		if _, err = tx.ExecContext(ctx, "UPDATE messages SET original_sent_at=?,original_date_known=?,protocol_revision=?,local_revision=local_revision+1,state=?,source_kind=?,body=?,caption=?,normalized_body=?,normalized_caption=?,body_kind=?,content_hash=?,extension_media_identity=?,edited=?,expires_at=?,media_availability=?,view_once=?,revision_barrier=?,reply_account=?,reply_chat=?,reply_sender=?,reply_message=?,reply_from_me=? WHERE id=?", sent, e.OriginalDateKnown, protocol, state, sourceKind, body, caption, normalBody, normalCaption, kind, hash, mediaIdentity, edited, expires, availability, viewOnce, barrier, reply.Account, reply.Chat, reply.Sender, reply.Message, reply.FromMe, v.id); err != nil {
			return err
		}
		if promotedDocument {
			if _, err = tx.ExecContext(ctx, "DELETE FROM extension_document_promotions WHERE message_id=?", v.id); err != nil {
				return err
			}
		}
		if state == "active" {
			if _, err = tx.ExecContext(ctx, "INSERT INTO message_fts(rowid,normalized_body,normalized_caption) VALUES(?,?,?)", v.rowid, normalBody, normalCaption); err != nil {
				return err
			}
		}
		rev, err := bumpRevision(ctx, tx)
		if err != nil {
			return err
		}
		if err = dirtyWindow(ctx, tx, chatID, p.Generation, sent, rev); err != nil {
			return err
		}
		if err = saveCheckpoint(ctx, tx, k, e.Checkpoint); err != nil {
			return err
		}
		v.sent = sent
		v.known = e.OriginalDateKnown
		v.local++
		v.protocol = protocol
		v.state = state
		v.kind = string(sourceKind)
		v.hash = hash
		v.mediaIdentity = mediaIdentity
		v.expires = expires
		v.viewOnce = viewOnce
		v.barrier = barrier
		result.Outcome = outcome
		result.ArchiveRevision = rev
		result.Version = versionOf(v, p.Generation)
		if state == "active" {
			result.MediaIDs, err = s.extensionAssociateMedia(ctx, tx, SourceRef{Account: k.Account, Version: result.Version, ContentHash: hash}, e.Media)
			if err != nil {
				return err
			}
		}
		if state == "active" && outcome == OutcomeApplied {
			if err = s.recordDeliveredBody(ctx, tx, e); err != nil {
				return err
			}
		}
		if !allowed && !terminal {
			result.MessageID = ""
		}
		return nil
	}, e.Key.Account)
	if err != nil {
		return ApplyResult{}, err
	}
	return result, boundaryErr
}
func (s *Store) setSync(ctx context.Context, u SyncUpdate) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if e := bindAccount(ctx, tx, u.Account); e != nil {
			return e
		}
		chat, e := canonicalQuery(ctx, tx, u.Account, u.Chat)
		if e != nil {
			return e
		}
		oldest, latest := unixms(u.ObservedOldest), unixms(u.ObservedLatest)
		kind, id := "", ""
		if u.Checkpoint != nil {
			kind = u.Checkpoint.Kind
			id = u.Checkpoint.ID
		}
		// Coverage-only updates do not describe the connection. Preserve its
		// previous state, and use disconnected only when creating a new row.
		connection := u.Connection
		if connection == "" {
			connection = "disconnected"
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO sync_state(account,chat,connection,checkpoint_kind,checkpoint_id,observed_oldest,observed_latest,received,applied,dropped,coverage_unknown) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account,chat) DO UPDATE SET connection=CASE WHEN ? THEN excluded.connection ELSE connection END,checkpoint_kind=CASE WHEN excluded.checkpoint_id<>'' THEN excluded.checkpoint_kind ELSE checkpoint_kind END,checkpoint_id=CASE WHEN excluded.checkpoint_id<>'' THEN excluded.checkpoint_id ELSE checkpoint_id END,observed_oldest=CASE WHEN observed_oldest=0 OR (excluded.observed_oldest>0 AND excluded.observed_oldest<observed_oldest) THEN excluded.observed_oldest ELSE observed_oldest END,observed_latest=MAX(observed_latest,excluded.observed_latest),received=MAX(received,excluded.received),applied=MAX(applied,excluded.applied),dropped=MAX(dropped,excluded.dropped),coverage_unknown=MAX(coverage_unknown,excluded.coverage_unknown)", u.Account, chat, connection, kind, id, oldest, latest, u.Received, u.Applied, u.Dropped, u.CoverageUnknown, u.Connection != "")
		if e != nil {
			return e
		}
		if u.Chat != "" {
			if _, e = ensureChat(ctx, tx, SourceKey{Account: u.Account, Chat: chat}, oldest); e != nil {
				return e
			}
			if latest != 0 {
				if _, e = ensureChat(ctx, tx, SourceKey{Account: u.Account, Chat: chat}, latest); e != nil {
					return e
				}
			}
		}
		if u.GapCode != "" {
			_, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,?,?)", u.Account, chat, u.GapCode)
		}
		return e
	})
}

type sourceDeletion struct {
	row                                   int64
	id, chat, sourceChat, sender, message string
	from                                  bool
	revision, sent                        int64
}

func terminalizeSource(ctx context.Context, tx *sql.Tx, account string, generation Generation, v sourceDeletion, rev ArchiveRevision) error {
	if err := extensionInvalidate(ctx, tx, MessageID(v.id), rev); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM message_fts WHERE rowid=?", v.row); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE messages SET state='deleted',body='',caption='',normalized_body='',normalized_caption='',content_hash='',extension_media_identity='',local_revision=local_revision+1,media_availability='not_retained',reply_account='',reply_chat='',reply_sender='',reply_message='',reply_from_me=0 WHERE id=?", v.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO source_tombstones(account,chat,sender,message,from_me,protocol_revision) VALUES(?,?,?,?,?,?) ON CONFLICT(account,chat,sender,message,from_me) DO UPDATE SET protocol_revision=MAX(protocol_revision,excluded.protocol_revision)", account, v.sourceChat, v.sender, v.message, v.from, v.revision); err != nil {
		return err
	}
	return dirtyWindow(ctx, tx, v.chat, generation, v.sent, rev)
}

func (s *Store) clearChat(ctx context.Context, a, c string, cutoff time.Time) error {
	if _, e := s.readSafety(a); e != nil {
		return e
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if e := bindAccount(ctx, tx, a); e != nil {
			return e
		}
		chat, e := canonicalQuery(ctx, tx, a, c)
		if e != nil {
			return e
		}
		p, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO chat_tombstones(account,chat,cutoff) VALUES(?,?,?) ON CONFLICT(account,chat) DO UPDATE SET cutoff=MAX(cutoff,excluded.cutoff)", a, chat, unixms(cutoff)); e != nil {
			return e
		}
		rows, e := tx.QueryContext(ctx, "SELECT rowid,id,chat_id,source_chat,source_sender,source_message,from_me,protocol_revision,original_sent_at FROM messages WHERE account=? AND source_chat=? AND original_date_known=1 AND original_sent_at<=? AND state<>'deleted'", a, chat, unixms(cutoff))
		if e != nil {
			return e
		}
		var list []sourceDeletion
		for rows.Next() {
			var v sourceDeletion
			if e = rows.Scan(&v.row, &v.id, &v.chat, &v.sourceChat, &v.sender, &v.message, &v.from, &v.revision, &v.sent); e != nil {
				rows.Close()
				return e
			}
			list = append(list, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		rev, e := bumpRevision(ctx, tx)
		if e != nil {
			return e
		}
		for _, v := range list {
			if e = terminalizeSource(ctx, tx, a, p.Generation, v, rev); e != nil {
				return e
			}
		}
		return nil
	})
}

func reconstructDirty(ctx context.Context, tx *sql.Tx, g Generation) error {
	return reconstructChatDirty(ctx, tx, g, "")
}
func reconstructChatDirty(ctx context.Context, tx *sql.Tx, g Generation, chat string) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO dirty_windows(chat_id,generation,from_time,until_time,archive_revision) SELECT m.chat_id,?,MIN(m.original_sent_at),MAX(m.original_sent_at)+1,(SELECT archive_revision FROM archive_meta WHERE id=1) FROM messages m JOIN archive_policy p ON p.id=1 WHERE m.state='active' AND m.original_date_known=1 AND (?='' OR m.chat_id=?) AND (p.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id)) AND (p.date_mode='all_available' OR m.original_sent_at>=p.import_start) GROUP BY m.chat_id HAVING 1 ON CONFLICT(chat_id,generation) DO UPDATE SET from_time=MIN(from_time,excluded.from_time),until_time=MAX(until_time,excluded.until_time),archive_revision=excluded.archive_revision", g, chat, chat)
	if e != nil {
		return e
	}
	return extensionReconstructDirty(ctx, tx, g, chat)
}

// SweepExpired schedules one bounded priority maintenance batch. Read access
// independently denies actual-clock expiry and retention before this cleanup.
func (s *Store) SweepExpired(ctx context.Context) error {
	_, e := s.submit(ctx, expiryCommand{}, "", true, 0)
	return e
}

func expiryMaintenanceDelay(pending bool, err error) time.Duration {
	if err == nil && pending {
		return time.Millisecond
	}
	return time.Minute
}

func (s *Store) runExpiryMaintenance(ctx context.Context, stage string) (bool, error) {
	pending, err := s.sweepExpired(ctx)
	if err != nil {
		s.recordExpiryMaintenanceFailure(stage, err)
	}
	return pending, err
}

// A failed cleanup may have an uncertain commit. Invalidate earlier responses
// without treating deterministic, already-denied expiry as a lost phone event.
// Existing unsafe state and physical ownership are preserved.
func (s *Store) recordExpiryMaintenanceFailure(stage string, err error) {
	if err == nil {
		return
	}
	s.safetyMu.Lock()
	s.safetyEpoch++
	s.safetyMu.Unlock()
	_, _ = os.Stderr.Write(expiryMaintenanceDiagnostic(stage, err))
}

func expiryMaintenanceDiagnostic(stage string, err error) []byte {
	switch stage {
	case "expiry_maintenance_admission", "expiry_maintenance_wait", "expiry_maintenance_result", "expiry_maintenance_timer":
	default:
		stage = "unknown"
	}
	class := "unknown"
	switch {
	case err == nil:
		class = "none"
	case errors.Is(err, context.Canceled):
		class = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		class = "deadline"
	case errors.Is(err, ErrArchiveUnavailable):
		class = "archive_unavailable"
	case errors.Is(err, ErrReconciliationRequired):
		class = "reconciliation_required"
	}
	encoded, _ := json.Marshal(struct {
		Event string `json:"event"`
		Stage string `json:"stage"`
		Code  string `json:"code"`
		Class string `json:"class"`
	}{"archive_failure", stage, "expiry_cleanup_failed", class})
	return append(encoded, '\n')
}

func (s *Store) sweepExpired(ctx context.Context) (bool, error) {
	// Offline original-cache repair preserves committed content byte-for-byte.
	// All source/read guards still use the actual clock and deny expired rows.
	if s.options.originalCacheRecovery {
		return false, nil
	}
	// Do not move the committed revision beneath a prepared owner recovery.
	// All reads/claims still enforce actual-clock expiry independently. Physical
	// cleanup resumes after explicit completion, including the next normal Open.
	if s.snapshotRecoveryPending.Load() {
		return false, nil
	}
	pending := false
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		p, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		if p.Generation == 0 {
			return nil
		}
		now := unixms(s.now())
		retention := int64(0)
		if p.RollingRetention > 0 {
			retention = unixms(s.now().Add(-p.RollingRetention))
		}
		rows, e := tx.QueryContext(ctx, "SELECT rowid,id,chat_id,original_sent_at,expires_at FROM messages WHERE state='active' AND ((expires_at>0 AND expires_at<=?) OR (?<>0 AND original_sent_at<?)) ORDER BY original_sent_at,id LIMIT 256", now, retention, retention)
		if e != nil {
			return e
		}
		type item struct {
			row           int64
			id, chat      string
			sent, expires int64
		}
		var list []item
		for rows.Next() {
			var v item
			if e = rows.Scan(&v.row, &v.id, &v.chat, &v.sent, &v.expires); e != nil {
				rows.Close()
				return e
			}
			list = append(list, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(list) == 0 {
			return nil
		}
		pending = len(list) == 256
		rev, e := bumpRevision(ctx, tx)
		if e != nil {
			return e
		}
		for _, v := range list {
			if e = extensionInvalidate(ctx, tx, MessageID(v.id), rev); e != nil {
				return e
			}
			state := "retention_expired"
			media := "not_retained"
			if v.expires > 0 && v.expires <= now {
				state = "expired"
				media = "expired"
			}
			if _, e = tx.ExecContext(ctx, "DELETE FROM message_fts WHERE rowid=?", v.row); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE messages SET state=?,body='',caption='',normalized_body='',normalized_caption='',content_hash='',extension_media_identity='',media_availability=?,local_revision=local_revision+1,reply_account='',reply_chat='',reply_sender='',reply_message='',reply_from_me=0 WHERE id=?", state, media, v.id); e != nil {
				return e
			}
			if e = dirtyWindow(ctx, tx, v.chat, p.Generation, v.sent, rev); e != nil {
				return e
			}
		}
		return nil
	})
	return pending, e
}

func checkpointBytes(c *SyncCheckpoint) int {
	if c == nil {
		return 0
	}
	return 64 + len(c.Kind) + len(c.ID)
}
