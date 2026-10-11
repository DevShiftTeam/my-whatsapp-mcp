//go:build sqlite_fts5

package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func attachmentFixture(t *testing.T, s *Store, p ArchivePolicy, id, chat, filename, caption string, sent time.Time) (SourceRef, MediaID) {
	t.Helper()
	e := readerEvent(id, chat, "", sent)
	e.BodyKind = MessageImage
	e.Caption = caption
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic authenticated image"), MimeType: "image/png", DeclaredBytes: 64, Availability: "not_retained"}}
	message := readerApply(t, s, p, e)
	source, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	var media MediaID
	if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", message).Scan(&media); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET filename=? WHERE id=?", filename, media); err != nil {
		t.Fatal(err)
	}
	return source, media
}

func TestAttachmentSearchScopesFilenameAndPinsRankingNeighbors(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	before := readerApply(t, s, p, readerEvent("before", "allowed", "הצעת מחיר Project42", now.Add(-3*time.Minute)))
	source, media := attachmentFixture(t, s, p, "proposal", "allowed", "הַצָּעָה Project-42.png", "signed proposal", now.Add(-2*time.Minute))
	after := readerApply(t, s, p, readerEvent("after", "allowed", "please review it", now.Add(-time.Minute)))
	_, denied := attachmentFixture(t, s, p, "private", "denied", "הצעה Project42.png", "private canary", now)
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	page, guard, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "מחיר signed", Filename: "הצעה project42", FileType: "image", Limit: 1})
	if err != nil || len(page.Attachments) != 1 || page.Attachments[0].AttachmentID != media {
		t.Fatalf("scoped contextual filename search: %#v %v", page, err)
	}
	if len(page.Attachments[0].Before) != 0 || len(page.Attachments[0].After) != 0 {
		t.Fatal("unrequested neighbors returned")
	}
	seen := make(map[MessageID]bool)
	for _, pin := range guard.Sources {
		seen[pin.Ref.Version.MessageID] = true
	}
	if !seen[before] || !seen[after] || !seen[source.Version.MessageID] || len(seen) != 3 {
		t.Fatalf("rank contributors were not all pinned: %#v", seen)
	}
	if _, _, err = s.AttachmentGuarded(context.Background(), scope, denied); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("denied attachment guessed: %v", err)
	}
	if _, err = s.AttachmentResponseCheck(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentContextSelectionSQLExecutesDirectly(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	readerApply(t, s, p, readerEvent("before", "chat", "contextcanary", now.Add(-2*time.Minute)))
	source, media := attachmentFixture(t, s, p, "file", "chat", "file.png", "captioncanary", now.Add(-time.Minute))
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	filter, err := attachmentRequestFilter(AttachmentSearchRequest{Query: "contextcanary captioncanary"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := s.beginRead(context.Background(), scope, nil, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer frame.close()
	query, args := frame.attachmentSelectionSQL(filter, 0)
	rows, err := frame.tx.QueryContext(frame.ctx, query, args...)
	if err != nil {
		t.Fatalf("synthetic attachment selection SQL: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("synthetic contextual attachment missing: %v", rows.Err())
	}
	var id MediaID
	var before, after string
	var score float64
	if err = rows.Scan(&id, &before, &after, &score); err != nil || id != media || score <= 0 {
		t.Fatalf("synthetic contextual attachment selection id=%q score=%v err=%v", id, score, err)
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("synthetic attachment selection completion: %v", rows.Err())
	}
}

func TestAttachmentSearchNormalizesEmptyDisplayLabelsAsText(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	source, media := attachmentFixture(t, s, p, "empty-labels", "chat", "matching-file.png", "matching caption", now)
	chat := readerChat(t, s, source.Version.MessageID)
	if _, err := s.writeDB.Exec("UPDATE chats SET name='' WHERE id=?", chat); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("UPDATE identities SET name='' WHERE account='account'"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"matching", "image/png", "missing"} {
		page, _, err := s.SearchAttachmentsGuarded(context.Background(), readerScope(p, chat), AttachmentSearchRequest{Query: query})
		if err != nil {
			t.Fatalf("empty display labels rejected query %q: %v", query, err)
		}
		if query == "missing" {
			if len(page.Attachments) != 0 {
				t.Fatal("unmatched empty-label query returned a candidate")
			}
		} else if len(page.Attachments) != 1 || page.Attachments[0].AttachmentID != media {
			t.Fatalf("empty labels lost valid metadata match %q: %#v", query, page)
		}
	}
}

func TestAttachmentContextMatchingUsesNearestScopedThree(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	readerApply(t, s, p, readerEvent("distant", "chat", "distantcanary", now.Add(-5*time.Minute)))
	for i := 3; i > 0; i-- {
		readerApply(t, s, p, readerEvent(fmt.Sprintf("near-%d", i), "chat", "nearby", now.Add(-time.Duration(i+1)*time.Minute)))
	}
	source, media := attachmentFixture(t, s, p, "file", "chat", "file.png", "", now.Add(-time.Minute))
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "distantcanary", Before: 3})
	if err != nil || len(page.Attachments) != 0 {
		t.Fatalf("distant matching message influenced context: %#v %v", page, err)
	}
	page, _, err = s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "nearby", Before: 2})
	if err != nil || len(page.Attachments) != 1 || page.Attachments[0].AttachmentID != media || len(page.Attachments[0].Before) != 2 {
		t.Fatalf("nearest context: %#v %v", page, err)
	}
	if !page.Attachments[0].Before[0].SentAt.Before(page.Attachments[0].Before[1].SentAt) {
		t.Fatal("before messages are not chronological")
	}
}

func TestAttachmentGuardBindsRetainedArtifactAndSafeMetadata(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	source, media := attachmentFixture(t, s, p, "retained", "chat", "report.png", "report", now.Add(-time.Minute))
	extensionPublish(t, s, extensionAdmit(t, s, source, media, BudgetOriginals, 64), 32)
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	view, guard, err := s.AttachmentGuarded(context.Background(), scope, media)
	if err != nil || view.Availability != "retained" || view.Bytes != 32 || view.Snapshot.PolicyGeneration != p.Generation {
		t.Fatalf("retained metadata %#v %v", view, err)
	}
	artifact, retained := view.OriginalArtifact()
	if !retained || artifact.MediaID != media || artifact.Source != source || artifact.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("original authority %#v %v", artifact, retained)
	}
	wire, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"authenticated", "private-sender@", string(artifact.ID), artifact.SHA256, "ProtocolReference", "ReservationID"} {
		if strings.Contains(string(wire), private) {
			t.Fatalf("private original field leaked: %q", private)
		}
	}
	if _, err = s.AttachmentResponseCheck(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET state='evicting' WHERE id=?", artifact.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AttachmentResponseCheck(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("changed artifact state escaped guard: %v", err)
	}
}

