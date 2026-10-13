package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unsafe"
)

// Metadata deliberately excludes body, caption, and generated text.
type savedMetadata struct {
	ID                               MessageID
	Key                              SourceKey
	Chat                             ChatID
	Sender, ChatName, SenderName     string
	Sent, Expiry                     time.Time
	Revision                         uint64
	Protocol                         int64
	Kind                             MessageKind
	SourceKind, Hash, Media, Barrier string
	ViewOnce                         bool
	Transcript, Retained             bool
}

const savedMetadataColumns = `m.id,m.account,m.source_chat,m.source_sender,m.source_message,m.from_me,m.chat_id,m.sender_id,substr(c.name,1,256),substr(i.name,1,256),m.original_sent_at,m.expires_at,m.local_revision,m.protocol_revision,m.body_kind,m.source_kind,m.content_hash,m.extension_media_identity,m.revision_barrier,m.view_once,CASE WHEN m.body_kind='voice' THEN ` + statsTranscriptSQL + ` ELSE 0 END,CASE WHEN m.body_kind='voice' THEN ` + statsRetainedSQL + ` ELSE 0 END`

func scanSavedMetadata(row interface{ Scan(...any) error }) (savedMetadata, error) {
	var m savedMetadata
	var sent, expiry int64
	err := row.Scan(&m.ID, &m.Key.Account, &m.Key.Chat, &m.Key.Sender, &m.Key.Message, &m.Key.FromMe, &m.Chat, &m.Sender, &m.ChatName, &m.SenderName, &sent, &expiry, &m.Revision, &m.Protocol, &m.Kind, &m.SourceKind, &m.Hash, &m.Media, &m.Barrier, &m.ViewOnce, &m.Transcript, &m.Retained)
	m.Sent, m.Expiry = fromMillis(sent), fromMillis(expiry)
	return m, err
}
func savedMetadataByID(f *readFrame, id MessageID) (savedMetadata, error) {
	args := append(append([]any(nil), f.args...), id)
	m, err := scanSavedMetadata(f.tx.QueryRowContext(f.ctx, "SELECT "+savedMetadataColumns+" FROM "+authorizedTables+" WHERE "+f.where+" AND m.id=?", args...))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrSourceUnavailable
	}
	return m, readError(f.ctx, err)
}
func savedMetadataRows(f *readFrame, limit, offset int) ([]savedMetadata, error) {
	args := append([]any(nil), f.args...)
	order := "m.original_sent_at DESC,"
	aliases := f.savedOrderAliases
	if aliases == nil && f.savedEvidence != nil {
		aliases = f.savedEvidence.aliases
	}
	if len(aliases) > maxReadChats {
		return nil, ErrSavedReadLimit
	}
	keys := make([]string, 0, len(aliases))
	for key := range aliases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, column := range []string{"m.source_chat", "m.source_sender"} {
		if len(keys) == 0 {
			order += column + ","
			continue
		}
		var expr strings.Builder
		expr.WriteString("CASE " + column)
		for _, key := range keys {
			expr.WriteString(" WHEN ? THEN ?")
			args = append(args, key, aliases[key])
		}
		expr.WriteString(" ELSE " + column + " END,")
		order += expr.String()
	}
	order += "m.source_message,m.from_me,m.id"
	args = append(args, limit, offset)
	rows, err := f.tx.QueryContext(f.ctx, "SELECT "+savedMetadataColumns+" FROM "+authorizedTables+" WHERE "+f.where+" ORDER BY "+order+" LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	out := make([]savedMetadata, 0, min(limit, 1000))
	bytes := 0
	for rows.Next() {
		m, e := scanSavedMetadata(rows)
		if e != nil {
			return nil, readError(f.ctx, e)
		}
		bytes += savedMetadataBytes(m)
		if bytes > savedMemoryLimit {
			return nil, ErrSavedReadLimit
		}
		out = append(out, m)
	}
	return out, readError(f.ctx, rows.Err())
}
func savedMetadataBytes(m savedMetadata) int {
	return int(unsafe.Sizeof(m)) + len(m.ID) + len(m.Key.Account) + len(m.Key.Chat) + len(m.Key.Sender) + len(m.Key.Message) + len(m.Chat) + len(m.Sender) + len(m.ChatName) + len(m.SenderName) + len(m.Hash) + len(m.Media) + len(m.Barrier)
}
func savedCanonical(ctx context.Context, q sqlQuerier, k SourceKey) (SourceKey, error) {
	out, err := canonicalKey(ctx, q, k)
	if err != nil {
		return out, readError(ctx, err)
	}
	terminal, err := canonicalKey(ctx, q, out)
	if err != nil {
		return out, readError(ctx, err)
	}
	if terminal != out {
		return out, ErrSourceUnavailable
	}
	return out, nil
}
func savedCanonicalSQL(alias, column string) string {
	return "COALESCE((SELECT canonical FROM identity_aliases WHERE account=" + alias + ".account AND alias=" + alias + "." + column + ")," + alias + "." + column + ")"
}
func savedFullKeySQL(alias, chat, sender string) string {
	return alias + ".account=? AND " + savedCanonicalSQL(alias, chat) + "=? AND " + savedCanonicalSQL(alias, sender) + "=? AND " + alias + ".source_message=? AND " + alias + ".from_me=?"
}
func savedKeyArgs(k SourceKey) []any { return []any{k.Account, k.Chat, k.Sender, k.Message, k.FromMe} }

func savedCapture(f *readFrame, m savedMetadata) error {
	f.note(m.Sent, m.Expiry)
	f.pin(m.ID, m.Revision)
	return f.captureSource(SourceReadVersion{Ref: SourceRef{Account: m.Key.Account, Version: SourceVersion{MessageID: m.ID, Revision: m.Revision, PolicyGeneration: f.snapshot.PolicyGeneration, ExpiresAt: m.Expiry}, ContentHash: m.Hash}, ChatID: m.Chat, SenderID: m.Sender, OriginalSentAt: m.Sent, ExpiresAt: m.Expiry}, m.ChatName, m.SenderName)
}
func savedOrderKey(k SourceKey) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t", k.Account, k.Chat, k.Sender, k.Message, k.FromMe)
}

