package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Concrete, private commands own their lane and failure classification. No
// exported method accepts transaction callbacks, SQL handles or safety flags.
type extensionSensitivity uint8

const (
	extensionBackground extensionSensitivity = iota
	extensionCredentialMutation
)

type extensionCommand interface {
	run(*Store, context.Context) (any, error)
	priority() bool
	account() string
	sensitivity() extensionSensitivity
}
type credentialMutationCommand interface {
	extensionCommand
	credentialID() CredentialID
}

func (s *Store) submitExtension(ctx context.Context, c extensionCommand, bytes int64) (any, error) {
	if bytes < 0 || bytes > int64(s.options.BodyQueueBytes) {
		return nil, ErrLimitExceeded
	}
	return s.submit(ctx, c, c.account(), c.priority(), bytes)
}
func commandSourceSensitive(payload any, control bool) bool {
	if _, ok := payload.(extensionCommand); ok {
		return false
	}
	if p, ok := payload.(policyCommand); ok {
		return p.policy != nil
	}
	switch payload.(type) {
	case drainCommand, expiryCommand:
		return false
	}
	if _, ok := payload.(eventCommand); ok {
		return true
	}
	return control
}
func (s *Store) extensionCommandFailure(payload any) {
	if c, ok := payload.(credentialMutationCommand); ok {
		_ = s.denyCredentialImmediate(c.credentialID())
	}
}

type extensionCommitSource struct {
	ref   SourceRef
	media bool
}
type extensionCommitGuard struct {
	sources      map[MessageID]extensionCommitSource
	activations  map[IndexGenerationID]bool
	roots        map[DocumentID]extensionRootResponsePin
	rootSources  int
	generations  map[IndexGenerationID]IndexVersion
	deadline     time.Time
	revokesRoots bool
}
type extensionGuardState struct {
	mu           sync.Mutex
	transactions map[*sql.Tx]*extensionCommitGuard
}

func (s *Store) extensionRegisterDeadline(q sqlQuerier, deadline time.Time) {
	tx, ok := q.(*sql.Tx)
	if !ok || deadline.IsZero() {
		return
	}
	s.extensionGuards.mu.Lock()
	defer s.extensionGuards.mu.Unlock()
	if guard := s.extensionGuards.transactions[tx]; guard != nil && (guard.deadline.IsZero() || deadline.Before(guard.deadline)) {
		guard.deadline = deadline
	}
}
func (s *Store) extensionRegisterIndexActivation(tx *sql.Tx, id IndexGenerationID) {
	s.extensionGuards.mu.Lock()
	defer s.extensionGuards.mu.Unlock()
	if guard := s.extensionGuards.transactions[tx]; guard != nil {
		guard.activations[id] = true
	}
}
func (s *Store) extensionRegisterRootRevocation(tx *sql.Tx) {
	s.extensionGuards.mu.Lock()
	defer s.extensionGuards.mu.Unlock()
	if guard := s.extensionGuards.transactions[tx]; guard != nil {
		guard.revokesRoots = true
	}
}
func (s *Store) extensionRegisterRoot(q sqlQuerier, record extensionRootRecord) error {
	tx, ok := q.(*sql.Tx)
	if !ok {
		return nil
	}
	s.extensionGuards.mu.Lock()
	guard := s.extensionGuards.transactions[tx]
	s.extensionGuards.mu.Unlock()
	if guard == nil {
		return nil
	}
	pin, err := extensionRootIdentity(record)
	if err != nil {
		return err
	}
	s.extensionGuards.mu.Lock()
	defer s.extensionGuards.mu.Unlock()
	for _, ref := range record.Origin.Sources {
		old, exists := guard.sources[ref.Version.MessageID]
		if exists && !extensionSameSource(old.ref, ref) {
			return ErrSourceUnavailable
		}
		if !exists && len(guard.sources) >= extensionMaxSources {
			return ErrLimitExceeded
		}
		guard.sources[ref.Version.MessageID] = extensionCommitSource{ref: ref, media: true}
	}
	if old, exists := guard.roots[pin.ID]; exists {
		if old != pin {
			return ErrSourceUnavailable
		}
		return nil
	}
	if len(guard.roots) >= extensionMaxDocuments || guard.rootSources+len(record.Origin.Sources) > extensionMaxSources {
		return ErrLimitExceeded
	}
	guard.roots[pin.ID] = pin
	guard.rootSources += len(record.Origin.Sources)
	return nil
}
func (s *Store) extensionRegisterGeneration(q sqlQuerier, g IndexGeneration) error {
	tx, ok := q.(*sql.Tx)
	if !ok {
		return nil
	}
	s.extensionGuards.mu.Lock()
	defer s.extensionGuards.mu.Unlock()
	guard := s.extensionGuards.transactions[tx]
	if guard == nil {
		return nil
	}
	if !validString(string(g.ID), 256) || !extensionValidVersion(g.Version) || (g.State != "building" && g.State != "active") {
		return ErrInvalidExtension
	}
	if old, exists := guard.generations[g.ID]; exists {
		if old != g.Version {
			return ErrSourceUnavailable
		}
		return nil
	}
	if len(guard.generations) >= extensionMaxDocuments {
		return ErrLimitExceeded
	}
	guard.generations[g.ID] = g.Version
	return nil
}
func (s *Store) registerExtensionSources(q sqlQuerier, refs []SourceRef, media bool) {
	tx, ok := q.(*sql.Tx)
	if !ok {
		return
	}
	s.extensionGuards.mu.Lock()
	defer s.extensionGuards.mu.Unlock()
	guard := s.extensionGuards.transactions[tx]
	if guard == nil {
		return
	}
	for _, ref := range refs {
		old := guard.sources[ref.Version.MessageID]
		guard.sources[ref.Version.MessageID] = extensionCommitSource{ref: ref, media: media || old.media}
	}
}