func TestAttachmentGuardRevokesRankOnlyNeighborAndFilenameChanges(t *testing.T) {
	for _, change := range []string{"neighbor", "filename"} {
		t.Run(change, func(t *testing.T) {
			now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("neighbor", "chat", "specific context", now.Add(-2*time.Minute))
			readerApply(t, s, p, e)
			source, media := attachmentFixture(t, s, p, "file", "chat", "file.png", "", now.Add(-time.Minute))
			scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
			_, guard, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "specific"})
			if err != nil {
				t.Fatal(err)
			}
			if change == "neighbor" {
				e.Kind, e.Body, e.SourceRevision = EventDelete, "", 2
				readerApply(t, s, p, e)
			} else if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET filename='changed.png' WHERE id=?", media); err != nil {
				t.Fatal(err)
			}
			if _, err = s.AttachmentResponseCheck(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("revoked %s contributor escaped: %v", change, err)
			}
		})
	}
}

func TestAttachmentCursorBindsAllFiltersAndSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var source SourceRef
	for i := 0; i < 3; i++ {
		source, _ = attachmentFixture(t, s, p, fmt.Sprintf("file-%d", i), "chat", "report.png", "report", now.Add(-time.Duration(i+1)*time.Minute))
	}
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	req := AttachmentSearchRequest{Query: "report", Filename: "report", FileType: "image", Limit: 1, Before: 1}
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, req)
	if err != nil || len(page.Attachments) != 1 || page.NextCursor == "" {
		t.Fatalf("first page %#v %v", page, err)
	}
	req.Cursor = page.NextCursor
	second, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, req)
	if err != nil || len(second.Attachments) != 1 || second.Attachments[0].AttachmentID == page.Attachments[0].AttachmentID {
		t.Fatalf("second page %#v %v", second, err)
	}
	mutations := []func(*AttachmentSearchRequest){
		func(r *AttachmentSearchRequest) { r.Query = "other" },
		func(r *AttachmentSearchRequest) { r.Filename = "other" },
		func(r *AttachmentSearchRequest) { r.FileType = "voice" },
		func(r *AttachmentSearchRequest) { r.Limit = 2 },
		func(r *AttachmentSearchRequest) { r.Before = 0 },
		func(r *AttachmentSearchRequest) { r.After = 1 },
		func(r *AttachmentSearchRequest) { r.From = now.Add(-time.Hour) },
		func(r *AttachmentSearchRequest) { r.Until = now.Add(time.Hour) },
		func(r *AttachmentSearchRequest) { r.ChatIDs = []ChatID{"other"} },
	}
	for i, mutate := range mutations {
		changed := req
		mutate(&changed)
		if _, _, err = s.SearchAttachmentsGuarded(context.Background(), scope, changed); !errors.Is(err, ErrCursorStale) {
			t.Fatalf("cursor mutation %d accepted: %v", i, err)
		}
	}
	readerApply(t, s, p, readerEvent("new", "chat", "unrelated", now))
	if _, _, err = s.SearchAttachmentsGuarded(context.Background(), scope, req); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("snapshot change accepted old cursor: %v", err)
	}
}

