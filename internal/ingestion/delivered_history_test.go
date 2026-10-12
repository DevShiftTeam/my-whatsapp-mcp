package ingestion

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type deliveredFixtureStore struct {
	*rebuildFixtureStore
	selection        archive.RebuildHistorySelection
	captures         int
	positiveAttempts []archive.SourceEvent
	accepted         int
	positiveLimit    int
	policyRace       bool
	frontiers        []archive.SourceAnchor
	legacySelections int
	pages            []archive.DeliveredHistoryPage
	registered       []archive.DeliveredHistoryPage
}

func (s *deliveredFixtureStore) RebuildHistorySelection(context.Context) (*archive.RebuildHistorySelection, error) {
	v := s.selection
	return &v, nil
}
func (s *deliveredFixtureStore) CaptureDeliveredHistoryAdmission(context.Context) (*archive.DeliveredHistoryAdmission, error) {
	s.captures++
	return &archive.DeliveredHistoryAdmission{StrategyEpoch: s.selection.StrategyEpoch, ObservationGeneration: s.selection.ObservationGeneration, PolicyGeneration: s.policy.Generation}, nil
}
func (s *deliveredFixtureStore) ObserveDeliveredHistoryAnchor(_ context.Context, a archive.DeliveredHistoryAdmission, k archive.SourceAnchor) error {
	if a.StrategyEpoch == s.selection.StrategyEpoch && a.PolicyGeneration == s.policy.Generation {
		s.frontiers = append(s.frontiers, k)
	}
	return nil
}
func (s *deliveredFixtureStore) NextDeliveredHistoryPage(context.Context) (*archive.DeliveredHistoryPage, error) {
	if len(s.pages) == 0 {
		return nil, nil
	}
	p := s.pages[0]
	return &p, nil
}
func (s *deliveredFixtureStore) RegisterDeliveredHistoryPage(_ context.Context, p archive.DeliveredHistoryPage) error {
	s.registered = append(s.registered, p)
	s.pages = s.pages[1:]
	return nil
}
func (s *deliveredFixtureStore) NextRebuildCandidates(context.Context, int) ([]archive.SourceAnchor, error) {
	s.legacySelections++
	return nil, nil
}
func (s *deliveredFixtureStore) NextRebuildDiscovery(context.Context) (*archive.RebuildDiscovery, error) {
	s.legacySelections++
	return nil, nil
}
func (s *deliveredFixtureStore) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, g archive.Generation) (archive.ApplyResult, error) {
	if e.Kind == archive.EventOriginal || e.Kind == archive.EventEditContent {
		s.positiveAttempts = append(s.positiveAttempts, e)
		if e.DeliveredHistory != nil {
			if s.policyRace {
				s.policyRace = false
				s.policy.Generation++
				return archive.ApplyResult{}, archive.ErrPolicyChanged
			}
			a := e.DeliveredHistory
			if g != a.PolicyGeneration || a.PolicyGeneration != s.policy.Generation || a.StrategyEpoch != s.selection.StrategyEpoch || a.ObservationGeneration != s.selection.ObservationGeneration || e.RebuildRequestID != "" || s.accepted >= s.positiveLimit {
				return archive.ApplyResult{Outcome: archive.OutcomeFiltered}, nil
			}
			s.accepted++
		}
	}
	return s.recordingStore.ApplySourceEvent(ctx, e, g)
}
func deliveredFixture(t *testing.T) (*Runtime, *deliveredFixtureStore) {
	t.Helper()
	r, base := rebuildFixture()
	s := &deliveredFixtureStore{rebuildFixtureStore: base, positiveLimit: 10000, selection: archive.RebuildHistorySelection{Policy: archive.RebuildHistoryPolicy("delivered-history"), StrategyEpoch: 1, ObservationGeneration: r.config.Rebuild.ObservationID, Account: r.SourceAccount()}}
	r.store = s
	r.config.RebuildHistoryPolicy = s.selection.Policy
	if err := configureRebuild(context.Background(), &r.config, s); err != nil {
		t.Fatal(err)
	}
	return r, s
}
func deliveredNotification(r *Runtime, records ...*waHistorySync.HistorySyncMsg) *events.Message {
	e := liveMessage("synthetic-history-notification", "")
	e.Info.IsFromMe = true
	e.Info.Sender = *r.client.Store.ID
	e.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: &waE2E.HistorySyncNotification{InitialHistBootstrapInlinePayload: compressed(historyWire(records...))}}}
	return e
}
func deliveredFiftyRecords() []*waHistorySync.HistorySyncMsg {
	var rows []*waHistorySync.HistorySyncMsg
	for i := 0; i < 50; i++ {
		m := historyRecord(fmt.Sprintf("delivered-%02d", i), "deliveredbodyreceipt")
		m.Message.MessageTimestamp = proto.Uint64(uint64(1000 - i))
		rows = append(rows, m)
	}
	return rows
}
func TestDeliveredHistoryAuthenticatedFiftyBodies(t *testing.T) {
	r, s := deliveredFixture(t)
	if err := r.historyFrom(context.Background(), deliveredNotification(r, deliveredFiftyRecords()...)); err != nil {
		t.Fatal(err)
	}
	if s.captures != 1 || s.accepted != 50 || len(s.events) != 50 {
		t.Fatalf("delivered bulk admission missing: captures=%d accepted=%d events=%d", s.captures, s.accepted, len(s.events))
	}
	for _, e := range s.events {
		if e.DeliveredHistory == nil || e.RebuildRequestID != "" {
			t.Fatal("delivered snapshot misrepresented as exact request proof")
		}
	}
	if len(s.frontiers) != 1 || s.frontiers[0].Key.Message != "delivered-49" {
		t.Fatal("whole successful chunk did not advance oldest observed frontier")
	}
}
func TestDeliveredHistoryCapturedPolicyNeverRefreshed(t *testing.T) {
	r, s := deliveredFixture(t)
	s.policyRace = true
	records := deliveredFiftyRecords()
	records = append(records, deliveredControlRecord("removed", waE2E.ProtocolMessage_REVOKE, 2000000))
	if err := r.historyFrom(context.Background(), deliveredNotification(r, records...)); err != nil {
		t.Fatal(err)
	}
	if s.captures != 1 || s.accepted != 0 || r.Status().ReconciliationRequired {
		t.Fatal("stale captured positive admission was refreshed or fenced")
	}
	for _, e := range s.positiveAttempts {
		if e.DeliveredHistory == nil || e.DeliveredHistory.PolicyGeneration != 1 {
			t.Fatal("positive policy recaptured during apply")
		}
	}
	found := false
	for _, e := range s.events {
		found = found || e.Kind == archive.EventDelete
	}
	if !found {
		t.Fatal("stale positives discarded following deletion")
	}
}
func TestDeliveredHistoryPositiveExhaustionKeepsControls(t *testing.T) {
	r, s := deliveredFixture(t)
	s.positiveLimit = 2
	records := deliveredFiftyRecords()
	records = append(records, deliveredControlRecord("removed", waE2E.ProtocolMessage_REVOKE, 2000000), deliveredControlRecord("edited", waE2E.ProtocolMessage_MESSAGE_EDIT, 2001000))
	if err := r.historyFrom(context.Background(), deliveredNotification(r, records...)); err != nil {
		t.Fatal(err)
	}
	if s.accepted != 2 || r.Status().ReconciliationRequired {
		t.Fatal("positive exhaustion did not stay filtered")
	}
	deleted, invalidated := false, false
	for _, e := range s.events {
		deleted = deleted || e.Kind == archive.EventDelete
		invalidated = invalidated || e.Kind == archive.EventEditInvalidation
	}
	if !deleted || !invalidated {
		t.Fatal("positive exhaustion dropped negative suffix")
	}
}
func TestDeliveredHistoryRequiresAuthenticatedPrivateEntry(t *testing.T) {
	for _, entry := range []string{"forged_notification", "direct_history", "direct_live"} {
		t.Run(entry, func(t *testing.T) {
			r, s := deliveredFixture(t)
			switch entry {
			case "forged_notification":
				e := deliveredNotification(r, historyRecord("forged", "MUST_NOT_IMPORT"))
				e.Info.IsFromMe = false
				if err := r.historyFrom(context.Background(), e); err == nil {
					t.Fatal("unauthenticated notification accepted")
				}
			case "direct_history":
				if err := r.projectHistory(context.Background(), historyWire(historyRecord("direct", "MUST_NOT_IMPORT"))); err != nil {
					t.Fatal(err)
				}
			case "direct_live":
				if err := r.message(context.Background(), liveMessage("direct", "MUST_NOT_IMPORT")); err != nil {
					t.Fatal(err)
				}
			}
			if s.captures != 0 || s.accepted != 0 || len(s.events) != 0 {
				t.Fatal("non-authenticated-history path minted delivered authority")
			}
		})
	}
}
func TestDeliveredHistoryBadSuffixDoesNotAdvanceFrontier(t *testing.T) {
	r, s := deliveredFixture(t)
	bad := historyRecord("bad", "")
	bad.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum()}}
	if err := r.historyFrom(context.Background(), deliveredNotification(r, historyRecord("good", "prefixreceipt"), bad)); err == nil {
		t.Fatal("malformed control accepted")
	}
	if len(s.frontiers) != 0 {
		t.Fatal("failed whole chunk advanced frontier")
	}
}
func TestDeliveredHistorySchedulerDoesNotSelectLegacyCopies(t *testing.T) {
	r, s := deliveredFixture(t)
	r.mu.Lock()
	r.status.Connection = archive.ConnectionState("connected")
	r.mu.Unlock()
	r.lastRebuildRequest = time.Time{}
	r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()})
	if s.legacySelections != 0 {
		t.Fatal("delivered scheduler selected legacy per-key or oldest work")
	}
}

