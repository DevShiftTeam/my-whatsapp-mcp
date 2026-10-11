package archive

import (
	"context"
	"database/sql"
	"time"
)

const maxStatsBuckets = 90

// StatsRequest narrows the server-owned read scope. Until is exclusive.
type StatsRequest struct {
	From, Until time.Time
	ChatID      ChatID
	Kind        string
}

type StatsActivity struct {
	Date     string `json:"date"`
	Messages int64  `json:"messages"`
}

type StatsKind struct {
	Kind     string `json:"kind"`
	Messages int64  `json:"messages"`
}

type StatsChat struct {
	ChatID        ChatID `json:"chat_id"`
	Name          string `json:"name"`
	Messages      int64  `json:"messages"`
	LastMessageAt int64  `json:"last_message_at"`
}

type StatsMedia struct {
	VoiceMessages    int64 `json:"voice_messages"`
	TranscribedVoice int64 `json:"transcribed_voice"`
	RetainedVoice    int64 `json:"retained_voice"`
}

// StatsSnapshot contains readable messages, never a claim of complete phone
// history. Operational import and worker progress remain in StatusSnapshot.
type StatsSnapshot struct {
	SampledAt        int64           `json:"sampled_at"`
	ArchiveRevision  ArchiveRevision `json:"archive_revision"`
	Messages         int64           `json:"messages"`
	Chats            int64           `json:"chats"`
	Sent             int64           `json:"sent"`
	Received         int64           `json:"received"`
	FirstMessageAt   int64           `json:"first_message_at"`
	LastMessageAt    int64           `json:"last_message_at"`
	ActivityFrom     int64           `json:"activity_from"`
	ActivityUntil    int64           `json:"activity_until"`
	ActivityInterval string          `json:"activity_interval"`
	ActivityStep     int             `json:"activity_step"`
	Daily            []StatsActivity `json:"daily"`
	Kinds            []StatsKind     `json:"kinds"`
	TopChats         []StatsChat     `json:"top_chats"`
	Media            StatsMedia      `json:"media"`
}

func ValidStatsKind(kind string) bool {
	switch kind {
	case "", "all", "text", "image", "voice", "other":
		return true
	default:
		return false
	}
}

func ValidStatsRequest(request StatsRequest) bool {
	return ValidStatsKind(request.Kind) && validReadTime(request.From) && validReadTime(request.Until) &&
		(request.From.IsZero() || request.Until.IsZero() || request.From.Before(request.Until)) &&
		(request.ChatID == "" || validReadID(string(request.ChatID)))
}

const statsKindSQL = `CASE m.body_kind WHEN 'text' THEN 'text' WHEN 'image' THEN 'image' WHEN 'voice' THEN 'voice' ELSE 'other' END`

func (s *Store) Stats(ctx context.Context, scope ReadScope, request StatsRequest) (StatsSnapshot, error) {
	page, _, err := s.StatsGuarded(ctx, scope, request)
	return page, err
}

// StatsGuarded captures all aggregates in one bounded authorized snapshot. The
// aggregate response guard covers every contributor without hydrating bodies.
func (s *Store) StatsGuarded(ctx context.Context, scope ReadScope, request StatsRequest) (StatsSnapshot, ResponseGuard, error) {
	if !ValidStatsRequest(request) {
		return StatsSnapshot{}, ResponseGuard{}, ErrInvalidQuery
	}
	var chats []ChatID
	if request.ChatID != "" {
		chats = []ChatID{request.ChatID}
	}
	f, err := s.beginRead(ctx, scope, chats, request.From, request.Until)
	if err != nil {
		return StatsSnapshot{}, ResponseGuard{}, err
	}
	defer f.close()
	if request.Kind != "" && request.Kind != "all" {
		f.where += " AND " + statsKindSQL + "=?"
		f.args = append(f.args, request.Kind)
	}
	page := StatsSnapshot{SampledAt: f.now.Unix(), ArchiveRevision: f.snapshot.ArchiveRevision,
		Daily: []StatsActivity{}, Kinds: []StatsKind{}, TopChats: []StatsChat{}, ActivityInterval: "day", ActivityStep: 1}
	var first, last, expiry sql.NullInt64
	err = f.tx.QueryRowContext(f.ctx, `SELECT COUNT(*),COUNT(DISTINCT m.chat_id),
	 COALESCE(SUM(m.from_me),0),MIN(m.original_sent_at),MAX(m.original_sent_at),MIN(NULLIF(m.expires_at,0))
	 FROM `+authorizedTables+` WHERE `+f.where, f.args...).Scan(&page.Messages, &page.Chats, &page.Sent, &first, &last, &expiry)
	if err != nil {
		return StatsSnapshot{}, ResponseGuard{}, readError(f.ctx, err)
	}
	page.Received = page.Messages - page.Sent
	if first.Valid {
		firstAt, lastAt := time.UnixMilli(first.Int64).UTC(), time.UnixMilli(last.Int64).UTC()
		page.FirstMessageAt, page.LastMessageAt = firstAt.Unix(), lastAt.Unix()
		f.note(firstAt, fromMillis(expiry.Int64))
		if err = f.statsActivity(&page, firstAt, lastAt); err != nil {
			return StatsSnapshot{}, ResponseGuard{}, err
		}
		if err = f.statsKinds(&page); err != nil {
			return StatsSnapshot{}, ResponseGuard{}, err
		}
		if err = f.statsChats(&page); err != nil {
			return StatsSnapshot{}, ResponseGuard{}, err
		}
		if err = f.statsMedia(&page); err != nil {
			return StatsSnapshot{}, ResponseGuard{}, err
		}
	}
	guard, err := f.responseGuard()
	if err != nil {
		return StatsSnapshot{}, ResponseGuard{}, err
	}
	if err = f.finish(); err != nil {
		return StatsSnapshot{}, ResponseGuard{}, err
	}
	return page, guard, nil
}

