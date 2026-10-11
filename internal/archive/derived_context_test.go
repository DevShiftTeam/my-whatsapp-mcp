//go:build sqlite_fts5

package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func derivedContextProvenance(kind DocumentKind) DerivedProvenance {
	p := DerivedProvenance{Kind: kind, MediaID: "context-media", ProfileID: "context-ocr", ExtractorVersion: "fixture-v1", OffsetKind: "none"}
	if kind == DocumentCaption {
		p.ProfileID, p.PromptVersion = "context-caption", "fixture-caption-v1"
	} else if kind == DocumentTranscript {
		p.ProfileID, p.OffsetKind, p.StartMS, p.EndMS = "context-transcript", "slice", 1250, 3500
	}
	return p
}

func derivedContextDocument(t *testing.T, s *Store, p ArchivePolicy, generation IndexGeneration, id DocumentID, text string, kind DocumentKind, members []MessageID, provenance DerivedProvenance) DerivedProvenance {
	t.Helper()
	if kind != DocumentWindow {
		seed := provenance
		seed.Kind = kind
		seedText := text
		if len(seedText) > 64<<10 {
			seedText = "Bounded valid root fixture"
		}
		bound := candidateRoot(t, s, p, string(id), seedText, members, seed)
		// Preserve an intentionally mismatched provenance kind while binding
		// its media to the actual document kind for fail-closed read tests.
		bound.Kind = provenance.Kind
		provenance = bound
		if len(text) > 64<<10 {
			rootReaderRewriteText(t, s, id, text)
		}
	}
	encoded, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if kind != DocumentWindow {
		if _, err = s.writeDB.Exec(`UPDATE extension_roots SET provenance_json=? WHERE id=?`, string(encoded), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_documents(id,generation,kind,text,normalized_text,provenance_json,spans_json,passage_tokens,content_hash,source_count,origin_document_id,origin_end_byte)
 VALUES(?,?,?,?,?,?,'[]',4,?,?,?,?)`, id, generation.ID, kind, text, NormalizeLexical(text), string(encoded), extensionExtractionSHA([]byte(text)), len(members), id, len(text)); err != nil {
		t.Fatal(err)
	}
	for ordinal, member := range members {
		if _, err = s.writeDB.Exec(`INSERT INTO extension_document_sources(document_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal)
 SELECT ?,account,id,local_revision,?,expires_at,content_hash,? FROM messages WHERE id=?`, id, p.Generation, ordinal, member); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_document_fts(rowid,normalized_text) SELECT rowid,normalized_text FROM extension_documents WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	return provenance
}

func derivedContextMessage(t *testing.T, page ContextPage, id MessageID) MessageView {
	t.Helper()
	for _, message := range page.Messages {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("context omitted requested source %q: %#v", id, page.Messages)
	return MessageView{}
}

func TestDerivedContextKeepsOriginalFieldsAndExplicitGeneratedProvenance(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("context-original", "allowed", "Original logical-order שָׁלוֹם", now.Add(-time.Minute))
	event.Caption, event.BodyKind = "Original image caption", MessageImage
	id := readerApply(t, s, p, event)
	if err := s.SetSourceNames(context.Background(), SourceNames{Account: event.Key.Account, Chat: event.Key.Chat, Sender: event.Key.Sender, ChatName: "Original Chat", SenderName: "Original Sender"}); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, id))
	baseline, err := s.Context(context.Background(), scope, ContextRequest{MessageID: id})
	if err != nil {
		t.Fatal(err)
	}
	expectedSource := guardedExpectedSource(t, s, p, event, id)
	generation := candidateIndex(t, s)
	ocr, caption, transcript := derivedContextProvenance(DocumentOCR), derivedContextProvenance(DocumentCaption), derivedContextProvenance(DocumentTranscript)
	ocr = derivedContextDocument(t, s, p, generation, "context-ocr", "Generated OCR טקסט", DocumentOCR, []MessageID{id}, ocr)
	caption = derivedContextDocument(t, s, p, generation, "context-caption", "Generated media caption תיאור", DocumentCaption, []MessageID{id}, caption)
	transcript = derivedContextDocument(t, s, p, generation, "context-transcript", "Generated transcript תמלול", DocumentTranscript, []MessageID{id}, transcript)
	derivedContextDocument(t, s, p, generation, "context-window", "WINDOW_MUST_NOT_BE_GENERATED", DocumentWindow, []MessageID{id}, DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"})
	inactive, err := s.EnsureIndex(context.Background(), indexTestVersion("context-building-index"))
	if err != nil {
		t.Fatal(err)
	}
	building := derivedContextDocument(t, s, p, inactive, "context-building-ocr", "Generated root from building index", DocumentOCR, []MessageID{id}, ocr)
	// The fixture's typed original publication truthfully changes cache
	// availability; generated excerpts must preserve every other original field.
	baseline.Messages[0].MediaAvailability = "retained"
	page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("derived context: %#v %v", page, err)
	}
	view := derivedContextMessage(t, page, id)
	if len(view.Derived) != 4 {
		t.Fatalf("independent generated roots missing or text window leaked: %#v", view.Derived)
	}
	wantTexts := map[DocumentID]string{"context-ocr": "Generated OCR טקסט", "context-caption": "Generated media caption תיאור", "context-transcript": "Generated transcript תמלול", "context-building-ocr": "Generated root from building index"}
	wantProvenance := map[DocumentID]DerivedProvenance{"context-ocr": ocr, "context-caption": caption, "context-transcript": transcript, "context-building-ocr": building}
	for _, excerpt := range view.Derived {
		wantText, exists := wantTexts[excerpt.DocumentID]
		if !exists || excerpt.Text != wantText || excerpt.Provenance != wantProvenance[excerpt.DocumentID] || excerpt.Source != expectedSource.Ref.Version {
			t.Fatalf("generated provenance/source changed: %#v", excerpt)
		}
		delete(wantTexts, excerpt.DocumentID)
	}
	if len(wantTexts) != 0 {
		t.Fatalf("generated document was omitted or duplicated: %#v", wantTexts)
	}
	view.Derived = nil
	original := baseline.Messages[0]
	original.Derived = nil
	if !reflect.DeepEqual(view, original) {
		t.Fatalf("generated text replaced original message fields: got %#v, want %#v", view, original)
	}
	guardedAssertSources(t, guard, expectedSource)
	if guard.Generation != generation.ID || guard.IndexVersionID != generation.Version.ID {
		t.Fatalf("derived context did not bind current active generation: %#v", guard)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("current derived context guard rejected: %v", err)
	}
}

func TestDerivedContextNeverDisplaysHiddenEmbeddingInput(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("context-original", "allowed", "Original permitted context", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	expectedSource := guardedExpectedSource(t, s, p, event, id)
	generation := candidateIndex(t, s)
	provenance := derivedContextProvenance(DocumentOCR)
	provenance = derivedContextDocument(t, s, p, generation, "99-visible-ocr", "Visible generated OCR", DocumentOCR, []MessageID{id}, provenance)
	// The hidden copied input looks otherwise valid and sorts ahead of the
	// visible document. FTS and vector rows cannot make it public context.
	rootReaderHiddenDocument(t, s, p, generation, "00-hidden-embedding-input", "HIDDEN_EMBEDDING_INPUT_CANARY", []MessageID{id}, provenance, "99-visible-ocr")
	scope := readerScope(p, readerChat(t, s, id))
	page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Body != event.Body {
		t.Fatalf("hidden-input context lost permitted original: %#v %v", page, err)
	}
	if len(page.Messages[0].Derived) != 1 || page.Messages[0].Derived[0].DocumentID != "99-visible-ocr" || page.Messages[0].Derived[0].Text != "Visible generated OCR" || strings.Contains(string(guardedJSON(t, page)), "HIDDEN_EMBEDDING_INPUT_CANARY") {
		t.Fatalf("hidden copied embedding input became generated context: %#v", page.Messages[0].Derived)
	}
	guardedAssertSources(t, guard, expectedSource)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("visible-only generated guard rejected: %v", err)
	}
}

func TestDerivedContextRequiresEveryMemberBeforeIncludingDocument(t *testing.T) {
	for _, scenario := range []string{"out_of_scope_member", "excluded_member", "date_member", "policy_date_member", "rolling_member", "expired_member", "deleted_member"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			anchor := readerEvent("anchor", "allowed", "Original permitted anchor", now.Add(-time.Minute))
			extra := readerEvent("extra", "other", "Other original contributor", now.Add(-2*time.Hour))
			extra.Key.Sender = "other-private-sender@s.whatsapp.net"
			if scenario == "expired_member" {
				extra.ExpiresAt = now.Add(time.Minute)
			}
			anchorID, extraID := readerApply(t, s, p, anchor), readerApply(t, s, p, extra)
			anchorChat, extraChat := readerChat(t, s, anchorID), readerChat(t, s, extraID)
			extraRef := guardedExpectedSource(t, s, p, extra, extraID).Ref
			if scenario == "excluded_member" || scenario == "policy_date_member" {
				policy := ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{anchorChat}}
				if scenario == "policy_date_member" {
					policy = ArchivePolicy{DateMode: DateSince, ImportStart: now.Add(-time.Hour), ChatMode: ChatsAll}
				}
				var err error
				p, err = s.SetPolicy(context.Background(), policy, p.Generation)
				if err != nil {
					t.Fatal(err)
				}
			}
			generation := candidateIndex(t, s)
			provenance := derivedContextProvenance(DocumentOCR)
			derivedContextDocument(t, s, p, generation, "99-allowed-ocr", "Permitted generated OCR", DocumentOCR, []MessageID{anchorID}, provenance)
			// Denied documents sort before the allowed control and outnumber
			// the per-message limit. Authorization after LIMIT would starve it.
			for n := 0; n < 9; n++ {
				id := DocumentID(fmt.Sprintf("00-denied-%02d", n))
				members := []MessageID{anchorID, extraID}
				if scenario == "excluded_member" || scenario == "policy_date_member" {
					members = members[:1]
				}
				derivedContextDocument(t, s, p, generation, id, "DENIED_DERIVED_CANARY", DocumentOCR, members, provenance)
				if len(members) == 1 {
					// Preserve a structurally valid historical source pin whose
					// contributor is now excluded. It must stay wholly private.
					if _, err := s.writeDB.Exec(`INSERT INTO extension_root_sources(root_id,account,message_id,local_revision,policy_generation,content_hash,expires_at,ordinal) VALUES(?,?,?,?,?,?,?,1)`, id, extraRef.Account, extraID, extraRef.Version.Revision, p.Generation, extraRef.ContentHash, unixms(extraRef.Version.ExpiresAt)); err != nil {
						t.Fatal(err)
					}
					if _, err := s.writeDB.Exec(`INSERT INTO extension_document_sources(document_id,account,message_id,local_revision,policy_generation,content_hash,expires_at,ordinal) VALUES(?,?,?,?,?,?,?,1)`, id, extraRef.Account, extraID, extraRef.Version.Revision, p.Generation, extraRef.ContentHash, unixms(extraRef.Version.ExpiresAt)); err != nil {
						t.Fatal(err)
					}
					if _, err := s.writeDB.Exec(`UPDATE extension_roots SET source_count=2 WHERE id=?`, id); err != nil {
						t.Fatal(err)
					}
					if _, err := s.writeDB.Exec(`UPDATE extension_documents SET source_count=2 WHERE id=?`, id); err != nil {
						t.Fatal(err)
					}
				}
			}
			scope := readerScope(p, anchorChat, extraChat)
			switch scenario {
			case "out_of_scope_member":
				scope.Chats = []ChatID{anchorChat}
			case "date_member":
				scope.From = now.Add(-time.Hour)
			case "rolling_member":
				scope.Rolling = time.Hour
			case "expired_member":
				now = now.Add(2 * time.Minute)
			case "deleted_member":
				extra.Kind, extra.SourceRevision, extra.Body = EventDelete, 2, ""
				readerApply(t, s, p, extra)
			}
			page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: anchorID})
			if err != nil || len(page.Messages) != 1 {
				t.Fatalf("permitted original lost while filtering derived membership: %#v %v", page, err)
			}
			view := derivedContextMessage(t, page, anchorID)
			if len(view.Derived) != 1 || view.Derived[0].DocumentID != "99-allowed-ocr" || view.Derived[0].Text != "Permitted generated OCR" || strings.Contains(string(guardedJSON(t, page)), "DENIED_DERIVED_CANARY") {
				t.Fatalf("mixed document partly included or allowed control missing: %#v", view.Derived)
			}
			guardedAssertSources(t, guard, guardedExpectedSource(t, s, p, anchor, anchorID))
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatalf("filtered derived response rejected: %v", err)
			}
		})
	}
}

func TestDerivedContextExcludesDeniedPayloadsBeforeDecodingOrLimits(t *testing.T) {
	for _, denial := range []string{"mixed_member", "hidden_input"} {
		for _, payload := range []string{"malformed_provenance", "oversized_text", "oversized_provenance"} {
			t.Run(denial+"/"+payload, func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				anchor := readerEvent("anchor", "allowed", "Original permitted anchor", now.Add(-time.Minute))
				extra := readerEvent("extra", "denied", "Denied contributor", now.Add(-time.Minute))
				extra.Key.Sender = "denied-private-sender@s.whatsapp.net"
				anchorID, extraID := readerApply(t, s, p, anchor), readerApply(t, s, p, extra)
				expected := guardedExpectedSource(t, s, p, anchor, anchorID)
				generation := candidateIndex(t, s)
				provenance := derivedContextProvenance(DocumentOCR)
				provenance = derivedContextDocument(t, s, p, generation, "99-visible-control", "Permitted generated control", DocumentOCR, []MessageID{anchorID}, provenance)
				encoded, err := json.Marshal(provenance)
				if err != nil {
					t.Fatal(err)
				}
				text, raw := "DENIED_PAYLOAD_CANARY", string(encoded)
				switch payload {
				case "malformed_provenance":
					raw = `{"Kind":`
				case "oversized_text":
					text += strings.Repeat("x", 64<<10)
				case "oversized_provenance":
					raw += strings.Repeat(" ", 4097-len(encoded))
				}
				// Nine lower-sorting documents would exhaust the eight-document
				// allowance if exclusion ran after caps or payload decoding.
				for n := 0; n < 9; n++ {
					id := DocumentID(fmt.Sprintf("00-denied-%02d", n))
					if denial == "hidden_input" {
						rootReaderHiddenDocument(t, s, p, generation, string(id), text, []MessageID{anchorID}, provenance, "99-visible-control")
						if _, err = s.writeDB.Exec(`UPDATE extension_documents SET provenance_json=? WHERE id=?`, raw, id); err != nil {
							t.Fatal(err)
						}
					} else {
						derivedContextDocument(t, s, p, generation, id, "DENIED_PAYLOAD_CANARY", DocumentOCR, []MessageID{anchorID, extraID}, provenance)
						rootReaderRewriteText(t, s, id, text)
						rootReaderCorrupt(t, s, `UPDATE extension_roots SET provenance_json=? WHERE id=?`, raw, id)
					}
				}
				scope := readerScope(p, readerChat(t, s, anchorID))
				page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: anchorID})
				if err != nil || len(page.Messages) != 1 || page.Messages[0].Body != anchor.Body {
					t.Fatalf("denied payload affected permitted original: %#v %v", page, err)
				}
				view := page.Messages[0]
				if len(view.Derived) != 1 || view.Derived[0].DocumentID != "99-visible-control" || view.Derived[0].Text != "Permitted generated control" || strings.Contains(string(guardedJSON(t, page)), "DENIED_PAYLOAD_CANARY") {
					t.Fatalf("denied payload starved or escaped into allowed context: %#v", view.Derived)
				}
				guardedAssertSources(t, guard, expected)
				if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
					t.Fatalf("allowed-only context guard rejected: %v", err)
				}
			})
		}
	}
}

func TestDerivedContextGuardsNonAnchorContributorsThroughFinalDelivery(t *testing.T) {
	for _, scenario := range []string{"extra_edit", "extra_delete", "extra_chat_rename", "extra_sender_rename", "unrelated_original", "unrelated_rename"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			anchor := readerEvent("anchor", "anchor-chat", "Original context anchor", now.Add(-2*time.Minute))
			extra := readerEvent("extra", "extra-chat", "Original outside visible context", now.Add(-time.Minute))
			extra.Key.Sender = "extra-private-sender@s.whatsapp.net"
			anchorID, extraID := readerApply(t, s, p, anchor), readerApply(t, s, p, extra)
			other := readerEvent("other", "unrelated-chat", "Unrelated source", now)
			other.Key.Sender = "unrelated-private-sender@s.whatsapp.net"
			readerApply(t, s, p, other)
			if err := s.SetSourceNames(context.Background(), SourceNames{Account: extra.Key.Account, Chat: extra.Key.Chat, Sender: extra.Key.Sender, ChatName: "Extra Chat", SenderName: "Extra Sender"}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetSourceNames(context.Background(), SourceNames{Account: other.Key.Account, Chat: other.Key.Chat, Sender: other.Key.Sender, ChatName: "Unrelated Chat", SenderName: "Unrelated Sender"}); err != nil {
				t.Fatal(err)
			}
			scope := readerScope(p, readerChat(t, s, anchorID), readerChat(t, s, extraID))
			want := []SourceReadVersion{guardedExpectedSource(t, s, p, anchor, anchorID), guardedExpectedSource(t, s, p, extra, extraID)}
			generation := candidateIndex(t, s)
			derivedContextDocument(t, s, p, generation, "multi-source-ocr", "Generated from both contributors", DocumentOCR, []MessageID{anchorID, extraID}, derivedContextProvenance(DocumentOCR))
			page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: anchorID})
			if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Derived) != 1 || page.Messages[0].Derived[0].Source.MessageID != anchorID {
				t.Fatalf("multi-source derived context anchor: %#v %v", page, err)
			}
			guardedAssertSources(t, guard, want...)
			for _, contributor := range guard.Sources {
				if contributor.MetadataHash == [32]byte{} {
					t.Fatal("generated contributor lacks its original read metadata hash")
				}
			}
			// Membership does not attach duplicate excerpts to non-anchor rows.
			extraPage, err := s.Context(context.Background(), scope, ContextRequest{MessageID: extraID})
			if err != nil || len(extraPage.Messages) != 1 || len(extraPage.Messages[0].Derived) != 0 {
				t.Fatalf("non-anchor source repeated generated document: %#v %v", extraPage, err)
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatalf("current non-anchor source rejected: %v", err)
			}
			wantError := ErrSourceUnavailable
			switch scenario {
			case "extra_edit", "extra_delete":
				extra.Kind, extra.SourceRevision, extra.Body = EventEditContent, 2, "Edited contributor"
				if scenario == "extra_delete" {
					extra.Kind, extra.Body = EventDelete, ""
				}
				readerApply(t, s, p, extra)
			case "extra_chat_rename", "extra_sender_rename":
				names := SourceNames{Account: extra.Key.Account, Chat: extra.Key.Chat, Sender: extra.Key.Sender, ChatName: "Extra Chat", SenderName: "Extra Sender"}
				if scenario == "extra_chat_rename" {
					names.ChatName = "Renamed Extra Chat"
				} else {
					names.SenderName = "Renamed Extra Sender"
				}
				if err = s.SetSourceNames(context.Background(), names); err != nil {
					t.Fatal(err)
				}
			case "unrelated_original":
				readerApply(t, s, p, readerEvent("new-unrelated", "new-unrelated-chat", "New unrelated original", now))
				wantError = ""
			case "unrelated_rename":
				if err = s.SetSourceNames(context.Background(), SourceNames{Account: other.Key.Account, Chat: other.Key.Chat, Sender: other.Key.Sender, ChatName: "Renamed Unrelated Chat", SenderName: "Renamed Unrelated Sender"}); err != nil {
					t.Fatal(err)
				}
				wantError = ""
			}
			_, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard})
			if (wantError == "" && err != nil) || (wantError != "" && !errors.Is(err, wantError)) {
				t.Fatalf("post-return %s ignored generated contributor authority: %v, want %v", scenario, err, wantError)
			}
		})
	}
}

func TestDerivedContextNonAnchorTimeHorizonsRejectWithoutRevisionChange(t *testing.T) {
	for _, horizon := range []string{"expiry", "rolling"} {
		t.Run(horizon, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			anchor := readerEvent("anchor", "anchor-chat", "Original fresh anchor", now.Add(-time.Minute))
			extra := readerEvent("extra", "extra-chat", "Non-anchor time-bound contributor", now.Add(-59*time.Minute))
			extra.Key.Sender = "extra-private-sender@s.whatsapp.net"
			if horizon == "expiry" {
				extra.ExpiresAt = now.Add(time.Minute)
			}
			anchorID, extraID := readerApply(t, s, p, anchor), readerApply(t, s, p, extra)
			want := []SourceReadVersion{guardedExpectedSource(t, s, p, anchor, anchorID), guardedExpectedSource(t, s, p, extra, extraID)}
			generation := candidateIndex(t, s)
			derivedContextDocument(t, s, p, generation, "time-bound-derived", "Generated from time-bound contributor", DocumentOCR, []MessageID{anchorID, extraID}, derivedContextProvenance(DocumentOCR))
			scope := readerScope(p, readerChat(t, s, anchorID), readerChat(t, s, extraID))
			if horizon == "rolling" {
				scope.Rolling = time.Hour
			}
			page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: anchorID})
			if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Derived) != 1 {
				t.Fatalf("initial time-bound derived context: %#v %v", page, err)
			}
			guardedAssertSources(t, guard, want...)
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatalf("unexpired non-anchor guard rejected: %v", err)
			}
			// Only time advances: the visible original remains in scope, while
			// its generated document's extra contributor loses authority.
			now = now.Add(2 * time.Minute)
			for n, event := range []SourceEvent{anchor, extra} {
				metadata, err := s.SourceMetadata(context.Background(), event.Key)
				if err != nil || metadata.Version != want[n].Ref.Version {
					t.Fatalf("time passage changed source revision: %#v %v", metadata, err)
				}
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("post-return non-anchor %s horizon ignored: %v", horizon, err)
			}
		})
	}
}

func TestDerivedContextSourceUnionBoundaryAppliesToAllReadFrames(t *testing.T) {
	for _, capture := range []bool{true, false} {
		t.Run(fmt.Sprintf("capture_sources_%t", capture), func(t *testing.T) {
			frame := &readFrame{captureSources: capture}
			sent := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			const limit = 2048
			for n := 0; n < limit; n++ {
				source := SourceReadVersion{
					Ref:    SourceRef{Account: "account", Version: SourceVersion{MessageID: MessageID(fmt.Sprintf("source-%04d", n)), Revision: 1, PolicyGeneration: 1}, ContentHash: strings.Repeat("a", 64)},
					ChatID: "chat", SenderID: "sender", OriginalSentAt: sent,
				}
				frame.pin(source.Ref.Version.MessageID, source.Ref.Version.Revision)
				if err := frame.captureSource(source, "", ""); err != nil {
					t.Fatalf("bounded union rejected source %d: %v", n, err)
				}
				// Original rows and generated members can contribute the same
				// source; repeated membership must not consume union capacity.
				frame.pin(source.Ref.Version.MessageID, source.Ref.Version.Revision)
				if err := frame.captureSource(source, "", ""); err != nil {
					t.Fatalf("duplicate membership consumed source limit: %v", err)
				}
			}
			if len(frame.pins) != limit || (capture && len(frame.sources) != limit) {
				t.Fatalf("source union did not deduplicate at boundary: pins=%d captures=%d", len(frame.pins), len(frame.sources))
			}
			overflow := SourceReadVersion{
				Ref:    SourceRef{Account: "account", Version: SourceVersion{MessageID: "source-overflow", Revision: 1, PolicyGeneration: 1}, ContentHash: strings.Repeat("a", 64)},
				ChatID: "chat", SenderID: "sender", OriginalSentAt: sent,
			}
			frame.pin(overflow.Ref.Version.MessageID, overflow.Ref.Version.Revision)
			if err := frame.captureSource(overflow, "", ""); !errors.Is(err, ErrLimitExceeded) {
				t.Fatalf("read frame admitted source union above 2048: %v", err)
			}
		})
	}
}

func TestDerivedContextRejectsMalformedAndOversizedAuthorizedPayloads(t *testing.T) {
	for _, scenario := range []string{"broken_json", "kind_mismatch", "ocr_word_offsets", "reversed_transcript_offsets", "invalid_utf8_text", "text_sha_mismatch", "oversized_text", "oversized_provenance"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("anchor", "allowed", "Original valid context", now.Add(-time.Minute))
			id := readerApply(t, s, p, event)
			generation := candidateIndex(t, s)
			kind, provenance := DocumentOCR, derivedContextProvenance(DocumentOCR)
			text := "Generated valid text"
			if scenario == "reversed_transcript_offsets" {
				kind, provenance = DocumentTranscript, derivedContextProvenance(DocumentTranscript)
				provenance.StartMS, provenance.EndMS = 3500, 1250
			} else if scenario == "kind_mismatch" {
				provenance = derivedContextProvenance(DocumentTranscript)
			} else if scenario == "ocr_word_offsets" {
				provenance.OffsetKind, provenance.StartMS, provenance.EndMS = "word", 1, 2
			} else if scenario == "invalid_utf8_text" {
				text = string([]byte{'x', 0xff})
			} else if scenario == "oversized_text" {
				text = strings.Repeat("x", (64<<10)+1)
			}
			provenance = derivedContextDocument(t, s, p, generation, "corrupt-derived", text, kind, []MessageID{id}, provenance)
			if scenario == "text_sha_mismatch" {
				if _, err := s.writeDB.Exec(`UPDATE extension_roots SET text_sha256=? WHERE id='corrupt-derived'`, strings.Repeat("b", 64)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "broken_json" || scenario == "oversized_provenance" {
				raw := `{"Kind":`
				if scenario == "oversized_provenance" {
					valid, err := json.Marshal(provenance)
					if err != nil {
						t.Fatal(err)
					}
					raw = string(valid) + strings.Repeat(" ", 4097-len(valid))
				}
				rootReaderCorrupt(t, s, `UPDATE extension_roots SET provenance_json=? WHERE id='corrupt-derived'`, raw)
			}
			page, guard, err := s.ContextGuarded(context.Background(), readerScope(p, readerChat(t, s, id)), ContextRequest{MessageID: id})
			if scenario == "oversized_text" || scenario == "oversized_provenance" {
				if err != nil || len(page.Messages) != 1 || page.Messages[0].Body != event.Body || len(page.Messages[0].Derived) != 0 || len(guard.roots) != 0 {
					t.Fatalf("structurally oversized root affected permitted original: %#v %#v %v", page, guard, err)
				}
			} else if !errors.Is(err, ErrSourceUnavailable) || !reflect.DeepEqual(page, ContextPage{}) || !reflect.DeepEqual(guard, ResponseGuard{}) {
				t.Fatalf("invalid admitted derived payload escaped: page=%#v guard=%#v error=%v, want SourceUnavailable and zero outputs", page, guard, err)
			}
		})
	}
}

func TestDerivedContextAcceptsExactStoredPayloadLimits(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("anchor", "allowed", "Original center must survive", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	generation := candidateIndex(t, s)
	text := strings.Repeat("x", 64<<10)
	provenance := derivedContextProvenance(DocumentOCR)
	provenance = derivedContextDocument(t, s, p, generation, "exact-derived", text, DocumentOCR, []MessageID{id}, provenance)
	encoded, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(encoded) + strings.Repeat(" ", 4096-len(encoded))
	if _, err = s.writeDB.Exec(`UPDATE extension_roots SET provenance_json=? WHERE id='exact-derived'`, raw); err != nil {
		t.Fatal(err)
	}
	page, guard, err := s.ContextGuarded(context.Background(), readerScope(p, readerChat(t, s, id)), ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Derived) != 1 || page.Messages[0].Body != event.Body || page.Messages[0].Derived[0].Text != text || page.Messages[0].Derived[0].Provenance != provenance {
		t.Fatalf("exact valid stored limits rejected or unnecessarily truncated: %#v %v", page, err)
	}
	if size := len(guardedJSON(t, page)); size > 128<<10 {
		t.Fatalf("exact stored-limit response exceeds serialized cap: %d", size)
	}
	if _, err = s.CheckResponse(context.Background(), readerScope(p, readerChat(t, s, id)), ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("exact limit response guard rejected: %v", err)
	}
}

func TestDerivedContextBoundsExactEscapedJSONAcrossMultipleMessages(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var centerID MessageID
	var center SourceEvent
	reply := readerEvent("reply-target", "allowed", strings.Repeat("\u0002", 1024), now.Add(-10*time.Hour))
	replyID := readerApply(t, s, p, reply)
	ids := make([]MessageID, 0, 5)
	events := make([]SourceEvent, 0, 5)
	for n := 0; n < 5; n++ {
		event := readerEvent(fmt.Sprintf("row-%d", n), "allowed", fmt.Sprintf("Original context row %d שָׁלוֹם", n), now.Add(time.Duration(n-5)*time.Minute))
		event.Caption = fmt.Sprintf("Original caption %d", n)
		if n == 2 {
			event.ReplyTo = &reply.Key
		}
		id := readerApply(t, s, p, event)
		ids = append(ids, id)
		events = append(events, event)
		if n == 2 {
			centerID, center = id, event
		}
	}
	expectedSources := []SourceReadVersion{guardedExpectedSource(t, s, p, reply, replyID)}
	for n, id := range ids {
		expectedSources = append(expectedSources, guardedExpectedSource(t, s, p, events[n], id))
	}
	generation := candidateIndex(t, s)
	centerText := strings.Repeat("\u0001", 1000)
	neighborText := strings.Repeat("\u0001", 2500)
	provenance := derivedContextProvenance(DocumentOCR)
	wantProvenance := make(map[MessageID]DerivedProvenance, len(ids))
	for row, id := range ids {
		text := neighborText
		if id == centerID {
			text = centerText
		}
		for document := 0; document < 9; document++ {
			wantProvenance[id] = derivedContextDocument(t, s, p, generation, DocumentID(fmt.Sprintf("row-%d-ocr-%d", row, document)), text, DocumentOCR, []MessageID{id}, provenance)
		}
	}
	scope := readerScope(p, readerChat(t, s, centerID))
	page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: centerID, Before: 2, After: 2})
	if err != nil || len(page.Messages) != 5 {
		t.Fatalf("bounded multi-row derived context: %#v %v", page, err)
	}
	view := derivedContextMessage(t, page, centerID)
	if view.Body != center.Body || view.Caption != center.Caption || view.ReplyTo != replyID || view.ReplyQuote != reply.Body || len(view.Derived) != 8 {
		t.Fatalf("response budget discarded requested original or its generated provenance: %#v", view)
	}
	for _, excerpt := range view.Derived {
		if excerpt.Text != centerText {
			t.Fatalf("neighbor text took priority over known-fit center generated text: %#v", excerpt)
		}
	}
	nearest := derivedContextMessage(t, page, ids[1])
	otherNearest := derivedContextMessage(t, page, ids[3])
	if len(nearest.Derived) != 8 || len(otherNearest.Derived) != 8 || (nearest.Derived[0].Text != neighborText && otherNearest.Derived[0].Text != neighborText) {
		t.Fatal("farther context took priority over nearest generated provenance/text")
	}
	count, truncated := 0, false
	for _, message := range page.Messages {
		if len(message.Derived) > 8 {
			t.Fatalf("per-message derived admission exceeded 8: %d", len(message.Derived))
		}
		for _, excerpt := range message.Derived {
			count++
			text := neighborText
			if message.ID == centerID {
				text = centerText
			}
			if excerpt.Provenance != wantProvenance[message.ID] || excerpt.Source.MessageID != message.ID || !strings.HasPrefix(text, excerpt.Text) {
				t.Fatalf("budget rewrote provenance, source, or generated text order: %#v", excerpt)
			}
			truncated = truncated || len(excerpt.Text) < len(text)
		}
	}
	if count != 32 || !truncated {
		t.Fatalf("page derived admission or text budget failed: count=%d truncated=%v", count, truncated)
	}
	encoded := guardedJSON(t, page)
	if len(encoded) > 128<<10 {
		t.Fatalf("escaping breached exact serialized 128KiB cap: %d bytes", len(encoded))
	}
	guardedAssertSources(t, guard, expectedSources...)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("bounded generated context guard rejected: %v", err)
	}
}