// This private wrapper owns a short final commit check. Connection admission,
// arbitrary callbacks, and the injected clock run before the commit fence.
func (s *Store) extensionTransaction(ctx context.Context, account string, fn func(*sql.Tx) error) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fixedSQLError(err)
	}
	defer tx.Rollback()
	s.extensionGuards.mu.Lock()
	if s.extensionGuards.transactions == nil {
		s.extensionGuards.transactions = make(map[*sql.Tx]*extensionCommitGuard)
	}
	s.extensionGuards.transactions[tx] = &extensionCommitGuard{sources: make(map[MessageID]extensionCommitSource), activations: make(map[IndexGenerationID]bool), roots: make(map[DocumentID]extensionRootResponsePin), generations: make(map[IndexGenerationID]IndexVersion)}
	s.extensionGuards.mu.Unlock()
	defer func() {
		s.extensionGuards.mu.Lock()
		delete(s.extensionGuards.transactions, tx)
		s.extensionGuards.mu.Unlock()
	}()
	var before, after ArchiveRevision
	var beforePolicy, afterPolicy Generation
	var beforeIndex, afterIndex uint64
	if err = tx.QueryRowContext(ctx, "SELECT archive_revision,COALESCE((SELECT generation FROM archive_policy WHERE id=1),0),index_generation FROM archive_meta WHERE id=1").Scan(&before, &beforePolicy, &beforeIndex); err != nil {
		return fixedSQLError(err)
	}
	if err = fn(tx); err != nil {
		return fixedSQLError(err)
	}
	if err = tx.QueryRowContext(ctx, "SELECT archive_revision,COALESCE((SELECT generation FROM archive_policy WHERE id=1),0),index_generation FROM archive_meta WHERE id=1").Scan(&after, &afterPolicy, &afterIndex); err != nil {
		return fixedSQLError(err)
	}
	s.extensionGuards.mu.Lock()
	var refs []extensionCommitSource
	for _, ref := range s.extensionGuards.transactions[tx].sources {
		refs = append(refs, ref)
	}
	var activations []IndexGenerationID
	for id := range s.extensionGuards.transactions[tx].activations {
		activations = append(activations, id)
	}
	deadline := s.extensionGuards.transactions[tx].deadline
	revokesRoots := s.extensionGuards.transactions[tx].revokesRoots
	roots := make([]extensionRootResponsePin, 0, len(s.extensionGuards.transactions[tx].roots))
	for _, pin := range s.extensionGuards.transactions[tx].roots {
		roots = append(roots, pin)
	}
	type generationPin struct {
		id      IndexGenerationID
		version IndexVersion
	}
	generations := make([]generationPin, 0, len(s.extensionGuards.transactions[tx].generations))
	for id, version := range s.extensionGuards.transactions[tx].generations {
		generations = append(generations, generationPin{id, version})
	}
	s.extensionGuards.mu.Unlock()
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ref.Account == refs[j].ref.Account {
			return refs[i].ref.Version.MessageID < refs[j].ref.Version.MessageID
		}
		return refs[i].ref.Account < refs[j].ref.Account
	})
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	sort.Slice(generations, func(i, j int) bool { return generations[i].id < generations[j].id })
	now := s.now()
	sample := time.Now()
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	s.safetyMu.Lock()
	defer s.safetyMu.Unlock()
	if err = s.extensionSafetyLocked(account); err != nil {
		return err
	}
	var minExpiry, minSent time.Time
	var rolling int64
	if len(refs) > 0 || len(activations) > 0 {
		if err = tx.QueryRowContext(bounded, "SELECT rolling_retention FROM archive_policy WHERE id=1").Scan(&rolling); err != nil {
			return fixedSQLError(err)
		}
	}
	note := func(sent, expiry time.Time) { minSent = earlier(minSent, sent); minExpiry = earlier(minExpiry, expiry) }
	for _, item := range refs {
		current, e := s.extensionCheckSourcesAt(bounded, tx, []SourceRef{item.ref}, item.media, now.Add(time.Since(sample)))
		if e != nil {
			return e
		}
		for _, source := range current {
			note(source.OriginalSentAt, source.ExpiresAt)
		}
	}
	for _, pin := range generations {
		current, e := extensionLoadGeneration(bounded, tx, pin.id)
		if e != nil {
			return e
		}
		if current.Version != pin.version || (current.State != "building" && current.State != "active") {
			return ErrSourceUnavailable
		}
	}
	for _, expected := range roots {
		current, e := s.extensionAuthorizeRootAt(bounded, tx, expected.ID, now.Add(time.Since(sample)))
		if e != nil {
			return e
		}
		pin, e := extensionRootIdentity(current)
		if e != nil {
			return e
		}
		if pin != expected {
			return ErrSourceUnavailable
		}
	}
	for _, id := range activations {
		if err = s.extensionIndexCoverageAt(bounded, tx, id, now.Add(time.Since(sample))); err != nil {
			return err
		}
		var sent, expiry sql.NullInt64
		if err = tx.QueryRowContext(bounded, `SELECT MIN(m.original_sent_at),MIN(NULLIF(m.expires_at,0)) FROM extension_document_sources ds JOIN extension_documents d ON d.id=ds.document_id JOIN messages m ON m.id=ds.message_id WHERE d.generation=?`, id).Scan(&sent, &expiry); err != nil {
			return fixedSQLError(err)
		}
		if sent.Valid {
			note(fromMillis(sent.Int64), time.Time{})
		}
		if expiry.Valid {
			note(time.Time{}, fromMillis(expiry.Int64))
		}
	}
	finalNow := now.Add(time.Since(sample))
	if !deadline.IsZero() && !finalNow.Before(deadline) {
		return ErrNoWork
	}
	if (!minExpiry.IsZero() && !finalNow.Before(minExpiry)) || (rolling > 0 && !minSent.IsZero() && minSent.Before(finalNow.Add(-time.Duration(rolling)))) {
		return ErrSourceUnavailable
	}
	if err = bounded.Err(); err != nil {
		return err
	}
	err = tx.Commit()
	if err == nil && (beforeIndex != afterIndex || beforePolicy != afterPolicy || revokesRoots) {
		s.safetyEpoch++
	}
	_ = before
	_ = after
	return fixedSQLError(err)
}
func (s *Store) extensionSafetyLocked(account string) error {
	if s.unclean {
		return ErrReconciliationRequired
	}
	if account != "" {
		if s.unsafe[account] {
			return ErrReconciliationRequired
		}
		return nil
	}
	for _, unsafe := range s.unsafe {
		if unsafe {
			return ErrReconciliationRequired
		}
	}
	return nil
}

