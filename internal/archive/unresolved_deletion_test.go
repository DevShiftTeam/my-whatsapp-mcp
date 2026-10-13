//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func unresolvedScope(p ArchivePolicy) ReadScope {
	return ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
}

func unresolvedSearchCount(t *testing.T, s *Store, p ArchivePolicy, query string, count int) {
	t.Helper()
	page, err := s.Search(context.Background(), unresolvedScope(p), SearchRequest{Query: query})
	if err != nil || len(page.Hits) != count {
		t.Fatalf("withholding search count got=%d want=%d err=%v", len(page.Hits), count, err)
	}
}

func TestUnresolvedDeletionGuardsAllAuthorsAndDirectionsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	const chat, message = "300@g.us", "senderless-target"
	other := readerEvent(message, "400@g.us", "unresolvedcrosschatreceipt שלום", now.Add(-time.Minute))
	otherID := readerApply(t, s, p, other)
	if err := s.RecordUnresolvedDeletion(ctx, "account", chat, message); err != nil {
		t.Fatal(err)
	}
	var before, after ArchiveRevision
	if err := s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUnresolvedDeletion(ctx, "account", chat, message); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&after); err != nil || after != before {
		t.Fatalf("duplicate guard rewrote the archive: before=%d after=%d err=%v", before, after, err)
	}
	// The unknown target must not create an invented sender, source row,
	// original date, or backfill anchor.
	var sources, anchors int
	if err := s.readDB.QueryRow("SELECT count(*) FROM messages WHERE account=? AND source_chat=? AND source_message=?", "account", chat, message).Scan(&sources); err != nil || sources != 0 {
		t.Fatalf("senderless control fabricated a source row: count=%d err=%v", sources, err)
	}
	if err := s.readDB.QueryRow("SELECT count(*) FROM source_anchors WHERE account=? AND chat=? AND message=?", "account", chat, message).Scan(&anchors); err != nil || anchors != 0 {
		t.Fatalf("senderless control fabricated an anchor: count=%d err=%v", anchors, err)
	}
	targets, err := s.MutationTargets(ctx, "account", chat, message)
	if err != nil || len(targets) != 0 {
		t.Fatalf("senderless control fabricated mutation targets: count=%d err=%v", len(targets), err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, sender := range []string{"200@s.whatsapp.net", "777@s.whatsapp.net", "999@s.whatsapp.net"} {
		for _, fromMe := range []bool{false, true} {
			for _, kind := range []SourceEventKind{EventOriginal, EventEditContent} {
				e := readerEvent(message, chat, "unresolvedforbiddenreceipt", now.Add(-time.Minute))
				e.Key.Sender, e.Key.FromMe, e.Kind, e.SourceRevision = sender, fromMe, kind, 9000
				e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("private synthetic protocol reference"), MimeType: "image/png", Availability: "not_retained"}}
				result, err := s.ApplySourceEvent(ctx, e, p.Generation)
				if err != nil || result.Outcome != OutcomeStale || result.MessageID != "" || !result.Version.Deleted || len(result.MediaIDs) != 0 {
					t.Fatalf("guard failed an author/direction/revision replay: sender=%s fromMe=%t kind=%s result=%#v err=%v", sender, fromMe, kind, result, err)
				}
			}
		}
	}
	unresolvedSearchCount(t, s, p, "unresolvedforbiddenreceipt", 0)
	unresolvedSearchCount(t, s, p, "שלום", 1)
	page, err := s.Context(ctx, unresolvedScope(p), ContextRequest{MessageID: otherID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != otherID {
		t.Fatalf("same protocol ID in another chat was withheld: count=%d err=%v", len(page.Messages), err)
	}
	status, err := s.Status(ctx, unresolvedScope(p))
	if err != nil || status.Messages != 1 || status.Paused || status.ReconciliationRequired {
		t.Fatalf("persistent withholding blocked unrelated reads: %#v %v", status, err)
	}
	found := false
	for _, code := range status.Codes {
		found = found || code == "unresolved_deletion"
	}
	if !found {
		t.Fatal("durable senderless omission has no visible gap code")
	}
}

func TestUnresolvedDeletionPurgesExistingDerivedMediaAndOldResponses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("existing-senderless-target", "300@g.us", "unresolvedoriginalreceipt שלום", now.Add(-time.Minute))
	e.Caption, e.BodyKind = "unresolvedoriginalcaptionreceipt", MessageImage
	id := readerApply(t, s, p, e)
	other := e
	other.Key.Sender, other.Key.FromMe = "777@s.whatsapp.net", true
	other.Body = "unresolvedotherdirectionreceipt"
	otherID := readerApply(t, s, p, other)
	generation := candidateIndex(t, s)
	for _, kind := range []DocumentKind{DocumentOCR, DocumentCaption, DocumentTranscript} {
		derivedContextDocument(t, s, p, generation, DocumentID("senderless-"+string(kind)), "unresolvedderivedreceipt "+string(kind), kind, []MessageID{id}, derivedContextProvenance(kind))
	}
	scope := unresolvedScope(p)
	contextPage, contextGuard, err := s.ContextGuarded(ctx, scope, ContextRequest{MessageID: id})
	if err != nil || len(contextPage.Messages) != 1 || len(contextPage.Messages[0].Derived) != 3 {
		t.Fatalf("pre-withholding generated context fixture missing: %#v %v", contextPage, err)
	}
	searchPage, searchGuard, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "unresolvedoriginalreceipt"})
	if err != nil || len(searchPage.Hits) != 1 {
		t.Fatalf("pre-withholding keyword fixture missing: %#v %v", searchPage, err)
	}
	pair, err := s.CandidatePair(ctx, scope, CandidatePairRequest{Search: SearchRequest{Query: "unresolvedderivedreceipt"}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 3 {
		t.Fatalf("pre-withholding derived search fixture missing: count=%d err=%v", len(pair.Keyword), err)
	}
	if err := s.RecordUnresolvedDeletion(ctx, e.Key.Account, e.Key.Chat, e.Key.Message); err != nil {
		t.Fatal(err)
	}
	for _, guard := range []ResponseGuard{contextGuard, searchGuard} {
		if _, err := s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("old response survived durable senderless withholding: %v", err)
		}
	}
	for _, sourceID := range []MessageID{id, otherID} {
		if _, err := s.Context(ctx, scope, ContextRequest{MessageID: sourceID}); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("withheld author/direction remained visible in context: %v", err)
		}
	}
	for _, query := range []string{"unresolvedoriginalreceipt", "unresolvedotherdirectionreceipt", "unresolvedoriginalcaptionreceipt", "unresolvedderivedreceipt"} {
		unresolvedSearchCount(t, s, p, query, 0)
	}
	pair, err = s.CandidatePair(ctx, scope, CandidatePairRequest{Search: SearchRequest{Query: "unresolvedderivedreceipt"}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 0 {
		t.Fatalf("withheld OCR/caption/transcript remained searchable: count=%d err=%v", len(pair.Keyword), err)
	}
	metadata, err := s.SourceMetadata(ctx, e.Key)
	if err != nil || !metadata.Version.Deleted || !metadata.OriginalDateKnown || !metadata.OriginalSentAt.Equal(e.OriginalSentAt) || metadata.ProtocolRevision != e.SourceRevision {
		t.Fatalf("existing target lost genuine metadata during withholding: %#v %v", metadata, err)
	}
	var payloads, activeMedia int
	if err := s.readDB.QueryRow("SELECT count(*) FROM messages WHERE id IN (?,?) AND (body<>'' OR caption<>'' OR normalized_body<>'' OR normalized_caption<>'')", id, otherID).Scan(&payloads); err != nil || payloads != 0 {
		t.Fatalf("withholding retained searchable source plaintext: count=%d err=%v", payloads, err)
	}
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_media_associations WHERE message_id IN (?,?) AND active=1", id, otherID).Scan(&activeMedia); err != nil || activeMedia != 0 {
		t.Fatalf("withholding left media authorized: count=%d err=%v", activeMedia, err)
	}
	cleanup, err := s.ReconcileMedia(ctx, 32)
	if err != nil || len(cleanup) == 0 {
		t.Fatalf("retained originals lost cleanup accounting: count=%d err=%v", len(cleanup), err)
	}
}

