package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func ambiguousProjectionMessage() *events.Message {
	e := liveMessage("synthetic_target", "SYNTHETIC_PRIVATE_CONTENT")
	e.Message.DeviceSentMessage = &waE2E.DeviceSentMessage{Message: &waE2E.Message{Conversation: proto.String("SYNTHETIC_PRIVATE_CONTENT")}}
	e.Message.EditedMessage = &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: proto.String("SYNTHETIC_PRIVATE_CONTENT")}}
	return e
}

func TestUnsafeProjectionReasonsAreFixedAndSticky(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		fail         func(*Runtime)
	}{
		{"delivered_structure", "ambiguous_message_structure", func(r *Runtime) {
			r.deliver(ambiguousProjectionMessage())
		}},
		{"projected_structure", "ambiguous_message_structure", func(r *Runtime) {
			_ = r.message(context.Background(), ambiguousProjectionMessage())
		}},
		{"mutation_envelope", "ambiguous_message_structure", func(r *Runtime) {
			_, _, _ = r.trustedMutation(context.Background(), ambiguousProjectionMessage())
		}},
		{"edited_structure", "ambiguous_edit_structure", func(r *Runtime) {
			e := mutation("synthetic_target", waE2E.ProtocolMessage_MESSAGE_EDIT)
			e.Message.ProtocolMessage.EditedMessage = ambiguousProjectionMessage().Message
			r.deliver(e)
		}},
	} {
		s := fixtureStore()
		r := testRuntime(s)
		t.Cleanup(func() { _ = r.Close() })
		if err := r.Connect(context.Background()); err != nil {
			t.Fatal("synthetic connection setup failed")
		}
		initialCodes := r.Status().Codes
		tc.fail(r)
		first := r.Status()
		if !first.ReconciliationRequired || first.Connection != "reconciliation_required" || !slices.Equal(first.Codes, append(initialCodes, tc.reason)) {
			t.Errorf("%s: fixed first branch = %v, want %s", tc.name, first.Codes, tc.reason)
		}
		// A later event cannot replace the first witness or resume projection.
		r.deliver(&events.UndecryptableMessage{})
		r.deliver(&events.AppStateSyncError{Error: opaqueConnectionError{}})
		r.deliver(liveMessage("later", "SYNTHETIC_PRIVATE_CONTENT"))
		if !slices.Equal(r.Status().Codes, first.Codes) {
			t.Errorf("%s: later delivery replaced first witness", tc.name)
		}
		encoded, err := json.Marshal(r.Status())
		if err != nil || strings.Contains(string(encoded), "SYNTHETIC_PRIVATE_CONTENT") || strings.Contains(string(encoded), "synthetic_target") {
			t.Errorf("%s: status retained private payload metadata", tc.name)
		}
		if err := r.Close(); err != nil && !errors.Is(err, archive.ErrReconciliationRequired) {
			t.Errorf("%s: synthetic owner cleanup failed", tc.name)
		}
		s.mu.Lock()
		if len(s.unsafe) != 1 || string(s.unsafe[0]) != tc.reason || len(s.events) != 0 {
			t.Errorf("%s: first durable denial/projection changed", tc.name)
		}
		for _, step := range s.order {
			if step == "end" {
				t.Errorf("%s: unsafe observation cleared", tc.name)
			}
		}
		s.mu.Unlock()
	}
}
