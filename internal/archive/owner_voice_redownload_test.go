//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ownerVoicePort(t *testing.T, s *Store) OwnerVoiceRecoveryStore {
	t.Helper()
	p, ok := any(s).(OwnerVoiceRecoveryStore)
	if !ok {
		t.Fatal("owner voice recovery store is absent")
	}
	return p
}

func TestOwnerVoiceRecoveryPreservesHistoricalCompletion(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	ctx := context.Background()
	p := testPolicy(t, s)
	for p.Generation < 3 {
		var err error
		p, err = s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
		if err != nil {
			t.Fatal(err)
		}
	}
	e, ref, media := originalDownloadTestSource(t, s, p, "recovery-synthetic", MessageVoice)
	old := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: old, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatalf("fixture publish: %v %v", ok, err)
	}
	p, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if p.Generation != 4 {
		t.Fatal("fixture requires current PG4")
	}
	before, err := extensionLoadDownloadRow(ctx, s.readDB, media)
	if err != nil {
		t.Fatal(err)
	}
	parentBefore := ownerVoiceParentSnapshot(t, s, ref.Version.MessageID)
	initialBefore := ownerVoiceInitialSnapshot(t, s, media)
	port := ownerVoicePort(t, s)
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	intents, err := port.PrepareOwnerVoiceRedownloads(ctx, scope, 8)
	if err != nil || len(intents) != 1 {
		t.Fatalf("prepare: %d %v", len(intents), err)
	}
	i := intents[0]
	if i.Source.Version.PolicyGeneration != 4 || i.MediaID != media {
		t.Fatal("wrong current intent")
	}
	if err = port.RegisterOwnerVoiceRequest(ctx, OwnerVoiceRequestRegistration{IntentID: i.ID, RequestID: "synthetic-request", Source: i.Source}); err != nil {
		t.Fatal(err)
	}
	reply := OwnerVoiceDescriptorReply{RequestID: "synthetic-request", Key: e.Key, OriginalSentAt: e.OriginalSentAt, SourceRevision: e.SourceRevision, ExpiresAt: e.ExpiresAt, Media: e.Media[0]}
	if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || !ok {
		t.Fatalf("accept: %v %v", ok, err)
	}
	originalDownloadTestNoWork(t, s)
	ownerVoiceCleanup(t, s, old)
	w := originalDownloadTestClaim(t, s)
	if w.Lease.IntentID != i.ID {
		t.Fatal("owner lease discriminator lost")
	}
	if _, err = s.LoadOriginalDownload(ctx, w.Lease); err != nil {
		t.Fatal(err)
	}
	r, err := s.AdmitMedia(ctx, MediaAdmissionRequest{Source: w.Source, MediaID: media, Budget: BudgetOriginals, MaximumBytes: 128, TTL: 2 * time.Minute, OwnerDownloadLease: w.Lease})
	if err != nil {
		t.Fatal(err)
	}
	if r.ArtifactID == old.ArtifactID || r.ID == old.ID {
		t.Fatal("old physical identity reused")
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatalf("publication: %v %v", ok, err)
	}
	if ok, err := s.FinishOriginalDownload(ctx, w.Lease); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	x, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobTranscription), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if x.Source != i.Source || x.ArtifactID != r.ArtifactID || x.OriginalSHA256 != strings.Repeat("a", 64) {
		t.Fatal("fresh current extraction tuple missing")
	}
	after, err := extensionLoadDownloadRow(ctx, s.readDB, media)
	if err != nil || before != after {
		t.Fatalf("historical initial row changed: %v", err)
	}
	if ownerVoiceParentSnapshot(t, s, ref.Version.MessageID) != parentBefore || ownerVoiceInitialSnapshot(t, s, media) != initialBefore {
		t.Fatal("parent or historical audit bytes changed")
	}
	current, err := s.CurrentSource(ctx, e.Key)
	if err != nil || current != i.Source {
		t.Fatal("parent source changed")
	}
	if _, known, err := port.LookupOwnerVoiceRequest(ctx, reply.RequestID); err != nil || !known {
		t.Fatal("receipt lost")
	}
	if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || ok {
		t.Fatalf("replay admitted: %v %v", ok, err)
	}
}

