package ingestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

type controlFirstStore struct {
	*recordingStore
	reasons      []archive.HistoryUncertaintyReason
	gapErr       error
	observations []archive.ObservationID
}

func (s *controlFirstStore) RecordHistoryUncertainty(_ context.Context, account string, reason archive.HistoryUncertaintyReason, observation archive.ObservationID) error {
	if account != "100@s.whatsapp.net" {
		return archive.ErrInvalidEvent
	}
	if s.gapErr != nil {
		return s.gapErr
	}
	s.reasons = append(s.reasons, reason)
	s.observations = append(s.observations, observation)
	s.order = append(s.order, "uncertainty")
	return nil
}
func controlFirstFixture() (*Runtime, *controlFirstStore) {
	s := &controlFirstStore{recordingStore: fixtureStore()}
	r := testRuntime(s.recordingStore)
	r.store = s
	r.config.UnknownHistoryPolicy = UnknownHistoryPartial
	return r, s
}
func controlFirstTail() []*waHistorySync.HistorySyncMsg {
	body := bodylessHistoryRecord("300@g.us", "following-body", "followingbodyreceipt")
	revoke := bodylessHistoryRecord("300@g.us", "following-revoke", "")
	revoke.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("revoke-target"), RemoteJID: proto.String("300@g.us"), Participant: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}}}
	edit := bodylessHistoryRecord("300@g.us", "following-edit", "")
	edit.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("edit-target"), RemoteJID: proto.String("300@g.us"), Participant: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}, TimestampMS: proto.Int64(2000123), EditedMessage: &waE2E.Message{Conversation: proto.String("followingeditreceipt")}}}
	return []*waHistorySync.HistorySyncMsg{body, revoke, edit}
}
func TestControlFirstPartialMetadataAndNoticeContinueStrictSuffix(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*waHistorySync.HistorySync)
	}{
		{"unknown_notice", func(*waHistorySync.HistorySync) {}},
		{"starred_false", func(c *waHistorySync.HistorySync) { c.Conversations[0].Messages[0].Message.Starred = proto.Bool(false) }},
		{"starred_true", func(c *waHistorySync.HistorySync) { c.Conversations[0].Messages[0].Message.Starred = proto.Bool(true) }},
		{"parent_extra", func(c *waHistorySync.HistorySync) { addUnknownHistoryField(c) }},
		{"conversation_extra", func(c *waHistorySync.HistorySync) { addUnknownHistoryField(c.Conversations[0]) }},
		{"known_unsupported_notice", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.MessageStubType = waWeb.WebMessageInfo_StubType(37).Enum()
		}},
		{"known_unsupported_control", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.MessageStubType = waWeb.WebMessageInfo_StubType(72).Enum()
		}},
		{"notice_parameters", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.MessageStubParameters = []string{"never-an-alias-canary"}
		}},
		{"unused_invalid_actor", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.Participant = proto.String("invalid-unused-actor-canary")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, s := controlFirstFixture()
			chunk := unknownHistoryChunk(unknownHistoryRecord(), controlFirstTail()...)
			tc.mutate(chunk)
			if err := r.projectHistory(context.Background(), unknownHistoryWire(t, chunk)); err != nil {
				t.Fatalf("unsupported metadata stopped strict suffix: %v", err)
			}
			if len(s.reasons) == 0 || len(s.order) == 0 || s.order[0] != "uncertainty" || r.Status().ReconciliationRequired {
				t.Fatal("missing durable uncertainty before suffix")
			}
			kinds := map[archive.SourceEventKind]bool{}
			for _, e := range s.events {
				if e.Key.Message == "unknown-source-canary" {
					t.Fatal("discarded notice manufactured source")
				}
				kinds[e.Kind] = true
			}
			if !kinds[archive.EventOriginal] || !kinds[archive.EventDelete] || !kinds[archive.EventEditInvalidation] {
				t.Fatalf("strict suffix missing: %v", kinds)
			}
			if len(s.aliases) != 0 || len(s.names) != 0 {
				t.Fatal("discarded notice manufactured identity")
			}
		})
	}
}
func TestControlFirstPartialGroupNoticeActorClaimsBecomeGap(t *testing.T) {
	for _, kind := range []waWeb.WebMessageInfo_StubType{waWeb.WebMessageInfo_GROUP_CREATE, waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD} {
		for _, actors := range []string{"unmapped_lid", "direction_mismatch", "consistent"} {
			t.Run(kind.String()+"/"+actors, func(t *testing.T) {
				r, s := controlFirstFixture()
				notice := bodylessHistoryStub("300@g.us", "discarded-group-notice", kind)
				notice.Message.Key.Participant = proto.String("200@s.whatsapp.net")
				notice.Message.MessageStubParameters = []string{"discarded-notice-metadata"}
				switch actors {
				case "unmapped_lid":
					notice.Message.Participant = proto.String("900@lid")
				case "direction_mismatch":
					notice.Message.Key.FromMe = proto.Bool(true)
				}
				if err := r.projectHistory(t.Context(), unknownHistoryWire(t, unknownHistoryChunk(notice, controlFirstTail()...))); err != nil {
					t.Fatalf("bodyless group notice stopped partial import: %v", err)
				}
				if len(s.reasons) != 1 || s.reasons[0] != archive.HistoryUncertaintyUnsupportedNotice || len(s.order) == 0 || s.order[0] != "uncertainty" || r.Status().ReconciliationRequired {
					t.Fatal("group notice did not commit a gap before continuing")
				}
				kinds := map[archive.SourceEventKind]bool{}
				for _, event := range s.events {
					if event.Key.Message == "discarded-group-notice" {
						t.Fatal("discarded group notice manufactured a source event")
					}
					kinds[event.Kind] = true
				}
				if !kinds[archive.EventOriginal] || !kinds[archive.EventDelete] || !kinds[archive.EventEditInvalidation] || len(s.aliases) != 0 || len(s.names) != 0 {
					t.Fatal("notice changed identities or hid the following body and controls")
				}
			})
		}
	}
}

