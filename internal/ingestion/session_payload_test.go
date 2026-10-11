package ingestion

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type payloadPolicyStore struct {
	*recordingStore
	current archive.ArchivePolicy
}

func (s *payloadPolicyStore) Policy(context.Context) (archive.ArchivePolicy, error) {
	return s.current, nil
}

func TestOpenSessionRequestsBoundedFullInitialHistory(t *testing.T) {
	globalProps := proto.Clone(store.DeviceProps)
	globalBase := proto.Clone(store.BaseClientPayload)
	container, client, err := openSession(context.Background(), filepath.Join(canonicalSessionTestDir(t), "session"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Close() })
	s := &payloadPolicyStore{recordingStore: fixtureStore(), current: archive.ArchivePolicy{Generation: 1, DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}}
	var connectedPayload *waWa6.ClientPayload
	c := &contextConnection{fakeConnection: &fakeConnection{}, connect: func(context.Context) error {
		connectedPayload = client.Store.GetClientPayload()
		if client.GetClientPayload != nil {
			connectedPayload = client.GetClientPayload()
		}
		return nil
	}}
	r := newRuntime(Config{BodyQueueCount: 8, BodyQueueBytes: 8 << 20, MediaMax: 4 << 20}, s, Callbacks{}, client, c)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Pair(context.Background()); err != nil {
		t.Fatal(err)
	}
	defaultPayload := client.Store.GetClientPayload()
	payload := connectedPayload
	if payload == nil || payload.DevicePairingData == nil {
		t.Fatal("fresh session did not produce a registration payload")
	}
	props := &waCompanionReg.DeviceProps{}
	if err := proto.Unmarshal(payload.DevicePairingData.DeviceProps, props); err != nil {
		t.Fatal(err)
	}
	if !props.GetRequireFullSync() || props.GetHistorySyncConfig().GetFullSyncDaysLimit() != 90 {
		t.Fatal("registration did not request bounded full initial history")
	}
	defaultProps := &waCompanionReg.DeviceProps{}
	if err := proto.Unmarshal(defaultPayload.DevicePairingData.DeviceProps, defaultProps); err != nil {
		t.Fatal(err)
	}
	props.RequireFullSync = defaultProps.RequireFullSync
	props.HistorySyncConfig.FullSyncDaysLimit = defaultProps.HistorySyncConfig.FullSyncDaysLimit
	if !proto.Equal(props, defaultProps) {
		t.Fatal("history request changed other device properties")
	}
	registration := proto.Clone(payload).(*waWa6.ClientPayload)
	registration.DevicePairingData.DeviceProps = defaultPayload.DevicePairingData.DeviceProps
	if !proto.Equal(registration, defaultPayload) {
		t.Fatal("history request changed keys, version, or other registration fields")
	}
	if client.GetClientPayload == nil {
		t.Fatal("history request did not use a per-client payload callback")
	}
	payload.DevicePairingData.DeviceProps[0] ^= 0xff
	repeated := client.GetClientPayload()
	repeatedProps := &waCompanionReg.DeviceProps{}
	if err := proto.Unmarshal(repeated.DevicePairingData.DeviceProps, repeatedProps); err != nil || !repeatedProps.GetRequireFullSync() {
		t.Fatal("a returned payload mutated the next registration")
	}
	if !proto.Equal(store.DeviceProps, globalProps) || !proto.Equal(store.BaseClientPayload, globalBase) {
		t.Fatal("per-client request mutated SDK global defaults")
	}
	for _, tc := range []struct {
		age  time.Duration
		days uint32
	}{{30*24*time.Hour - time.Hour, 31}, {time.Hour, 2}, {-time.Hour, 1}, {120*24*time.Hour - time.Hour, 90}} {
		if err := r.Disconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.current.DateMode = archive.DateSince
		s.current.ImportStart = time.Now().Add(-tc.age)
		if err := r.Pair(context.Background()); err != nil {
			t.Fatal(err)
		}
		p := &waCompanionReg.DeviceProps{}
		if err := proto.Unmarshal(connectedPayload.DevicePairingData.DeviceProps, p); err != nil || !p.GetRequireFullSync() || p.GetHistorySyncConfig().GetFullSyncDaysLimit() != tc.days {
			t.Fatalf("policy-bounded registration request did not use %d days", tc.days)
		}
		s.current.ImportStart = time.Now().Add(-60 * 24 * time.Hour)
		p = &waCompanionReg.DeviceProps{}
		if err := proto.Unmarshal(client.GetClientPayload().DevicePairingData.DeviceProps, p); err != nil || p.GetHistorySyncConfig().GetFullSyncDaysLimit() != tc.days {
			t.Fatal("policy changes mutated the in-flight attempt's immutable request")
		}
	}
	jid := types.NewADJID("15550000001", 0, 1)
	client.Store.ID = &jid
	wantLogin, err := proto.Marshal(client.Store.GetClientPayload())
	if err != nil {
		t.Fatal(err)
	}
	login := client.GetClientPayload()
	gotLogin, err := proto.Marshal(login)
	if err != nil || login.DevicePairingData != nil || !bytes.Equal(gotLogin, wantLogin) {
		t.Fatal("saved-session reconnect changed the SDK login payload")
	}
}
