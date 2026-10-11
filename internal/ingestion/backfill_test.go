package ingestion

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

type backfillAnchorStore struct {
	archive.ConnectorStore
	mu        sync.Mutex
	anchors   []archive.SourceAnchor
	queries   int
	selection archive.BackfillSelection
	malformed bool
}

// The fake implements only the approved selector port. Account/policy scope
// precedes ranking and LIMIT, just as the production archive must enforce.
func (s *backfillAnchorStore) SelectedBackfillAnchors(ctx context.Context, q archive.BackfillSelection) ([]archive.SourceAnchor, error) {
	s.mu.Lock()
	s.queries++
	s.selection = q
	anchors := append([]archive.SourceAnchor(nil), s.anchors...)
	malformed := s.malformed
	s.mu.Unlock()
	if malformed {
		return anchors, nil
	}
	policy, err := s.SourcePolicy(ctx, q.Account)
	if err != nil {
		return nil, err
	}
	if policy.Generation != q.PolicyGeneration {
		return nil, archive.ErrPolicyChanged
	}
	sort.SliceStable(anchors, func(i, j int) bool { return anchors[i].OriginalSentAt.Before(anchors[j].OriginalSentAt) })
	var out []archive.SourceAnchor
	seen := map[string]bool{}
	for _, anchor := range anchors {
		if anchor.Key.Account != q.Account || anchor.OriginalSentAt.IsZero() || seen[anchor.Key.Chat] || !policyPermits(policy, archive.SourceEvent{Key: anchor.Key, OriginalSentAt: anchor.OriginalSentAt, OriginalDateKnown: true}, time.Now()) {
			continue
		}
		if !q.AllSelected {
			selected := false
			for _, chat := range q.Chats {
				selected = selected || string(chat) == anchor.Key.Chat
			}
			if !selected {
				continue
			}
		}
		seen[anchor.Key.Chat] = true
		out = append(out, anchor)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}
func backfillAnchor(chat, id string, sent time.Time) archive.SourceAnchor {
	jid := chat + "@s.whatsapp.net"
	return archive.SourceAnchor{Key: archive.SourceKey{Account: durableAccount, Chat: jid, Sender: jid, Message: id}, OriginalSentAt: sent}
}
func backfillFixture(t *testing.T, budget int, anchors ...archive.SourceAnchor) (*Runtime, *backfillAnchorStore, *recordingStore) {
	t.Helper()
	base := fixtureStore()
	r := testRuntime(base)
	s := &backfillAnchorStore{ConnectorStore: base, anchors: anchors}
	r.store, r.config.BackfillRequests = s, budget
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close backfill runtime: %v", err)
		}
	})
	return r, s, base
}

type capturedBackfill struct {
	mu       sync.Mutex
	messages []*waE2E.Message
	failure  error
}