func TestControlFirstGroupNoticeCompatibilityKeepsSafetyChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strict bool
		mutate func(*waWeb.WebMessageInfo)
	}{
		{"strict_default", true, nil},
		{"different_notice", false, func(w *waWeb.WebMessageInfo) { w.MessageStubType = waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT.Enum() }},
		{"wrong_chat", false, func(w *waWeb.WebMessageInfo) { w.Key.RemoteJID = proto.String("999@g.us") }},
		{"invalid_actor", false, func(w *waWeb.WebMessageInfo) { w.Key.Participant = proto.String("@s.whatsapp.net") }},
		{"missing_id", false, func(w *waWeb.WebMessageInfo) { w.Key.ID = nil }},
		{"missing_direction", false, func(w *waWeb.WebMessageInfo) { w.Key.FromMe = nil }},
		{"missing_parameters", false, func(w *waWeb.WebMessageInfo) { w.MessageStubParameters = nil }},
		{"missing_timestamp", false, func(w *waWeb.WebMessageInfo) { w.MessageTimestamp = nil }},
		{"expiry", false, func(w *waWeb.WebMessageInfo) { w.EphemeralExpirationTimestamp = proto.Uint64(9000) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s := controlFirstFixture()
			if tc.strict {
				r.config.UnknownHistoryPolicy = UnknownHistoryStrict
			}
			notice := bodylessHistoryStub("300@g.us", "guarded-group-notice", waWeb.WebMessageInfo_GROUP_CREATE)
			notice.Message.Key.Participant = proto.String("900@lid")
			notice.Message.MessageStubParameters = []string{"discarded-notice-metadata"}
			if tc.mutate != nil {
				tc.mutate(notice.Message)
			}
			if err := r.projectHistory(t.Context(), unknownHistoryWire(t, unknownHistoryChunk(notice, controlFirstTail()[0]))); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("unverified group notice bypassed the source fence")
			}
			if len(s.events) != 0 || len(s.reasons) != 0 || len(s.aliases) != 0 || len(s.names) != 0 {
				t.Fatal("unverified group notice created state or admitted its suffix")
			}
		})
	}
}

