//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func extensionTestMedia(t *testing.T, s *Store, p ArchivePolicy, name string) (SourceRef, MediaID) {
	t.Helper()
	e := event(name, "synthetic image caption", 1, EventOriginal)
	e.BodyKind = MessageImage
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("authenticated synthetic key"), MimeType: "image/png", Availability: "not_retained"}}
	r := apply(t, s, e, p.Generation)
	source, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	var id MediaID
	if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", r.MessageID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return source, id
}

func extensionAdmit(t *testing.T, s *Store, source SourceRef, id MediaID, budget BudgetKind, bytes int64) MediaReservation {
	t.Helper()
	r, err := s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: source, MediaID: id, Budget: budget, MaximumBytes: bytes, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if r.ArtifactID == "" || r.ID == "" || r.LeaseID == "" || r.MaximumBytes != bytes {
		t.Fatalf("incomplete admission: %#v", r)
	}
	return r
}

func extensionPublish(t *testing.T, s *Store, r MediaReservation, bytes int64) {
	t.Helper()
	ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: bytes, ActualFormat: "png"})
	if err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
}

func extensionMediaCharge(t *testing.T, s *Store, budget BudgetKind) int64 {
	t.Helper()
	var total int64
	if err := s.readDB.QueryRow("SELECT coalesce(sum(charged_bytes),0) FROM extension_media_artifacts WHERE budget=? AND cleanup_ack=0", budget).Scan(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

func TestMediaReservationsCountCompetingInflightAndIndependentPools(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	first, firstID := extensionTestMedia(t, s, p, "first")
	second, secondID := extensionTestMedia(t, s, p, "second")
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, input := range []struct {
		source SourceRef
		id     MediaID
	}{{first, firstID}, {second, secondID}} {
		wg.Add(1)
		go func(source SourceRef, id MediaID) {
			defer wg.Done()
			<-start
			_, err := s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: source, MediaID: id, Budget: BudgetOriginals, MaximumBytes: (1 << 30) + 1, TTL: time.Minute})
			results <- err
		}(input.source, input.id)
	}
	close(start)
	wg.Wait()
	close(results)
	success, quota := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrQuotaExceeded) {
			quota++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || quota != 1 || extensionMediaCharge(t, s, BudgetOriginals) != (1<<30)+1 {
		t.Fatalf("competing quota success=%d quota=%d", success, quota)
	}
	extensionAdmit(t, s, first, firstID, BudgetDecoded, 128<<20)
	if _, err := s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: second, MediaID: secondID, Budget: BudgetDecoded, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("decoded quota not shared: %v", err)
	}
}

