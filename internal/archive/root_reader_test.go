//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rootReaderAssertPins(t *testing.T, s *Store, guard ResponseGuard, ids ...DocumentID) {
	t.Helper()
	if len(guard.roots) != len(ids) {
		t.Fatalf("root response membership: got %#v, want %v", guard.roots, ids)
	}
	want := make(map[DocumentID]extensionRootResponsePin, len(ids))
	for _, id := range ids {
		record, err := s.extensionAuthorizeRootAt(context.Background(), s.readDB, id, s.now())
		if err != nil {
			t.Fatal(err)
		}
		pin, err := extensionRootIdentity(record)
		if err != nil {
			t.Fatal(err)
		}
		want[id] = pin
	}
	for _, pin := range guard.roots {
		if expected, exists := want[pin.ID]; !exists || pin != expected || pin.IdentitySHA256 == [32]byte{} {
			t.Fatalf("root response pin lost authoritative identity: %#v", pin)
		}
		delete(want, pin.ID)
	}
	if len(want) != 0 {
		t.Fatalf("root response omitted or duplicated pins: %#v", want)
	}
}

// Test corruption bypasses only schema CHECKs on its own borrowed connection.
// Production publication cannot create these oversized or malformed records.
func rootReaderCorrupt(t *testing.T, s *Store, statement string, args ...any) {
	t.Helper()
	connection, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err = connection.ExecContext(context.Background(), `PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := connection.ExecContext(context.Background(), `PRAGMA ignore_check_constraints=OFF`); err != nil {
			t.Error(err)
		}
	}()
	if _, err = connection.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

func rootReaderRewriteText(t *testing.T, s *Store, id DocumentID, text string) {
	t.Helper()
	digest := sha256.Sum256([]byte(text))
	rootReaderCorrupt(t, s, `UPDATE extension_roots SET text=?,text_sha256=?,text_bytes=? WHERE id=?`, text, hex.EncodeToString(digest[:]), len(text), id)
}

// Copied encoder inputs have no independent extraction root. Their forged
// FTS/vector rows must not turn them into public generated context.
func rootReaderHiddenDocument(t *testing.T, s *Store, p ArchivePolicy, generation IndexGeneration, id, text string, members []MessageID, provenance DerivedProvenance, origin DocumentID) {
	t.Helper()
	encoded, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_documents(id,generation,kind,text,normalized_text,provenance_json,spans_json,passage_tokens,content_hash,source_count,origin_document_id,origin_start_byte,origin_end_byte,embedding_input)
 VALUES(?,?,?,?,?,?,'[]',4,'fixture-hidden-document-hash',?,?,0,?,1)`, id, generation.ID, provenance.Kind, text, NormalizeLexical(text), string(encoded), len(members), origin, len(text)); err != nil {
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
	if _, err = s.writeDB.Exec(`INSERT INTO extension_vectors(document_id,generation,dimensions,values_blob) VALUES(?,?,2,X'0000803f00000000')`, id, generation.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRootReaderWithoutIndexPreservesOriginalsAndTruthfulProvenance(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	anchor := readerEvent("root-anchor", "allowed", "Original rootneedle שָׁלוֹם", now.Add(-time.Minute))
	anchor.BodyKind, anchor.Caption = MessageImage, "Original image caption"
	extra := readerEvent("root-extra", "extra", "Original extra contributor", now.Add(-2*time.Minute))
	extra.Key.Sender = "extra-private-sender@s.whatsapp.net"
	voice := readerEvent("root-voice", "voice", "Original voice body", now.Add(-time.Minute))
	voice.BodyKind, voice.Caption = MessageVoice, "Original voice caption"
	voice.Key.Sender = "voice-private-sender@s.whatsapp.net"
	anchorID, extraID, voiceID := readerApply(t, s, p, anchor), readerApply(t, s, p, extra), readerApply(t, s, p, voice)
	scope := readerScope(p, readerChat(t, s, anchorID), readerChat(t, s, extraID), readerChat(t, s, voiceID))
	baseline, err := s.Context(context.Background(), scope, ContextRequest{MessageID: anchorID})
	if err != nil {
		t.Fatal(err)
	}
	wantSources := []SourceReadVersion{guardedExpectedSource(t, s, p, anchor, anchorID), guardedExpectedSource(t, s, p, extra, extraID), guardedExpectedSource(t, s, p, voice, voiceID)}
	ocr := candidateRoot(t, s, p, "root-ocr", "rootneedle Generated OCR טקסט", []MessageID{anchorID, extraID}, derivedContextProvenance(DocumentOCR))
	caption := candidateRoot(t, s, p, "root-caption", "rootneedle Generated media caption תיאור", []MessageID{anchorID}, derivedContextProvenance(DocumentCaption))
	transcript := candidateRoot(t, s, p, "root-transcript", "rootneedle Generated transcript תמלול", []MessageID{voiceID}, derivedContextProvenance(DocumentTranscript))
	baseline.Messages[0].MediaAvailability = "retained"
	search, guard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "rootneedle", Limit: 10})
	if err != nil || len(search.Hits) != 4 {
		t.Fatalf("encoder-independent original/root FTS: %#v %v", search, err)
	}
	wantTexts := map[DocumentID]string{"root-ocr": "rootneedle Generated OCR טקסט", "root-caption": "rootneedle Generated media caption תיאור", "root-transcript": "rootneedle Generated transcript תמלול"}
	wantProvenance := map[DocumentID]DerivedProvenance{"root-ocr": ocr, "root-caption": caption, "root-transcript": transcript}
	originals := 0
	for _, hit := range search.Hits {
		if hit.DocumentID == "" {
			originals++
			if hit.MessageID != anchorID || hit.Snippet != anchor.Body+"\n"+anchor.Caption || len(hit.Derived) != 0 {
				t.Fatalf("root search changed original logical-order fields: %#v", hit)
			}
			continue
		}
		text, exists := wantTexts[hit.DocumentID]
		if !exists || hit.Snippet != text || len(hit.Derived) != 1 || hit.Derived[0].DocumentID != hit.DocumentID || hit.Derived[0].Text != text || hit.Derived[0].Provenance != wantProvenance[hit.DocumentID] {
			t.Fatalf("root search fabricated provenance or text: %#v", hit)
		}
		want := []SourceVersion{wantSources[0].Ref.Version}
		if hit.DocumentID == "root-ocr" {
			want = append(want, wantSources[1].Ref.Version)
		} else if hit.DocumentID == "root-transcript" {
			want = []SourceVersion{wantSources[2].Ref.Version}
		}
		if !reflect.DeepEqual(hit.Sources, want) || hit.MessageID != want[0].MessageID || hit.Derived[0].Source != want[0] {
			t.Fatalf("root search omitted non-anchor contributor: %#v", hit)
		}
		delete(wantTexts, hit.DocumentID)
	}
	if originals != 1 || len(wantTexts) != 0 {
		t.Fatalf("original/root search duplicated or omitted a result: originals=%d remaining=%v", originals, wantTexts)
	}
	guardedAssertSources(t, guard, wantSources...)
	rootReaderAssertPins(t, s, guard, "root-ocr", "root-caption", "root-transcript")
	if guard.Generation != "" || guard.IndexVersionID != "" {
		t.Fatalf("independent extraction fabricated an encoder generation: %#v", guard)
	}
	legacySearch, err := s.Search(context.Background(), scope, SearchRequest{Query: "rootneedle", Limit: 10})
	if err != nil || !reflect.DeepEqual(legacySearch, search) {
		t.Fatalf("legacy Search omitted independent roots: %#v %v", legacySearch, err)
	}
	page, contextGuard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: anchorID})
	if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Derived) != 2 {
		t.Fatalf("no-index root context: %#v %v", page, err)
	}
	contextTexts := map[DocumentID]string{"root-ocr": "rootneedle Generated OCR טקסט", "root-caption": "rootneedle Generated media caption תיאור"}
	for _, excerpt := range page.Messages[0].Derived {
		if text, exists := contextTexts[excerpt.DocumentID]; !exists || excerpt.Text != text || excerpt.Provenance != wantProvenance[excerpt.DocumentID] || excerpt.Source != wantSources[0].Ref.Version {
			t.Fatalf("no-index context fabricated or duplicated a generated root: %#v", excerpt)
		}
		delete(contextTexts, excerpt.DocumentID)
	}
	if len(contextTexts) != 0 {
		t.Fatalf("no-index context omitted generated root: %v", contextTexts)
	}
	view := page.Messages[0]
	view.Derived = nil
	if !reflect.DeepEqual(view, baseline.Messages[0]) {
		t.Fatalf("independent roots replaced original fields: got %#v want %#v", view, baseline.Messages[0])
	}
	guardedAssertSources(t, contextGuard, wantSources[0], wantSources[1])
	rootReaderAssertPins(t, s, contextGuard, "root-ocr", "root-caption")
	legacyContext, err := s.Context(context.Background(), scope, ContextRequest{MessageID: anchorID})
	if err != nil || !reflect.DeepEqual(legacyContext, page) {
		t.Fatalf("legacy Context omitted independent roots: %#v %v", legacyContext, err)
	}
	voicePage, voiceGuard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: voiceID})
	if err != nil || len(voicePage.Messages) != 1 || voicePage.Messages[0].Body != voice.Body || voicePage.Messages[0].Caption != voice.Caption || len(voicePage.Messages[0].Derived) != 1 || voicePage.Messages[0].Derived[0].DocumentID != "root-transcript" || voicePage.Messages[0].Derived[0].Provenance != transcript || voicePage.Messages[0].Derived[0].Text != "rootneedle Generated transcript תמלול" || voicePage.Messages[0].Derived[0].Source != wantSources[2].Ref.Version {
		t.Fatalf("no-index transcript context changed original or generated fields: %#v %v", voicePage, err)
	}
	guardedAssertSources(t, voiceGuard, wantSources[2])
	rootReaderAssertPins(t, s, voiceGuard, "root-transcript")
	for _, responseGuard := range []ResponseGuard{guard, contextGuard, voiceGuard} {
		if responseGuard.Generation != "" || responseGuard.IndexVersionID != "" {
			t.Fatalf("no-index root context invented an encoder identity: %#v", responseGuard)
		}
		if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: responseGuard}); err != nil {
			t.Fatalf("current no-index root guard rejected: %v", err)
		}
	}
	if active, err := s.ActiveIndex(context.Background()); err != nil || active != (IndexGeneration{}) {
		t.Fatalf("root readers invented active index: %#v %v", active, err)
	}
	status, statusGuard, err := s.StatusGuarded(context.Background(), scope)
	if err != nil || status.Messages != 3 || len(status.Chats) != 3 || status.Semantic.State != "not_configured" || len(statusGuard.Sources) != 0 || len(statusGuard.roots) != 0 || statusGuard.Generation != "" || statusGuard.IndexVersionID != "" {
		t.Fatalf("generated roots changed original aggregate status: %#v %#v %v", status, statusGuard, err)
	}
	chats, chatGuard, err := s.ListChatsGuarded(context.Background(), scope, ChatListRequest{Limit: 10})
	if err != nil || len(chats.Chats) != 3 || len(chatGuard.Sources) != 0 || len(chatGuard.roots) != 0 {
		t.Fatalf("generated roots changed chat aggregate guard: %#v %#v %v", chats, chatGuard, err)
	}
	for _, chat := range chats.Chats {
		if chat.MessageCount != 1 {
			t.Fatalf("generated root counted as original message: %#v", chat)
		}
	}
	for _, responseGuard := range []ResponseGuard{statusGuard, chatGuard} {
		if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: responseGuard}); err != nil {
			t.Fatalf("current metadata-only no-index guard rejected: %v", err)
		}
	}
}