func TestAttachmentSearchRejectsInvalidInputsAndMimeOperators(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	source, _ := attachmentFixture(t, s, p, "file", "chat", "report.png", "report", now)
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	for _, request := range []AttachmentSearchRequest{
		{Query: strings.Repeat("a", 513)}, {Query: string([]byte{0xff})}, {Query: "\x00"},
		{Filename: strings.Repeat("a", 256)}, {Filename: "\x00"},
		{FileType: "application/pdf; charset=utf-8"}, {FileType: "image/*"}, {FileType: "' OR 1=1 --"},
		{Before: 4}, {After: -1}, {Limit: 21}, {Limit: -1},
		{ChatIDs: make([]ChatID, 101)}, {Cursor: strings.Repeat("a", 1025)},
	} {
		if _, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, request); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid input accepted %#v: %v", request, err)
		}
	}
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{FileType: "IMAGE/PNG"})
	if err != nil || len(page.Attachments) != 1 {
		t.Fatalf("literal canonical MIME filter failed: %#v %v", page, err)
	}
}

func TestAttachmentSearchBoundsEscapedContextPage(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var source SourceRef
	for i := 0; i < 24; i++ {
		readerApply(t, s, p, readerEvent(fmt.Sprintf("context-%d", i), "chat", strings.Repeat("\u0001", 16000), now.Add(-time.Duration(2*i+2)*time.Minute)))
		source, _ = attachmentFixture(t, s, p, fmt.Sprintf("file-%d", i), "chat", "file.png", strings.Repeat("\u0001", 16000), now.Add(-time.Duration(2*i+1)*time.Minute))
	}
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), readerScope(p, readerChat(t, s, source.Version.MessageID)), AttachmentSearchRequest{Before: 3, After: 3, Limit: 20})
	if err != nil || len(page.Attachments) != 20 || page.NextCursor == "" {
		t.Fatalf("bounded page %#v %v", page, err)
	}
	wire, err := json.Marshal(page)
	if err != nil || len(wire) > 128<<10 {
		t.Fatalf("escaped attachment page bytes=%d err=%v", len(wire), err)
	}
}

