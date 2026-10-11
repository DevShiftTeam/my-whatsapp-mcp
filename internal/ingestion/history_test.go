package ingestion

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"
)

func historyRecord(id, body string) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(id), RemoteJID: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}, MessageTimestamp: proto.Uint64(1000), Message: &waE2E.Message{Conversation: proto.String(body)}}}
}
func historyWire(records ...*waHistorySync.HistorySyncMsg) []byte {
	raw, _ := proto.Marshal(&waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String("200@s.whatsapp.net"), Messages: records}}})
	return raw
}
func compressed(raw []byte) []byte {
	var b bytes.Buffer
	z := zlib.NewWriter(&b)
	_, _ = z.Write(raw)
	_ = z.Close()
	return b.Bytes()
}
func TestHistoryOwnEnvelopeRequiredBeforeAliasAndDownload(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := liveMessage("history", "")
	e.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: &waE2E.HistorySyncNotification{InitialHistBootstrapInlinePayload: compressed(historyWire(historyRecord("source", "body")))}}}
	if err := r.historyFrom(context.Background(), e); !errors.Is(err, archive.ErrInvalidEvent) {
		t.Fatalf("peer history accepted: %v", err)
	}
	if len(s.events) != 0 || len(s.aliases) != 0 {
		t.Fatal("peer history modified archive")
	}
	e.Info.IsFromMe = true
	e.Info.Sender = *r.client.Store.ID
	if err := r.historyFrom(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 {
		t.Fatal("own history not projected")
	}
}
func TestHistoryScansControlsBeyondBodySoftCap(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.message(context.Background(), liveMessage("target", "original")); err != nil {
		t.Fatal(err)
	}
	// This was the former body total. Every valid body in the byte budget must
	// now reach policy projection, including one immediately before a control.
	const legacyBodyTotal = 2000
	records := make([]*waHistorySync.HistorySyncMsg, legacyBodyTotal+2)
	for i := 0; i < legacyBodyTotal; i++ {
		records[i] = historyRecord("body", "duplicate")
	}
	records[legacyBodyTotal] = historyRecord("tail-body", "body beyond former total")
	records[len(records)-1] = historyRecord("control", "")
	records[len(records)-1].Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("target"), RemoteJID: proto.String("200@s.whatsapp.net")}}}
	if err := r.projectHistory(context.Background(), historyWire(records...)); err != nil {
		t.Fatal(err)
	}
	if s.events[len(s.events)-1].Kind != archive.EventDelete {
		t.Fatal("soft body cap hid revoke")
	}
	if len(s.unsafe) != 0 {
		t.Fatal("complete control scan quarantined unnecessarily")
	}
	if len(s.events) != len(records)+1 || s.events[len(s.events)-2].Key.Message != "tail-body" {
		t.Fatal("byte-bounded tail body was omitted")
	}
	if len(s.syncs) == 0 || s.syncs[len(s.syncs)-1].GapCode != "" || s.syncs[len(s.syncs)-1].Dropped != 0 || r.Status().Dropped != 0 {
		t.Fatal("complete projection invented a count-limit coverage gap")
	}
}
func TestCorruptHistoryAndHardScanCapDenyArchive(t *testing.T) {
	// Record bytes remain a real hard bound even when total record counts do not.
	oversized := historyWire(historyRecord("oversized", strings.Repeat("x", recordMax)))
	for i, raw := range [][]byte{{0xff}, oversized} {
		s := fixtureStore()
		r := testRuntime(s)
		err := r.projectHistory(context.Background(), raw)
		if err == nil {
			t.Fatal("corrupt/capped history accepted")
		}
		if i == 1 && !errors.Is(err, errLimitExceeded) {
			t.Fatalf("oversized record did not reach the hard byte cap: %v", err)
		}
		if len(s.unsafe) == 0 || !r.Status().ReconciliationRequired {
			t.Fatal("uninspected controls did not deny archive")
		}
	}
}