func TestRootReaderCompleteAuthorizationPrecedesLimitAndPayloadDecode(t *testing.T) {
	for _, scenario := range []string{"chat", "date", "rolling", "expiry", "deleted", "member_view_once", "media_revoked", "media_view_once"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			anchor := readerEvent("anchor", "allowed", "Original permitted anchor", now.Add(-time.Minute))
			extra := readerEvent("extra", "other", "Other contributor", now.Add(-2*time.Hour))
			extra.Key.Sender = "extra-private-sender@s.whatsapp.net"
			if scenario == "expiry" {
				extra.ExpiresAt = now.Add(time.Minute)
			}
			anchorID, extraID := readerApply(t, s, p, anchor), readerApply(t, s, p, extra)
			expected := guardedExpectedSource(t, s, p, anchor, anchorID)
			control := candidateRoot(t, s, p, "99-root-control", "rootgate Permitted generated control", []MessageID{anchorID}, derivedContextProvenance(DocumentOCR))
			extraMedia := candidateRoot(t, s, p, "extra-media-fixture", "Unsearched extra media fixture", []MessageID{extraID}, derivedContextProvenance(DocumentOCR))
			for n := 0; n < 9; n++ {
				id := DocumentID(fmt.Sprintf("00-denied-%02d", n))
				provenance := candidateRoot(t, s, p, string(id), "rootgate DENIED_ROOT_CANARY", []MessageID{anchorID, extraID}, derivedContextProvenance(DocumentOCR))
				if scenario == "media_revoked" || scenario == "media_view_once" {
					provenance.MediaID = extraMedia.MediaID
					encoded, err := json.Marshal(provenance)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = s.writeDB.Exec(`UPDATE extension_roots SET media_id=?,provenance_json=? WHERE id=?`, provenance.MediaID, string(encoded), id); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.writeDB.Exec(`UPDATE extension_roots SET provenance_json=? WHERE id=?`, `{"Kind":`, id); err != nil {
					t.Fatal(err)
				}
			}
			scope := readerScope(p, readerChat(t, s, anchorID), readerChat(t, s, extraID))
			switch scenario {
			case "chat":
				scope.Chats = scope.Chats[:1]
			case "date":
				scope.From = now.Add(-time.Hour)
			case "rolling":
				scope.Rolling = time.Hour
			case "expiry":
				now = now.Add(2 * time.Minute)
			case "deleted":
				if _, err := s.writeDB.Exec(`UPDATE messages SET state='deleted' WHERE id=?`, extraID); err != nil {
					t.Fatal(err)
				}
			case "member_view_once":
				if _, err := s.writeDB.Exec(`UPDATE messages SET view_once=1 WHERE id=?`, extraID); err != nil {
					t.Fatal(err)
				}
			case "media_revoked", "media_view_once":
				column := "active"
				value := 0
				if scenario == "media_view_once" {
					column, value = "view_once", 1
				}
				if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET `+column+`=? WHERE id=?`, value, extraMedia.MediaID); err != nil {
					t.Fatal(err)
				}
			}
			search, searchGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "rootgate", Limit: 1})
			if err != nil || len(search.Hits) != 1 || search.Hits[0].DocumentID != "99-root-control" || len(search.Hits[0].Derived) != 1 || search.Hits[0].Derived[0].Provenance != control {
				t.Fatalf("denied root decoded or starved permitted FTS control: %#v %v", search, err)
			}
			page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: anchorID})
			if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Derived) != 1 || page.Messages[0].Derived[0].DocumentID != "99-root-control" || page.Messages[0].Body != anchor.Body || strings.Contains(string(guardedJSON(t, page)), "DENIED_ROOT_CANARY") {
				t.Fatalf("denied root partly surfaced or affected allowed context: %#v %v", page, err)
			}
			for _, responseGuard := range []ResponseGuard{searchGuard, guard} {
				guardedAssertSources(t, responseGuard, expected)
				rootReaderAssertPins(t, s, responseGuard, "99-root-control")
				if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: responseGuard}); err != nil {
					t.Fatalf("allowed root-only response guard rejected: %v", err)
				}
			}
		})
	}
}

func TestRootReaderFinalGuardBindsImmutableRootAndCurrentMedia(t *testing.T) {
	for _, reader := range []string{"search", "context"} {
		for _, mutation := range []string{"deleted_root", "text_sha", "provenance", "media_sha", "unrelated_root"} {
			t.Run(reader+"/"+mutation, func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				anchor := readerEvent("anchor", "allowed", "Original context body", now.Add(-time.Minute))
				extra := readerEvent("extra", "extra", "Original non-anchor body", now.Add(-2*time.Minute))
				extra.Key.Sender = "extra-private-sender@s.whatsapp.net"
				anchorID, extraID := readerApply(t, s, p, anchor), readerApply(t, s, p, extra)
				want := []SourceReadVersion{guardedExpectedSource(t, s, p, anchor, anchorID), guardedExpectedSource(t, s, p, extra, extraID)}
				provenance := candidateRoot(t, s, p, "guarded-root", "rootguard Generated text", []MessageID{anchorID, extraID}, derivedContextProvenance(DocumentOCR))
				scope := readerScope(p, readerChat(t, s, anchorID), readerChat(t, s, extraID))
				var guard ResponseGuard
				var err error
				if reader == "search" {
					var page SearchPage
					page, guard, err = s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "rootguard"})
					if err != nil || len(page.Hits) != 1 || page.Hits[0].DocumentID != "guarded-root" {
						t.Fatalf("guarded root search: %#v %v", page, err)
					}
				} else {
					var page ContextPage
					page, guard, err = s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: anchorID})
					if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Derived) != 1 {
						t.Fatalf("guarded root context: %#v %v", page, err)
					}
				}
				guardedAssertSources(t, guard, want...)
				rootReaderAssertPins(t, s, guard, "guarded-root")
				if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
					t.Fatalf("initial exact root guard rejected: %v", err)
				}
				switch mutation {
				case "deleted_root":
					_, err = s.writeDB.Exec(`DELETE FROM extension_roots WHERE id='guarded-root'`)
				case "text_sha":
					_, err = s.writeDB.Exec(`UPDATE extension_roots SET text_sha256=? WHERE id='guarded-root'`, strings.Repeat("e", 64))
				case "provenance":
					provenance.ProfileID = "changed-root-profile"
					var encoded []byte
					encoded, err = json.Marshal(provenance)
					if err == nil {
						_, err = s.writeDB.Exec(`UPDATE extension_roots SET provenance_json=? WHERE id='guarded-root'`, string(encoded))
					}
				case "media_sha":
					_, err = s.writeDB.Exec(`UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?`, strings.Repeat("f", 64), provenance.MediaID)
				case "unrelated_root":
					other := readerEvent("other", "unrelated", "Unrelated original", now)
					other.Key.Sender = "unrelated-private-sender@s.whatsapp.net"
					otherID := readerApply(t, s, p, other)
					candidateRoot(t, s, p, "unrelated-root", "Unrelated generated text", []MessageID{otherID}, derivedContextProvenance(DocumentOCR))
				}
				if err != nil {
					t.Fatal(err)
				}
				for n, event := range []SourceEvent{anchor, extra} {
					metadata, err := s.SourceMetadata(context.Background(), event.Key)
					if err != nil || metadata.Version != want[n].Ref.Version {
						t.Fatalf("root-only mutation changed original source version: %#v %v", metadata, err)
					}
				}
				_, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard})
				if mutation == "unrelated_root" {
					if err != nil {
						t.Fatalf("unrelated valid root invalidated contributing guard: %v", err)
					}
				} else if !errors.Is(err, ErrSourceUnavailable) {
					t.Fatalf("post-return %s ignored immutable root/media authority: %v", mutation, err)
				}
			})
		}
	}
}

type rootReaderBatchFixture struct {
	s          *Store
	policy     ArchivePolicy
	scope      ReadScope
	events     []SourceEvent
	ids        []MessageID
	sources    []SourceReadVersion
	roots      []DocumentID
	provenance map[DocumentID]DerivedProvenance
	unused     DerivedProvenance
	guards     []ResponseGuard
}

// Selected IDs are sparse among legitimate unselected roots. Their ordered
// memberships overlap, so a batch must retain each complete list while the
// response guard contains only the five unique contributing originals.
func rootReaderBatchGuards(t *testing.T) rootReaderBatchFixture {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	fixture := rootReaderBatchFixture{s: s, policy: p, provenance: make(map[DocumentID]DerivedProvenance)}
	for n, chat := range []string{"batch-visible", "batch-visible", "batch-visible", "batch-shared", "batch-leaf", "batch-unrelated"} {
		event := readerEvent(fmt.Sprintf("batch-source-%d", n), chat, fmt.Sprintf("Original source %d", n), now.Add(time.Duration(n-10)*time.Minute))
		event.Key.Sender = fmt.Sprintf("batch-private-sender-%d@s.whatsapp.net", n)
		if n < 2 {
			event.BodyKind, event.Caption = MessageImage, "Original image caption"
		} else if n == 2 {
			event.BodyKind, event.Caption = MessageVoice, "Original voice caption"
		}
		id := readerApply(t, s, p, event)
		if err := s.SetSourceNames(context.Background(), SourceNames{Account: event.Key.Account, Chat: event.Key.Chat, Sender: event.Key.Sender, ChatName: chat, SenderName: fmt.Sprintf("Original sender %d", n)}); err != nil {
			t.Fatal(err)
		}
		fixture.events = append(fixture.events, event)
		fixture.ids = append(fixture.ids, id)
		fixture.sources = append(fixture.sources, guardedExpectedSource(t, s, p, event, id))
	}
	fixture.scope = readerScope(p, readerChat(t, s, fixture.ids[0]), readerChat(t, s, fixture.ids[3]), readerChat(t, s, fixture.ids[4]), readerChat(t, s, fixture.ids[5]))
	fixture.roots = []DocumentID{"batch-root-001", "batch-root-777", "batch-root-z99"}
	members := [][]int{{0, 3, 4}, {1, 4, 3}, {2, 3}}
	kinds := []DocumentKind{DocumentOCR, DocumentCaption, DocumentTranscript}
	wantMembers := make(map[DocumentID][]SourceVersion)
	wantText := make(map[DocumentID]string)
	for n, root := range fixture.roots {
		var ids []MessageID
		for _, member := range members[n] {
			ids = append(ids, fixture.ids[member])
			wantMembers[root] = append(wantMembers[root], fixture.sources[member].Ref.Version)
		}
		text := fmt.Sprintf("batchneedle Generated %s text", kinds[n])
		wantText[root] = text
		fixture.provenance[root] = candidateRoot(t, s, p, string(root), text, ids, derivedContextProvenance(kinds[n]))
	}
	for _, root := range []string{"batch-root-100", "batch-root-555", "batch-root-z98"} {
		fixture.unused = candidateRoot(t, s, p, root, "Unsearched unrelated generated text", []MessageID{fixture.ids[5]}, derivedContextProvenance(DocumentOCR))
	}
	search, searchGuard, err := s.SearchGuarded(context.Background(), fixture.scope, SearchRequest{Query: "batchneedle", Limit: 10})
	if err != nil || len(search.Hits) != len(fixture.roots) {
		t.Fatalf("sparse root batch Search: %#v %v", search, err)
	}
	seen := make(map[DocumentID]bool)
	for _, hit := range search.Hits {
		ordered, exists := wantMembers[hit.DocumentID]
		if !exists || seen[hit.DocumentID] || !reflect.DeepEqual(hit.Sources, ordered) || hit.MessageID != ordered[0].MessageID || hit.Snippet != wantText[hit.DocumentID] || len(hit.Derived) != 1 || hit.Derived[0].DocumentID != hit.DocumentID || hit.Derived[0].Source != ordered[0] || hit.Derived[0].Text != wantText[hit.DocumentID] || hit.Derived[0].Provenance != fixture.provenance[hit.DocumentID] {
			t.Fatalf("sparse root batch lost ordered membership or truthful provenance: %#v", hit)
		}
		seen[hit.DocumentID] = true
	}
	page, contextGuard, err := s.ContextGuarded(context.Background(), fixture.scope, ContextRequest{MessageID: fixture.ids[1], Before: 1, After: 1})
	if err != nil || len(page.Messages) != 3 {
		t.Fatalf("sparse root batch Context: %#v %v", page, err)
	}
	for n, message := range page.Messages {
		root := fixture.roots[n]
		if message.ID != fixture.ids[n] || message.Body != fixture.events[n].Body || message.Caption != fixture.events[n].Caption || len(message.Derived) != 1 || message.Derived[0].DocumentID != root || message.Derived[0].Text != wantText[root] || message.Derived[0].Provenance != fixture.provenance[root] || message.Derived[0].Source != wantMembers[root][0] {
			t.Fatalf("sparse root batch changed original fields or excerpt anchor: %#v", message)
		}
	}
	fixture.guards = []ResponseGuard{searchGuard, contextGuard}
	for _, guard := range fixture.guards {
		guardedAssertSources(t, guard, fixture.sources[:5]...)
		rootReaderAssertPins(t, s, guard, fixture.roots...)
		if guard.Generation != "" || guard.IndexVersionID != "" {
			t.Fatalf("legacy root batch invented an encoder generation: %#v", guard)
		}
		if snapshot, err := s.CheckResponse(context.Background(), fixture.scope, ResponseCheck{Guard: guard}); err != nil || snapshot.PolicyGeneration != p.Generation {
			t.Fatalf("complete sparse root batch guard rejected: %#v %v", snapshot, err)
		}
	}
	return fixture
}

func TestRootReaderBatchGuardPreservesSparseMembershipThroughUnrelatedCorruption(t *testing.T) {
	fixture := rootReaderBatchGuards(t)
	s := fixture.s
	// None of these roots, labels, or media associations contributed to either
	// page. A sparse batch must not decode them or borrow their media authority.
	for _, mutation := range []struct {
		statement string
		args      []any
	}{
		{`UPDATE extension_roots SET provenance_json='{"Kind":' WHERE id='batch-root-100'`, nil},
		{`UPDATE extension_roots SET text_sha256=? WHERE id='batch-root-555'`, []any{strings.Repeat("z", 64)}},
		{`DELETE FROM extension_roots WHERE id='batch-root-z98'`, nil},
		{`UPDATE extension_media_associations SET active=0,last_original_sha256=? WHERE id=?`, []any{strings.Repeat("f", 64), fixture.unused.MediaID}},
	} {
		if _, err := s.writeDB.Exec(mutation.statement, mutation.args...); err != nil {
			t.Fatal(err)
		}
	}
	other := fixture.events[5]
	if err := s.SetSourceNames(context.Background(), SourceNames{Account: other.Key.Account, Chat: other.Key.Chat, Sender: other.Key.Sender, ChatName: "Renamed unrelated chat", SenderName: "Renamed unrelated sender"}); err != nil {
		t.Fatal(err)
	}
	for _, guard := range fixture.guards {
		guardedAssertSources(t, guard, fixture.sources[:5]...)
		if snapshot, err := s.CheckResponse(context.Background(), fixture.scope, ResponseCheck{Guard: guard}); err != nil || snapshot.PolicyGeneration != fixture.policy.Generation {
			t.Fatalf("unrequested malformed root or unrelated authority invalidated sparse guard: %#v %v", snapshot, err)
		}
	}
}

func TestRootReaderBatchGuardRejectsIncompleteOrMalformedAuthority(t *testing.T) {
	for _, mutation := range []string{"missing_member", "missing_guard_contributor", "denied_member", "unknown_date_member", "ordinal_gap", "ordinal_blob", "ordinal_blob_oversized", "member_reordering", "source_count", "source_count_type", "metadata_sha", "metadata_sha_type", "malformed_provenance", "media_sha", "media_revoked", "displayed_chat_rename", "displayed_sender_rename", "non_anchor_edit"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := rootReaderBatchGuards(t)
			s, root := fixture.s, fixture.roots[1]
			var err error
			switch mutation {
			case "missing_member":
				_, err = s.writeDB.Exec(`DELETE FROM extension_root_sources WHERE root_id=? AND ordinal=2`, root)
			case "missing_guard_contributor":
				for n, guard := range fixture.guards {
					var sources []SourceReadVersion
					for _, source := range guard.Sources {
						if source.Ref.Version.MessageID != fixture.ids[4] {
							sources = append(sources, source)
						}
					}
					fixture.guards[n].Sources = sources
				}
			case "denied_member":
				_, err = s.writeDB.Exec(`UPDATE messages SET state='deleted' WHERE id=?`, fixture.ids[4])
			case "unknown_date_member":
				_, err = s.writeDB.Exec(`UPDATE messages SET original_date_known=0 WHERE id=?`, fixture.ids[4])
			case "ordinal_gap":
				_, err = s.writeDB.Exec(`UPDATE extension_root_sources SET ordinal=7 WHERE root_id=? AND ordinal=2`, root)
			case "ordinal_blob", "ordinal_blob_oversized":
				// SQLite's ordinal>=0 CHECK accepts a BLOB and sorts it after
				// integers 0/1. Driver coercion of BLOB "2" must not turn that
				// malformed last member into valid ordered root authority.
				ordinal := []byte("2")
				if mutation == "ordinal_blob_oversized" {
					ordinal = []byte(strings.Repeat("2", 16*1024))
				}
				_, err = s.writeDB.Exec(`UPDATE extension_root_sources SET ordinal=? WHERE root_id=? AND ordinal=2`, ordinal, root)
			case "member_reordering":
				// Keep the same complete unordered set and unchanged anchor, but
				// exchange its two non-anchor ordinals without violating UNIQUE.
				for _, ordinals := range [][2]int{{1, 7}, {2, 1}, {7, 2}} {
					if _, err = s.writeDB.Exec(`UPDATE extension_root_sources SET ordinal=? WHERE root_id=? AND ordinal=?`, ordinals[1], root, ordinals[0]); err != nil {
						break
					}
				}
			case "source_count":
				_, err = s.writeDB.Exec(`UPDATE extension_roots SET source_count=2 WHERE id=?`, root)
			case "source_count_type":
				rootReaderCorrupt(t, s, `UPDATE extension_roots SET source_count=X'33' WHERE id=?`, root)
			case "metadata_sha":
				_, err = s.writeDB.Exec(`UPDATE extension_roots SET text_sha256=? WHERE id=?`, strings.Repeat("z", 64), root)
			case "metadata_sha_type":
				rootReaderCorrupt(t, s, `UPDATE extension_roots SET text_sha256=CAST(text_sha256 AS BLOB) WHERE id=?`, root)
			case "malformed_provenance":
				_, err = s.writeDB.Exec(`UPDATE extension_roots SET provenance_json='{"Kind":' WHERE id=?`, root)
			case "media_sha":
				_, err = s.writeDB.Exec(`UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?`, strings.Repeat("f", 64), fixture.provenance[root].MediaID)
			case "media_revoked":
				_, err = s.writeDB.Exec(`UPDATE extension_media_associations SET active=0 WHERE id=?`, fixture.provenance[root].MediaID)
			case "displayed_chat_rename", "displayed_sender_rename":
				event := fixture.events[1]
				names := SourceNames{Account: event.Key.Account, Chat: event.Key.Chat, Sender: event.Key.Sender, ChatName: event.Key.Chat, SenderName: "Original sender 1"}
				if mutation == "displayed_chat_rename" {
					names.ChatName = "Renamed contributing chat"
				} else {
					names.SenderName = "Renamed contributing sender"
				}
				err = s.SetSourceNames(context.Background(), names)
			case "non_anchor_edit":
				event := fixture.events[4]
				event.Kind, event.SourceRevision, event.Body = EventEditContent, 2, "Edited contributing original"
				readerApply(t, s, fixture.policy, event)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mutation != "non_anchor_edit" && mutation != "denied_member" {
				for n, event := range fixture.events[:5] {
					metadata, err := s.SourceMetadata(context.Background(), event.Key)
					if err != nil || metadata.Version != fixture.sources[n].Ref.Version {
						t.Fatalf("root/member metadata mutation accidentally changed source revision: %#v %v", metadata, err)
					}
				}
			}
			for _, guard := range fixture.guards {
				snapshot, err := s.CheckResponse(context.Background(), fixture.scope, ResponseCheck{Guard: guard})
				if !errors.Is(err, ErrSourceUnavailable) || snapshot != (ReadSnapshot{}) {
					t.Fatalf("post-return %s left incomplete batched authority usable: %#v %v", mutation, snapshot, err)
				}
			}
		})
	}
}

func TestRootReaderSemanticCopyCannotReplaceAuthoritativeText(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("anchor", "allowed", "Original unchanged body", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	expected := guardedExpectedSource(t, s, p, event, id)
	generation := candidateIndex(t, s)
	text := "authoritativeneedle original extraction root"
	candidateDocument(t, s, p, generation, "authoritative-root", text, []MessageID{id}, nil)
	var raw string
	if err := s.readDB.QueryRow(`SELECT provenance_json FROM extension_roots WHERE id='authoritative-root'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var provenance DerivedProvenance
	if err := json.Unmarshal([]byte(raw), &provenance); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`UPDATE extension_documents SET text='SEMANTIC_TEXT_FORGERY_CANARY',provenance_json='{"Kind":' WHERE id='authoritative-root'`); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, id))
	search, searchGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "authoritativeneedle"})
	if err != nil || len(search.Hits) != 1 || search.Hits[0].DocumentID != "authoritative-root" || search.Hits[0].Snippet != text || len(search.Hits[0].Derived) != 1 || search.Hits[0].Derived[0].Text != text || search.Hits[0].Derived[0].Provenance != provenance {
		t.Fatalf("mutable semantic copy replaced immutable root in Search: %#v %v", search, err)
	}
	page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Body != event.Body || len(page.Messages[0].Derived) != 1 || page.Messages[0].Derived[0].Text != text || page.Messages[0].Derived[0].Provenance != provenance || strings.Contains(string(guardedJSON(t, page)), "SEMANTIC_TEXT_FORGERY_CANARY") {
		t.Fatalf("mutable semantic copy replaced immutable root in Context: %#v %v", page, err)
	}
	for _, responseGuard := range []ResponseGuard{searchGuard, guard} {
		guardedAssertSources(t, responseGuard, expected)
		rootReaderAssertPins(t, s, responseGuard, "authoritative-root")
		if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: responseGuard}); err != nil {
			t.Fatalf("truthful root guard depended on corrupt encoder copy: %v", err)
		}
	}
}

