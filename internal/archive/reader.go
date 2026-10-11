package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxReadChats = 4096
const maxContextSide = 20
const maxContextBytes = 128 << 10
const maxViewFieldBytes = 16 << 10
const maxContextDerivedPerMessage = 8
const maxContextDerivedPerPage = 32
const maxContextProvenanceBytes = 4096

var _ GuardedReader = (*Store)(nil)

// readFrame owns a bounded snapshot. Every source query uses its authorization
// predicate before ordering/limiting, and finish verifies newer revocations.
type readFrame struct {
	s                  *Store
	ctx                context.Context
	cancel             context.CancelFunc
	tx                 *sql.Tx
	scope              ReadScope
	snapshot           ReadSnapshot
	epoch              uint64
	now, from, until   time.Time
	fixedFrom          time.Time
	rolling            time.Duration
	where              string
	args               []any
	minSent, minExpiry time.Time
	pins               map[MessageID]uint64
	captureSources     bool
	sources            map[MessageID]SourceReadVersion
	refSourceCopies    int
	roots              map[DocumentID]extensionRootResponsePin
	rootSources        map[MessageID]SourceReadVersion
}

const authorizedTables = `messages m JOIN chats c ON c.id=m.chat_id AND c.account=m.account
 JOIN identities i ON i.id=m.sender_id AND i.account=m.account
 JOIN account_safety saf ON saf.account=m.account AND saf.unsafe=0
 JOIN archive_policy ap ON ap.id=1`

func readError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrArchiveUnavailable
}
func validReadID(v string) bool {
	return v != "" && len(v) <= 128 && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}
func validReadTime(t time.Time) bool { return t.IsZero() || (t.Year() >= 1 && t.Year() <= 9999) }

// Both >= From and < Until compare millisecond source timestamps against a
// possibly fractional client bound. Ceiling preserves exact interval meaning.
func ceilMillis(t time.Time) int64 {
	ms := t.UTC().UnixMilli()
	if !t.Equal(time.UnixMilli(ms)) {
		ms++
	}
	return ms
}
func canonicalChats(chats []ChatID) ([]ChatID, error) {
	if len(chats) > maxReadChats {
		return nil, ErrInvalidQuery
	}
	out := append([]ChatID(nil), chats...)
	for _, id := range out {
		if !validReadID(string(id)) {
			return nil, ErrInvalidQuery
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	for _, id := range out {
		if n == 0 || id != out[n-1] {
			out[n] = id
			n++
		}
	}
	return out[:n], nil
}
func later(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.After(a)) {
		return b
	}
	return a
}
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

func (s *Store) beginRead(ctx context.Context, scope ReadScope, requestChats []ChatID, from, until time.Time) (*readFrame, error) {
	if scope.Account == "" || len(scope.Account) > 512 || !utf8.ValidString(scope.Account) || scope.Principal == "" || len(scope.Principal) > 256 || !utf8.ValidString(scope.Principal) || scope.Rolling < 0 || !validReadTime(scope.From) || !validReadTime(scope.Until) {
		return nil, ErrUnauthorized
	}
	if !scope.AllChats && len(scope.Chats) == 0 {
		return nil, ErrUnauthorized
	}
	if !validReadTime(from) || !validReadTime(until) || (!from.IsZero() && !until.IsZero() && !from.Before(until)) {
		return nil, ErrInvalidQuery
	}
	chats, err := canonicalChats(scope.Chats)
	if err != nil {
		return nil, ErrUnauthorized
	}
	scope.Chats = chats
	requested, err := canonicalChats(requestChats)
	if err != nil {
		return nil, err
	}
	epoch, err := s.readSafety(scope.Account)
	if err != nil {
		return nil, err
	}
	bounded, cancel := s.readContext(ctx)
	tx, err := s.readDB.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		cancel()
		return nil, readError(bounded, err)
	}
	f := &readFrame{s: s, ctx: bounded, cancel: cancel, tx: tx, scope: scope, epoch: epoch, now: s.now()}
	fail := func(e error) (*readFrame, error) { f.close(); return nil, e }
	var generation Generation
	var mode DateMode
	var importStart int64
	var rolling int64
	err = tx.QueryRowContext(bounded, `SELECT p.generation,p.date_mode,p.import_start,p.rolling_retention,a.archive_revision,a.index_generation
 FROM archive_policy p JOIN archive_meta a ON a.id=1 WHERE p.id=1`).Scan(&generation, &mode, &importStart, &rolling, &f.snapshot.ArchiveRevision, &f.snapshot.IndexGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrUnauthorized)
	}
	if err != nil {
		return fail(readError(bounded, err))
	}
	if generation == 0 {
		return fail(ErrUnauthorized)
	}
	if generation != scope.PolicyGeneration {
		return fail(ErrPolicyChanged)
	}
	f.snapshot.PolicyGeneration = generation
	var unsafe int
	err = tx.QueryRowContext(bounded, "SELECT unsafe FROM account_safety WHERE account=?", scope.Account).Scan(&unsafe)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrUnauthorized)
	}
	if err != nil {
		return fail(readError(bounded, err))
	}
	if unsafe != 0 {
		return fail(ErrReconciliationRequired)
	}
	f.from = later(scope.From.UTC(), from.UTC())
	f.until = earlier(scope.Until.UTC(), until.UTC())
	if mode == DateSince {
		f.from = later(f.from, fromMillis(importStart))
	}
	f.fixedFrom = f.from
	f.rolling = scope.Rolling
	if rolling > 0 && (f.rolling == 0 || time.Duration(rolling) < f.rolling) {
		f.rolling = time.Duration(rolling)
	}
	if f.rolling > 0 {
		f.from = later(f.from, f.now.Add(-f.rolling))
	}
	f.where = `m.account=? AND m.state='active' AND m.original_date_known=1 AND ap.generation=?
 AND (m.expires_at=0 OR m.expires_at>?) AND (ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id))
 AND (ap.date_mode='all_available' OR m.original_sent_at>=ap.import_start)`
	f.args = []any{scope.Account, generation, unixms(f.now)}
	addChats := func(ids []ChatID) {
		marks := make([]string, len(ids))
		for i, id := range ids {
			marks[i] = "?"
			f.args = append(f.args, id)
		}
		f.where += " AND m.chat_id IN (" + strings.Join(marks, ",") + ")"
	}
	if !scope.AllChats {
		addChats(chats)
	}
	if len(requested) > 0 {
		addChats(requested)
	}
	if !f.from.IsZero() {
		f.where += " AND m.original_sent_at>=?"
		f.args = append(f.args, ceilMillis(f.from))
	}
	if !f.until.IsZero() {
		f.where += " AND m.original_sent_at<?"
		f.args = append(f.args, ceilMillis(f.until))
	}
	return f, nil
}
func (f *readFrame) close() {
	if f.tx != nil {
		f.tx.Rollback()
		f.tx = nil
	}
	f.cancel()
}
func (f *readFrame) note(sent, expiry time.Time) {
	f.minSent = earlier(f.minSent, sent)
	f.minExpiry = earlier(f.minExpiry, expiry)
}
func (f *readFrame) pin(id MessageID, revision uint64) {
	if f.pins == nil {
		f.pins = make(map[MessageID]uint64)
	}
	f.pins[id] = revision
}

