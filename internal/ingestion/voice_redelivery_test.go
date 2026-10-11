package ingestion

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

type voiceRedeliveryStore struct {
	*recordingStore
	items          []archive.VoiceRedeliverySource
	current        map[archive.SourceKey]archive.SourceRef
	selectionCalls int
	canonical      map[string]string
}

func (s *voiceRedeliveryStore) Canonical(_ context.Context, _ string, identity string) (string, error) {
	if canonical, ok := s.canonical[identity]; ok {
		return canonical, nil
	}
	return identity, nil
}

type voiceRedeliveryLIDStore struct {
	wmstore.NoopStore
	mappings map[types.JID]types.JID
	err      error
}

func (s *voiceRedeliveryLIDStore) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	if err := ctx.Err(); err != nil {
		return types.EmptyJID, err
	}
	return s.mappings[pn], s.err
}

func (s *voiceRedeliveryStore) VoiceRedeliverySources(_ context.Context, scope archive.ReadScope, limit int) ([]archive.VoiceRedeliverySource, error) {
	s.selectionCalls++
	if scope.Principal != "owner" || scope.Account != "100@s.whatsapp.net" || limit != 8 {
		return nil, archive.ErrUnauthorized
	}
	return append([]archive.VoiceRedeliverySource(nil), s.items...), nil
}
func (s *voiceRedeliveryStore) CurrentSource(_ context.Context, key archive.SourceKey) (archive.SourceRef, error) {
	ref, ok := s.current[key]
	if !ok {
		return archive.SourceRef{}, archive.ErrSourceUnavailable
	}
	return ref, nil
}

type voiceRedeliveryRuntimePort interface {
	RequestVoiceRedelivery(context.Context, archive.ReadScope) (int, error)
}

func voiceRedeliveryFixture(t *testing.T) (*Runtime, *voiceRedeliveryStore, voiceRedeliveryRuntimePort, archive.ReadScope) {
	t.Helper()
	base := fixtureStore()
	r := testRuntime(base)
	r.client.Store.LIDs = &voiceRedeliveryLIDStore{mappings: make(map[types.JID]types.JID)}
	s := &voiceRedeliveryStore{recordingStore: base, current: make(map[archive.SourceKey]archive.SourceRef), canonical: make(map[string]string)}
	source := archive.VoiceRedeliverySource{
		Key:            archive.SourceKey{Account: "100@s.whatsapp.net", Chat: "200@s.whatsapp.net", Sender: "100@s.whatsapp.net", Message: "voice-to-redeliver", FromMe: true},
		Source:         archive.SourceRef{Account: "100@s.whatsapp.net", Version: archive.SourceVersion{MessageID: "opaque-voice", Revision: 1, PolicyGeneration: 1}, ContentHash: strings.Repeat("a", 64)},
		OriginalSentAt: time.Now().Add(-time.Minute),
	}
	s.items = []archive.VoiceRedeliverySource{source}
	s.current[source.Key] = source.Source
	s.metadata[source.Key] = archive.SourceMetadata{Key: source.Key, Version: source.Source.Version, OriginalSentAt: source.OriginalSentAt, OriginalDateKnown: true}
	r.store = s
	r.status.Connection = "connected"
	r.runCtx = context.Background()
	port, ok := any(r).(voiceRedeliveryRuntimePort)
	if !ok {
		t.Fatal("runtime lacks bounded owner phone redelivery")
	}
	return r, s, port, archive.ReadScope{Account: source.Key.Account, Principal: "owner", PolicyGeneration: 1, AllChats: true}
}

func setVoiceRedeliveryKey(s *voiceRedeliveryStore, key archive.SourceKey) {
	old := s.items[0].Key
	metadata := s.metadata[old]
	delete(s.current, old)
	delete(s.metadata, old)
	s.items[0].Key = key
	s.current[key] = s.items[0].Source
	metadata.Key = key
	s.metadata[key] = metadata
}

