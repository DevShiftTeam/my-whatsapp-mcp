//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func documentFailDownload(t *testing.T, s *Store, account string) OriginalDownloadWork {
	t.Helper()
	work, err := s.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: account, TTL: time.Minute})
	if err != nil || work.Lease.LeaseID == "" {
		t.Fatal("document claim unavailable", err)
	}
	if err = s.RetryOriginalDownload(context.Background(), OriginalDownloadRetry{Lease: work.Lease, Code: "media_unavailable", After: time.Second, Terminal: true}); err != nil {
		t.Fatal(err)
	}
	return work
}

func TestDocumentDownloadsEnforceScopeCompletionAndDeletion(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	doc := documentEvent(t, "document-download")
	r := apply(t, s, doc, p.Generation)
	if len(r.MediaIDs) != 1 {
		t.Fatal("document association missing")
	}
	work, err := s.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: time.Minute})
	if err != nil || work.Kind != MessageDocument || work.DeclaredBytes != 64 {
		t.Fatal("document descriptor not scheduled", err)
	}
	input, err := s.LoadOriginalDownload(context.Background(), work.Lease)
	if err != nil || input.Key != doc.Key || !bytes.Equal(input.ProtocolReference, doc.Media[0].ProtocolReference) {
		t.Fatal("document source/protocol pin lost", err)
	}
	clear(input.ProtocolReference)
	if err = s.RetryOriginalDownload(context.Background(), OriginalDownloadRetry{Lease: work.Lease, Code: "media_unavailable", After: time.Second, Terminal: true}); err != nil {
		t.Fatal(err)
	}
	denied := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, Chats: []ChatID{"not-this-chat"}}
	if retry, err := s.RetryDocumentDownloads(context.Background(), denied, []MessageID{r.MessageID}); err != nil || retry.Queued != 0 || retry.Skipped != 1 {
		t.Fatal("out-of-scope document retry gained authority", err)
	}
	scope := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}
	if retry, err := s.RetryDocumentDownloads(context.Background(), scope, []MessageID{r.MessageID}); err != nil || retry.Queued != 1 || retry.Skipped != 0 {
		t.Fatal("authorized document retry unavailable", err)
	}
	work, err = s.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: time.Minute})
	if err != nil || work.Attempts != 1 {
		t.Fatal("bounded explicit retry did not renew work", err)
	}
	reservation := extensionAdmit(t, s, work.Source, r.MediaIDs[0], BudgetOriginals, 100)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("b", 64), Bytes: 64, ActualFormat: "pdf"}); err != nil || !ok {
		t.Fatal("document original not retained", err)
	}
	var completionState, completionDigest string
	if err = s.readDB.QueryRow(`SELECT state,completed_sha256 FROM extension_original_downloads WHERE media_id=?`, r.MediaIDs[0]).Scan(&completionState, &completionDigest); err != nil || completionState != "complete" || completionDigest != strings.Repeat("b", 64) {
		t.Fatal("publication did not atomically preserve document completion proof", err)
	}
	if ok, err := s.FinishOriginalDownload(context.Background(), work.Lease); err != nil || !ok {
		t.Fatal("completion proof not recorded", err)
	}
	if retry, err := s.RetryDocumentDownloads(context.Background(), scope, []MessageID{r.MessageID}); err != nil || retry.Queued != 0 || retry.Skipped != 1 {
		t.Fatal("completed original reset by fetch", err)
	}
	artifact, err := s.AuthorizeOriginal(context.Background(), scope, r.MediaIDs[0])
	if err != nil || artifact.ActualFormat != "pdf" || artifact.Bytes != 64 {
		t.Fatal("verified document metadata unavailable", err)
	}
	if _, err = s.AuthorizeOriginal(context.Background(), denied, r.MediaIDs[0]); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatal("scope did not fence original", err)
	}
	candidates, err := s.EvictionCandidates(context.Background(), 10)
	if err != nil || len(candidates) != 1 || candidates[0] != r.MediaIDs[0] {
		t.Fatal("documents waited for nonexistent extractor jobs", err)
	}
	eviction, err := s.BeginEviction(context.Background(), r.MediaIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthorizeOriginal(context.Background(), scope, r.MediaIDs[0]); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatal("evicting document remained readable", err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 64 {
		t.Fatal("physical eviction released charge before cleanup")
	}
	if err = s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: eviction.ReservationID, ArtifactID: eviction.ID, LeaseID: eviction.CleanupLease, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
	if retry, err := s.RetryDocumentDownloads(context.Background(), scope, []MessageID{r.MessageID}); err != nil || retry.Queued != 0 {
		t.Fatal("evicted completed document was automatically redownloaded", err)
	}
	del := doc
	del.Kind = EventDelete
	del.SourceRevision = 2
	del.Body = ""
	del.Caption = ""
	del.Media = nil
	apply(t, s, del, p.Generation)
	if _, err = s.LoadOriginalDownload(context.Background(), work.Lease); err == nil {
		t.Fatal("deleted document lease remained usable")
	}
	if retry, err := s.RetryDocumentDownloads(context.Background(), scope, []MessageID{r.MessageID}); err != nil || retry.Queued != 0 {
		t.Fatal("deleted document gained fetch authority", err)
	}
}

func TestDocumentMetadataWithoutProtocolAndHardAdmissionCap(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	doc := documentEvent(t, "metadata-only-document")
	doc.Media[0].ProtocolReference = nil
	doc.Media[0].Availability = "unavailable"
	r := apply(t, s, doc, p.Generation)
	if len(r.MediaIDs) != 1 {
		t.Fatal("unavailable attachment lost searchable metadata")
	}
	var filename, kind string
	var size int
	if err := s.readDB.QueryRow(`SELECT filename,typeof(protocol_reference),length(protocol_reference) FROM extension_media_associations WHERE id=?`, r.MediaIDs[0]).Scan(&filename, &kind, &size); err != nil || filename != doc.Media[0].Filename || kind != "blob" || size != 0 {
		t.Fatal("metadata-only reference changed", err)
	}
	if work, err := s.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: time.Minute}); (err != nil && !errors.Is(err, ErrNoWork)) || work.Lease.LeaseID != "" {
		t.Fatal("missing protocol authorized a download", err)
	}
	doc = documentEvent(t, "cap-document")
	r = apply(t, s, doc, p.Generation)
	source, err := s.CurrentSource(context.Background(), doc.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: source, MediaID: r.MediaIDs[0], Budget: BudgetOriginals, MaximumBytes: MaximumDocumentBytes + 27, TTL: time.Minute}); !errors.Is(err, ErrInvalidExtension) {
		t.Fatal("document ciphertext cap bypassed", err)
	}
	if _, err = s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: source, MediaID: r.MediaIDs[0], Budget: BudgetDecoded, MaximumBytes: 64, TTL: time.Minute}); !errors.Is(err, ErrInvalidExtension) {
		t.Fatal("document admitted a decoder/model reservation", err)
	}
	reservation := extensionAdmit(t, s, source, r.MediaIDs[0], BudgetOriginals, 100)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "png"}); !errors.Is(err, ErrInvalidExtension) || ok {
		t.Fatal("document masqueraded as an image", err)
	}
	for _, bad := range []string{strings.Repeat("x", 256), "bad\x00.pdf", string([]byte{0xff})} {
		doc.Media[0].Filename = bad
		if _, err = s.ApplySourceEvent(context.Background(), doc, p.Generation); !errors.Is(err, ErrInvalidEvent) {
			t.Fatal("unbounded/invalid filename admitted", err)
		}
	}
}

