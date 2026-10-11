//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSourceHashV1FieldCollisionCannotBecomeDuplicate(t *testing.T) {
	for _, name := range []string{"same_protocol", "higher_protocol", "stale_protocol", "normal_duplicate"} {
		t.Run(name, func(t *testing.T) {
			s := testArchive(t)
			policy := testPolicy(t, s)
			original := event("field-collision", "a\x00b", 5, EventOriginal)
			original.Caption = "c"
			before := apply(t, s, original, policy.Generation)
			incoming := original
			incoming.Body, incoming.Caption = "a", "b\x00c"
			if hashEvent(original) != "be4a0a86fa3233c0ec8e8af753e64672041f25975f190c98194c66d85c1ddecc" || hashEvent(original) != hashEvent(incoming) {
				t.Fatal("v1 delimiter hash interpretation changed")
			}
			switch name {
			case "higher_protocol":
				incoming.SourceRevision++
			case "stale_protocol":
				incoming.SourceRevision--
			case "normal_duplicate":
				incoming = original
			}
			result, err := s.ApplySourceEvent(context.Background(), incoming, policy.Generation)
			var state, body, caption, hash string
			var revision uint64
			if readErr := s.readDB.QueryRow("SELECT state,body,caption,content_hash,local_revision FROM messages WHERE id=?", before.MessageID).Scan(&state, &body, &caption, &hash, &revision); readErr != nil {
				t.Fatal(readErr)
			}
			switch name {
			case "same_protocol":
				if !errors.Is(err, ErrSourceRevisionConflict) || state != "invalidated" || body != "" || caption != "" || hash != "" || revision <= before.Version.Revision {
					t.Fatalf("same protocol field collision did not fail closed: state=%s revision=%d err=%v", state, revision, err)
				}
			case "higher_protocol":
				if err != nil || state != "active" || body != incoming.Body || caption != incoming.Caption || hash != hashEvent(incoming) || revision <= before.Version.Revision || result.Outcome == OutcomeDuplicate {
					t.Fatalf("higher protocol retained wrong field allocation: state=%s revision=%d err=%v", state, revision, err)
				}
			case "stale_protocol", "normal_duplicate":
				want := OutcomeStale
				if name == "normal_duplicate" {
					want = OutcomeDuplicate
				}
				if err != nil || state != "active" || body != original.Body || caption != original.Caption || revision != before.Version.Revision || result.Outcome != want {
					t.Fatalf("%s changed authoritative fields or version: state=%s revision=%d err=%v", name, state, revision, err)
				}
			}
		})
	}
}

