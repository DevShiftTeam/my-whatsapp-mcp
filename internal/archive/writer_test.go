//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testArchive(t *testing.T) *Store {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s, e := Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testPolicy(t *testing.T, s *Store) ArchivePolicy {
	t.Helper()
	p, e := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func event(id, body string, rev int64, kind SourceEventKind) SourceEvent {
	return SourceEvent{Key: SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: id}, Kind: kind, OriginalSentAt: time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC), OriginalDateKnown: true, SourceRevision: rev, Body: body, BodyKind: MessageText}
}
func apply(t *testing.T, s *Store, e SourceEvent, g Generation) ApplyResult {
	t.Helper()
	r, err := s.ApplySourceEvent(context.Background(), e, g)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestReplayEditDeleteAndRevisionSeparation(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ctx := context.Background()
	r := apply(t, s, event("m", "original", 1, EventOriginal), p.Generation)
	if r.MessageID == "" || r.Version.Revision != 1 || r.ArchiveRevision != 1 {
		t.Fatalf("initial %#v", r)
	}
	duplicate := apply(t, s, event("m", "original", 1, EventOriginal), p.Generation)
	if duplicate.Outcome != OutcomeDuplicate || duplicate.ArchiveRevision != r.ArchiveRevision {
		t.Fatalf("duplicate %#v", duplicate)
	}
	invalid := event("m", "", 5, EventEditInvalidation)
	invalid.OriginalDateKnown = false
	invalid.OriginalSentAt = time.Time{}
	r = apply(t, s, invalid, p.Generation)
	if r.Version.Revision != 2 {
		t.Fatalf("local and protocol revision conflated: %#v", r)
	}
	r = apply(t, s, event("m", "edited", 5, EventEditContent), p.Generation)
	if r.Version.Revision != 3 {
		t.Fatalf("fill %#v", r)
	}
	older := apply(t, s, event("m", "original", 1, EventOriginal), p.Generation)
	if older.Outcome != OutcomeStale {
		t.Fatalf("older original %#v", older)
	}
	conflicting := event("m", "different edit", 5, EventEditContent)
	_, err := s.ApplySourceEvent(ctx, conflicting, p.Generation)
	if !errors.Is(err, ErrSourceRevisionConflict) {
		t.Fatalf("conflict %v", err)
	}
	var body string
	if err := s.readDB.QueryRow("SELECT body FROM messages WHERE id=?", r.MessageID).Scan(&body); err != nil || body != "" {
		t.Fatalf("conflict left readable body %q %v", body, err)
	}
	r = apply(t, s, event("m", "", 6, EventDelete), p.Generation)
	if !r.Version.Deleted {
		t.Fatal("not tombstoned")
	}
	r = apply(t, s, event("m", "resurrection", 1000, EventOriginal), p.Generation)
	if r.Outcome != OutcomeStale || !r.Version.Deleted {
		t.Fatalf("resurrected %#v", r)
	}
}
func TestUnknownDateEditDoesNotPersistBodyThenOriginalEstablishesDate(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("late", "forbidden unknown date", 7, EventEditContent)
	e.OriginalDateKnown = false
	e.OriginalSentAt = time.Time{}
	r := apply(t, s, e, p.Generation)
	if r.Outcome != OutcomeAwaitingMetadata {
		t.Fatalf("%#v", r)
	}
	r = apply(t, s, event("late", "older original", 1, EventOriginal), p.Generation)
	m, err := s.SourceMetadata(context.Background(), e.Key)
	if err != nil || !m.OriginalDateKnown || m.ProtocolRevision != 7 {
		t.Fatalf("metadata %#v %v", m, err)
	}
	var body string
	s.readDB.QueryRow("SELECT body FROM messages WHERE id=?", r.MessageID).Scan(&body)
	if body != "" {
		t.Fatalf("late original overwrote edit %q", body)
	}
	r = apply(t, s, event("late", "latest edit", 7, EventEditContent), p.Generation)
	if r.Outcome != OutcomeApplied {
		t.Fatalf("fill %#v", r)
	}
}
func TestPolicyCASAndNarrowingInvalidateVersions(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	r := apply(t, s, event("m", "retained", 1, EventOriginal), p.Generation)
	p2, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplySourceEvent(context.Background(), event("new", "old generation", 1, EventOriginal), p.Generation)
	if !errors.Is(err, ErrPolicyChanged) {
		t.Fatal(err)
	}
	m, err := s.SourceMetadata(context.Background(), event("m", "", 1, EventOriginal).Key)
	if err != nil || !m.Version.Excluded || m.Version.PolicyGeneration != p2.Generation || m.Version.Revision <= r.Version.Revision {
		t.Fatalf("version %#v %v", m, err)
	}
	var count int
	s.readDB.QueryRow("SELECT count(*) FROM message_fts").Scan(&count)
	if count != 0 {
		t.Fatalf("fts rows %d", count)
	}
}
func TestAliasAccountCollisionAndStablePublicIDs(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("m", "hello", 1, EventOriginal)
	e.Key.Chat = "123@s.whatsapp.net"
	e.Key.Sender = e.Key.Chat
	r := apply(t, s, e, p.Generation)
	alias := SourceAlias{Account: "account", PhoneJID: e.Key.Chat, LID: "456@lid", Provenance: AliasConnectorMapping}
	if err := s.ApplyAlias(context.Background(), alias); err != nil {
		t.Fatal(err)
	}
	e.Key.Chat = alias.LID
	e.Key.Sender = alias.LID
	r2 := apply(t, s, e, p.Generation)
	if r2.MessageID != r.MessageID || r2.Outcome != OutcomeDuplicate {
		t.Fatalf("alias replay %#v %#v", r, r2)
	}
	alias.PhoneJID = "999@s.whatsapp.net"
	err := s.ApplyAlias(context.Background(), alias)
	if !errors.Is(err, ErrIdentityAliasConflict) {
		t.Fatalf("collision %v", err)
	}
	_, err = s.ApplySourceEvent(context.Background(), event("unsafe", "x", 1, EventOriginal), p.Generation)
	if !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("unsafe account still admitted %v", err)
	}
}
func TestDeleteBeforeOriginalAndClearCutoff(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("missing", "", 2, EventDelete)
	e.OriginalDateKnown = false
	e.OriginalSentAt = time.Time{}
	apply(t, s, e, p.Generation)
	r := apply(t, s, event("missing", "must never return", 99, EventOriginal), p.Generation)
	if r.Outcome != OutcomeStale || !r.Version.Deleted {
		t.Fatalf("%#v", r)
	}
	e = event("old", "old", 1, EventOriginal)
	apply(t, s, e, p.Generation)
	if err := s.ClearChat(context.Background(), "account", "chat", e.OriginalSentAt); err != nil {
		t.Fatal(err)
	}
	r = apply(t, s, e, p.Generation)
	if !r.Version.Deleted {
		t.Fatal("clear not terminal")
	}
	e = event("new", "new", 1, EventOriginal)
	e.OriginalSentAt = e.OriginalSentAt.Add(time.Second)
	r = apply(t, s, e, p.Generation)
	if r.Outcome != OutcomeApplied {
		t.Fatalf("new after clear %#v", r)
	}
}
func TestAtomicCheckpointDirtyAndFTSRollback(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ctx := context.Background()
	if _, err := s.writeDB.Exec("CREATE TRIGGER fail_dirty BEFORE INSERT ON dirty_windows BEGIN SELECT RAISE(ABORT,'test'); END"); err != nil {
		t.Fatal(err)
	}
	e := event("m", "atomic canary", 1, EventOriginal)
	e.Checkpoint = &SyncCheckpoint{Kind: "history", ID: "delivery"}
	if _, err := s.ApplySourceEvent(ctx, e, p.Generation); err == nil {
		t.Fatal("expected transaction failure")
	}
	for _, table := range []string{"messages", "message_fts", "sync_state", "dirty_windows"} {
		var n int
		if err := s.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s persisted %d %v", table, n, err)
		}
	}
	var rev int
	s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&rev)
	if rev != 0 {
		t.Fatalf("revision %d", rev)
	}
}

