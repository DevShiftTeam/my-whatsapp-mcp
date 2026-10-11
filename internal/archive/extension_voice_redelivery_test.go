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

type ownerVoiceRedeliveryPort interface {
	VoiceRedeliverySources(context.Context, ReadScope, int) ([]VoiceRedeliverySource, error)
	RetryRedeliveredVoiceDownloads(context.Context, ReadScope, int) (VoiceDownloadRetryResult, error)
}

func TestVoiceRedeliveryNeverRequestsOrQueuesUnavailableOrCompletedSources(t *testing.T) {
	for _, scenario := range []string{"deleted", "expired", "view_once", "message_view_once", "conflicted", "excluded", "image", "revision", "hash", "policy", "completed", "association_completed", "physical", "lease", "other_cancel_reason", "selected_chat", "date_scope"} {
		t.Run(scenario, func(t *testing.T) {
			s, clock, _ := originalDownloadTestArchive(t)
			p := testPolicy(t, s)
			kind := MessageVoice
			if scenario == "image" {
				kind = MessageImage
			}
			_, ref, id := originalDownloadTestSource(t, s, p, "guard-voice", kind)
			if _, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET state='cancelled',lease_id='',lease_until=0,last_code='media_source_changed' WHERE media_id=?`, id); err != nil {
				t.Fatal(err)
			}
			scope := voiceRetryScope(p)
			var query string
			var args []any
			switch scenario {
			case "deleted", "conflicted", "excluded":
				query, args = `UPDATE messages SET state=? WHERE id=?`, []any{scenario, ref.Version.MessageID}
			case "expired":
				query, args = `UPDATE messages SET expires_at=? WHERE id=?`, []any{clock.Load() - 1, ref.Version.MessageID}
			case "view_once":
				query, args = `UPDATE extension_media_associations SET view_once=1 WHERE id=?`, []any{id}
			case "message_view_once":
				query, args = `UPDATE messages SET view_once=1 WHERE id=?`, []any{ref.Version.MessageID}
			case "revision":
				query, args = `UPDATE extension_original_downloads SET local_revision=local_revision+1 WHERE media_id=?`, []any{id}
			case "hash":
				query, args = `UPDATE messages SET content_hash=? WHERE id=?`, []any{strings.Repeat("b", 64), ref.Version.MessageID}
			case "policy":
				query, args = `UPDATE extension_original_downloads SET policy_generation=policy_generation+1 WHERE media_id=?`, []any{id}
			case "completed":
				query, args = `UPDATE extension_original_downloads SET completed_sha256=? WHERE media_id=?`, []any{strings.Repeat("a", 64), id}
			case "association_completed":
				query, args = `UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?`, []any{strings.Repeat("a", 64), id}
			case "physical":
				extensionAdmit(t, s, ref, id, BudgetOriginals, 128)
			case "lease":
				query, args = `UPDATE extension_original_downloads SET lease_id='busy',lease_until=? WHERE media_id=?`, []any{clock.Load() + 60000, id}
			case "other_cancel_reason":
				query, args = `UPDATE extension_original_downloads SET last_code='media_invalid_admission' WHERE media_id=?`, []any{id}
			case "selected_chat":
				scope.AllChats = false
				scope.Chats = []ChatID{"not-the-selected-chat"}
			case "date_scope":
				scope.From = fromMillis(clock.Load())
			}
			if query != "" {
				if _, err := s.writeDB.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			port := voiceRedeliveryPort(t, s)
			queued, err := port.RetryRedeliveredVoiceDownloads(context.Background(), scope, 32)
			if err != nil || queued.Queued != 0 {
				t.Fatalf("unsafe source was queued: %#v %v", queued, err)
			}
			if _, err = s.writeDB.Exec(`UPDATE extension_media_associations SET active=0,protocol_reference=X'' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			missing, err := port.VoiceRedeliverySources(context.Background(), scope, 8)
			if err != nil || len(missing) != 0 {
				t.Fatalf("unsafe source was requested: %v %v", len(missing), err)
			}
		})
	}
}