func (s *Store) CurrentSource(ctx context.Context, key SourceKey) (SourceRef, error) {
	if !validKey(key) {
		return SourceRef{}, ErrInvalidExtension
	}
	epoch, err := s.readSafety(key.Account)
	if err != nil {
		return SourceRef{}, err
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	tx, err := s.readDB.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SourceRef{}, fixedSQLError(err)
	}
	defer tx.Rollback()
	key, err = canonicalKey(bounded, tx, key)
	if err != nil {
		return SourceRef{}, fixedSQLError(err)
	}
	var id MessageID
	err = tx.QueryRowContext(bounded, "SELECT id FROM messages WHERE account=? AND source_chat=? AND source_sender=? AND source_message=? AND from_me=?", key.Account, key.Chat, key.Sender, key.Message, key.FromMe).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceRef{}, ErrSourceUnavailable
	}
	if err != nil {
		return SourceRef{}, fixedSQLError(err)
	}
	current, err := s.extensionLookupSourceAt(bounded, tx, key.Account, id, false, s.now())
	if err != nil {
		return SourceRef{}, err
	}
	if err = tx.Commit(); err != nil {
		return SourceRef{}, fixedSQLError(err)
	}
	if err = s.verifyReadSafety(key.Account, epoch); err != nil {
		return SourceRef{}, err
	}
	if !current.ExpiresAt.IsZero() && !s.now().Before(current.ExpiresAt) {
		return SourceRef{}, ErrSourceUnavailable
	}
	return current.Ref, nil
}
func (s *Store) extensionLookupSourceAt(ctx context.Context, q sqlQuerier, account string, id MessageID, media bool, now time.Time) (SourceReadVersion, error) {
	var current SourceReadVersion
	var state, hash string
	var known, viewOnce bool
	var revision uint64
	var generation Generation
	var sent, expiry, rolling, importStart int64
	var mode DateMode
	var included bool
	var unsafe int
	var chatName, senderName string
	err := q.QueryRowContext(ctx, `SELECT m.local_revision,m.content_hash,m.state,m.original_date_known,m.chat_id,m.sender_id,m.original_sent_at,m.expires_at,m.view_once,
 ap.generation,ap.date_mode,ap.import_start,ap.rolling_retention,(ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id)),ac.unsafe,
 CASE WHEN length(CAST(c.name AS BLOB))<=256 THEN c.name ELSE NULL END,CASE WHEN length(CAST(i.name AS BLOB))<=256 THEN i.name ELSE NULL END
 FROM messages m JOIN archive_policy ap ON ap.id=1 JOIN account_safety ac ON ac.account=m.account JOIN chats c ON c.id=m.chat_id JOIN identities i ON i.id=m.sender_id WHERE m.account=? AND m.id=?
 AND length(CAST(m.content_hash AS BLOB))=64 AND length(CAST(m.chat_id AS BLOB))<=256 AND length(CAST(m.sender_id AS BLOB))<=256 AND length(CAST(m.state AS BLOB))<=32`, account, id).Scan(&revision, &hash, &state, &known, &current.ChatID, &current.SenderID, &sent, &expiry, &viewOnce, &generation, &mode, &importStart, &rolling, &included, &unsafe, &chatName, &senderName)
	if errors.Is(err, sql.ErrNoRows) {
		return current, ErrSourceUnavailable
	}
	if err != nil {
		return current, fixedSQLError(err)
	}
	if unsafe != 0 {
		return current, ErrReconciliationRequired
	}
	if generation == 0 || state != "active" || !known || !included || len(hash) != 64 || (media && viewOnce) || (expiry != 0 && expiry <= unixms(now)) || (mode == DateSince && sent < importStart) || (rolling > 0 && sent < ceilMillis(now.Add(-time.Duration(rolling)))) {
		return current, ErrSourceUnavailable
	}
	current.OriginalSentAt = fromMillis(sent)
	current.ExpiresAt = fromMillis(expiry)
	current.MetadataHash, err = sourceMetadataHash(chatName, senderName)
	if err != nil {
		return SourceReadVersion{}, err
	}
	current.Ref = SourceRef{Account: account, Version: SourceVersion{MessageID: id, Revision: revision, PolicyGeneration: generation, ExpiresAt: current.ExpiresAt}, ContentHash: hash}
	return current, nil
}

