package ingestion

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type ownerVoiceTestStore struct {
	*voiceRedeliveryStore
	intent            archive.OwnerVoiceIntent
	registered        bool
	consumed          bool
	accepted          int
	restrictions      int
	restrictionExpiry time.Time
	restrictionView   bool
}

func (s *ownerVoiceTestStore) VoiceRedeliverySources(context.Context, archive.ReadScope, int) ([]archive.VoiceRedeliverySource, error) {
	return nil, nil
}
func (s *ownerVoiceTestStore) PrepareOwnerVoiceRedownloads(context.Context, archive.ReadScope, int) ([]archive.OwnerVoiceIntent, error) {
	if s.registered {
		return nil, nil
	}
	return []archive.OwnerVoiceIntent{s.intent}, nil
}
func (s *ownerVoiceTestStore) RegisterOwnerVoiceRequest(_ context.Context, q archive.OwnerVoiceRequestRegistration) error {
	s.registered = true
	s.intent.RequestID = q.RequestID
	return nil
}
func (s *ownerVoiceTestStore) LookupOwnerVoiceRequest(_ context.Context, id string) (archive.OwnerVoiceIntent, bool, error) {
	return s.intent, s.registered && id == s.intent.RequestID, nil
}
func (s *ownerVoiceTestStore) RecordOwnerVoiceRestriction(_ context.Context, _ string, _ archive.SourceKey, expiry time.Time, view bool) error {
	s.restrictionExpiry = expiry
	s.restrictionView = view
	s.restrictions++
	return nil
}
func (s *ownerVoiceTestStore) AcceptOwnerVoiceDescriptor(_ context.Context, q archive.OwnerVoiceDescriptorReply) (bool, error) {
	if s.consumed || q.Key != s.intent.Key || q.RequestID != s.intent.RequestID {
		return false, nil
	}
	s.accepted++
	s.consumed = true
	return true, nil
}
func (s *ownerVoiceTestStore) OwnerVoiceRecoveryStatus(context.Context, archive.ReadScope) (archive.OwnerVoiceRecoveryStatus, error) {
	return archive.OwnerVoiceRecoveryStatus{}, nil
}

func TestOwnerVoiceDescriptorDeliveredStrategyBeforeSendReturns(t *testing.T) {
	r, base, _, scope := voiceRedeliveryFixture(t)
	item := base.items[0]
	item.OriginalSentAt = time.Unix(item.OriginalSentAt.Unix(), 0)
	base.items[0] = item
	metadata := base.metadata[item.Key]
	metadata.OriginalSentAt = item.OriginalSentAt
	base.metadata[item.Key] = metadata
	s := &ownerVoiceTestStore{voiceRedeliveryStore: base, intent: archive.OwnerVoiceIntent{ID: "owner-intent", Key: item.Key, Source: item.Source, MediaID: "media", OriginalSentAt: item.OriginalSentAt, SourceRevision: item.OriginalSentAt.UnixMilli()}}
	r.store = s
	r.config.RebuildHistoryPolicy = archive.RebuildHistoryDelivered
	r.config.Rebuild = &RebuildConfig{Account: scope.Account, Boundary: time.Now()}
	r.sendRebuildRequest = func(ctx context.Context, id string, _ *waE2E.Message) error {
		if !s.registered || s.intent.RequestID != id {
			t.Fatal("send preceded durable registration")
		}
		web := historyRecord(item.Key.Message, "").Message
		web.Key.RemoteJID = proto.String(item.Key.Chat)
		web.Key.FromMe = proto.Bool(true)
		web.MessageTimestamp = proto.Uint64(uint64(item.OriginalSentAt.Unix()))
		web.Message = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{DirectPath: proto.String("/fresh-voice"), MediaKey: make([]byte, 32), FileSHA256: make([]byte, 32), FileEncSHA256: make([]byte, 32), FileLength: proto.Uint64(100), Mimetype: proto.String("audio/ogg")}}
		response, err := r.client.ParseWebMessage(types.EmptyJID, web)
		if err != nil {
			t.Fatal(err)
		}
		response.UnavailableRequestID = id
		if err = r.message(ctx, response); err != nil {
			t.Fatal(err)
		}
		if err = r.message(ctx, response); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	port, ok := any(r).(interface {
		RequestOwnerVoiceRedownloads(context.Context, archive.ReadScope) (int, error)
	})
	if !ok {
		t.Fatal("missing owner voice recovery request integration")
	}
	count, err := port.RequestOwnerVoiceRedownloads(context.Background(), scope)
	if err != nil || count != 1 || s.accepted != 1 {
		t.Fatalf("descriptor-only recovery failed: requested=%d accepted=%d err=%v", count, s.accepted, err)
	}
	if len(s.events) != 0 || r.Status().Received != 0 || r.Status().Applied != 0 {
		t.Fatal("descriptor reply changed parent body or positive admission counters")
	}
}

