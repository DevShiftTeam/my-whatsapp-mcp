package ingestion

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const syntheticUnknownStub = int32(987654)

type unknownHistoryStore struct {
	*recordingStore
	gaps   []archive.HistoryUncertaintyReason
	gapErr error
}

func (s *unknownHistoryStore) RecordHistoryUncertainty(_ context.Context, account string, reason archive.HistoryUncertaintyReason, _ archive.ObservationID) error {
	if account != "100@s.whatsapp.net" {
		return archive.ErrInvalidEvent
	}
	if s.gapErr != nil {
		return s.gapErr
	}
	s.gaps = append(s.gaps, reason)
	s.order = append(s.order, "unknown_gap")
	return nil
}
func unknownHistoryFixture(policy UnknownHistoryPolicy) (*Runtime, *unknownHistoryStore) {
	s := &unknownHistoryStore{recordingStore: fixtureStore()}
	r := testRuntime(s.recordingStore)
	r.store = s
	r.config.UnknownHistoryPolicy = policy
	return r, s
}
func unknownHistoryRecord() *waHistorySync.HistorySyncMsg {
	m := bodylessHistoryStub("300@g.us", "unknown-source-canary", waWeb.WebMessageInfo_StubType(syntheticUnknownStub))
	m.Message.Participant = nil
	return m
}
func unknownHistoryChunk(record *waHistorySync.HistorySyncMsg, tail ...*waHistorySync.HistorySyncMsg) *waHistorySync.HistorySync {
	return &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String("300@g.us"), Messages: append([]*waHistorySync.HistorySyncMsg{record}, tail...)}}}
}
func unknownHistoryWire(t *testing.T, chunk *waHistorySync.HistorySync) []byte {
	t.Helper()
	b, err := (proto.MarshalOptions{AllowPartial: true}).Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func addUnknownHistoryField(m proto.Message) {
	b := protowire.AppendTag(nil, 19000, protowire.VarintType)
	m.ProtoReflect().SetUnknown(protowire.AppendVarint(b, 0))
}
func TestUnknownHistoryStrictDefaultAndInvalidPolicy(t *testing.T) {
	for _, policy := range []UnknownHistoryPolicy{UnknownHistoryStrict, UnknownHistoryPolicy(2)} {
		r, s := unknownHistoryFixture(policy)
		if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord()))); err == nil || !r.Status().ReconciliationRequired {
			t.Fatal("strict or invalid policy accepted unknown record")
		}
		if len(s.gaps) != 0 || len(s.events) != 0 {
			t.Fatal("strict policy created gap or source")
		}
	}
	if _, err := New(Config{UnknownHistoryPolicy: UnknownHistoryPolicy(2)}, fixtureStore(), Callbacks{}); !errors.Is(err, archive.ErrInvalidEvent) {
		t.Fatalf("invalid policy was not rejected before resource setup: %v", err)
	}
}
func TestUnknownHistoryPartialCommitsGapBeforeBodyAndControls(t *testing.T) {
	r, s := unknownHistoryFixture(UnknownHistoryPartial)
	body := bodylessHistoryRecord("300@g.us", "following-body", "followingbodyreceipt")
	revoke := bodylessHistoryRecord("300@g.us", "following-revoke", "")
	revoke.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("revoke-target"), RemoteJID: proto.String("300@g.us"), Participant: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}}}
	edit := bodylessHistoryRecord("300@g.us", "following-edit", "")
	edit.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("edit-target"), RemoteJID: proto.String("300@g.us"), Participant: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}, TimestampMS: proto.Int64(2000123), EditedMessage: &waE2E.Message{Conversation: proto.String("followingeditreceipt")}}}
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord(), body, revoke, edit))); err != nil {
		t.Fatalf("partial history stopped valid suffix: %v", err)
	}
	if r.Status().ReconciliationRequired || len(s.gaps) != 1 || s.gaps[0] != archive.HistoryUncertaintyUnsupportedControl || len(s.order) == 0 || s.order[0] != "unknown_gap" {
		t.Fatal("gap was not committed before suffix")
	}
	kinds := map[archive.SourceEventKind]bool{}
	for _, e := range s.events {
		if e.Key.Message == "unknown-source-canary" {
			t.Fatal("unknown record created source authority")
		}
		kinds[e.Kind] = true
	}
	if !kinds[archive.EventOriginal] || !kinds[archive.EventDelete] || !kinds[archive.EventEditInvalidation] {
		t.Fatalf("suffix controls lost: %v", kinds)
	}
	if len(s.aliases) != 0 || len(s.names) != 0 {
		t.Fatal("unknown record fabricated identity")
	}
}
func TestUnknownHistoryPartialGapFailureFences(t *testing.T) {
	for _, missing := range []bool{false, true} {
		r, s := unknownHistoryFixture(UnknownHistoryPartial)
		if missing {
			r.store = s.recordingStore
		} else {
			s.gapErr = errors.New("synthetic durable gap failure")
		}
		tail := bodylessHistoryRecord("300@g.us", "blocked-tail", "mustnotadmitreceipt")
		if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord(), tail))); err == nil || !r.Status().ReconciliationRequired {
			t.Fatal("uncommitted gap did not fence")
		}
		if len(s.events) != 0 || len(s.gaps) != 0 {
			t.Fatal("failed gap allowed source")
		}
	}
}
func TestUnknownHistoryPartialSeparatesUnsupportedMetadataFromBodyCorruption(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*waHistorySync.HistorySync)
	}{
		{"wrapper_unknown", func(c *waHistorySync.HistorySync) { addUnknownHistoryField(c.Conversations[0].Messages[0]) }},
		{"record_unknown", func(c *waHistorySync.HistorySync) { addUnknownHistoryField(c.Conversations[0].Messages[0].Message) }},
		{"key_unknown", func(c *waHistorySync.HistorySync) { addUnknownHistoryField(c.Conversations[0].Messages[0].Message.Key) }},
		{"conversation_unknown", func(c *waHistorySync.HistorySync) { addUnknownHistoryField(c.Conversations[0]) }},
		{"chunk_unknown", func(c *waHistorySync.HistorySync) { addUnknownHistoryField(c) }},
		{"conversation_expiry_zero", func(c *waHistorySync.HistorySync) { c.Conversations[0].EphemeralExpiration = proto.Uint32(0) }},
		{"conversation_retention_zero", func(c *waHistorySync.HistorySync) { c.Conversations[0].AfterReadDuration = proto.Uint32(0) }},
		{"conversation_identity", func(c *waHistorySync.HistorySync) { c.Conversations[0].NewJID = proto.String("999@s.whatsapp.net") }},
		{"body", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.Message = &waE2E.Message{Conversation: proto.String("unknown-body-canary")}
		}},
		{"parameters", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.MessageStubParameters = []string{"parameter-canary"}
		}},
		{"missing_direction", func(c *waHistorySync.HistorySync) { c.Conversations[0].Messages[0].Message.Key.FromMe = nil }},
		{"missing_remote", func(c *waHistorySync.HistorySync) { c.Conversations[0].Messages[0].Message.Key.RemoteJID = nil }},
		{"wrong_remote", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.Key.RemoteJID = proto.String("301@g.us")
		}},
		{"missing_key", func(c *waHistorySync.HistorySync) { c.Conversations[0].Messages[0].Message.Key = nil }},
		{"empty_id", func(c *waHistorySync.HistorySync) { c.Conversations[0].Messages[0].Message.Key.ID = proto.String("") }},
		{"bad_actor", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.Participant = proto.String("@s.whatsapp.net")
		}},
		{"empty_present_actor", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.Participant = proto.String("")
		}},
		{"group_actor", func(c *waHistorySync.HistorySync) {
			c.Conversations[0].Messages[0].Message.Participant = proto.String("301@g.us")
		}},
		{"conflicting_actor", func(c *waHistorySync.HistorySync) {
			m := c.Conversations[0].Messages[0].Message
			m.Participant = proto.String("200@s.whatsapp.net")
			m.Key.Participant = proto.String("999@s.whatsapp.net")
		}},
		{"outgoing_wrong_actor", func(c *waHistorySync.HistorySync) {
			m := c.Conversations[0].Messages[0].Message
			m.Key.FromMe = proto.Bool(true)
			m.Participant = proto.String("200@s.whatsapp.net")
		}},
	}
	for _, name := range []string{"EphemeralStartTimestamp", "EphemeralDuration", "EphemeralExpirationTimestamp", "EphemeralOffToOn", "EphemeralOutOfSync", "KeepInChat", "FutureproofData", "TargetMessageID", "MessageAddOns", "QuarantinedMessage", "GroupHistoryBundleInfo", "ClearMedia", "RevokeMessageTimestamp", "PinInChat", "ScheduledMessageMetadata", "OriginalSelfAuthorUserJIDString"} {
		cases = append(cases, struct {
			name   string
			mutate func(*waHistorySync.HistorySync)
		}{name, func(c *waHistorySync.HistorySync) {
			v := reflect.ValueOf(c.Conversations[0].Messages[0].Message).Elem().FieldByName(name)
			switch v.Kind() {
			case reflect.Pointer:
				v.Set(reflect.New(v.Type().Elem()))
			case reflect.Slice:
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			default:
				t.Fatal("invalid fixture field")
			}
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, s := unknownHistoryFixture(UnknownHistoryPartial)
			chunk := unknownHistoryChunk(unknownHistoryRecord(), bodylessHistoryRecord("300@g.us", "blocked-tail", "mustnotadmitreceipt"))
			tc.mutate(chunk)
			err := r.projectHistory(context.Background(), unknownHistoryWire(t, chunk))
			hard := tc.name == "body" || tc.name == "missing_key"
			if hard {
				if err == nil || !r.Status().ReconciliationRequired || len(s.events) != 0 || len(s.gaps) != 0 {
					t.Fatal("malformed body/required wrapper escaped strict fence")
				}
			} else {
				if err != nil || r.Status().ReconciliationRequired || len(s.gaps) == 0 || len(s.events) != 1 {
					t.Fatalf("unsupported noncontent failed uncertainty-only handling: %v", err)
				}
				if s.events[0].Key.Message != "blocked-tail" {
					t.Fatal("unsupported metadata created a source")
				}
			}
			if len(s.aliases) != 0 {
				t.Fatal("unsupported metadata manufactured alias")
			}
		})
	}
}
func TestUnknownHistoryPartialUnsupportedKindsDoNotWaiveMalformedSuffix(t *testing.T) {
	for _, value := range []int32{0, -1, 3, 37, 43, 72, 149, 186, 203, 208, 213, 216, 220, 221, 248, 250} {
		t.Run(waWeb.WebMessageInfo_StubType(value).String(), func(t *testing.T) {
			r, s := unknownHistoryFixture(UnknownHistoryPartial)
			m := unknownHistoryRecord()
			m.Message.MessageStubType = waWeb.WebMessageInfo_StubType(value).Enum()
			if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(m))); err != nil || r.Status().ReconciliationRequired {
				t.Fatalf("unimplemented noncontent should record generic uncertainty: %v", err)
			}
			if len(s.gaps) != 1 || len(s.events) != 0 {
				t.Fatal("unsupported kind granted authority or lost uncertainty")
			}
		})
	}
	r, s := unknownHistoryFixture(UnknownHistoryPartial)
	malformed := bodylessHistoryRecord("300@g.us", "malformed-edit", "")
	malformed.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum()}}
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord(), malformed))); err == nil || !r.Status().ReconciliationRequired {
		t.Fatal("malformed known edit bypassed fence")
	}
	if len(s.gaps) != 1 {
		t.Fatal("preceding durable gap missing")
	}
}