func TestControlFirstGroupNoticeGapFailureStopsSuffix(t *testing.T) {
	r, s := controlFirstFixture()
	s.gapErr = errors.New("synthetic notice gap persistence failure")
	notice := bodylessHistoryStub("300@g.us", "uncommitted-group-notice", waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD)
	notice.Message.Key.Participant = proto.String("900@lid")
	notice.Message.MessageStubParameters = []string{"discarded-notice-metadata"}
	if err := r.projectHistory(t.Context(), unknownHistoryWire(t, unknownHistoryChunk(notice, controlFirstTail()[0]))); !errors.Is(err, s.gapErr) || !r.Status().ReconciliationRequired {
		t.Fatalf("gap failure did not stop import: %v", err)
	}
	if len(s.events) != 0 || len(s.reasons) != 0 || len(s.aliases) != 0 || len(s.names) != 0 {
		t.Fatal("failed gap admitted state")
	}
}

func TestControlFirstNegativePrecedesUncertainty(t *testing.T) {
	r, s := controlFirstFixture()
	revoke := bodylessHistoryStub("300@g.us", "revoked-source", waWeb.WebMessageInfo_REVOKE)
	addUnknownHistoryField(revoke.Message)
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(revoke, controlFirstTail()[0]))); err != nil {
		t.Fatal(err)
	}
	if len(s.events) < 2 || s.events[0].Kind != archive.EventDelete || len(s.reasons) != 1 || len(s.order) < 3 || s.order[0] != "apply" || s.order[1] != "uncertainty" || s.order[2] != "apply" {
		t.Fatalf("negative/gap/suffix ordering lost: %v", s.order)
	}
}
func TestControlFirstGapFailureStopsSuffixAndParentEffects(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "record", true: "parent"}[parent], func(t *testing.T) {
			r, s := controlFirstFixture()
			s.gapErr = errors.New("synthetic durable uncertainty failure")
			chunk := unknownHistoryChunk(unknownHistoryRecord(), controlFirstTail()[0])
			if parent {
				addUnknownHistoryField(chunk)
				chunk.PhoneNumberToLidMappings = []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String("200@s.whatsapp.net"), LidJID: proto.String("900@lid")}}
				chunk.Conversations[0].Name = proto.String("dependent-name-canary")
			}
			if err := r.projectHistory(context.Background(), unknownHistoryWire(t, chunk)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("failed uncertainty did not fence")
			}
			if len(s.events) != 0 || len(s.aliases) != 0 || len(s.names) != 0 || len(s.reasons) != 0 {
				t.Fatal("failed uncertainty admitted dependent state")
			}
		})
	}
}
func TestControlFirstMalformedActionableControlRemainsFatal(t *testing.T) {
	r, s := controlFirstFixture()
	bad := bodylessHistoryRecord("300@g.us", "invalid-edit", "")
	bad.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum()}}
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord(), bad, controlFirstTail()[0]))); err == nil || !r.Status().ReconciliationRequired {
		t.Fatal("malformed actionable edit bypassed fence")
	}
	if len(s.reasons) != 1 || len(s.events) != 0 {
		t.Fatal("known-control error swallowed or preceding gap missing")
	}
}
func TestControlFirstStrictDefaultDoesNotGainCompatibility(t *testing.T) {
	r, s := controlFirstFixture()
	r.config.UnknownHistoryPolicy = UnknownHistoryStrict
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord(), controlFirstTail()[0]))); err == nil || !r.Status().ReconciliationRequired {
		t.Fatal("strict default accepted unsupported notice")
	}
	if len(s.events) != 0 || len(s.reasons) != 0 {
		t.Fatal("strict default gained partial authority")
	}
}

