//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// Keep the first producer RED compilable while the optional port is absent.
// These tests use the real archive, its writer, and SQLite; no queue fake can
// manufacture durable scheduling or authoritative original publication.
func originalDownloadTestPort(t *testing.T, s *Store) OriginalDownloadStore {
	t.Helper()
	p, ok := any(s).(OriginalDownloadStore)
	if !ok {
		t.Fatal("archive does not implement the optional OriginalDownloadStore")
	}
	return p
}

func originalDownloadTestArchive(t *testing.T) (*Store, *atomic.Int64, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := new(atomic.Int64)
	clock.Store(unixms(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)))
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clock, dir
}

func originalDownloadTestReference(t *testing.T, kind MessageKind, tag string) []byte {
	t.Helper()
	var m proto.Message = &waE2E.ImageMessage{
		DirectPath: proto.String("/synthetic/" + tag), MediaKey: bytes.Repeat([]byte{0x11}, 32),
		FileSHA256: bytes.Repeat([]byte{0xaa}, 32), FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(64),
	}
	if kind == MessageVoice {
		m = &waE2E.AudioMessage{DirectPath: proto.String("/synthetic/" + tag), MediaKey: bytes.Repeat([]byte{0x11}, 32),
			FileSHA256: bytes.Repeat([]byte{0xaa}, 32), FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(64)}
	}
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func originalDownloadTestSource(t *testing.T, s *Store, p ArchivePolicy, name string, kind MessageKind) (SourceEvent, SourceRef, MediaID) {
	t.Helper()
	e := event(name, "", 1, EventOriginal)
	e.BodyKind, e.Caption = kind, "private caption "+name
	e.Media = []MediaReference{{Kind: string(kind), ProtocolReference: originalDownloadTestReference(t, kind, name), MimeType: "image/png", DeclaredBytes: 64, Availability: "not_retained"}}
	if kind == MessageVoice {
		e.Media[0].MimeType = "audio/ogg"
	}
	r := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.MediaIDs) != 1 {
		t.Fatalf("source has no unique media association: %#v", r)
	}
	return e, ref, r.MediaIDs[0]
}

func originalDownloadTestClaim(t *testing.T, p OriginalDownloadStore) OriginalDownloadWork {
	t.Helper()
	w, err := p.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if w.Lease.MediaID == "" || w.Lease.LeaseID == "" || w.Attempts < 1 || w.LeaseUntil.IsZero() {
		t.Fatalf("incomplete scheduling lease: %#v", w)
	}
	return w
}

func originalDownloadTestNoWork(t *testing.T, p OriginalDownloadStore) {
	t.Helper()
	if w, err := p.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: 2 * time.Minute}); !errors.Is(err, ErrNoWork) || w.Lease.MediaID != "" {
		t.Fatalf("unexpected automatic original attempt: %#v %v", w, err)
	}
}

func TestOriginalDownloadClaimLoadFinishRequiresRetainedAuthority(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	e, ref, media := originalDownloadTestSource(t, s, p, "original", MessageImage)
	w := originalDownloadTestClaim(t, port)
	if w.Lease.MediaID != media || w.Source != ref || w.Kind != MessageImage || w.DeclaredBytes != 64 {
		t.Fatalf("wrong work: %#v", w)
	}
	in, err := port.LoadOriginalDownload(context.Background(), w.Lease)
	if err != nil || in.Work != w || in.Key != e.Key || !bytes.Equal(in.ProtocolReference, e.Media[0].ProtocolReference) {
		t.Fatalf("wrong bounded private input: %#v %v", in, err)
	}
	if ok, err := port.FinishOriginalDownload(context.Background(), w.Lease); err != nil || ok {
		t.Fatalf("caller success manufactured completion: %v %v", ok, err)
	}
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	extensionPublish(t, s, r, 64)
	for i := 0; i < 2; i++ {
		if ok, err := port.FinishOriginalDownload(context.Background(), w.Lease); err != nil || !ok {
			t.Fatalf("exact publication acknowledgement %v %v", ok, err)
		}
	}
	if err := port.RetryOriginalDownload(context.Background(), OriginalDownloadRetry{Lease: w.Lease, Code: "media_unavailable", After: time.Second}); err != nil {
		t.Fatal(err)
	}
	originalDownloadTestNoWork(t, port)
	var state, marker string
	if err := s.readDB.QueryRow("SELECT state,completed_sha256 FROM extension_original_downloads WHERE media_id=?", media).Scan(&state, &marker); err != nil || state != "complete" || marker != strings.Repeat("a", 64) {
		t.Fatalf("completion overwritten: %s %s %v", state, marker, err)
	}
}

