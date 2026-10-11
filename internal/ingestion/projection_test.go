package ingestion

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func liveMessage(id, body string) *events.Message {
	j := types.NewJID("200", types.DefaultUserServer)
	return &events.Message{Info: types.MessageInfo{ID: id, Timestamp: time.Unix(1000, 0), MessageSource: types.MessageSource{Chat: j, Sender: j}}, Message: &waE2E.Message{Conversation: proto.String(body)}}
}
func mutation(id string, kind waE2E.ProtocolMessage_Type) *events.Message {
	e := liveMessage("control", "")
	e.Info.Timestamp = time.Unix(2000, 0)
	e.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: kind.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id), RemoteJID: proto.String(e.Info.Chat.String()), FromMe: proto.Bool(true)}, EditedMessage: &waE2E.Message{Conversation: proto.String("edited")}, TimestampMS: proto.Int64(2000000)}}
	return e
}
func TestDateAndSelectiveFiltersRemoveBodyBeforeStore(t *testing.T) {
	for _, p := range []archive.IngestionPolicy{{Generation: 1, DateMode: archive.DateSince, ImportStart: time.Unix(1100, 0), ChatMode: archive.ChatsAll}, {Generation: 1, DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}} {
		s := fixtureStore()
		s.policy = p
		r := testRuntime(s)
		if err := r.message(context.Background(), liveMessage("excluded", "CANARY_EXCLUDED")); err != nil {
			t.Fatal(err)
		}
		if len(s.events) != 1 || s.events[0].Body != "" || s.events[0].Caption != "" {
			t.Fatalf("excluded plaintext reached writer: %+v", s.events)
		}
		if !s.events[0].OriginalDateKnown {
			t.Fatal("filtered metadata anchor lost")
		}
	}
}
func TestMutationPayloadCannotOverrideEnvelopeAuthority(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.message(context.Background(), liveMessage("target", "original")); err != nil {
		t.Fatal(err)
	}
	e := mutation("target", waE2E.ProtocolMessage_MESSAGE_EDIT)
	e.Message.ProtocolMessage.Key.Participant = proto.String("999@s.whatsapp.net")
	if _, _, err := r.trustedMutation(context.Background(), e); !errors.Is(err, archive.ErrInvalidEvent) {
		t.Fatalf("forged participant accepted: %v", err)
	}
	e.Message.ProtocolMessage.Key.Participant = nil
	e.Message.ProtocolMessage.Key.RemoteJID = proto.String("999@s.whatsapp.net")
	if _, _, err := r.trustedMutation(context.Background(), e); !errors.Is(err, archive.ErrInvalidEvent) {
		t.Fatalf("forged chat accepted: %v", err)
	}
	e.Message.ProtocolMessage.Key.RemoteJID = proto.String(e.Info.Chat.String())
	projected, _, err := r.trustedMutation(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Key.FromMe || projected.Key.Sender != "200@s.whatsapp.net" {
		t.Fatal("payload fromMe overrode local envelope")
	}
}
func TestEditUsesOriginalDateAndMissingMetadataOnlyInvalidates(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.message(context.Background(), liveMessage("target", "original")); err != nil {
		t.Fatal(err)
	}
	if err := r.message(context.Background(), mutation("target", waE2E.ProtocolMessage_MESSAGE_EDIT)); err != nil {
		t.Fatal(err)
	}
	last := s.events[len(s.events)-1]
	if last.Kind != archive.EventEditContent || !last.OriginalSentAt.Equal(time.Unix(1000, 0)) || last.SourceRevision != 2000000 {
		t.Fatalf("edit conflated original date and revision: %+v", last)
	}
	before := len(s.events)
	if err := r.message(context.Background(), mutation("missing", waE2E.ProtocolMessage_MESSAGE_EDIT)); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != before+1 || s.events[len(s.events)-1].Kind != archive.EventEditInvalidation || s.events[len(s.events)-1].Body != "" {
		t.Fatal("unknown original metadata admitted edited body")
	}
}
func TestUnsupportedAndUnavailableMediaPreserveReceipt(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := liveMessage("image", "")
	e.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("caption"), URL: proto.String("https://arbitrary.invalid/secret"), MediaKey: make([]byte, 33)}}
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := s.events[len(s.events)-1]
	if got.BodyKind != archive.MessageImage || got.Caption != "caption" || len(got.Media) != 1 || got.Media[0].Availability != "unavailable" || len(got.Media[0].ProtocolReference) != 0 {
		t.Fatalf("bad media receipt: %+v", got)
	}
	e = liveMessage("unsupported", "")
	e.Message = &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if s.events[len(s.events)-1].BodyKind != archive.MessageUnsupported {
		t.Fatal("unsupported kind became text")
	}
}
func TestEphemeralTrustedExpiryPermitsCurrentBodyAndNeverExtends(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := liveMessage("ephemeral", "")
	e.Info.Timestamp = time.Now().Truncate(time.Second)
	e.IsEphemeral = true
	e.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("current disappearing text"), ContextInfo: &waE2E.ContextInfo{Expiration: proto.Uint32(60)}}}
	earliest := e.Info.Timestamp.Add(20 * time.Second)
	e.SourceWebMsg = &waWeb.WebMessageInfo{EphemeralStartTimestamp: proto.Uint64(uint64(e.Info.Timestamp.Unix())), EphemeralDuration: proto.Uint32(30), EphemeralExpirationTimestamp: proto.Uint64(uint64(earliest.Unix()))}
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := s.events[len(s.events)-1]
	if got.Body != "current disappearing text" || !got.ExpiresAt.Equal(earliest) {
		t.Fatalf("current disappearing content/earliest expiry lost: %+v", got)
	}
	key := got.Key
	meta := s.metadata[key]
	meta.Version.ExpiresAt = earliest
	s.metadata[key] = meta
	e.SourceWebMsg.EphemeralExpirationTimestamp = proto.Uint64(uint64(earliest.Add(time.Hour).Unix()))
	e.Message.ExtendedTextMessage.ContextInfo.Expiration = proto.Uint32(3600)
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if s.events[len(s.events)-1].ExpiresAt.After(earliest) {
		t.Fatal("replay extended existing source expiry")
	}
}
func TestEphemeralUnknownExpiryDeniesBodies(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := liveMessage("ephemeral", "must not remain searchable")
	e.IsEphemeral = true
	if err := r.message(context.Background(), e); !errors.Is(err, archive.ErrMutationMetadataMissing) {
		t.Fatalf("unknown disappearing expiry accepted: %v", err)
	}
	if len(s.events) != 0 || len(s.unsafe) == 0 {
		t.Fatal("unknown expiry failed open")
	}
}
func TestTrustedDisplayNamesAreBoundedAndDoNotChangeSourceKeys(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := liveMessage("named", "text")
	e.Info.PushName = "דנה"
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(s.names) != 1 || s.names[0].SenderName != "דנה" || s.names[0].ChatName != "דנה" {
		t.Fatalf("usable source display metadata missing: %+v", s.names)
	}
	if s.events[0].Key.Sender != "200@s.whatsapp.net" {
		t.Fatal("display name selected identity")
	}
}