func TestVoiceRedeliveryUsesVerifiedSDKLIDTransport(t *testing.T) {
	for _, scenario := range []string{"outgoing", "incoming", "group"} {
		t.Run(scenario, func(t *testing.T) {
			r, s, port, scope := voiceRedeliveryFixture(t)
			ownPN := types.NewJID("100", types.DefaultUserServer)
			ownLID := types.NewJID("800", types.HiddenUserServer)
			chatPN := types.NewJID("200", types.DefaultUserServer)
			chatLID := types.NewJID("900", types.HiddenUserServer)
			r.client.Store.LID = ownLID
			lids := r.client.Store.LIDs.(*voiceRedeliveryLIDStore)
			lids.mappings[ownPN] = ownLID
			lids.mappings[chatPN] = chatLID
			s.canonical[ownLID.String()] = ownPN.String()
			s.canonical[chatLID.String()] = chatPN.String()
			expectedChat, expectedParticipant := chatLID.String(), ""
			key := s.items[0].Key
			if scenario != "outgoing" {
				key.FromMe = false
				key.Sender = chatPN.String()
			}
			if scenario == "group" {
				key.Chat = "300@g.us"
				expectedChat = key.Chat
				expectedParticipant = chatLID.String()
			}
			setVoiceRedeliveryKey(s, key)
			sends := 0
			r.sendHistoryRequest = func(_ context.Context, message *waE2E.Message) error {
				requests := message.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
				if len(requests) != 1 {
					t.Fatal("request did not contain one exact source key")
				}
				got := requests[0].GetMessageKey()
				if got.GetRemoteJID() != expectedChat || got.GetParticipant() != expectedParticipant || got.GetID() != key.Message || got.GetFromMe() != key.FromMe {
					t.Fatal("verified LID transport did not preserve the authorized message key")
				}
				sends++
				return nil
			}
			requested, err := port.RequestVoiceRedelivery(context.Background(), scope)
			if err != nil || requested != 1 || sends != 1 {
				t.Fatalf("verified transport was not requested: count=%d error=%v", requested, err)
			}
			if len(s.events) != 0 || len(s.aliases) != 0 || s.items[0].Key != key {
				t.Fatal("transport conversion mutated archive authority")
			}
		})
	}
}

func TestVoiceRedeliveryRejectsUnverifiedSDKMappingsAndFromMe(t *testing.T) {
	for _, scenario := range []string{"wrong_chat_alias", "wrong_sender_alias", "ad_lid", "wrong_server", "empty_user", "lookup_error", "empty_mapping_error", "unrecognized_own_lid", "stale_from_me", "stale_outgoing_sender"} {
		t.Run(scenario, func(t *testing.T) {
			r, s, port, scope := voiceRedeliveryFixture(t)
			ownPN := types.NewJID("100", types.DefaultUserServer)
			ownLID := types.NewJID("800", types.HiddenUserServer)
			chatPN := types.NewJID("200", types.DefaultUserServer)
			chatLID := types.NewJID("900", types.HiddenUserServer)
			r.client.Store.LID = ownLID
			lids := r.client.Store.LIDs.(*voiceRedeliveryLIDStore)
			lids.mappings[ownPN] = ownLID
			lids.mappings[chatPN] = chatLID
			s.canonical[ownLID.String()] = ownPN.String()
			s.canonical[chatLID.String()] = chatPN.String()
			switch scenario {
			case "wrong_chat_alias":
				s.canonical[chatLID.String()] = "400@s.whatsapp.net"
			case "wrong_sender_alias":
				s.canonical[ownLID.String()] = "400@s.whatsapp.net"
			case "ad_lid":
				mapped := chatLID
				mapped.Device = 1
				lids.mappings[chatPN] = mapped
				s.canonical[mapped.String()] = chatPN.String()
			case "wrong_server":
				lids.mappings[chatPN] = chatPN
			case "empty_user":
				mapped := types.NewJID("", types.HiddenUserServer)
				lids.mappings[chatPN] = mapped
				s.canonical[mapped.String()] = chatPN.String()
			case "lookup_error":
				lids.err = errors.New("synthetic lookup failure")
			case "empty_mapping_error":
				lids.mappings = make(map[types.JID]types.JID)
				lids.err = errors.New("synthetic lookup failure")
			case "unrecognized_own_lid":
				r.client.Store.LID = types.NewJID("801", types.HiddenUserServer)
			case "stale_from_me":
				key := s.items[0].Key
				key.FromMe = false
				setVoiceRedeliveryKey(s, key)
			case "stale_outgoing_sender":
				key := s.items[0].Key
				key.Sender = chatPN.String()
				setVoiceRedeliveryKey(s, key)
			}
			sends := 0
			r.sendHistoryRequest = func(context.Context, *waE2E.Message) error { sends++; return nil }
			requested, _ := port.RequestVoiceRedelivery(context.Background(), scope)
			if requested != 0 || sends != 0 {
				t.Fatal("unverified transport or source direction escaped authorization")
			}
			if len(s.events) != 0 || len(s.aliases) != 0 {
				t.Fatal("rejected transport mutated archive authority")
			}
		})
	}
}