func ownerVoiceFixture(t *testing.T) (*Store, *atomic.Int64, string, OwnerVoiceRecoveryStore, ReadScope, OwnerVoiceIntent, OwnerVoiceDescriptorReply, MediaReservation) {
	t.Helper()
	s, clock, dir := originalDownloadTestArchive(t)
	ctx := context.Background()
	p := testPolicy(t, s)
	e, ref, media := originalDownloadTestSource(t, s, p, "owner-negative", MessageVoice)
	old := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: old, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatal("fixture original", err)
	}
	p, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	port := ownerVoicePort(t, s)
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	intents, err := port.PrepareOwnerVoiceRedownloads(ctx, scope, 8)
	if err != nil || len(intents) != 1 {
		t.Fatalf("prepare fixture %d %v", len(intents), err)
	}
	i := intents[0]
	if err = port.RegisterOwnerVoiceRequest(ctx, OwnerVoiceRequestRegistration{IntentID: i.ID, RequestID: "owner-request", Source: i.Source}); err != nil {
		t.Fatal(err)
	}
	return s, clock, dir, port, scope, i, OwnerVoiceDescriptorReply{RequestID: "owner-request", Key: e.Key, OriginalSentAt: e.OriginalSentAt, SourceRevision: e.SourceRevision, ExpiresAt: e.ExpiresAt, Media: e.Media[0]}, old
}
func TestOwnerVoiceRecoveryRejectsChangedAuthorityAndDescriptors(t *testing.T) {
	for _, name := range []string{"request", "key", "sent", "revision", "expiry", "mime", "sha", "view_once", "oversize", "policy", "hash", "tombstone", "chat_clear", "unresolved", "alias", "parent_view_once", "parent_revision"} {
		t.Run(name, func(t *testing.T) {
			s, clock, _, port, scope, i, r, _ := ownerVoiceFixture(t)
			ctx := context.Background()
			var err error
			switch name {
			case "request":
				r.RequestID = "unknown"
			case "key":
				r.Key.Message = "other"
			case "sent":
				r.OriginalSentAt = r.OriginalSentAt.Add(time.Second)
			case "revision":
				r.SourceRevision++
			case "expiry":
				clock.Add(int64(6 * time.Minute / time.Millisecond))
			case "path", "sha":
				var a waE2E.AudioMessage
				if err = proto.Unmarshal(r.Media.ProtocolReference, &a); err != nil {
					t.Fatal(err)
				}
				if name == "path" {
					a.DirectPath = proto.String("/changed")
				} else {
					a.FileSHA256 = bytes.Repeat([]byte{0xbb}, 32)
				}
				r.Media.ProtocolReference, err = proto.Marshal(&a)
			case "mime":
				r.Media.MimeType = "audio/mp4"
			case "view_once":
				r.Media.ViewOnce = true
			case "oversize":
				r.Media.ProtocolReference = make([]byte, 8193)
			case "policy":
				_, err = s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, scope.PolicyGeneration)
			case "hash":
				_, err = s.writeDB.Exec("UPDATE messages SET content_hash=? WHERE id=?", strings.Repeat("c", 64), i.Source.Version.MessageID)
			case "parent_revision":
				_, err = s.writeDB.Exec("UPDATE messages SET local_revision=local_revision+1 WHERE id=?", i.Source.Version.MessageID)
			case "parent_view_once":
				_, err = s.writeDB.Exec("UPDATE messages SET view_once=1 WHERE id=?", i.Source.Version.MessageID)
			case "tombstone":
				_, err = s.writeDB.Exec("INSERT INTO source_tombstones VALUES(?,?,?,?,?,1)", r.Key.Account, r.Key.Chat, r.Key.Sender, r.Key.Message, r.Key.FromMe)
			case "chat_clear":
				_, err = s.writeDB.Exec("INSERT INTO chat_tombstones VALUES(?,?,?)", r.Key.Account, r.Key.Chat, unixms(r.OriginalSentAt))
			case "unresolved":
				_, err = s.writeDB.Exec("INSERT INTO unresolved_deletions VALUES(?,?,?)", r.Key.Account, r.Key.Chat, r.Key.Message)
			case "alias":
				_, err = s.writeDB.Exec("INSERT INTO sync_gaps VALUES(?,?,'identity_alias_conflict')", r.Key.Account, r.Key.Chat)
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, r); err != nil || ok {
				t.Fatalf("negative admitted %v %v", ok, err)
			}
			if _, known, err := port.LookupOwnerVoiceRequest(ctx, "owner-request"); err != nil || !known {
				t.Fatalf("known denial lost %v %v", known, err)
			}
			originalDownloadTestNoWork(t, s)
		})
	}
}
func TestOwnerVoiceRecoveryRestartNoResendAndBoundPublication(t *testing.T) {
	s, clock, dir, port, scope, i, reply, old := ownerVoiceFixture(t)
	ctx := context.Background()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	port = ownerVoicePort(t, s)
	items, err := port.PrepareOwnerVoiceRedownloads(ctx, scope, 8)
	if err != nil || len(items) != 0 {
		t.Fatalf("restart resent known request %v %v", items, err)
	}
	if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || !ok {
		t.Fatal("restart reply", err)
	}
	status, err := port.OwnerVoiceRecoveryStatus(ctx, scope)
	if err != nil || status.WaitingCleanup != 1 {
		t.Fatalf("cleanup status %#v %v", status, err)
	}
	ownerVoiceCleanup(t, s, old)
	w := originalDownloadTestClaim(t, s)
	wrong := w.Lease
	wrong.IntentID = "unknown"
	if _, err = s.LoadOriginalDownload(ctx, wrong); err == nil {
		t.Fatal("unknown intent loaded")
	}
	wrong = w.Lease
	wrong.LeaseID = "wrong"
	if _, err = s.LoadOriginalDownload(ctx, wrong); err == nil {
		t.Fatal("wrong lease loaded")
	}
	req := MediaAdmissionRequest{Source: i.Source, MediaID: i.MediaID, Budget: BudgetOriginals, MaximumBytes: 128, TTL: 2 * time.Minute}
	if _, err = s.AdmitMedia(ctx, req); err == nil {
		t.Fatal("owner authority omitted")
	}
	req.OwnerDownloadLease = w.Lease
	r, err := s.AdmitMedia(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: old, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || ok {
		t.Fatalf("old receipt accepted %v %v", ok, err)
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("b", 64), Bytes: 64, ActualFormat: "ogg"}); err == nil || ok {
		t.Fatal("changed SHA accepted")
	}
	if _, err = s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, scope.PolicyGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RenewMedia(ctx, r, 2*time.Minute); err == nil {
		t.Fatal("invalidated owner lease renewed")
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err == nil && ok {
		t.Fatal("invalidated owner published")
	}
}

func TestOwnerVoiceRecoveryNewestBatchesAndConcurrentDedup(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	ctx := context.Background()
	p := testPolicy(t, s)
	for n := 0; n < 13; n++ {
		e := event(fmt.Sprintf("batch-%02d", n), "", 1, EventOriginal)
		e.BodyKind = MessageVoice
		e.OriginalSentAt = time.Date(2026, 9, 1, 0, n, 0, 0, time.UTC)
		if n == 12 {
			e.Key.Chat = "another-chat"
		}
		e.Media = []MediaReference{{Kind: "voice", ProtocolReference: originalDownloadTestReference(t, MessageVoice, e.Key.Message), MimeType: "audio/ogg", DeclaredBytes: 64, Availability: "not_retained"}}
		applied := apply(t, s, e, p.Generation)
		ref, err := s.CurrentSource(ctx, e.Key)
		if err != nil {
			t.Fatal(err)
		}
		r := extensionAdmit(t, s, ref, applied.MediaIDs[0], BudgetOriginals, 128)
		if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
			t.Fatal(err)
		}
	}
	p, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatID
	if err = s.readDB.QueryRow("SELECT chat_id FROM messages WHERE source_message='batch-00'").Scan(&chat); err != nil {
		t.Fatal(err)
	}
	scope := ReadScope{Account: "account", Principal: "owner", Chats: []ChatID{chat}, PolicyGeneration: p.Generation}
	port := ownerVoicePort(t, s)
	first, err := port.PrepareOwnerVoiceRedownloads(ctx, scope, 8)
	if err != nil || len(first) != 8 {
		t.Fatalf("first batch %d %v", len(first), err)
	}
	for n, i := range first {
		if i.Key.Message != fmt.Sprintf("batch-%02d", 11-n) {
			t.Fatal("not newest first", i.Key.Message)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var rest []OwnerVoiceIntent
	var errs []error
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := port.PrepareOwnerVoiceRedownloads(ctx, scope, 8)
			mu.Lock()
			defer mu.Unlock()
			rest = append(rest, items...)
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	if len(errs) != 0 || len(rest) != 4 {
		t.Fatalf("concurrent batches %d %v", len(rest), errs)
	}
	seen := map[MediaID]bool{}
	for _, i := range append(first, rest...) {
		if seen[i.MediaID] {
			t.Fatal("duplicate intent")
		}
		seen[i.MediaID] = true
	}
	var other int
	if err = s.readDB.QueryRow("SELECT count(*) FROM owner_voice_redownload_intents r JOIN messages m ON m.id=r.message_id WHERE m.source_chat='another-chat'").Scan(&other); err != nil || other != 0 {
		t.Fatal("narrowed chat leaked", other, err)
	}
}
func TestOwnerVoiceRecoveryMigration19PreservesSourceAndOriginal(t *testing.T) {
	s, clock, dir := originalDownloadTestArchive(t)
	ctx := context.Background()
	p := testPolicy(t, s)
	e, ref, media := originalDownloadTestSource(t, s, p, "migration-synthetic", MessageVoice)
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatal(err)
	}
	before, err := extensionLoadDownloadRow(ctx, s.readDB, media)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("DROP TABLE owner_voice_redownload_intents; DELETE FROM schema_migrations WHERE version=20"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := extensionLoadDownloadRow(ctx, s.readDB, media)
	if err != nil || after != before {
		t.Fatal("migration rewrote initial evidence", err)
	}
	current, err := s.CurrentSource(ctx, e.Key)
	if err != nil || current != ref {
		t.Fatal("migration rewrote source", err)
	}
	var version, count int
	if err = s.readDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 20 {
		t.Fatal("migration20 missing", err)
	}
	if err = s.readDB.QueryRow("SELECT count(*) FROM owner_voice_redownload_intents").Scan(&count); err != nil || count != 0 {
		t.Fatal("migration manufactured owner intent", err)
	}
}

func ownerVoiceCleanup(t *testing.T, s *Store, old MediaReservation) {
	t.Helper()
	items, err := s.ReconcileMedia(context.Background(), 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range items {
		if a.ID == old.ArtifactID {
			if err = s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: a.ReservationID, LeaseID: a.CleanupLease, ArtifactID: a.ID, Code: "cleanup_complete"}); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("normal reconciliation did not schedule predecessor cleanup")
}

func TestOwnerVoiceRecoveryFreshTransportPinsExpectedPlaintext(t *testing.T) {
	s, _, _, port, _, i, reply, old := ownerVoiceFixture(t)
	ctx := context.Background()
	var a waE2E.AudioMessage
	if err := proto.Unmarshal(reply.Media.ProtocolReference, &a); err != nil {
		t.Fatal(err)
	}
	a.DirectPath = proto.String("/fresh-transport")
	a.MediaKey = bytes.Repeat([]byte{0x33}, 32)
	a.FileEncSHA256 = bytes.Repeat([]byte{0x44}, 32)
	var err error
	reply.Media.ProtocolReference, err = proto.Marshal(&a)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || !ok {
		t.Fatalf("fresh transport denied %v %v", ok, err)
	}
	current, err := s.CurrentSource(ctx, reply.Key)
	if err != nil || current != i.Source {
		t.Fatal("transport rewrote parent", err)
	}
	var identity string
	if err = s.readDB.QueryRow("SELECT extension_media_identity FROM messages WHERE id=?", i.Source.Version.MessageID).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	freshIdentity, _ := sourceMediaIdentity([]MediaReference{reply.Media})
	if identity == freshIdentity {
		t.Fatal("transport rebound source identity")
	}
	ownerVoiceCleanup(t, s, old)
	w := originalDownloadTestClaim(t, s)
	in, err := s.LoadOriginalDownload(ctx, w.Lease)
	if err != nil || !bytes.Equal(in.ProtocolReference, reply.Media.ProtocolReference) {
		t.Fatal("fresh descriptor not leased", err)
	}
	req := MediaAdmissionRequest{Source: w.Source, MediaID: i.MediaID, Budget: BudgetOriginals, MaximumBytes: 128, TTL: time.Minute, OwnerDownloadLease: w.Lease}
	// Descriptor substitution between load and admission cannot borrow the lease.
	if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET protocol_reference=? WHERE id=?", originalDownloadTestReference(t, MessageVoice, "changed-after-claim"), i.MediaID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitMedia(ctx, req); err == nil {
		t.Fatal("changed descriptor admitted")
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET protocol_reference=? WHERE id=?", reply.Media.ProtocolReference, i.MediaID); err != nil {
		t.Fatal(err)
	}
	reservation, err := s.AdmitMedia(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET protocol_reference=? WHERE id=?", originalDownloadTestReference(t, MessageVoice, "changed-before-publish"), i.MediaID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err == nil && ok {
		t.Fatal("substituted descriptor published")
	}
}
func TestOwnerVoiceRecoveryReservationCrashWaitsForCleanup(t *testing.T) {
	s, clock, dir, port, scope, i, reply, old := ownerVoiceFixture(t)
	ctx := context.Background()
	if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || !ok {
		t.Fatal(err)
	}
	ownerVoiceCleanup(t, s, old)
	w := originalDownloadTestClaim(t, s)
	r, err := s.AdmitMedia(ctx, MediaAdmissionRequest{Source: i.Source, MediaID: i.MediaID, Budget: BudgetOriginals, MaximumBytes: 128, TTL: time.Minute, OwnerDownloadLease: w.Lease})
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(3 * time.Minute / time.Millisecond))
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	port = ownerVoicePort(t, s)
	originalDownloadTestNoWork(t, s)
	row, err := ownerVoiceLoad(ctx, s.readDB, "id", i.ID)
	if err != nil || row.artifact != string(r.ArtifactID) || row.reservation != string(r.ID) {
		t.Fatal("crash dropped binding", err)
	}
	ownerVoiceCleanup(t, s, r)
	next := originalDownloadTestClaim(t, s)
	if next.Lease.LeaseID == w.Lease.LeaseID || next.Attempts != 2 {
		t.Fatal("crash reused lease")
	}
	fresh, err := s.AdmitMedia(ctx, MediaAdmissionRequest{Source: i.Source, MediaID: i.MediaID, Budget: BudgetOriginals, MaximumBytes: 128, TTL: time.Minute, OwnerDownloadLease: next.Lease})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ArtifactID == r.ArtifactID {
		t.Fatal("failed artifact reused")
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: fresh, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	originalDownloadTestNoWork(t, s)
	status, err := s.OwnerVoiceRecoveryStatus(ctx, scope)
	if err != nil || status.Completed != 1 {
		t.Fatal("publication receipt lost before finish", err)
	}
	if ok, err := s.FinishOriginalDownload(ctx, next.Lease); err != nil || !ok {
		t.Fatal("durable finish", err)
	}
}
func TestOwnerVoiceRecoveryNegativeRestrictionsNormalStore(t *testing.T) {
	for _, mode := range []string{"expired", "view_once", "future"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, port, scope, i, reply, _ := ownerVoiceFixture(t)
			ctx := context.Background()
			expiry := time.Time{}
			view := mode == "view_once"
			if mode == "expired" {
				expiry = s.now().Add(-time.Second)
			}
			if mode == "future" {
				expiry = s.now().Add(time.Hour)
			}
			if err := port.RecordOwnerVoiceRestriction(ctx, reply.RequestID, reply.Key, expiry, view); err != nil {
				t.Fatal(err)
			}
			if _, err := s.readSafety(scope.Account); err != nil {
				t.Fatal("normal negative fenced account", err)
			}
			var state, body, hash string
			var revision uint64
			if err := s.readDB.QueryRow("SELECT state,body,content_hash,local_revision FROM messages WHERE id=?", i.Source.Version.MessageID).Scan(&state, &body, &hash, &revision); err != nil {
				t.Fatal(err)
			}
			if mode != "future" && (state == "active" || body != "" || hash != "") {
				t.Fatal("negative left protected body readable")
			}
			if revision <= i.Source.Version.Revision {
				t.Fatal("negative did not invalidate old source")
			}
			if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || ok {
				t.Fatal("negative restored descriptor", err)
			}
			if _, err := s.AuthorizeOriginal(ctx, scope, i.MediaID); err == nil {
				t.Fatal("negative retained attachment")
			}
		})
	}
}