func TestHistoryBootstrapMappingsWithinByteBudget(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	history := &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()}
	for i := 0; i < 1609; i++ {
		history.PhoneNumberToLidMappings = append(history.PhoneNumberToLidMappings, &waHistorySync.PhoneNumberToLIDMapping{PnJID: proto.String(fmt.Sprintf("%d@s.whatsapp.net", 10000+i)), LidJID: proto.String(fmt.Sprintf("%d@lid", 20000+i))})
	}
	raw, err := proto.Marshal(history)
	if err != nil || int64(len(raw)) > decodedMax {
		t.Fatalf("bounded mapping fixture: %v", err)
	}
	if err := r.projectHistory(context.Background(), raw); err != nil {
		t.Fatalf("normal mapping inventory was rejected: %v", err)
	}
	if len(s.aliases) != 1609 || len(s.unsafe) != 0 || r.Status().ReconciliationRequired {
		t.Fatal("mapping inventory was incomplete or quarantined")
	}
}

func TestHistoryBootstrapConversationsWithinByteBudget(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	history := &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()}
	for i := 0; i < 201; i++ {
		chat := fmt.Sprintf("%d@s.whatsapp.net", 30000+i)
		record := historyRecord(fmt.Sprintf("chat-%d", i), "finite synthetic chat")
		record.Message.Key.RemoteJID = proto.String(chat)
		history.Conversations = append(history.Conversations, &waHistorySync.Conversation{ID: proto.String(chat), Messages: []*waHistorySync.HistorySyncMsg{record}})
	}
	raw, err := proto.Marshal(history)
	if err != nil || int64(len(raw)) > decodedMax {
		t.Fatalf("bounded conversation fixture: %v", err)
	}
	if err := r.projectHistory(context.Background(), raw); err != nil {
		t.Fatalf("normal conversation inventory was rejected: %v", err)
	}
	if len(s.events) != 201 || len(s.unsafe) != 0 || r.Status().ReconciliationRequired {
		t.Fatal("conversation inventory was incomplete or quarantined")
	}
}

func TestHistoryBootstrapControlsBeyondLegacyRecordTotal(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.message(context.Background(), liveMessage("target", "original")); err != nil {
		t.Fatal(err)
	}
	records := make([]*waHistorySync.HistorySyncMsg, 2514)
	for i := 0; i < len(records)-1; i++ {
		records[i] = historyRecord(fmt.Sprintf("record-%d", i), "finite synthetic record")
	}
	records[len(records)-1] = historyRecord("tail-control", "")
	records[len(records)-1].Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("target"), RemoteJID: proto.String("200@s.whatsapp.net")}}}
	raw := historyWire(records...)
	if int64(len(raw)) > decodedMax {
		t.Fatal("record fixture exceeded the byte budget")
	}
	if err := r.projectHistory(context.Background(), raw); err != nil {
		t.Fatalf("bounded record inventory was rejected: %v", err)
	}
	if len(s.events) != len(records)+1 || s.events[len(s.events)-1].Kind != archive.EventDelete || s.events[len(s.events)-1].Key.Message != "target" || len(s.unsafe) != 0 || r.Status().ReconciliationRequired || r.Status().Dropped != 0 {
		t.Fatal("tail control or body projection was lost")
	}
}

