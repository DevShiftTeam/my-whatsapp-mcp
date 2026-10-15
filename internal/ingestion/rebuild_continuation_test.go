package ingestion

import (
	"context"
	"errors"
	"fmt"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"testing"
	"time"
)

func continuationTick(r *Runtime, s *rebuildFixtureStore) {
	r.lastRebuildRequest = time.Time{}
	s.progress.LastAttemptAt = time.Time{}
}
func TestRebuildContinuationAcrossWindowsAndRuntimeRestart(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	// Existing durable anchors seed work without waiting for a new history event.
	for i := 0; i < 96; i++ {
		s.discoveries = append(s.discoveries, archive.SourceAnchor{Key: archive.SourceKey{Account: r.SourceAccount(), Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: fmt.Sprintf("seed-%d", i)}, OriginalSentAt: time.Unix(1000, 0)})
	}
	sends := 0
	send := func(context.Context, string, *waE2E.Message) error {
		sends++
		return errors.New("synthetic unknown outcome")
	}
	r.sendRebuildRequest = send
	for i := 0; i < 96; i++ {
		if i == 48 {
			r = testRuntime(s.recordingStore)
			r.store = s
			r.config.Rebuild = &RebuildConfig{ObservationID: s.progress.Generation, Account: r.SourceAccount(), Boundary: time.Unix(1500, 0), Requests: 32}
			r.client.Store.LIDs = &voiceRedeliveryLIDStore{mappings: map[types.JID]types.JID{}}
			r.status.Connection = "connected"
			r.sendRebuildRequest = send
		}
		continuationTick(r, s)
		if !r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) {
			t.Fatalf("durable copy progress stopped at %d", i)
		}
	}
	continuationTick(r, s)
	if r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) {
		t.Fatal("unknown sends retried")
	}
	if sends != 96 || len(s.requests) != 96 || s.progress.CopySpent != 96 || s.selectorLimit > 32 {
		t.Fatal("continuation lost finite window or durable dispatch accounting")
	}
	live := liveMessage("live-during-recovery", "LIVE_BODY")
	live.Info.Timestamp = time.Now()
	if err := r.message(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 || s.events[0].Body != "LIVE_BODY" {
		t.Fatal("recovery interfered with normal live source")
	}
}
func TestRebuildDiscoveryContinuesAcrossChatsWithoutRepeatingUnknownCursor(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	s.progress.CopySpent = s.progress.CopyLimit
	for i := 0; i < 40; i++ {
		s.discoveryAnchors = append(s.discoveryAnchors, archive.SourceAnchor{Key: archive.SourceKey{Account: r.SourceAccount(), Chat: fmt.Sprintf("%d@g.us", 300+i), Sender: "200@s.whatsapp.net", Message: fmt.Sprintf("cursor-%d", i)}, OriginalSentAt: time.Unix(1000, 0)})
	}
	sends := 0
	r.sendRebuildRequest = func(_ context.Context, id string, m *waE2E.Message) error {
		q := m.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
		if q == nil || q.GetOnDemandMsgCount() != 50 {
			t.Fatal("unqualified discovery batch")
		}
		registered := false
		for _, a := range s.discoveryAttempts {
			registered = registered || a.ID == id && a.Anchor.Key.Chat == q.GetChatJID() && a.Anchor.Key.Message == q.GetOldestMsgID()
		}
		if !registered {
			t.Fatal("discovery sent before exact cursor registration")
		}
		sends++
		return errors.New("unknown discovery send")
	}
	for i := 0; i < 40; i++ {
		continuationTick(r, s)
		if !r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) {
			t.Fatalf("chat rotation stopped at %d", i)
		}
	}
	continuationTick(r, s)
	if r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) || sends != 40 {
		t.Fatal("same unknown frontier resent")
	}
	older := s.discoveryAnchors[0]
	older.Key.Message = "older-cursor"
	older.OriginalSentAt = time.Unix(900, 0)
	s.discoveryAnchors = append(s.discoveryAnchors, older)
	continuationTick(r, s)
	if !r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) || sends != 41 {
		t.Fatal("older observed frontier did not resume discovery")
	}
}
func TestRebuildSharedCadenceSurvivesRuntimeRestart(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	s.progress.LastAttemptAt = time.Now()
	s.discoveries = []archive.SourceAnchor{{Key: archive.SourceKey{Account: r.SourceAccount(), Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "known"}, OriginalSentAt: time.Unix(1000, 0)}}
	r.sendRebuildRequest = func(context.Context, string, *waE2E.Message) error {
		t.Fatal("durable cadence bypassed after runtime restart")
		return nil
	}
	if r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) {
		t.Fatal("runtime-local empty clock reset durable cadence")
	}
}
func TestRebuildMetadataCapacityDoesNotDiscardControls(t *testing.T) {
	r, s := rebuildFixture()
	s.discoveryErr = archive.ErrQueueLimitExceeded
	retention := &historyRawRetention{}
	ctx := context.WithValue(context.Background(), historyRawRetentionKey{}, retention)
	if err := r.message(ctx, liveMessage("at-capacity", "BODY")); err != nil {
		t.Fatal(err)
	}
	if err := r.message(context.Background(), mutation("at-capacity", waE2E.ProtocolMessage_REVOKE)); err != nil {
		t.Fatal(err)
	}
	if r.Status().ReconciliationRequired || len(s.events) != 1 || s.events[0].Kind != archive.EventDelete {
		t.Fatal("positive metadata capacity swallowed security control")
	}
	if !retention.incomplete {
		t.Fatal("capacity-dropped history was falsely eligible for raw deletion")
	}
}
func TestRebuildLateResponseRemainsExactAndPolicyGuarded(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			r, s := rebuildFixture()
			r.status.Connection = "connected"
			e := liveMessage("late-source", "body")
			if err := r.message(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			var id string
			r.sendRebuildRequest = func(_ context.Context, requestID string, _ *waE2E.Message) error {
				id = requestID
				return context.DeadlineExceeded
			}
			if !r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) || id == "" {
				t.Fatal("request missing")
			}
			if stale {
				s.policy.Generation++
				s.progress.PolicyGeneration = s.policy.Generation
				s.progress.Revision++
			}
			late, err := r.client.ParseWebMessage(types.EmptyJID, historyRecord("late-source", "LATE_FRESH_BODY").Message)
			if err != nil {
				t.Fatal(err)
			}
			late.UnavailableRequestID = id
			if err = r.message(context.Background(), late); err != nil {
				t.Fatal(err)
			}
			if stale && len(s.events) != 0 || !stale && (len(s.events) != 1 || s.events[0].Body != "LATE_FRESH_BODY") {
				t.Fatal("late reply admission lost exact current-policy semantics")
			}
		})
	}
}
func TestRebuildDrainCancelsOnlyRecoveryAttempt(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	if err := r.message(context.Background(), liveMessage("drain-source", "body")); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	r.sendRebuildRequest = func(ctx context.Context, _ string, _ *waE2E.Message) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	go func() { r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}); close(finished) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("recovery attempt not started")
	}
	s.progress.State = "paused"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.DrainRebuild(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("recovery send did not join")
	}
	if r.Status().Connection != "connected" || r.Status().ReconciliationRequired {
		t.Fatal("recovery drain stopped ordinary live runtime")
	}
	continuationTick(r, s)
	if r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) {
		t.Fatal("paused operation dispatched")
	}
}