func ownerVoiceParentSnapshot(t *testing.T, s *Store, id MessageID) string {
	t.Helper()
	var snapshot string
	err := s.readDB.QueryRow(`SELECT json_object('account',account,'chat_id',chat_id,'sender_id',sender_id,'source_chat',source_chat,'source_sender',source_sender,'source_message',source_message,'from_me',from_me,'original_sent_at',original_sent_at,'original_date_known',original_date_known,'protocol_revision',protocol_revision,'local_revision',local_revision,'state',state,'source_kind',source_kind,'body',body,'caption',caption,'normalized_body',normalized_body,'normalized_caption',normalized_caption,'body_kind',body_kind,'content_hash',content_hash,'edited',edited,'expires_at',expires_at,'view_once',view_once,'revision_barrier',revision_barrier,'reply_account',reply_account,'reply_chat',reply_chat,'reply_sender',reply_sender,'reply_message',reply_message,'reply_from_me',reply_from_me,'media_identity',extension_media_identity) FROM messages WHERE id=?`, id).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func ownerVoiceInitialSnapshot(t *testing.T, s *Store, id MediaID) string {
	t.Helper()
	var snapshot string
	err := s.readDB.QueryRow(`SELECT json_object('media_id',media_id,'account',account,'message_id',message_id,'local_revision',local_revision,'policy_generation',policy_generation,'content_hash',content_hash,'source_expires_at',source_expires_at,'protocol_sha256',protocol_sha256,'descriptor_sha256',descriptor_sha256,'legacy_source_media_identity',legacy_source_media_identity,'state',state,'attempts',attempts,'next_due',next_due,'lease_id',lease_id,'lease_until',lease_until,'last_code',last_code,'completed_sha256',completed_sha256) FROM extension_original_downloads WHERE media_id=?`, id).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func TestOwnerVoiceRecoveryDeniedNewestDoNotCrowdOut(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	ctx := context.Background()
	p := testPolicy(t, s)
	for n := 0; n < 9; n++ {
		e, ref, media := originalDownloadTestSource(t, s, p, fmt.Sprintf("denied-%02d", n), MessageVoice)
		r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
		if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec("UPDATE messages SET original_sent_at=? WHERE id=?", unixms(e.OriginalSentAt.Add(time.Duration(n)*time.Minute)), ref.Version.MessageID); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`INSERT INTO source_tombstones(account,chat,sender,message,from_me,protocol_revision) SELECT account,source_chat,source_sender,source_message,from_me,protocol_revision FROM messages WHERE source_message<>'denied-00'`); err != nil {
		t.Fatal(err)
	}
	items, err := s.PrepareOwnerVoiceRedownloads(ctx, ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, 8)
	if err != nil || len(items) != 1 || items[0].Key.Message != "denied-00" {
		t.Fatalf("newer denied sources starved older eligible source: %d %v", len(items), err)
	}
}

func TestOwnerVoiceRecoveryWipesDescriptorSecretsForEveryState(t *testing.T) {
	for _, state := range []string{"prepared", "awaiting_phone", "ready", "leased", "complete", "failed", "cancelled", "expired"} {
		t.Run(state, func(t *testing.T) {
			s, _, _, port, scope, i, reply, _ := ownerVoiceFixture(t)
			ctx := context.Background()
			if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || !ok {
				t.Fatal(err)
			}
			if _, err := s.writeDB.Exec("UPDATE owner_voice_redownload_intents SET state=? WHERE id=?", state, i.ID); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := s.readDB.QueryRow("SELECT length(protocol_reference) FROM owner_voice_redownload_intents WHERE id=?", i.ID).Scan(&n); err != nil || n == 0 {
				t.Fatal("fixture no secret", err)
			}
			audit := ownerVoiceInitialSnapshot(t, s, i.MediaID)
			if _, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, scope.PolicyGeneration); err != nil {
				t.Fatal(err)
			}
			if err := s.readDB.QueryRow("SELECT length(protocol_reference) FROM owner_voice_redownload_intents WHERE id=?", i.ID).Scan(&n); err != nil || n != 0 {
				t.Fatal("invalidated secret retained", err)
			}
			if audit != ownerVoiceInitialSnapshot(t, s, i.MediaID) {
				t.Fatal("privacy cleanup rewrote initial audit")
			}
			if _, known, err := port.LookupOwnerVoiceRequest(ctx, reply.RequestID); err != nil || !known {
				t.Fatal("privacy cleanup erased replay receipt", err)
			}
		})
	}
}
func TestOwnerVoiceRecoveryNegativePersistenceFailureFences(t *testing.T) {
	s, _, _, port, scope, _, reply, _ := ownerVoiceFixture(t)
	if _, err := s.writeDB.Exec(`CREATE TRIGGER reject_owner_negative BEFORE UPDATE ON messages BEGIN SELECT RAISE(ABORT,'synthetic_negative_failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := port.RecordOwnerVoiceRestriction(context.Background(), reply.RequestID, reply.Key, time.Time{}, true); err == nil {
		t.Fatal("lost negative acknowledged")
	}
	if _, err := s.readSafety(scope.Account); err == nil {
		t.Fatal("lost authenticated negative did not fence reads")
	}
}

func TestOwnerVoiceRecoveryKnownNegativeSurvivesPolicyAndLocalRevisionDrift(t *testing.T) {
	for _, mode := range []string{"policy", "local_revision", "expired_request", "complete_request", "wrong_original", "wrong_key"} {
		t.Run(mode, func(t *testing.T) {
			s, clock, _, port, scope, i, reply, _ := ownerVoiceFixture(t)
			ctx := context.Background()
			switch mode {
			case "policy":
				if _, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, scope.PolicyGeneration); err != nil {
					t.Fatal(err)
				}
			case "local_revision":
				if _, err := s.writeDB.Exec("UPDATE messages SET local_revision=local_revision+1 WHERE id=?", i.Source.Version.MessageID); err != nil {
					t.Fatal(err)
				}
			case "expired_request":
				clock.Add(int64(6 * time.Minute / time.Millisecond))
			case "complete_request":
				if _, err := s.writeDB.Exec("UPDATE owner_voice_redownload_intents SET state='complete' WHERE id=?", i.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong_original":
				if _, err := s.writeDB.Exec("UPDATE messages SET original_sent_at=original_sent_at+1000 WHERE id=?", i.Source.Version.MessageID); err != nil {
					t.Fatal(err)
				}
			case "wrong_key":
				reply.Key.Message = "different-key"
			}
			before := ownerVoiceParentSnapshot(t, s, i.Source.Version.MessageID)
			err := port.RecordOwnerVoiceRestriction(ctx, reply.RequestID, reply.Key, time.Time{}, true)
			if mode == "wrong_original" || mode == "wrong_key" {
				if err == nil || before != ownerVoiceParentSnapshot(t, s, i.Source.Version.MessageID) {
					t.Fatal("unrelated original negatively mutated")
				}
				return
			}
			if err != nil {
				t.Fatal("known privacy fact lost", err)
			}
			var state, body string
			if err = s.readDB.QueryRow("SELECT state,body FROM messages WHERE id=?", i.Source.Version.MessageID).Scan(&state, &body); err != nil || state == "active" || body != "" {
				t.Fatal("known negative not enforced", err)
			}
			if _, err = s.readSafety(scope.Account); err != nil {
				t.Fatal("legitimate negative fenced account", err)
			}
			if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || ok {
				t.Fatal("known negative gained positive authority", err)
			}
		})
	}
}

func ownerVoiceDiagnosticPort(t *testing.T, s *Store) interface {
	RecordOwnerVoiceReplyDiagnostic(context.Context, string, SourceRef, string) (bool, error)
} {
	t.Helper()
	port, ok := any(s).(interface {
		RecordOwnerVoiceReplyDiagnostic(context.Context, string, SourceRef, string) (bool, error)
	})
	if !ok {
		t.Fatal("missing source-checked owner reply diagnostic port")
	}
	return port
}

func TestOwnerVoiceReplyDiagnosticPreservesAwaitingAndClearsOnAcceptance(t *testing.T) {
	s, _, _, port, _, intent, reply, _ := ownerVoiceFixture(t)
	ctx := context.Background()
	diagnostics := ownerVoiceDiagnosticPort(t, s)
	parent := ownerVoiceParentSnapshot(t, s, intent.Source.Version.MessageID)
	initial := ownerVoiceInitialSnapshot(t, s, intent.MediaID)
	for _, code := range []string{"voice_reply_invalid_envelope", "voice_reply_key_mismatch", "voice_reply_original_mismatch", "voice_reply_no_audio", "voice_reply_invalid_descriptor", "voice_reply_not_accepted", "voice_reply_bodyless", "voice_reply_mutation", "voice_reply_restricted"} {
		ok, err := diagnostics.RecordOwnerVoiceReplyDiagnostic(ctx, reply.RequestID, intent.Source, code)
		if err != nil || !ok {
			t.Fatalf("diagnostic write: %s %v %v", code, ok, err)
		}
		var state, got, protocol string
		if err := s.readDB.QueryRow("SELECT state,last_code,hex(protocol_reference) FROM owner_voice_redownload_intents WHERE id=?", intent.ID).Scan(&state, &got, &protocol); err != nil {
			t.Fatal(err)
		}
		if state != "awaiting_phone" || got != code || protocol != "" {
			t.Fatalf("diagnostic changed admission: %s %s %s", state, got, protocol)
		}
	}
	if parent != ownerVoiceParentSnapshot(t, s, intent.Source.Version.MessageID) || initial != ownerVoiceInitialSnapshot(t, s, intent.MediaID) {
		t.Fatal("diagnostic changed parent or original completion")
	}
	if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || !ok {
		t.Fatalf("later valid reply: %v %v", ok, err)
	}
	var state, code string
	if err := s.readDB.QueryRow("SELECT state,last_code FROM owner_voice_redownload_intents WHERE id=?", intent.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || code != "" {
		t.Fatal("accepted reply retained rejection diagnostic", state, code)
	}
}

func TestOwnerVoiceReplyDiagnosticRejectsStaleOrUnknownAuthority(t *testing.T) {
	for _, scenario := range []string{"unknown", "expired", "policy", "source", "ready", "failed", "cancelled", "complete", "invalid_code"} {
		t.Run(scenario, func(t *testing.T) {
			s, clock, _, port, scope, intent, reply, _ := ownerVoiceFixture(t)
			ctx := context.Background()
			diagnostics := ownerVoiceDiagnosticPort(t, s)
			request, source, code := reply.RequestID, intent.Source, "voice_reply_invalid_envelope"
			switch scenario {
			case "unknown":
				request = "unknown-request"
			case "expired":
				clock.Add(int64(6 * time.Minute / time.Millisecond))
			case "policy":
				if _, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, scope.PolicyGeneration); err != nil {
					t.Fatal(err)
				}
			case "source":
				source.Version.Revision++
			case "ready":
				if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || !ok {
					t.Fatal("fixture accept", err)
				}
			case "failed", "cancelled", "complete":
				if _, err := s.writeDB.Exec("UPDATE owner_voice_redownload_intents SET state=?,last_code='media_unavailable' WHERE id=?", scenario, intent.ID); err != nil {
					t.Fatal(err)
				}
			case "invalid_code":
				code = "private payload must never persist"
			}
			var before string
			if err := s.readDB.QueryRow("SELECT last_code FROM owner_voice_redownload_intents WHERE id=?", intent.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if ok, _ := diagnostics.RecordOwnerVoiceReplyDiagnostic(ctx, request, source, code); ok {
				t.Fatal("diagnostic accepted stale or unknown authority")
			}
			var after string
			if err := s.readDB.QueryRow("SELECT last_code FROM owner_voice_redownload_intents WHERE id=?", intent.ID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatal("diagnostic changed unavailable/final request", before, after)
			}
		})
	}
}

func TestOwnerVoiceReplyDiagnosticSurvivesExpiryWithExistingStatusCode(t *testing.T) {
	s, clock, _, port, scope, intent, reply, _ := ownerVoiceFixture(t)
	ctx := context.Background()
	if ok, err := ownerVoiceDiagnosticPort(t, s).RecordOwnerVoiceReplyDiagnostic(ctx, reply.RequestID, intent.Source, "voice_reply_original_mismatch"); err != nil || !ok {
		t.Fatal("diagnostic missing", err)
	}
	clock.Add(int64(6 * time.Minute / time.Millisecond))
	status, err := port.OwnerVoiceRecoveryStatus(ctx, scope)
	if err != nil || status.Failed != 1 || status.Code != "media_deadline_exceeded" {
		t.Fatalf("expiry status changed: %+v %v", status, err)
	}
	var state, code string
	if err := s.readDB.QueryRow("SELECT state,last_code FROM owner_voice_redownload_intents WHERE id=?", intent.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "expired" || code != "voice_reply_original_mismatch" {
		t.Fatal("expiry erased reply evidence", state, code)
	}
	if ok, err := port.AcceptOwnerVoiceDescriptor(ctx, reply); err != nil || ok {
		t.Fatal("diagnostic renewed expired authority", ok, err)
	}
}

func TestOwnerVoiceReplyDiagnosticRejectsDeadlineCrossedBeforeCommit(t *testing.T) {
	s, clock, _, _, _, intent, reply, _ := ownerVoiceFixture(t)
	ctx := context.Background()
	err := s.extensionTransaction(ctx, intent.Source.Account, func(tx *sql.Tx) error {
		ok, err := s.ownerVoiceReplyDiagnostic(ctx, tx, reply.RequestID, intent.Source, "voice_reply_invalid_envelope")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("diagnostic was not initially eligible")
		}
		clock.Add(int64(6 * time.Minute / time.Millisecond))
		return nil
	})
	if err == nil {
		t.Fatal("diagnostic committed after the request deadline")
	}
	var code string
	if err := s.readDB.QueryRow("SELECT last_code FROM owner_voice_redownload_intents WHERE id=?", intent.ID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "" {
		t.Fatal("expired diagnostic transaction was not rolled back", code)
	}
}

func TestOwnerVoiceReplyDiagnosticCancelledCallLeavesEvidenceUnchanged(t *testing.T) {
	s, _, _, _, _, intent, reply, _ := ownerVoiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := ownerVoiceDiagnosticPort(t, s).RecordOwnerVoiceReplyDiagnostic(ctx, reply.RequestID, intent.Source, "voice_reply_invalid_envelope"); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled diagnostic call lost its cancellation", ok, err)
	}
	var code string
	if err := s.readDB.QueryRow("SELECT last_code FROM owner_voice_redownload_intents WHERE id=?", intent.ID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "" {
		t.Fatal("cancelled diagnostic call changed evidence", code)
	}
}