func TestUnresolvedDeletionInvalidatesWorkerLeases(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("leased-senderless-target", "unresolvedleasedreceipt", 1, EventOriginal)
	e.Key.Chat = "300@g.us"
	e.BodyKind = MessageImage
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: originalDownloadTestReference(t, MessageImage, e.Key.Message), MimeType: "image/png", DeclaredBytes: 64, Availability: "not_retained"}}
	result := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(ctx, e.Key)
	if err != nil || len(result.MediaIDs) != 1 {
		t.Fatalf("media source fixture missing: %v", err)
	}
	download := originalDownloadTestClaim(t, s)
	reservation := extensionAdmit(t, s, ref, result.MediaIDs[0], BudgetOriginals, 128)
	generation, err := s.EnsureIndex(ctx, indexTestVersion("senderless-worker-index"))
	if err != nil {
		t.Fatal(err)
	}
	job := indexTestPublishedJob(t, s, generation)
	if err := s.RecordUnresolvedDeletion(ctx, e.Key.Account, e.Key.Chat, e.Key.Message); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadOriginalDownload(ctx, download.Lease); err == nil {
		t.Fatal("old original-download lease disclosed a withheld source")
	}
	if _, err := s.LoadJobInput(ctx, job.Lease); err == nil {
		t.Fatal("old embedding lease disclosed a withheld source")
	}
	if ok, _ := s.CommitJob(ctx, indexTestResult(job)); ok {
		t.Fatal("old embedding lease committed a withheld source")
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: reservation, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Bytes: 64, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("old media reservation published a withheld source: %t %v", ok, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 128 {
		t.Fatal("cancelled publication released physical cleanup accounting")
	}
}

func TestUnresolvedDeletionPersistenceFailureRollsBackExistingPurge(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("failed-senderless-target", "unresolvedrollbackreceipt", 1, EventOriginal)
	e.Key.Chat = "300@g.us"
	id := apply(t, s, e, p.Generation).MessageID
	if _, err := s.writeDB.Exec("CREATE TRIGGER fail_senderless_insert BEFORE INSERT ON unresolved_deletions BEGIN SELECT RAISE(ABORT,'synthetic_write_failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUnresolvedDeletion(ctx, e.Key.Account, e.Key.Chat, e.Key.Message); err == nil {
		t.Fatal("failed durable write returned success")
	}
	metadata, err := s.SourceMetadata(ctx, e.Key)
	if err != nil || metadata.Version.Deleted || metadata.Version.MessageID != id {
		t.Fatalf("failed withholding partially purged existing source: %#v %v", metadata, err)
	}
	var guards int
	if err := s.readDB.QueryRow("SELECT count(*) FROM unresolved_deletions").Scan(&guards); err != nil || guards != 0 {
		t.Fatalf("failed transaction left a partial guard: count=%d err=%v", guards, err)
	}
	if _, err := s.Search(ctx, unresolvedScope(p), SearchRequest{Query: "unresolvedrollbackreceipt"}); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("failed source-sensitive withholding left reads authorized: %v", err)
	}
}

