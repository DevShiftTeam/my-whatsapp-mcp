package ingestion

import (
	"context"
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestHistoryPinnedNoticeObservedShapeContinuesBodyRevokeAndEdit(t *testing.T) {
	// The durable helper asserts actual shape411, no retained parameter/source
	// canaries, no cursor, and correct following original/revoke/edit FTS effects.
	assertReviewedHistoryNoticeSuffix(t, waWeb.WebMessageInfo_PINNED_MESSAGE_IN_CHAT)
}

func TestHistoryPinnedNoticeCreatesNoAuthority(t *testing.T) {
	for _, policy := range []UnknownHistoryPolicy{UnknownHistoryStrict, UnknownHistoryPartial} {
		store := fixtureStore()
		r := testRuntime(store)
		r.config.UnknownHistoryPolicy = policy
		notice := bodylessHistoryStub("300@g.us", "pinned-notice-canary", waWeb.WebMessageInfo_PINNED_MESSAGE_IN_CHAT)
		notice.Message.Participant = nil
		notice.Message.MessageStubParameters = []string{"parameter-body-canary", "999@s.whatsapp.net", "999@lid"}
		if historyPresence(notice.Message) != 411 {
			t.Fatal("fixture does not match observed177/411")
		}
		if undeclaredHistoryStub(notice.Message) {
			t.Fatal("known pinned notice became unknown-partial authority")
		}
		if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", notice)); err != nil {
			t.Fatalf("known bodyless pinned notice fenced: %v", err)
		}
		if r.Status().ReconciliationRequired || len(store.events) != 0 || len(store.aliases) != 0 || len(store.names) != 0 || len(store.metadata) != 0 {
			t.Fatal("pinned display notice fabricated archive authority")
		}
	}
}

func TestHistoryPinnedNoticeSafetyAndClaimsRemainStrict(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*waWeb.WebMessageInfo)
	}{
		{"missing_key", func(m *waWeb.WebMessageInfo) { m.Key = nil }},
		{"empty_id", func(m *waWeb.WebMessageInfo) { m.Key.ID = proto.String("") }},
		{"wrong_remote", func(m *waWeb.WebMessageInfo) { m.Key.RemoteJID = proto.String("301@g.us") }},
		{"malformed_actor", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("@s.whatsapp.net") }},
		{"conflicting_actors", func(m *waWeb.WebMessageInfo) {
			m.Participant = proto.String("200@s.whatsapp.net")
			m.Key.Participant = proto.String("201@s.whatsapp.net")
		}},
	}
	for _, field := range []string{"EphemeralStartTimestamp", "EphemeralDuration", "EphemeralExpirationTimestamp", "EphemeralOffToOn", "EphemeralOutOfSync", "KeepInChat", "FutureproofData", "TargetMessageID", "MessageAddOns", "QuarantinedMessage", "GroupHistoryBundleInfo", "PinInChat", "RevokeMessageTimestamp", "ClearMedia"} {
		cases = append(cases, struct {
			name   string
			mutate func(*waWeb.WebMessageInfo)
		}{field, func(m *waWeb.WebMessageInfo) {
			v := reflect.ValueOf(m).Elem().FieldByName(field)
			switch v.Kind() {
			case reflect.Pointer:
				v.Set(reflect.New(v.Type().Elem()))
			case reflect.Slice:
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			default:
				t.Fatal("invalid safety fixture")
			}
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore()
			r := testRuntime(store)
			notice := bodylessHistoryStub("300@g.us", "unsafe-pinned-notice", waWeb.WebMessageInfo_PINNED_MESSAGE_IN_CHAT)
			notice.Message.Participant = nil
			notice.Message.MessageStubParameters = []string{"ignored-parameter"}
			tc.mutate(notice.Message)
			tail := bodylessHistoryRecord("300@g.us", "blocked-pin-tail", "mustnotadmitreceipt")
			if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(notice, tail))); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("pinned notice swallowed safety metadata or conflicting claims")
			}
			if len(store.events) != 0 || len(store.aliases) != 0 {
				t.Fatal("unsafe pinned notice admitted source authority")
			}
		})
	}
}

func TestHistoryPinnedNoticeDoesNotSkipPinControlBody(t *testing.T) {
	store := fixtureStore()
	r := testRuntime(store)
	notice := bodylessHistoryStub("300@g.us", "pin-control", waWeb.WebMessageInfo_PINNED_MESSAGE_IN_CHAT)
	notice.Message.Message = &waE2E.Message{PinInChatMessage: &waE2E.PinInChatMessage{}}
	if err := r.projectBodylessHistory(context.Background(), types.NewJID("300", types.GroupServer), notice.Message); err == nil {
		t.Fatal("bodyless display path swallowed actual pin control body")
	}
	if len(store.events) != 0 || len(store.aliases) != 0 {
		t.Fatal("display skip fabricated control authority")
	}
}