func TestUnknownHistoryStrictKeepsExistingBodyAndContextRouting(t *testing.T) {
	r, s := unknownHistoryFixture(UnknownHistoryStrict)
	record := unknownHistoryRecord()
	record.Message.Participant = proto.String("200@s.whatsapp.net")
	record.Message.Message = &waE2E.Message{Conversation: proto.String("strictordinaryreceipt")}
	chunk := unknownHistoryChunk(record)
	chunk.Conversations[0].EphemeralExpiration = proto.Uint32(0)
	addUnknownHistoryField(chunk)
	addUnknownHistoryField(record)
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, chunk)); err != nil {
		t.Fatalf("strict ordinary body routing changed: %v", err)
	}
	if len(s.events) != 1 || len(s.gaps) != 0 || r.Status().ReconciliationRequired {
		t.Fatal("partial-only context restriction affected strict routing")
	}
}

func TestUnknownHistoryPartialDirectProjectionRemainsStrict(t *testing.T) {
	r, s := unknownHistoryFixture(UnknownHistoryPartial)
	if err := r.projectBodylessHistory(context.Background(), types.NewJID("300", types.GroupServer), unknownHistoryRecord().Message); err == nil {
		t.Fatal("direct source projection used incomplete history context")
	}
	if len(s.gaps) != 0 || len(s.events) != 0 {
		t.Fatal("direct source projection gained unknown-gap authority")
	}
}
func TestUnknownHistoryPartialDuplicateClaimsRejectBeforeGap(t *testing.T) {
	encoded := func(number protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), value)
	}
	for _, field := range []string{"key", "web", "wrapper", "conversation", "history"} {
		t.Run(field, func(t *testing.T) {
			m := unknownHistoryRecord().Message
			key, err := proto.Marshal(m.Key)
			if err != nil {
				t.Fatal(err)
			}
			if field == "key" {
				key = append(key, encoded(1, []byte("300@g.us"))...)
			}
			web := encoded(1, key)
			web = append(web, protowire.AppendVarint(protowire.AppendTag(nil, 24, protowire.VarintType), uint64(syntheticUnknownStub))...)
			if field == "web" {
				web = append(web, encoded(5, []byte("999@s.whatsapp.net"))...)
				web = append(web, encoded(5, []byte("200@s.whatsapp.net"))...)
			}
			wrapper := encoded(1, web)
			if field == "wrapper" {
				wrapper = append(wrapper, encoded(1, web)...)
			}
			conversation := encoded(1, []byte("300@g.us"))
			conversation = append(conversation, encoded(2, wrapper)...)
			if field == "conversation" {
				conversation = append(conversation, encoded(13, []byte("first"))...)
				conversation = append(conversation, encoded(13, []byte("second"))...)
			}
			raw := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 3)
			raw = append(raw, encoded(2, conversation)...)
			if field == "history" {
				raw = append(raw, protowire.AppendVarint(protowire.AppendTag(nil, 5, protowire.VarintType), 1)...)
				raw = append(raw, protowire.AppendVarint(protowire.AppendTag(nil, 5, protowire.VarintType), 2)...)
			}
			r, s := unknownHistoryFixture(UnknownHistoryPartial)
			if err := r.projectHistory(context.Background(), raw); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("duplicate singular claim accepted")
			}
			if len(s.gaps) != 0 || len(s.events) != 0 {
				t.Fatal("duplicate claim admitted gap or source")
			}
		})
	}
}
func TestUnknownHistoryPartialDurableSuffixAndNoNoticeSource(t *testing.T) {
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	r.config.UnknownHistoryPolicy = UnknownHistoryPartial
	ctx := context.Background()
	var observationErr error
	r.observation, observationErr = store.BeginObservation(ctx, durableAccount)
	if observationErr != nil {
		t.Fatal(observationErr)
	}
	original := bodylessHistoryRecord("300@g.us", "durable-target", "unknownoldreceipt")
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, "300@g.us", original)); err != nil {
		t.Fatal(err)
	}
	revoke := bodylessHistoryRecord("300@g.us", "durable-revoke", "")
	revoke.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("durable-target"), RemoteJID: proto.String("300@g.us"), FromMe: proto.Bool(false), Participant: proto.String("200@s.whatsapp.net")}}}
	tail := bodylessHistoryRecord("300@g.us", "durable-tail", "unknowntailreceipt")
	if err := r.projectHistory(ctx, unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord(), tail, revoke))); err != nil {
		t.Fatal(err)
	}
	if r.Status().ReconciliationRequired {
		t.Fatal("eligible partial history fenced")
	}
	queueBurstAssertSearch(t, store, "unknownoldreceipt", 0)
	queueBurstAssertSearch(t, store, "unknowntailreceipt", 1)
	meta, err := store.SourceMetadata(ctx, bodylessHistoryKey("300@g.us", "unknown-source-canary"))
	if err != nil || meta.Version.MessageID != "" || meta.OriginalDateKnown {
		t.Fatal("unknown notice fabricated source metadata")
	}
	status := bodylessHistoryStatus(t, store)
	if !status.Snapshot.UnknownHistoryGap || status.Coverage != "partial" {
		t.Fatal("durable partial marker missing")
	}
	anchors, err := store.BackfillAnchors(ctx, durableAccount, 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range anchors {
		if a.Key.Message == "unknown-source-canary" {
			t.Fatal("unknown notice fabricated cursor")
		}
	}
	durableAssertAbsent(t, dir, "unknown-source-canary")
}