func TestAttachmentSearchDateExpiryViewOnceAndSourceDeletion(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	readerApply(t, s, p, readerEvent("old-context", "chat", "outsidewindow", now.Add(-time.Hour)))
	source, media := attachmentFixture(t, s, p, "current", "chat", "current.png", "current", now.Add(-time.Minute))
	_, viewOnce := attachmentFixture(t, s, p, "view-once", "chat", "secret.png", "secret", now)
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET view_once=1 WHERE id=?", viewOnce); err != nil {
		t.Fatal(err)
	}
	expiredSource, expired := attachmentFixture(t, s, p, "expired", "chat", "expired.png", "expired", now.Add(-2*time.Minute))
	if _, err := s.writeDB.Exec("UPDATE messages SET expires_at=? WHERE id=?", unixms(now), expiredSource.Version.MessageID); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "outsidewindow", From: now.Add(-5 * time.Minute), Before: 3})
	if err != nil || len(page.Attachments) != 0 {
		t.Fatalf("request date boundary exposed older context: %#v %v", page, err)
	}
	for _, id := range []MediaID{viewOnce, expired} {
		if _, _, err = s.AttachmentGuarded(context.Background(), scope, id); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("view-once or expired media allowed: %v", err)
		}
	}
	view, _, err := s.AttachmentGuarded(context.Background(), scope, media)
	if err != nil || view.Availability != "not_retained" {
		t.Fatalf("unretained metadata unavailable: %#v %v", view, err)
	}
	if _, retained := view.OriginalArtifact(); retained {
		t.Fatal("metadata-only attachment manufactured original authority")
	}
	e := readerEvent("current", "chat", "", now.Add(-time.Minute))
	e.Kind, e.SourceRevision = EventDelete, 2
	readerApply(t, s, p, e)
	if _, _, err = s.AttachmentGuarded(context.Background(), scope, media); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("deleted attachment allowed: %v", err)
	}
}

func TestAttachmentSearchFiltersDeniedCandidatesBeforeLimit(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	for i := 0; i < 24; i++ {
		attachmentFixture(t, s, p, fmt.Sprintf("denied-%d", i), "denied", "matching.png", "matching", now.Add(time.Duration(i)*time.Second))
	}
	source, media := attachmentFixture(t, s, p, "allowed", "allowed", "matching.png", "matching", now.Add(-time.Hour))
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), readerScope(p, readerChat(t, s, source.Version.MessageID)), AttachmentSearchRequest{Query: "matching", Limit: 1})
	if err != nil || len(page.Attachments) != 1 || page.Attachments[0].AttachmentID != media || page.NextCursor != "" {
		t.Fatalf("denied candidates consumed page: %#v %v", page, err)
	}
}

func TestAttachmentSearchReusesWideScopeBindings(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	terms := make([]string, 32)
	for i := range terms {
		terms[i] = fmt.Sprintf("word%d", i)
	}
	query := strings.Join(terms, " ")
	source, media := attachmentFixture(t, s, p, "wide-grant", "allowed", "file.png", query, now)
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	for i := 0; i < 600; i++ {
		scope.Chats = append(scope.Chats, ChatID(fmt.Sprintf("granted-chat-%04d", i)))
	}
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: query})
	if err != nil || len(page.Attachments) != 1 || page.Attachments[0].AttachmentID != media {
		t.Fatalf("wide grant exceeded SQL bindings: %#v %v", page, err)
	}
}

func TestAttachmentNameRankingUsesPinnedUTF8ByteBoundary(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	source, media := attachmentFixture(t, s, p, "name-boundary", "chat", "file.png", "", now)
	chat := readerChat(t, s, source.Version.MessageID)
	name := strings.Repeat("א", 128) + "suffixcanary"
	if _, err := s.writeDB.Exec("UPDATE chats SET name=? WHERE id=?", name, chat); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, chat)
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "suffixcanary"})
	if err != nil || len(page.Attachments) != 0 {
		t.Fatalf("unhashed Unicode suffix influenced rank: %#v %v", page, err)
	}
	if _, _, err = s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "א"}); !errors.Is(err, ErrArchiveUnavailable) && !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("malformed oversized source label escaped its existing guard: %v", err)
	}
	name = strings.Repeat("א", 122) + "labelcanary"
	if _, err = s.writeDB.Exec("UPDATE chats SET name=? WHERE id=?", name, chat); err != nil {
		t.Fatal(err)
	}
	page, guard, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Query: "labelcanary"})
	if err != nil || len(page.Attachments) != 1 || page.Attachments[0].AttachmentID != media || page.Attachments[0].ChatName != name {
		t.Fatalf("valid byte-bounded Unicode label unavailable: %#v %v", page, err)
	}
	if _, err = s.writeDB.Exec("UPDATE chats SET name=? WHERE id=?", strings.Repeat("א", 122)+"changedname", chat); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AttachmentResponseCheck(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("changed Unicode label escaped its metadata pin: %v", err)
	}
}