func TestMediaLeaseExpiryAndReopenDoNotFreeQuotaUntilCleanup(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy(t, s)
	source, id := extensionTestMedia(t, s, p, "crashed")
	r := extensionAdmit(t, s, source, id, BudgetOriginals, 2<<30)
	now = now.Add(2 * time.Minute)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, otherID := extensionTestMedia(t, s, p, "other")
	if _, err = s.AdmitMedia(ctx, MediaAdmissionRequest{Source: other, MediaID: otherID, Budget: BudgetOriginals, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expired reservation released without cleanup: %v", err)
	}
	artifacts, err := s.ReconcileMedia(ctx, 1)
	if err != nil || len(artifacts) != 1 || artifacts[0].ID != r.ArtifactID || artifacts[0].ReservedBytes != 2<<30 {
		t.Fatalf("reconcile %#v %v", artifacts, err)
	}
	cleanup := MediaCleanup{ReservationID: artifacts[0].ReservationID, ArtifactID: artifacts[0].ID, LeaseID: artifacts[0].CleanupLease, Code: "removed"}
	if err = s.FinishMediaCleanup(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishMediaCleanup(ctx, cleanup); err != nil {
		t.Fatalf("duplicate cleanup: %v", err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 0 {
		t.Fatal("cleanup retained charge")
	}
	extensionAdmit(t, s, other, otherID, BudgetOriginals, 2<<30)
}

func TestMediaStalePublicationRetainsAccountingAndRejectsForgedReservation(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	source, id := extensionTestMedia(t, s, p, "stale")
	r := extensionAdmit(t, s, source, id, BudgetOriginals, 1024)
	forged := r
	forged.MaximumBytes++
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: forged, SHA256: strings.Repeat("b", 64), Bytes: 10, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("forged reservation published: %v %v", ok, err)
	}
	apply(t, s, event("stale", "", 2, EventDelete), p.Generation)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("b", 64), Bytes: 10, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("stale source published: %v %v", ok, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 1024 {
		t.Fatal("stale publication lost cleanup accounting")
	}
	if _, err := s.AuthorizeOriginal(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, id); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("stale original authorized: %v", err)
	}
	items, err := s.ReconcileMedia(context.Background(), 5)
	if err != nil || len(items) != 1 {
		t.Fatalf("missing cleanup: %#v %v", items, err)
	}
	if err = s.FinishMediaCleanup(context.Background(), MediaCleanup{ArtifactID: items[0].ID, ReservationID: items[0].ReservationID, LeaseID: items[0].CleanupLease, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaOriginalAuthorizationNeverFollowsHashAcrossSourceScope(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	first, firstID := extensionTestMedia(t, s, p, "hash-first")
	second, secondID := extensionTestMedia(t, s, p, "hash-second")
	extensionPublish(t, s, extensionAdmit(t, s, first, firstID, BudgetOriginals, 64), 32)
	extensionPublish(t, s, extensionAdmit(t, s, second, secondID, BudgetOriginals, 64), 32)
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	allowed, err := s.AuthorizeOriginal(context.Background(), scope, secondID)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, event("hash-first", "", 2, EventDelete), p.Generation)
	if _, err = s.AuthorizeOriginal(context.Background(), scope, firstID); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("deleted hash association allowed: %v", err)
	}
	current, err := s.AuthorizeOriginal(context.Background(), scope, secondID)
	if err != nil || current.ID != allowed.ID {
		t.Fatalf("unrelated shared hash denied: %#v %v", current, err)
	}
	scope.Account = "another-account"
	if _, err = s.AuthorizeOriginal(context.Background(), scope, secondID); !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("wrong account authorized: %v", err)
	}
	scope.Account = "account"
	scope.AllChats = false
	scope.Chats = []ChatID{"other-chat"}
	if _, err = s.AuthorizeOriginal(context.Background(), scope, secondID); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("wrong chat authorized: %v", err)
	}
}

func TestMediaTwoPhaseEvictionRequiresProcessedJobsAndCleanupAck(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	_, err := s.EnsureIndex(context.Background(), IndexVersion{ID: "media-test-v1", EncoderProfile: "synthetic", TokenizerSHA256: strings.Repeat("c", 64), TokenizerImplementation: "test", PrefixVersion: "v1", ChunkingVersion: "v1", NormalizationVersion: LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 128})
	if err != nil {
		t.Fatal(err)
	}
	source, id := extensionTestMedia(t, s, p, "eviction")
	r := extensionAdmit(t, s, source, id, BudgetOriginals, 64)
	extensionPublish(t, s, r, 32)
	items, err := s.EvictionCandidates(context.Background(), 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("pending original offered: %#v %v", items, err)
	}
	if _, err = s.BeginEviction(context.Background(), id); !errors.Is(err, ErrNoWork) {
		t.Fatalf("pending original evicted: %v", err)
	}
	extractionCompleteImage(t, s, id)
	if _, err = s.writeDB.Exec("UPDATE extension_root_sources SET policy_generation=policy_generation+1 WHERE root_id IN (SELECT id FROM extension_roots WHERE media_id=?)", id); err != nil {
		t.Fatal(err)
	}
	items, err = s.EvictionCandidates(context.Background(), 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("old source completion authorized eviction: %#v %v", items, err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_root_sources SET policy_generation=? WHERE root_id IN (SELECT id FROM extension_roots WHERE media_id=?)", source.Version.PolicyGeneration, id); err != nil {
		t.Fatal(err)
	}
	extra, _ := extensionTestMedia(t, s, p, "unexpected-job-contributor")
	if _, err = s.writeDB.Exec(`INSERT INTO extension_root_sources(root_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal) SELECT id,?,?,?,?,?,?,1 FROM extension_roots WHERE media_id=?`, extra.Account, extra.Version.MessageID, extra.Version.Revision, extra.Version.PolicyGeneration, unixms(extra.Version.ExpiresAt), extra.ContentHash, id); err != nil {
		t.Fatal(err)
	}
	items, err = s.EvictionCandidates(context.Background(), 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("extra root contributor authorized eviction: %#v %v", items, err)
	}
	if _, err = s.BeginEviction(context.Background(), id); !errors.Is(err, ErrNoWork) {
		t.Fatalf("inexact contributor membership evicted original: %v", err)
	}
	if _, err = s.writeDB.Exec("DELETE FROM extension_root_sources WHERE message_id=?", extra.Version.MessageID); err != nil {
		t.Fatal(err)
	}
	items, err = s.EvictionCandidates(context.Background(), 10)
	if err != nil || len(items) != 1 || items[0] != id {
		t.Fatalf("processed original unavailable: %#v %v", items, err)
	}
	artifact, err := s.BeginEviction(context.Background(), id)
	if err != nil || artifact.State != "evicting" || artifact.CleanupLease == "" {
		t.Fatalf("eviction %#v %v", artifact, err)
	}
	if _, err = s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: source, MediaID: id, Budget: BudgetDecoded, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("new decoded reader admitted after eviction began: %v", err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 32 {
		t.Fatal("eviction freed bytes before cleanup")
	}
	if err = s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: artifact.ReservationID, ArtifactID: artifact.ID, LeaseID: "wrong", Code: "removed"}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("wrong lease released bytes: %v", err)
	}
	if err = s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: artifact.ReservationID, ArtifactID: artifact.ID, LeaseID: artifact.CleanupLease, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 0 {
		t.Fatal("eviction cleanup did not release")
	}
}

func TestMediaAdmissionRejectsViewOnceStaleAndCrossAssociation(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	first, firstID := extensionTestMedia(t, s, p, "association-one")
	second, _ := extensionTestMedia(t, s, p, "association-two")
	request := MediaAdmissionRequest{Source: second, MediaID: firstID, Budget: BudgetOriginals, MaximumBytes: 10, TTL: time.Minute}
	if _, err := s.AdmitMedia(context.Background(), request); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("cross source associated: %v", err)
	}
	request.Source = first
	if _, err := s.writeDB.Exec("UPDATE messages SET view_once=1 WHERE id=?", first.Version.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitMedia(context.Background(), request); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("view-once admitted: %v", err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 0 {
		t.Fatal("denied admission charged bytes")
	}
}

func TestMediaTrueSharedArtifactKeepsEachSourceAuthority(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	first, firstID := extensionTestMedia(t, s, p, "shared-one")
	_, secondID := extensionTestMedia(t, s, p, "shared-two")
	r := extensionAdmit(t, s, first, firstID, BudgetOriginals, 64)
	extensionPublish(t, s, r, 32)
	// This fixture models an internally verified shared-file adoption, never a
	// public shared-hash lookup. Both media IDs retain their exact source refs.
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id=? WHERE id=?", r.ArtifactID, secondID); err != nil {
		t.Fatal(err)
	}
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	artifact, err := s.AuthorizeOriginal(context.Background(), scope, secondID)
	if err != nil || artifact.ID != r.ArtifactID || artifact.MediaID != secondID || artifact.Source.Version.MessageID == first.Version.MessageID {
		t.Fatalf("shared authorization %#v %v", artifact, err)
	}
	apply(t, s, event("shared-one", "", 2, EventDelete), p.Generation)
	if _, err = s.AuthorizeOriginal(context.Background(), scope, firstID); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("deleted association allowed: %v", err)
	}
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 32, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("deleted source retry %v %v", ok, err)
	}
	if _, err = s.AuthorizeOriginal(context.Background(), scope, secondID); err != nil {
		t.Fatalf("allowed shared association lost: %v", err)
	}
	if records, err := s.ReconcileMedia(context.Background(), 10); err != nil || len(records) != 0 {
		t.Fatalf("live shared artifact offered for cleanup: %#v %v", records, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 32 {
		t.Fatal("shared file charge duplicated or removed")
	}
}

func TestMediaRetainedCleanupRejectsDownloadLeaseAndPublicationIsIdempotent(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "idempotent")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	extensionPublish(t, s, r, 20)
	extensionPublish(t, s, r, 20)
	if extensionMediaCharge(t, s, BudgetOriginals) != 20 {
		t.Fatal("retry double charged original")
	}
	if err := s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: r.ID, ArtifactID: r.ArtifactID, LeaseID: r.LeaseID, Code: "removed"}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("download cleanup freed retained bytes: %v", err)
	}
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("b", 64), Bytes: 20, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("conflicting retry mutated original: %v %v", ok, err)
	}
}

