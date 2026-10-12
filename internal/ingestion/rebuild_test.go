package ingestion

import (
	"context"
	"errors"
	"fmt"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

type rebuildFixtureStore struct {
	*recordingStore
	requests          map[string]archive.RebuildRequest
	discoveries       []archive.SourceAnchor
	guard             archive.RebuildGuard
	canonical         map[string]string
	restrictions      []rebuildRestrictionFixture
	restrictionErr    error
	progress          *archive.RebuildProgress
	discoveryAnchors  []archive.SourceAnchor
	discoveryAttempts map[archive.SourceKey]archive.RebuildDiscovery
	selectorLimit     int
	discoveryErr      error
	deferred          map[archive.SourceKey]time.Time
}

type rebuildRestrictionFixture struct {
	key      archive.SourceKey
	expires  time.Time
	viewOnce bool
}

func (s *rebuildFixtureStore) RecordRebuildRestriction(_ context.Context, key archive.SourceKey, expires time.Time, viewOnce bool) error {
	if s.restrictionErr != nil {
		return s.restrictionErr
	}
	s.restrictions = append(s.restrictions, rebuildRestrictionFixture{key, expires, viewOnce})
	if !expires.IsZero() && (s.guard.ExpiresAt.IsZero() || expires.Before(s.guard.ExpiresAt)) {
		s.guard.ExpiresAt = expires
	}
	s.guard.ViewOnce = s.guard.ViewOnce || viewOnce
	return nil
}
func (s *rebuildFixtureStore) Canonical(_ context.Context, _ string, identity string) (string, error) {
	if canonical, ok := s.canonical[identity]; ok {
		return canonical, nil
	}
	return identity, nil
}
func (s *rebuildFixtureStore) DeferRebuildCandidate(_ context.Context, key archive.SourceKey) error {
	if s.deferred == nil {
		s.deferred = map[archive.SourceKey]time.Time{}
	}
	s.deferred[key] = time.Now().Add(10 * time.Minute)
	return nil
}
func (s *rebuildFixtureStore) RebuildDispatchState(context.Context) (*archive.RebuildProgress, error) {
	p := *s.progress
	return &p, nil
}
func (s *rebuildFixtureStore) NextRebuildCandidates(_ context.Context, limit int) ([]archive.SourceAnchor, error) {
	s.selectorLimit = max(s.selectorLimit, limit)
	var out []archive.SourceAnchor
	for _, a := range s.discoveries {
		if until := s.deferred[a.Key]; until.After(time.Now()) {
			continue
		}
		seen := false
		for _, q := range s.requests {
			seen = seen || q.Key == a.Key
		}
		if !seen {
			out = append(out, a)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (s *rebuildFixtureStore) NextRebuildDiscovery(context.Context) (*archive.RebuildDiscovery, error) {
	for _, a := range s.discoveryAnchors {
		if until := s.deferred[a.Key]; until.After(time.Now()) {
			continue
		}
		if _, seen := s.discoveryAttempts[a.Key]; !seen {
			return &archive.RebuildDiscovery{ID: fmt.Sprintf("discovery-%d", len(s.discoveryAttempts)), OperationID: s.progress.OperationID, Generation: s.progress.Generation, OperationRevision: s.progress.Revision, PolicyGeneration: s.progress.PolicyGeneration, Anchor: a, CreatedAt: time.Now()}, nil
		}
	}
	return nil, nil
}
func (s *rebuildFixtureStore) RegisterRebuildDiscovery(_ context.Context, q archive.RebuildDiscovery) error {
	p := s.progress
	if q.OperationID != p.OperationID || q.OperationRevision != p.Revision || q.PolicyGeneration != s.policy.Generation {
		return archive.ErrPolicyChanged
	}
	if p.State != "running" || p.DiscoverySpent >= p.DiscoveryLimit || !p.LastAttemptAt.IsZero() && time.Since(p.LastAttemptAt) < optionalCooldown {
		return archive.ErrNoWork
	}
	if _, seen := s.discoveryAttempts[q.Anchor.Key]; seen {
		return archive.ErrNoWork
	}
	s.discoveryAttempts[q.Anchor.Key] = q
	p.DiscoverySpent++
	p.LastAttemptAt = q.CreatedAt
	return nil
}
func (s *rebuildFixtureStore) PendingRebuildRequest(_ context.Context, q archive.RebuildRequest) error {
	p := s.progress
	if q.OperationID != p.OperationID || q.OperationRevision != p.Revision || q.PolicyGeneration != s.policy.Generation {
		return archive.ErrPolicyChanged
	}
	if p.State != "running" || p.CopySpent >= p.CopyLimit || !p.LastAttemptAt.IsZero() && time.Since(p.LastAttemptAt) < optionalCooldown {
		return archive.ErrNoWork
	}
	for _, old := range s.requests {
		if old.Key == q.Key {
			return archive.ErrNoWork
		}
	}
	s.requests[q.ID] = q
	p.CopySpent++
	p.LastAttemptAt = q.CreatedAt
	return nil
}

func (s *rebuildFixtureStore) MatchRebuildRequest(_ context.Context, id string, key archive.SourceKey, generation string, policy archive.Generation) (bool, error) {
	q, ok := s.requests[id]
	return ok && q.Key == key && q.Generation == generation && q.PolicyGeneration == policy, nil
}
func (s *rebuildFixtureStore) RebuildGuard(context.Context, archive.SourceKey) (archive.RebuildGuard, error) {
	return s.guard, nil
}
func (s *rebuildFixtureStore) RecordRebuildDiscovery(_ context.Context, a archive.SourceAnchor) error {
	if s.discoveryErr != nil {
		return s.discoveryErr
	}
	for _, old := range s.discoveries {
		if old.Key == a.Key {
			return nil
		}
	}
	s.discoveries = append(s.discoveries, a)
	return nil
}
func rebuildFixture() (*Runtime, *rebuildFixtureStore) {
	s := &rebuildFixtureStore{recordingStore: fixtureStore(), requests: map[string]archive.RebuildRequest{}, discoveryAttempts: map[archive.SourceKey]archive.RebuildDiscovery{}, progress: &archive.RebuildProgress{OperationID: "synthetic-operation", Generation: "synthetic-generation", Revision: 1, PolicyGeneration: 1, State: "running", CopyLimit: 10000, DiscoveryLimit: 512}}
	r := testRuntime(s.recordingStore)
	r.store = s
	r.client.Store.LIDs = &voiceRedeliveryLIDStore{mappings: make(map[types.JID]types.JID)}
	r.config.Rebuild = &RebuildConfig{ObservationID: "synthetic-generation", Account: r.SourceAccount(), Boundary: time.Unix(1500, 0), Requests: 32}
	return r, s
}
func TestRebuildUnrequestedOldOriginalWithheld(t *testing.T) {
	r, s := rebuildFixture()
	if err := r.message(context.Background(), liveMessage("old", "OLD_BODY_CANARY")); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 0 || len(s.discoveries) != 1 {
		t.Fatal("unrequested old original reached writer or discovery lost")
	}
}
func TestRebuildOrdinaryLiveAndControlsContinue(t *testing.T) {
	r, s := rebuildFixture()
	e := liveMessage("new", "LIVE_BODY")
	e.Info.Timestamp = time.Unix(1600, 0)
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 || s.events[0].Body != "LIVE_BODY" {
		t.Fatal("new live original withheld")
	}
}
func TestRebuildExactCorrelatedOldResponse(t *testing.T) {
	for _, mismatch := range []string{"", "id", "chat", "sender", "direction", "policy", "generation", "web"} {
		t.Run(mismatch, func(t *testing.T) {
			r, s := rebuildFixture()
			e := liveMessage("old", "FRESH_PHONE_BODY")
			e.UnavailableRequestID = "fresh-request"
			e.SourceWebMsg = historyRecord("old", "FRESH_PHONE_BODY").Message
			key, err := r.envelopeKey(context.Background(), e.Info)
			if err != nil {
				t.Fatal(err)
			}
			q := archive.RebuildRequest{ID: e.UnavailableRequestID, Generation: r.config.Rebuild.ObservationID, Key: key, PolicyGeneration: 1, CreatedAt: time.Now()}
			switch mismatch {
			case "id":
				q.ID = "other"
			case "chat":
				q.Key.Chat = "other@s.whatsapp.net"
			case "sender":
				q.Key.Sender = "other@s.whatsapp.net"
			case "direction":
				q.Key.FromMe = true
			case "policy":
				q.PolicyGeneration++
			case "generation":
				q.Generation = "old"
			case "web":
				e.SourceWebMsg = nil
			}
			s.requests[q.ID] = q
			if err := r.message(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if mismatch == "" {
				if len(s.events) != 1 || s.events[0].Body != "FRESH_PHONE_BODY" || s.events[0].RebuildRequestID != "fresh-request" {
					t.Fatal("exact fresh response not admitted")
				}
			} else if len(s.events) != 0 {
				t.Fatal("mismatched response reached writer")
			}
		})
	}
}
func TestRebuildHistoryAlwaysDiscoveryOnly(t *testing.T) {
	r, s := rebuildFixture()
	if err := r.projectHistory(context.Background(), historyWire(historyRecord("old", "HISTORY_BODY_CANARY"))); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 0 || len(s.discoveries) != 1 {
		t.Fatal("unmatched history body admitted")
	}
}

func TestRebuildRequestRegisteredBeforeSend(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	e := liveMessage("old", "body")
	key, err := r.envelopeKey(context.Background(), e.Info)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.sendRebuildRequest = func(_ context.Context, id string, m *waE2E.Message) error {
		calls++
		q, ok := s.requests[id]
		if !ok || q.Key != key || q.Generation != r.config.Rebuild.ObservationID {
			t.Fatal("send preceded durable registration")
		}
		if len(m.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()) != 1 {
			t.Fatal("not exact copy request")
		}
		return nil
	}
	if err = r.requestRebuildCopy(context.Background(), archive.SourceAnchor{Key: key, OriginalSentAt: e.Info.Timestamp}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("request not sent")
	}
}

func TestRebuildKnownNegativeGuardsPreventDiscoveryRequests(t *testing.T) {
	for _, g := range []archive.RebuildGuard{{Deleted: true}, {ViewOnce: true}, {ExpiresAt: time.Unix(1001, 0)}, {ChatClearCutoff: time.Unix(1001, 0)}} {
		r, s := rebuildFixture()
		s.guard = g
		if err := r.message(context.Background(), liveMessage("old", "body")); err != nil {
			t.Fatal(err)
		}
		if len(s.events) != 0 || len(s.discoveries) != 0 {
			t.Fatal("known negative guard admitted discovery/body")
		}
	}
}
func TestRebuildHistoryAfterBoundaryStillDiscoveryOnly(t *testing.T) {
	r, s := rebuildFixture()
	record := historyRecord("new-history", "UNMATCHED_NEW_HISTORY_CANARY")
	record.Message.MessageTimestamp = proto.Uint64(1600)
	if err := r.projectHistory(context.Background(), historyWire(record)); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 0 || len(s.discoveries) != 1 {
		t.Fatal("history timestamp substituted for fresh request provenance")
	}
}
func TestRebuildOldControlsStillApply(t *testing.T) {
	r, s := rebuildFixture()
	e := mutation("old-target", waE2E.ProtocolMessage_REVOKE)
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 || s.events[0].Kind != archive.EventDelete {
		t.Fatal("old deletion skipped by rebuild boundary")
	}
}
func TestRebuildRequestCooldownAndFiniteBudget(t *testing.T) {
	r, s := rebuildFixture()
	r.status.Connection = "connected"
	for _, id := range []string{"first", "second"} {
		if err := r.message(context.Background(), liveMessage(id, "body")); err != nil {
			t.Fatal(err)
		}
	}
	r.sendRebuildRequest = func(context.Context, string, *waE2E.Message) error { return nil }
	n := &optionalNetwork{ctx: context.Background()}
	if !r.optionalRebuildStep(n) || len(s.requests) != 1 {
		t.Fatal("first request missing")
	}
	if r.optionalRebuildStep(n) || len(s.requests) != 1 {
		t.Fatal("cooldown ignored")
	}
	r.lastRebuildRequest = time.Time{}
	s.progress.LastAttemptAt = time.Time{}
	s.progress.CopySpent = s.progress.CopyLimit
	if r.optionalRebuildStep(n) || len(s.requests) != 1 {
		t.Fatal("durable finite budget ignored")
	}
}

type provenanceFixtureStore struct {
	*rebuildFixtureStore
	provenance *archive.RebuildProvenance
}

func (s *provenanceFixtureStore) RebuildProvenance(context.Context) (*archive.RebuildProvenance, error) {
	return s.provenance, nil
}
func TestRebuildProvenanceCannotBeIgnoredOrOverridden(t *testing.T) {
	r, s := rebuildFixture()
	p := &archive.RebuildProvenance{Generation: r.config.Rebuild.ObservationID, Account: r.SourceAccount(), DeviceID: "100:7@s.whatsapp.net", Boundary: r.config.Rebuild.Boundary}
	store := &provenanceFixtureStore{rebuildFixtureStore: s, provenance: p}
	c := Config{}
	if err := configureRebuild(context.Background(), &c, store); err != nil {
		t.Fatal(err)
	}
	if c.Rebuild == nil || c.Rebuild.ObservationID != p.Generation || c.Rebuild.DeviceID != p.DeviceID || c.Rebuild.Requests != 32 {
		t.Fatal("persisted provenance ignored")
	}
	c.Rebuild.Account = "other@s.whatsapp.net"
	if err := configureRebuild(context.Background(), &c, store); err == nil {
		t.Fatal("provenance override accepted")
	}
}

func TestRebuildPhoneCopyUsesVerifiedLIDTransport(t *testing.T) {
	for _, scenario := range []string{"incoming", "outgoing", "group"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, s := rebuildFixture()
			r.status.Connection = "connected"
			ownPN := types.NewJID("100", types.DefaultUserServer)
			ownLID := types.NewJID("800", types.HiddenUserServer)
			peerPN := types.NewJID("200", types.DefaultUserServer)
			peerLID := types.NewJID("900", types.HiddenUserServer)
			r.client.Store.LID = ownLID
			r.client.Store.LIDs = &voiceRedeliveryLIDStore{mappings: map[types.JID]types.JID{ownPN: ownLID, peerPN: peerLID}}
			s.canonical = map[string]string{ownLID.String(): ownPN.String(), peerLID.String(): peerPN.String()}
			key := archive.SourceKey{Account: ownPN.String(), Chat: peerPN.String(), Sender: peerPN.String(), Message: "lid-source"}
			expectedChat, expectedParticipant := peerLID.String(), ""
			if scenario == "outgoing" {
				key.Sender = ownPN.String()
				key.FromMe = true
			}
			if scenario == "group" {
				key.Chat = "300@g.us"
				expectedChat = key.Chat
				expectedParticipant = peerLID.String()
			}
			var requestID string
			r.sendRebuildRequest = func(_ context.Context, id string, m *waE2E.Message) error {
				requests := m.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
				if len(requests) != 1 {
					t.Fatal("request is not one exact key")
				}
				got := requests[0].GetMessageKey()
				if got.GetRemoteJID() != expectedChat || got.GetParticipant() != expectedParticipant || got.GetID() != key.Message || got.GetFromMe() != key.FromMe {
					t.Fatal("SDK phone request did not use verified transport identity")
				}
				if q, ok := s.requests[id]; !ok || q.Key != key {
					t.Fatal("transport mapping changed durable canonical authority")
				}
				requestID = id
				return nil
			}
			if err := r.requestRebuildCopy(ctx, archive.SourceAnchor{Key: key, OriginalSentAt: time.Unix(1000, 0)}); err != nil {
				t.Fatal(err)
			}
			if requestID == "" {
				t.Fatal("phone copy not requested")
			}
			web := historyRecord(key.Message, "LID_FRESH_PHONE_BODY").Message
			web.Key.RemoteJID = proto.String(expectedChat)
			web.Key.Participant = proto.String(expectedParticipant)
			web.Key.FromMe = proto.Bool(key.FromMe)
			response, err := r.client.ParseWebMessage(types.EmptyJID, web)
			if err != nil {
				t.Fatal(err)
			}
			response.UnavailableRequestID = requestID
			if err = r.message(ctx, response); err != nil {
				t.Fatal(err)
			}
			if len(s.events) != 1 || s.events[0].Key != key || s.events[0].RebuildRequestID != requestID || s.events[0].Body != "LID_FRESH_PHONE_BODY" {
				t.Fatal("returned LID response did not match registered canonical key")
			}
		})
	}
}
func TestRebuildPhoneCopyRejectsUnverifiedLIDBeforeRegistration(t *testing.T) {
	for _, scenario := range []string{"wrong_alias", "ad_lid", "wrong_server", "lookup_error", "missing_store"} {
		t.Run(scenario, func(t *testing.T) {
			r, s := rebuildFixture()
			r.status.Connection = "connected"
			peerPN := types.NewJID("200", types.DefaultUserServer)
			peerLID := types.NewJID("900", types.HiddenUserServer)
			lids := &voiceRedeliveryLIDStore{mappings: map[types.JID]types.JID{peerPN: peerLID}}
			r.client.Store.LIDs = lids
			s.canonical = map[string]string{peerLID.String(): peerPN.String()}
			switch scenario {
			case "wrong_alias":
				s.canonical[peerLID.String()] = "400@s.whatsapp.net"
			case "ad_lid":
				peerLID.Device = 7
				lids.mappings[peerPN] = peerLID
			case "wrong_server":
				lids.mappings[peerPN] = peerPN
			case "lookup_error":
				lids.err = errors.New("synthetic lookup error")
			case "missing_store":
				r.client.Store.LIDs = nil
			}
			sends := 0
			r.sendRebuildRequest = func(context.Context, string, *waE2E.Message) error { sends++; return nil }
			key := archive.SourceKey{Account: r.SourceAccount(), Chat: peerPN.String(), Sender: peerPN.String(), Message: "lid-source"}
			if err := r.requestRebuildCopy(context.Background(), archive.SourceAnchor{Key: key, OriginalSentAt: time.Unix(1000, 0)}); err == nil {
				t.Fatal("unverified transport accepted")
			}
			if sends != 0 || len(s.requests) != 0 {
				t.Fatal("unverified transport reached network or durable request ledger")
			}
		})
	}
}

func TestRebuildUnmatchedRestrictionSurvivesBodyGate(t *testing.T) {
	for _, route := range []string{"delayed_live", "unmatched_response", "history"} {
		for _, kind := range []string{"expiry", "view_once", "inline_view_once"} {
			t.Run(route+"_"+kind, func(t *testing.T) {
				r, s := rebuildFixture()
				ctx := context.Background()
				event := liveMessage("restriction-source", "RESTRICTED_BODY_CANARY")
				web := historyRecord(event.Info.ID, "RESTRICTED_BODY_CANARY").Message
				expiry := time.Now().Add(time.Hour).Truncate(time.Second)
				if kind == "expiry" {
					web.EphemeralExpirationTimestamp = proto.Uint64(uint64(expiry.Unix()))
				} else if kind == "inline_view_once" {
					web.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{ViewOnce: proto.Bool(true), Caption: proto.String("RESTRICTED_BODY_CANARY")}}
					event.Message = web.Message
				} else {
					web.Message = &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: web.Message}}
					event.IsViewOnce = true
				}
				event.SourceWebMsg = web
				if route == "unmatched_response" {
					event.UnavailableRequestID = "not-pending"
				}
				var err error
				if route == "history" {
					record := historyRecord(event.Info.ID, "")
					record.Message = web
					err = r.projectHistory(ctx, historyWire(record))
				} else {
					err = r.message(ctx, event)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(s.events) != 0 {
					t.Fatal("unmatched restricted body reached writer")
				}
				if len(s.restrictions) == 0 {
					t.Fatal("unmatched body gate discarded stricter source restriction")
				}
				last := s.restrictions[len(s.restrictions)-1]
				if last.key.Message != event.Info.ID || last.key.Account != r.SourceAccount() || kind == "expiry" && !last.expires.Equal(expiry) || kind != "expiry" && !last.viewOnce {
					t.Fatal("exact source restriction not preserved")
				}
			})
		}
	}
}
func TestRebuildHistoryEditPreservesRestrictionWithoutBody(t *testing.T) {
	r, s := rebuildFixture()
	ctx := context.Background()
	event := mutation("edit-target", waE2E.ProtocolMessage_MESSAGE_EDIT)
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	record := historyRecord("edit-control", "")
	record.Message.Message = event.Message
	record.Message.EphemeralExpirationTimestamp = proto.Uint64(uint64(expiry.Unix()))
	if err := r.projectHistory(ctx, historyWire(record)); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 || s.events[0].Kind != archive.EventEditInvalidation || s.events[0].Body != "" {
		t.Fatal("history edit negative effect lost or positive body admitted")
	}
	if len(s.restrictions) == 0 || s.restrictions[len(s.restrictions)-1].key.Message != "edit-target" || !s.restrictions[len(s.restrictions)-1].expires.Equal(expiry) {
		t.Fatal("history edit stricter expiry discarded")
	}
}

func TestRebuildRestrictionFailureFences(t *testing.T) {
	r, s := rebuildFixture()
	s.restrictionErr = archive.ErrArchiveUnavailable
	e := liveMessage("restriction-failure", "BODY_CANARY")
	e.IsViewOnce = true
	if err := r.message(context.Background(), e); err == nil || !r.Status().ReconciliationRequired || len(s.events) != 0 {
		t.Fatal("restriction commit failure did not fence source")
	}
}