func TestAttachmentListingSelectsPageBeforeTextNormalization(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var source SourceRef
	for i := 0; i < 80; i++ {
		source, _ = attachmentFixture(t, s, p, fmt.Sprintf("list-%d", i), "chat", "file.png", strings.Repeat("large caption ", 300), now.Add(-time.Duration(i)*time.Minute))
	}
	s.readDB.SetMaxOpenConns(1)
	connection, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	err = connection.Raw(func(raw any) error {
		return raw.(*sqlite3.SQLiteConn).RegisterFunc("wa_indexer_name_key", func(value string) string {
			calls.Add(1)
			return chatNameKey(value)
		}, true)
	})
	if closeErr := connection.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := s.SearchAttachmentsGuarded(context.Background(), readerScope(p, readerChat(t, s, source.Version.MessageID)), AttachmentSearchRequest{Before: 3, After: 3, Limit: 2})
	if err != nil || len(page.Attachments) != 2 || page.NextCursor == "" || len(guard.Sources) > 21 {
		t.Fatalf("enumeration work was not page bounded: %#v sources=%d %v", page, len(guard.Sources), err)
	}
	if calls.Load() != 0 {
		t.Fatalf("queryless listing normalized %d corpus fields", calls.Load())
	}
}

func TestAttachmentCursorSentinelRemainsGuarded(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	attachmentFixture(t, s, p, "first", "chat", "report.png", "report", now)
	_, sentinel := attachmentFixture(t, s, p, "sentinel", "chat", "report.png", "report", now.Add(-time.Minute))
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	page, guard, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Limit: 1})
	if err != nil || page.NextCursor == "" || len(page.Attachments) != 1 || len(guard.attachments) != 2 {
		t.Fatalf("sentinel not captured: %#v pins=%d %v", page, len(guard.attachments), err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET filename='changed.png' WHERE id=?", sentinel); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AttachmentResponseCheck(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("changed pagination sentinel escaped final guard: %v", err)
	}
}

func TestAttachmentSearchDocumentTypeAndMimeBase(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	for _, input := range []struct {
		id, filename, mime string
	}{{"pdf", "הצעת מחיר.pdf", "APPLICATION/PDF; charset=binary"}, {"sheet", "budget.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}} {
		e := readerEvent(input.id, "chat", "", now)
		e.BodyKind = MessageDocument
		e.Caption = "review the attached proposal"
		e.Media = []MediaReference{{Kind: string(MessageDocument), Filename: input.filename, MimeType: input.mime, DeclaredBytes: 8 << 20, Availability: "unavailable"}}
		readerApply(t, s, p, e)
	}
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	for _, input := range []struct {
		fileType string
		count    int
	}{{"document", 2}, {"pdf", 1}, {"application/pdf", 1}, {"image", 0}, {"voice", 0}, {"all", 2}} {
		page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{FileType: input.fileType})
		if err != nil || len(page.Attachments) != input.count {
			t.Fatalf("type %q count=%d want=%d err=%v", input.fileType, len(page.Attachments), input.count, err)
		}
	}
	page, _, err := s.SearchAttachmentsGuarded(context.Background(), scope, AttachmentSearchRequest{Filename: "הצעתמחיר", FileType: "pdf"})
	if err != nil || len(page.Attachments) != 1 || page.Attachments[0].Filename != "הצעת מחיר.pdf" || page.Attachments[0].Availability != "unavailable" {
		t.Fatalf("general document metadata missing: %#v %v", page, err)
	}
}