func TestMediaBackgroundCancellationPreservesKeywordAvailability(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "cancelled")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RenewMedia(ctx, r, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("renew cancellation %v", err)
	}
	if _, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 20, ActualFormat: "png"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("publish cancellation %v", err)
	}
	if err := s.FinishMediaCleanup(ctx, MediaCleanup{ReservationID: r.ID, ArtifactID: r.ArtifactID, LeaseID: r.LeaseID, Code: "removed"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup cancellation %v", err)
	}
	if _, err := s.AuthorizeOriginal(ctx, ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation %v", err)
	}
	page, err := s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "synthetic", Limit: 10})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("background cancellation denied archive: %#v %v", page, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 100 {
		t.Fatal("cancelled operation lost reservation")
	}
}

func TestMediaSharedDecodedPinSurvivesSourceInvalidation(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	if _, err := s.EnsureIndex(ctx, IndexVersion{ID: "shared-pin-v1", EncoderProfile: "synthetic", TokenizerSHA256: strings.Repeat("c", 64), TokenizerImplementation: "test", PrefixVersion: "v1", ChunkingVersion: "v1", NormalizationVersion: LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 128}); err != nil {
		t.Fatal(err)
	}
	first, firstID := extensionTestMedia(t, s, p, "pin-one")
	second, secondID := extensionTestMedia(t, s, p, "pin-two")
	r := extensionAdmit(t, s, first, firstID, BudgetOriginals, 64)
	extensionPublish(t, s, r, 32)
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id=? WHERE id=?", r.ArtifactID, secondID); err != nil {
		t.Fatal(err)
	}
	extractionCompleteImage(t, s, firstID)
	extractionCompleteImage(t, s, secondID)
	if _, err := s.AdmitMedia(ctx, MediaAdmissionRequest{Source: second, MediaID: secondID, Budget: BudgetOriginals, MaximumBytes: 10, TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("shared retained association readmitted: %v", err)
	}
	decoded := extensionAdmit(t, s, first, firstID, BudgetDecoded, 100)
	apply(t, s, event("pin-one", "", 2, EventDelete), p.Generation)
	if ids, err := s.EvictionCandidates(ctx, 10); err != nil || len(ids) != 0 {
		t.Fatalf("unreaped shared pin ignored: %#v %v", ids, err)
	}
	if _, err := s.BeginEviction(ctx, secondID); !errors.Is(err, ErrNoWork) {
		t.Fatalf("unreaped shared pin evicted: %v", err)
	}
	if err := s.FinishMediaCleanup(ctx, MediaCleanup{ReservationID: decoded.ID, ArtifactID: decoded.ArtifactID, LeaseID: decoded.LeaseID, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.EvictionCandidates(ctx, 10); err != nil || len(ids) != 1 || ids[0] != secondID {
		t.Fatalf("cleaned pin did not release eviction: %#v %v", ids, err)
	}
}

func TestMediaRenewUsesDurableDeadlineAndOldDTORecoversLostReply(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 123456789, time.UTC)
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "renew")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	now = now.Add(30 * time.Second)
	deadline, err := s.RenewMedia(ctx, r, time.Minute)
	if err != nil || !deadline.Equal(fromMillis(unixms(now.Add(time.Minute)))) {
		t.Fatalf("renew deadline %v %v", deadline, err)
	}
	// Simulate a completed renewal whose reply the producer did not receive.
	if _, err = s.RenewMedia(ctx, r, time.Minute); err != nil {
		t.Fatalf("old DTO unable to recover renewal: %v", err)
	}
	forged := r
	forged.LeaseUntil = deadline.Add(time.Hour)
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: forged, SHA256: strings.Repeat("a", 64), Bytes: 20, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("caller deadline expanded authority: %v %v", ok, err)
	}
	now = r.LeaseUntil.Add(time.Second)
	extensionPublish(t, s, r, 20)
}

