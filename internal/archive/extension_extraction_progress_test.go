//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func extractionProgressVoice(t *testing.T, s *Store, p ArchivePolicy, e SourceEvent) (SourceRef, MediaID, MediaReservation) {
	t.Helper()
	e.BodyKind = MessageVoice
	e.Media = []MediaReference{{Kind: "voice", ProtocolReference: []byte("synthetic authenticated voice"), MimeType: "audio/ogg", Availability: "not_retained"}}
	id := readerApply(t, s, p, e)
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	var media MediaID
	if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", id).Scan(&media); err != nil {
		t.Fatal(err)
	}
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatalf("voice publication: %v %v", ok, err)
	}
	return ref, media, r
}

func extractionProgressClaim(t *testing.T, s *Store, version ExtractionVersion, media MediaID) ExtractionWork {
	t.Helper()
	w, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: version, TTL: time.Minute})
	if err != nil || w.MediaID != media {
		t.Fatalf("claim: %v %v", w.MediaID == media, err)
	}
	return w
}

func extractionProgressAssert(t *testing.T, s *Store, scope ReadScope, version ExtractionVersion, pending, complete, failed int64) {
	t.Helper()
	got, err := s.ExtractionProgress(context.Background(), scope, version)
	want := Progress{State: "ready", Pending: pending, Complete: complete, Failed: failed}
	if pending > 0 {
		want.State = "pending"
	} else if failed > 0 {
		want.State = "failed"
	}
	if err != nil || got != want {
		t.Fatalf("progress: got %#v, want %#v, error %v", got, want, err)
	}
}

func TestExtractionProgressCountsConfiguredVersionAndUnclaimedOriginals(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	version := extractionTestVersion(JobTranscription)
	ref, media, original := extractionProgressVoice(t, s, p, readerEvent("complete", "voice", "", now.Add(-time.Hour)))
	scope := readerScope(p, readerChat(t, s, ref.Version.MessageID))
	extractionProgressAssert(t, s, scope, version, 1, 0, 0)
	w := extractionProgressClaim(t, s, version, media)
	extractionProgressAssert(t, s, scope, version, 1, 0, 0)
	doc := extractionTestDocument(w, "voice-progress-root", "שלום from a synthetic voice")
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: w.Lease, ContentHash: w.ContentHash, Outcome: ExtractionText, Documents: []Document{doc}}); err != nil || !ok {
		t.Fatalf("text commit: %v %v", ok, err)
	}
	extractionProgressAssert(t, s, scope, version, 0, 1, 0)
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET state='evicting',cleanup_ack=1 WHERE id=?", original.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id='',availability='not_retained' WHERE id=?", media); err != nil {
		t.Fatal(err)
	}
	// A successful transcript remains current when its cached original is evicted.
	extractionProgressAssert(t, s, scope, version, 0, 1, 0)
	_, silent, _ := extractionProgressVoice(t, s, p, readerEvent("silent", "voice", "", now.Add(-30*time.Minute)))
	w = extractionProgressClaim(t, s, version, silent)
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: w.Lease, ContentHash: w.ContentHash, Outcome: ExtractionSilent}); err != nil || !ok {
		t.Fatalf("silent commit: %v %v", ok, err)
	}
	_, failed, _ := extractionProgressVoice(t, s, p, readerEvent("failed", "voice", "", now.Add(-20*time.Minute)))
	w = extractionProgressClaim(t, s, version, failed)
	if err := s.RetryExtraction(context.Background(), JobRetry{Lease: w.Lease, Code: "unsupported", Terminal: true}); err != nil {
		t.Fatal(err)
	}
	extractionProgressAssert(t, s, scope, version, 0, 2, 1)
	_, pending, _ := extractionProgressVoice(t, s, p, readerEvent("pending", "voice", "", now.Add(-10*time.Minute)))
	w = extractionProgressClaim(t, s, version, pending)
	if err := s.RetryExtraction(context.Background(), JobRetry{Lease: w.Lease, Code: "model_unavailable"}); err != nil {
		t.Fatal(err)
	}
	extractionProgressVoice(t, s, p, readerEvent("unclaimed", "voice", "", now.Add(-time.Minute)))
	extractionProgressAssert(t, s, scope, version, 2, 2, 1)
	newVersion := version
	newVersion.ID = "second-configured-voice"
	newVersion.Language = "he"
	// An old configuration cannot count as successful work for a replacement.
	extractionProgressAssert(t, s, scope, newVersion, 4, 0, 0)
}

