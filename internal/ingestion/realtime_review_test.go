//go:build sqlite_fts5

package ingestion

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type realtimePrerequisiteBarrier struct {
	*realtimeApplyBarrier
	secondEntered, secondRelease chan struct{}
	secondOnce                   sync.Once
}

func (s *realtimePrerequisiteBarrier) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	if e.Kind == archive.EventOriginal && e.Key.Message == "prerequisite-held" {
		s.secondOnce.Do(func() { close(s.secondEntered) })
		select {
		case <-s.secondRelease:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	return s.realtimeApplyBarrier.ApplySourceEvent(ctx, e, generation)
}

func realtimePaddedEvent(t *testing.T, event *events.Message, charged int64) *events.Message {
	t.Helper()
	padding := charged - 64 - retainedBytes(event)
	if padding < 0 {
		t.Fatal("synthetic frame exceeds its requested exact charge")
	}
	event.Info.PushName = strings.Repeat("p", int(padding))
	if 64+retainedBytes(event) != charged {
		t.Fatal("synthetic callback charge is not exact")
	}
	return event
}

func TestRealtimeWaitingPrerequisiteOwnsCapacityBeforeLaterLive(t *testing.T) {
	for _, countOne := range []bool{false, true} {
		t.Run(map[bool]string{false: "1024_bytes", true: "count_one_competition"}[countOne], func(t *testing.T) {
			store, _ := durableFixture(t)
			barrier := &realtimePrerequisiteBarrier{realtimeApplyBarrier: &realtimeApplyBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}, secondEntered: make(chan struct{}), secondRelease: make(chan struct{})}
			r := durableRuntime(t, barrier)
			r.config.BodyQueueBytes = 1024
			r.config.BodyQueueCount = 2
			if countOne {
				r.config.BodyQueueCount = 1
			}
			r.admissionTimeout = 750 * time.Millisecond
			var firstOnce, secondOnce sync.Once
			releaseFirst := func() { firstOnce.Do(func() { close(barrier.release) }) }
			releaseSecond := func() { secondOnce.Do(func() { close(barrier.secondRelease) }) }
			t.Cleanup(releaseFirst)
			t.Cleanup(releaseSecond)
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := r.message(t.Context(), liveMessage("capacity-delete-target", "capacitydeletereceipt")); err != nil {
				t.Fatal(err)
			}
			r.deliver(realtimePaddedEvent(t, realtimeHistory(r, historyRecord("realtime-held", "firstreceipt")), 600))
			awaitHistoryDrain(t, barrier.entered, "600-byte historical positive")
			secondDone := queueBurstDelivery(r, realtimePaddedEvent(t, realtimeHistory(r, historyRecord("prerequisite-held", "secondreceipt")), 800))
			deadline := time.Now().Add(time.Second)
			for r.IngestionLatency().WaitingCount != 1 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if r.IngestionLatency().WaitingCount != 1 {
				t.Fatal("800-byte prerequisite did not wait on real admission")
			}
			var heldWake chan struct{}
			var wakeOnce sync.Once
			releaseWake := func() {
				if heldWake != nil {
					wakeOnce.Do(func() { close(heldWake) })
				}
			}
			t.Cleanup(releaseWake)
			if countOne {
				// Model a callback that has not resumed after capacity release by
				// withholding only its wake. Neither admission ledger is modified.
				r.mu.Lock()
				heldWake = r.admissionChanged
				r.admissionChanged = make(chan struct{})
				r.mu.Unlock()
				releaseFirst()
				for {
					r.mu.Lock()
					_, firstPresent := r.sourceTickets[1]
					r.mu.Unlock()
					if !firstPresent {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
			liveDone := queueBurstDelivery(r, realtimePaddedEvent(t, liveMessage("prerequisite-live", "prerequisitelivereceipt"), 300))
			select {
			case <-liveDone:
				t.Error("later live callback consumed capacity required by an earlier waiting history")
			case <-time.After(20 * time.Millisecond):
			}
			// Privacy controls still bypass both body/admission queues.
			r.deliver(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "capacity-delete-target", Timestamp: time.Unix(2000, 0)})
			page, err := durableSearch(t, store, "capacitydeletereceipt")
			if err != nil || len(page.Hits) != 0 {
				t.Fatal("prerequisite pressure delayed a received negative")
			}
			releaseFirst()
			releaseWake()
			select {
			case <-barrier.secondEntered:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("later live occupancy prevented the earlier prerequisite from executing")
			}
			queueBurstAwaitDelivery(t, secondDone)
			snapshot := r.IngestionLatency()
			if !snapshot.Valid() || snapshot.QueuedCount != 1 || snapshot.QueuedBytes != 800 || snapshot.WaitingCount != 1 || snapshot.WaitingBytes != 300 || snapshot.Unsafe || r.Status().Dropped != 0 {
				t.Fatal("shared capacity was stolen or counted twice", snapshot)
			}
			releaseSecond()
			queueBurstAwaitDelivery(t, liveDone)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := r.Disconnect(ctx); err != nil {
				t.Fatal("bounded valid source work caused a false safety pause", err)
			}
			page, err = durableSearch(t, store, "prerequisitelivereceipt")
			if err != nil || len(page.Hits) != 1 || r.Status().Dropped != 0 || r.Status().ReconciliationRequired {
				t.Fatal("prerequisite scheduling lost fresh plaintext or introduced a false unsafe marker")
			}
		})
	}
}

func TestRealtimeMultipleWaitingPrerequisitesDoNotTrapParkedLiveReservation(t *testing.T) {
	store, _ := durableFixture(t)
	barrier := &realtimePrerequisiteBarrier{realtimeApplyBarrier: &realtimeApplyBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}, secondEntered: make(chan struct{}), secondRelease: make(chan struct{})}
	r := durableRuntime(t, barrier)
	r.config.BodyQueueBytes, r.config.BodyQueueCount = 1024, 2
	r.admissionTimeout = 750 * time.Millisecond
	var firstOnce, secondOnce sync.Once
	releaseFirst := func() { firstOnce.Do(func() { close(barrier.release) }) }
	releaseSecond := func() { secondOnce.Do(func() { close(barrier.secondRelease) }) }
	t.Cleanup(releaseFirst)
	t.Cleanup(releaseSecond)
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.deliver(realtimePaddedEvent(t, realtimeHistory(r, historyRecord("realtime-held", "firstreceipt")), 220))
	awaitHistoryDrain(t, barrier.entered, "first positive before writer admission")
	secondDone := queueBurstDelivery(r, realtimePaddedEvent(t, realtimeHistory(r, historyRecord("prerequisite-held", "secondreceipt")), 500))
	deadline := time.Now().Add(time.Second)
	for r.IngestionLatency().WaitingCount != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	thirdDone := queueBurstDelivery(r, realtimePaddedEvent(t, realtimeHistory(r, historyRecord("third-prerequisite", "thirdreceipt")), 300))
	for r.IngestionLatency().WaitingCount != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if snapshot := r.IngestionLatency(); !snapshot.Valid() || snapshot.WaitingCount != 2 || snapshot.WaitingBytes != 800 {
		t.Fatal("synthetic prerequisites were not retained within the original waiting budget")
	}
	liveDone := queueBurstDelivery(r, realtimePaddedEvent(t, liveMessage("large-parked-live", "largeparkedlivereceipt"), 800))
	select {
	case <-liveDone:
		t.Fatal("parked live payload enqueued before its prerequisites")
	case <-time.After(20 * time.Millisecond):
	}
	if snapshot := r.IngestionLatency(); !snapshot.Valid() || snapshot.QueuedCount != 2 || snapshot.QueuedBytes != 1020 || snapshot.WaitingCount != 2 || snapshot.WaitingBytes != 800 {
		t.Fatal("large live callback did not own the bounded parked reservation", snapshot)
	}
	releaseFirst()
	select {
	case <-barrier.secondEntered:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("another waiting prerequisite prevented reservation exchange and source progress")
	}
	if snapshot := r.IngestionLatency(); !snapshot.Valid() || snapshot.QueuedCount != 2 || snapshot.QueuedBytes != 800 || snapshot.WaitingCount != 1 || snapshot.WaitingBytes != 800 || snapshot.Unsafe {
		t.Fatal("idle prefix exchange exceeded a ledger or lost a reservation", snapshot)
	}
	releaseSecond()
	queueBurstAwaitDelivery(t, secondDone)
	queueBurstAwaitDelivery(t, thirdDone)
	queueBurstAwaitDelivery(t, liveDone)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal("valid bounded waiting prefixes caused a false unsafe pause", err)
	}
	page, err := durableSearch(t, store, "largeparkedlivereceipt")
	if err != nil || len(page.Hits) != 1 || r.Status().Dropped != 0 || r.Status().ReconciliationRequired {
		t.Fatal("reservation exchange lost an independently readable live body")
	}
}