func TestMediaClockDeadlineRejectsLatePublicationWithoutReleasingQuota(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "deadline")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	now = r.LeaseUntil
	if _, err = s.RenewMedia(ctx, r, time.Minute); !errors.Is(err, ErrNoWork) {
		t.Fatalf("expired lease renewed: %v", err)
	}
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 20, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("expired lease published: %v %v", ok, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 100 {
		t.Fatal("lease expiration freed quota")
	}
	first, err := s.ReconcileMedia(ctx, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("reconcile %#v %v", first, err)
	}
	second, err := s.ReconcileMedia(ctx, 10)
	if err != nil || len(second) != 1 || second[0].CleanupLease != first[0].CleanupLease {
		t.Fatalf("cleanup identity changed: %#v %#v %v", first, second, err)
	}
}

func TestMediaPrepublicationPinsProtectBothAdmissionOrders(t *testing.T) {
	for _, decodedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "original_first", true: "decoded_first"}[decodedFirst], func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			ref, id := extensionTestMedia(t, s, p, "prepublication")
			var original, decoded MediaReservation
			if decodedFirst {
				decoded = extensionAdmit(t, s, ref, id, BudgetDecoded, 10)
			}
			original = extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
			if !decodedFirst {
				decoded = extensionAdmit(t, s, ref, id, BudgetDecoded, 10)
			}
			cleanup := MediaCleanup{ReservationID: original.ID, ArtifactID: original.ArtifactID, LeaseID: original.LeaseID, Code: "cancelled"}
			if err := s.FinishMediaCleanup(context.Background(), cleanup); !errors.Is(err, ErrNoWork) {
				t.Fatalf("original cleanup ignored live decoded owner: %v", err)
			}
			if err := s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: decoded.ID, ArtifactID: decoded.ArtifactID, LeaseID: decoded.LeaseID, Code: "removed"}); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishMediaCleanup(context.Background(), cleanup); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMediaClockSourceExpiryAndRollingCutoffDenyPublication(t *testing.T) {
	for _, rolling := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "rolling"}[rolling], func(t *testing.T) {
			ctx := context.Background()
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, err := Open(ctx, dir, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			policy := ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}
			if rolling {
				policy.RollingRetention = time.Minute
			}
			p, err := s.SetPolicy(ctx, policy, 0)
			if err != nil {
				t.Fatal(err)
			}
			e := event("clock-source", "synthetic clock image", 1, EventOriginal)
			e.BodyKind = MessageImage
			e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic authenticated reference"), Availability: "not_retained"}}
			e.OriginalSentAt = now.Add(-30 * time.Second)
			if !rolling {
				e.ExpiresAt = now.Add(30 * time.Second)
			}
			result := apply(t, s, e, p.Generation)
			ref, err := s.CurrentSource(ctx, e.Key)
			if err != nil {
				t.Fatal(err)
			}
			var id MediaID
			if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", result.MessageID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
			now = now.Add(31 * time.Second)
			if _, err = s.RenewMedia(ctx, r, time.Minute); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("clock-stale source renewed: %v", err)
			}
			if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 20, ActualFormat: "png"}); err != nil || ok {
				t.Fatalf("clock-stale source published: %v %v", ok, err)
			}
			if extensionMediaCharge(t, s, BudgetOriginals) != 100 {
				t.Fatal("clock source denial released bytes")
			}
		})
	}
}