func TestRootReaderFinalFenceRejectsRootMutationWithoutSourceOrEpochChange(t *testing.T) {
	for _, method := range []string{"search", "search_guarded", "context", "context_guarded"} {
		t.Run(method, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("anchor", "allowed", "Original valid body", now.Add(-time.Minute))
			id := readerApply(t, s, p, event)
			candidateRoot(t, s, p, "final-fence-root", "fenceneedle generated text", []MessageID{id}, derivedContextProvenance(DocumentOCR))
			sourceBefore, err := s.SourceMetadata(context.Background(), event.Key)
			if err != nil {
				t.Fatal(err)
			}
			epochBefore, err := s.readSafety(event.Key.Account)
			if err != nil {
				t.Fatal(err)
			}
			scope := readerScope(p, readerChat(t, s, id))
			calls := 0
			var mutationError error
			originalClock := s.options.Now
			// One admitted root: beginRead samples clock 1, loading its full
			// plaintext samples clock 2, and finish samples fresh clock 3.
			s.options.Now = func() time.Time {
				calls++
				if calls == 3 {
					_, mutationError = s.writeDB.Exec(`DELETE FROM extension_roots WHERE id='final-fence-root'`)
				}
				return now
			}
			var page any
			var guard ResponseGuard
			var empty any
			switch method {
			case "search":
				page, err = s.Search(context.Background(), scope, SearchRequest{Query: "fenceneedle"})
				empty = SearchPage{}
			case "search_guarded":
				page, guard, err = s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "fenceneedle"})
				empty = SearchPage{}
			case "context":
				page, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id})
				empty = ContextPage{}
			case "context_guarded":
				page, guard, err = s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
				empty = ContextPage{}
			}
			s.options.Now = originalClock
			if calls < 3 || mutationError != nil {
				t.Fatalf("fresh-fence root mutation did not execute: clock calls=%d error=%v", calls, mutationError)
			}
			if !errors.Is(err, ErrSourceUnavailable) || !reflect.DeepEqual(page, empty) || !reflect.DeepEqual(guard, ResponseGuard{}) {
				t.Fatalf("fresh root revocation escaped final check: page=%#v guard=%#v error=%v", page, guard, err)
			}
			sourceAfter, sourceError := s.SourceMetadata(context.Background(), event.Key)
			epochAfter, epochError := s.readSafety(event.Key.Account)
			if sourceError != nil || epochError != nil || sourceBefore != sourceAfter || epochBefore != epochAfter {
				t.Fatalf("root integrity fixture changed unrelated source/epoch: source=%#v epoch=%d errors=%v/%v", sourceAfter, epochAfter, sourceError, epochError)
			}
		})
	}
}