func deliveredControlRecord(id string, kind waE2E.ProtocolMessage_Type, revision int64) *waHistorySync.HistorySyncMsg {
	m := historyRecord("control-"+id, "")
	m.Message.MessageTimestamp = proto.Uint64(uint64(revision / 1000))
	m.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: kind.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id), RemoteJID: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}, TimestampMS: proto.Int64(revision)}}
	if kind == waE2E.ProtocolMessage_MESSAGE_EDIT {
		m.Message.Message.ProtocolMessage.EditedMessage = &waE2E.Message{Conversation: proto.String("deliverededitreceipt")}
	}
	return m
}

func TestDeliveredHistoryDefaultSelectedOriginalStillWithholdsBulk(t *testing.T) {
	r, s := deliveredFixture(t)
	s.selection.Policy = archive.RebuildHistorySelectedOriginal
	r.config.RebuildHistoryPolicy = archive.RebuildHistorySelectedOriginal
	if err := configureRebuild(context.Background(), &r.config, s); err != nil {
		t.Fatal(err)
	}
	if err := r.historyFrom(context.Background(), deliveredNotification(r, deliveredFiftyRecords()...)); err != nil {
		t.Fatal(err)
	}
	if s.captures != 0 || s.accepted != 0 || len(s.events) != 0 || len(s.discoveries) != 50 {
		t.Fatal("default selection gained bulk positive admission")
	}
}
func TestDeliveredHistoryStartupSelectionMismatchRejects(t *testing.T) {
	r, s := deliveredFixture(t)
	r.config.RebuildHistoryPolicy = archive.RebuildHistorySelectedOriginal
	if err := configureRebuild(context.Background(), &r.config, s); err == nil {
		t.Fatal("running immutable delivered selection silently changed")
	}
}
func TestDeliveredHistorySchedulerRegistersBoundedPageBeforeUnknownSend(t *testing.T) {
	r, s := deliveredFixture(t)
	r.status.Connection = "connected"
	anchor := archive.SourceAnchor{Key: archive.SourceKey{Account: r.SourceAccount(), Chat: "300@g.us", Sender: "200@s.whatsapp.net", Message: "newest-seed"}, OriginalSentAt: time.Unix(1400, 0)}
	s.pages = []archive.DeliveredHistoryPage{{RebuildDiscovery: archive.RebuildDiscovery{ID: "delivered-page-id", OperationID: s.progress.OperationID, OperationRevision: s.progress.Revision, Generation: s.progress.Generation, PolicyGeneration: s.progress.PolicyGeneration, Anchor: anchor, CreatedAt: time.Now()}, StrategyEpoch: s.selection.StrategyEpoch}}
	sends := 0
	r.sendRebuildRequest = func(_ context.Context, id string, m *waE2E.Message) error {
		q := m.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
		if len(s.registered) != 1 || id != s.registered[0].ID || q == nil || q.GetOnDemandMsgCount() != 500 || q.GetOldestMsgID() != anchor.Key.Message || q.GetChatJID() != anchor.Key.Chat {
			t.Fatal("page sent without exact durable registration or bounded SDK request")
		}
		sends++
		return errors.New("synthetic unknown send")
	}
	if !r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) || sends != 1 {
		t.Fatal("delivered page did not dispatch")
	}
	r.lastRebuildRequest = time.Time{}
	if r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) || sends != 1 || s.legacySelections != 0 || len(s.requests) != 0 {
		t.Fatal("unknown page retried or legacy copy authority created")
	}
}