func TestMediaCleanupAndReconcileRemainAvailableWhileSourceDenied(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "denied-recovery")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	s.safetyMu.Lock()
	s.unclean = true
	s.unsafe["account"] = true
	s.safetyMu.Unlock()
	items, err := s.ReconcileMedia(context.Background(), 10)
	if err != nil || len(items) != 1 || items[0].ID != r.ArtifactID {
		t.Fatalf("unsafe state blocked recovery: %#v %v", items, err)
	}
	if err = s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: items[0].ReservationID, ArtifactID: items[0].ID, LeaseID: items[0].CleanupLease, Code: "removed"}); err != nil {
		t.Fatalf("unsafe state blocked cleanup: %v", err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 0 {
		t.Fatal("denied recovery did not reclaim")
	}
	if _, err = s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "synthetic", Limit: 10}); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("cleanup cleared source denial: %v", err)
	}
}

func TestMediaFinalCommitExpiryReturnsSupersededAndKeepsReservation(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var advance atomic.Bool
	var calls atomic.Int64
	s, err := Open(ctx, dir, Options{Now: func() time.Time {
		if advance.Load() && calls.Add(1) >= 4 {
			return base.Add(2 * time.Second)
		}
		return base
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := testPolicy(t, s)
	e := event("final-commit-expiry", "synthetic final commit image", 1, EventOriginal)
	e.BodyKind = MessageImage
	e.ExpiresAt = base.Add(time.Second)
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic authenticated key"), Availability: "not_retained"}}
	result := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(ctx, e.Key)
	if err != nil {
		t.Fatal(err)
	}
	var id MediaID
	if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", result.MessageID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	advance.Store(true)
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 20, ActualFormat: "png"}); err != nil || ok {
		t.Fatalf("late commit expiry published or returned uncertain error: %v %v", ok, err)
	}
	var state string
	if err = s.readDB.QueryRow("SELECT state FROM extension_media_artifacts WHERE id=?", r.ArtifactID).Scan(&state); err != nil || state != "reserved" {
		t.Fatalf("late guard did not roll back publication: %s %v", state, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 100 {
		t.Fatal("late guard released reservation")
	}
}

type extensionMediaLostReplyCommand struct {
	publication OriginalPublication
	committed   chan error
	release     chan struct{}
}

func (c extensionMediaLostReplyCommand) account() string {
	return c.publication.Reservation.Source.Account
}
func (c extensionMediaLostReplyCommand) priority() bool { return false }
func (c extensionMediaLostReplyCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}
func (c extensionMediaLostReplyCommand) run(s *Store, ctx context.Context) (any, error) {
	value, err := (extensionMediaCommand{op: extensionMediaPublish, publication: c.publication}).run(s, ctx)
	c.committed <- err
	<-c.release
	return value, err
}

func TestMediaInspectorResolvesCommittedPublicationAfterResponseLoss(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "response-loss")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	committed := make(chan error, 1)
	release := make(chan struct{})
	result := make(chan error, 1)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	go func() {
		_, err := s.submitExtension(ctx, extensionMediaLostReplyCommand{publication: OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 20, ActualFormat: "png"}, committed: committed, release: release}, 1024)
		result <- err
	}()
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("publication response not lost: %v", err)
	}
	close(release)
	artifact, err := s.InspectMediaReservation(context.Background(), r)
	if err != nil || artifact.State != "retained" || artifact.ID != r.ArtifactID || artifact.ReservedBytes != 20 {
		t.Fatalf("committed original inspection %#v %v", artifact, err)
	}
	if err = s.FinishMediaCleanup(context.Background(), MediaCleanup{ReservationID: r.ID, ArtifactID: r.ArtifactID, LeaseID: r.LeaseID, Code: "removed"}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("lost reply led to retained cleanup: %v", err)
	}
	if _, err = s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "synthetic", Limit: 10}); err != nil {
		t.Fatalf("response loss denied keyword archive: %v", err)
	}
}

func TestMediaInspectorRetainsSharedFileAfterAdmittingSourceDeletion(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	first, firstID := extensionTestMedia(t, s, p, "inspect-shared-one")
	second, secondID := extensionTestMedia(t, s, p, "inspect-shared-two")
	r := extensionAdmit(t, s, first, firstID, BudgetOriginals, 100)
	extensionPublish(t, s, r, 20)
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id=? WHERE id=?", r.ArtifactID, secondID); err != nil {
		t.Fatal(err)
	}
	apply(t, s, event("inspect-shared-one", "", 2, EventDelete), p.Generation)
	s.safetyMu.Lock()
	s.unclean = true
	s.unsafe["account"] = true
	s.safetyMu.Unlock()
	artifact, err := s.InspectMediaReservation(context.Background(), r)
	if err != nil || artifact.State != "retained" || artifact.ReservedBytes != 20 {
		t.Fatalf("shared retained recovery %#v %v", artifact, err)
	}
	forged := r
	forged.Source = second
	forged.MediaID = secondID
	if _, err = s.InspectMediaReservation(context.Background(), forged); !errors.Is(err, ErrNoWork) {
		t.Fatalf("shared link substituted reservation capability: %v", err)
	}
	if _, err = s.AuthorizeOriginal(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, secondID); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("inspection granted current media access: %v", err)
	}
}