func TestRootReaderExactRootLimitsAreIndependentOfEncoder(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("anchor", "allowed", "Original bounded body", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	text := "rootlimit " + strings.Repeat("x", (64<<10)-len("rootlimit "))
	provenance := candidateRoot(t, s, p, "bounded-root", text, []MessageID{id}, derivedContextProvenance(DocumentOCR))
	encoded, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(encoded) + strings.Repeat(" ", 4096-len(encoded))
	if _, err = s.writeDB.Exec(`UPDATE extension_roots SET provenance_json=? WHERE id='bounded-root'`, raw); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, id))
	page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Body != event.Body || len(page.Messages[0].Derived) != 1 || page.Messages[0].Derived[0].Text != text || page.Messages[0].Derived[0].Provenance != provenance || len(guardedJSON(t, page)) > 128<<10 || guard.Generation != "" || guard.IndexVersionID != "" {
		t.Fatalf("exact 64KiB/4096-byte independent root limits failed: %#v %#v %v", page, guard, err)
	}
	rootReaderAssertPins(t, s, guard, "bounded-root")
	for _, corruption := range []string{"text", "provenance"} {
		if corruption == "text" {
			rootReaderRewriteText(t, s, "bounded-root", text+"x")
		} else {
			rootReaderRewriteText(t, s, "bounded-root", text)
			rootReaderCorrupt(t, s, `UPDATE extension_roots SET provenance_json=? WHERE id='bounded-root'`, raw+" ")
		}
		contextPage, contextGuard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
		if err != nil || len(contextPage.Messages) != 1 || contextPage.Messages[0].Body != event.Body || len(contextPage.Messages[0].Derived) != 0 || len(contextGuard.roots) != 0 {
			t.Fatalf("oversized %s root affected permitted original: %#v %#v %v", corruption, contextPage, contextGuard, err)
		}
		search, searchGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "rootlimit"})
		if err != nil || len(search.Hits) != 0 || len(searchGuard.roots) != 0 {
			t.Fatalf("oversized %s root admitted by FTS: %#v %#v %v", corruption, search, searchGuard, err)
		}
		if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("oversized %s root preserved prior contributing guard: %v", corruption, err)
		}
	}
}