func TestRevisionConflictBarrierCannotBeRefilledAtSameRevision(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("m", "first edit", 5, EventEditContent)
	apply(t, s, e, p.Generation)
	e.Body = "different edit"
	if _, err := s.ApplySourceEvent(context.Background(), e, p.Generation); !errors.Is(err, ErrSourceRevisionConflict) {
		t.Fatal(err)
	}
	for _, body := range []string{"first edit", "different edit"} {
		e.Body = body
		if _, err := s.ApplySourceEvent(context.Background(), e, p.Generation); !errors.Is(err, ErrSourceRevisionConflict) {
			t.Fatalf("same conflicted revision refilled: %v", err)
		}
	}
	var count int
	s.readDB.QueryRow("SELECT count(*) FROM message_fts").Scan(&count)
	if count != 0 {
		t.Fatal("conflict searchable")
	}
	e.SourceRevision = 6
	e.Body = "trusted newer edit"
	if r := apply(t, s, e, p.Generation); r.Outcome != OutcomeApplied {
		t.Fatalf("newer blocked %#v", r)
	}
}
func TestMissingMutationOrderingCannotRestoreOlderKnownRevision(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("m", "known edit", 5, EventEditContent)
	apply(t, s, e, p.Generation)
	missing := event("m", "", 0, EventEditInvalidation)
	if _, err := s.ApplySourceEvent(context.Background(), missing, p.Generation); !errors.Is(err, ErrMutationMetadataMissing) {
		t.Fatal(err)
	}
	if _, err := s.ApplySourceEvent(context.Background(), e, p.Generation); err == nil {
		t.Fatal("old known revision restored after orderless mutation")
	}
	var count int
	s.readDB.QueryRow("SELECT count(*) FROM message_fts").Scan(&count)
	if count != 0 {
		t.Fatal("missing mutation barrier searchable")
	}
}
func TestMediaCompletionGuardsCurrentVersionPolicyAndActualExpiry(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("m", "media", 1, EventOriginal)
	e.BodyKind = MessageImage
	r := apply(t, s, e, p.Generation)
	ok, err := s.EligibleMedia(context.Background(), e.Key, r.Version)
	if err != nil || !ok {
		t.Fatalf("eligible %v %v", ok, err)
	}
	if err = s.UpdateMedia(context.Background(), e.Key, r.Version, "retained"); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("fake retained %v", err)
	}
	inv := event("m", "", 2, EventEditInvalidation)
	apply(t, s, inv, p.Generation)
	ok, err = s.EligibleMedia(context.Background(), e.Key, r.Version)
	if err != nil || ok {
		t.Fatalf("stale eligible %v %v", ok, err)
	}
	if err = s.UpdateMedia(context.Background(), e.Key, r.Version, "download_failed"); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("late completion %v", err)
	}
}
func waitArchiveCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("archive condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestPriorityDeletePrecedesQueuedBody(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("m", "old body", 1, EventOriginal)
	original := apply(t, s, e, p.Generation)
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	baseWait := s.writeDB.Stats().WaitCount
	type result struct {
		value ApplyResult
		err   error
	}
	first := make(chan result, 1)
	go func() {
		r, e := s.ApplySourceEvent(context.Background(), event("a", "first body", 1, EventOriginal), p.Generation)
		first <- result{r, e}
	}()
	waitArchiveCondition(t, func() bool { return s.writeDB.Stats().WaitCount > baseWait })
	second := make(chan result, 1)
	go func() {
		r, e := s.ApplySourceEvent(context.Background(), event("b", "queued body", 1, EventOriginal), p.Generation)
		second <- result{r, e}
	}()
	waitArchiveCondition(t, func() bool { return len(s.bodies) == 1 })
	deleted := make(chan result, 1)
	go func() {
		r, e := s.ApplySourceEvent(context.Background(), event("m", "", 2, EventDelete), p.Generation)
		deleted <- result{r, e}
	}()
	waitArchiveCondition(t, func() bool { return len(s.controls) == 1 })
	conn.Close()
	r1, rdelete, r2 := <-first, <-deleted, <-second
	for _, r := range []result{r1, rdelete, r2} {
		if r.err != nil {
			t.Fatal(r.err)
		}
	}
	if !(r1.value.ArchiveRevision < rdelete.value.ArchiveRevision && rdelete.value.ArchiveRevision < r2.value.ArchiveRevision) {
		t.Fatalf("priority ordering first=%d delete=%d queued=%d", r1.value.ArchiveRevision, rdelete.value.ArchiveRevision, r2.value.ArchiveRevision)
	}
	var body string
	s.readDB.QueryRow("SELECT body FROM messages WHERE id=?", original.MessageID).Scan(&body)
	if body != "" {
		t.Fatal("delete failed")
	}
}