// Exercise cancellation inside the metadata pass, before it can publish names.
type historyMetadataCancellation struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *historyMetadataCancellation) Err() error {
	c.checks++
	if c.checks == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestHistoryProjectionCancellationAndRecursion(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlled := &historyMetadataCancellation{Context: ctx, cancel: cancel}
	conversation, err := proto.Marshal(&waHistorySync.Conversation{ID: proto.String("200@s.whatsapp.net"), Name: proto.String("synthetic name")})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.projectConversation(controlled, conversation); !errors.Is(err, context.Canceled) || controlled.checks != 2 || len(s.names) != 0 || len(s.events) != 0 {
		t.Fatalf("metadata cancellation admitted later effects: err=%v checks=%d", err, controlled.checks)
	}
	// The outer projection retains sticky denial when cancellation prevents a
	// complete control scan, even if no message has been applied yet.
	if err := r.projectHistory(ctx, historyWire(historyRecord("cancelled", "body"))); !errors.Is(err, context.Canceled) || len(s.unsafe) == 0 || !r.Status().ReconciliationRequired || len(s.events) != 0 {
		t.Fatal("cancelled history did not deny incomplete projection")
	}
	s = fixtureStore()
	r = testRuntime(s)
	record := historyRecord("recursive", "body")
	for i := 0; i < 40; i++ {
		record.Message.Message = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: record.Message.Message}}
	}
	if err := r.projectHistory(context.Background(), historyWire(record)); !errors.Is(err, errHistoryCorrupt) || len(s.unsafe) == 0 || !r.Status().ReconciliationRequired || len(s.events) != 0 {
		t.Fatal("recursive record bypassed bounded unmarshal and sticky denial")
	}
}
func TestBackfillZeroDoesNotLoadAnchorsOrSend(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.requestBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestHistoryEditRetainsProtocolAuthorityAfterPinnedParserUnwrap(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.message(context.Background(), liveMessage("target", "original")); err != nil {
		t.Fatal(err)
	}
	edit := historyRecord("control", "")
	edit.Message.MessageTimestamp = proto.Uint64(2000)
	edit.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("target"), RemoteJID: proto.String("200@s.whatsapp.net"), Participant: proto.String("999@s.whatsapp.net")}, EditedMessage: &waE2E.Message{Conversation: proto.String("forged")}}}
	err := r.projectHistory(context.Background(), historyWire(edit))
	if err == nil {
		t.Fatal("parser unwrapping concealed forged edit participant")
	}
	for _, e := range s.events {
		if e.Body == "forged" {
			t.Fatal("forged edit persisted")
		}
	}
}
func TestWrappedHistoryEditRetainsAuthorityAndMillisecondOrder(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	_ = r.message(context.Background(), liveMessage("target", "original"))
	edit := historyRecord("control", "")
	edit.Message.MessageTimestamp = proto.Uint64(2000)
	edit.Message.Message = r.client.BuildEdit(types.NewJID("200", types.DefaultUserServer), "target", &waE2E.Message{Conversation: proto.String("forged")})
	edit.Message.Message.EditedMessage.Message.ProtocolMessage.Key.Participant = proto.String("999@s.whatsapp.net")
	edit.Message.Message.EditedMessage.Message.ProtocolMessage.TimestampMS = proto.Int64(2000123)
	if err := r.projectHistory(context.Background(), historyWire(edit)); err == nil {
		t.Fatal("wrapped edit concealed forged participant")
	}
	s = fixtureStore()
	r = testRuntime(s)
	_ = r.message(context.Background(), liveMessage("target", "original"))
	edit.Message.Message.EditedMessage.Message.ProtocolMessage.Key.Participant = nil
	edit.Message.Message.EditedMessage.Message.ProtocolMessage.EditedMessage.Conversation = proto.String("current")
	if err := r.projectHistory(context.Background(), historyWire(edit)); err != nil {
		t.Fatal(err)
	}
	got := s.events[len(s.events)-1]
	if got.Kind != archive.EventEditContent || got.SourceRevision != 2000123 || !got.OriginalSentAt.Equal(time.Unix(1000, 0)) {
		t.Fatalf("wrapped edit lost trusted mutation order/date: %+v", got)
	}
}
func TestEditedProtocolBodyCannotReplaceOuterAuthority(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	_ = r.message(context.Background(), liveMessage("outer", "original"))
	_ = r.message(context.Background(), liveMessage("unrelated", "must remain"))
	record := historyRecord("control", "")
	record.Message.Message = r.client.BuildEdit(types.NewJID("200", types.DefaultUserServer), "outer", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("unrelated")}}})
	record.Message.Message.EditedMessage.Message.ProtocolMessage.TimestampMS = proto.Int64(2000123)
	if err := r.projectHistory(context.Background(), historyWire(record)); err != nil {
		t.Fatal(err)
	}
	for _, e := range s.events {
		if e.Kind == archive.EventDelete && e.Key.Message == "unrelated" {
			t.Fatal("edited body replaced outer mutation authority")
		}
	}
	last := s.events[len(s.events)-1]
	if last.Key.Message != "outer" || last.Kind != archive.EventEditInvalidation || last.SourceRevision != 2000123 {
		t.Fatalf("outer edit invalidation/order lost: %+v", last)
	}
}
func TestAmbiguousHistoryWrappersDenyInsteadOfBecomingOriginal(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	record := historyRecord("control", "")
	record.Message.Message = &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: &waE2E.Message{Conversation: proto.String("hidden")}}, EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("target")}}}}}
	if err := r.projectHistory(context.Background(), historyWire(record)); err == nil {
		t.Fatal("ambiguous wrappers accepted as an original")
	}
	if len(s.events) != 0 || len(s.unsafe) == 0 {
		t.Fatal("ambiguous source graph failed open")
	}
}