func TestOriginalDownloadCrashReclaimKeepsPhysicalCleanupBarrier(t *testing.T) {
	s, clock, dir := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	_, ref, media := originalDownloadTestSource(t, s, p, "crash", MessageVoice)
	w := originalDownloadTestClaim(t, port)
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	clock.Add(int64(3 * time.Minute / time.Millisecond))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	port = originalDownloadTestPort(t, reopened)
	originalDownloadTestNoWork(t, port)
	if charge := extensionMediaCharge(t, reopened, BudgetOriginals); charge != 128 {
		t.Fatalf("scheduler expiry released physical charge: %d", charge)
	}
	if err := reopened.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: r.ID, LeaseID: r.LeaseID, ArtifactID: r.ArtifactID, Code: "cleanup_complete"}); err != nil {
		t.Fatal(err)
	}
	next := originalDownloadTestClaim(t, port)
	if next.Lease.MediaID != media || next.Lease.LeaseID == w.Lease.LeaseID || next.Attempts != 2 {
		t.Fatalf("wrong reclaimed scheduling lease: %#v", next)
	}
	if _, err := port.LoadOriginalDownload(context.Background(), w.Lease); !errors.Is(err, ErrNoWork) {
		t.Fatalf("old crashed lease loaded bytes: %v", err)
	}
}

func TestOriginalDownloadRetryBudgetDoesNotConsumeFailureAllowance(t *testing.T) {
	s, clock, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	originalDownloadTestSource(t, s, p, "budget", MessageImage)
	for i := 0; i < 10; i++ {
		w := originalDownloadTestClaim(t, port)
		if w.Attempts != 1 {
			t.Fatalf("budget pressure exhausted real attempt allowance: %d", w.Attempts)
		}
		if err := port.RetryOriginalDownload(context.Background(), OriginalDownloadRetry{Lease: w.Lease, Code: "waiting_for_disk_budget"}); err != nil {
			t.Fatal(err)
		}
		var due int64
		if err := s.readDB.QueryRow("SELECT next_due FROM extension_original_downloads WHERE media_id=?", w.Lease.MediaID).Scan(&due); err != nil || due < clock.Load()+int64(time.Minute/time.Millisecond) {
			t.Fatalf("missing budget retry floor: %d %v", due, err)
		}
		originalDownloadTestNoWork(t, port)
		clock.Add(int64(2 * time.Minute / time.Millisecond))
	}
	for i := 1; i <= 8; i++ {
		w := originalDownloadTestClaim(t, port)
		if w.Attempts != i {
			t.Fatalf("unbounded or lost attempt counter: %d != %d", w.Attempts, i)
		}
		if err := port.RetryOriginalDownload(context.Background(), OriginalDownloadRetry{Lease: w.Lease, Code: "media_deadline_exceeded"}); err != nil {
			t.Fatal(err)
		}
		clock.Add(int64(2 * time.Second / time.Millisecond))
	}
	originalDownloadTestNoWork(t, port)
}