func TestOwnerVoiceKnownReplyRejectsForgedEnvelopeWithoutPositiveAdmission(t *testing.T) {
	for _, scenario := range []string{"missing_sdk_source", "wrong_key", "wrong_body", "expired_or_consumed"} {
		t.Run(scenario, func(t *testing.T) {
			r, base, _, scope := voiceRedeliveryFixture(t)
			item := base.items[0]
			s := &ownerVoiceTestStore{voiceRedeliveryStore: base, registered: true, intent: archive.OwnerVoiceIntent{ID: "intent", RequestID: "known-request", Key: item.Key, Source: item.Source}}
			r.store = s
			r.config.Rebuild = &RebuildConfig{Account: scope.Account, Boundary: time.Now()}
			r.config.RebuildHistoryPolicy = archive.RebuildHistoryDelivered
			web := historyRecord(item.Key.Message, "forged positive body").Message
			web.Key.RemoteJID = proto.String(item.Key.Chat)
			web.Key.FromMe = proto.Bool(true)
			web.Message = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{DirectPath: proto.String("/voice"), MediaKey: make([]byte, 32), FileSHA256: make([]byte, 32), FileEncSHA256: make([]byte, 32), FileLength: proto.Uint64(100)}}
			response, err := r.client.ParseWebMessage(types.EmptyJID, web)
			if err != nil {
				t.Fatal(err)
			}
			response.UnavailableRequestID = s.intent.RequestID
			switch scenario {
			case "missing_sdk_source":
				response.SourceWebMsg = nil
			case "wrong_key":
				response.Info.ID = "different"
			case "wrong_body":
				response.Message = &waE2E.Message{Conversation: proto.String("changed")}
			case "expired_or_consumed":
				s.consumed = true
			}
			_ = r.message(context.Background(), response)
			if s.accepted != 0 || len(s.events) != 0 || r.Status().Received != 0 {
				t.Fatal("invalid known reply obtained positive authority")
			}
		})
	}
}

func TestOwnerVoiceUncertainSendNeverAutomaticallyResends(t *testing.T) {
	r, base, _, scope := voiceRedeliveryFixture(t)
	item := base.items[0]
	s := &ownerVoiceTestStore{voiceRedeliveryStore: base, intent: archive.OwnerVoiceIntent{ID: "intent", Key: item.Key, Source: item.Source, OriginalSentAt: item.OriginalSentAt}}
	r.store = s
	sends := 0
	r.sendRebuildRequest = func(context.Context, string, *waE2E.Message) error { sends++; return errors.New("uncertain send") }
	if n, err := r.RequestOwnerVoiceRedownloads(context.Background(), scope); err == nil || n != 0 || !s.registered {
		t.Fatal("failed send lacked durable request")
	}
	r.lastBackfillExplicit = time.Time{}
	if n, err := r.RequestOwnerVoiceRedownloads(context.Background(), scope); err != nil || n != 0 || sends != 1 {
		t.Fatal("owner retry resent uncertain existing request")
	}
}

