package archive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"mime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxAttachmentLimit        = 20
	maxAttachmentSide         = 3
	maxAttachmentPageBytes    = 128 << 10
	maxAttachmentContextText  = 4096
	maxAttachmentResponsePins = maxAttachmentLimit + 1
)

type attachmentFilter struct {
	terms                 []string
	chats                 []ChatID
	fileType, filenameKey string
	before, after, limit  int
	from, until           time.Time
}

func attachmentRequestFilter(request AttachmentSearchRequest) (attachmentFilter, error) {
	var filter attachmentFilter
	for _, field := range []struct {
		value string
		max   int
	}{{request.Query, 512}, {request.Filename, 255}, {request.FileType, 127}, {request.Cursor, maxCursorBytes}} {
		if len(field.value) > field.max || !utf8.ValidString(field.value) || strings.ContainsRune(field.value, 0) {
			return filter, ErrInvalidQuery
		}
	}
	if len(request.ChatIDs) > 100 || request.Before < 0 || request.Before > maxAttachmentSide || request.After < 0 || request.After > maxAttachmentSide {
		return filter, ErrInvalidQuery
	}
	var err error
	filter.chats, err = canonicalChats(request.ChatIDs)
	if err != nil {
		return filter, err
	}
	filter.limit = request.Limit
	if filter.limit == 0 {
		filter.limit = 10
	}
	if filter.limit < 1 || filter.limit > maxAttachmentLimit {
		return filter, ErrInvalidQuery
	}
	filter.before, filter.after = request.Before, request.After
	filter.from, filter.until = request.From.UTC(), request.Until.UTC()
	filter.filenameKey = chatNameKey(request.Filename)
	if request.Filename != "" && filter.filenameKey == "" {
		return filter, ErrInvalidQuery
	}
	filter.fileType = strings.ToLower(strings.TrimSpace(request.FileType))
	switch filter.fileType {
	case "", "all":
		filter.fileType = "all"
	case "document", "pdf", "image", "voice":
	default:
		canonical, parameters, err := mime.ParseMediaType(filter.fileType)
		if err != nil || len(parameters) != 0 || !strings.Contains(canonical, "/") || strings.Contains(canonical, "*") {
			return filter, ErrInvalidQuery
		}
		filter.fileType = canonical
	}
	if strings.TrimSpace(request.Query) != "" {
		expression, err := literalQuery(request.Query)
		if err != nil {
			return filter, err
		}
		for _, quoted := range strings.Split(expression, " AND ") {
			term := strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
			term = strings.ReplaceAll(term, `""`, `"`)
			key := chatNameKey(term)
			if key == "" {
				return filter, ErrInvalidQuery
			}
			filter.terms = append(filter.terms, key)
		}
		sort.Strings(filter.terms)
		filter.terms = compactAttachmentTerms(filter.terms)
	}
	return filter, nil
}

func compactAttachmentTerms(terms []string) []string {
	out := terms[:0]
	for _, term := range terms {
		if len(out) == 0 || out[len(out)-1] != term {
			out = append(out, term)
		}
	}
	return out
}

const attachmentAssociationJoin = ` JOIN extension_media_associations l ON l.message_id=m.id AND l.account=m.account
 AND l.local_revision=m.local_revision AND l.policy_generation=ap.generation
 AND l.content_hash=m.content_hash AND l.source_expires_at=m.expires_at`
const attachmentReadable = ` AND l.active=1 AND l.view_once=0 AND m.view_once=0
 AND l.kind IN ('document','image','voice') AND m.body_kind=l.kind`
const attachmentMimeBase = `lower(trim(CASE WHEN instr(l.mime_type,';')>0 THEN substr(l.mime_type,1,instr(l.mime_type,';')-1) ELSE l.mime_type END))`

// Reuse the authorization arguments in every correlated subquery. Repeating
// positional bindings for a wide chat grant can exceed SQLite's variable cap.
func attachmentScopeSQL(where string) string {
	var out strings.Builder
	index := 0
	for _, char := range where {
		if char == '?' {
			index++
			out.WriteByte('?')
			out.WriteString(strconv.Itoa(index))
		} else {
			out.WriteRune(char)
		}
	}
	return out.String()
}