func TestSourceHashV1MediaCollisionCannotRetainOldArtifactAuthority(t *testing.T) {
	for _, name := range []string{"mime_boundary", "ordered_list_boundary"} {
		t.Run(name, func(t *testing.T) {
			s := testArchive(t)
			policy := testPolicy(t, s)
			original := event("media-collision", "image caption", 5, EventOriginal)
			original.BodyKind = MessageImage
			original.Media = []MediaReference{{Kind: "image", MimeType: "a\x00b", ProtocolReference: []byte("c"), Availability: "not_retained"}}
			incoming := original
			incoming.Media = []MediaReference{{Kind: "image", MimeType: "a", ProtocolReference: []byte("b\x00c"), Availability: "not_retained"}}
			if name == "ordered_list_boundary" {
				original.Media = []MediaReference{
					{Kind: "image", MimeType: "image/png", ProtocolReference: []byte("c"), Availability: "not_retained"},
					{Kind: "image", MimeType: "image/png", ProtocolReference: []byte("d"), Availability: "not_retained"},
				}
				incoming.Media = []MediaReference{{Kind: "image", MimeType: "image/png", ProtocolReference: []byte("c\x00image\x00image/png\x00d"), Availability: "not_retained"}}
			}
			if err := validateEvent(original); err != nil {
				t.Fatal(err)
			}
			if err := validateEvent(incoming); err != nil || hashEvent(original) != hashEvent(incoming) {
				t.Fatalf("fixture does not preserve accepted v1 collision: %v", err)
			}
			before := apply(t, s, original, policy.Generation)
			source, err := s.CurrentSource(context.Background(), original.Key)
			if err != nil || len(before.MediaIDs) == 0 {
				t.Fatalf("initial media unavailable: %v", err)
			}
			reservation := extensionAdmit(t, s, source, before.MediaIDs[0], BudgetOriginals, 4)
			extensionPublish(t, s, reservation, 4)
			result, err := s.ApplySourceEvent(context.Background(), incoming, policy.Generation)
			if !errors.Is(err, ErrSourceRevisionConflict) || result.Outcome == OutcomeDuplicate {
				t.Fatalf("media collision accepted as duplicate: outcome=%s err=%v", result.Outcome, err)
			}
			var live, privateBytes, links int
			if err = s.readDB.QueryRow("SELECT coalesce(sum(active),0),coalesce(sum(length(protocol_reference)),0),coalesce(sum(artifact_id<>''),0) FROM extension_media_associations WHERE message_id=?", before.MessageID).Scan(&live, &privateBytes, &links); err != nil {
				t.Fatal(err)
			}
			var state, identity string
			if err = s.readDB.QueryRow("SELECT state FROM extension_media_artifacts WHERE id=?", reservation.ArtifactID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if err = s.readDB.QueryRow("SELECT extension_media_identity FROM messages WHERE id=?", before.MessageID).Scan(&identity); err != nil {
				t.Fatal(err)
			}
			if live != 0 || privateBytes != 0 || links != 0 || state != "evicting" || identity != "" || extensionMediaCharge(t, s, BudgetOriginals) != 4 {
				t.Fatalf("collision retained old media authority or released cleanup charge: live=%d bytes=%d links=%d state=%s identity=%s", live, privateBytes, links, state, identity)
			}
		})
	}
}

func TestLegacyMediaIdentityRefreshInvalidatesOldRevisionBeforeReuse(t *testing.T) {
	s := testArchive(t)
	policy := testPolicy(t, s)
	original := event("legacy-media-identity", "image caption", 5, EventOriginal)
	original.BodyKind = MessageImage
	original.Media = []MediaReference{{Kind: "image", MimeType: "image/png", ProtocolReference: []byte("legacy key"), Availability: "not_retained"}}
	before := apply(t, s, original, policy.Generation)
	source, err := s.CurrentSource(context.Background(), original.Key)
	if err != nil || len(before.MediaIDs) != 1 {
		t.Fatalf("initial source: %v", err)
	}
	reservation := extensionAdmit(t, s, source, before.MediaIDs[0], BudgetOriginals, 4)
	extensionPublish(t, s, reservation, 4)
	// Simulate the default installed by 005 for a source written before it.
	if _, err = s.writeDB.Exec("UPDATE messages SET extension_media_identity='' WHERE id=?", before.MessageID); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplySourceEvent(context.Background(), original, policy.Generation)
	if err != nil || result.Outcome == OutcomeDuplicate || result.Version.Revision <= before.Version.Revision || len(result.MediaIDs) != 1 || result.MediaIDs[0] == before.MediaIDs[0] {
		t.Fatalf("unknown legacy identity reused old association: outcome=%s revision=%d err=%v", result.Outcome, result.Version.Revision, err)
	}
	if _, err = s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: source, MediaID: before.MediaIDs[0], Budget: BudgetDecoded, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("legacy source version retained decoded authority: %v", err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 4 {
		t.Fatal("legacy refresh released charge before file cleanup")
	}
}

func TestSourceMediaIdentityLengthDelimitedGolden(t *testing.T) {
	refs := []MediaReference{
		{Kind: "image", MimeType: "image/png", ProtocolReference: []byte("c")},
		{Kind: "image", MimeType: "image/png", ProtocolReference: []byte("d")},
	}
	identity, err := sourceMediaIdentity(refs)
	if err != nil || identity != "18f00468f6385d2ae45f76019f69198ffccce1a8c415f8674437ee68b6326149" {
		t.Fatalf("media identity encoding changed: %s %v", identity, err)
	}
	merged := []MediaReference{{Kind: "image", MimeType: "image/png", ProtocolReference: []byte("c\x00image\x00image/png\x00d")}}
	other, err := sourceMediaIdentity(merged)
	if err != nil || other == identity {
		t.Fatal("ordered media boundaries collided")
	}
}