func TestUnknownHistoryPartialRejectsNormalizedActionableControlClaims(t *testing.T) {
	for _, location := range []string{"conversation", "remote", "key_actor", "web_actor"} {
		for _, form := range []string{"device_zero", "device", "device_overflow", "agent_overflow", "extra_at", "leading_space", "internal_space", "control"} {
			t.Run(location+"/"+form, func(t *testing.T) {
				user, server := "300", "g.us"
				if location == "key_actor" || location == "web_actor" {
					user, server = "200", "s.whatsapp.net"
				}
				claim := user + "@" + server
				switch form {
				case "device_zero":
					claim = user + ":0@" + server
				case "device":
					claim = user + ":1@" + server
				case "device_overflow":
					claim = user + ":65536@" + server
				case "agent_overflow":
					claim = user + ".256@" + server
				case "extra_at":
					claim += "@ignored.example"
				case "leading_space":
					claim = " " + claim
				case "internal_space":
					claim = user + " @" + server
				case "control":
					claim = user + "\n@" + server
				}
				r, s := unknownHistoryFixture(UnknownHistoryPartial)
				chunk := unknownHistoryChunk(bodylessHistoryStub("300@g.us", "known-delete", waWeb.WebMessageInfo_REVOKE))
				m := chunk.Conversations[0].Messages[0].Message
				switch location {
				case "conversation":
					chunk.Conversations[0].ID = proto.String(claim)
					// A matching malformed remote must not make the pair authoritative.
					m.Key.RemoteJID = proto.String(claim)
				case "remote":
					m.Key.RemoteJID = proto.String(claim)
				case "key_actor":
					m.Key.Participant = proto.String(claim)
				case "web_actor":
					m.Participant = proto.String(claim)
				}
				if err := r.projectHistory(context.Background(), unknownHistoryWire(t, chunk)); err == nil || !r.Status().ReconciliationRequired {
					t.Fatal("malformed wire claim became authority after normalization")
				}
				if len(s.gaps) != 0 || len(s.events) != 0 || len(s.aliases) != 0 {
					t.Fatal("malformed wire claim admitted gap or source")
				}
			})
		}
	}
}