func TestOriginalDownloadAuthorizationPrecedesCandidateLimit(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	for i := 0; i < 70; i++ {
		originalDownloadTestSource(t, s, p, fmt.Sprintf("denied-%03d", i), MessageImage)
	}
	e, _, _ := originalDownloadTestSource(t, s, p, "selected", MessageImage)
	e.Key.Chat = "selected-chat"
	e.Key.Message = "selected-chat-original"
	apply(t, s, e, p.Generation)
	var chat ChatID
	if err := s.readDB.QueryRow("SELECT id FROM chats WHERE account=? AND source_chat=?", e.Key.Account, e.Key.Chat).Scan(&chat); err != nil {
		t.Fatal(err)
	}
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{chat}}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately make denied metadata tempting to a queue-first LIMIT. Exact
	// authoritative chat inclusion still excludes every one before ranking.
	for _, query := range []string{
		"UPDATE messages SET state='active' WHERE source_chat='chat'",
		"UPDATE extension_media_associations SET active=1,policy_generation=? WHERE message_id IN(SELECT id FROM messages WHERE source_chat='chat')",
		"UPDATE extension_original_downloads SET state='pending',policy_generation=?,next_due=0 WHERE message_id IN(SELECT id FROM messages WHERE source_chat='chat')",
	} {
		var err error
		if strings.Contains(query, "?") {
			_, err = s.writeDB.Exec(query, p.Generation)
		} else {
			_, err = s.writeDB.Exec(query)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	e.SourceRevision++
	r := apply(t, s, e, p.Generation)
	w := originalDownloadTestClaim(t, port)
	if w.Source.Version.MessageID != r.MessageID {
		t.Fatalf("denied crowd displaced permitted source: %#v", w)
	}
	in, err := port.LoadOriginalDownload(context.Background(), w.Lease)
	if err != nil || in.Key.Chat != e.Key.Chat {
		t.Fatalf("wrong selected source: %#v %v", in, err)
	}
}

func TestOriginalDownloadExactLeaseSourceAndReferencePins(t *testing.T) {
	s, clock, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	e, _, media := originalDownloadTestSource(t, s, p, "pins", MessageImage)
	w := originalDownloadTestClaim(t, port)
	forged := w.Lease
	forged.LeaseID = "caller-forged-lease"
	if _, err := port.LoadOriginalDownload(context.Background(), forged); !errors.Is(err, ErrNoWork) {
		t.Fatalf("forged lease loaded: %v", err)
	}
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET protocol_reference=? WHERE id=?", originalDownloadTestReference(t, MessageImage, "changed-authenticated-reference"), media); err != nil {
		t.Fatal(err)
	}
	if _, err := port.LoadOriginalDownload(context.Background(), w.Lease); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("changed reference loaded under old digest: %v", err)
	}
	if ok, err := port.FinishOriginalDownload(context.Background(), w.Lease); err != nil || ok {
		t.Fatalf("changed reference finished: %v %v", ok, err)
	}
	if _, err := s.CurrentSource(context.Background(), e.Key); err != nil {
		t.Fatalf("optional digest failure poisoned source: %v", err)
	}
	clock.Add(int64(3 * time.Minute / time.Millisecond))
	if _, err := port.LoadOriginalDownload(context.Background(), w.Lease); !errors.Is(err, ErrNoWork) {
		t.Fatalf("expired lease loaded: %v", err)
	}
}

func TestOriginalDownloadCompletionSurvivesCaptionContextPolicyAndEviction(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	e, ref, media := originalDownloadTestSource(t, s, p, "same-reference", MessageImage)
	w := originalDownloadTestClaim(t, port)
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	extensionPublish(t, s, r, 64)
	if ok, err := port.FinishOriginalDownload(context.Background(), w.Lease); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	// Simulate the already authenticated original's processed-cache eviction;
	// scheduling never owns file cleanup or gains read authority from this mark.
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET state='evicting',charged_bytes=0,cleanup_ack=1 WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id='',availability='not_retained' WHERE id=?", media); err != nil {
		t.Fatal(err)
	}
	originalDownloadTestNoWork(t, port)
	var descriptor waE2E.ImageMessage
	if err := proto.Unmarshal(e.Media[0].ProtocolReference, &descriptor); err != nil {
		t.Fatal(err)
	}
	descriptor.Caption = proto.String("protocol-caption-canary")
	descriptor.ContextInfo = &waE2E.ContextInfo{StanzaID: proto.String("reply-context-canary")}
	e.Media[0].ProtocolReference, _ = proto.Marshal(&descriptor)
	e.Media[0].MimeType = "image/jpeg"
	e.Caption, e.SourceRevision, e.Kind = "changed caption", 2, EventEditContent
	r2 := apply(t, s, e, p.Generation)
	if len(r2.MediaIDs) != 1 || r2.MediaIDs[0] == media {
		t.Fatalf("caption edit did not create exact new association: %#v", r2)
	}
	var marker string
	if err := s.readDB.QueryRow("SELECT last_original_sha256 FROM extension_media_associations WHERE id=?", r2.MediaIDs[0]).Scan(&marker); err != nil || marker != strings.Repeat("a", 64) {
		t.Fatalf("same authenticated identity lost completion: %s %v", marker, err)
	}
	originalDownloadTestNoWork(t, port)
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	e.SourceRevision++
	apply(t, s, e, p.Generation)
	originalDownloadTestNoWork(t, port)
	// A changed authenticated direct path is a genuinely different byte intent.
	descriptor.DirectPath = proto.String("/synthetic/genuinely-changed-media")
	e.Media[0].ProtocolReference, _ = proto.Marshal(&descriptor)
	e.SourceRevision++
	r3 := apply(t, s, e, p.Generation)
	newWork := originalDownloadTestClaim(t, port)
	if newWork.Lease.MediaID != r3.MediaIDs[0] {
		t.Fatalf("new authenticated media was incorrectly completed: %#v", newWork)
	}
}