func realtimeRestrictedBody(kind string, active bool) *waE2E.Message {
	m := &waE2E.Message{Conversation: proto.String("restrictionreceipt")}
	switch kind {
	case "limit_sharing", "limit_sharing_v2":
		m.MessageContextInfo = &waE2E.MessageContextInfo{}
		sharing := &waCommon.LimitSharing{SharingLimited: proto.Bool(active)}
		if kind == "limit_sharing" {
			m.MessageContextInfo.LimitSharing = sharing
		} else {
			m.MessageContextInfo.LimitSharingV2 = sharing
		}
	case "after_read", "ephemeral_setting", "disappearing_mode":
		contextInfo := &waE2E.ContextInfo{}
		if kind == "after_read" {
			duration := uint32(0)
			if active {
				duration = 10
			}
			contextInfo.AfterReadDuration = proto.Uint32(duration)
		}
		if kind == "ephemeral_setting" {
			contextInfo.EphemeralSettingTimestamp = proto.Int64(0)
		}
		if kind == "disappearing_mode" {
			contextInfo.DisappearingMode = &waE2E.DisappearingMode{}
		}
		m.Conversation = nil
		m.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: proto.String("restrictionreceipt"), ContextInfo: contextInfo}
	case "addon_duration":
		m.MessageContextInfo = &waE2E.MessageContextInfo{MessageAddOnDurationInSecs: proto.Uint32(0)}
	case "keep_in_chat":
		m.ProtoReflect().Set(m.ProtoReflect().Descriptor().Fields().ByName("keepInChatMessage"), protoreflect.ValueOfMessage((&waE2E.KeepInChatMessage{}).ProtoReflect()))
	}
	return m
}