func TestMediaInspectorReturnsExpiredCleanupIdentityAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "expired-inspection")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 100)
	now = now.Add(time.Hour)
	a, err := s.InspectMediaReservation(ctx, r)
	if err != nil || a.State != "reserved" || a.CleanupLease != r.LeaseID || a.ReservedBytes != 100 {
		t.Fatalf("expired inspection %#v %v", a, err)
	}
	if err = s.FinishMediaCleanup(ctx, MediaCleanup{ReservationID: a.ReservationID, ArtifactID: a.ID, LeaseID: a.CleanupLease, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
	cleaned, err := s.InspectMediaReservation(ctx, r)
	if err != nil || cleaned.State != "evicting" || cleaned.ReservedBytes != 0 || cleaned.CleanupLease != a.CleanupLease {
		t.Fatalf("acked inspection %#v %v", cleaned, err)
	}
}

func TestMediaProcessedVictimIsNotHiddenBehindPendingCandidateLimit(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	if _, err := s.EnsureIndex(context.Background(), IndexVersion{ID: "victim-limit-v1", EncoderProfile: "synthetic", TokenizerSHA256: strings.Repeat("c", 64), TokenizerImplementation: "test", PrefixVersion: "v1", ChunkingVersion: "v1", NormalizationVersion: LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 128}); err != nil {
		t.Fatal(err)
	}
	var processed MediaID
	for i := 0; i < extensionMediaBatch+2; i++ {
		ref, id := extensionTestMedia(t, s, p, fmt.Sprintf("pending-candidate-%03d", i))
		extensionPublish(t, s, extensionAdmit(t, s, ref, id, BudgetOriginals, 64), 32)
		processed = id
	}
	extractionCompleteImage(t, s, processed)
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET updated_at=CASE WHEN media_id=? THEN 2 ELSE 1 END", processed); err != nil {
		t.Fatal(err)
	}
	ids, err := s.EvictionCandidates(context.Background(), 1)
	if err != nil || len(ids) != 1 || ids[0] != processed {
		t.Fatalf("processed victim hidden by older pending originals: %#v %v", ids, err)
	}
}

func TestMediaPublicationAndExtractionIgnoreManyEncoderGenerations(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	for i := 0; i < 65; i++ {
		if _, err := s.EnsureIndex(ctx, IndexVersion{ID: fmt.Sprintf("many-media-generations-%02d", i), EncoderProfile: "synthetic", TokenizerSHA256: strings.Repeat("c", 64), TokenizerImplementation: "test", PrefixVersion: "v1", ChunkingVersion: "v1", NormalizationVersion: LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 128}); err != nil {
			t.Fatal(err)
		}
	}
	ref, id := extensionTestMedia(t, s, p, "many-generation-media")
	extensionPublish(t, s, extensionAdmit(t, s, ref, id, BudgetOriginals, 64), 32)
	var jobs int
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_jobs WHERE media_id=? AND kind IN ('ocr','caption','transcription')", id).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("publication invented encoder-bound extraction: %d %v", jobs, err)
	}
	extractionCompleteImage(t, s, id)
	if _, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("encoder generations reran completed extraction: %v", err)
	}
	if _, err := s.BeginEviction(ctx, id); err != nil {
		t.Fatalf("semantic backlog pinned independently processed original: %v", err)
	}
}