func TestTighterExpiryInvalidatesDuplicateVersionAndNeverExtends(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("m", "expires", 1, EventOriginal)
	r := apply(t, s, e, p.Generation)
	expires := time.Now().UTC().Add(time.Minute)
	e.ExpiresAt = expires
	r2 := apply(t, s, e, p.Generation)
	m, err := s.SourceMetadata(context.Background(), e.Key)
	if err != nil || m.Version.ExpiresAt.UnixMilli() != expires.UnixMilli() || r2.Version.Revision <= r.Version.Revision {
		t.Fatalf("duplicate lost expiry %#v %#v %v", r2, m, err)
	}
	e.ExpiresAt = expires.Add(time.Hour)
	e.SourceRevision = 2
	e.Kind = EventEditContent
	e.Body = "edit"
	apply(t, s, e, p.Generation)
	m, err = s.SourceMetadata(context.Background(), e.Key)
	if err != nil || m.Version.ExpiresAt.UnixMilli() != expires.UnixMilli() {
		t.Fatalf("expiry extended %#v %v", m, err)
	}
}
func TestMediaAdmissionHonorsRollingPolicyAndViewOnce(t *testing.T) {
	s := testArchive(t)
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll, RollingRetention: time.Minute}, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := event("m", "media", 1, EventOriginal)
	e.OriginalSentAt = time.Now().UTC()
	e.BodyKind = MessageImage
	e.Media = []MediaReference{{Kind: "image", ViewOnce: true, Availability: "not_retained"}}
	r := apply(t, s, e, p.Generation)
	if ok, err := s.EligibleMedia(context.Background(), e.Key, r.Version); err != nil || ok {
		t.Fatalf("view once authorized %v %v", ok, err)
	}
	e = event("fresh", "media", 1, EventOriginal)
	e.OriginalSentAt = time.Now().UTC()
	r = apply(t, s, e, p.Generation)
	if _, err = s.writeDB.Exec("UPDATE messages SET original_sent_at=? WHERE id=?", e.OriginalSentAt.Add(-2*time.Minute).UnixMilli(), r.MessageID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.EligibleMedia(context.Background(), e.Key, r.Version); err != nil || ok {
		t.Fatalf("aged rolling source authorized %v %v", ok, err)
	}
}

