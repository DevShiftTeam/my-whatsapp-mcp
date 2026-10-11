//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func aliasRetainedVoice(t *testing.T, s *Store, p ArchivePolicy, name string, change func(*SourceEvent)) (SourceEvent, SourceRef, MediaID, ExtractionWork) {
	t.Helper()
	e := event(name, "", 1, EventOriginal)
	e.BodyKind = MessageVoice
	e.Caption = "synthetic original voice caption " + name
	e.Media = []MediaReference{{Kind: "voice", ProtocolReference: originalDownloadTestReference(t, MessageVoice, name), MimeType: "audio/ogg", DeclaredBytes: 64, Availability: "not_retained"}}
	if change != nil {
		change(&e)
	}
	applied := apply(t, s, e, p.Generation)
	if len(applied.MediaIDs) != 1 {
		t.Fatal("fixture did not create one voice association")
	}
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	media := applied.MediaIDs[0]
	reservation := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatalf("fixture voice publication: %v %v", ok, err)
	}
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: extractionTestVersion(JobTranscription), TTL: time.Minute})
	if err != nil || work.MediaID != media {
		t.Fatalf("fixture voice lease: %v", err)
	}
	return e, ref, media, work
}

func aliasCommitTranscript(t *testing.T, s *Store, work ExtractionWork, name string) Document {
	t.Helper()
	doc := extractionTestDocument(work, "alias-transcript-"+name, "aliasretainedspokenphrase "+name)
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{doc}}); err != nil || !ok {
		t.Fatalf("fixture transcript commit: %v %v", ok, err)
	}
	return doc
}

func aliasAssertRetained(t *testing.T, s *Store, p ArchivePolicy, media MediaID, doc Document) {
	t.Helper()
	ctx := context.Background()
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	if _, err := s.AuthorizeOriginal(ctx, scope, media); err != nil {
		t.Errorf("unrelated alias revoked retained voice: %v", err)
	}
	var state, completed string
	if err := s.readDB.QueryRow("SELECT state,completed_sha256 FROM extension_original_downloads WHERE media_id=?", media).Scan(&state, &completed); err != nil || state != "complete" || completed != strings.Repeat("a", 64) {
		t.Errorf("unrelated alias changed original completion: state=%s %v", state, err)
	}
	var roots int
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_roots WHERE id=?", doc.ID).Scan(&roots); err != nil || roots != 1 {
		t.Errorf("unrelated alias discarded completed transcript: count=%d %v", roots, err)
	}
	pair, err := s.CandidatePair(ctx, scope, CandidatePairRequest{Search: SearchRequest{Query: "aliasretainedspokenphrase", Limit: 10}, KeywordOnly: true})
	found := false
	for _, item := range pair.Keyword {
		found = found || item.Ref.ID == doc.ID
	}
	if err != nil || !found {
		t.Errorf("unrelated alias removed transcript from keyword search: %v", err)
	}
}

func aliasAssertTerminal(t *testing.T, s *Store, p ArchivePolicy, e SourceEvent, work ExtractionWork) {
	t.Helper()
	var state string
	var row, revision, contentBytes, indexed int64
	if err := s.readDB.QueryRow("SELECT rowid,state,local_revision,length(body)+length(caption)+length(normalized_body)+length(normalized_caption)+length(content_hash)+length(extension_media_identity)+length(reply_account)+length(reply_chat)+length(reply_sender)+length(reply_message) FROM messages WHERE id=?", work.Source.Version.MessageID).Scan(&row, &state, &revision, &contentBytes); err != nil || state != "deleted" || revision != int64(work.Source.Version.Revision)+1 || contentBytes != 0 {
		t.Errorf("alias tombstone did not terminalize original source: state=%s revision=%d content_bytes=%d %v", state, revision, contentBytes, err)
	}
	if err := s.readDB.QueryRow("SELECT count(*) FROM message_fts WHERE rowid=?", row).Scan(&indexed); err != nil || indexed != 0 {
		t.Errorf("alias tombstone left original keyword authority: count=%d %v", indexed, err)
	}
	replay := apply(t, s, e, p.Generation)
	if replay.Outcome != OutcomeStale || !replay.Version.Deleted {
		t.Errorf("source replay resurrected canonicalized deletion: outcome=%s deleted=%v", replay.Outcome, replay.Version.Deleted)
	}
}

func TestAliasWithoutSourceReferencesPreservesCompletedVoice(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	_, _, media, work := aliasRetainedVoice(t, s, p, "unrelated", nil)
	doc := aliasCommitTranscript(t, s, work, "unrelated")
	_, imageRef, image := originalDownloadTestSource(t, s, p, "unrelated-image", MessageImage)
	extensionPublish(t, s, extensionAdmit(t, s, imageRef, image, BudgetOriginals, 128), 64)
	if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	aliasAssertRetained(t, s, p, media, doc)
	if _, err := s.AuthorizeOriginal(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, image); err != nil {
		t.Errorf("unrelated alias revoked retained image: %v", err)
	}
}