// savedEvidence is built once from one current read transaction. It contains
// only bounded source metadata, and never inserts or repairs source authority.
type savedEvidence struct {
	aliases    map[string]string
	current    map[SourceKey][]savedCurrent
	chats      map[string]ChatID
	chatNames  map[ChatID]string
	deleted    map[SourceKey]bool
	unresolved map[[2]string]bool
	cutoffs    map[string]int64
	anchors    map[SourceKey][]int64
	guards     map[SourceKey][]RebuildGuard
	bytes      int
}
type savedCurrent struct {
	metadata savedMetadata
	readable bool
}

func (e *savedEvidence) canonical(k SourceKey) (SourceKey, error) {
	for _, p := range []*string{&k.Chat, &k.Sender} {
		if v, ok := e.aliases[*p]; ok {
			if next, exists := e.aliases[v]; exists && next != v {
				return k, ErrSourceUnavailable
			}
			*p = v
		}
	}
	return k, nil
}
func savedEvidenceFor(f *readFrame) (*savedEvidence, error) {
	if f.savedEvidence != nil {
		return f.savedEvidence, nil
	}
	e := &savedEvidence{aliases: map[string]string{}, current: map[SourceKey][]savedCurrent{}, chats: map[string]ChatID{}, chatNames: map[ChatID]string{}, deleted: map[SourceKey]bool{}, unresolved: map[[2]string]bool{}, cutoffs: map[string]int64{}, anchors: map[SourceKey][]int64{}, guards: map[SourceKey][]RebuildGuard{}}
	count := 0
	visit := func(query string, args []any, read func(*sql.Rows) error) error {
		localCount := 0
		if len(f.savedEvidenceChats) > 0 {
			negative := false
			for _, table := range []string{"source_tombstones", "unresolved_deletions", "chat_tombstones", "source_anchors", "rebuild_guards"} {
				negative = negative || strings.Contains(query, "FROM "+table+" WHERE")
			}
			if negative {
				wanted := map[string]bool{}
				for source, id := range e.chats {
					for _, chat := range f.savedEvidenceChats {
						if id == chat {
							wanted[source] = true
						}
					}
				}
				forms := map[string]bool{}
				for source := range wanted {
					forms[source] = true
				}
				for alias, target := range e.aliases {
					if wanted[target] {
						forms[alias] = true
					}
				}
				if len(forms) == 0 {
					query = strings.Replace(query, " LIMIT ?", " AND 0 LIMIT ?", 1)
				} else {
					limit := args[len(args)-1]
					args = append([]any(nil), args[:len(args)-1]...)
					marks := make([]string, 0, len(forms))
					for form := range forms {
						marks = append(marks, "?")
						args = append(args, form)
					}
					args = append(args, limit)
					query = strings.Replace(query, " LIMIT ?", " AND chat IN ("+strings.Join(marks, ",")+") LIMIT ?", 1)
				}
			}
		}
		rows, err := f.tx.QueryContext(f.ctx, query, args...)
		if err != nil {
			return readError(f.ctx, err)
		}
		defer rows.Close()
		for rows.Next() {
			localCount++
			count++
			if localCount > savedMetadataLimit || count > savedMetadataLimit*4 || e.bytes > savedMemoryLimit {
				return ErrSavedReadLimit
			}
			if err = read(rows); err != nil {
				return err
			}
		}
		return readError(f.ctx, rows.Err())
	}
	err := visit("SELECT alias,canonical FROM identity_aliases WHERE account=? LIMIT ?", []any{f.scope.Account, savedMetadataLimit + 1}, func(rows *sql.Rows) error {
		var a, c string
		if err := rows.Scan(&a, &c); err != nil {
			return err
		}
		e.aliases[a] = c
		e.bytes += 64 + len(a) + len(c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = visit("SELECT id,source_chat,substr(name,1,256) FROM chats WHERE account=? LIMIT ?", []any{f.scope.Account, maxReadChats + 1}, func(rows *sql.Rows) error {
		var id ChatID
		var chat, name string
		if err := rows.Scan(&id, &chat, &name); err != nil {
			return err
		}
		k, err := e.canonical(SourceKey{Account: f.scope.Account, Chat: chat})
		if err != nil {
			return err
		}
		if other, exists := e.chats[k.Chat]; exists && other != id {
			return ErrSourceUnavailable
		}
		e.chats[k.Chat] = id
		e.chatNames[id] = name
		e.bytes += 96 + len(chat) + len(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	args := append(append([]any(nil), f.args...), f.scope.Account)
	evidenceWhere := "m.account=?"
	if len(f.savedEvidenceChats) > 0 {
		marks := make([]string, len(f.savedEvidenceChats))
		for i, chat := range f.savedEvidenceChats {
			marks[i] = "?"
			args = append(args, chat)
		}
		evidenceWhere += " AND m.chat_id IN (" + strings.Join(marks, ",") + ")"
	}
	args = append(args, savedMetadataLimit+1)
	err = visit("SELECT "+savedMetadataColumns+",CASE WHEN "+f.where+" THEN 1 ELSE 0 END FROM "+authorizedTables+" WHERE "+evidenceWhere+" LIMIT ?", args, func(rows *sql.Rows) error {
		var m savedMetadata
		var sent, expiry int64
		var readable bool
		if err := rows.Scan(&m.ID, &m.Key.Account, &m.Key.Chat, &m.Key.Sender, &m.Key.Message, &m.Key.FromMe, &m.Chat, &m.Sender, &m.ChatName, &m.SenderName, &sent, &expiry, &m.Revision, &m.Protocol, &m.Kind, &m.SourceKind, &m.Hash, &m.Media, &m.Barrier, &m.ViewOnce, &m.Transcript, &m.Retained, &readable); err != nil {
			return err
		}
		m.Sent, m.Expiry = fromMillis(sent), fromMillis(expiry)
		k, err := e.canonical(m.Key)
		if err != nil {
			return err
		}
		e.current[k] = append(e.current[k], savedCurrent{m, readable})
		e.bytes += savedMetadataBytes(m) + 160 + len(k.Chat) + len(k.Sender)
		if len(e.current) > savedMetadataLimit {
			return ErrSavedReadLimit
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = visit("SELECT chat,sender,message,from_me FROM source_tombstones WHERE account=? LIMIT ?", []any{f.scope.Account, savedMetadataLimit + 1}, func(rows *sql.Rows) error {
		k := SourceKey{Account: f.scope.Account}
		if err := rows.Scan(&k.Chat, &k.Sender, &k.Message, &k.FromMe); err != nil {
			return err
		}
		k, err := e.canonical(k)
		if err != nil {
			return err
		}
		e.deleted[k] = true
		e.bytes += 128 + len(k.Chat) + len(k.Sender) + len(k.Message)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = visit("SELECT chat,message FROM unresolved_deletions WHERE account=? LIMIT ?", []any{f.scope.Account, savedMetadataLimit + 1}, func(rows *sql.Rows) error {
		var chat, message string
		if err := rows.Scan(&chat, &message); err != nil {
			return err
		}
		k, err := e.canonical(SourceKey{Chat: chat})
		if err != nil {
			return err
		}
		e.unresolved[[2]string{k.Chat, message}] = true
		e.bytes += 96 + len(chat) + len(message)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = visit("SELECT chat,cutoff FROM chat_tombstones WHERE account=? LIMIT ?", []any{f.scope.Account, maxReadChats + 1}, func(rows *sql.Rows) error {
		var chat string
		var cutoff int64
		if err := rows.Scan(&chat, &cutoff); err != nil {
			return err
		}
		k, err := e.canonical(SourceKey{Chat: chat})
		if err != nil {
			return err
		}
		e.cutoffs[k.Chat] = max(e.cutoffs[k.Chat], cutoff)
		e.bytes += 64 + len(chat)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = visit("SELECT chat,sender,message,from_me,original_sent_at FROM source_anchors WHERE account=? AND date_known=1 LIMIT ?", []any{f.scope.Account, savedMetadataLimit + 1}, func(rows *sql.Rows) error {
		k := SourceKey{Account: f.scope.Account}
		var sent int64
		if err := rows.Scan(&k.Chat, &k.Sender, &k.Message, &k.FromMe, &sent); err != nil {
			return err
		}
		k, err := e.canonical(k)
		if err != nil {
			return err
		}
		e.anchors[k] = append(e.anchors[k], sent)
		e.bytes += 128 + len(k.Chat) + len(k.Sender) + len(k.Message)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var exists bool
	if err = f.tx.QueryRowContext(f.ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='rebuild_guards')").Scan(&exists); err != nil {
		return nil, readError(f.ctx, err)
	}
	if exists {
		err = visit("SELECT chat,sender,message,from_me,original_sent_at,date_known,expires_at,view_once,protocol_revision,revision_barrier,source_kind,deleted FROM rebuild_guards WHERE account=? LIMIT ?", []any{f.scope.Account, savedMetadataLimit + 1}, func(rows *sql.Rows) error {
			k := SourceKey{Account: f.scope.Account}
			var g RebuildGuard
			var sent, expiry int64
			if err := rows.Scan(&k.Chat, &k.Sender, &k.Message, &k.FromMe, &sent, &g.OriginalDateKnown, &expiry, &g.ViewOnce, &g.ProtocolRevision, &g.RevisionBarrier, &g.SourceKind, &g.Deleted); err != nil {
				return err
			}
			k, err := e.canonical(k)
			if err != nil {
				return err
			}
			g.OriginalSentAt, g.ExpiresAt = fromMillis(sent), fromMillis(expiry)
			e.guards[k] = append(e.guards[k], g)
			e.bytes += 224 + len(k.Chat) + len(k.Sender) + len(k.Message) + len(g.RevisionBarrier)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if e.bytes > savedMemoryLimit {
		return nil, ErrSavedReadLimit
	}
	f.savedEvidence = e
	return e, nil
}
func (r *savedOwnerRead) eligible(f *readFrame, m savedMetadata, equivalent bool) (savedHistoricalPin, bool, error) {
	p := savedHistoricalPin{metadata: m, equivalent: equivalent, expiry: m.Expiry}
	e, err := savedEvidenceFor(f)
	if err != nil {
		return p, false, err
	}
	k, err := e.canonical(m.Key)
	if err != nil {
		return p, false, err
	}
	p.canonical = k
	chat, exists := e.chats[k.Chat]
	if !exists {
		return p, false, nil
	}
	p.chat = chat
	if !r.scope.AllChats {
		found := false
		for _, id := range r.scope.Chats {
			found = found || id == chat
		}
		if !found {
			return p, false, nil
		}
	}
	current := e.current[k]
	if len(current) > 1 {
		return p, false, ErrSourceUnavailable
	}
	if len(current) == 1 {
		if !equivalent {
			return p, false, nil
		}
		c := current[0]
		v := c.metadata
		if !c.readable || v.ViewOnce || v.Barrier != "" || v.Protocol != m.Protocol || v.SourceKind != m.SourceKind || v.Hash == "" || v.Hash != m.Hash || v.Media == "" || v.Media != m.Media || !v.Sent.Equal(m.Sent) {
			return p, false, nil
		}
		p.expiry = earlier(p.expiry, v.Expiry)
		p.current = v.ID
		if f.captureSources {
			if err = savedCapture(f, v); err != nil {
				return p, false, err
			}
		}
	}
	if m.ViewOnce || m.Barrier != "" || e.deleted[k] || e.unresolved[[2]string{k.Chat, k.Message}] {
		return p, false, nil
	}
	if cutoff, ok := e.cutoffs[k.Chat]; ok && unixms(m.Sent) <= cutoff {
		return p, false, nil
	}
	for _, sent := range e.anchors[k] {
		if sent != unixms(m.Sent) {
			return p, false, nil
		}
	}
	for _, g := range e.guards[k] {
		if g.Deleted || g.ViewOnce || g.RevisionBarrier != "" || g.ProtocolRevision > m.Protocol || g.OriginalDateKnown && !g.OriginalSentAt.Equal(m.Sent) {
			return p, false, nil
		}
		if g.ProtocolRevision == m.Protocol && g.SourceKind != "" && g.SourceKind != SourceEventKind(m.SourceKind) {
			return p, false, nil
		}
		p.expiry = earlier(p.expiry, g.ExpiresAt)
	}
	if !p.expiry.IsZero() && !f.now.Before(p.expiry) {
		return p, false, nil
	}
	return p, true, nil
}