func TestVoiceRedeliveryUsesPNTransportWithoutSDKMapping(t *testing.T) {
	r, s, port, scope := voiceRedeliveryFixture(t)
	sends := 0
	r.sendHistoryRequest = func(_ context.Context, message *waE2E.Message) error {
		requests := message.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
		if len(requests) != 1 || requests[0].GetMessageKey().GetRemoteJID() != s.items[0].Key.Chat || !requests[0].GetMessageKey().GetFromMe() {
			t.Fatal("missing SDK mapping did not retain the authorized PN transport")
		}
		sends++
		return nil
	}
	requested, err := port.RequestVoiceRedelivery(context.Background(), scope)
	if err != nil || requested != 1 || sends != 1 {
		t.Fatalf("unmapped PN request failed: count=%d error=%v", requested, err)
	}
}

func TestVoiceRedeliveryAcknowledgementIsNotImportAndRepeatedRequestsAreBounded(t *testing.T) {
	r, s, port, scope := voiceRedeliveryFixture(t)
	requests := 0
	r.sendHistoryRequest = func(ctx context.Context, message *waE2E.Message) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("phone request has no deadline")
		}
		keys := message.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
		if len(keys) != 1 || keys[0].GetMessageKey().GetID() != s.items[0].Key.Message {
			t.Fatal("request did not bind exact source message")
		}
		requests++
		return nil
	}
	requested, err := port.RequestVoiceRedelivery(context.Background(), scope)
	if err != nil || requested != 1 || requests != 1 {
		t.Fatalf("actual phone acknowledgement was not counted: %v %v", requested, err)
	}
	if len(s.events) != 0 {
		t.Fatal("phone acknowledgement fabricated archived message delivery")
	}
	if requested, err = port.RequestVoiceRedelivery(context.Background(), scope); !errors.Is(err, ErrMediaLimit) || requested != 0 || requests != 1 {
		t.Fatal("owner request ignored finite cooldown")
	}
}

func TestVoiceRedeliveryRejectsStaleUnsafeOrDifferentAccountSources(t *testing.T) {
	for _, scenario := range []string{"account", "principal", "disconnected", "paused", "unsafe", "source_changed", "policy_changed", "expired", "invalid_jid"} {
		t.Run(scenario, func(t *testing.T) {
			r, s, port, scope := voiceRedeliveryFixture(t)
			sends := 0
			r.sendHistoryRequest = func(context.Context, *waE2E.Message) error { sends++; return nil }
			switch scenario {
			case "account":
				scope.Account = "different-account"
			case "principal":
				scope.Principal = "mcp-client"
			case "disconnected":
				r.status.Connection = "disconnected"
			case "paused":
				r.status.Paused = true
			case "unsafe":
				r.status.ReconciliationRequired = true
			case "source_changed":
				changed := s.items[0].Source
				changed.Version.Revision++
				s.current[s.items[0].Key] = changed
			case "policy_changed":
				s.policy.Generation++
			case "expired":
				s.items[0].Source.Version.ExpiresAt = time.Now().Add(-time.Second)
				s.current[s.items[0].Key] = s.items[0].Source
			case "invalid_jid":
				s.items[0].Key.Chat = "not-a-jid"
				s.current[s.items[0].Key] = s.items[0].Source
			}
			requested, _ := port.RequestVoiceRedelivery(context.Background(), scope)
			if requested != 0 || sends != 0 {
				t.Fatal("invalid source phone request escaped guards")
			}
		})
	}
}