func (f *readFrame) statsKinds(page *StatsSnapshot) error {
	rows, err := f.tx.QueryContext(f.ctx, `SELECT `+statsKindSQL+`,COUNT(*) FROM `+authorizedTables+` WHERE `+f.where+` GROUP BY 1 ORDER BY 1`, f.args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind StatsKind
		if err = rows.Scan(&kind.Kind, &kind.Messages); err != nil {
			return readError(f.ctx, err)
		}
		page.Kinds = append(page.Kinds, kind)
	}
	return readError(f.ctx, rows.Err())
}

func (f *readFrame) statsChats(page *StatsSnapshot) error {
	rows, err := f.tx.QueryContext(f.ctx, `SELECT c.id,substr(c.name,1,256),COUNT(*),MAX(m.original_sent_at)
	 FROM `+authorizedTables+` WHERE `+f.where+` GROUP BY c.id ORDER BY COUNT(*) DESC,c.id LIMIT 8`, f.args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var chat StatsChat
		var last int64
		if err = rows.Scan(&chat.ChatID, &chat.Name, &chat.Messages, &last); err != nil {
			return readError(f.ctx, err)
		}
		chat.Name = boundedOriginal(chat.Name, 256)
		chat.LastMessageAt = time.UnixMilli(last).Unix()
		page.TopChats = append(page.TopChats, chat)
	}
	return readError(f.ctx, rows.Err())
}

const statsMediaSourceSQL = `l.message_id=m.id AND l.account=m.account AND l.local_revision=m.local_revision
 AND l.policy_generation=ap.generation AND l.content_hash=m.content_hash AND l.source_expires_at=m.expires_at
 AND l.active=1 AND l.kind='voice' AND l.view_once=0 AND m.view_once=0`

func (f *readFrame) statsMedia(page *StatsSnapshot) error {
	// Successful roots survive cache eviction. Exact single-member authority
	// keeps transcript counts independent of optional encoder configuration.
	transcript := `EXISTS(SELECT 1 FROM extension_media_associations l JOIN extension_roots r ON r.media_id=l.id
	 WHERE ` + statsMediaSourceSQL + ` AND r.kind='transcript' AND r.source_count=1 AND ` + extensionRootMetadataPredicate + ` AND ` + extensionRootMediaPredicate + `
	 AND EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND rs.ordinal=0
	 AND rs.account=m.account AND rs.message_id=m.id AND rs.local_revision=m.local_revision
	 AND rs.policy_generation=ap.generation AND rs.content_hash=m.content_hash AND rs.expires_at=m.expires_at))`
	retained := `EXISTS(SELECT 1 FROM extension_media_associations l JOIN extension_media_artifacts a ON a.id=l.artifact_id
	 WHERE ` + statsMediaSourceSQL + ` AND a.media_id=l.id AND a.account=l.account AND a.message_id=l.message_id
	 AND a.local_revision=l.local_revision AND a.policy_generation=l.policy_generation
	 AND a.content_hash=l.content_hash AND a.source_expires_at=l.source_expires_at
	 AND a.budget='originals' AND a.state='retained' AND a.cleanup_ack=0
	 AND a.bytes>0 AND a.bytes<=a.maximum_bytes AND a.sha256=l.last_original_sha256
	 AND length(a.sha256)=64 AND a.sha256 NOT GLOB '*[^0-9a-f]*'
	 AND a.actual_format IN ('ogg','opus','wav','mp3','flac','m4a'))`
	err := f.tx.QueryRowContext(f.ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN `+transcript+` THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN `+retained+` THEN 1 ELSE 0 END),0)
	 FROM `+authorizedTables+` WHERE `+f.where+` AND m.body_kind='voice'`, f.args...).Scan(&page.Media.VoiceMessages, &page.Media.TranscribedVoice, &page.Media.RetainedVoice)
	return readError(f.ctx, err)
}