func TestOriginalDownloadLegacyWholeSetProofAndUnknownCompletion(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			s, _, _ := originalDownloadTestArchive(t)
			port := originalDownloadTestPort(t, s)
			p := testPolicy(t, s)
			e, _, media := originalDownloadTestSource(t, s, p, "legacy", MessageImage)
			var set string
			if known {
				if err := s.readDB.QueryRow("SELECT extension_media_identity FROM messages WHERE source_message=?", e.Key.Message).Scan(&set); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.writeDB.Exec("UPDATE extension_original_downloads SET state='complete',completed_sha256=?,protocol_sha256='',descriptor_sha256='',legacy_source_media_identity=? WHERE media_id=?", strings.Repeat("a", 64), set, media); err != nil {
				t.Fatal(err)
			}
			if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET protocol_reference=X'',last_original_sha256=? WHERE id=?", strings.Repeat("a", 64), media); err != nil {
				t.Fatal(err)
			}
			if !known {
				// A genuinely unprepared historical source cannot borrow an
				// authoritative current whole-set digest during cancellation.
				if _, err := s.writeDB.Exec("UPDATE messages SET extension_media_identity='' WHERE account=? AND source_message=?", e.Key.Account, e.Key.Message); err != nil {
					t.Fatal(err)
				}
			}
			p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
			if err != nil {
				t.Fatal(err)
			}
			e.SourceRevision++
			r := apply(t, s, e, p.Generation)
			originalDownloadTestNoWork(t, port)
			var marker string
			if err := s.readDB.QueryRow("SELECT last_original_sha256 FROM extension_media_associations WHERE id=?", r.MediaIDs[0]).Scan(&marker); err != nil {
				t.Fatal(err)
			}
			if known && marker != strings.Repeat("a", 64) {
				t.Fatalf("exact legacy set proof discarded: %s", marker)
			}
			if !known && marker != "" {
				t.Fatalf("unverifiable legacy marker manufactured identity: %s", marker)
			}
		})
	}
	t.Run("retained_protocol_unknown", func(t *testing.T) {
		s, _, _ := originalDownloadTestArchive(t)
		port := originalDownloadTestPort(t, s)
		p := testPolicy(t, s)
		e, _, oldMedia := originalDownloadTestSource(t, s, p, "legacy-retained", MessageImage)
		sha := strings.Repeat("a", 64)
		if _, err := s.writeDB.Exec("UPDATE extension_original_downloads SET state='complete',completed_sha256=?,protocol_sha256='',descriptor_sha256='',legacy_source_media_identity='' WHERE media_id=?", sha, oldMedia); err != nil {
			t.Fatal(err)
		}
		// Retain the current old protocol blob. The per-message pre-clear hook
		// must encounter it before any global policy invalidation removes it.
		if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?", sha, oldMedia); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec("UPDATE messages SET extension_media_identity='' WHERE account=? AND source_message=?", e.Key.Account, e.Key.Message); err != nil {
			t.Fatal(err)
		}
		e.Kind, e.SourceRevision, e.Caption = EventEditContent, 2, "edited caption with the same authenticated media"
		r := apply(t, s, e, p.Generation)
		if len(r.MediaIDs) != 1 || r.MediaIDs[0] == oldMedia {
			t.Fatalf("source edit did not create a new exact association: %#v", r)
		}
		w, err := port.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: 2 * time.Minute})
		if !errors.Is(err, ErrNoWork) || w.Lease.MediaID != "" {
			t.Errorf("unproven completed legacy original was automatically scheduled: %#v %v", w, err)
		}
		var completed, descriptor, marker string
		if err := s.readDB.QueryRow("SELECT completed_sha256,descriptor_sha256 FROM extension_original_downloads WHERE media_id=?", oldMedia).Scan(&completed, &descriptor); err != nil {
			t.Fatal(err)
		}
		if completed != sha || descriptor != "" {
			t.Errorf("pre-clear preparation erased or fabricated historical identity: completed=%q descriptor=%q", completed, descriptor)
		}
		if err := s.readDB.QueryRow("SELECT last_original_sha256 FROM extension_media_associations WHERE id=?", r.MediaIDs[0]).Scan(&marker); err != nil {
			t.Fatal(err)
		}
		if marker != "" {
			t.Errorf("unproven historical completion became a current media identity: %q", marker)
		}
	})
}