// Display labels are versioned and length-delimited independently of the
// unchanged v1 source hash. Empty authoritative labels remain valid pins.
func sourceMetadataHash(chatName, senderName string) ([32]byte, error) {
	for _, label := range []string{chatName, senderName} {
		if len(label) > 256 || !utf8.ValidString(label) || strings.IndexByte(label, 0) >= 0 {
			return [32]byte{}, ErrSourceUnavailable
		}
	}
	h := sha256.New()
	_, _ = h.Write([]byte("wa-indexer-display-metadata/v1\x00"))
	var length [4]byte
	for _, label := range []string{chatName, senderName} {
		binary.BigEndian.PutUint32(length[:], uint32(len(label)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(label))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// Media references are opaque binary, so the unchanged v1 delimiter hash is
// insufficient to identify the ordered set. Persist only this bounded digest,
// including for view-once entries whose protocol bytes must never enter SQL.
func sourceMediaIdentity(refs []MediaReference) (string, error) {
	if len(refs) > 8 {
		return "", ErrInvalidEvent
	}
	for _, ref := range refs {
		if len(ref.Kind) > 64 || len(ref.MimeType) > 256 || !utf8.ValidString(ref.MimeType) || len(ref.ProtocolReference) > maxProtocolReferenceBytes {
			return "", ErrInvalidEvent
		}
	}
	h := sha256.New()
	_, _ = h.Write([]byte("wa-indexer-source-media/v1\x00"))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(refs)))
	_, _ = h.Write(length[:])
	for _, ref := range refs {
		for _, field := range [][]byte{[]byte(ref.Kind), []byte(ref.MimeType), ref.ProtocolReference} {
			binary.BigEndian.PutUint32(length[:], uint32(len(field)))
			_, _ = h.Write(length[:])
			_, _ = h.Write(field)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Store) extensionCheckSources(ctx context.Context, q sqlQuerier, refs []SourceRef, media bool) ([]SourceReadVersion, error) {
	result, err := s.extensionCheckSourcesAt(ctx, q, refs, media, s.now())
	if err == nil {
		s.registerExtensionSources(q, refs, media)
	}
	return result, err
}
func (s *Store) extensionCheckSourcesAt(ctx context.Context, q sqlQuerier, refs []SourceRef, media bool, now time.Time) ([]SourceReadVersion, error) {
	if len(refs) == 0 {
		return nil, ErrInvalidExtension
	}
	if len(refs) > 2048 {
		return nil, ErrLimitExceeded
	}
	seen := make(map[MessageID]SourceRef, len(refs))
	result := make([]SourceReadVersion, 0, len(refs))
	for _, ref := range refs {
		if !validString(ref.Account, 512) || !validString(string(ref.Version.MessageID), 128) || ref.Version.Revision == 0 || ref.Version.PolicyGeneration == 0 || ref.Version.Deleted || ref.Version.Excluded || len(ref.ContentHash) != 64 {
			return nil, ErrSourceUnavailable
		}
		if old, ok := seen[ref.Version.MessageID]; ok {
			if old != ref {
				return nil, ErrSourceUnavailable
			}
			continue
		}
		current, err := s.extensionLookupSourceAt(ctx, q, ref.Account, ref.Version.MessageID, media, now)
		if err != nil {
			return nil, err
		}
		if current.Ref != ref {
			return nil, ErrSourceUnavailable
		}
		seen[ref.Version.MessageID] = ref
		result = append(result, current)
	}
	return result, nil
}

// CanonicalWorkDigest is separate from messages.content_hash (the unchanged
// v1 hashEvent interpretation). Fixed struct fields avoid delimiter ambiguity.
func CanonicalWorkDigest(g IndexGenerationID, v IndexVersion, kind JobKind, media MediaID, sources []SourceRef, documents []Document) (string, error) {
	if len(sources) == 0 || len(sources) > 2048 || len(documents) > 256 {
		return "", ErrLimitExceeded
	}
	if !validString(string(g), 256) || !extensionValidVersion(v) || !extensionValidJobKind(kind) || !extensionOptionalString(string(media), 128) {
		return "", ErrInvalidExtension
	}
	if _, err := extensionDocumentBounds(documents, extensionMaxInputBytes); err != nil {
		return "", err
	}
	validSource := func(ref SourceRef) bool {
		if !extensionValidSourceMetadata(ref) || ref.Version.Revision == 0 || ref.Version.PolicyGeneration == 0 || ref.Version.Deleted || ref.Version.Excluded || (!ref.Version.ExpiresAt.IsZero() && (unixms(ref.Version.ExpiresAt) == 0 || ref.Version.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0)) {
			return false
		}
		decoded, err := hex.DecodeString(ref.ContentHash)
		return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == ref.ContentHash
	}
	allSources := make(map[MessageID]SourceRef, len(sources))
	for _, ref := range sources {
		if !validSource(ref) {
			return "", ErrInvalidExtension
		}
		if _, duplicate := allSources[ref.Version.MessageID]; duplicate {
			return "", ErrInvalidExtension
		}
		allSources[ref.Version.MessageID] = ref
	}
	for _, document := range documents {
		input := kind == JobDerivedEmbedding && document.PassageTokens == 0 && (document.Kind == DocumentOCR || document.Kind == DocumentCaption || document.Kind == DocumentTranscript)
		if (!input && document.PassageTokens < 1) || document.PassageTokens > v.MaxPassageTokens {
			return "", ErrInvalidExtension
		}
		members := make(map[MessageID]SourceRef, len(document.Sources))
		for _, ref := range document.Sources {
			if !validSource(ref) || !extensionSameSource(allSources[ref.Version.MessageID], ref) {
				return "", ErrInvalidExtension
			}
			if _, duplicate := members[ref.Version.MessageID]; duplicate {
				return "", ErrInvalidExtension
			}
			members[ref.Version.MessageID] = ref
		}
		for _, span := range document.Spans {
			if !extensionSameSource(members[span.Source.Version.MessageID], span.Source) || span.StartByte < 0 || span.EndByte <= span.StartByte || span.EndByte > maxEventTextBytes {
				return "", ErrInvalidExtension
			}
		}
	}
	type source struct {
		Account, MessageID         string
		Revision, PolicyGeneration uint64
		ExpiresAt                  int64
		ContentHash                string
	}
	type span struct {
		Source             source
		StartByte, EndByte int
		Field              string
	}
	type doc struct {
		ID            DocumentID
		Kind          DocumentKind
		TextSHA256    string
		Sources       []source
		Spans         []span
		Provenance    DerivedProvenance
		PassageTokens int
	}
	type digest struct {
		Schema     string
		Generation IndexGenerationID
		Version    IndexVersion
		Kind       JobKind
		MediaID    MediaID
		Sources    []source
		Documents  []doc
	}
	convert := func(ref SourceRef) source {
		return source{ref.Account, string(ref.Version.MessageID), ref.Version.Revision, uint64(ref.Version.PolicyGeneration), unixms(ref.Version.ExpiresAt), ref.ContentHash}
	}
	canonical := func(refs []SourceRef) []source {
		out := make([]source, len(refs))
		for i, ref := range refs {
			out[i] = convert(ref)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Account == out[j].Account {
				return out[i].MessageID < out[j].MessageID
			}
			return out[i].Account < out[j].Account
		})
		return out
	}
	payload := digest{Schema: "wa-indexer-work-digest-v1", Generation: g, Version: v, Kind: kind, MediaID: media, Sources: canonical(sources), Documents: make([]doc, len(documents))}
	bytes := 0
	for i, d := range documents {
		bytes += len(d.Text)
		if bytes > 512<<10 {
			return "", ErrLimitExceeded
		}
		hash := sha256.Sum256([]byte(d.Text))
		entry := doc{ID: d.ID, Kind: d.Kind, TextSHA256: hex.EncodeToString(hash[:]), Sources: canonical(d.Sources), Spans: make([]span, len(d.Spans)), Provenance: d.Provenance, PassageTokens: d.PassageTokens}
		for j, s := range d.Spans {
			entry.Spans[j] = span{convert(s.Source), s.StartByte, s.EndByte, s.Field}
		}
		payload.Documents[i] = entry
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", ErrInvalidExtension
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalOriginWorkDigest binds metadata for the whole authoritative root.
// Batch text and offsets cannot replace this immutable job identity.
func CanonicalOriginWorkDigest(g IndexGenerationID, v IndexVersion, origin DerivedOrigin) (string, error) {
	if len(origin.Sources) == 0 || len(origin.Sources) > extensionMaxSources || origin.Bytes > 64<<10 {
		return "", ErrLimitExceeded
	}
	if !validString(string(g), 256) || !extensionValidVersion(v) || !validString(string(origin.ID), 256) || origin.Bytes < 1 || len(origin.TextSHA256) != 64 {
		return "", ErrInvalidExtension
	}
	hash, err := hex.DecodeString(origin.TextSHA256)
	if err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != origin.TextSHA256 {
		return "", ErrInvalidExtension
	}
	p := origin.Provenance
	if !validString(string(p.MediaID), 128) || !validString(p.ProfileID, 128) || !validString(p.ExtractorVersion, 128) || !extensionOptionalString(p.PromptVersion, 128) || p.StartMS < 0 || p.EndMS < p.StartMS || p.EndMS > 24*60*60*1000 {
		return "", ErrInvalidExtension
	}
	switch p.Kind {
	case DocumentOCR, DocumentCaption:
		if p.OffsetKind != "none" || p.StartMS != 0 || p.EndMS != 0 {
			return "", ErrInvalidExtension
		}
	case DocumentTranscript:
		if p.OffsetKind != "slice" && p.OffsetKind != "word" && (p.OffsetKind != "none" || p.StartMS != 0 || p.EndMS != 0) {
			return "", ErrInvalidExtension
		}
	default:
		return "", ErrInvalidExtension
	}
	type source struct {
		Account, MessageID         string
		Revision, PolicyGeneration uint64
		ExpiresAt                  int64
		ContentHash                string
	}
	refs := make([]source, len(origin.Sources))
	seen := make(map[MessageID]bool, len(origin.Sources))
	for i, ref := range origin.Sources {
		if !extensionValidSourceMetadata(ref) || ref.Version.Revision == 0 || ref.Version.PolicyGeneration == 0 || ref.Version.Deleted || ref.Version.Excluded || seen[ref.Version.MessageID] || (!ref.Version.ExpiresAt.IsZero() && (unixms(ref.Version.ExpiresAt) == 0 || ref.Version.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0)) {
			return "", ErrInvalidExtension
		}
		decoded, e := hex.DecodeString(ref.ContentHash)
		if e != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != ref.ContentHash {
			return "", ErrInvalidExtension
		}
		seen[ref.Version.MessageID] = true
		refs[i] = source{ref.Account, string(ref.Version.MessageID), ref.Version.Revision, uint64(ref.Version.PolicyGeneration), unixms(ref.Version.ExpiresAt), ref.ContentHash}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Account == refs[j].Account {
			return refs[i].MessageID < refs[j].MessageID
		}
		return refs[i].Account < refs[j].Account
	})
	payload := struct {
		Schema     string
		Generation IndexGenerationID
		Version    IndexVersion
		Kind       JobKind
		OriginID   DocumentID
		TextSHA256 string
		Bytes      int
		Provenance DerivedProvenance
		Sources    []source
	}{"wa-indexer-origin-work-digest-v1", g, v, JobDerivedEmbedding, origin.ID, origin.TextSHA256, origin.Bytes, p, refs}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", ErrInvalidExtension
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// Atomic source invalidation removes all dependent generated content and
// revokes obsolete leases. Artifact accounting survives until file cleanup.
func extensionInvalidate(ctx context.Context, tx *sql.Tx, id MessageID, revision ArchiveRevision) error {
	if err := extensionInvalidateOriginalDownloads(ctx, tx, id); err != nil {
		return err
	}
	// A boundary window can contain sources beyond the edited message's
	// neighboring dirty range or behind its cursor. Retain the whole old extent
	// using metadata before deleting membership; no original bodies enter Go.
	_, err := tx.ExecContext(ctx, `INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision)
 SELECT lower(hex(randomblob(16))),m.account,m.chat_id,g.id,ap.generation,MIN(m.original_sent_at),MAX(m.original_sent_at)+1,CASE WHEN ?=0 THEN am.archive_revision+1 ELSE ? END
 FROM extension_document_sources changed JOIN extension_documents d ON d.id=changed.document_id
 JOIN extension_document_sources members ON members.document_id=d.id JOIN messages m ON m.id=members.message_id
 CROSS JOIN extension_generations g JOIN archive_policy ap ON ap.id=1 JOIN archive_meta am ON am.id=1
 WHERE changed.message_id=? AND d.kind='text_window' AND g.state IN ('building','active') GROUP BY m.account,m.chat_id,g.id
 ON CONFLICT(chat_id,generation) DO UPDATE SET policy_generation=excluded.policy_generation,from_time=MIN(from_time,excluded.from_time),until_time=MAX(until_time,excluded.until_time),cursor_time=0,cursor_id='',dirty_revision=MAX(dirty_revision,excluded.dirty_revision)`, revision, revision, id)
	if err != nil {
		return err
	}
	statements := []string{
		`DELETE FROM extension_window_coverage WHERE message_id=? OR message_id IN (SELECT members.message_id FROM extension_document_sources changed JOIN extension_documents d ON d.id=changed.document_id JOIN extension_document_sources members ON members.document_id=d.id WHERE changed.message_id=? AND d.kind='text_window')`,
		`DELETE FROM extension_publications WHERE (kind,work_id) IN (SELECT kind,work_id FROM extension_publication_sources WHERE message_id=?)`,
		`UPDATE extension_extraction_jobs SET state='cancelled',last_code='source_invalidated',outcome='',completion_digest='' WHERE message_id=?`,
		`UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='source_invalidated' WHERE id IN (SELECT jo.job_id FROM extension_job_origins jo JOIN extension_root_sources rs ON rs.root_id=jo.root_id WHERE rs.message_id=?)`,
		`DELETE FROM extension_document_fts WHERE rowid IN (SELECT d.rowid FROM extension_documents d JOIN extension_root_sources rs ON rs.root_id=d.origin_document_id WHERE rs.message_id=?)`,
		`DELETE FROM extension_documents WHERE origin_document_id IN (SELECT root_id FROM extension_root_sources WHERE message_id=?)`,
		`DELETE FROM extension_root_fts WHERE rowid IN (SELECT r.rowid FROM extension_roots r JOIN extension_root_sources rs ON rs.root_id=r.id WHERE rs.message_id=?)`,
		`DELETE FROM extension_roots WHERE id IN (SELECT root_id FROM extension_root_sources WHERE message_id=?)`,
		`UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='source_invalidated' WHERE id IN (SELECT job_id FROM extension_job_sources WHERE message_id=?)`,
		`DELETE FROM extension_document_fts WHERE rowid IN (SELECT d.rowid FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id WHERE ds.message_id=?)`,
		`DELETE FROM extension_documents WHERE id IN (SELECT document_id FROM extension_document_sources WHERE message_id=?)`,
		`UPDATE extension_dirty SET lease_id='',lease_until=0,claim_revision=0,claim_from=0,claim_until=0,claim_cursor_time=0,claim_cursor_id='',cursor_time=0,cursor_id='' WHERE id IN (SELECT dirty_id FROM extension_dirty_sources WHERE message_id=?)`,
		`DELETE FROM extension_dirty_sources WHERE message_id=?`,
		`UPDATE extension_media_associations SET active=0,protocol_reference=X'',artifact_id='' WHERE message_id=?`,
		`UPDATE extension_media_artifacts SET state='evicting' WHERE cleanup_ack=0 AND media_id IN (SELECT id FROM extension_media_associations WHERE message_id=?) AND NOT EXISTS(SELECT 1 FROM extension_media_associations a WHERE a.artifact_id=extension_media_artifacts.id AND a.active=1)`,
	}
	for i, stmt := range statements {
		args := []any{id}
		if i == 0 {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

// Verified bytes changing under one media association revoke its old generated
// authority without discarding the original source text or physical input pins.
func (s *Store) extensionInvalidateMedia(ctx context.Context, tx *sql.Tx, media MediaID) error {
	s.extensionRegisterRootRevocation(tx)
	statements := []string{
		`DELETE FROM extension_publications WHERE kind='derived' AND work_id IN (SELECT j.id FROM extension_jobs j LEFT JOIN extension_job_origins jo ON jo.job_id=j.id LEFT JOIN extension_roots r ON r.id=jo.root_id WHERE j.media_id=? OR r.media_id=? OR j.id IN (SELECT jd.job_id FROM extension_job_documents jd JOIN extension_documents d ON d.id=jd.document_id WHERE d.kind<>'text_window' AND CASE WHEN json_valid(d.provenance_json) THEN json_extract(d.provenance_json,'$.MediaID') ELSE '' END=?))`,
		`UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='media_invalidated' WHERE media_id=? OR id IN (SELECT jo.job_id FROM extension_job_origins jo JOIN extension_roots r ON r.id=jo.root_id WHERE r.media_id=?) OR id IN (SELECT jd.job_id FROM extension_job_documents jd JOIN extension_documents d ON d.id=jd.document_id WHERE d.kind<>'text_window' AND CASE WHEN json_valid(d.provenance_json) THEN json_extract(d.provenance_json,'$.MediaID') ELSE '' END=?)`,
		`DELETE FROM extension_document_fts WHERE rowid IN (SELECT rowid FROM extension_documents WHERE kind<>'text_window' AND CASE WHEN json_valid(provenance_json) THEN json_extract(provenance_json,'$.MediaID') ELSE '' END=?)`,
		`DELETE FROM extension_documents WHERE kind<>'text_window' AND CASE WHEN json_valid(provenance_json) THEN json_extract(provenance_json,'$.MediaID') ELSE '' END=?`,
		`DELETE FROM extension_document_fts WHERE rowid IN (SELECT d.rowid FROM extension_documents d JOIN extension_roots r ON r.id=d.origin_document_id WHERE r.media_id=?)`,
		`DELETE FROM extension_documents WHERE origin_document_id IN (SELECT id FROM extension_roots WHERE media_id=?)`,
		`DELETE FROM extension_root_fts WHERE rowid IN (SELECT rowid FROM extension_roots WHERE media_id=?)`,
		`DELETE FROM extension_roots WHERE media_id=?`,
		`UPDATE extension_extraction_jobs SET state='cancelled',last_code='media_invalidated',outcome='',completion_digest='' WHERE media_id=?`,
	}
	for i, stmt := range statements {
		args := []any{media}
		if i < 2 {
			args = append(args, media, media)
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

func extensionInvalidateAll(ctx context.Context, tx *sql.Tx, account string) error {
	if err := extensionInvalidateOriginalDownloadsAll(ctx, tx, account); err != nil {
		return err
	}
	statements := []string{
		`DELETE FROM extension_window_coverage WHERE (?='' OR account=?)`,
		`DELETE FROM extension_publications WHERE (?='' OR account=?)`,
		`UPDATE extension_extraction_jobs SET state='cancelled',last_code='source_invalidated',outcome='',completion_digest='' WHERE (?='' OR account=?)`,
		`UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='source_invalidated' WHERE id IN (SELECT jo.job_id FROM extension_job_origins jo JOIN extension_root_sources rs ON rs.root_id=jo.root_id WHERE (?='' OR rs.account=?))`,
		`DELETE FROM extension_document_fts WHERE rowid IN (SELECT d.rowid FROM extension_documents d JOIN extension_root_sources rs ON rs.root_id=d.origin_document_id WHERE (?='' OR rs.account=?))`,
		`DELETE FROM extension_documents WHERE origin_document_id IN (SELECT root_id FROM extension_root_sources WHERE (?='' OR account=?))`,
		`DELETE FROM extension_root_fts WHERE rowid IN (SELECT r.rowid FROM extension_roots r JOIN extension_root_sources rs ON rs.root_id=r.id WHERE (?='' OR rs.account=?))`,
		`DELETE FROM extension_roots WHERE id IN (SELECT root_id FROM extension_root_sources WHERE (?='' OR account=?))`,
		`UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='source_invalidated' WHERE (?='' OR account=?)`,
		`DELETE FROM extension_document_fts WHERE rowid IN (SELECT d.rowid FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id WHERE (?='' OR ds.account=?))`,
		`DELETE FROM extension_documents WHERE id IN (SELECT document_id FROM extension_document_sources WHERE (?='' OR account=?))`,
		`DELETE FROM extension_dirty WHERE (?='' OR account=?)`,
		`UPDATE extension_media_associations SET active=0,protocol_reference=X'',artifact_id='' WHERE (?='' OR account=?)`,
		`UPDATE extension_media_artifacts SET state='evicting' WHERE cleanup_ack=0 AND (?='' OR account=?) AND NOT EXISTS(SELECT 1 FROM extension_media_associations a WHERE a.artifact_id=extension_media_artifacts.id AND a.active=1)`,
	}
	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt, account, account); err != nil {
			return err
		}
	}
	return nil
}
func extensionDirty(ctx context.Context, tx *sql.Tx, chat string, policy Generation, sent int64, revision ArchiveRevision) error {
	// Include neighboring boundaries when an edit changes passage token length.
	from, until := sent-int64((5*time.Minute)/time.Millisecond), sent+int64((5*time.Minute)/time.Millisecond)+1
	_, err := tx.ExecContext(ctx, `INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision)
 SELECT lower(hex(randomblob(16))),c.account,c.id,g.id,?,?,?,? FROM chats c CROSS JOIN extension_generations g WHERE c.id=? AND g.state IN ('building','active')
 ON CONFLICT(chat_id,generation) DO UPDATE SET policy_generation=excluded.policy_generation,from_time=MIN(from_time,excluded.from_time),until_time=MAX(until_time,excluded.until_time),cursor_time=CASE WHEN dirty_revision<>excluded.dirty_revision THEN 0 ELSE cursor_time END,cursor_id=CASE WHEN dirty_revision<>excluded.dirty_revision THEN '' ELSE cursor_id END,dirty_revision=MAX(dirty_revision,excluded.dirty_revision)`, policy, from, until, revision, chat)
	return err
}
func extensionReconstructDirty(ctx context.Context, tx *sql.Tx, policy Generation, chat string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision)
 SELECT lower(hex(randomblob(16))),c.account,w.chat_id,g.id,?,w.from_time,w.until_time,w.archive_revision FROM dirty_windows w JOIN chats c ON c.id=w.chat_id CROSS JOIN extension_generations g WHERE w.generation=? AND (?='' OR w.chat_id=?) AND g.state IN ('building','active')
 ON CONFLICT(chat_id,generation) DO UPDATE SET policy_generation=excluded.policy_generation,from_time=MIN(from_time,excluded.from_time),until_time=MAX(until_time,excluded.until_time),cursor_time=CASE WHEN dirty_revision<>excluded.dirty_revision THEN 0 ELSE cursor_time END,cursor_id=CASE WHEN dirty_revision<>excluded.dirty_revision THEN '' ELSE cursor_id END,dirty_revision=MAX(dirty_revision,excluded.dirty_revision)`, policy, policy, chat, chat)
	return err
}