func TestDocumentRedeliveryIsOwnerOnlyBoundedAndNeverResetsCompletedOriginal(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	legacy := event("legacy-document-target", "", 1, EventOriginal)
	legacy.BodyKind = MessageUnsupported
	legacyResult := markLegacyDocument(t, s, legacy, p)
	doc := documentEvent(t, "failed-document-target")
	docResult := apply(t, s, doc, p.Generation)
	documentFailDownload(t, s, "account")
	owner := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}
	ids := []MessageID{legacyResult.MessageID, docResult.MessageID}
	items, err := s.DocumentRedeliverySources(context.Background(), owner, ids)
	if err != nil || len(items) != 2 {
		t.Fatal("eligible owner document targets unavailable", err)
	}
	client := owner
	client.Principal = "fixture-client"
	if _, err = s.DocumentRedeliverySources(context.Background(), client, ids); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("read client acquired phone-copy authority", err)
	}
	if _, err = s.DocumentRedeliverySources(context.Background(), owner, append(ids, ids...)); !errors.Is(err, ErrInvalidExtension) {
		t.Fatal("duplicate targets admitted", err)
	}
	if _, err = s.DocumentRedeliverySources(context.Background(), owner, make([]MessageID, 9)); !errors.Is(err, ErrInvalidExtension) {
		t.Fatal("unbounded targets admitted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.RetryDocumentDownloads(ctx, owner, []MessageID{docResult.MessageID}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled retry admitted", err)
	}
	items, err = s.DocumentRedeliverySources(context.Background(), owner, ids)
	if err != nil || len(items) != 2 {
		t.Fatal("cancelled optional retry poisoned readable sources", err)
	}
	if retry, err := s.RetryDocumentDownloads(context.Background(), owner, []MessageID{docResult.MessageID}); err != nil || retry.Queued != 1 {
		t.Fatal(err)
	}
	work, err := s.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	reservation := extensionAdmit(t, s, work.Source, docResult.MediaIDs[0], BudgetOriginals, 100)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "document"}); err != nil || !ok {
		t.Fatal(err)
	}
	items, err = s.DocumentRedeliverySources(context.Background(), owner, ids)
	if err != nil || len(items) != 1 || items[0].Source.Version.MessageID != legacyResult.MessageID {
		t.Fatal("completed document requested another phone copy", err)
	}
}
