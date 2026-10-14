package ingestion

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

// These fixtures model ordinary strict fresh history, not rebuild admission or
// partial unknown-history acceptance. Both participant fields are absent.
func groupRevokeReviewStub(chat, id string, omittedFromMe bool) *waWeb.WebMessageInfo {
	stub := bodylessHistoryStub(chat, id, waWeb.WebMessageInfo_REVOKE).Message
	stub.Key.Participant, stub.Participant = nil, nil
	if omittedFromMe {
		stub.Key.FromMe = nil
	}
	return stub
}

func groupRevokeReviewAssertGap(t *testing.T, store *archive.Store) {
	t.Helper()
	status := bodylessHistoryStatus(t, store)
	if status.ReconciliationRequired || status.Paused || !slices.Contains(status.Codes, "unresolved_deletion") {
		t.Fatalf("senderless revoke must remain a durable scoped gap, not a global fence: paused=%t unsafe=%t gap=%t", status.Paused, status.ReconciliationRequired, slices.Contains(status.Codes, "unresolved_deletion"))
	}
}

func TestHistoryBodylessGroupRevokeReviewUnknownAuthorIsScopedAndDurable(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		omittedFromMe, selective bool
		adminBaseline            bool
	}{
		{name: "explicit_false"},
		{name: "omitted_false", omittedFromMe: true},
		{name: "fresh_selective_zero", omittedFromMe: true, selective: true},
		// Qualify the later persistence/read assertions through the existing
		// admin fallback before the ordinary revoke fix is implemented.
		{name: "admin_baseline_explicit_false", adminBaseline: true},
		{name: "admin_baseline_omitted_false", omittedFromMe: true, adminBaseline: true},
		{name: "admin_baseline_selective_zero", omittedFromMe: true, selective: true, adminBaseline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const chat, target = "300@g.us", "review-senderless-revoke-target"
			const forbidden = "reviewgrouprevokeforbiddenreceipt"
			ctx := context.Background()
			store, dir := durableFixture(t)
			if tc.selective {
				p, err := store.Policy(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}, p.Generation); err != nil {
					t.Fatal(err)
				}
			}
			r := durableRuntime(t, store)
			if r.config.Rebuild != nil || r.config.UnknownHistoryPolicy != UnknownHistoryStrict {
				t.Fatal("fixture must exercise ordinary fresh strict history")
			}
			stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
			stub.Message = groupRevokeReviewStub(chat, target, tc.omittedFromMe)
			if tc.adminBaseline {
				stub.Message.MessageStubType = waWeb.WebMessageInfo_ADMIN_REVOKE.Enum()
			}
			targets, err := store.MutationTargets(ctx, durableAccount, chat, target)
			if err != nil || len(targets) != 0 {
				t.Fatal("fresh fixture unexpectedly retained a revoke target")
			}
			tail := bodylessHistoryRecord(chat, "review-senderless-tail", "reviewgrouprevoketailreceipt")
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail)); err != nil {
				// The bounded diagnostic is intentionally value-free. On the broken
				// branch it reproduces record_structure/revoke with scalar defaults.
				t.Fatalf("valid targetless group revoke stopped unrelated history: corrupt=%t diagnostic=%s", errors.Is(err, errHistoryCorrupt), historyFailureDiagnostic(err))
			}
			if r.Status().ReconciliationRequired {
				t.Fatal("valid senderless revoke globally fenced the fresh archive")
			}
			// Store status obeys policy scope: a zero-selection policy does not
			// expose even the affected group's gap until that chat is included.
			if !tc.selective {
				groupRevokeReviewAssertGap(t, store)
			}
			targets, err = store.MutationTargets(ctx, durableAccount, chat, target)
			if err != nil || len(targets) != 0 {
				t.Fatal("unresolved deletion fabricated an author-specific mutation target")
			}
			anchors, err := store.BackfillAnchors(ctx, durableAccount, 32)
			if err != nil {
				t.Fatal(err)
			}
			for _, anchor := range anchors {
				if anchor.Key.Message == target {
					t.Fatal("unresolved deletion fabricated an original date or history cursor")
				}
			}
			for _, sender := range []string{"200@s.whatsapp.net", "777@s.whatsapp.net", durableAccount} {
				key := bodylessHistoryKey(chat, target)
				key.Sender, key.FromMe = sender, sender == durableAccount
				metadata, err := store.SourceMetadata(ctx, key)
				if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown || !metadata.OriginalSentAt.IsZero() || metadata.ProtocolRevision != 0 {
					t.Fatal("senderless stub invented a source identity, date, or revision")
				}
			}
			if tc.selective {
				queueBurstAssertSearch(t, store, "reviewgrouprevoketailreceipt", 0)
				p, err := store.Policy(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, p.Generation); err != nil {
					t.Fatal(err)
				}
				if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, tail)); err != nil {
					t.Fatal(err)
				}
			}
			queueBurstAssertSearch(t, store, "reviewgrouprevoketailreceipt", 1)
			groupRevokeReviewAssertGap(t, store)
			// Every later valid original author/direction must be withheld. A
			// source-specific guessed tombstone would fail this matrix.
			for _, sender := range []string{"200@s.whatsapp.net", "777@s.whatsapp.net", durableAccount} {
				original := bodylessHistoryRecord(chat, target, forbidden)
				original.Message.Participant = proto.String(sender)
				if sender == durableAccount {
					original.Message.Key.FromMe = proto.Bool(true)
					original.Message.OriginalSelfAuthorUserJIDString = proto.String(sender)
				}
				if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
					t.Fatal("withheld future original stopped unrelated source processing")
				}
			}
			queueBurstAssertSearch(t, store, forbidden, 0)
			other := bodylessHistoryRecord("400@g.us", target, "reviewgrouprevokeotherchatreceipt")
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, "400@g.us", other)); err != nil {
				t.Fatal(err)
			}
			queueBurstAssertSearch(t, store, "reviewgrouprevokeotherchatreceipt", 1)
			durableAssertAbsent(t, dir, forbidden)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := durableStore(t, dir)
			replayed := durableRuntime(t, reopened)
			p, err := reopened.Policy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, sender := range []string{"200@s.whatsapp.net", "777@s.whatsapp.net", durableAccount} {
				original := bodylessHistoryRecord(chat, target, forbidden)
				original.Message.Participant = proto.String(sender)
				if sender == durableAccount {
					original.Message.Key.FromMe = proto.Bool(true)
					original.Message.OriginalSelfAuthorUserJIDString = proto.String(sender)
				}
				if err := replayed.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
					t.Fatal(err)
				}
				edit := archive.SourceEvent{Key: bodylessHistoryKey(chat, target), Kind: archive.EventEditContent, OriginalSentAt: time.Unix(1000, 0), OriginalDateKnown: true, SourceRevision: 9000, Body: forbidden, BodyKind: archive.MessageText}
				edit.Key.Sender, edit.Key.FromMe = sender, sender == durableAccount
				if _, err := reopened.ApplySourceEvent(ctx, edit, p.Generation); err != nil {
					t.Fatal(err)
				}
			}
			queueBurstAssertSearch(t, reopened, forbidden, 0)
			queueBurstAssertSearch(t, reopened, "reviewgrouprevoketailreceipt", 1)
			queueBurstAssertSearch(t, reopened, "reviewgrouprevokeotherchatreceipt", 1)
			groupRevokeReviewAssertGap(t, reopened)
			durableAssertAbsent(t, dir, forbidden)
		})
	}
}