func TestAliasInvalidatesOnlyReferencedSources(t *testing.T) {
	for _, field := range []string{"chat", "sender", "reply_chat", "reply_sender"} {
		t.Run(field, func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			_, _, keepMedia, keepWork := aliasRetainedVoice(t, s, p, "unrelated", nil)
			keepDoc := aliasCommitTranscript(t, s, keepWork, "unrelated")
			_, _, changedMedia, changedWork := aliasRetainedVoice(t, s, p, "affected", func(e *SourceEvent) {
				switch field {
				case "chat":
					e.Key.Chat = "456@lid"
				case "sender":
					e.Key.Sender = "456@lid"
				case "reply_chat", "reply_sender":
					key := e.Key
					key.Message = "quoted-source"
					if field == "reply_chat" {
						key.Chat = "456@lid"
					} else {
						key.Sender = "456@lid"
					}
					e.ReplyTo = &key
				}
			})
			changedDoc := aliasCommitTranscript(t, s, changedWork, "affected")
			if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
				t.Fatal(err)
			}
			aliasAssertRetained(t, s, p, keepMedia, keepDoc)
			var roots int
			if err := s.readDB.QueryRow("SELECT count(*) FROM extension_roots WHERE id=?", changedDoc.ID).Scan(&roots); err != nil || roots != 0 {
				t.Errorf("alias retained obsolete source transcript: count=%d %v", roots, err)
			}
			if _, err := s.AuthorizeOriginal(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, changedMedia); !errors.Is(err, ErrSourceUnavailable) {
				t.Errorf("alias retained obsolete original authority: %v", err)
			}
		})
	}
}

func TestAliasPreservesUnrelatedExtractionLeaseAndRevokesChangedLease(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	_, _, keepMedia, keepWork := aliasRetainedVoice(t, s, p, "running-unrelated", nil)
	_, _, changedMedia, changedWork := aliasRetainedVoice(t, s, p, "running-affected", func(e *SourceEvent) { e.Key.Sender = "456@lid" })
	if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadExtractionInput(context.Background(), changedWork.Lease); !errors.Is(err, ErrNoWork) {
		t.Errorf("alias did not revoke old input lease: %v", err)
	}
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: changedWork.Lease, ContentHash: changedWork.ContentHash, Outcome: ExtractionText, Documents: []Document{extractionTestDocument(changedWork, "obsolete-alias-root", "obsolete generated text")}}); err != nil || ok {
		t.Errorf("old alias source committed after identity remap: %v %v", ok, err)
	}
	if _, err := s.BeginEviction(context.Background(), changedMedia); !errors.Is(err, ErrNoWork) {
		t.Errorf("alias erased stopped-worker physical pin: %v", err)
	}
	keepDoc := extractionTestDocument(keepWork, "alias-transcript-running-unrelated", "aliasretainedspokenphrase running-unrelated")
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: keepWork.Lease, ContentHash: keepWork.ContentHash, Outcome: ExtractionText, Documents: []Document{keepDoc}}); err != nil || !ok {
		t.Fatalf("unrelated alias revoked live extraction lease: %v %v", ok, err)
	}
	aliasAssertRetained(t, s, p, keepMedia, keepDoc)
}

func TestAliasTransferredChatTombstoneRevokesOnlyCoveredPNMedia(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	oldEvent, _, oldMedia, oldWork := aliasRetainedVoice(t, s, p, "old-pn", func(e *SourceEvent) { e.Key.Chat = "123@s.whatsapp.net" })
	oldDoc := aliasCommitTranscript(t, s, oldWork, "old-pn")
	_, _, newerMedia, newerWork := aliasRetainedVoice(t, s, p, "new-pn", func(e *SourceEvent) {
		e.Key.Chat = "123@s.whatsapp.net"
		e.OriginalSentAt = oldEvent.OriginalSentAt.Add(2 * time.Minute)
	})
	newerDoc := aliasCommitTranscript(t, s, newerWork, "new-pn")
	_, _, unrelatedMedia, unrelatedWork := aliasRetainedVoice(t, s, p, "outside-tombstone", nil)
	unrelatedDoc := aliasCommitTranscript(t, s, unrelatedWork, "outside-tombstone")
	if err := s.ClearChat(context.Background(), "account", "456@lid", oldEvent.OriginalSentAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	_, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: oldWork.Source.Version.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	var roots int
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_roots WHERE id=?", oldDoc.ID).Scan(&roots); err != nil || roots != 0 {
		t.Errorf("canonicalized chat tombstone retained old transcript: count=%d %v", roots, err)
	}
	if _, err := s.AuthorizeOriginal(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, oldMedia); !errors.Is(err, ErrSourceUnavailable) {
		t.Errorf("canonicalized chat tombstone retained old original: %v", err)
	}
	if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Errorf("pre-alias response survived canonicalized deletion: %v", err)
	}
	aliasAssertTerminal(t, s, p, oldEvent, oldWork)
	aliasAssertRetained(t, s, p, newerMedia, newerDoc)
	aliasAssertRetained(t, s, p, unrelatedMedia, unrelatedDoc)
}