func (f *readFrame) captureSource(source SourceReadVersion, chatName, senderName string) error {
	if len(f.pins) > maxResponseSources {
		return ErrLimitExceeded
	}
	if !f.captureSources {
		return nil
	}
	metadataHash, err := sourceMetadataHash(chatName, senderName)
	if err != nil {
		return ErrSourceUnavailable
	}
	source.MetadataHash = metadataHash
	return f.captureReadSource(source)
}

// Generated document members already carry metadata hashes from their exact
// authorized SQL rows. Rehashing absent display names would weaken those pins.
func (f *readFrame) captureReadSource(source SourceReadVersion) error {
	if len(f.pins) > maxResponseSources {
		return ErrLimitExceeded
	}
	id := source.Ref.Version.MessageID
	if revision, exists := f.pins[id]; !exists || revision != source.Ref.Version.Revision || source.MetadataHash == [32]byte{} {
		return ErrSourceUnavailable
	}
	if !f.captureSources {
		return nil
	}
	if f.sources == nil {
		f.sources = make(map[MessageID]SourceReadVersion)
	}
	if previous, exists := f.sources[id]; exists && !extensionCandidateSameSource(previous, source) {
		return ErrSourceUnavailable
	}
	if _, exists := f.sources[id]; !exists && len(f.sources) == maxResponseSources {
		return ErrLimitExceeded
	}
	f.sources[id] = source
	return nil
}

// Root authority is independent of source revisions and of an active encoder.
// Keep these exact snapshot pins for the initial admission check even when the
// caller does not request a response guard. Membership allocation is reserved
// by extensionRootRef before loading the prepared source versions.
func (f *readFrame) captureRootRef(ref DocumentRef) error {
	if ref.root == nil {
		return nil
	}
	expected, err := extensionExpectedSources(ref)
	if err != nil {
		return err
	}
	if f.roots == nil {
		f.roots = make(map[DocumentID]extensionRootResponsePin)
	}
	if err = extensionAddRootPinLimit(f.roots, ref.root, maxCandidateRootPins); err != nil {
		return err
	}
	if f.rootSources == nil {
		f.rootSources = make(map[MessageID]SourceReadVersion)
	}
	for id, source := range expected {
		if previous, exists := f.rootSources[id]; exists && !extensionCandidateSameSource(previous, source) {
			return ErrSourceUnavailable
		}
		if _, exists := f.rootSources[id]; !exists && len(f.rootSources) == maxResponseSources {
			return ErrLimitExceeded
		}
		if err = f.captureReadSource(source); err != nil {
			return err
		}
		f.rootSources[id] = source
	}
	return nil
}