func TestOwnerVoiceUnknownRequestDoesNotGainDeliveredBodyAuthority(t *testing.T) {
	r, base, _, scope := voiceRedeliveryFixture(t)
	r.store = &ownerVoiceTestStore{voiceRedeliveryStore: base}
	r.config.Rebuild = &RebuildConfig{Account: scope.Account, Boundary: time.Now()}
	r.config.RebuildHistoryPolicy = archive.RebuildHistoryDelivered
	e := &events.Message{Info: types.MessageInfo{ID: "unknown", Timestamp: time.Now().Add(-time.Hour), MessageSource: types.MessageSource{Chat: types.NewJID("200", types.DefaultUserServer), Sender: types.NewJID("200", types.DefaultUserServer)}}, Message: &waE2E.Message{Conversation: proto.String("unknown request")}, UnavailableRequestID: "unknown"}
	_ = r.message(context.Background(), e)
	if len(base.events) != 0 || r.Status().Received != 0 {
		t.Fatal("unknown request minted delivered authority")
	}
}

func TestOwnerVoiceOriginalDecoderThreadsOnlyIntentLease(t *testing.T) {
	for _, intent := range []string{"", "new-owner-intent"} {
		t.Run(intent, func(t *testing.T) {
			r, _ := admissionFixture(t)
			_, input := syntheticOriginalWork(t, r.SourceAccount())
			input.Work.Lease.IntentID = intent
			pending, err := decodeOriginal(input, r.config.MediaMax)
			if err != nil {
				t.Fatal(err)
			}
			defer wipePending(pending)
			want := archive.OriginalDownloadLease{}
			if intent != "" {
				want = input.Work.Lease
			}
			if pending.ownerDownloadLease != want {
				t.Fatal("decoder changed owner lease discriminator")
			}
			called := false
			r.callbacks.MediaAdmitter = func(_ context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
				called = true
				if q.OwnerDownloadLease != want {
					t.Fatal("transfer dropped exact owner lease")
				}
				return nil, ErrMediaUnavailable
			}
			var cleanup mediaTransferCleanup
			_ = r.transferMedia(context.Background(), pending, &cleanup)
			if !called {
				t.Fatal("transfer did not reach bound admission")
			}
		})
	}
}

type ownerVoiceBatchStore struct {
	*ownerVoiceTestStore
	prepared  []archive.OwnerVoiceIntent
	missing   []archive.VoiceRedeliverySource
	remaining int
}

func (s *ownerVoiceBatchStore) PrepareOwnerVoiceRedownloads(context.Context, archive.ReadScope, int) ([]archive.OwnerVoiceIntent, error) {
	return s.prepared, nil
}
func (s *ownerVoiceBatchStore) VoiceRedeliverySources(_ context.Context, _ archive.ReadScope, limit int) ([]archive.VoiceRedeliverySource, error) {
	s.remaining = limit
	return s.missing, nil
}
func TestOwnerVoiceSharesEightRequestBudgetWithMissingReferences(t *testing.T) {
	r, base, _, scope := voiceRedeliveryFixture(t)
	seed := base.items[0]
	batch := &ownerVoiceBatchStore{ownerVoiceTestStore: &ownerVoiceTestStore{voiceRedeliveryStore: base}}
	for n := 0; n < 8; n++ {
		item := seed
		item.Key.Message = fmt.Sprintf("synthetic-voice-%d", n)
		base.current[item.Key] = item.Source
		base.metadata[item.Key] = archive.SourceMetadata{Key: item.Key, Version: item.Source.Version, OriginalSentAt: item.OriginalSentAt, OriginalDateKnown: true}
		if n == 0 {
			batch.prepared = append(batch.prepared, archive.OwnerVoiceIntent{ID: "new-owner-intent", Key: item.Key, Source: item.Source, OriginalSentAt: item.OriginalSentAt})
		} else {
			batch.missing = append(batch.missing, item)
		}
	}
	r.store = batch
	sends := 0
	r.sendRebuildRequest = func(context.Context, string, *waE2E.Message) error { sends++; return nil }
	r.sendHistoryRequest = func(context.Context, *waE2E.Message) error { sends++; return nil }
	count, err := r.RequestOwnerVoiceRedownloads(context.Background(), scope)
	if err != nil || count != 8 || sends != 8 || batch.remaining != 7 {
		t.Fatalf("independent request budgets escaped: count=%d sends=%d remaining=%d err=%v", count, sends, batch.remaining, err)
	}
}