func TestDeliveredHistorySchedulerUsesDedicatedCadence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        archive.RebuildHistoryPolicy
		elapsed     time.Duration
		wantAttempt bool
	}{
		{"delivered_before_one_second", archive.RebuildHistoryDelivered, 500 * time.Millisecond, false},
		{"delivered_after_one_second", archive.RebuildHistoryDelivered, 2 * time.Second, true},
		{"selected_still_waits_ten_seconds", archive.RebuildHistorySelectedOriginal, 2 * time.Second, false},
		{"selected_after_ten_seconds", archive.RebuildHistorySelectedOriginal, 11 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s := deliveredFixture(t)
			r.config.RebuildHistoryPolicy = tc.mode
			s.selection.Policy = tc.mode
			if err := configureRebuild(context.Background(), &r.config, s); err != nil {
				t.Fatal(err)
			}
			r.status.Connection = "connected"
			then := time.Now().Add(-tc.elapsed)
			r.lastRebuildRequest = then
			s.progress.LastAttemptAt = then
			anchor := archive.SourceAnchor{Key: archive.SourceKey{Account: r.SourceAccount(), Chat: "300@g.us", Sender: "200@s.whatsapp.net", Message: "cadence-anchor"}, OriginalSentAt: time.Unix(1400, 0)}
			s.pages = []archive.DeliveredHistoryPage{{RebuildDiscovery: archive.RebuildDiscovery{ID: "cadence-page", OperationID: s.progress.OperationID, OperationRevision: s.progress.Revision, Generation: s.progress.Generation, PolicyGeneration: s.progress.PolicyGeneration, Anchor: anchor, CreatedAt: time.Now()}, StrategyEpoch: s.selection.StrategyEpoch}}
			sends := 0
			r.sendRebuildRequest = func(context.Context, string, *waE2E.Message) error { sends++; return nil }
			r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()})
			attempted := sends > 0 || s.legacySelections > 0
			if attempted != tc.wantAttempt {
				t.Fatalf("wrong mode-specific cadence: attempted=%t want=%t", attempted, tc.wantAttempt)
			}
			if tc.mode == archive.RebuildHistoryDelivered && s.legacySelections != 0 {
				t.Fatal("delivered cadence selected legacy work")
			}
			if tc.mode == archive.RebuildHistorySelectedOriginal && sends != 0 {
				t.Fatal("selected cadence used delivered page")
			}
		})
	}
}