func TestPolicyReconstructsPendingDirtyWorkAtCurrentGeneration(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	r := apply(t, s, event("m", "pending", 1, EventOriginal), p.Generation)
	p2, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	s.readDB.QueryRow("SELECT count(*) FROM dirty_windows WHERE generation=? AND chat_id=(SELECT chat_id FROM messages WHERE id=?)", p2.Generation, r.MessageID).Scan(&n)
	if n != 1 {
		t.Fatal("permitted pending source lost durable work")
	}
}
func TestAliasReconstructsDirtyWorkOnCanonicalChat(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("lid-message", "lid work", 1, EventOriginal)
	e.Key.Chat = "456@lid"
	e.Key.Sender = "456@lid"
	r := apply(t, s, e, p.Generation)
	if _, err := s.writeDB.Exec("INSERT INTO chats(id,account,source_chat) VALUES('existing-pn','account','123@s.whatsapp.net')"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	var n int
	s.readDB.QueryRow("SELECT count(*) FROM dirty_windows WHERE generation=? AND chat_id=(SELECT chat_id FROM messages WHERE id=?)", p.Generation, r.MessageID).Scan(&n)
	if n != 1 {
		t.Fatal("canonical chat lost source dirty work")
	}
}
func TestPolicyReadUsesIndependentPoolAndCancellationKeepsSafety(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	got, err := s.Policy(ctx)
	cancel()
	if err != nil || got.Generation != p.Generation {
		t.Fatalf("writer blocked policy read: %#v %v", got, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err = s.Policy(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = s.readSafety("account"); err != nil {
		t.Fatalf("readonly cancellation denied archive: %v", err)
	}
}

func TestTrustedNamesAreMetadataOnlyAndCannotMergeIdentities(t *testing.T) {
	s := testArchive(t)
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sender := range []string{"person-one", "person-two"} {
		if err = s.SetSourceNames(context.Background(), SourceNames{Account: "account", Chat: "unselected-chat", Sender: sender, ChatName: "קבוצה", SenderName: "Same Name"}); err != nil {
			t.Fatal(err)
		}
	}
	r := apply(t, s, SourceEvent{Key: SourceKey{Account: "account", Chat: "unselected-chat", Sender: "person-one", Message: "m"}, Kind: EventOriginal, OriginalDateKnown: true, OriginalSentAt: time.Now(), Body: "never persist excluded", BodyKind: MessageText}, p.Generation)
	if r.Outcome != OutcomeFiltered {
		t.Fatal(r)
	}
	var identities, chats, messages int
	s.readDB.QueryRow("SELECT count(*) FROM identities WHERE name='Same Name'").Scan(&identities)
	s.readDB.QueryRow("SELECT count(*) FROM chats WHERE name='קבוצה'").Scan(&chats)
	s.readDB.QueryRow("SELECT count(*) FROM messages").Scan(&messages)
	if identities != 2 || chats != 1 || messages != 0 {
		t.Fatalf("metadata merged or body persisted: identities=%d chats=%d messages=%d", identities, chats, messages)
	}
}

func TestNewPermittedOriginalPreservesReadSafetyButExistingMutationsInvalidate(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	epoch, e := s.readSafety("account")
	if e != nil {
		t.Fatal(e)
	}
	r := apply(t, s, event("new", "unrelated permitted original", 1, EventOriginal), p.Generation)
	after, e := s.readSafety("account")
	if e != nil {
		t.Fatal(e)
	}
	if after != epoch {
		t.Fatalf("new independent original revoked existing source reads: before=%d after=%d", epoch, after)
	}
	if r.ArchiveRevision == 0 {
		t.Fatal("new original did not advance cursor archive identity")
	}
	if e = s.verifyReadSafety("account", epoch); e != nil {
		t.Fatalf("unchanged source pin denied %v", e)
	}
	apply(t, s, event("new", "", 2, EventEditInvalidation), p.Generation)
	if e = s.verifyReadSafety("account", epoch); !errors.Is(e, ErrPolicyChanged) {
		t.Fatalf("existing source invalidation kept old read safety: %v", e)
	}
}

func TestRepeatedSourceNamesDoNotRevokeReadsOrAdvanceArchive(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	n := SourceNames{Account: "account", Chat: "chat", Sender: "sender", ChatName: "Group", SenderName: "Sender"}
	if e := s.SetSourceNames(context.Background(), n); e != nil {
		t.Fatal(e)
	}
	epoch, e := s.readSafety("account")
	if e != nil {
		t.Fatal(e)
	}
	var before, after ArchiveRevision
	s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&before)
	if e = s.SetSourceNames(context.Background(), n); e != nil {
		t.Fatal(e)
	}
	s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&after)
	if before != after {
		t.Fatalf("metadata replay changed archive: %d->%d", before, after)
	}
	if e = s.verifyReadSafety("account", epoch); e != nil {
		t.Fatalf("repeated name canceled stable source read: %v", e)
	}
}