func (f *readFrame) attachmentSelectionSQL(filter attachmentFilter, offset int) (string, []any) {
	scopeWhere := attachmentScopeSQL(f.where)
	where := scopeWhere + attachmentReadable
	args := append([]any(nil), f.args...)
	switch filter.fileType {
	case "document", "image", "voice":
		where += " AND l.kind=?"
		args = append(args, filter.fileType)
	case "pdf":
		where += " AND l.kind='document' AND " + attachmentMimeBase + "=?"
		args = append(args, "application/pdf")
	case "all":
	default:
		where += " AND " + attachmentMimeBase + "=?"
		args = append(args, filter.fileType)
	}
	if filter.filenameKey != "" {
		where += " AND instr(wa_indexer_name_key(l.filename),?)>0"
		args = append(args, filter.filenameKey)
	}
	eligibleLimit := ""
	if len(filter.terms) == 0 {
		// Enumeration can select its exact page before doing any context work.
		eligibleLimit = " ORDER BY m.original_sent_at DESC,l.id LIMIT ? OFFSET ?"
		args = append(args, filter.limit+1, offset)
	}
	before, after := filter.before, filter.after
	if len(filter.terms) != 0 {
		before, after = maxAttachmentSide, maxAttachmentSide
	}
	window := func(side int, before bool) string {
		if side == 0 {
			return "'[]'"
		}
		comparison, order := ">", "m.original_sent_at,m.id"
		if before {
			comparison, order = "<", "m.original_sent_at DESC,m.id DESC"
		}
		args = append(args, side)
		return `(SELECT json_group_array(id) FROM (
 SELECT m.id FROM ` + authorizedTables + ` WHERE ` + scopeWhere + ` AND m.chat_id=e.chat_id
 AND (m.original_sent_at,m.id)` + comparison + `(e.sent,e.message_id) ORDER BY ` + order + ` LIMIT ?))`
	}
	beforeSQL, afterSQL := window(before, true), window(after, false)
	scoreParts, requiredParts := []string{}, []string{}
	for i, term := range filter.terms {
		name := "term_" + strconv.Itoa(i)
		// A metadata match avoids even the six-row contextual scoring join.
		part := `CASE WHEN instr(e.filename_key,?)>0 THEN 8
 WHEN instr(e.caption_key,?)>0 THEN 4 WHEN instr(e.metadata_key,?)>0 THEN 1
 ELSE coalesce((SELECT max(CASE WHEN instr(wa_indexer_name_key(substr(m.normalized_body,1,4096)||' '||substr(m.normalized_caption,1,4096)),?)>0 THEN 1 ELSE 0 END)
 FROM ` + authorizedTables + ` JOIN json_each(e.before_ids||'') chosen ON chosen.value=m.id
 WHERE ` + scopeWhere + `),0)+coalesce((SELECT max(CASE WHEN instr(wa_indexer_name_key(substr(m.normalized_body,1,4096)||' '||substr(m.normalized_caption,1,4096)),?)>0 THEN 1 ELSE 0 END)
 FROM ` + authorizedTables + ` JOIN json_each(e.after_ids||'') chosen ON chosen.value=m.id
 WHERE ` + scopeWhere + `),0) END AS ` + name
		scoreParts = append(scoreParts, part)
		requiredParts = append(requiredParts, name+">0")
		args = append(args, term, term, term, term, term)
	}
	scoreColumns, score, required := "", "0", "1"
	if len(scoreParts) != 0 {
		scoreColumns = "," + strings.Join(scoreParts, ",")
		names := make([]string, len(scoreParts))
		for i := range names {
			names[i] = "term_" + strconv.Itoa(i)
		}
		score, required = strings.Join(names, "+"), strings.Join(requiredParts, " AND ")
	}
	finalLimit := ""
	if len(filter.terms) != 0 {
		finalLimit = " LIMIT ? OFFSET ?"
		args = append(args, filter.limit+1, offset)
	}
	keys := "'' AS filename_key,'' AS caption_key,'' AS metadata_key"
	if len(filter.terms) != 0 {
		// Byte slicing plus the name normalizer drops a partial trailing UTF-8
		// rune, matching boundedOriginal's exact display-label byte boundary.
		keys = `wa_indexer_name_key(l.filename) AS filename_key,
 wa_indexer_name_key(substr(m.normalized_body,1,16384)||' '||substr(m.normalized_caption,1,16384)) AS caption_key,
 wa_indexer_name_key(coalesce(CAST(substr(CAST(c.name AS BLOB),1,256) AS TEXT),'')||' '||coalesce(CAST(substr(CAST(i.name AS BLOB),1,256) AS TEXT),'')||' '||` + attachmentMimeBase + `) AS metadata_key`
	}
	query := `WITH eligible AS MATERIALIZED (
 SELECT l.id,m.id AS message_id,m.chat_id,m.original_sent_at AS sent,
 ` + keys + `
 FROM ` + authorizedTables + attachmentAssociationJoin + ` WHERE ` + where + eligibleLimit + `
 ),neighbors AS MATERIALIZED (
 SELECT e.*,` + beforeSQL + ` AS before_ids,` + afterSQL + ` AS after_ids FROM eligible e
 ),scored AS MATERIALIZED (
 SELECT e.*` + scoreColumns + ` FROM neighbors e
 ) SELECT id,before_ids,after_ids,(` + score + `) AS score FROM scored
 WHERE ` + required + ` ORDER BY score DESC,sent DESC,id` + finalLimit
	return query, args
}