// Construct a server-owned guard while the original source/body snapshot is
// still open. Filling source versions after returning plaintext would bind a
// newer version to an older body and defeat the final response check.
func (f *readFrame) responseGuard() (ResponseGuard, error) {
	if len(f.roots) > maxRootResponsePins {
		return ResponseGuard{}, ErrLimitExceeded
	}
	identity, err := extensionScopeIdentity(f.scope)
	if err != nil {
		return ResponseGuard{}, err
	}
	generation, err := extensionActiveGeneration(f.ctx, f.tx)
	if err != nil {
		return ResponseGuard{}, err
	}
	guard := ResponseGuard{
		PolicyGeneration: f.snapshot.PolicyGeneration,
		Generation:       generation.ID,
		IndexVersionID:   generation.Version.ID,
		ScopeIdentity:    identity,
		safetyEpoch:      f.epoch,
		indexGeneration:  f.snapshot.IndexGeneration,
		minSent:          f.minSent,
		minExpiry:        f.minExpiry,
		rolling:          f.rolling,
		roots:            extensionSortedRootPins(f.roots),
	}
	for _, source := range f.sources {
		guard.Sources = append(guard.Sources, source)
	}
	sort.Slice(guard.Sources, func(i, j int) bool {
		return guard.Sources[i].Ref.Version.MessageID < guard.Sources[j].Ref.Version.MessageID
	})
	return guard, nil
}

// New permitted originals can change archive membership without revoking a
// returned source. Revalidate the exact source revisions against fresh SQL
// authorization, instead of discarding every read on a global insertion.
func (f *readFrame) verifySources(tx *sql.Tx, now time.Time) error {
	if len(f.pins) == 0 {
		return nil
	}
	where := f.where + " AND (m.expires_at=0 OR m.expires_at>?)"
	args := append(append([]any(nil), f.args...), unixms(now))
	if f.rolling > 0 {
		where += " AND m.original_sent_at>=?"
		args = append(args, ceilMillis(now.Add(-f.rolling)))
	}
	marks := make([]string, 0, len(f.pins))
	for id := range f.pins {
		marks = append(marks, "?")
		args = append(args, id)
	}
	where += " AND m.id IN (" + strings.Join(marks, ",") + ")"
	rows, err := tx.QueryContext(f.ctx, "SELECT m.id,m.local_revision FROM "+authorizedTables+" WHERE "+where, args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id MessageID
		var revision uint64
		if err = rows.Scan(&id, &revision); err != nil {
			return readError(f.ctx, err)
		}
		if f.pins[id] != revision {
			return ErrSourceUnavailable
		}
		seen++
	}
	if err = rows.Err(); err != nil {
		return readError(f.ctx, err)
	}
	if seen != len(f.pins) {
		return ErrSourceUnavailable
	}
	return nil
}
func (f *readFrame) finish() error {
	if err := f.tx.Commit(); err != nil {
		return readError(f.ctx, err)
	}
	f.tx = nil
	if err := f.s.verifyReadSafety(f.scope.Account, f.epoch); err != nil {
		return err
	}
	now := f.s.now()
	verification, err := f.s.readDB.BeginTx(f.ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return readError(f.ctx, err)
	}
	defer verification.Rollback()
	var live ReadSnapshot
	err = verification.QueryRowContext(f.ctx, `SELECT p.generation,a.archive_revision,a.index_generation FROM archive_policy p JOIN archive_meta a ON a.id=1 WHERE p.id=1`).Scan(&live.PolicyGeneration, &live.ArchiveRevision, &live.IndexGeneration)
	if err != nil {
		return readError(f.ctx, err)
	}
	if live.PolicyGeneration != f.snapshot.PolicyGeneration {
		return ErrPolicyChanged
	}
	if live.IndexGeneration != f.snapshot.IndexGeneration {
		return ErrCursorStale
	}
	if err = f.verifySources(verification, now); err != nil {
		return err
	}
	rootWhere := f.where + " AND (m.expires_at=0 OR m.expires_at>?)"
	rootArgs := append(append([]any(nil), f.args...), unixms(now))
	if f.rolling > 0 {
		rootWhere += " AND m.original_sent_at>=?"
		rootArgs = append(rootArgs, ceilMillis(now.Add(-f.rolling)))
	}
	rootVerification := &readFrame{s: f.s, ctx: f.ctx, tx: verification, now: now,
		scope: f.scope, snapshot: live, where: rootWhere, args: rootArgs, rolling: f.rolling}
	if err = rootVerification.extensionCheckRootPins(extensionSortedRootPins(f.roots), f.rootSources); err != nil {
		return err
	}
	if err = verification.Commit(); err != nil {
		return readError(f.ctx, err)
	}
	now = f.s.now()
	if !f.minExpiry.IsZero() && !now.Before(f.minExpiry) {
		return ErrSourceUnavailable
	}
	if f.rolling > 0 && !f.minSent.IsZero() && f.minSent.Before(now.Add(-f.rolling)) {
		return ErrSourceUnavailable
	}
	return f.s.verifyReadSafety(f.scope.Account, f.epoch)
}
func (f *readFrame) expiryHorizon() (time.Time, error) {
	var expiry int64
	// The authorized predicate already removes expired rows. This explicit
	// range provides an expiry-index seek instead of aggregating the whole
	// scope; nonzero preserves pre-epoch injected-clock behavior.
	args := append(append([]any(nil), f.args...), unixms(f.now))
	err := f.tx.QueryRowContext(f.ctx, "SELECT m.expires_at FROM "+authorizedTables+" WHERE "+f.where+" AND m.expires_at<>0 AND m.expires_at>? ORDER BY m.expires_at LIMIT 1", args...).Scan(&expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, readError(f.ctx, err)
	}
	return fromMillis(expiry), nil
}