type controlFirstDeliveredStore struct {
	*deliveredFixtureStore
	reasons []archive.HistoryUncertaintyReason
}

func (s *controlFirstDeliveredStore) RecordHistoryUncertainty(_ context.Context, _ string, reason archive.HistoryUncertaintyReason, _ archive.ObservationID) error {
	s.reasons = append(s.reasons, reason)
	s.order = append(s.order, "uncertainty")
	return nil
}
func TestControlFirstPausedExhaustedDeliveryPreservesNegativeSuffix(t *testing.T) {
	r, base := deliveredFixture(t)
	s := &controlFirstDeliveredStore{deliveredFixtureStore: base}
	r.store = s
	r.config.UnknownHistoryPolicy = UnknownHistoryPartial
	s.positiveLimit = 0
	s.progress.State = "paused"
	expiry := unknownHistoryRecord()
	expiry.Message.Key.ID = proto.String("restricted-unknown")
	expiry.Message.Participant = proto.String("200@s.whatsapp.net")
	expiration := time.Now().Add(time.Hour).Truncate(time.Second)
	expiry.Message.EphemeralExpirationTimestamp = proto.Uint64(uint64(expiration.Unix()))
	view := bodylessHistoryRecord("300@g.us", "following-view-once", "")
	view.Message.Message = &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("NEVER-RETAIN-VIEW-ONCE")}}}}
	rows := append([]*waHistorySync.HistorySyncMsg{unknownHistoryRecord(), expiry}, controlFirstTail()...)
	rows = append(rows, view)
	notification := deliveredNotification(r)
	notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(unknownHistoryWire(t, unknownHistoryChunk(rows[0], rows[1:]...)))
	if err := r.historyFrom(context.Background(), notification); err != nil {
		t.Fatal(err)
	}
	if len(s.reasons) < 2 || s.accepted != 0 {
		t.Fatal("paused exhausted positives lost gap or admitted body")
	}
	kinds := map[archive.SourceEventKind]bool{}
	for _, e := range s.events {
		kinds[e.Kind] = true
		if e.Kind == archive.EventOriginal || e.Kind == archive.EventEditContent {
			t.Fatal("exhausted positive admitted")
		}
	}
	if !kinds[archive.EventDelete] || !kinds[archive.EventEditInvalidation] {
		t.Fatal("paused/exhausted suffix lost known negatives")
	}
	var expirySeen, viewSeen bool
	for _, restriction := range s.restrictions {
		expirySeen = expirySeen || restriction.key.Message == "restricted-unknown" && restriction.expires.Equal(expiration)
		viewSeen = viewSeen || restriction.key.Message == "following-view-once" && restriction.viewOnce
	}
	if !expirySeen || !viewSeen {
		t.Fatal("expiry/view-once negative facts lost")
	}
	for _, a := range s.frontiers {
		if a.Key.Message == "unknown-source-canary" || a.Key.Message == "restricted-unknown" {
			t.Fatal("discarded notice manufactured frontier")
		}
	}
}
func TestControlFirstNegativePersistenceFailureCannotBecomeGap(t *testing.T) {
	r, base := deliveredFixture(t)
	s := &controlFirstDeliveredStore{deliveredFixtureStore: base}
	r.store = s
	r.config.UnknownHistoryPolicy = UnknownHistoryPartial
	s.restrictionErr = errors.New("synthetic restriction failure")
	m := unknownHistoryRecord()
	m.Message.Participant = proto.String("200@s.whatsapp.net")
	m.Message.EphemeralExpirationTimestamp = proto.Uint64(9000)
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(m, controlFirstTail()[0]))); err == nil || !r.Status().ReconciliationRequired {
		t.Fatal("failed understood negative became continuation")
	}
	if len(s.reasons) != 0 || len(s.events) != 0 {
		t.Fatal("negative failure demoted to gap or allowed suffix")
	}
}
