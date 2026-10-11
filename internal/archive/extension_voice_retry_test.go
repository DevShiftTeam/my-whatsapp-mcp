//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type failedVoiceRetryPort interface {
	RetryFailedVoiceDownloads(context.Context, ReadScope, int) (VoiceDownloadRetryResult, error)
}

func voiceRetryPort(t *testing.T, s *Store) failedVoiceRetryPort {
	t.Helper()
	port, ok := any(s).(failedVoiceRetryPort)
	if !ok {
		t.Fatal("archive does not implement explicit failed voice download retry")
	}
	return port
}

func voiceRetryFail(t *testing.T, s *Store, id MediaID) {
	t.Helper()
	if _, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET state='failed',attempts=8,next_due=123,lease_id='',lease_until=0,last_code='media_unavailable' WHERE media_id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func voiceRetryScope(p ArchivePolicy) ReadScope {
	return ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}
}

func TestVoiceRetryExplicitlyRestartsFailedAllowanceAndKeepsDuplicatesPending(t *testing.T) {
	s, clock, dir := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	e, ref, media := originalDownloadTestSource(t, s, p, "failed-voice", MessageVoice)
	for i := 1; i <= 8; i++ {
		work := originalDownloadTestClaim(t, s)
		if work.Attempts != i {
			t.Fatal("download failure allowance changed")
		}
		if err := s.RetryOriginalDownload(context.Background(), OriginalDownloadRetry{Lease: work.Lease, Code: "media_unavailable", After: time.Second}); err != nil {
			t.Fatal(err)
		}
		clock.Add(int64(2 * time.Second / time.Millisecond))
	}
	originalDownloadTestNoWork(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return fromMillis(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	originalDownloadTestNoWork(t, s)
	result, err := voiceRetryPort(t, s).RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if err != nil || result != (VoiceDownloadRetryResult{Queued: 1}) {
		t.Fatalf("explicit retry did not queue failed voice: %#v %v", result, err)
	}
	duplicate := apply(t, s, e, p.Generation)
	if duplicate.Outcome != OutcomeDuplicate {
		t.Fatal("synthetic redelivery did not stay duplicate")
	}
	if result, err = voiceRetryPort(t, s).RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32); err != nil || result.Queued != 0 {
		t.Fatal("already pending voice queued twice")
	}
	work := originalDownloadTestClaim(t, s)
	if work.Attempts != 1 || work.Source != ref || work.Lease.MediaID != media {
		t.Fatal("retry replaced source authority or retained exhausted allowance")
	}
	reservation := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if accepted, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !accepted {
		t.Fatal("retry could not publish verified voice format")
	}
	if accepted, err := s.FinishOriginalDownload(context.Background(), work.Lease); err != nil || !accepted {
		t.Fatal("retry could not complete normal original publication")
	}
	result, err = voiceRetryPort(t, s).RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if err != nil || result.Queued != 0 {
		t.Fatal("retained original was redownloaded")
	}
	originalDownloadTestNoWork(t, s)
}

func TestVoiceRetryRequiresCurrentOwnerScopeAndBoundedLimit(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	_, _, id := originalDownloadTestSource(t, s, p, "scope-voice", MessageVoice)
	voiceRetryFail(t, s, id)
	port := voiceRetryPort(t, s)
	for _, change := range []func(*ReadScope){
		func(s *ReadScope) { s.Principal = "mcp-client" },
		func(s *ReadScope) { s.Account = "different-account" },
		func(s *ReadScope) { s.PolicyGeneration++ },
		func(s *ReadScope) { s.AllChats = false },
		func(s *ReadScope) {
			s.Until = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			s.From = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		},
	} {
		scope := voiceRetryScope(p)
		change(&scope)
		if result, err := port.RetryFailedVoiceDownloads(context.Background(), scope, 32); err == nil || result.Queued != 0 {
			t.Fatal("invalid owner scope granted retry")
		}
	}
	for _, limit := range []int{-1, 0, 33} {
		if _, err := port.RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), limit); !errors.Is(err, ErrInvalidExtension) {
			t.Fatal("retry accepted unbounded or ambiguous batch")
		}
	}
	originalDownloadTestNoWork(t, s)
}