func TestOriginalDownloadOptionalMalformedAndOversizedReferencesDoNotPoisonSource(t *testing.T) {
	for _, data := range [][]byte{[]byte("malformed-private-protocol-canary"), bytes.Repeat([]byte{1}, 8193)} {
		t.Run(fmt.Sprint(len(data)), func(t *testing.T) {
			s, _, _ := originalDownloadTestArchive(t)
			port := originalDownloadTestPort(t, s)
			p := testPolicy(t, s)
			e := event("malformed", "keyword survives optional protocol parsing", 1, EventOriginal)
			e.BodyKind = MessageImage
			e.Media = []MediaReference{{Kind: "image", ProtocolReference: data, DeclaredBytes: 64, Availability: "not_retained"}}
			apply(t, s, e, p.Generation)
			originalDownloadTestNoWork(t, port)
			if _, err := s.CurrentSource(context.Background(), e.Key); err != nil {
				t.Fatalf("optional parse failure quarantined source: %v", err)
			}
			if err := s.Drain(context.Background()); err != nil {
				t.Fatalf("optional failure stopped owner writer: %v", err)
			}
		})
	}
}

func TestOriginalDownloadInputBoundsAndCanceledCommandsAreReadOnlyForSafety(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	e, _, _ := originalDownloadTestSource(t, s, p, "bounded", MessageImage)
	for _, c := range []OriginalDownloadClaim{{Account: "", TTL: time.Minute}, {Account: "account", TTL: time.Second - 1}, {Account: "account", TTL: 15*time.Minute + 1}} {
		if _, err := port.ClaimOriginalDownload(context.Background(), c); !errors.Is(err, ErrInvalidExtension) {
			t.Fatalf("invalid scheduling claim accepted: %#v %v", c, err)
		}
	}
	w := originalDownloadTestClaim(t, port)
	for _, retry := range []OriginalDownloadRetry{{Lease: w.Lease, Code: "caller-canary"}, {Lease: w.Lease, Code: "media_unavailable", After: -1}, {Lease: w.Lease, Code: "media_unavailable", After: 24*time.Hour + 1}} {
		if err := port.RetryOriginalDownload(context.Background(), retry); !errors.Is(err, ErrInvalidExtension) {
			t.Fatalf("invalid retry accepted: %#v %v", retry, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := port.LoadOriginalDownload(ctx, w.Lease); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled optional command result: %v", err)
	}
	if _, err := s.CurrentSource(context.Background(), e.Key); err != nil {
		t.Fatalf("canceled optional command poisoned source: %v", err)
	}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalDownloadPublicationCrashDoesNotRedownloadEvictedOriginal(t *testing.T) {
	s, clock, dir := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	_, ref, media := originalDownloadTestSource(t, s, p, "reply-lost", MessageImage)
	originalDownloadTestClaim(t, port)
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	extensionPublish(t, s, r, 64)
	// The process loses the scheduling acknowledgement after publication. Its
	// physical original later leaves cache; neither event permits a fresh fetch.
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET state='evicting',charged_bytes=0,cleanup_ack=1 WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id='',availability='not_retained' WHERE id=?", media); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(3 * time.Minute / time.Millisecond))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	port = originalDownloadTestPort(t, reopened)
	originalDownloadTestNoWork(t, port)
	var state, marker string
	if err := reopened.readDB.QueryRow("SELECT state,completed_sha256 FROM extension_original_downloads WHERE media_id=?", media).Scan(&state, &marker); err != nil || state != "complete" || marker != strings.Repeat("a", 64) {
		t.Fatalf("publication completion was not atomic/durable: %s %s %v", state, marker, err)
	}
}
