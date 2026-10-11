//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func documentReference(t *testing.T, length uint64) []byte {
	t.Helper()
	ref, err := proto.Marshal(&waE2E.DocumentMessage{DirectPath: proto.String("/synthetic-document"), MediaKey: bytes.Repeat([]byte{1}, 32), FileSHA256: bytes.Repeat([]byte{2}, 32), FileEncSHA256: bytes.Repeat([]byte{3}, 32), FileLength: proto.Uint64(length)})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func documentEvent(t *testing.T, name string) SourceEvent {
	t.Helper()
	e := event(name, "", 1, EventOriginal)
	e.BodyKind = MessageDocument
	e.Caption = "proposal for the next meeting"
	e.Media = []MediaReference{{Kind: "document", ProtocolReference: documentReference(t, 64), MimeType: "application/pdf", Filename: "הצעת מחיר.pdf", DeclaredBytes: 64, Availability: "not_retained"}}
	return e
}
func markLegacyDocument(t *testing.T, s *Store, e SourceEvent, p ArchivePolicy) ApplyResult {
	t.Helper()
	r := apply(t, s, e, p.Generation)
	if _, err := s.writeDB.Exec(`INSERT INTO extension_document_promotions(message_id,account,local_revision,protocol_revision,content_hash,media_identity) SELECT id,account,local_revision,protocol_revision,content_hash,extension_media_identity FROM messages WHERE id=?`, r.MessageID); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestDocumentEnrichmentPromotesOnlyPreUpgradeExactLegacyOriginal(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	doc := documentEvent(t, "historical-document")
	reply := event("reply-target", "meeting context", 1, EventOriginal).Key
	doc.ReplyTo = &reply
	legacy := doc
	legacy.BodyKind = MessageUnsupported
	legacy.Body = ""
	legacy.Caption = ""
	legacy.Media = nil
	before := markLegacyDocument(t, s, legacy, p)
	r, err := s.ApplySourceEvent(context.Background(), doc, p.Generation)
	if err != nil || r.Outcome != OutcomeApplied || r.MessageID != before.MessageID || r.Version.Revision != before.Version.Revision+1 || len(r.MediaIDs) != 1 {
		t.Fatalf("legacy document was not enriched atomically: %#v %v", r, err)
	}
	var protocol int64
	var kind, caption string
	var markers int
	if err = s.readDB.QueryRow(`SELECT protocol_revision,body_kind,caption FROM messages WHERE id=?`, r.MessageID).Scan(&protocol, &kind, &caption); err != nil || protocol != legacy.SourceRevision || kind != "document" || caption != doc.Caption {
		t.Fatal("document enrichment changed protocol order or lost fields", err)
	}
	if err = s.readDB.QueryRow(`SELECT count(*) FROM extension_document_promotions WHERE message_id=?`, r.MessageID).Scan(&markers); err != nil || markers != 0 {
		t.Fatal("promotion evidence was reusable", err)
	}
	duplicate, err := s.ApplySourceEvent(context.Background(), doc, p.Generation)
	if err != nil || duplicate.Outcome != OutcomeDuplicate || duplicate.Version != r.Version || len(duplicate.MediaIDs) != 1 || duplicate.MediaIDs[0] != r.MediaIDs[0] {
		t.Fatal("enriched replay did not retain source identity", err)
	}
	work, err := s.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: time.Minute})
	if err != nil || work.Kind != MessageDocument || work.Lease.MediaID != r.MediaIDs[0] {
		t.Fatalf("enriched document did not enter guarded download queue: %#v %v", work, err)
	}
}

func TestDocumentEnrichmentRefusesChangedLegacyEvidenceAndFutureUnsupported(t *testing.T) {
	for _, name := range []string{"future unsupported", "different reply", "changed date", "edited", "view once", "existing association", "changed marker hash", "deleted", "expired"} {
		t.Run(name, func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			doc := documentEvent(t, "legacy-guard")
			legacy := doc
			legacy.BodyKind = MessageUnsupported
			legacy.Caption = ""
			legacy.Media = nil
			before := apply(t, s, legacy, p.Generation)
			if name != "future unsupported" {
				if _, err := s.writeDB.Exec(`INSERT INTO extension_document_promotions SELECT id,account,local_revision,protocol_revision,content_hash,extension_media_identity FROM messages WHERE id=?`, before.MessageID); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "different reply":
				r := event("another-target", "", 1, EventOriginal).Key
				doc.ReplyTo = &r
			case "changed date":
				doc.OriginalSentAt = doc.OriginalSentAt.Add(time.Hour)
			case "edited":
				if _, err := s.writeDB.Exec(`UPDATE messages SET edited=1 WHERE id=?`, before.MessageID); err != nil {
					t.Fatal(err)
				}
			case "view once":
				doc.Media[0].ViewOnce = true
			case "existing association":
				if _, err := s.writeDB.Exec(`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,ordinal,kind,protocol_reference,declared_bytes,availability) SELECT 'unexpected','account',id,local_revision,?,content_hash,0,'image',X'01',1,'not_retained' FROM messages WHERE id=?`, p.Generation, before.MessageID); err != nil {
					t.Fatal(err)
				}
			case "changed marker hash":
				if _, err := s.writeDB.Exec(`UPDATE extension_document_promotions SET content_hash=? WHERE message_id=?`, strings.Repeat("f", 64), before.MessageID); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				del := legacy
				del.Kind = EventDelete
				del.SourceRevision = 2
				apply(t, s, del, p.Generation)
			case "expired":
				doc.ExpiresAt = doc.OriginalSentAt
			}
			r, err := s.ApplySourceEvent(context.Background(), doc, p.Generation)
			if err == nil && r.Outcome == OutcomeApplied && len(r.MediaIDs) > 0 {
				t.Fatalf("%s was promoted despite changed evidence", name)
			}
			if err != nil && !errors.Is(err, ErrSourceRevisionConflict) {
				t.Fatal(err)
			}
			var active int
			if err = s.readDB.QueryRow(`SELECT count(*) FROM extension_media_associations WHERE message_id=? AND kind='document' AND active=1`, before.MessageID).Scan(&active); err != nil || active != 0 {
				t.Fatal("rejected source gained document authority", err)
			}
		})
	}
}

func TestDocumentFilenameIdentityDoesNotChangeImageVoiceV1(t *testing.T) {
	for _, kind := range []string{"image", "voice"} {
		refs := []MediaReference{{Kind: kind, MimeType: "fixture/type", ProtocolReference: []byte{1, 2, 3}}}
		before, err := sourceMediaIdentity(refs)
		if err != nil {
			t.Fatal(err)
		}
		refs[0].Filename = "optional display metadata"
		after, err := sourceMediaIdentity(refs)
		if err != nil || before != after {
			t.Fatal("existing image/voice v1 identity changed", err)
		}
	}
	refs := []MediaReference{{Kind: "document", MimeType: "application/pdf", ProtocolReference: []byte{1, 2, 3}, Filename: "a.pdf"}}
	before, err := sourceMediaIdentity(refs)
	if err != nil {
		t.Fatal(err)
	}
	refs[0].Filename = "b.pdf"
	after, err := sourceMediaIdentity(refs)
	if err != nil || before == after {
		t.Fatal("document identity did not bind filename", err)
	}
	for _, bad := range []string{strings.Repeat("x", 256), "bad\x00.pdf", string([]byte{0xff})} {
		refs[0].Filename = bad
		if _, err = sourceMediaIdentity(refs); !errors.Is(err, ErrInvalidEvent) {
			t.Fatal("invalid document name accepted", err)
		}
	}
}