type canonicalUnknownHistoryStore struct{ *unknownHistoryStore }

func (s *canonicalUnknownHistoryStore) Canonical(_ context.Context, _ string, id string) (string, error) {
	if id == "900@lid" {
		return "100@s.whatsapp.net", nil
	}
	return id, nil
}

func TestUnknownHistoryPartialRejectsIncomingOwnControlActor(t *testing.T) {
	for _, claim := range []string{"100@s.whatsapp.net", "900@lid"} {
		for _, location := range []string{"key", "web", "both"} {
			t.Run(claim+"/"+location, func(t *testing.T) {
				r, s := unknownHistoryFixture(UnknownHistoryPartial)
				r.store = &canonicalUnknownHistoryStore{s}
				m := bodylessHistoryStub("300@g.us", "known-delete", waWeb.WebMessageInfo_REVOKE)
				if location == "key" || location == "both" {
					m.Message.Key.Participant = proto.String(claim)
				}
				if location == "web" || location == "both" {
					m.Message.Participant = proto.String(claim)
				}
				if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(m))); err == nil || !r.Status().ReconciliationRequired {
					t.Fatal("incoming group notice accepted explicit own-account actor")
				}
				if len(s.gaps) != 0 || len(s.events) != 0 || len(s.aliases) != 0 {
					t.Fatal("contradictory actor admitted gap or source")
				}
			})
		}
	}
}
