package archive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unsafe"
)

type savedContributor struct {
	Metadata             *savedMetadata
	Historical           bool
	Canonical            SourceKey
	Chat                 ChatID
	Person               string
	Transcript, Retained bool
	HistoricalTranscript *savedMetadata
}

func (s *savedSession) contributors(request StatsRequest) ([]savedContributor, error) {
	existingEvidence := s.live.savedEvidence != nil
	keys, err := savedAggregateNegativeKeys(s.live, s.history)
	if err != nil {
		return nil, err
	}
	evidence, err := savedEvidenceFor(s.live, keys)
	if err != nil {
		return nil, err
	}

	var out []savedContributor
	inspected, bytes := 0, evidence.bytes
	if existingEvidence {
		bytes += keys.bytes
	}
	indices := map[MessageID]int{}
	peopleBudget := map[string]bool{}
	chatBudget := map[ChatID]bool{}
	dayBudget := map[string]bool{}
	membershipBudget := map[[2]string]bool{}
	s.live.captureSources = false
	defer func() { s.live.captureSources = true }()
	for _, historical := range []bool{false, true} {
		f := s.live
		if historical {
			f = s.history
		}
		if f == nil {
			continue
		}
		seen := map[[32]byte]bool{}
		err := savedVisitMetadata(f, savedMetadataLimit+1-inspected, func(m savedMetadata) error {
			inspected++
			if inspected > savedMetadataLimit {
				return ErrSavedReadLimit
			}
			kind := string(m.Kind)
			if kind != "text" && kind != "voice" && kind != "image" {
				kind = "other"
			}
			if request.Kind != "" && request.Kind != "all" && request.Kind != kind {
				return nil
			}
			key, e := evidence.canonical(m.Key)
			if e != nil {
				return e
			}
			keyHash := sha256.Sum256([]byte(savedOrderKey(key)))
			if seen[keyHash] {
				return ErrSourceUnavailable
			}
			seen[keyHash] = true
			metadata := &m
			if !historical {
				current := evidence.current[key]
				if len(current) != 1 || current[0].metadata.ID != m.ID {
					return ErrSourceUnavailable
				}
				metadata = &current[0].metadata
			}
			c := savedContributor{Metadata: metadata, Canonical: key, Chat: m.Chat, Person: m.Sender, Historical: historical, Transcript: m.Transcript, Retained: m.Retained}
			if historical {
				pin, ok, e := s.reader.eligible(s.live, m, false)
				if e != nil {
					return e
				}
				if !ok {
					if m.Kind == MessageVoice && m.Transcript {
						pin, equivalent, e := s.reader.eligible(s.live, m, true)
						if e != nil {
							return e
						}
						if index, exists := indices[pin.current]; exists && equivalent && pin.current == out[index].Metadata.ID && !out[index].Transcript {
							copy := m
							copy.Expiry = pin.expiry
							s.live.note(copy.Sent, copy.Expiry)
							out[index].HistoricalTranscript = &copy
							out[index].Transcript = true
							bytes += savedMetadataBytes(copy)
							if bytes > savedMemoryLimit {
								return ErrSavedReadLimit
							}
						}
					}
					return nil
				}
				c.Chat = pin.chat
				c.Metadata.Expiry = pin.expiry
				id, e := s.reader.history.wrapHandle(m.Sender, "i")
				if e != nil {
					return e
				}
				c.Person = id
			}
			if request.ChatID != "" && c.Chat != request.ChatID {
				return nil
			}
			// Current contributors borrow the evidence row; only historical rows
			// allocate another metadata record. Reserve map/slice/witness overhead.
			bytes += int(unsafe.Sizeof(c)) + 192 + len(key.Chat) + len(key.Sender) + len(c.Person)
			if historical {
				bytes += savedMetadataBytes(*c.Metadata)
			}
			person := key.Account + "\x00" + key.Sender
			day := m.Sent.UTC().Format("2006-01-02")
			member := [2]string{person, string(c.Chat)}
			if !peopleBudget[person] {
				peopleBudget[person] = true
				bytes += 512 + len(person)
			}
			if !chatBudget[c.Chat] {
				chatBudget[c.Chat] = true
				bytes += 384
			}
			if !dayBudget[day] {
				dayBudget[day] = true
				bytes += 128
			}
			if !membershipBudget[member] {
				membershipBudget[member] = true
				bytes += 160 + len(person)
			}
			if bytes > savedMemoryLimit {
				return ErrSavedReadLimit
			}
			s.live.note(m.Sent, c.Metadata.Expiry)
			if name, ok := evidence.chatNames[c.Chat]; ok {
				c.Metadata.ChatName = name
			}
			indices[m.ID] = len(out)
			out = append(out, c)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func savedVisitMetadata(f *readFrame, limit int, visit func(savedMetadata) error) error {
	args := append(append([]any(nil), f.args...), limit)
	rows, err := f.tx.QueryContext(f.ctx, "SELECT "+savedMetadataColumns+" FROM "+authorizedTables+" WHERE "+f.where+" ORDER BY m.original_sent_at DESC,m.source_chat,m.source_sender,m.source_message,m.from_me,m.id LIMIT ?", args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		m, e := scanSavedMetadata(rows)
		if e != nil {
			return readError(f.ctx, e)
		}
		if e = visit(m); e != nil {
			return e
		}
	}
	return readError(f.ctx, rows.Err())
}
func savedDigest(rows []savedContributor) [32]byte {
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for _, row := range rows {
		_ = encoder.Encode(row)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
func (r *savedOwnerRead) Stats(ctx context.Context, request StatsRequest) (StatsSnapshot, SavedResponseGuard, error) {
	if !ValidStatsRequest(request) {
		return StatsSnapshot{}, SavedResponseGuard{}, ErrInvalidQuery
	}
	var chats []ChatID
	if request.ChatID != "" {
		chats = []ChatID{request.ChatID}
	}
	s, err := r.begin(ctx, chats, request.From, request.Until)
	if err != nil {
		return StatsSnapshot{}, SavedResponseGuard{}, err
	}
	defer s.close()
	rows, err := s.contributors(request)
	if err != nil {
		return StatsSnapshot{}, SavedResponseGuard{}, err
	}
	page := savedStats(rows, request, s.live.now)
	page.ArchiveRevision = s.live.snapshot.ArchiveRevision
	page.UnknownHistoryGap = s.snapshot().UnknownHistoryGap
	page.DeliveredHistorySnapshot = s.snapshot().DeliveredHistorySnapshot
	guard, err := s.guard()
	if err != nil {
		return StatsSnapshot{}, SavedResponseGuard{}, err
	}
	guard.aggregate = &savedAggregateWitness{request: request, digest: savedDigest(rows), members: savedMembers(rows)}
	s.close()
	s.live = nil
	s.history = nil
	if err = r.Check(ctx, guard); err != nil {
		return StatsSnapshot{}, SavedResponseGuard{}, err
	}
	return page, guard, nil
}
func savedStats(rows []savedContributor, request StatsRequest, now time.Time) StatsSnapshot {
	page := EmptyStatsSnapshot(request, now)
	page.Messages = int64(len(rows))
	if len(rows) == 0 {
		return page
	}
	type group struct {
		row   StatsBreakdownRow
		chats map[ChatID]bool
	}
	chats := map[ChatID]*group{}
	people := map[string]*group{}
	days := map[string]int64{}
	kinds := map[string]int64{}
	rhythm := map[[2]int]int64{}
	first, last := rows[0].Metadata.Sent, rows[0].Metadata.Sent
	add := func(g *group, c savedContributor) {
		g.row.Messages++
		if c.Metadata.Key.FromMe {
			g.row.Sent++
		}
		g.row.Received = g.row.Messages - g.row.Sent
		g.chats[c.Chat] = true
		g.row.Chats = int64(len(g.chats))
		g.row.LastMessageAt = max(g.row.LastMessageAt, c.Metadata.Sent.Unix())
	}
	for _, c := range rows {
		m := c.Metadata
		first = earlier(first, m.Sent)
		last = later(last, m.Sent)
		if m.Key.FromMe {
			page.Sent++
		}
		day := m.Sent.UTC().Format("2006-01-02")
		days[day]++
		rhythm[[2]int{int(m.Sent.UTC().Weekday()), m.Sent.UTC().Hour()}]++
		kind := string(m.Kind)
		if kind != "text" && kind != "voice" && kind != "image" {
			kind = "other"
		}
		kinds[kind]++
		if m.Kind == MessageVoice {
			page.Media.VoiceMessages++
			if c.Transcript {
				page.Media.TranscribedVoice++
			}
			if c.Retained && !c.Historical {
				page.Media.RetainedVoice++
			}
		}
		if chats[c.Chat] == nil {
			chats[c.Chat] = &group{row: StatsBreakdownRow{ID: string(c.Chat), Name: m.ChatName}, chats: map[ChatID]bool{}}
		}
		add(chats[c.Chat], c)
		key := c.Canonical.Account + "\x00" + c.Canonical.Sender
		if people[key] == nil {
			people[key] = &group{row: StatsBreakdownRow{ID: c.Person, Name: m.SenderName}, chats: map[ChatID]bool{}}
		}
		if !c.Historical {
			people[key].row.ID = c.Person
			people[key].row.Name = m.SenderName
		}
		add(people[key], c)
	}
	page.Chats = int64(len(chats))
	page.Received = page.Messages - page.Sent
	page.FirstMessageAt = first.Unix()
	page.LastMessageAt = last.Unix()
	interval := statsChooseInterval(first, last)
	page.ActivityInterval = interval.name
	page.ActivityStep = interval.step
	page.ActivityFrom = interval.start(first).Unix()
	buckets := map[string]int64{}
	for _, c := range rows {
		buckets[interval.start(c.Metadata.Sent).Format("2006-01-02")]++
	}
	for at := interval.start(first); !at.After(last); at = interval.next(at) {
		date := at.Format("2006-01-02")
		page.Daily = append(page.Daily, StatsActivity{Date: date, Messages: buckets[date]})
		page.ActivityUntil = interval.next(at).Unix()
	}
	var dates []string
	for day := range days {
		dates = append(dates, day)
	}
	sort.Strings(dates)
	page.Insights.ActiveDays = int64(len(dates))
	var previous time.Time
	streak := int64(0)
	for _, day := range dates {
		at, _ := time.Parse("2006-01-02", day)
		if previous.AddDate(0, 0, 1).Equal(at) {
			streak++
		} else {
			streak = 1
		}
		previous = at
		page.Insights.LongestStreakDays = max(page.Insights.LongestStreakDays, streak)
		if days[day] > page.Insights.BusiestDateMessages {
			page.Insights.BusiestDate = day
			page.Insights.BusiestDateMessages = days[day]
		}
	}
	for _, kind := range []string{"image", "other", "text", "voice"} {
		if kinds[kind] > 0 {
			page.Kinds = append(page.Kinds, StatsKind{Kind: kind, Messages: kinds[kind]})
		}
	}
	for weekday := 0; weekday < 7; weekday++ {
		for hour := 0; hour < 24; hour++ {
			if n := rhythm[[2]int{weekday, hour}]; n > 0 {
				page.Rhythm = append(page.Rhythm, StatsRhythm{Weekday: weekday, Hour: hour, Messages: n})
			}
		}
	}
	var chatRows []StatsBreakdownRow
	for _, g := range chats {
		chatRows = append(chatRows, g.row)
	}
	sort.Slice(chatRows, func(i, j int) bool {
		if chatRows[i].Messages != chatRows[j].Messages {
			return chatRows[i].Messages > chatRows[j].Messages
		}
		return chatRows[i].ID < chatRows[j].ID
	})
	for i, row := range chatRows {
		if i == 8 {
			break
		}
		page.TopChats = append(page.TopChats, StatsChat{ChatID: ChatID(row.ID), Name: row.Name, Messages: row.Messages, LastMessageAt: row.LastMessageAt})
	}
	breakdown := chatRows
	if page.Breakdown.Dimension == "people" {
		breakdown = nil
		for _, g := range people {
			breakdown = append(breakdown, g.row)
		}
	}
	sort.Slice(breakdown, func(i, j int) bool {
		a, b := breakdown[i], breakdown[j]
		switch page.Breakdown.Sort {
		case "name":
			an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
			if an != bn {
				return an < bn
			}
		case "recent":
			if a.LastMessageAt != b.LastMessageAt {
				return a.LastMessageAt > b.LastMessageAt
			}
		default:
			if a.Messages != b.Messages {
				return a.Messages > b.Messages
			}
		}
		return a.ID < b.ID
	})
	page.Breakdown.Total = int64(len(breakdown))
	page.Breakdown.Page = max(1, request.Page)
	lastPage := (len(breakdown) + page.Breakdown.PageSize - 1) / page.Breakdown.PageSize
	page.Breakdown.Page = min(page.Breakdown.Page, max(1, lastPage))
	start := (page.Breakdown.Page - 1) * page.Breakdown.PageSize
	end := min(start+page.Breakdown.PageSize, len(breakdown))
	page.Breakdown.Rows = append(page.Breakdown.Rows, breakdown[start:end]...)
	return page
}
func (r *savedOwnerRead) Chats(ctx context.Context, request ChatListRequest) (ChatPage, SavedResponseGuard, error) {
	limit, err := readLimit(request.Limit)
	if err != nil {
		return ChatPage{}, SavedResponseGuard{}, err
	}
	query, kind, err := chatListFilter(request)
	if err != nil {
		return ChatPage{}, SavedResponseGuard{}, err
	}
	request.Limit = limit
	request.Query = query
	request.ChatType = kind
	raw := request.Cursor
	request.Cursor = ""
	s, err := r.begin(ctx, nil, time.Time{}, time.Time{})
	if err != nil {
		return ChatPage{}, SavedResponseGuard{}, err
	}
	defer s.close()
	identity := s.identity("chats", request)
	cursor, err := r.decode(identity, raw)
	if err != nil {
		return ChatPage{}, SavedResponseGuard{}, err
	}
	rows, err := s.contributors(StatsRequest{})
	if err != nil {
		return ChatPage{}, SavedResponseGuard{}, err
	}
	byID := map[ChatID]*ChatView{}
	for _, c := range rows {

		chat := byID[c.Chat]
		if chat == nil {
			chat = &ChatView{ID: c.Chat, Name: c.Metadata.ChatName, Coverage: "partial", GapCodes: []string{}}
			byID[c.Chat] = chat
		}
		chat.MessageCount++
		chat.Oldest = earlier(chat.Oldest, c.Metadata.Sent)
		chat.Latest = later(chat.Latest, c.Metadata.Sent)
	}
	var all []ChatView
	for _, chat := range byID {
		if query != "" && !strings.Contains(chatNameKey(chat.Name), query) {
			continue
		}
		var source string
		for _, row := range rows {
			if row.Chat == chat.ID {
				source = row.Canonical.Chat
				break
			}
		}
		if kind == "group" && !strings.HasSuffix(source, "@g.us") {
			continue
		}
		if kind == "direct" && !strings.HasSuffix(source, "@s.whatsapp.net") && !strings.HasSuffix(source, "@lid") {
			continue
		}
		all = append(all, *chat)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if cursor.LiveOffset > len(all) {
		return ChatPage{}, SavedResponseGuard{}, ErrSavedCursorStale
	}
	end := min(cursor.LiveOffset+limit, len(all))
	page := ChatPage{Chats: append([]ChatView{}, all[cursor.LiveOffset:end]...), Snapshot: s.snapshot()}
	if end < len(all) {
		cursor.LiveOffset = end
		page.NextCursor, err = r.encode(identity, cursor)
		if err != nil {
			return ChatPage{}, SavedResponseGuard{}, err
		}
	}
	guard, err := s.guard()
	if err != nil {
		return ChatPage{}, SavedResponseGuard{}, err
	}
	guard.aggregate = &savedAggregateWitness{digest: savedDigest(rows), members: savedMembers(rows)}
	s.close()
	s.live = nil
	s.history = nil
	if err = r.Check(ctx, guard); err != nil {
		return ChatPage{}, SavedResponseGuard{}, err
	}
	return page, guard, nil
}

func savedMemberID(c savedContributor) string {
	if c.Historical {
		return "h:" + string(c.Metadata.ID)
	}
	return "l:" + string(c.Metadata.ID)
}
func savedMembers(rows []savedContributor) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[savedMemberID(row)] = true
	}
	return out
}

// Aggregates need negative evidence only for possible historical contributors.
// Current metadata remains complete: presence, readability and exact counts are
// never pruned. The same key selection runs again inside the final commit fence.
type savedNegativeKeys struct {
	full       map[[32]byte]bool
	unresolved map[[32]byte]bool
	chats      map[string]bool
	bytes      int
}

func (k *savedNegativeKeys) contains(key SourceKey) bool {
	return k.full[sha256.Sum256([]byte(savedOrderKey(key)))]
}
func savedUnresolvedHash(chat, message string) [32]byte {
	return sha256.Sum256([]byte(chat + "\x00" + message))
}
func savedAggregateNegativeKeys(live, history *readFrame) (*savedNegativeKeys, error) {
	out := &savedNegativeKeys{full: map[[32]byte]bool{}, unresolved: map[[32]byte]bool{}, chats: map[string]bool{}}
	if history == nil {
		return out, nil
	}
	aliases := &savedEvidence{aliases: map[string]string{}}
	rows, err := live.tx.QueryContext(live.ctx, "SELECT alias,canonical FROM identity_aliases WHERE account=? LIMIT ?", live.scope.Account, savedMetadataLimit+1)
	if err != nil {
		return nil, readError(live.ctx, err)
	}
	aliasBytes := 0
	for rows.Next() {
		var a, c string
		if err = rows.Scan(&a, &c); err != nil {
			rows.Close()
			return nil, readError(live.ctx, err)
		}
		aliases.aliases[a] = c
		aliasBytes += 64 + len(a) + len(c)
		if len(aliases.aliases) > savedMetadataLimit || aliasBytes > savedMemoryLimit {
			rows.Close()
			return nil, ErrSavedReadLimit
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, readError(live.ctx, err)
	}
	chats := history.scope.Chats
	if len(history.savedEvidenceChats) > 0 {
		chats = history.savedEvidenceChats
	}
	args := []any{history.scope.Account}
	where := "account=?"
	if len(chats) > 0 {
		marks := make([]string, len(chats))
		for i, chat := range chats {
			marks[i] = "?"
			args = append(args, chat)
		}
		where += " AND chat_id IN (" + strings.Join(marks, ",") + ")"
	}
	args = append(args, savedMetadataLimit+1)
	rows, err = history.tx.QueryContext(history.ctx, "SELECT source_chat,source_sender,source_message,from_me FROM messages WHERE "+where+" LIMIT ?", args...)
	if err != nil {
		return nil, readError(history.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		key := SourceKey{Account: history.scope.Account}
		if err = rows.Scan(&key.Chat, &key.Sender, &key.Message, &key.FromMe); err != nil {
			return nil, readError(history.ctx, err)
		}
		key, err = aliases.canonical(key)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256([]byte(savedOrderKey(key)))
		if out.full[hash] {
			return nil, ErrSourceUnavailable
		}
		out.full[hash] = true
		out.bytes += 64
		unresolved := savedUnresolvedHash(key.Chat, key.Message)
		if !out.unresolved[unresolved] {
			out.unresolved[unresolved] = true
			out.bytes += 64
		}
		if !out.chats[key.Chat] {
			out.chats[key.Chat] = true
			out.bytes += 64 + len(key.Chat)
		}
		if len(out.full) > savedMetadataLimit || out.bytes+aliasBytes > savedMemoryLimit {
			return nil, ErrSavedReadLimit
		}
	}
	return out, readError(history.ctx, rows.Err())
}