func TestRebuildBackfillWakesOnlyDurableOperation(t *testing.T) {
	r, s := rebuildFixture()
	r.config.BackfillRequests = 32
	n := &optionalNetwork{accepting: true, commands: make(chan optionalCommand, 1)}
	r.optional = n
	q := BackfillRequest{PolicyGeneration: s.policy.Generation, AllSelected: true, Limit: 32}
	if err := r.RequestBackfill(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	select {
	case cmd := <-n.commands:
		if !cmd.wake {
			t.Fatal("rebuild enqueued unledgered legacy history request")
		}
	default:
		t.Fatal("durable operation not woken")
	}
	if n.backfillPending || r.backfillRequests != 0 {
		t.Fatal("rebuild used ordinary lifetime backfill budget")
	}
	q.PolicyGeneration++
	if err := r.RequestBackfill(context.Background(), q); !errors.Is(err, archive.ErrPolicyChanged) {
		t.Fatal("stale policy backfill wake accepted")
	}
}

func TestRebuildWindowSkipsUnverifiedTransportWithoutSpending(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	blocked := archive.SourceAnchor{Key: archive.SourceKey{Account: r.SourceAccount(), Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "blocked"}, OriginalSentAt: time.Unix(1000, 0)}
	good := blocked
	good.Key.Chat = "300@s.whatsapp.net"
	good.Key.Sender = good.Key.Chat
	good.Key.Message = "permitted"
	s.discoveries = []archive.SourceAnchor{blocked, good}
	r.client.Store.LIDs = &voiceRedeliveryLIDStore{mappings: map[types.JID]types.JID{types.NewJID("200", types.DefaultUserServer): types.NewJID("900", types.HiddenUserServer)}}
	r.sendRebuildRequest = func(_ context.Context, id string, _ *waE2E.Message) error {
		if s.requests[id].Key != good.Key {
			t.Fatal("unverified transport was dispatched")
		}
		return nil
	}
	if !r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) || len(s.requests) != 1 || s.progress.CopySpent != 1 {
		t.Fatal("blocked first candidate starved valid candidate in bounded window")
	}
	if !s.deferred[blocked.Key].After(time.Now()) || !s.deferred[good.Key].IsZero() {
		t.Fatal("presend transport deferral did not preserve exact authority boundary")
	}
}

func TestRebuildNeverDefersUnknownRegisteredSend(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	if err := r.message(context.Background(), liveMessage("unknown-send", "body")); err != nil {
		t.Fatal(err)
	}
	r.sendRebuildRequest = func(context.Context, string, *waE2E.Message) error { return ErrMediaUnavailable }
	if !r.optionalRebuildStep(&optionalNetwork{ctx: context.Background()}) || len(s.requests) != 1 || len(s.deferred) != 0 {
		t.Fatal("unknown registered send incorrectly became retryable deferral")
	}
}
