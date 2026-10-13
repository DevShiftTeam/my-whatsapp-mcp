package archive

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
	"unsafe"
)

type savedSession struct {
	reader        *savedOwnerRead
	live, history *readFrame
	cancel        context.CancelFunc
	pins          []savedHistoricalPin
}

func (r *savedOwnerRead) begin(ctx context.Context, chats []ChatID, from, until time.Time) (*savedSession, error) {
	ctx, cancel := context.WithDeadline(ctx, r.deadline)
	if r.bridge != nil {
		if err := r.bridge.checkLive(ctx, r.bridge.original); err != nil {
			cancel()
			return nil, err
		}
	}
	lf, err := r.live.beginRead(ctx, r.scope, chats, from, until)
	if err != nil {
		cancel()
		return nil, err
	}
	lf.captureSources = true
	lf.savedEvidenceChats = append([]ChatID(nil), chats...)
	s := &savedSession{reader: r, live: lf, cancel: cancel}
	if r.bridge != nil {
		var translated []ChatID
		if len(chats) > 0 {
			evidence, e := savedEvidenceFor(lf)
			if e != nil {
				s.close()
				return nil, e
			}
			wanted := map[string]bool{}
			for _, id := range chats {
				for source, currentID := range evidence.chats {
					if currentID == id {
						wanted[source] = true
					}
				}
			}
			for _, hid := range r.bridge.translated.Chats {
				var source string
				if e = r.history.snapshot.readDB.QueryRowContext(ctx, "SELECT source_chat FROM chats WHERE id=?", hid).Scan(&source); e != nil {
					s.close()
					return nil, readError(ctx, e)
				}
				key, e := evidence.canonical(SourceKey{Account: r.scope.Account, Chat: source})
				if e != nil {
					s.close()
					return nil, e
				}
				if wanted[key.Chat] {
					translated = append(translated, hid)
				}
			}
			if len(translated) == 0 {
				return s, nil
			}
		}
		hf, e := r.history.snapshot.beginRead(ctx, r.bridge.translated, translated, from, until)
		if e != nil {
			s.close()
			return nil, e
		}
		hf.captureSources = true
		hf.savedEvidenceChats = append([]ChatID(nil), translated...)
		s.history = hf
	}
	return s, nil
}
func (s *savedSession) close() {
	if s.history != nil {
		s.history.close()
	}
	if s.live != nil {
		s.live.close()
	}
	s.cancel()
}
func (s *savedSession) identity(kind string, request any) string {
	var historical ReadSnapshot
	var tag string
	if s.history != nil {
		historical = s.history.snapshot
		tag = s.reader.history.meta.SnapshotID
	}
	return cursorIdentity("saved/"+kind, s.reader.scope, struct {
		Request                          any
		History                          ReadSnapshot
		Tag                              string
		LiveCoverage, HistoricalCoverage uint64
	}{request, historical, tag, s.live.snapshot.CoverageEpoch, historical.CoverageEpoch}, s.live.snapshot, s.live.fixedFrom, s.live.until, time.Time{})
}
func (s *savedSession) guard() (SavedResponseGuard, error) {
	g := SavedResponseGuard{owner: s.reader, historicalPins: append([]savedHistoricalPin(nil), s.pins...)}
	live, err := s.live.responseGuard()
	if err != nil {
		return g, err
	}
	g.live = []ResponseGuard{live}
	if s.history != nil {
		h, e := s.history.responseGuard()
		if e != nil {
			return g, e
		}
		g.historical = []ResponseGuard{h}
	}
	return g, nil
}
func (s *savedSession) accept(m savedMetadata, equivalent bool) (savedHistoricalPin, bool, error) {
	p, ok, err := s.reader.eligible(s.live, m, equivalent)
	if err != nil || !ok {
		return p, ok, err
	}
	if len(s.pins) >= maxResponseSources {
		return p, false, ErrSavedReadLimit
	}
	s.pins = append(s.pins, p)
	s.live.note(m.Sent, p.expiry)
	return p, true, nil
}
func (s *savedSession) historicalHit(selected searchSelection) (SearchHit, bool, error) {
	var metas []savedMetadata
	if selected.root {
		rows, err := s.history.tx.QueryContext(s.history.ctx, "SELECT message_id FROM extension_root_sources WHERE root_id=? ORDER BY ordinal LIMIT ?", selected.id, maxResponseSources+1)
		if err != nil {
			return SearchHit{}, false, readError(s.history.ctx, err)
		}
		var ids []MessageID
		for rows.Next() {
			var id MessageID
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return SearchHit{}, false, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return SearchHit{}, false, err
		}
		if len(ids) != selected.count || len(ids) > maxResponseSources {
			return SearchHit{}, false, ErrSavedReadLimit
		}
		for _, id := range ids {
			m, e := savedMetadataByID(s.history, id)
			if e != nil {
				return SearchHit{}, false, e
			}
			metas = append(metas, m)
		}
	} else {
		m, e := savedMetadataByID(s.history, MessageID(selected.id))
		if e != nil {
			return SearchHit{}, false, e
		}
		metas = []savedMetadata{m}
	}
	var pins []savedHistoricalPin
	for _, m := range metas {
		if err := s.historicalUnique(m); err != nil {
			return SearchHit{}, false, err
		}
		p, ok, e := s.reader.eligible(s.live, m, selected.root)
		if e != nil || !ok {
			return SearchHit{}, false, e
		}
		pins = append(pins, p)
	}
	if len(s.pins)+len(pins) > maxResponseSources {
		return SearchHit{}, false, ErrSavedReadLimit
	}
	// An equivalent current generated root wins; different generated text remains distinct.
	if selected.root {
		var digest, provenance string
		if err := s.history.tx.QueryRowContext(s.history.ctx, "SELECT text_sha256,provenance_json FROM extension_roots WHERE id=?", selected.id).Scan(&digest, &provenance); err != nil {
			return SearchHit{}, false, err
		}
		allCurrent := true
		var members []any
		for ordinal, pin := range pins {
			if pin.current == "" {
				allCurrent = false
				break
			}
			members = append(members, ordinal, pin.current)
		}
		if allCurrent {
			args := append([]any{selected.kind, digest, provenance, len(pins)}, s.live.args...)
			args = append(args, members...)
			var duplicate bool
			err := s.live.tx.QueryRowContext(s.live.ctx, `SELECT EXISTS(SELECT 1 FROM extension_roots r WHERE r.kind=? AND r.text_sha256=? AND json_remove(r.provenance_json,'$.MediaID')=json_remove(?,'$.MediaID') AND r.source_count=? AND `+s.live.extensionRootPredicate()+` AND NOT EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND (rs.ordinal,rs.message_id) NOT IN (`+strings.TrimSuffix(strings.Repeat("(?,?),", len(pins)), ",")+`)))`, args...).Scan(&duplicate)
			if err != nil {
				return SearchHit{}, false, readError(s.live.ctx, err)
			}
			if duplicate {
				return SearchHit{}, false, nil
			}
		}
	}
	var hit SearchHit
	var err error
	if selected.root {
		hit, err = s.history.rootSearchHit(selected)
	} else {
		var hits map[MessageID]SearchHit
		hits, err = s.history.originalSearchHits([]searchSelection{selected})
		hit = hits[MessageID(selected.id)]
	}
	if err != nil {
		return hit, false, err
	}
	page := SearchPage{Hits: []SearchHit{hit}}
	if err = s.reader.history.wrapSearch(&page); err != nil {
		return hit, false, err
	}
	hit = page.Hits[0]
	hit.ChatID = pins[0].chat
	s.pins = append(s.pins, pins...)
	for _, p := range pins {
		s.live.note(p.metadata.Sent, p.expiry)
	}
	return hit, true, nil
}
func (r *savedOwnerRead) Browse(ctx context.Context, request SavedBrowseRequest) (SearchPage, SavedResponseGuard, error) {
	limit, err := readLimit(request.Limit)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	request.Limit = limit
	request.Chats, err = canonicalChats(request.Chats)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	raw := request.Cursor
	request.Cursor = ""
	s, err := r.begin(ctx, request.Chats, request.From, request.Until)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	defer s.close()
	identity := s.identity("browse", request)
	cursor, err := r.decode(identity, raw)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	evidence, err := savedEvidenceFor(s.live)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	if s.history != nil {
		s.history.savedOrderAliases = evidence.aliases
	}
	current, err := savedMetadataRows(s.live, limit+1, cursor.LiveOffset)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	type candidate struct {
		m        savedMetadata
		hist     bool
		position int
		key      string
	}
	candidates := make([]candidate, 0, len(current)+limit)
	for i, m := range current {
		k, e := evidence.canonical(m.Key)
		if e != nil {
			return SearchPage{}, SavedResponseGuard{}, e
		}
		if len(evidence.current[k]) != 1 {
			return SearchPage{}, SavedResponseGuard{}, ErrSourceUnavailable
		}
		candidates = append(candidates, candidate{m: m, position: cursor.LiveOffset + i, key: savedOrderKey(k)})
	}
	historyEnd := true
	scanned := 0
	historyPosition := cursor.HistoryOffset
	var frontier time.Time
	var frontierKey string
	if s.history != nil && !cursor.HistoryDone {
		history, e := savedMetadataRows(s.history, savedCandidateLimit+1, cursor.HistoryOffset)
		if e != nil {
			return SearchPage{}, SavedResponseGuard{}, e
		}
		metadataBytes := evidence.bytes
		for _, m := range history {
			metadataBytes += savedMetadataBytes(m) + int(unsafe.Sizeof(candidate{})) + 128
		}
		for _, m := range current {
			metadataBytes += savedMetadataBytes(m) + int(unsafe.Sizeof(candidate{})) + 128
		}
		if metadataBytes > savedMemoryLimit {
			return SearchPage{}, SavedResponseGuard{}, ErrSavedReadLimit
		}
		historyEnd = len(history) <= savedCandidateLimit
		if !historyEnd {
			history = history[:savedCandidateLimit]
			frontier = history[len(history)-1].Sent
			key, e := evidence.canonical(history[len(history)-1].Key)
			if e != nil {
				return SearchPage{}, SavedResponseGuard{}, e
			}
			frontierKey = savedOrderKey(key)
		}
		for i, m := range history {
			scanned++
			p, ok, e := r.eligible(s.live, m, false)
			if e != nil {
				return SearchPage{}, SavedResponseGuard{}, e
			}
			if !ok {
				continue
			}
			k, e := evidence.canonical(m.Key)
			if e != nil {
				return SearchPage{}, SavedResponseGuard{}, e
			}
			m.Chat = p.chat
			candidates = append(candidates, candidate{m: m, hist: true, position: cursor.HistoryOffset + i, key: savedOrderKey(k)})
		}
		historyPosition += scanned
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.m.Sent.Equal(b.m.Sent) {
			return a.m.Sent.After(b.m.Sent)
		}
		if a.key != b.key {
			return a.key < b.key
		}
		return !a.hist && b.hist
	})
	page := SearchPage{Hits: []SearchHit{}, Snapshot: s.snapshot()}
	for _, c := range candidates {
		if !c.hist && !frontier.IsZero() && (c.m.Sent.Before(frontier) || (c.m.Sent.Equal(frontier) && c.key > frontierKey)) {
			continue
		}
		if len(page.Hits) == limit {
			break
		}
		if c.hist {
			hit, ok, e := s.historicalHit(searchSelection{id: string(c.m.ID), count: 1})
			if e != nil {
				return SearchPage{}, SavedResponseGuard{}, e
			}
			if ok {
				page.Hits = append(page.Hits, hit)
			}
			cursor.HistoryOffset = c.position + 1
		} else {
			hits, e := s.live.originalSearchHits([]searchSelection{{id: string(c.m.ID), count: 1}})
			if e != nil {
				return SearchPage{}, SavedResponseGuard{}, e
			}
			page.Hits = append(page.Hits, hits[c.m.ID])
			cursor.LiveOffset = c.position + 1
		}
	}
	histRemaining := false
	liveRemaining := false
	for _, c := range candidates {
		if c.hist && c.position >= cursor.HistoryOffset {
			histRemaining = true
		}
		if !c.hist && c.position >= cursor.LiveOffset {
			liveRemaining = true
		}
	}
	if !histRemaining {
		cursor.HistoryOffset = historyPosition
		cursor.HistoryDone = historyEnd
	}
	if liveRemaining || histRemaining || !cursor.HistoryDone {
		page.NextCursor, err = r.encode(identity, cursor)
		if err != nil {
			return SearchPage{}, SavedResponseGuard{}, err
		}
	}
	if err = boundSearch(&page); err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	guard, err := s.guard()
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	s.close()
	s.live = nil
	s.history = nil
	if err = r.Check(ctx, guard); err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	return page, guard, nil
}
func (r *savedOwnerRead) Search(ctx context.Context, request SearchRequest) (SearchPage, SavedResponseGuard, error) {
	expression, err := literalQuery(request.Query)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	limit, err := readLimit(request.Limit)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	request.Limit = limit
	request.Chats, err = canonicalChats(request.Chats)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	raw := request.Cursor
	request.Cursor = ""
	s, err := r.begin(ctx, request.Chats, request.From, request.Until)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	defer s.close()
	normalized := request
	normalized.Query = expression
	identity := s.identity("search", normalized)
	cursor, err := r.decode(identity, raw)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	liveReq := request
	liveReq.Limit = 100
	liveReq.Cursor = cursor.LiveCursor
	var live SearchPage
	var lg ResponseGuard
	s.close()
	s.live, s.history = nil, nil
	searchCtx, searchCancel := context.WithDeadline(ctx, r.deadline)
	defer searchCancel()
	if !cursor.LiveDone {
		live, lg, err = r.search.SearchGuarded(searchCtx, r.scope, liveReq)
		if err != nil {
			if errors.Is(err, ErrCursorStale) {
				err = ErrSavedCursorStale
			}
			return SearchPage{}, SavedResponseGuard{}, err
		}
	}
	s, err = r.begin(ctx, request.Chats, request.From, request.Until)
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	defer s.close()
	finalIdentity := s.identity("search", normalized)
	if raw != "" && finalIdentity != identity {
		return SearchPage{}, SavedResponseGuard{}, ErrSavedCursorStale
	}
	identity = finalIdentity
	if cursor.LiveSkip > len(live.Hits) {
		return SearchPage{}, SavedResponseGuard{}, ErrSavedCursorStale
	}
	var selections []searchSelection
	if s.history != nil && !cursor.HistoryDone {
		selections, err = s.history.searchSelections(expression, savedCandidateLimit, cursor.HistoryOffset)
		if err != nil {
			return SearchPage{}, SavedResponseGuard{}, err
		}
	}
	historyMore := len(selections) > savedCandidateLimit
	if historyMore {
		selections = selections[:savedCandidateLimit]
	}
	page := SearchPage{Hits: []SearchHit{}, Snapshot: s.snapshot()}
	hi := 0
	li := cursor.LiveSkip
	for len(page.Hits) < limit {
		if hi == len(selections) && historyMore && cursor.LiveRank > cursor.HistoryRank {
			break
		}
		if li == len(live.Hits) && live.NextCursor != "" && cursor.LiveRank <= cursor.HistoryRank {
			break
		}
		useLive := li < len(live.Hits) && (hi >= len(selections) || cursor.LiveRank <= cursor.HistoryRank)
		if useLive {
			page.Hits = append(page.Hits, live.Hits[li])
			li++
			cursor.LiveRank++
			continue
		}
		if hi >= len(selections) {
			break
		}
		hit, ok, e := s.historicalHit(selections[hi])
		if e != nil {
			return SearchPage{}, SavedResponseGuard{}, e
		}
		hi++
		cursor.HistoryOffset++
		if ok {
			page.Hits = append(page.Hits, hit)
			cursor.HistoryRank++
		}
	}
	cursor.LiveSkip = li
	if li == len(live.Hits) {
		cursor.LiveCursor = live.NextCursor
		cursor.LiveSkip = 0
		cursor.LiveDone = live.NextCursor == ""
	}
	if hi == len(selections) {
		cursor.HistoryDone = !historyMore
	}
	if !cursor.LiveDone || !cursor.HistoryDone {
		page.NextCursor, err = r.encode(identity, cursor)
		if err != nil {
			return SearchPage{}, SavedResponseGuard{}, err
		}
	}
	if err = boundSearch(&page); err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	guard, err := s.guard()
	if err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	if lg.ScopeIdentity != [32]byte{} {
		guard.live = append(guard.live, lg)
	}
	s.close()
	s.live = nil
	s.history = nil
	if err = r.Check(ctx, guard); err != nil {
		return SearchPage{}, SavedResponseGuard{}, err
	}
	return page, guard, nil
}
func (r *savedOwnerRead) Check(ctx context.Context, g SavedResponseGuard) error {
	if g.owner != r {
		return ErrUnauthorized
	}
	ctx, cancel := context.WithDeadline(ctx, r.deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, guard := range g.live {
		if _, err := r.live.CheckResponse(ctx, r.scope, ResponseCheck{Guard: guard}); err != nil {
			return err
		}
	}
	for _, guard := range g.historical {
		if r.bridge == nil {
			return ErrUnauthorized
		}
		if _, err := r.bridge.CheckResponse(ctx, r.bridge.original, ResponseCheck{Guard: guard}); err != nil {
			return err
		}
	}
	return r.checkFenced(ctx, g)
}

// Acquire pools and call clocks before the commit fence. Every current
// contributor and historical absence fact is then checked in the same snapshot.
func (r *savedOwnerRead) checkFenced(ctx context.Context, g SavedResponseGuard) error {
	connection, err := r.live.readDB.Conn(ctx)
	if err != nil {
		return readError(ctx, err)
	}
	defer connection.Close()
	var hf *readFrame
	if r.bridge != nil {
		hf, err = r.history.snapshot.beginRead(ctx, r.bridge.translated, nil, time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		defer hf.close()
	}
	sample, wall := r.live.now(), time.Now()
	r.live.safetyMu.RLock()
	defer r.live.safetyMu.RUnlock()
	if r.live.unclean || r.live.unsafe[r.scope.Account] {
		return ErrReconciliationRequired
	}
	tx, err := connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return readError(ctx, err)
	}
	defer tx.Rollback()
	f, err := extensionResponseFrame(r.live, ctx, tx, r.scope, sample.Add(time.Since(wall)))
	if err != nil {
		return err
	}
	for _, guard := range g.live {
		if guard.coverageEpoch != f.snapshot.CoverageEpoch || guard.recoveryEpoch != f.snapshot.RecoveryEpoch || guard.indexGeneration != 0 && guard.indexGeneration != f.snapshot.IndexGeneration {
			return ErrCursorStale
		}
		generation, e := extensionActiveGeneration(ctx, tx)
		if e != nil {
			return e
		}
		if generation.ID != guard.Generation || generation.Version.ID != guard.IndexVersionID {
			return ErrCursorStale
		}
		expected := make(map[MessageID]SourceReadVersion, len(guard.Sources))
		refs := make([]SourceRef, 0, len(guard.Sources))
		for _, source := range guard.Sources {
			expected[source.Ref.Version.MessageID] = source
			refs = append(refs, source.Ref)
		}
		if len(refs) > 0 {
			if _, e = r.live.extensionCheckSourcesAt(ctx, tx, refs, false, f.now); e != nil {
				return e
			}
		}
		current, e := f.extensionReadSources(expected)
		if e != nil {
			return e
		}
		if e = f.extensionCheckRootPinsWithSources(guard.roots, expected, current); e != nil {
			return e
		}
		if e = f.checkAttachmentPins(guard.attachments, expected); e != nil {
			return e
		}
		f.note(guard.minSent, guard.minExpiry)
	}
	// Absence checks need all current rows only in contributor chats, never
	// unrelated account content. Source guards were already checked above.
	needed := map[ChatID]bool{}
	for _, pin := range g.historicalPins {
		needed[pin.chat] = true
	}
	for _, guard := range g.live {
		for _, source := range guard.Sources {
			needed[source.ChatID] = true
		}
	}
	for chat := range needed {
		f.savedEvidenceChats = append(f.savedEvidenceChats, chat)
	}
	if len(needed) > 0 {
		evidence, e := savedEvidenceFor(f)
		if e != nil {
			return e
		}
		for _, current := range evidence.current {
			if len(current) > 1 {
				return ErrSourceUnavailable
			}
		}
	}
	for _, pin := range g.historicalPins {
		if hf == nil {
			return ErrSourceUnavailable
		}
		if e := (&savedSession{reader: r, live: f, history: hf}).historicalUnique(pin.metadata); e != nil {
			return e
		}
		current, ok, e := r.eligible(f, pin.metadata, pin.equivalent)
		if e != nil {
			return e
		}
		if !ok || current.chat != pin.chat || current.canonical != pin.canonical || !current.expiry.Equal(pin.expiry) {
			return ErrSourceUnavailable
		}
	}
	if g.aggregate != nil {
		request := g.aggregate.request
		savedNarrowFrame(f, request.ChatID, request.From, request.Until)
		if hf != nil {
			savedNarrowFrame(hf, "", request.From, request.Until)
			if request.ChatID != "" {
				evidence, e := savedEvidenceFor(f)
				if e != nil {
					return e
				}
				var wanted string
				for key, id := range evidence.chats {
					if id == request.ChatID {
						wanted = key
					}
				}
				var mapped ChatID
				for _, hid := range r.bridge.translated.Chats {
					var source string
					if e = hf.tx.QueryRowContext(ctx, "SELECT source_chat FROM chats WHERE id=?", hid).Scan(&source); e != nil {
						return readError(ctx, e)
					}
					key, e := evidence.canonical(SourceKey{Chat: source})
					if e != nil {
						return e
					}
					if key.Chat == wanted {
						mapped = hid
					}
				}
				if mapped != "" {
					savedNarrowFrame(hf, mapped, time.Time{}, time.Time{})
				} else {
					// A current-only chat has no historical collision domain.
					hf = nil
				}
			}
		}
		session := &savedSession{reader: r, live: f, history: hf}
		rows, e := session.contributors(request)
		if e != nil {
			return e
		}
		selected := rows[:0]
		for _, row := range rows {
			if g.aggregate.members[savedMemberID(row)] {
				selected = append(selected, row)
			}
		}
		if len(selected) != len(g.aggregate.members) || savedDigest(selected) != g.aggregate.digest {
			return ErrSourceUnavailable
		}
	}
	if err = tx.Commit(); err != nil {
		return readError(ctx, err)
	}
	now := sample.Add(time.Since(wall))
	if !f.minExpiry.IsZero() && !now.Before(f.minExpiry) || f.rolling > 0 && !f.minSent.IsZero() && f.minSent.Before(now.Add(-f.rolling)) {
		return ErrSourceUnavailable
	}
	return ctx.Err()
}
func savedNarrowFrame(f *readFrame, chat ChatID, from, until time.Time) {
	if chat != "" {
		f.where += " AND m.chat_id=?"
		f.args = append(f.args, chat)
		f.savedEvidenceChats = []ChatID{chat}
		f.savedEvidence = nil
	}
	if !from.IsZero() {
		f.where += " AND m.original_sent_at>=?"
		f.args = append(f.args, ceilMillis(from))
	}
	if !until.IsZero() {
		f.where += " AND m.original_sent_at<?"
		f.args = append(f.args, ceilMillis(until))
	}
}

func (r *savedOwnerRead) Context(ctx context.Context, request ContextRequest) (ContextPage, SavedResponseGuard, error) {
	if request.Before < 0 || request.Before > maxContextSide || request.After < 0 || request.After > maxContextSide {
		return ContextPage{}, SavedResponseGuard{}, ErrInvalidQuery
	}
	s, err := r.begin(ctx, nil, time.Time{}, time.Time{})
	if err != nil {
		return ContextPage{}, SavedResponseGuard{}, err
	}
	defer s.close()
	historical := IsHistoricalHandle(string(request.MessageID))
	id := request.MessageID
	f := s.live
	if historical {
		if s.history == nil {
			return ContextPage{}, SavedResponseGuard{}, ErrSourceUnavailable
		}
		raw, e := r.history.unwrapHandle(string(id), "m")
		if e != nil {
			return ContextPage{}, SavedResponseGuard{}, e
		}
		id = MessageID(raw)
		f = s.history
	}
	center, err := savedMetadataByID(f, id)
	if err != nil {
		return ContextPage{}, SavedResponseGuard{}, err
	}
	chat := center.Chat
	if historical {
		var currentChat ChatID
		key, e := savedCanonical(s.live.ctx, s.live.tx, center.Key)
		if e != nil {
			return ContextPage{}, SavedResponseGuard{}, e
		}
		e = s.live.tx.QueryRowContext(s.live.ctx, "SELECT c.id FROM chats c WHERE c.account=? AND "+savedCanonicalSQL("c", "source_chat")+"=?", key.Account, key.Chat).Scan(&currentChat)
		if e != nil {
			return ContextPage{}, SavedResponseGuard{}, ErrSourceUnavailable
		}
		s.live.savedEvidenceChats = []ChatID{currentChat}
		s.live.savedEvidence = nil
		pin, ok, e := r.eligible(s.live, center, true)
		if e != nil {
			return ContextPage{}, SavedResponseGuard{}, e
		}
		if !ok {
			return ContextPage{}, SavedResponseGuard{}, ErrSourceUnavailable
		}
		chat = pin.chat
		if pin.current != "" {
			id = pin.current
			historical = false
			center, e = savedMetadataByID(s.live, id)
			if e != nil {
				return ContextPage{}, SavedResponseGuard{}, e
			}
		}
	}
	savedNarrowFrame(s.live, chat, time.Time{}, time.Time{})
	if s.history != nil {
		var mapped ChatID
		if historical {
			mapped = center.Chat
		} else {
			evidence, e := savedEvidenceFor(s.live)
			if e != nil {
				return ContextPage{}, SavedResponseGuard{}, e
			}
			canonical, e := evidence.canonical(center.Key)
			if e != nil {
				return ContextPage{}, SavedResponseGuard{}, e
			}
			for _, hid := range r.bridge.translated.Chats {
				var source string
				if e = s.history.tx.QueryRowContext(s.history.ctx, "SELECT source_chat FROM chats WHERE id=?", hid).Scan(&source); e != nil {
					return ContextPage{}, SavedResponseGuard{}, e
				}
				key, e := evidence.canonical(SourceKey{Chat: source})
				if e != nil {
					return ContextPage{}, SavedResponseGuard{}, e
				}
				if key.Chat == canonical.Chat {
					mapped = hid
				}
			}
		}
		if mapped == "" {
			s.history.close()
			s.history = nil
		} else {
			savedNarrowFrame(s.history, mapped, time.Time{}, time.Time{})
		}
	}
	rows, err := s.contributors(StatsRequest{})
	if err != nil {
		return ContextPage{}, SavedResponseGuard{}, err
	}
	neighborhood := make([]savedContributor, 0, len(rows))
	for _, row := range rows {
		if row.Chat == chat {
			neighborhood = append(neighborhood, row)
		}
	}
	sort.Slice(neighborhood, func(i, j int) bool {
		a, b := neighborhood[i], neighborhood[j]
		if !a.Metadata.Sent.Equal(b.Metadata.Sent) {
			return a.Metadata.Sent.Before(b.Metadata.Sent)
		}
		return savedOrderKey(a.Canonical) < savedOrderKey(b.Canonical)
	})
	at := -1
	for i, c := range neighborhood {
		if c.Metadata.ID == id && c.Historical == historical {
			at = i
			break
		}
	}
	if at < 0 {
		return ContextPage{}, SavedResponseGuard{}, ErrSourceUnavailable
	}
	page := ContextPage{Messages: []MessageView{}, Snapshot: s.snapshot()}
	selected := neighborhood[max(0, at-request.Before):min(len(neighborhood), at+request.After+1)]
	for _, c := range selected {
		frame := s.live
		if c.Historical {
			frame = s.history
		}
		raw, err := frame.contextRows(" AND m.id=?", []any{c.Metadata.ID}, "m.id", 1)
		if err != nil {
			return ContextPage{}, SavedResponseGuard{}, err
		}
		if len(raw) != 1 {
			return ContextPage{}, SavedResponseGuard{}, ErrSourceUnavailable
		}
		row := raw[0]
		view := row.view
		frame.note(c.Metadata.Sent, c.Metadata.Expiry)
		if c.Historical {
			if _, ok, e := s.accept(*c.Metadata, false); e != nil {
				return ContextPage{}, SavedResponseGuard{}, e
			} else if !ok {
				return ContextPage{}, SavedResponseGuard{}, ErrSourceUnavailable
			}
		}
		// Resolve every reply target through the same union, never stored quote text.
		if row.reply.Message != "" {
			key, e := savedCanonical(s.live.ctx, s.live.tx, row.reply)
			if e != nil {
				return ContextPage{}, SavedResponseGuard{}, e
			}
			for _, target := range rows {
				if target.Canonical != key {
					continue
				}
				tf := s.live
				if target.Historical {
					tf = s.history
					if _, ok, e := s.accept(*target.Metadata, false); e != nil {
						return ContextPage{}, SavedResponseGuard{}, e
					} else if !ok {
						continue
					}
				}
				quoted, e := tf.contextRows(" AND m.id=?", []any{target.Metadata.ID}, "m.id", 1)
				if e != nil {
					return ContextPage{}, SavedResponseGuard{}, e
				}
				if len(quoted) == 1 {
					view.ReplyTo = target.Metadata.ID
					view.ReplyQuote = boundedOriginal(quoted[0].view.Body, 1024)
					if view.ReplyQuote == "" {
						view.ReplyQuote = boundedOriginal(quoted[0].view.Caption, 1024)
					}
					tf.note(target.Metadata.Sent, target.Metadata.Expiry)
					if target.Historical {
						handle, e := r.history.wrapHandle(string(view.ReplyTo), "m")
						if e != nil {
							return ContextPage{}, SavedResponseGuard{}, e
						}
						view.ReplyTo = MessageID(handle)
					}
				}
				break
			}
		}
		if c.Historical {
			reply := view.ReplyTo
			view.ReplyTo = ""
			wrapped := ContextPage{Messages: []MessageView{view}}
			if err = r.history.wrapContext(&wrapped); err != nil {
				return ContextPage{}, SavedResponseGuard{}, err
			}
			view = wrapped.Messages[0]
			view.ChatID = c.Chat
			view.ReplyTo = reply
		}
		page.Messages = append(page.Messages, view)
	}
	centerID := id
	if historical {
		centerID = request.MessageID
	}
	remaining := maxContextDerivedPerPage
	for _, index := range contextPriority(&page, centerID) {
		if remaining == 0 {
			break
		}
		derived, e := s.contextSavedDerived(selected[index], min(remaining, maxContextDerivedPerMessage))
		if e != nil {
			return ContextPage{}, SavedResponseGuard{}, e
		}
		page.Messages[index].Derived = derived
		remaining -= len(derived)
	}
	if err = boundContext(&page, centerID); err != nil {
		return ContextPage{}, SavedResponseGuard{}, err
	}
	guard, err := s.guard()
	if err != nil {
		return ContextPage{}, SavedResponseGuard{}, err
	}
	s.close()
	s.live = nil
	s.history = nil
	if err = r.Check(ctx, guard); err != nil {
		return ContextPage{}, SavedResponseGuard{}, err
	}
	return page, guard, nil
}

func (s *savedSession) contextSavedDerived(c savedContributor, limit int) ([]DerivedExcerpt, error) {
	var out []DerivedExcerpt
	if !c.Historical {
		derived, err := s.live.contextDerivedFor(c.Metadata.ID, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, derived...)
	}
	if s.history == nil || len(out) >= limit {
		return out, nil
	}
	var historicalID MessageID
	if c.Historical {
		historicalID = c.Metadata.ID
	} else {
		// Metadata-only lookup; current aliases canonicalize both full-key sides.
		rows, err := s.history.tx.QueryContext(s.history.ctx, "SELECT m.id,m.source_chat,m.source_sender,m.source_message,m.from_me FROM messages m WHERE m.account=? AND m.source_message=? AND m.from_me=? LIMIT ?", c.Canonical.Account, c.Canonical.Message, c.Canonical.FromMe, savedCandidateLimit+1)
		if err != nil {
			return nil, readError(s.history.ctx, err)
		}
		evidence, err := savedEvidenceFor(s.live)
		if err != nil {
			rows.Close()
			return nil, err
		}
		count := 0
		for rows.Next() {
			var id MessageID
			k := SourceKey{Account: c.Canonical.Account}
			if err = rows.Scan(&id, &k.Chat, &k.Sender, &k.Message, &k.FromMe); err != nil {
				rows.Close()
				return nil, err
			}
			count++
			if count > savedCandidateLimit {
				rows.Close()
				return nil, ErrSavedReadLimit
			}
			k, err = evidence.canonical(k)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if k == c.Canonical {
				if historicalID != "" {
					rows.Close()
					return nil, ErrSourceUnavailable
				}
				historicalID = id
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, readError(s.history.ctx, err)
		}
	}
	if historicalID == "" {
		return out, nil
	}
	args := append([]any{historicalID}, s.history.args...)
	args = append(args, limit+1)
	rows, err := s.history.tx.QueryContext(s.history.ctx, `SELECT r.id,r.kind,r.source_count FROM extension_roots r JOIN extension_root_sources a ON a.root_id=r.id AND a.ordinal=0 WHERE a.message_id=? AND `+s.history.extensionRootPredicate()+` ORDER BY r.id LIMIT ?`, args...)
	if err != nil {
		return nil, readError(s.history.ctx, err)
	}
	var roots []searchSelection
	for rows.Next() {
		var root searchSelection
		root.root = true
		if err = rows.Scan(&root.id, &root.kind, &root.count); err != nil {
			rows.Close()
			return nil, err
		}
		roots = append(roots, root)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, readError(s.history.ctx, err)
	}
	for _, root := range roots {
		if len(out) == limit {
			break
		}
		hit, ok, e := s.historicalHit(root)
		if e != nil {
			return nil, e
		}
		if ok {
			out = append(out, hit.Derived...)
		}
	}
	return out, nil
}

func (s *savedSession) snapshot() ReadSnapshot {
	snapshot := s.live.snapshot
	if s.history != nil {
		snapshot.UnknownHistoryGap = snapshot.UnknownHistoryGap || s.history.snapshot.UnknownHistoryGap
		snapshot.DeliveredHistorySnapshot = snapshot.DeliveredHistorySnapshot || s.history.snapshot.DeliveredHistorySnapshot
	}
	return snapshot
}

// Source keys may have acquired aliases after the snapshot was captured.
// Check both raw alias sets before choosing any historical original or root.
func (s *savedSession) historicalUnique(m savedMetadata) error {
	evidence, err := savedEvidenceFor(s.live)
	if err != nil {
		return err
	}
	key, err := evidence.canonical(m.Key)
	if err != nil {
		return err
	}
	forms := func(canonical string) []string {
		out := []string{canonical}
		for alias, target := range evidence.aliases {
			if target == canonical && alias != canonical {
				out = append(out, alias)
			}
		}
		return out
	}
	chats, senders := forms(key.Chat), forms(key.Sender)
	if len(chats)+len(senders) > maxReadChats {
		return ErrSavedReadLimit
	}
	args := []any{key.Account, key.Message, key.FromMe}
	marks := func(values []string) string {
		out := make([]string, len(values))
		for i, value := range values {
			out[i] = "?"
			args = append(args, value)
		}
		return strings.Join(out, ",")
	}
	query := "SELECT count(*) FROM (SELECT 1 FROM messages WHERE account=? AND source_message=? AND from_me=? AND source_chat IN (" + marks(chats) + ") AND source_sender IN (" + marks(senders) + ") LIMIT 2)"
	var count int
	if err = s.history.tx.QueryRowContext(s.history.ctx, query, args...).Scan(&count); err != nil {
		return readError(s.history.ctx, err)
	}
	if count != 1 {
		return ErrSourceUnavailable
	}
	return nil
}