func TestMediaFractionalRetentionMatchesExactSourceBoundary(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	now := base.Add(600 * time.Microsecond)
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll, RollingRetention: time.Minute + 800*time.Microsecond}, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := event("fractional-retention", "synthetic fractional image", 1, EventOriginal)
	e.OriginalSentAt = base.Add(-30 * time.Second)
	e.BodyKind = MessageImage
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic authenticated reference"), Availability: "not_retained"}}
	result := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(ctx, e.Key)
	if err != nil {
		t.Fatal(err)
	}
	var id MediaID
	if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", result.MessageID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	extensionPublish(t, s, extensionAdmit(t, s, ref, id, BudgetOriginals, 64), 32)
	g, err := s.EnsureIndex(ctx, IndexVersion{ID: "fractional-retention-v1", EncoderProfile: "synthetic", TokenizerSHA256: strings.Repeat("c", 64), TokenizerImplementation: "test", PrefixVersion: "v1", ChunkingVersion: "v1", NormalizationVersion: LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 128})
	if err != nil {
		t.Fatal(err)
	}
	extractionCompleteImage(t, s, id)
	// A direct fixture timestamp puts the source 200us inside the exact cutoff.
	// Private transactions keep real final-commit elapsed time from racing
	// against that deliberately tiny margin in this SQL precision regression.
	if _, err = s.writeDB.Exec("UPDATE messages SET original_sent_at=? WHERE id=?", unixms(base.Add(-time.Minute)), ref.Version.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.extensionCheckSourcesAt(ctx, s.readDB, []SourceRef{ref}, true, now); err != nil {
		t.Fatalf("exact source cutoff rejected valid boundary: %v", err)
	}
	if _, err = s.writeDB.Exec("DELETE FROM extension_jobs WHERE media_id=? AND kind='caption'", id); err != nil {
		t.Fatal(err)
	}
	if err = s.extensionMediaCoverageAt(ctx, s.readDB, g.ID, now); !errors.Is(err, ErrNoWork) {
		t.Fatalf("fractional SQL cutoff hid missing valid media work: %v", err)
	}
	if err = s.transaction(ctx, func(tx *sql.Tx) error { return s.extensionSeedMediaJobs(ctx, tx, g.ID) }); err != nil {
		t.Fatal(err)
	}
	if err = s.extensionMediaCoverageAt(ctx, s.readDB, g.ID, now); err != nil {
		t.Fatalf("fractional boundary failed to seed media: %v", err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_jobs SET state='complete' WHERE media_id=?", id); err != nil {
		t.Fatal(err)
	}
	var candidates []MediaID
	if err = s.transaction(ctx, func(tx *sql.Tx) error {
		var queryErr error
		candidates, queryErr = s.extensionEvictionCandidates(ctx, tx, 1)
		return queryErr
	}); err != nil || len(candidates) != 1 || candidates[0] != id {
		t.Fatalf("fractional cutoff hid valid processed candidate: %#v %v", candidates, err)
	}
	now = now.Add(201 * time.Microsecond)
	if _, err = s.extensionCheckSourcesAt(ctx, s.readDB, []SourceRef{ref}, true, now); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("source remained valid beyond exact boundary: %v", err)
	}
	if err = s.transaction(ctx, func(tx *sql.Tx) error {
		var queryErr error
		candidates, queryErr = s.extensionEvictionCandidates(ctx, tx, 1)
		return queryErr
	}); err != nil || len(candidates) != 0 {
		t.Fatalf("fractional cutoff offered expired candidate: %#v %v", candidates, err)
	}
}

func extensionBLOBPinFixture(t *testing.T, name string) (*Store, ArchivePolicy, MediaReservation, SourceRef, MediaID) {
	t.Helper()
	s := testArchive(t)
	p := testPolicy(t, s)
	_, owner, original := extractionTestOriginal(t, s, p, name+"-owner", JobOCR)
	extractionCompleteImage(t, s, owner)
	ref, holder := extensionTestMedia(t, s, p, name+"-holder")
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id=?,last_original_sha256=? WHERE id=?", original.ArtifactID, strings.Repeat("a", 64), holder); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.EvictionCandidates(context.Background(), 10); err != nil || len(ids) != 0 {
		t.Fatalf("unfinished shared holder unexpectedly evictable: %#v %v", ids, err)
	}
	return s, p, original, ref, holder
}

func extensionAssertBLOBPinBlocksCleanup(t *testing.T, s *Store, p ArchivePolicy, name string, original MediaReservation, decoded ArtifactID) MediaCleanup {
	t.Helper()
	ctx := context.Background()
	apply(t, s, event(name+"-holder", "", 2, EventDelete), p.Generation)
	var state string
	if err := s.readDB.QueryRow("SELECT state FROM extension_media_artifacts WHERE id=?", original.ArtifactID).Scan(&state); err != nil || state != "retained" {
		t.Fatalf("shared original fixture lost current owner: %q %v", state, err)
	}
	if ids, err := s.EvictionCandidates(ctx, 10); err != nil || len(ids) != 0 {
		t.Errorf("BLOB physical pin offered a processed shared original: %#v %v", ids, err)
	}
	if _, err := s.BeginEviction(ctx, original.MediaID); !errors.Is(err, ErrNoWork) {
		t.Errorf("BLOB physical pin allowed shared original eviction: %v", err)
	}
	apply(t, s, event(name+"-owner", "", 2, EventDelete), p.Generation)
	if err := s.readDB.QueryRow("SELECT state FROM extension_media_artifacts WHERE id=?", original.ArtifactID).Scan(&state); err != nil || state != "evicting" {
		t.Fatalf("owner invalidation did not enter cleanup state: %q %v", state, err)
	}
	// An existing filesystem owner already knows this valid cleanup identity.
	// Denial must come from the physical pin, rather than a missing lease.
	cleanup := MediaCleanup{ReservationID: original.ID, ArtifactID: original.ArtifactID, LeaseID: "blob-pin-cleanup-lease", Code: "removed"}
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET cleanup_lease=? WHERE id=?", cleanup.LeaseID, original.ArtifactID); err != nil {
		t.Fatal(err)
	}
	items, err := s.ReconcileMedia(ctx, 10)
	if err != nil {
		t.Errorf("physical pin reconciliation failed: %v", err)
	} else {
		for _, item := range items {
			if item.ID == original.ArtifactID {
				t.Error("reconciliation exposed the physically pinned original")
			}
		}
		if decoded != "" && (len(items) != 1 || items[0].ID != decoded) {
			t.Errorf("decoded owner's own cleanup record was not available: %#v", items)
		}
	}
	if _, err = s.InspectMediaReservation(ctx, original); !errors.Is(err, ErrNoWork) {
		t.Errorf("inspection exposed physically pinned original cleanup: %v", err)
	}
	if err = s.FinishMediaCleanup(ctx, cleanup); !errors.Is(err, ErrNoWork) {
		t.Errorf("cleanup acknowledged physically pinned original removal: %v", err)
	}
	var ack int
	var charge int64
	if err = s.readDB.QueryRow("SELECT cleanup_ack,charged_bytes FROM extension_media_artifacts WHERE id=?", original.ArtifactID).Scan(&ack, &charge); err != nil || ack != 0 || charge != 64 {
		t.Errorf("denied cleanup lost durable original accounting: ack=%d charge=%d %v", ack, charge, err)
	}
	budget, err := s.SetOriginalBudget(ctx, OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: 1})
	if err != nil || budget.ChargedBytes != 64 {
		t.Errorf("physically pinned original disappeared from budget: %#v %v", budget, err)
	}
	other, media := extensionTestMedia(t, s, p, name+"-competitor")
	if _, err = s.AdmitMedia(ctx, MediaAdmissionRequest{Source: other, MediaID: media, Budget: BudgetOriginals, MaximumBytes: MinimumOriginalBudgetBytes, TTL: time.Minute}); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("physically pinned bytes became available to a competing admission: %v", err)
	}
	return cleanup
}

