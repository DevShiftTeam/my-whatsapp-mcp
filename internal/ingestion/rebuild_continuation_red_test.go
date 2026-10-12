package ingestion

import (
	"context"
	"fmt"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"testing"
	"time"
)

func TestRebuildDiscoveryPersistsBeyondWindow(t *testing.T) {
	r, s := rebuildFixture()
	for i := 0; i < 96; i++ {
		e := liveMessage(fmt.Sprintf("message-%d", i), "UNMATCHED_BODY")
		if err := r.message(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.discoveries) != 96 {
		t.Fatal("bounded dispatch window discarded durable discovery metadata")
	}
	if len(s.events) != 0 {
		t.Fatal("unmatched bodies admitted")
	}
	if err := r.message(context.Background(), mutation("message-95", waE2E.ProtocolMessage_REVOKE)); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 || s.events[0].Kind != archive.EventDelete {
		t.Fatal("control after discovery window lost")
	}
}
func TestRebuildDiscoveryPersistsMoreThan32Chats(t *testing.T) {
	r, s := rebuildFixture()
	for i := 0; i < 40; i++ {
		e := liveMessage(fmt.Sprintf("chat-message-%d", i), "body")
		e.Info.Chat = types.NewJID(fmt.Sprintf("%d", 300+i), types.GroupServer)
		e.Info.IsGroup = true
		if err := r.message(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.discoveries) != 40 {
		t.Fatal("chats after first discovery window lost")
	}
}
func TestRebuildDiscoveryRetainedForEarlierDatePolicy(t *testing.T) {
	r, s := rebuildFixture()
	s.policy.DateMode = archive.DateSince
	s.policy.ImportStart = time.Unix(1100, 0)
	if err := r.message(context.Background(), liveMessage("earlier-anchor", "body")); err != nil {
		t.Fatal(err)
	}
	if len(s.discoveries) != 1 || len(s.events) != 0 {
		t.Fatal("date filtering discarded private discovery needed for later expansion")
	}
}