func TestExtractionProgressScopesAccountsChatsDatesAndCurrentSources(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	version := extractionTestVersion(JobTranscription)
	ref, _, _ := extractionProgressVoice(t, s, p, readerEvent("allowed", "allowed", "", now.Add(-time.Hour)))
	chat := readerChat(t, s, ref.Version.MessageID)
	extractionProgressVoice(t, s, p, readerEvent("other-chat", "other", "", now.Add(-time.Minute)))
	extractionProgressVoice(t, s, p, readerEvent("too-old", "allowed", "", now.Add(-4*time.Hour)))
	extractionProgressVoice(t, s, p, readerEvent("too-new", "allowed", "", now))
	scope := readerScope(p, chat)
	scope.From, scope.Until = now.Add(-2*time.Hour), now.Add(-time.Second)
	extractionProgressAssert(t, s, scope, version, 1, 0, 0)
	otherAccount := scope
	otherAccount.Account = "other-account"
	if progress, err := s.ExtractionProgress(context.Background(), otherAccount, version); !errors.Is(err, ErrUnauthorized) || progress != (Progress{}) {
		t.Fatalf("another account returned counts: %#v %v", progress, err)
	}
	credential := createCredential(t, s, "voice-progress", CredentialPolicy{Account: "account", Chats: []ChatID{chat}, From: scope.From, Until: scope.Until})
	grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	extractionProgressAssert(t, s, grant.Scope, version, 1, 0, 0)
	_, guard, err := s.StatusGuarded(context.Background(), grant.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeCredential(context.Background(), credential.ID, credential.Revision); err != nil {
		t.Fatal(err)
	}
	// Scope-only reads rely on the caller's existing final credential fence.
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked credential admitted response: %v", err)
	}
	mutation := readerEvent("allowed", "allowed", "", now.Add(-time.Hour))
	mutation.Kind, mutation.SourceRevision = EventDelete, 2
	readerApply(t, s, p, mutation)
	extractionProgressAssert(t, s, scope, version, 0, 0, 0)
	all := scope
	all.AllChats, all.Chats, all.From, all.Until = true, nil, time.Time{}, time.Time{}
	extractionProgressAssert(t, s, all, version, 3, 0, 0)
}

func TestExtractionProgressRejectsStalePinsAndInvalidVersion(t *testing.T) {
	mutations := map[string]string{
		"revision":  "UPDATE extension_media_associations SET local_revision=local_revision+1 WHERE id=?",
		"hash":      "UPDATE extension_media_associations SET content_hash='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE id=?",
		"expiry":    "UPDATE extension_media_associations SET source_expires_at=1 WHERE id=?",
		"policy":    "UPDATE extension_media_associations SET policy_generation=policy_generation+1 WHERE id=?",
		"inactive":  "UPDATE extension_media_associations SET active=0 WHERE id=?",
		"view-once": "UPDATE extension_media_associations SET view_once=1 WHERE id=?",
	}
	for name, statement := range mutations {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			ref, media, _ := extractionProgressVoice(t, s, p, readerEvent(name, "voice", "", now.Add(-time.Minute)))
			scope := readerScope(p, readerChat(t, s, ref.Version.MessageID))
			if _, err := s.writeDB.Exec(statement, media); err != nil {
				t.Fatal(err)
			}
			extractionProgressAssert(t, s, scope, extractionTestVersion(JobTranscription), 0, 0, 0)
		})
	}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	ref, media, _ := extractionProgressVoice(t, s, p, readerEvent("version", "voice", "", now.Add(-time.Minute)))
	scope := readerScope(p, readerChat(t, s, ref.Version.MessageID))
	version := extractionTestVersion(JobTranscription)
	extractionProgressClaim(t, s, version, media)
	rebound := version
	rebound.Language = "he"
	if _, err := s.ExtractionProgress(context.Background(), scope, rebound); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("rebound version accepted: %v", err)
	}
	invalid := version
	invalid.RuntimeSHA256 = "unpinned"
	if _, err := s.ExtractionProgress(context.Background(), scope, invalid); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("invalid runtime accepted: %v", err)
	}
	if _, err := s.writeDB.Exec("UPDATE extension_extraction_versions SET version_hash=? WHERE id=?", strings.Repeat("c", 64), version.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExtractionProgress(context.Background(), scope, version); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("invalid stored version hash accepted: %v", err)
	}
}

