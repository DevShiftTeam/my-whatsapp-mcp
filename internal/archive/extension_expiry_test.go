//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestLateClockExpiryClearsSourcePlaintext(t *testing.T) {
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	var advance atomic.Bool
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time {
		if advance.Load() && calls.Add(1) >= 3 {
			return base.Add(2 * time.Second)
		}
		return base
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := testPolicy(t, s)
	e := event("late-expiry", "unique late expiry plaintext", 1, EventOriginal)
	e.Caption = "unique late expiry caption"
	e.BodyKind = MessageImage
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("late expiry private protocol canary"), Availability: "not_retained"}}
	e.ReplyTo = &SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "late-expiry-private-reply-canary", FromMe: true}
	e.ExpiresAt = base.Add(time.Second)
	before, err := s.readSafety("account")
	if err != nil {
		t.Fatal(err)
	}
	advance.Store(true)
	r := apply(t, s, e, p.Generation)
	var state, body, caption, normalBody, normalCaption, hash, reply, mediaIdentity string
	var replyBytes, replyFromMe, mediaKeys int
	err = s.readDB.QueryRow("SELECT state,body,caption,normalized_body,normalized_caption,content_hash,reply_message,extension_media_identity,length(reply_account)+length(reply_chat)+length(reply_sender),reply_from_me FROM messages WHERE id=?", r.Version.MessageID).Scan(&state, &body, &caption, &normalBody, &normalCaption, &hash, &reply, &mediaIdentity, &replyBytes, &replyFromMe)
	if err != nil {
		t.Fatal(err)
	}
	if state != "expired" || body != "" || caption != "" || normalBody != "" || normalCaption != "" || hash != "" || reply != "" || mediaIdentity != "" || replyBytes != 0 || replyFromMe != 0 {
		t.Fatalf("expired source retained content: state=%s text_bytes=%d", state, len(body)+len(caption)+len(normalBody)+len(normalCaption)+len(hash)+len(reply))
	}
	after, err := s.readSafety("account")
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatal("late expiry skipped the revocation fence")
	}
	if err = s.readDB.QueryRow("SELECT count(*) FROM extension_media_associations WHERE message_id=? AND (active<>0 OR length(protocol_reference)>0)", r.MessageID).Scan(&mediaKeys); err != nil || mediaKeys != 0 {
		t.Fatalf("late expiry persisted media authority: %d %v", mediaKeys, err)
	}
	if _, err = s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{e.Body, e.Caption, e.ReplyTo.Message, string(e.Media[0].ProtocolReference)} {
		if bytes.Contains(onDisk, []byte(canary)) {
			t.Fatal("late expiry retained a private field in the checkpointed database")
		}
	}
	reopened, err := Open(context.Background(), dir, Options{Now: func() time.Time { return base.Add(2 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.CurrentSource(context.Background(), e.Key); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("expired source resurrected on reopen: %v", err)
	}
}

func TestStaleSecurityExpiryClearsExistingReplyAndMediaKeys(t *testing.T) {
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	e := event("stale-security-expiry", "existing original body", 1, EventOriginal)
	e.Caption = "existing original caption"
	e.ReplyTo = &SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "quoted-source", FromMe: true}
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic existing protocol key"), Availability: "not_retained"}}
	r := apply(t, s, e, p.Generation)
	if len(r.MediaIDs) != 1 {
		t.Fatal("source association fixture absent")
	}
	stale := e
	stale.SourceRevision = 0
	stale.Body, stale.Caption, stale.Media, stale.ReplyTo = "", "", nil, nil
	stale.ExpiresAt = time.UnixMilli(clock.Load()).Add(-time.Millisecond)
	apply(t, s, stale, p.Generation)
	var state string
	var privateBytes, replyFromMe, keys int
	err := s.readDB.QueryRow(`SELECT state,length(CAST(body AS BLOB))+length(CAST(caption AS BLOB))+length(CAST(normalized_body AS BLOB))+length(CAST(normalized_caption AS BLOB))+length(content_hash)+length(extension_media_identity)+length(reply_account)+length(reply_chat)+length(reply_sender)+length(reply_message),reply_from_me FROM messages WHERE id=?`, r.MessageID).Scan(&state, &privateBytes, &replyFromMe)
	if err != nil || state != "expired" || privateBytes != 0 || replyFromMe != 0 {
		t.Fatalf("stale expired source retained private content: state=%s bytes=%d fromme=%d err=%v", state, privateBytes, replyFromMe, err)
	}
	if err = s.readDB.QueryRow("SELECT COUNT(*) FROM extension_media_associations WHERE message_id=? AND (active<>0 OR length(protocol_reference)>0)", r.MessageID).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("stale expired source retained media authority: %d %v", keys, err)
	}
}