func TestVoiceRetryRejectsUnavailableSourceAndPrivateReferenceChanges(t *testing.T) {
	for _, scenario := range []string{"deleted", "expired", "view_once", "message_view_once", "inactive", "stale_revision", "stale_policy", "chat_policy", "date_policy", "rolling_policy", "changed_content_hash", "changed_descriptor", "missing_reference", "physical_original", "completed_marker", "association_marker", "image"} {
		t.Run(scenario, func(t *testing.T) {
			s, clock, _ := originalDownloadTestArchive(t)
			p := testPolicy(t, s)
			kind := MessageVoice
			if scenario == "image" {
				kind = MessageImage
			}
			_, ref, id := originalDownloadTestSource(t, s, p, "guard-voice", kind)
			voiceRetryFail(t, s, id)
			var query string
			var args []any
			switch scenario {
			case "deleted":
				query, args = `UPDATE messages SET state='deleted' WHERE id=?`, []any{ref.Version.MessageID}
			case "expired":
				query, args = `UPDATE messages SET expires_at=? WHERE id=?`, []any{clock.Load() - 1, ref.Version.MessageID}
			case "view_once":
				query, args = `UPDATE extension_media_associations SET view_once=1 WHERE id=?`, []any{id}
			case "message_view_once":
				query, args = `UPDATE messages SET view_once=1 WHERE id=?`, []any{ref.Version.MessageID}
			case "inactive":
				query, args = `UPDATE extension_media_associations SET active=0 WHERE id=?`, []any{id}
			case "stale_revision":
				query, args = `UPDATE extension_original_downloads SET local_revision=local_revision+1 WHERE media_id=?`, []any{id}
			case "stale_policy":
				query, args = `UPDATE extension_original_downloads SET policy_generation=policy_generation+1 WHERE media_id=?`, []any{id}
			case "chat_policy":
				query = `UPDATE archive_policy SET chat_mode='selective' WHERE id=1`
			case "date_policy":
				query, args = `UPDATE archive_policy SET date_mode='since',import_start=? WHERE id=1`, []any{clock.Load()}
			case "rolling_policy":
				query, args = `UPDATE archive_policy SET rolling_retention=? WHERE id=1`, []any{int64(time.Hour)}
			case "changed_content_hash":
				query, args = `UPDATE messages SET content_hash=? WHERE id=?`, []any{strings.Repeat("b", 64), ref.Version.MessageID}
			case "changed_descriptor":
				query, args = `UPDATE extension_media_associations SET protocol_reference=? WHERE id=?`, []any{originalDownloadTestReference(t, MessageVoice, "changed-reference"), id}
			case "missing_reference":
				query, args = `UPDATE extension_media_associations SET protocol_reference=X'' WHERE id=?`, []any{id}
			case "physical_original":
				extensionAdmit(t, s, ref, id, BudgetOriginals, 128)
			case "completed_marker":
				query, args = `UPDATE extension_original_downloads SET completed_sha256=? WHERE media_id=?`, []any{strings.Repeat("a", 64), id}
			case "association_marker":
				query, args = `UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?`, []any{strings.Repeat("a", 64), id}
			}
			if query != "" {
				if _, err := s.writeDB.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			result, err := voiceRetryPort(t, s).RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
			if err != nil || result.Queued != 0 {
				t.Fatalf("unsafe retry queued for %s: %#v %v", scenario, result, err)
			}
			var state string
			wantState := "failed"
			if scenario == "changed_descriptor" {
				wantState = "cancelled"
			}
			if err := s.readDB.QueryRow(`SELECT state FROM extension_original_downloads WHERE media_id=?`, id).Scan(&state); err != nil || state != wantState {
				t.Fatal("ineligible failed scheduling metadata changed")
			}
		})
	}
}

func TestVoiceRetryCannotOverrideUnsafeArchive(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	_, _, id := originalDownloadTestSource(t, s, p, "unsafe-voice", MessageVoice)
	voiceRetryFail(t, s, id)
	if err := s.MarkUnsafe(context.Background(), "account", "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	result, err := voiceRetryPort(t, s).RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if !errors.Is(err, ErrReconciliationRequired) || result.Queued != 0 {
		t.Fatal("owner voice retry bypassed archive safety")
	}
	var state string
	if err := s.readDB.QueryRow(`SELECT state FROM extension_original_downloads WHERE media_id=?`, id).Scan(&state); err != nil || state != "failed" {
		t.Fatal("unsafe archive reset failed work")
	}
}

func TestVoiceRetrySelectedChatScopePrecedesLimit(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	for i := 0; i < 40; i++ {
		_, _, id := originalDownloadTestSource(t, s, p, fmt.Sprintf("outside-%02d", i), MessageVoice)
		voiceRetryFail(t, s, id)
	}
	e := event("selected", "", 1, EventOriginal)
	e.Key.Chat = "selected-chat"
	e.BodyKind = MessageVoice
	e.Media = []MediaReference{{Kind: "voice", MimeType: "audio/ogg", ProtocolReference: originalDownloadTestReference(t, MessageVoice, "selected"), DeclaredBytes: 64, Availability: "not_retained"}}
	created := apply(t, s, e, p.Generation)
	voiceRetryFail(t, s, created.MediaIDs[0])
	scope := voiceRetryScope(p)
	scope.AllChats, scope.Chats = false, []ChatID{readerChat(t, s, created.MessageID)}
	result, err := voiceRetryPort(t, s).RetryFailedVoiceDownloads(context.Background(), scope, 1)
	if err != nil || result != (VoiceDownloadRetryResult{Queued: 1}) {
		t.Fatal("denied chats consumed selected retry limit")
	}
	work := originalDownloadTestClaim(t, s)
	if work.Lease.MediaID != created.MediaIDs[0] {
		t.Fatal("retry queued voice outside selected owner scope")
	}
}

func TestVoiceRetryMetadataExclusionsPrecedeBatchAndInvalidDescriptorsProgress(t *testing.T) {
	for _, scenario := range []string{"completed", "malformed_reference"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _ := originalDownloadTestArchive(t)
			p := testPolicy(t, s)
			var valid MediaID
			ids := make([]MediaID, 33)
			for i := range ids {
				_, _, ids[i] = originalDownloadTestSource(t, s, p, fmt.Sprintf("skip-%02d", i), MessageVoice)
				voiceRetryFail(t, s, ids[i])
			}
			// IDs are opaque and random. Select the final ordered ID so the
			// synthetic ineligible prefix is deterministic.
			if err := s.readDB.QueryRow(`SELECT media_id FROM extension_original_downloads ORDER BY media_id DESC LIMIT 1`).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				if id == valid {
					continue
				}
				var err error
				if scenario == "completed" {
					_, err = s.writeDB.Exec(`UPDATE extension_original_downloads SET completed_sha256=? WHERE media_id=?`, strings.Repeat("a", 64), id)
				} else {
					_, err = s.writeDB.Exec(`UPDATE extension_media_associations SET protocol_reference=X'01' WHERE id=?`, id)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			port := voiceRetryPort(t, s)
			result, err := port.RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "completed" {
				if result != (VoiceDownloadRetryResult{Queued: 1}) {
					t.Fatal("completed failed metadata starved current downloadable voice")
				}
			} else {
				if result != (VoiceDownloadRetryResult{Skipped: 32}) {
					t.Fatal("malformed descriptor batch did not remain bounded")
				}
				result, err = port.RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
				if err != nil || result != (VoiceDownloadRetryResult{Queued: 1}) {
					t.Fatal("explicit retry stayed stuck on malformed failed prefix")
				}
			}
			work := originalDownloadTestClaim(t, s)
			if work.Lease.MediaID != valid || work.Attempts != 1 {
				t.Fatal("wrong voice queued after guarded retry")
			}
		})
	}
}

func TestVoiceRetryLimitsBatchesAfterScopeAuthorization(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	for i := 0; i < 40; i++ {
		_, _, id := originalDownloadTestSource(t, s, p, fmt.Sprintf("batch-%02d", i), MessageVoice)
		voiceRetryFail(t, s, id)
	}
	port := voiceRetryPort(t, s)
	first, err := port.RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if err != nil || first != (VoiceDownloadRetryResult{Queued: 32}) {
		t.Fatalf("wrong first bounded batch: %#v %v", first, err)
	}
	second, err := port.RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if err != nil || second != (VoiceDownloadRetryResult{Queued: 8}) {
		t.Fatalf("remaining batch lost or pending downloads duplicated: %#v %v", second, err)
	}
	third, err := port.RetryFailedVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if err != nil || third != (VoiceDownloadRetryResult{}) {
		t.Fatal("retry did not stop after exhausting failed work")
	}
}