func TestHistoryBodylessGroupRevokeReviewUniqueRetainedAuthor(t *testing.T) {
	const chat, target = "300@g.us", "review-unique-revoke-target"
	ctx := context.Background()
	store, _ := durableFixture(t)
	r := durableRuntime(t, store)
	original := bodylessHistoryRecord(chat, target, "reviewuniquerevokedreceipt")
	original.Message.Participant = proto.String("777@s.whatsapp.net")
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
		t.Fatal(err)
	}
	stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
	stub.Message = groupRevokeReviewStub(chat, target, true)
	tail := bodylessHistoryRecord(chat, "review-unique-tail", "reviewuniquetailreceipt")
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail)); err != nil {
		t.Fatal("missing participant must resolve the unique retained incoming author")
	}
	key := bodylessHistoryKey(chat, target)
	key.Sender = "777@s.whatsapp.net"
	metadata, err := store.SourceMetadata(ctx, key)
	if err != nil || !metadata.Version.Deleted || !metadata.OriginalDateKnown || !metadata.OriginalSentAt.Equal(time.Unix(1000, 0)) || metadata.ProtocolRevision != 0 {
		t.Fatal("unique-author resolution lost the terminal deletion or original date")
	}
	queueBurstAssertSearch(t, store, "reviewuniquerevokedreceipt", 0)
	queueBurstAssertSearch(t, store, "reviewuniquetailreceipt", 1)
	if r.Status().ReconciliationRequired || slices.Contains(bodylessHistoryStatus(t, store).Codes, "unresolved_deletion") {
		t.Fatal("resolved author must not become unresolved or globally unsafe")
	}
}