func TestRealtimePrivacyMetadataPresenceNeverQualifiesForOvertaking(t *testing.T) {
	for _, kind := range []string{"limit_sharing", "limit_sharing_v2", "after_read", "ephemeral_setting", "disappearing_mode", "addon_duration", "keep_in_chat"} {
		for _, active := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "_zero", true: "_active"}[active], func(t *testing.T) {
				r := testRuntime(fixtureStore())
				body := realtimeRestrictedBody(kind, active)
				live := liveMessage("restricted-live", "")
				live.Message = body
				if r.realtimePlaintext(live) {
					t.Error("privacy/retention metadata entered the plaintext lane")
				}
				ticket := &sourceTicket{sequence: 1, admitted: true, history: true}
				r.sourceTickets = map[uint64]*sourceTicket{1: ticket}
				r.sourceChanged = make(chan struct{})
				record := historyRecord("restricted-history", "")
				record.Message.Message = body
				ctx := context.WithValue(t.Context(), sourceTicketContextKey{}, ticket)
				if err := r.prepareHistoryOvertaking(ctx, historyWire(record)); err != nil || ticket.prepared {
					t.Error("privacy metadata was discarded or caused a new source failure during preparation")
				}
			})
		}
	}
}

func TestRealtimeDiscardedRawWrapperLayerKeepsHistoryBarrierClosed(t *testing.T) {
	for _, layer := range []string{"raw_root", "device_wrapper", "bot_wrapper"} {
		t.Run(layer, func(t *testing.T) {
			store, r, barrier, release := realtimeBarrier(t)
			body := &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: &waE2E.Message{Conversation: proto.String("rawwrapperreceipt")}}}
			switch layer {
			case "raw_root":
				addUnknownHistoryField(body)
			case "device_wrapper":
				addUnknownHistoryField(body.DeviceSentMessage)
			case "bot_wrapper":
				body = &waE2E.Message{BotInvokeMessage: &waE2E.FutureProofMessage{Message: body.DeviceSentMessage.Message}}
				addUnknownHistoryField(body.BotInvokeMessage)
			}
			rawLive := liveMessage("raw-wrapper-live", "ordinaryreceipt")
			rawLive.RawMessage = body
			if r.realtimePlaintext(rawLive) {
				t.Error("live classification ignored a discarded raw wrapper layer")
			}
			tail := historyRecord("raw-wrapper-tail", "")
			tail.Message.Message = body
			r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt"), tail))
			awaitHistoryDrain(t, barrier.entered, "original ordered positive prefix")
			if r.IngestionLatency().HistoryPrepared {
				t.Error("SDK unwrapping hid an unknown raw wrapper layer from preparation")
			}
			r.deliver(liveMessage("after-raw-wrapper", "afterrawwrapperreceipt"))
			realtimeAssertBlocked(t, store, "afterrawwrapperreceipt")
			release()
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal("conservative raw-wrapper fallback changed ordinary source handling", err)
			}
			page, err := durableSearch(t, store, "rawwrapperreceipt")
			if err != nil || len(page.Hits) != 1 {
				t.Fatal("fallback lost the ordinary raw-wrapper body")
			}
		})
	}
}