func TestExtractionProgressEnforcesSafetyPolicyAndClockFences(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	ref, _, _ := extractionProgressVoice(t, s, p, readerEvent("edge", "voice", "", now.Add(-time.Hour+time.Millisecond)))
	scope := readerScope(p, readerChat(t, s, ref.Version.MessageID))
	version := extractionTestVersion(JobTranscription)
	scope.Rolling = time.Hour
	calls := 0
	s.options.Now = func() time.Time {
		calls++
		if calls > 1 {
			return now.Add(2 * time.Millisecond)
		}
		return now
	}
	if progress, err := s.ExtractionProgress(context.Background(), scope, version); !errors.Is(err, ErrSourceUnavailable) || progress != (Progress{}) {
		t.Fatalf("clock-crossed counts returned: %#v %v", progress, err)
	}
	s.options.Now = func() time.Time { return now }
	scope.Rolling = 0
	p2, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExtractionProgress(context.Background(), scope, version); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("stale policy accepted: %v", err)
	}
	scope.PolicyGeneration = p2.Generation
	extractionProgressAssert(t, s, scope, version, 0, 0, 0)
	if err = s.MarkUnsafe(context.Background(), "account", "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	if progress, err := s.ExtractionProgress(context.Background(), scope, version); !errors.Is(err, ErrReconciliationRequired) || progress != (Progress{}) {
		t.Fatalf("unsafe archive returned counts: %#v %v", progress, err)
	}
}

func TestExtractionProgressDoesNotCountUnavailableOrMalformedCompletion(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("expires", "voice", "", now.Add(-time.Minute))
	e.ExpiresAt = now.Add(time.Minute)
	ref, media, _ := extractionProgressVoice(t, s, p, e)
	scope := readerScope(p, readerChat(t, s, ref.Version.MessageID))
	version := extractionTestVersion(JobTranscription)
	w := extractionProgressClaim(t, s, version, media)
	doc := extractionTestDocument(w, "progress-current-root", "synthetic current transcript")
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: w.Lease, ContentHash: w.ContentHash, Outcome: ExtractionText, Documents: []Document{doc}}); err != nil || !ok {
		t.Fatalf("text commit: %v %v", ok, err)
	}
	extractionProgressAssert(t, s, scope, version, 0, 1, 0)
	if _, err := s.writeDB.Exec("UPDATE extension_roots SET text_sha256=? WHERE id=?", strings.Repeat("z", 64), doc.ID); err != nil {
		t.Fatal(err)
	}
	extractionProgressAssert(t, s, scope, version, 0, 0, 0)
	if _, err := s.writeDB.Exec("UPDATE extension_roots SET text_sha256=? WHERE id=?", extensionExtractionSHA([]byte(doc.Text)), doc.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	extractionProgressAssert(t, s, scope, version, 0, 0, 0)
}

func TestExtractionProgressCredentialExpiryStillRequiresFinalFence(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	ref, _, _ := extractionProgressVoice(t, s, p, readerEvent("credential-expiry", "voice", "", now.Add(-time.Minute)))
	chat := readerChat(t, s, ref.Version.MessageID)
	credential, err := s.CreateCredential(context.Background(), CredentialCreate{
		Label: "Synthetic expiring progress client", Principal: "client:expiring-progress", Digest: [32]byte{1},
		Policy: CredentialPolicy{Account: "account", Chats: []ChatID{chat}}, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	extractionProgressAssert(t, s, grant.Scope, extractionTestVersion(JobTranscription), 1, 0, 0)
	_, guard, err := s.StatusGuarded(context.Background(), grant.Scope)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired credential admitted progress response: %v", err)
	}
}

func TestExtractionProgressStatusUsesOneSnapshotAndExpiryGuard(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("ordinary", "voice", "synthetic ordinary text", now.Add(-time.Hour)))
	scope := readerScope(p, readerChat(t, s, id))
	_, oldGuard, err := s.StatusGuarded(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	e := readerEvent("short-lived", "voice", "", now.Add(-time.Minute))
	e.ExpiresAt = now.Add(time.Millisecond)
	extractionProgressVoice(t, s, p, e)
	version := extractionTestVersion(JobTranscription)
	status, guard, err := s.StatusWithExtractionGuarded(context.Background(), scope, version)
	if err != nil || status.Messages != 2 || status.Media != (Progress{State: "pending", Pending: 1}) {
		t.Fatalf("configured scoped status: %#v %v", status, err)
	}
	owner, err := s.StatusWithExtraction(context.Background(), scope, version)
	if err != nil || owner.Snapshot != status.Snapshot || owner.Media != status.Media {
		t.Fatalf("owner configured status disagrees: %#v %v", owner, err)
	}
	now = now.Add(2 * time.Millisecond)
	// A separately sampled original status guard does not know this new
	// original's expiry. The combined read must capture and enforce it.
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: oldGuard}); err != nil {
		t.Fatalf("ordinary metadata insertion unexpectedly revoked old status: %v", err)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("expired configured counts passed final guard: %v", err)
	}
	status, _, err = s.StatusWithExtractionGuarded(context.Background(), scope, version)
	if err != nil || status.Messages != 1 || status.Media != (Progress{State: "ready"}) {
		t.Fatalf("fresh status retains expired counts: %#v %v", status, err)
	}
}