func TestRootReaderSearchBoundsSelectedPinsEscapingAndCursor(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	text := strings.Repeat("\u0001", 400) + " boundaryneedle"
	events := make(map[MessageID]SourceEvent, 101)
	expected := make(map[MessageID]SourceReadVersion, 101)
	provenance := make(map[DocumentID]DerivedProvenance, 101)
	var chat ChatID
	for n := 0; n < 101; n++ {
		event := readerEvent(fmt.Sprintf("source-%03d", n), "allowed", "Original nonmatching source", now.Add(-time.Minute))
		id := readerApply(t, s, p, event)
		chat = readerChat(t, s, id)
		events[id], expected[id] = event, guardedExpectedSource(t, s, p, event, id)
		rootID := DocumentID(fmt.Sprintf("root-%03d", n))
		provenance[rootID] = candidateRoot(t, s, p, string(rootID), text, []MessageID{id}, derivedContextProvenance(DocumentOCR))
	}
	scope := readerScope(p, chat)
	page, guard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "boundaryneedle", Limit: 100})
	if err != nil || len(page.Hits) != 100 || page.NextCursor == "" {
		t.Fatalf("101-root bounded search: count=%d cursor=%q %v", len(page.Hits), page.NextCursor, err)
	}
	selected := make([]DocumentID, 0, 100)
	selectedSources := make([]SourceReadVersion, 0, 100)
	truncated := false
	for _, hit := range page.Hits {
		if len(hit.Derived) != 1 || hit.Derived[0].DocumentID != hit.DocumentID || hit.Derived[0].Provenance != provenance[hit.DocumentID] || !strings.HasPrefix(text, hit.Snippet) || !strings.HasPrefix(text, hit.Derived[0].Text) {
			t.Fatalf("escaped response rewrote root identity/provenance/text: %#v", hit)
		}
		selected = append(selected, hit.DocumentID)
		selectedSources = append(selectedSources, expected[hit.MessageID])
		truncated = truncated || len(hit.Snippet) < len(originalSnippet(text, "")) || len(hit.Derived[0].Text) < len(text)
		delete(expected, hit.MessageID)
	}
	if !truncated || len(guardedJSON(t, page)) > 128<<10 {
		t.Fatalf("root Search did not enforce exact escaped 128KiB JSON cap: bytes=%d truncated=%v", len(guardedJSON(t, page)), truncated)
	}
	guardedAssertSources(t, guard, selectedSources...)
	rootReaderAssertPins(t, s, guard, selected...)
	if len(expected) != 1 || len(guard.roots) != 100 {
		t.Fatalf("lookahead root became a contributing response pin: %#v", guard)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("exactly 100 root pins rejected: %v", err)
	}
	// The 101st root establishes NextCursor but contributes no displayed text.
	// Its deletion changes cursor identity without revoking the first 100 roots.
	for id := range expected {
		event := events[id]
		event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
		readerApply(t, s, p, event)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("undisplayed lookahead source entered selected-root guard: %v", err)
	}
	if next, nextGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "boundaryneedle", Limit: 100, Cursor: page.NextCursor}); !errors.Is(err, ErrCursorStale) || !reflect.DeepEqual(next, SearchPage{}) || !reflect.DeepEqual(nextGuard, ResponseGuard{}) {
		t.Fatalf("root revocation did not stale old cursor with zero output: %#v %#v %v", next, nextGuard, err)
	}
}