// SQL authorization always uses the fresh rolling cutoff. Cursor identity
// binds its fixed bounds and the first permitted source's clock horizon, so a
// clock tick alone does not discard pagination; a source crossing does.
func (f *readFrame) cursorHorizon() (time.Time, time.Time, error) {
	expiration, err := f.expiryHorizon()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if f.rolling == 0 {
		return time.Time{}, expiration, nil
	}
	var sent int64
	err = f.tx.QueryRowContext(f.ctx, "SELECT m.original_sent_at FROM "+authorizedTables+" WHERE "+f.where+" ORDER BY m.original_sent_at,m.id LIMIT 1", f.args...).Scan(&sent)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, expiration, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, readError(f.ctx, err)
	}
	return fromMillis(sent).Add(f.rolling), expiration, nil
}

func boundedOriginal(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func contextPriority(page *ContextPage, centerID MessageID) []int {
	if len(page.Messages) == 0 {
		return nil
	}
	center := 0
	for i := range page.Messages {
		if page.Messages[i].ID == centerID {
			center = i
			break
		}
	}
	priority := []int{center}
	for distance := 1; len(priority) < len(page.Messages); distance++ {
		if center-distance >= 0 {
			priority = append(priority, center-distance)
		}
		if center+distance < len(page.Messages) {
			priority = append(priority, center+distance)
		}
	}
	return priority
}

func reserveJSONText(value string, remaining *int) string {
	encoded, _ := json.Marshal(value)
	if len(encoded)-2 <= *remaining {
		*remaining -= len(encoded) - 2
		return value
	}
	runes := []rune(value)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		encoded, _ = json.Marshal(string(runes[:mid]))
		if len(encoded)-2 <= *remaining {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	out := string(runes[:lo])
	encoded, _ = json.Marshal(out)
	*remaining -= len(encoded) - 2
	return out
}

// The ceiling includes JSON escaping and DTO metadata, rather than counting
// raw text bytes (control characters can expand sixfold on the wire).
func boundContext(page *ContextPage, centerID MessageID) error {
	originals := make([][3]string, len(page.Messages))
	generated := make([][]string, len(page.Messages))
	priority := contextPriority(page, centerID)
	for _, i := range priority {
		v := &page.Messages[i]
		originals[i] = [3]string{v.Body, v.Caption, v.ReplyQuote}
		v.Body = ""
		v.Caption = ""
		v.ReplyQuote = ""
		generated[i] = make([]string, len(v.Derived))
		for j := range v.Derived {
			generated[i][j] = v.Derived[j].Text
			v.Derived[j].Text = ""
		}
	}
	metadata, err := json.Marshal(page)
	if err != nil || len(metadata) > maxContextBytes {
		return ErrArchiveUnavailable
	}
	remaining := maxContextBytes - len(metadata)
	for _, i := range priority {
		v := &page.Messages[i]
		v.Body = reserveJSONText(originals[i][0], &remaining)
		v.Caption = reserveJSONText(originals[i][1], &remaining)
		v.ReplyQuote = reserveJSONText(originals[i][2], &remaining)
		for j := range v.Derived {
			v.Derived[j].Text = reserveJSONText(generated[i][j], &remaining)
		}
	}
	return nil
}

type contextRow struct {
	view          MessageView
	expiry        time.Time
	reply         SourceKey
	revision      uint64
	account, hash string
	chatName      string
}

const contextColumns = `m.id,m.chat_id,m.sender_id,i.name,m.original_sent_at,m.body_kind,substr(m.body,1,16384),substr(m.caption,1,16384),m.edited,m.media_availability,m.expires_at,m.reply_account,m.reply_chat,m.reply_sender,m.reply_message,m.reply_from_me,m.local_revision,m.account,m.content_hash,substr(c.name,1,256)`

func scanContext(rows interface{ Scan(...any) error }) (contextRow, error) {
	var r contextRow
	var sent, expiry int64
	err := rows.Scan(&r.view.ID, &r.view.ChatID, &r.view.SenderID, &r.view.SenderName, &sent, &r.view.Kind, &r.view.Body, &r.view.Caption, &r.view.Edited, &r.view.MediaAvailability, &expiry, &r.reply.Account, &r.reply.Chat, &r.reply.Sender, &r.reply.Message, &r.reply.FromMe, &r.revision, &r.account, &r.hash, &r.chatName)
	r.view.SentAt = fromMillis(sent)
	r.expiry = fromMillis(expiry)
	r.view.Body = boundedOriginal(r.view.Body, maxViewFieldBytes)
	r.view.Caption = boundedOriginal(r.view.Caption, maxViewFieldBytes)
	r.view.SenderName = boundedOriginal(r.view.SenderName, 256)
	r.chatName = boundedOriginal(r.chatName, 256)
	return r, err
}
func (f *readFrame) contextRows(extra string, args []any, order string, limit int) ([]contextRow, error) {
	query := "SELECT " + contextColumns + " FROM " + authorizedTables + " WHERE " + f.where + extra + " ORDER BY " + order + " LIMIT ?"
	bind := append(append([]any(nil), f.args...), args...)
	bind = append(bind, limit)
	rows, err := f.tx.QueryContext(f.ctx, query, bind...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	out := []contextRow{}
	for rows.Next() {
		r, err := scanContext(rows)
		if err != nil {
			return nil, readError(f.ctx, err)
		}
		out = append(out, r)
		f.pin(r.view.ID, r.revision)
		source := SourceReadVersion{Ref: SourceRef{Account: r.account, Version: SourceVersion{MessageID: r.view.ID, Revision: r.revision, PolicyGeneration: f.snapshot.PolicyGeneration, ExpiresAt: r.expiry}, ContentHash: r.hash}, ChatID: r.view.ChatID, SenderID: r.view.SenderID, OriginalSentAt: r.view.SentAt, ExpiresAt: r.expiry}
		if err = f.captureSource(source, r.chatName, r.view.SenderName); err != nil {
			return nil, err
		}
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	return out, nil
}

func (f *readFrame) contextDerived(page *ContextPage, centerID MessageID) error {
	remaining := maxContextDerivedPerPage
	for _, i := range contextPriority(page, centerID) {
		if remaining == 0 {
			break
		}
		limit := maxContextDerivedPerMessage
		if remaining < limit {
			limit = remaining
		}
		excerpts, err := f.contextDerivedFor(page.Messages[i].ID, limit)
		if err != nil {
			return err
		}
		page.Messages[i].Derived = excerpts
		remaining -= len(excerpts)
	}
	return nil
}

// Select complete, currently authorized documents before ordering or limits.
// The ordinal-zero source receives the excerpt once; every contributor remains
// part of its source/version/time guard, even outside the context neighborhood.
func (f *readFrame) contextDerivedFor(messageID MessageID, limit int) ([]DerivedExcerpt, error) {
	args := append([]any{messageID, f.scope.Account}, f.args...)
	args = append(args, limit)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT r.id,r.kind,r.source_count
 FROM extension_root_sources anchor JOIN extension_roots r ON r.id=anchor.root_id
 WHERE anchor.message_id=? AND anchor.account=? AND anchor.ordinal=0
 AND `+f.extensionRootPredicate()+` ORDER BY r.id LIMIT ?`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	type generatedRow struct {
		id    DocumentID
		kind  DocumentKind
		count int
	}
	selected := make([]generatedRow, 0, limit)
	members := f.refSourceCopies
	for rows.Next() {
		var row generatedRow
		if err = rows.Scan(&row.id, &row.kind, &row.count); err != nil {
			return nil, readError(f.ctx, err)
		}
		if row.count < 1 || row.count > maxResponseSources || row.count > maxResponseSources-members {
			return nil, ErrLimitExceeded
		}
		members += row.count
		selected = append(selected, row)
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	if err = rows.Close(); err != nil {
		return nil, readError(f.ctx, err)
	}
	var excerpts []DerivedExcerpt
	for _, row := range selected {
		ref, err := f.extensionRootRef(row.id, row.kind)
		if err != nil {
			return nil, err
		}
		if len(ref.ReadSources) != row.count || ref.ReadSources[0].Ref.Account != f.scope.Account || ref.ReadSources[0].Ref.Version.MessageID != messageID {
			return nil, ErrSourceUnavailable
		}
		root, err := f.s.extensionLoadRoot(f.ctx, f.tx, row.id)
		if err != nil {
			return nil, err
		}
		if root.Kind != ref.Kind || !extensionSameRootMembers(root.Sources, ref.Sources) {
			return nil, ErrSourceUnavailable
		}
		excerpts = append(excerpts, DerivedExcerpt{DocumentID: row.id, Source: ref.ReadSources[0].Ref.Version, Text: root.Text, Provenance: root.Provenance})
	}
	return excerpts, nil
}

func (s *Store) Context(ctx context.Context, scope ReadScope, request ContextRequest) (ContextPage, error) {
	return s.contextPage(ctx, scope, request, nil)
}
func (s *Store) ContextGuarded(ctx context.Context, scope ReadScope, request ContextRequest) (ContextPage, ResponseGuard, error) {
	var guard ResponseGuard
	page, err := s.contextPage(ctx, scope, request, &guard)
	if err != nil {
		return ContextPage{}, ResponseGuard{}, err
	}
	return page, guard, nil
}
func (s *Store) contextPage(ctx context.Context, scope ReadScope, request ContextRequest, guardOut *ResponseGuard) (ContextPage, error) {
	if !validReadID(string(request.MessageID)) {
		return ContextPage{}, ErrSourceUnavailable
	}
	if request.Before < 0 || request.Before > maxContextSide || request.After < 0 || request.After > maxContextSide {
		return ContextPage{}, ErrInvalidQuery
	}
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return ContextPage{}, err
	}
	defer f.close()
	f.captureSources = guardOut != nil
	found, err := f.contextRows(" AND m.id=?", []any{request.MessageID}, "m.original_sent_at,m.id", 1)
	if err != nil {
		return ContextPage{}, err
	}
	if len(found) != 1 {
		return ContextPage{}, ErrSourceUnavailable
	}
	center := found[0]
	before, err := f.contextRows(" AND m.chat_id=? AND (m.original_sent_at<? OR (m.original_sent_at=? AND m.id<?))", []any{center.view.ChatID, unixms(center.view.SentAt), unixms(center.view.SentAt), center.view.ID}, "m.original_sent_at DESC,m.id DESC", request.Before)
	if err != nil {
		return ContextPage{}, err
	}
	after, err := f.contextRows(" AND m.chat_id=? AND (m.original_sent_at>? OR (m.original_sent_at=? AND m.id>?))", []any{center.view.ChatID, unixms(center.view.SentAt), unixms(center.view.SentAt), center.view.ID}, "m.original_sent_at,m.id", request.After)
	if err != nil {
		return ContextPage{}, err
	}
	ordered := make([]contextRow, 0, len(before)+1+len(after))
	for i := len(before) - 1; i >= 0; i-- {
		ordered = append(ordered, before[i])
	}
	ordered = append(ordered, center)
	ordered = append(ordered, after...)
	page := ContextPage{Messages: make([]MessageView, 0, len(ordered)), Snapshot: f.snapshot}
	for _, row := range ordered {
		f.note(row.view.SentAt, row.expiry)
		if row.reply.Message != "" {
			targets, err := f.contextRows(" AND m.account=? AND m.source_chat=? AND m.source_sender=? AND m.source_message=? AND m.from_me=?", []any{row.reply.Account, row.reply.Chat, row.reply.Sender, row.reply.Message, row.reply.FromMe}, "m.original_sent_at,m.id", 1)
			if err != nil {
				return ContextPage{}, err
			}
			if len(targets) == 1 {
				target := targets[0]
				row.view.ReplyTo = target.view.ID
				quote := target.view.Body
				if quote == "" {
					quote = target.view.Caption
				}
				row.view.ReplyQuote = boundedOriginal(quote, 1024)
				f.note(target.view.SentAt, target.expiry)
			}
		}
		page.Messages = append(page.Messages, row.view)
	}
	if err = f.contextDerived(&page, request.MessageID); err != nil {
		return ContextPage{}, err
	}
	if err = boundContext(&page, request.MessageID); err != nil {
		return ContextPage{}, err
	}
	if guardOut != nil {
		*guardOut, err = f.responseGuard()
		if err != nil {
			return ContextPage{}, err
		}
	}
	if err = f.finish(); err != nil {
		return ContextPage{}, err
	}
	return page, nil
}

func readLimit(limit int) (int, error) {
	if limit == 0 {
		return 50, nil
	}
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidQuery
	}
	return limit, nil
}
func (f *readFrame) chatViews(limit, offset int) ([]ChatView, error) {
	bind := append(append([]any(nil), f.args...), limit, offset)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT c.id,substr(c.name,1,256),min(m.original_sent_at),max(m.original_sent_at),count(*),EXISTS(SELECT 1 FROM sync_gaps g WHERE g.account=c.account AND g.chat=c.source_chat AND g.code='unresolved_deletion') FROM `+authorizedTables+" WHERE "+f.where+" GROUP BY c.id ORDER BY c.id LIMIT ? OFFSET ?", bind...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	out := []ChatView{}
	for rows.Next() {
		var chat ChatView
		var oldest, latest int64
		var unresolvedDeletion bool
		if err = rows.Scan(&chat.ID, &chat.Name, &oldest, &latest, &chat.MessageCount, &unresolvedDeletion); err != nil {
			return nil, readError(f.ctx, err)
		}
		chat.Name = boundedOriginal(chat.Name, 256)
		chat.Oldest = fromMillis(oldest)
		chat.Latest = fromMillis(latest)
		chat.Coverage = CoverageState("unknown")
		chat.GapCodes = []string{}
		if unresolvedDeletion {
			chat.Coverage = "partial"
			chat.GapCodes = append(chat.GapCodes, "unresolved_deletion")
		}
		out = append(out, chat)
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	return out, nil
}
func (s *Store) ListChats(ctx context.Context, scope ReadScope, request ChatListRequest) (ChatPage, error) {
	return s.chatPage(ctx, scope, request, nil)
}
func (s *Store) ListChatsGuarded(ctx context.Context, scope ReadScope, request ChatListRequest) (ChatPage, ResponseGuard, error) {
	var guard ResponseGuard
	page, err := s.chatPage(ctx, scope, request, &guard)
	if err != nil {
		return ChatPage{}, ResponseGuard{}, err
	}
	return page, guard, nil
}
func (s *Store) chatPage(ctx context.Context, scope ReadScope, request ChatListRequest, guardOut *ResponseGuard) (ChatPage, error) {
	limit, err := readLimit(request.Limit)
	if err != nil {
		return ChatPage{}, err
	}
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return ChatPage{}, err
	}
	defer f.close()
	horizon, expiry, err := f.cursorHorizon()
	if err != nil {
		return ChatPage{}, err
	}
	identity := cursorIdentity("chats", f.scope, struct {
		Limit          int
		RollingHorizon time.Time
	}{limit, horizon}, f.snapshot, f.fixedFrom, f.until, expiry)
	offset := 0
	if request.Cursor != "" {
		offset, err = decodeCursor(s.cursorKey, request.Cursor, identity)
		if err != nil {
			return ChatPage{}, err
		}
	}
	chats, err := f.chatViews(limit+1, offset)
	if err != nil {
		return ChatPage{}, err
	}
	page := ChatPage{Chats: chats, Snapshot: f.snapshot}
	if len(chats) > limit {
		page.Chats = chats[:limit]
		page.NextCursor, err = encodeCursor(s.cursorKey, identity, offset+limit)
		if err != nil {
			return ChatPage{}, err
		}
	}
	f.minExpiry = expiry
	if f.rolling > 0 && !horizon.IsZero() {
		// A chat page's aggregate guard covers the whole permitted scope,
		// including sources beyond the displayed page. The cursor aggregate
		// already computed its oldest source without hydrating every row.
		f.note(horizon.Add(-f.rolling), time.Time{})
	}
	for _, chat := range page.Chats {
		f.note(chat.Oldest, time.Time{})
	}
	if guardOut != nil {
		*guardOut, err = f.responseGuard()
		if err != nil {
			return ChatPage{}, err
		}
	}
	if err = f.finish(); err != nil {
		return ChatPage{}, err
	}
	return page, nil
}
func (s *Store) Status(ctx context.Context, scope ReadScope) (StatusSnapshot, error) {
	return s.statusPage(ctx, scope, nil)
}
func (s *Store) StatusGuarded(ctx context.Context, scope ReadScope) (StatusSnapshot, ResponseGuard, error) {
	var guard ResponseGuard
	page, err := s.statusPage(ctx, scope, &guard)
	if err != nil {
		return StatusSnapshot{}, ResponseGuard{}, err
	}
	return page, guard, nil
}
func (s *Store) statusPage(ctx context.Context, scope ReadScope, guardOut *ResponseGuard) (StatusSnapshot, error) {
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return StatusSnapshot{}, err
	}
	defer f.close()
	chats, err := f.chatViews(100, 0)
	if err != nil {
		return StatusSnapshot{}, err
	}
	status := StatusSnapshot{Connection: ConnectionState("disconnected"), Coverage: CoverageState("unknown"), Chats: chats, Snapshot: f.snapshot, Keyword: Progress{State: ProgressState("ready")}, Semantic: Progress{State: ProgressState("not_configured")}, Media: Progress{State: ProgressState("not_configured")}, Codes: []string{}}
	var minSent sql.NullInt64
	if err = f.tx.QueryRowContext(f.ctx, "SELECT count(*),min(m.original_sent_at) FROM "+authorizedTables+" WHERE "+f.where, f.args...).Scan(&status.Messages, &minSent); err != nil {
		return StatusSnapshot{}, readError(f.ctx, err)
	}
	status.Keyword.Complete = status.Messages
	if minSent.Valid {
		f.note(fromMillis(minSent.Int64), time.Time{})
	}
	// Connection is account-wide operational state. Source metadata and counts
	// remain scoped; no excluded chat names, gaps, or counters enter this DTO.
	var connection string
	err = f.tx.QueryRowContext(f.ctx, "SELECT connection FROM sync_state WHERE account=? AND chat=''", scope.Account).Scan(&connection)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return StatusSnapshot{}, readError(f.ctx, err)
	}
	switch connection {
	case "connected", "pairing", "disconnected", "paused":
		status.Connection = ConnectionState(connection)
		status.Paused = connection == "paused"
	}
	// A fixed account-wide operational gap does not expose per-chat coverage
	// or authorize content. Keep it visible until verified reconciliation.
	var unreadableGap, appStateGap bool
	if err = f.tx.QueryRowContext(f.ctx, `SELECT
 EXISTS(SELECT 1 FROM sync_gaps WHERE account=? AND chat='' AND code='undecryptable_message'),
 EXISTS(SELECT 1 FROM sync_gaps WHERE account=? AND chat='' AND code='app_state_sync_error')`, scope.Account, scope.Account).Scan(&unreadableGap, &appStateGap); err != nil {
		return StatusSnapshot{}, readError(f.ctx, err)
	}
	if unreadableGap {
		status.Coverage = "partial"
		status.Codes = append(status.Codes, "undecryptable_message")
	}
	if appStateGap {
		status.Coverage = "partial"
		status.Codes = append(status.Codes, "app_state_sync_error")
	}
	// The unresolved original has no date. Conservatively report its fixed gap
	// for any requested period in an authorized chat, without inventing a time
	// or exposing the existence of groups outside policy and the server grant.
	unresolvedWhere := `g.account=? AND g.code='unresolved_deletion' AND ap.generation=? AND (ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=c.id))`
	unresolvedArgs := []any{f.scope.Account, f.snapshot.PolicyGeneration}
	if !f.scope.AllChats {
		marks := make([]string, len(f.scope.Chats))
		for i, chat := range f.scope.Chats {
			marks[i] = "?"
			unresolvedArgs = append(unresolvedArgs, chat)
		}
		unresolvedWhere += " AND c.id IN (" + strings.Join(marks, ",") + ")"
	}
	var unresolvedDeletion bool
	if err = f.tx.QueryRowContext(f.ctx, "SELECT EXISTS(SELECT 1 FROM sync_gaps g JOIN chats c ON c.account=g.account AND c.source_chat=g.chat JOIN archive_policy ap ON ap.id=1 WHERE "+unresolvedWhere+")", unresolvedArgs...).Scan(&unresolvedDeletion); err != nil {
		return StatusSnapshot{}, readError(f.ctx, err)
	}
	if unresolvedDeletion {
		status.Coverage = "partial"
		status.Codes = append(status.Codes, "unresolved_deletion")
	}
	// A committed source conflict wipes the affected row. Its fixed gap remains
	// visible only where that row's metadata passes the caller's chat/date scope.
	// This diagnostic predicate does not authorize its invalidated body.
	conflictWhere := strings.Replace(f.where, "m.state='active'", "m.state IN ('active','invalidated')", 1)
	var conflictSent, conflictExpiry sql.NullInt64
	if err = f.tx.QueryRowContext(f.ctx, "SELECT min(m.original_sent_at),min(nullif(m.expires_at,0)) FROM "+authorizedTables+" WHERE "+conflictWhere+" AND m.revision_barrier='source_revision_conflict' AND EXISTS(SELECT 1 FROM sync_gaps g WHERE g.account=m.account AND g.chat=m.source_chat AND g.code='source_revision_conflict')", f.args...).Scan(&conflictSent, &conflictExpiry); err != nil {
		return StatusSnapshot{}, readError(f.ctx, err)
	}
	if conflictSent.Valid {
		status.Coverage = "partial"
		status.Codes = append(status.Codes, "source_revision_conflict")
		f.note(fromMillis(conflictSent.Int64), fromMillis(conflictExpiry.Int64))
	}
	horizon, err := f.expiryHorizon()
	if err != nil {
		return StatusSnapshot{}, err
	}
	f.minExpiry = earlier(f.minExpiry, horizon)
	if guardOut != nil {
		*guardOut, err = f.responseGuard()
		if err != nil {
			return StatusSnapshot{}, err
		}
	}
	if err = f.finish(); err != nil {
		return StatusSnapshot{}, err
	}
	return status, nil
}