func TestOwnerVoiceNegativeRequiresExactOriginalWitnessEvenForKnownRequest(t *testing.T) {
	for _, scenario := range []string{"valid_consumed", "wrong_timestamp", "wrong_original_revision"} {
		t.Run(scenario, func(t *testing.T) {
			r, base, _, _ := voiceRedeliveryFixture(t)
			item := base.items[0]
			sent := item.OriginalSentAt.Truncate(time.Second)
			s := &ownerVoiceTestStore{voiceRedeliveryStore: base, registered: true, consumed: true, intent: archive.OwnerVoiceIntent{ID: "intent", RequestID: "known-request", Key: item.Key, Source: item.Source, OriginalSentAt: sent, SourceRevision: sent.UnixMilli()}}
			r.store = s
			web := historyRecord(item.Key.Message, "").Message
			web.Key.RemoteJID = proto.String(item.Key.Chat)
			web.Key.FromMe = proto.Bool(true)
			web.MessageTimestamp = proto.Uint64(uint64(sent.Unix()))
			web.Message = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{ViewOnce: proto.Bool(true)}}
			switch scenario {
			case "wrong_timestamp":
				web.MessageTimestamp = proto.Uint64(uint64(sent.Add(time.Second).Unix()))
			case "wrong_original_revision":
				s.intent.SourceRevision++
			}
			response, err := r.client.ParseWebMessage(types.EmptyJID, web)
			if err != nil {
				t.Fatal(err)
			}
			response.UnavailableRequestID = s.intent.RequestID
			err = r.message(context.Background(), response)
			if scenario == "valid_consumed" {
				if err != nil || s.restrictions != 1 {
					t.Fatal("consumed request lost valid negative authority", err)
				}
			} else if s.restrictions != 0 {
				t.Fatal("wrong original witness admitted negative mutation")
			}
			if s.accepted != 0 || len(s.events) != 0 || r.Status().Received != 0 {
				t.Fatal("negative response gained positive authority")
			}
		})
	}
}

func TestOwnerVoiceKnownEditPreservesRestrictionWithoutPositiveBody(t *testing.T) {
	for _, kind := range []string{"expiry", "view_once"} {
		t.Run(kind, func(t *testing.T) {
			r, base, _, _ := voiceRedeliveryFixture(t)
			key := base.items[0].Key
			key.FromMe = false
			key.Sender = key.Chat
			setVoiceRedeliveryKey(base, key)
			item := base.items[0]
			s := &ownerVoiceTestStore{voiceRedeliveryStore: base, registered: true, intent: archive.OwnerVoiceIntent{ID: "intent", RequestID: "known-edit-request", Key: key, Source: item.Source, OriginalSentAt: item.OriginalSentAt, SourceRevision: item.OriginalSentAt.UnixMilli()}}
			r.store = s
			edit := mutation(key.Message, waE2E.ProtocolMessage_MESSAGE_EDIT)
			web := historyRecord(key.Message, "").Message
			web.Message = edit.Message
			expiry := time.Now().Add(time.Hour).Truncate(time.Second)
			if kind == "expiry" {
				web.EphemeralExpirationTimestamp = proto.Uint64(uint64(expiry.Unix()))
			} else {
				web.Message.ProtocolMessage.EditedMessage = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{ViewOnce: proto.Bool(true)}}
			}
			response, err := r.client.ParseWebMessage(types.EmptyJID, web)
			if err != nil {
				t.Fatal(err)
			}
			response.UnavailableRequestID = s.intent.RequestID
			if err = r.message(context.Background(), response); err != nil {
				t.Fatal(err)
			}
			if s.restrictions != 1 || kind == "expiry" && !s.restrictionExpiry.Equal(expiry) || kind == "view_once" && !s.restrictionView {
				t.Fatal("known recovery edit discarded authenticated privacy restriction")
			}
			if s.accepted != 0 || len(s.events) != 1 || s.events[0].Kind != archive.EventEditInvalidation || s.events[0].Key != key || s.events[0].Body != "" || len(s.events[0].Media) != 0 || r.Status().Received != 0 {
				t.Fatal("known edit gained positive body or descriptor authority")
			}
		})
	}
}