func TestRootReaderOriginalEvictionPreservesIndependentGeneratedText(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	ctx := context.Background()
	source, media, _ := extractionTestOriginal(t, s, p, "eviction-reader", JobOCR)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: source.Account, Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	document := extractionTestDocument(work, "retained-root", "evictionneedle retained generated text")
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{document}}); err != nil || !ok {
		t.Fatalf("independent extraction commit: %v %v", ok, err)
	}
	captionWork, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: source.Account, Version: extractionTestVersion(JobCaption), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: captionWork.Lease, ContentHash: captionWork.ContentHash, Outcome: ExtractionEmpty}); err != nil || !ok {
		t.Fatalf("explicit caption no-text completion: %v %v", ok, err)
	}
	scope := readerScope(p, readerChat(t, s, source.Version.MessageID))
	page, guard, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "evictionneedle"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].DocumentID != document.ID {
		t.Fatalf("pre-eviction independent root search: %#v %v", page, err)
	}
	artifact, err := s.BeginEviction(ctx, media)
	if err != nil || artifact.State != "evicting" || artifact.CleanupLease == "" {
		t.Fatalf("completed extraction original cannot evict: %#v %v", artifact, err)
	}
	if err = s.FinishMediaCleanup(ctx, MediaCleanup{ReservationID: artifact.ReservationID, ArtifactID: artifact.ID, LeaseID: artifact.CleanupLease, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("cache eviction revoked valid generated root: %v", err)
	}
	after, err := s.Search(ctx, scope, SearchRequest{Query: "evictionneedle"})
	if err != nil || len(after.Hits) != 1 || after.Hits[0].DocumentID != document.ID || after.Hits[0].Snippet != document.Text || len(after.Hits[0].Derived) != 1 || after.Hits[0].Derived[0].Provenance != document.Provenance {
		t.Fatalf("root depended on retained original cache after eviction: %#v %v", after, err)
	}
	contextPage, err := s.Context(ctx, scope, ContextRequest{MessageID: source.Version.MessageID})
	if err != nil || len(contextPage.Messages) != 1 || len(contextPage.Messages[0].Derived) != 1 || contextPage.Messages[0].Derived[0].Text != document.Text {
		t.Fatalf("cache eviction removed generated context: %#v %v", contextPage, err)
	}
}