func TestMediaBLOBExtractionPinsDenyCleanup(t *testing.T) {
	ctx := context.Background()
	const name = "blob-extraction-pin"
	s, p, original, _, holder := extensionBLOBPinFixture(t, name)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil || work.MediaID != holder || work.ArtifactID != original.ArtifactID {
		t.Fatalf("shared native input fixture: %#v %v", work, err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET artifact_id=CAST(? AS BLOB) WHERE id=?", original.ArtifactID, work.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadExtractionInput(ctx, work.Lease); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("malformed artifact reference granted native input: %v", err)
	}
	if err = s.RetryExtraction(ctx, JobRetry{Lease: work.Lease, Code: "cancelled", Terminal: true}); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("malformed artifact reference granted pin settlement: %v", err)
	}
	extensionAssertBLOBPinBlocksCleanup(t, s, p, name, original, "")
	var kind, state string
	var pinned, same int
	if err = s.readDB.QueryRow("SELECT typeof(artifact_id),artifact_id=CAST(? AS BLOB),state,input_pinned FROM extension_extraction_jobs WHERE id=?", original.ArtifactID, work.ID).Scan(&kind, &same, &state, &pinned); err != nil || kind != "blob" || same != 1 || state != "cancelled" || pinned != 1 {
		t.Fatalf("cleanup normalized or settled unresolved extraction ownership: type=%q same=%d state=%q pinned=%d %v", kind, same, state, pinned, err)
	}
}

func TestMediaBLOBDecodedPinsDenyCleanup(t *testing.T) {
	ctx := context.Background()
	const name = "blob-decoded-pin"
	s, p, original, ref, holder := extensionBLOBPinFixture(t, name)
	decoded := extensionAdmit(t, s, ref, holder, BudgetDecoded, 100)
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET input_artifact_id=CAST(? AS BLOB) WHERE id=?", original.ArtifactID, decoded.ArtifactID); err != nil {
		t.Fatal(err)
	}
	cleanup := extensionAssertBLOBPinBlocksCleanup(t, s, p, name, original, decoded.ArtifactID)
	var kind string
	var same, ack int
	if err := s.readDB.QueryRow("SELECT typeof(input_artifact_id),input_artifact_id=CAST(? AS BLOB),cleanup_ack FROM extension_media_artifacts WHERE id=?", original.ArtifactID, decoded.ArtifactID).Scan(&kind, &same, &ack); err != nil || kind != "blob" || same != 1 || ack != 0 || extensionMediaCharge(t, s, BudgetDecoded) != 100 {
		t.Fatalf("cleanup normalized or settled unresolved decoded ownership: type=%q same=%d ack=%d %v", kind, same, ack, err)
	}
	if err := s.FinishMediaCleanup(ctx, MediaCleanup{ReservationID: decoded.ID, ArtifactID: decoded.ArtifactID, LeaseID: decoded.LeaseID, Code: "removed"}); err != nil {
		t.Fatalf("exact decoded owner could not acknowledge its own stopped cleanup: %v", err)
	}
	if extensionMediaCharge(t, s, BudgetDecoded) != 0 {
		t.Fatal("acknowledged decoded cleanup retained quota")
	}
	items, err := s.ReconcileMedia(ctx, 10)
	if err != nil || len(items) != 1 || items[0].ID != original.ArtifactID {
		t.Fatalf("settled decoded owner did not release original reconciliation: %#v %v", items, err)
	}
	if artifact, err := s.InspectMediaReservation(ctx, original); err != nil || artifact.ID != original.ArtifactID || artifact.CleanupLease != cleanup.LeaseID {
		t.Fatalf("settled decoded owner did not release original inspection: %#v %v", artifact, err)
	}
	if err = s.FinishMediaCleanup(ctx, cleanup); err != nil || extensionMediaCharge(t, s, BudgetOriginals) != 0 {
		t.Fatalf("settled decoded owner did not release original cleanup: %v", err)
	}
}