func TestAliasTransferredSourceTombstoneRevokesOnlyExactPNMedia(t *testing.T) {
	for _, field := range []string{"chat", "sender"} {
		t.Run(field, func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			setPN := func(e *SourceEvent) { e.Key.Chat, e.Key.Sender = "123@s.whatsapp.net", "123@s.whatsapp.net" }
			oldEvent, _, oldMedia, oldWork := aliasRetainedVoice(t, s, p, "deleted-pn", setPN)
			oldDoc := aliasCommitTranscript(t, s, oldWork, "deleted-pn")
			_, _, keepMedia, keepWork := aliasRetainedVoice(t, s, p, "different-pn-message", setPN)
			keepDoc := aliasCommitTranscript(t, s, keepWork, "different-pn-message")
			chat, sender := oldEvent.Key.Chat, oldEvent.Key.Sender
			if field == "chat" {
				chat = "456@lid"
			} else {
				sender = "456@lid"
			}
			// Model an accepted deletion anchor whose original body was never imported.
			if _, err := s.writeDB.Exec("INSERT INTO source_tombstones(account,chat,sender,message,from_me,protocol_revision) VALUES(?,?,?,?,?,?)", "account", chat, sender, oldEvent.Key.Message, oldEvent.Key.FromMe, 1); err != nil {
				t.Fatal(err)
			}
			if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
				t.Fatal(err)
			}
			var roots int
			if err := s.readDB.QueryRow("SELECT count(*) FROM extension_roots WHERE id=?", oldDoc.ID).Scan(&roots); err != nil || roots != 0 {
				t.Errorf("canonicalized source tombstone retained exact transcript: count=%d %v", roots, err)
			}
			if _, err := s.AuthorizeOriginal(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, oldMedia); !errors.Is(err, ErrSourceUnavailable) {
				t.Errorf("canonicalized source tombstone retained exact original: %v", err)
			}
			aliasAssertTerminal(t, s, p, oldEvent, oldWork)
			aliasAssertRetained(t, s, p, keepMedia, keepDoc)
		})
	}
}

func TestAliasMergedMaximumCutoffTerminalizesMovedLIDSource(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	oldEvent, _, _, oldWork := aliasRetainedVoice(t, s, p, "cleared-lid", func(e *SourceEvent) { e.Key.Chat = "456@lid" })
	aliasCommitTranscript(t, s, oldWork, "cleared-lid")
	if err := s.ClearChat(context.Background(), "account", "123@s.whatsapp.net", oldEvent.OriginalSentAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearChat(context.Background(), "account", "456@lid", oldEvent.OriginalSentAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	aliasAssertTerminal(t, s, p, oldEvent, oldWork)
}

func TestAliasInvalidatesMixedWindowWhenOnlyNonAnchorMemberChanges(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	anchor := event("mixed-anchor", "anchor body", 1, EventOriginal)
	anchor.Key.Chat = "mixed-chat"
	apply(t, s, anchor, p.Generation)
	member := event("mixed-member", "mapped body", 1, EventOriginal)
	member.Key.Chat, member.Key.Sender = "mixed-chat", "456@lid"
	member.OriginalSentAt = anchor.OriginalSentAt.Add(time.Minute)
	apply(t, s, member, p.Generation)
	generation, err := s.EnsureIndex(context.Background(), indexTestVersion("alias-mixed-window"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, generation, 256)
	mixed := Document{ID: "alias-mixed-window", Kind: DocumentWindow, Text: "anchor body\nmapped body", PassageTokens: 4, Provenance: DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"}}
	for _, document := range publication.Documents {
		mixed.Sources = append(mixed.Sources, document.Sources...)
		mixed.Spans = append(mixed.Spans, document.Spans...)
	}
	if len(mixed.Sources) != 2 {
		t.Fatal("fixture did not create a two-source window")
	}
	publication.Documents = []Document{mixed}
	if ok, err := s.PublishWindows(context.Background(), publication); err != nil || !ok {
		t.Fatalf("fixture mixed publication: %v %v", ok, err)
	}
	_, _, keepMedia, keepWork := aliasRetainedVoice(t, s, p, "independent-window-voice", nil)
	keepDoc := aliasCommitTranscript(t, s, keepWork, "independent-window-voice")
	if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	var documents int
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_documents WHERE id=?", mixed.ID).Scan(&documents); err != nil || documents != 0 {
		t.Errorf("alias retained window with changed non-anchor source: count=%d %v", documents, err)
	}
	aliasAssertRetained(t, s, p, keepMedia, keepDoc)
}