func (c *capturedBackfill) send(ctx context.Context, m *waE2E.Message) error {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 10*time.Second || ctx.Err() != nil {
		return errors.New("missing bounded backfill deadline")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, m)
	return c.failure
}
func (c *capturedBackfill) snapshot() []*waE2E.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*waE2E.Message(nil), c.messages...)
}
func waitBackfillIdle(t *testing.T, r *Runtime, c *capturedBackfill, count int) {
	t.Helper()
	waitOptional(t, func() bool {
		r.optionalMu.Lock()
		pending := r.optional != nil && r.optional.backfillPending
		r.optionalMu.Unlock()
		return !pending && len(c.snapshot()) == count
	})
}
func TestConfiguredBackfillDisabledSkipsQueryAndRequest(t *testing.T) {
	r, s, _ := backfillFixture(t, 0, backfillAnchor("200", "disabled", time.Unix(1000, 0)))
	capture := &capturedBackfill{}
	r.sendHistoryRequest = capture.send
	if err := r.requestBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.queries != 0 || len(capture.snapshot()) != 0 || r.backfillRequests != 0 {
		t.Fatal("disabled backfill performed work")
	}
}
func TestConfiguredBackfillConnectedWorkerUsesAvailableBudget(t *testing.T) {
	r, s, _ := backfillFixture(t, 2, backfillAnchor("200", "connected-anchor", time.Unix(1000, 0)))
	capture := &capturedBackfill{}
	r.sendHistoryRequest = capture.send
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitBackfillIdle(t, r, capture, 1)
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.backfillRequests != 1 || s.queries != 1 || s.selection.Account != durableAccount || !s.selection.AllSelected || s.selection.PolicyGeneration != 1 || s.selection.Limit != 32 {
		t.Fatal("connected selector was not bounded/current/account scoped")
	}
	demand := capture.snapshot()[0].GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
	if demand.GetChatJID() != "200@s.whatsapp.net" || demand.GetOldestMsgID() != "connected-anchor" || demand.GetOnDemandMsgCount() != 50 {
		t.Fatalf("wrong bounded source anchor: %+v", demand)
	}
}
func TestConfiguredBackfillBudgetSurvivesRepeatedCallsAndReconnect(t *testing.T) {
	fromMe := backfillAnchor("300", "three", time.Unix(1300, 0))
	fromMe.Key.FromMe, fromMe.Key.Sender = true, durableAccount
	r, s, _ := backfillFixture(t, 2, fromMe, backfillAnchor("200", "two", time.Unix(1200, 0)), backfillAnchor("400", "four", time.Unix(1400, 0)))
	capture := &capturedBackfill{}
	r.sendHistoryRequest = capture.send
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitBackfillIdle(t, r, capture, 2)
	for i := 0; i < 3; i++ {
		if err := r.requestBackfill(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool { return r.Status().Connection == "connected" })
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages := capture.snapshot()
	if len(messages) != 2 || r.backfillRequests != 2 || s.queries != 1 {
		t.Fatal("automatic lifetime budget reset or requeried exhausted work")
	}
	for i, want := range []struct {
		chat, id string
		fromMe   bool
		sent     int64
	}{{"200@s.whatsapp.net", "two", false, 1200}, {"300@s.whatsapp.net", "three", true, 1300}} {
		p := messages[i].GetProtocolMessage()
		d := p.GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
		if p.GetType() != waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE || d.GetChatJID() != want.chat || d.GetOldestMsgID() != want.id || d.GetOldestMsgFromMe() != want.fromMe || d.GetOldestMsgTimestampMS() != want.sent || d.GetOnDemandMsgCount() != 50 {
			t.Fatalf("request lost exact anchor: %+v", d)
		}
	}
}
func TestConfiguredBackfillFiltersDateAndSelectiveAnchorsBeforeRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy archive.IngestionPolicy
	}{
		{"date", archive.IngestionPolicy{Generation: 1, DateMode: archive.DateSince, ImportStart: time.Unix(1100, 0), ChatMode: archive.ChatsAll}},
		{"selective", archive.IngestionPolicy{Generation: 1, DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective, IncludedSourceChats: []string{"300@s.whatsapp.net"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, base := backfillFixture(t, 1, backfillAnchor("200", "denied", time.Unix(1000, 0)), backfillAnchor("300", "allowed", time.Unix(1200, 0)), backfillAnchor("400", "unknown-date", time.Time{}))
			base.policy = tc.policy
			capture := &capturedBackfill{}
			r.sendHistoryRequest = capture.send
			if err := r.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.deliver(&events.Connected{})
			waitBackfillIdle(t, r, capture, 1)
			if err := r.Disconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			d := capture.snapshot()[0].GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
			if d.GetChatJID() != "300@s.whatsapp.net" || r.backfillRequests != 1 || s.selection.Limit != 32 {
				t.Fatal("denied anchor consumed finite request allowance")
			}
			base.mu.Lock()
			defer base.mu.Unlock()
			found := false
			for _, u := range base.syncs {
				found = found || u.Chat == "300@s.whatsapp.net" && u.GapCode == "backfill_requested_coverage_unknown" && u.CoverageUnknown
			}
			if !found {
				t.Fatal("permitted anchor lost fixed unknown-coverage gap")
			}
		})
	}
}
func TestConfiguredBackfillFailurePersistsFixedGapAfterReopen(t *testing.T) {
	s, dir := durableFixture(t)
	wrapper := &backfillAnchorStore{ConnectorStore: s, anchors: []archive.SourceAnchor{backfillAnchor("200", "seed", time.Unix(1000, 0))}}
	r := durableRuntime(t, wrapper)
	r.config.BackfillRequests = 1
	if err := r.message(context.Background(), liveMessage("seed", "permitted backfill anchor")); err != nil {
		t.Fatal(err)
	}
	const canary = "SENSITIVE_BACKFILL_PROVIDER_ERROR_CANARY"
	capture := &capturedBackfill{failure: errors.New(canary)}
	r.sendHistoryRequest = capture.send
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitBackfillIdle(t, r, capture, 1)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	page, err := reopened.DiscoveryChats(context.Background(), archive.ChatListRequest{Limit: 20})
	if err != nil || len(page.Chats) != 1 {
		t.Fatalf("reopened discovery: %+v %v", page, err)
	}
	if len(page.Chats[0].GapCodes) != 1 || page.Chats[0].GapCodes[0] != "backfill_request_failed" || page.Chats[0].Coverage != "unknown" {
		t.Fatal("failed request lost durable fixed gap")
	}
	durableAssertAbsent(t, dir, canary)
}
func TestConfiguredBackfillRejectsOversizedAnchorSetWithoutRequest(t *testing.T) {
	anchors := make([]archive.SourceAnchor, 33)
	for i := range anchors {
		anchors[i] = backfillAnchor("200", "anchor", time.Unix(1000, 0))
	}
	r, s, base := backfillFixture(t, 2, anchors...)
	s.malformed = true
	capture := &capturedBackfill{}
	r.sendHistoryRequest = capture.send
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool { return r.Status().MediaCode == ErrMediaLimit.Error() })
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.queries != 1 || s.selection.Limit != 32 || len(capture.snapshot()) != 0 || r.backfillRequests != 0 || len(base.unsafe) != 0 {
		t.Fatal("oversized selector partially sent or quarantined source")
	}
}