func TestVoiceRedeliverySelectsNewestEligibleBeforeBoundAndKeepsArchiveRecords(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	var want SourceKey
	for i := 0; i < 12; i++ {
		e, _, id := originalDownloadTestSource(t, s, p, fmt.Sprintf("voice-%02d", i), MessageVoice)
		if _, err := s.writeDB.Exec(`UPDATE messages SET original_sent_at=original_sent_at+? WHERE id=(SELECT message_id FROM extension_original_downloads WHERE media_id=?)`, i*1000, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET state='cancelled',lease_id='',lease_until=0,last_code='media_source_changed' WHERE media_id=?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET active=0,protocol_reference=X'' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if i == 11 {
			want = e.Key
		}
	}
	var before int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	missing, err := voiceRedeliveryPort(t, s).VoiceRedeliverySources(context.Background(), voiceRetryScope(p), 8)
	if err != nil || len(missing) != 8 || missing[0].Key != want {
		t.Fatalf("newest bounded voice selection failed: %v %v", len(missing), err)
	}
	var after int
	if err = s.readDB.QueryRow(`SELECT count(*) FROM messages`).Scan(&after); err != nil || after != before {
		t.Fatal("request selection replaced archive records")
	}
}

func voiceRedeliveryPort(t *testing.T, s *Store) ownerVoiceRedeliveryPort {
	t.Helper()
	p, ok := any(s).(ownerVoiceRedeliveryPort)
	if !ok {
		t.Fatal("archive lacks lossless owner voice redelivery")
	}
	return p
}

func TestVoiceRedeliveryRequestsMissingReferenceThenQueuesOnlyAuthenticatedReplay(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	e, ref, id := originalDownloadTestSource(t, s, p, "redelivery-voice", MessageVoice)
	if _, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET state='cancelled',lease_id='',lease_until=0,last_code='media_source_changed' WHERE media_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET active=0,protocol_reference=X'' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	port := voiceRedeliveryPort(t, s)
	missing, err := port.VoiceRedeliverySources(context.Background(), voiceRetryScope(p), 8)
	if err != nil || len(missing) != 1 || missing[0].Key != e.Key || missing[0].Source != ref {
		t.Fatalf("current missing voice reference was not recoverable: %v %v", len(missing), err)
	}
	queued, err := port.RetryRedeliveredVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if err != nil || queued.Queued != 0 {
		t.Fatal("missing protocol keys invented queue authority")
	}
	before, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := apply(t, s, e, p.Generation)
	if duplicate.Outcome != OutcomeDuplicate {
		t.Fatal("authenticated source replay changed source outcome")
	}
	queued, err = port.RetryRedeliveredVoiceDownloads(context.Background(), voiceRetryScope(p), 32)
	if err != nil || queued.Queued != 1 {
		t.Fatalf("fresh replay remained permanently cancelled: %#v %v", queued, err)
	}
	work := originalDownloadTestClaim(t, s)
	if work.Source != ref || work.Lease.MediaID != id || work.Attempts != 1 {
		t.Fatal("owner retry replaced source authority")
	}
	after, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil || after != before {
		t.Fatal("redelivery mutated archived content revision")
	}
	missing, err = port.VoiceRedeliverySources(context.Background(), voiceRetryScope(p), 8)
	if err != nil || len(missing) != 0 {
		t.Fatal("fresh pending reference was requested again")
	}
}

func TestVoiceRedeliveryRequiresOwnerAndCurrentSafety(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	p := testPolicy(t, s)
	_, _, id := originalDownloadTestSource(t, s, p, "guard-redelivery", MessageVoice)
	if _, err := s.writeDB.Exec(`UPDATE extension_original_downloads SET state='cancelled',lease_id='',lease_until=0,last_code='media_source_changed' WHERE media_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET active=0,protocol_reference=X'' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	port := voiceRedeliveryPort(t, s)
	for _, modify := range []func(*ReadScope){
		func(q *ReadScope) { q.Principal = "mcp-client" },
		func(q *ReadScope) { q.Account = "other-account" },
		func(q *ReadScope) { q.PolicyGeneration++ },
		func(q *ReadScope) { q.AllChats = false },
		func(q *ReadScope) { q.From, q.Until = time.Now(), time.Now().Add(-time.Hour) },
	} {
		scope := voiceRetryScope(p)
		modify(&scope)
		if values, err := port.VoiceRedeliverySources(context.Background(), scope, 8); err == nil || len(values) != 0 {
			t.Fatal("invalid source request scope admitted")
		}
		if queued, err := port.RetryRedeliveredVoiceDownloads(context.Background(), scope, 32); err == nil || queued.Queued != 0 {
			t.Fatal("invalid retry scope admitted")
		}
	}
	if _, err := port.VoiceRedeliverySources(context.Background(), voiceRetryScope(p), 9); !errors.Is(err, ErrInvalidExtension) {
		t.Fatal("unbounded phone redelivery accepted")
	}
	if err := s.MarkUnsafe(context.Background(), "account", "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := port.VoiceRedeliverySources(context.Background(), voiceRetryScope(p), 8); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatal("redelivery bypassed source safety")
	}
}