func (f *readFrame) attachmentSelections(filter attachmentFilter, offset int) ([]attachmentSelection, error) {
	query, args := f.attachmentSelectionSQL(filter, offset)
	rows, err := f.tx.QueryContext(f.ctx, query, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	selected := make([]attachmentSelection, 0, filter.limit+1)
	for rows.Next() {
		var item attachmentSelection
		var beforeJSON, afterJSON string
		if err = rows.Scan(&item.id, &beforeJSON, &afterJSON, &item.score); err != nil {
			return nil, readError(f.ctx, err)
		}
		if err = json.Unmarshal([]byte(beforeJSON), &item.before); err != nil {
			return nil, ErrSourceUnavailable
		}
		if err = json.Unmarshal([]byte(afterJSON), &item.after); err != nil {
			return nil, ErrSourceUnavailable
		}
		if !validReadID(string(item.id)) || len(item.before) > maxAttachmentSide || len(item.after) > maxAttachmentSide {
			return nil, ErrSourceUnavailable
		}
		selected = append(selected, item)
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	return selected, nil
}

type attachmentSelection struct {
	id            MediaID
	before, after []MessageID // exact scoped IDs that contributed to ranking
	score         float64
}

type attachmentRow struct {
	view   AttachmentView
	source SourceRef
	pin    attachmentResponsePin
}

type attachmentArtifactMetadata struct {
	ID, MediaID, Account, ReservationID, SHA256, Format, State, Budget, CleanupLease string
	Bytes, ChargedBytes, CleanupAck                                                  int64
}

func (f *readFrame) attachmentRows(ids []MediaID, capture bool) (map[MediaID]attachmentRow, error) {
	if len(ids) < 1 || len(ids) > maxAttachmentResponsePins {
		return nil, ErrLimitExceeded
	}
	marks := make([]string, len(ids))
	args := append([]any(nil), f.args...)
	for i, id := range ids {
		if !validReadID(string(id)) {
			return nil, ErrSourceUnavailable
		}
		marks[i], args = "?", append(args, id)
	}
	rows, err := f.tx.QueryContext(f.ctx, `SELECT l.id,m.id,m.chat_id,substr(c.name,1,256),m.sender_id,substr(i.name,1,256),m.original_sent_at,l.kind,
 substr(m.caption,1,16384),m.account,m.local_revision,m.content_hash,m.expires_at,l.filename,l.mime_type,l.declared_bytes,l.availability,l.artifact_id,l.last_original_sha256,
 coalesce(a.id,''),coalesce(a.media_id,''),coalesce(a.account,''),coalesce(a.reservation_id,''),coalesce(a.sha256,''),coalesce(a.bytes,0),coalesce(a.actual_format,''),coalesce(a.state,''),coalesce(a.budget,''),coalesce(a.charged_bytes,0),coalesce(a.cleanup_lease,''),coalesce(a.cleanup_ack,0)
 FROM `+authorizedTables+attachmentAssociationJoin+` LEFT JOIN extension_media_artifacts a ON a.id=l.artifact_id
 WHERE `+f.where+attachmentReadable+` AND l.id IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	result := make(map[MediaID]attachmentRow, len(ids))
	for rows.Next() {
		var row attachmentRow
		var sent, expiry int64
		var rawArtifactID, lastSHA, availability string
		var artifact attachmentArtifactMetadata
		err = rows.Scan(&row.view.AttachmentID, &row.view.MessageID, &row.view.ChatID, &row.view.ChatName, &row.view.SenderID, &row.view.SenderName, &sent, &row.view.Kind,
			&row.view.Caption, &row.source.Account, &row.source.Version.Revision, &row.source.ContentHash, &expiry, &row.view.Filename, &row.view.MIMEType, &row.view.DeclaredBytes, &availability, &rawArtifactID, &lastSHA,
			&artifact.ID, &artifact.MediaID, &artifact.Account, &artifact.ReservationID, &artifact.SHA256, &artifact.Bytes, &artifact.Format, &artifact.State, &artifact.Budget, &artifact.ChargedBytes, &artifact.CleanupLease, &artifact.CleanupAck)
		if err != nil {
			return nil, readError(f.ctx, err)
		}
		if !validReadID(string(row.view.AttachmentID)) || !validReadID(string(row.view.MessageID)) || !utf8.ValidString(row.view.Filename) || len(row.view.Filename) > 255 || strings.ContainsRune(row.view.Filename, 0) || !utf8.ValidString(row.view.MIMEType) || len(row.view.MIMEType) > 256 || strings.ContainsRune(row.view.MIMEType, 0) || row.view.DeclaredBytes < 0 {
			return nil, ErrSourceUnavailable
		}
		row.view.ChatName = boundedOriginal(row.view.ChatName, 256)
		row.view.SenderName = boundedOriginal(row.view.SenderName, 256)
		row.view.Caption = boundedOriginal(row.view.Caption, maxAttachmentContextText)
		row.view.SentAt, row.view.Snapshot = fromMillis(sent), f.snapshot
		row.view.Before, row.view.After = []MessageView{}, []MessageView{}
		row.source.Version.MessageID, row.source.Version.PolicyGeneration = row.view.MessageID, f.snapshot.PolicyGeneration
		row.source.Version.ExpiresAt = fromMillis(expiry)
		row.view.Availability = "not_retained"
		if availability == "unavailable" {
			row.view.Availability = "unavailable"
		}
		if artifact.ID != "" && artifact.State == "retained" && artifact.Budget == string(BudgetOriginals) && artifact.CleanupAck == 0 {
			if artifact.ID != rawArtifactID || artifact.Account != row.source.Account || !validReadID(artifact.ID) || !validReadID(artifact.MediaID) || !validReadID(artifact.ReservationID) || !extensionMediaDigest(artifact.SHA256) || lastSHA != artifact.SHA256 || artifact.Bytes < 1 || artifact.ChargedBytes < artifact.Bytes || artifact.Format == "" || len(artifact.Format) > 32 {
				return nil, ErrSourceUnavailable
			}
			row.view.Availability, row.view.Bytes = "retained", artifact.Bytes
			row.view.original = &MediaArtifact{ID: ArtifactID(artifact.ID), MediaID: row.view.AttachmentID, Source: row.source, SHA256: artifact.SHA256, Bytes: artifact.Bytes,
				ReservationID: ReservationID(artifact.ReservationID), CleanupLease: LeaseID(artifact.CleanupLease), Budget: BudgetOriginals, ReservedBytes: artifact.ChargedBytes, State: artifact.State, ActualFormat: artifact.Format}
		}
		metadata, err := json.Marshal(struct {
			Filename, MIME, Availability, ArtifactID, LastSHA string
			Kind                                              MessageKind
			Declared                                          int64
			Artifact                                          attachmentArtifactMetadata
		}{row.view.Filename, row.view.MIMEType, availability, rawArtifactID, lastSHA, row.view.Kind, row.view.DeclaredBytes, artifact})
		if err != nil {
			return nil, ErrArchiveUnavailable
		}
		row.pin = attachmentResponsePin{ID: row.view.AttachmentID, Source: row.source, MetadataHash: sha256.Sum256(metadata)}
		if capture {
			f.pin(row.view.MessageID, row.source.Version.Revision)
			f.note(row.view.SentAt, row.source.Version.ExpiresAt)
			if err = f.captureSource(SourceReadVersion{Ref: row.source, ChatID: row.view.ChatID, SenderID: row.view.SenderID, OriginalSentAt: row.view.SentAt, ExpiresAt: row.source.Version.ExpiresAt}, row.view.ChatName, row.view.SenderName); err != nil {
				return nil, err
			}
		}
		if _, exists := result[row.view.AttachmentID]; exists {
			return nil, ErrSourceUnavailable
		}
		result[row.view.AttachmentID] = row
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	if len(result) != len(ids) {
		return nil, ErrSourceUnavailable
	}
	return result, nil
}

func (f *readFrame) attachmentNeighbors(center AttachmentView, ids []MessageID, before bool) ([]MessageView, error) {
	if len(ids) == 0 {
		return []MessageView{}, nil
	}
	if len(ids) > maxAttachmentSide {
		return nil, ErrLimitExceeded
	}
	marks, args := make([]string, len(ids)), []any{center.ChatID}
	for i, id := range ids {
		if !validReadID(string(id)) || id == center.MessageID {
			return nil, ErrSourceUnavailable
		}
		marks[i], args = "?", append(args, id)
	}
	rows, err := f.contextRows(" AND m.chat_id=? AND m.id IN ("+strings.Join(marks, ",")+")", args, "m.original_sent_at,m.id", len(ids))
	if err != nil {
		return nil, err
	}
	if len(rows) != len(ids) {
		return nil, ErrSourceUnavailable
	}
	seen := make(map[MessageID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return nil, ErrSourceUnavailable
		}
		seen[id] = true
	}
	views := make([]MessageView, 0, len(rows))
	for _, row := range rows {
		if !seen[row.view.ID] || (before && !(row.view.SentAt.Before(center.SentAt) || (row.view.SentAt.Equal(center.SentAt) && row.view.ID < center.MessageID))) || (!before && !(row.view.SentAt.After(center.SentAt) || (row.view.SentAt.Equal(center.SentAt) && row.view.ID > center.MessageID))) {
			return nil, ErrSourceUnavailable
		}
		f.note(row.view.SentAt, row.expiry)
		row.view.Body = boundedOriginal(row.view.Body, maxAttachmentContextText)
		row.view.Caption = boundedOriginal(row.view.Caption, maxAttachmentContextText)
		views = append(views, row.view)
	}
	return views, nil
}

func (f *readFrame) checkAttachmentPins(pins []attachmentResponsePin, sources map[MessageID]SourceReadVersion) error {
	if len(pins) == 0 {
		return nil
	}
	if len(pins) > maxAttachmentResponsePins {
		return ErrLimitExceeded
	}
	ids := make([]MediaID, len(pins))
	for i, pin := range pins {
		source, exists := sources[pin.Source.Version.MessageID]
		if !exists || source.Ref != pin.Source || pin.MetadataHash == [32]byte{} {
			return ErrSourceUnavailable
		}
		ids[i] = pin.ID
	}
	rows, err := f.attachmentRows(ids, false)
	if err != nil {
		return err
	}
	for _, pin := range pins {
		if current, exists := rows[pin.ID]; !exists || current.pin != pin {
			return ErrSourceUnavailable
		}
	}
	return nil
}

// AttachmentResponseCheck keeps original source and byte identity checks in
// the same credential-fenced transaction as all other response authority.
func (s *Store) AttachmentResponseCheck(ctx context.Context, scope ReadScope, check ResponseCheck) (ReadSnapshot, error) {
	if len(check.Guard.attachments) == 0 {
		return ReadSnapshot{}, ErrSourceUnavailable
	}
	return s.CheckResponse(ctx, scope, check)
}

func (s *Store) SearchAttachmentsGuarded(ctx context.Context, scope ReadScope, request AttachmentSearchRequest) (AttachmentPage, ResponseGuard, error) {
	filter, err := attachmentRequestFilter(request)
	if err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	f, err := s.beginRead(ctx, scope, filter.chats, filter.from, filter.until)
	if err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	defer f.close()
	f.captureSources = true
	horizon, expiry, err := f.cursorHorizon()
	if err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	identity := cursorIdentity("attachments/context-literal-v1", f.scope, struct {
		Terms                []string
		ChatIDs              []ChatID
		FileType, Filename   string
		Before, After, Limit int
		RollingHorizon       time.Time
	}{filter.terms, filter.chats, filter.fileType, filter.filenameKey, filter.before, filter.after, filter.limit, horizon}, f.snapshot, f.fixedFrom, f.until, expiry)
	offset := 0
	if request.Cursor != "" {
		offset, err = decodeCursor(s.cursorKey, request.Cursor, identity)
		if err != nil {
			return AttachmentPage{}, ResponseGuard{}, err
		}
	}
	selected, err := f.attachmentSelections(filter, offset)
	if err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	page := AttachmentPage{Attachments: []AttachmentView{}, Snapshot: f.snapshot}
	pins := make([]attachmentResponsePin, 0, len(selected))
	if len(selected) != 0 {
		ids := make([]MediaID, len(selected))
		for i, item := range selected {
			ids[i] = item.id
		}
		rows, err := f.attachmentRows(ids, true)
		if err != nil {
			return AttachmentPage{}, ResponseGuard{}, err
		}
		for i, item := range selected {
			row := rows[item.id]
			before, err := f.attachmentNeighbors(row.view, item.before, true)
			if err != nil {
				return AttachmentPage{}, ResponseGuard{}, err
			}
			after, err := f.attachmentNeighbors(row.view, item.after, false)
			if err != nil {
				return AttachmentPage{}, ResponseGuard{}, err
			}
			pins = append(pins, row.pin)
			if i >= filter.limit {
				continue
			}
			row.view.Score = item.score
			if len(before) > filter.before {
				before = before[len(before)-filter.before:]
			}
			if len(after) > filter.after {
				after = after[:filter.after]
			}
			row.view.Before, row.view.After = before, after
			page.Attachments = append(page.Attachments, row.view)
		}
	}
	if len(selected) > filter.limit {
		page.NextCursor, err = encodeCursor(s.cursorKey, identity, offset+filter.limit)
		if err != nil {
			return AttachmentPage{}, ResponseGuard{}, err
		}
	}
	if err = boundAttachmentPage(&page); err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	guard, err := f.responseGuard()
	if err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	guard.attachments = pins
	if err = f.finish(); err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	// Source finish alone cannot detect an original evicted without a message
	// revision. Recheck all captured byte metadata under the shared fence.
	if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); err != nil {
		return AttachmentPage{}, ResponseGuard{}, err
	}
	return page, guard, nil
}

func (s *Store) AttachmentGuarded(ctx context.Context, scope ReadScope, id MediaID) (AttachmentView, ResponseGuard, error) {
	if !validReadID(string(id)) {
		return AttachmentView{}, ResponseGuard{}, ErrSourceUnavailable
	}
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return AttachmentView{}, ResponseGuard{}, err
	}
	defer f.close()
	f.captureSources = true
	rows, err := f.attachmentRows([]MediaID{id}, true)
	if err != nil {
		return AttachmentView{}, ResponseGuard{}, err
	}
	row, exists := rows[id]
	if !exists {
		return AttachmentView{}, ResponseGuard{}, ErrSourceUnavailable
	}
	guard, err := f.responseGuard()
	if err != nil {
		return AttachmentView{}, ResponseGuard{}, err
	}
	guard.attachments = []attachmentResponsePin{row.pin}
	if err = f.finish(); err != nil {
		return AttachmentView{}, ResponseGuard{}, err
	}
	if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); err != nil {
		return AttachmentView{}, ResponseGuard{}, err
	}
	return row.view, guard, nil
}

func boundAttachmentPage(page *AttachmentPage) error {
	type textSlot struct {
		pointer *string
		value   string
	}
	var slots []textSlot
	for i := range page.Attachments {
		view := &page.Attachments[i]
		slots = append(slots, textSlot{&view.Caption, view.Caption})
		view.Caption = ""
		for _, neighbors := range [][]MessageView{view.Before, view.After} {
			for j := range neighbors {
				slots = append(slots, textSlot{&neighbors[j].Body, neighbors[j].Body}, textSlot{&neighbors[j].Caption, neighbors[j].Caption})
				neighbors[j].Body, neighbors[j].Caption = "", ""
			}
		}
	}
	metadata, err := json.Marshal(page)
	if err != nil || len(metadata) > maxAttachmentPageBytes {
		return ErrArchiveUnavailable
	}
	remaining := maxAttachmentPageBytes - len(metadata)
	for _, slot := range slots {
		*slot.pointer = reserveJSONText(slot.value, &remaining)
	}
	return nil
}