func TestHistoryBodylessGroupRevokeReviewInvalidAuthorityStaysStrict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*waWeb.WebMessageInfo)
	}{
		{"conflicting_participants", func(m *waWeb.WebMessageInfo) {
			m.Key.Participant = proto.String("200@s.whatsapp.net")
			m.Participant = proto.String("777@s.whatsapp.net")
		}},
		{"malformed_key_participant", func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("@s.whatsapp.net") }},
		{"malformed_web_participant", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("@s.whatsapp.net") }},
		{"nonuser_key_participant", func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("400@g.us") }},
		{"nonuser_web_participant", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("400@g.us") }},
		{"incoming_self_author_claim", func(m *waWeb.WebMessageInfo) { m.OriginalSelfAuthorUserJIDString = proto.String(durableAccount) }},
		{"wrong_chat", func(m *waWeb.WebMessageInfo) { m.Key.RemoteJID = proto.String("400@g.us") }},
		{"missing_id", func(m *waWeb.WebMessageInfo) { m.Key.ID = nil }},
		{"explicit_zero_expiry", func(m *waWeb.WebMessageInfo) { m.EphemeralDuration = proto.Uint32(0) }},
		{"unknown_stub", func(m *waWeb.WebMessageInfo) { m.MessageStubType = waWeb.WebMessageInfo_StubType(987654).Enum() }},
		{"senderless_ciphertext", func(m *waWeb.WebMessageInfo) { m.MessageStubType = waWeb.WebMessageInfo_CIPHERTEXT.Enum() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const chat, target = "300@g.us", "review-invalid-revoke-target"
			ctx := context.Background()
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
			stub.Message = groupRevokeReviewStub(chat, target, true)
			tc.mutate(stub.Message)
			tail := bodylessHistoryRecord(chat, "review-invalid-tail", "reviewinvalidtailreceipt")
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("invalid explicit authority must not use the senderless fallback")
			}
			metadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, "review-invalid-tail"))
			if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown {
				t.Fatal("invalid revoke admitted unrelated tail bodies or anchors")
			}
			if _, err := durableSearch(t, store, "reviewinvalidtailreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal("invalid revoke failed to fence archive reads")
			}
		})
	}
}

func TestHistoryBodylessGroupRevokeReviewUnresolvedWriteFailureStaysStrict(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, archive.ErrArchiveUnavailable} {
		t.Run(failure.Error(), func(t *testing.T) {
			const chat = "300@g.us"
			ctx := context.Background()
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			r.store = &groupRevokeReviewUnknownFailureStore{ConnectorStore: store, failure: failure}
			stub := bodylessHistoryStub(chat, "review-write-failure-target", waWeb.WebMessageInfo_REVOKE)
			stub.Message = groupRevokeReviewStub(chat, "review-write-failure-target", true)
			tail := bodylessHistoryRecord(chat, "review-write-failure-tail", "reviewwritefailuretailreceipt")
			err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail))
			if !errors.Is(err, failure) || !r.Status().ReconciliationRequired {
				t.Fatalf("fallback must attempt durable guard and retain its failure: preserved=%t unsafe=%t diagnostic=%s", errors.Is(err, failure), r.Status().ReconciliationRequired, historyFailureDiagnostic(err))
			}
			metadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, "review-write-failure-tail"))
			if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown {
				t.Fatal("failed guard persistence admitted the history tail")
			}
			if _, err := durableSearch(t, store, "reviewwritefailuretailreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal("failed guard persistence left protected reads available")
			}
		})
	}
}

type groupRevokeReviewUnknownFailureStore struct {
	archive.ConnectorStore
	failure error
}

func (s *groupRevokeReviewUnknownFailureStore) RecordUnresolvedDeletionIfUnknown(context.Context, string, string, string) error {
	return s.failure
}

func TestHistoryBodylessGroupRevokeReviewRetainedAuthorityStaysStrict(t *testing.T) {
	for _, name := range []string{"explicit_author_mismatch", "incoming_direction_mismatch", "ambiguous_targets"} {
		t.Run(name, func(t *testing.T) {
			const chat, target = "300@g.us", "review-retained-authority-target"
			ctx := context.Background()
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			original := bodylessHistoryRecord(chat, target, "reviewretainedauthorityreceipt")
			key := bodylessHistoryKey(chat, target)
			if name == "incoming_direction_mismatch" {
				original.Message.Key.FromMe = proto.Bool(true)
				original.Message.Participant = proto.String(durableAccount)
				original.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
				key.Sender, key.FromMe = durableAccount, true
			}
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
				t.Fatal(err)
			}
			stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
			stub.Message = groupRevokeReviewStub(chat, target, true)
			if name == "explicit_author_mismatch" {
				stub.Message.Participant = proto.String("777@s.whatsapp.net")
			}
			if name == "ambiguous_targets" {
				other := bodylessHistoryRecord(chat, target, "reviewretainedotherreceipt")
				other.Message.Participant = proto.String("777@s.whatsapp.net")
				if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, other)); err != nil {
					t.Fatal(err)
				}
			}
			tail := bodylessHistoryRecord(chat, "review-retained-authority-tail", "reviewretainedtailreceipt")
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("mismatched or ambiguous retained authority must not use unresolved fallback")
			}
			metadata, err := store.SourceMetadata(ctx, key)
			if err != nil || metadata.Version.Deleted {
				t.Fatal("invalid retained-authority claim applied a deletion")
			}
			metadata, err = store.SourceMetadata(ctx, bodylessHistoryKey(chat, "review-retained-authority-tail"))
			if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown {
				t.Fatal("invalid retained-authority claim admitted the history tail")
			}
			if _, err := durableSearch(t, store, "reviewretainedauthorityreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal("invalid retained-authority claim left protected reads available")
			}
		})
	}
}