func TestOwnDirectionCannotClaimDifferentAccountSender(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := liveMessage("claim", "body")
	e.Info.IsFromMe = true
	if err := r.message(context.Background(), e); !errors.Is(err, archive.ErrInvalidEvent) {
		t.Fatalf("own direction accepted another account author: %v", err)
	}
	if len(s.events) != 0 {
		t.Fatal("cross-account own sender persisted")
	}
}
func TestNewsletterEditUsesOriginalTimestampForDatePolicy(t *testing.T) {
	s := fixtureStore()
	s.policy = archive.IngestionPolicy{Generation: 1, DateMode: archive.DateSince, ImportStart: time.Unix(1500, 0), ChatMode: archive.ChatsAll}
	r := testRuntime(s)
	e := liveMessage("edit", "excluded newsletter body")
	e.Info.Timestamp = time.Unix(2000, 0)
	e.NewsletterMeta = &events.NewsletterMessageMeta{OriginalTS: time.Unix(1000, 0), EditTS: time.Unix(2000, 123000000)}
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, out := range s.events {
		if out.Body != "" {
			t.Fatal("newsletter edit date bypassed old original policy")
		}
		if !out.OriginalSentAt.Equal(time.Unix(1000, 0)) || out.SourceRevision != 2000123 {
			t.Fatal("newsletter edit metadata lost original date/order")
		}
	}
}

func TestLiveEditRejectsAmbiguousEditedBodyStructure(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	_ = r.message(context.Background(), liveMessage("target", "original"))
	e := mutation("target", waE2E.ProtocolMessage_MESSAGE_EDIT)
	e.Message.ProtocolMessage.EditedMessage = &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: &waE2E.Message{Conversation: proto.String("hidden")}}, EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: proto.String("different")}}}
	if err := r.message(context.Background(), e); err == nil {
		t.Fatal("ambiguous edit body accepted")
	}
	if len(s.unsafe) == 0 {
		t.Fatal("invalid edited graph did not deny source reads")
	}
}