func TestRealtimeMetadataWhitelistCoversEveryDeclaredContextField(t *testing.T) {
	for _, messageContext := range []bool{false, true} {
		var contextMessage proto.Message = &waE2E.ContextInfo{}
		allowed := map[protoreflect.Name]bool{"forwardingScore": true, "isForwarded": true, "mentionedJID": true}
		if messageContext {
			contextMessage = &waE2E.MessageContextInfo{}
			allowed = map[protoreflect.Name]bool{"deviceListMetadata": true, "deviceListMetadataVersion": true, "messageSecret": true, "paddingBytes": true}
		}
		fields := contextMessage.ProtoReflect().Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			field := fields.Get(i)
			if allowed[field.Name()] {
				continue
			}
			t.Run(string(field.FullName()), func(t *testing.T) {
				metadata := contextMessage.ProtoReflect().New()
				value := metadata.NewField(field)
				if field.IsList() {
					list := value.List()
					list.Append(list.NewElement())
				}
				if field.Kind() == protoreflect.BytesKind {
					value = protoreflect.ValueOfBytes([]byte{0})
				}
				metadata.Set(field, value)
				if !metadata.Has(field) {
					t.Fatal("fixture failed to preserve explicit field presence")
				}
				body := &waE2E.Message{Conversation: proto.String("contextwhitelistreceipt")}
				if messageContext {
					body.MessageContextInfo = metadata.Interface().(*waE2E.MessageContextInfo)
				} else {
					body.Conversation = nil
					body.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: proto.String("contextwhitelistreceipt"), ContextInfo: metadata.Interface().(*waE2E.ContextInfo)}
				}
				r := testRuntime(fixtureStore())
				live := liveMessage("context-whitelist", "")
				live.Message = body
				if r.realtimePlaintext(live) {
					t.Fatal("unreviewed context-field presence entered the realtime lane")
				}
				ticket := &sourceTicket{sequence: 1, admitted: true, history: true}
				r.sourceTickets = map[uint64]*sourceTicket{1: ticket}
				r.sourceChanged = make(chan struct{})
				record := historyRecord("context-whitelist", "")
				record.Message.Message = body
				if err := r.prepareHistoryOvertaking(context.WithValue(t.Context(), sourceTicketContextKey{}, ticket), historyWire(record)); err != nil || ticket.prepared {
					t.Fatal("unreviewed context field escaped conservative history preparation")
				}
			})
		}
	}
}

func TestRealtimePrivacyMetadataKeepsActualHistoryWindowOrdered(t *testing.T) {
	for _, kind := range []string{"limit_sharing", "limit_sharing_v2", "after_read", "addon_duration"} {
		t.Run(kind, func(t *testing.T) {
			store, r, barrier, release := realtimeBarrier(t)
			tail := historyRecord("privacy-history-tail", "")
			tail.Message.Message = realtimeRestrictedBody(kind, false)
			r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt"), tail))
			awaitHistoryDrain(t, barrier.entered, "ordered privacy-metadata prefix")
			if r.IngestionLatency().HistoryPrepared {
				t.Fatal("privacy metadata allowed overtaking in the actual history window")
			}
			r.deliver(liveMessage("privacy-window-live", "privacywindowreceipt"))
			realtimeAssertBlocked(t, store, "privacywindowreceipt")
			release()
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal("optimization fallback changed valid ordered source behavior", err)
			}
			page, err := durableSearch(t, store, "privacywindowreceipt")
			if err != nil || len(page.Hits) != 1 {
				t.Fatal("privacy fallback dropped later source work")
			}
		})
	}
}

func TestRealtimeBenignMessageSecretsAndDeviceMetadataRemainFast(t *testing.T) {
	store, r, barrier, release := realtimeBarrier(t)
	handler, token, chat := realtimeAliasMCP(t, store, r)
	body := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("benignmetadatareceipt"), ContextInfo: &waE2E.ContextInfo{ForwardingScore: proto.Uint32(0), IsForwarded: proto.Bool(false)}}, MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: []byte{1, 2}, DeviceListMetadata: &waE2E.DeviceListMetadata{}, DeviceListMetadataVersion: proto.Int32(0)}}
	if !r.realtimePlaintext(&events.Message{Info: liveMessage("benign-live", "").Info, Message: body}) {
		t.Fatal("benign metadata was unnecessarily excluded from realtime text")
	}
	held := historyRecord("realtime-held", "")
	held.Message.Message = proto.Clone(body).(*waE2E.Message)
	r.deliver(realtimeHistory(r, held))
	awaitHistoryDrain(t, barrier.entered, "prepared benign metadata")
	if !r.IngestionLatency().HistoryPrepared {
		t.Fatal("benign secret/device-list metadata kept a needless history barrier")
	}
	live := liveMessage("benign-live", "")
	live.Message = body
	r.deliver(live)
	deadline := time.Now().Add(time.Second)
	for !realtimeAliasVisible(t, handler, token, "benignmetadatareceipt", chat) {
		if !time.Now().Before(deadline) {
			t.Fatal("benign metadata text waited for historical projection")
		}
		time.Sleep(time.Millisecond)
	}
	if r.Status().HistoryChunks != 0 {
		t.Fatal("history was released before live visibility")
	}
	release()
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal(err)
	}
}