func TestUnresolvedDeletionMigrationNineUpgradePreservesSourcesAndCursor(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("old-nine-source", "unresolvedmigrationreceipt", 1, EventOriginal)
	e.Key.Chat = "300@g.us"
	id := apply(t, s, e, p.Generation).MessageID
	var key []byte
	if err := s.readDB.QueryRow("SELECT cursor_key FROM archive_meta WHERE id=1").Scan(&key); err != nil {
		t.Fatal(err)
	}
	// Reconstruct a checksummed version-9 archive, including removal of the
	// later empty recovery audit and its archive metadata columns.
	for _, statement := range []string{"DROP TABLE owner_voice_redownload_intents", "DROP TABLE history_uncertainty_reasons", "DROP TABLE history_uncertainty_coverage", "DROP TABLE rebuild_history_frontiers", "DROP TABLE rebuild_history_pages", "DROP TABLE rebuild_history_bodies", "DROP TABLE rebuild_history_selection_audit", "DROP TABLE rebuild_history_selection", "DROP TABLE unknown_history_codes", "DROP TABLE unknown_history_coverage", "DROP TABLE extension_document_promotions", "ALTER TABLE extension_media_associations DROP COLUMN filename", "DROP TABLE send_permission_modes", "DROP TABLE send_requests", "DROP TABLE send_permissions", "DROP TABLE snapshot_recovery_audit", "ALTER TABLE archive_meta DROP COLUMN recovery_epoch", "ALTER TABLE archive_meta DROP COLUMN recovery_code"} {
		if _, err := s.writeDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.writeDB.Exec("DROP TABLE unresolved_deletions"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("DELETE FROM schema_migrations WHERE version>=10"); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	var reopenedKey []byte
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != len(entries) {
		t.Fatalf("version-9 archive did not upgrade to current schema: %d %v", version, err)
	}
	if err := s.readDB.QueryRow("SELECT cursor_key FROM archive_meta WHERE id=1").Scan(&reopenedKey); err != nil || !bytes.Equal(key, reopenedKey) {
		t.Fatalf("upgrade replaced the archive cursor identity: %v", err)
	}
	metadata, err := s.SourceMetadata(ctx, e.Key)
	if err != nil || metadata.Version.MessageID != id || metadata.Version.Deleted {
		t.Fatalf("upgrade damaged existing source: %#v %v", metadata, err)
	}
	unresolvedSearchCount(t, s, p, "unresolvedmigrationreceipt", 1)
	if err := s.RecordUnresolvedDeletion(ctx, e.Key.Account, e.Key.Chat, e.Key.Message); err != nil {
		t.Fatal(err)
	}
	unresolvedSearchCount(t, s, p, "unresolvedmigrationreceipt", 0)
}
