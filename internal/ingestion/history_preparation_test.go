package ingestion

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"
)

func TestPreparedContentTreeBoundsAndNestedUnknowns(t *testing.T) {
	budget := 4096
	if knownContentTree(nil, 0, &budget) {
		t.Fatal("nil graph was ordinary source authority")
	}
	budget = 0
	if knownContentTree(&waE2E.Message{Conversation: proto.String("text")}, 0, &budget) {
		t.Fatal("field budget was ignored")
	}
	budget = 4096
	if knownContentTree(&waE2E.Message{}, 33, &budget) || ordinaryContentRoot(nil, true, 0) || ordinaryContentRoot(&waE2E.Message{}, true, 33) {
		t.Fatal("depth or absent-content bounds were ignored")
	}
	chunk := &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String("200@s.whatsapp.net")}}}
	budget = 4096
	if !knownContentTree(chunk, 0, &budget) {
		t.Fatal("bounded known repeated subgraph was refused")
	}
	addUnknownHistoryField(chunk.Conversations[0])
	budget = 4096
	if knownContentTree(chunk, 0, &budget) {
		t.Fatal("unknown nested repeated field disappeared")
	}
}

func TestHistoryPreparationRejectsIndependentMalformedPositiveAuthority(t *testing.T) {
	for _, kind := range []string{"missing_wrapper", "mismatched_chat", "ambiguous_body"} {
		t.Run(kind, func(t *testing.T) {
			r := testRuntime(fixtureStore())
			ticket := &sourceTicket{sequence: 1, admitted: true, history: true}
			r.sourceTickets = map[uint64]*sourceTicket{1: ticket}
			r.sourceChanged = make(chan struct{})
			ctx := context.WithValue(t.Context(), sourceTicketContextKey{}, ticket)
			record := historyRecord("invalid-positive", "receipt")
			switch kind {
			case "missing_wrapper":
				record.Message = nil
			case "mismatched_chat":
				record.Message.Key.RemoteJID = proto.String("300@s.whatsapp.net")
			case "ambiguous_body":
				record.Message.Message = ambiguousProjectionMessage().Message
			}
			if err := r.prepareHistoryOvertaking(ctx, historyWire(record)); err == nil || ticket.prepared {
				t.Fatal("malformed independent authority allowed live overtaking")
			}
		})
	}
}

func TestHistoryPreparationFallbackDoesNotEvaluatePrefixDependentSuffix(t *testing.T) {
	r := testRuntime(fixtureStore())
	ticket := &sourceTicket{sequence: 1, admitted: true, history: true}
	r.sourceTickets = map[uint64]*sourceTicket{1: ticket}
	r.sourceChanged = make(chan struct{})
	ctx := context.WithValue(t.Context(), sourceTicketContextKey{}, ticket)
	prefix := historyRecord("prefix-control", "")
	prefix.Message.Message = mutation("target", waE2E.ProtocolMessage_MESSAGE_EDIT).Message
	suffix := historyRecord("dependent-suffix", "receipt")
	suffix.Message.Key.RemoteJID = proto.String("900@lid")
	// Resolving this suffix may depend on mappings/authority established by the
	// original prefix. A fallback cannot eagerly fail it or invent its metadata.
	if err := r.prepareHistoryOvertaking(ctx, historyWire(prefix, suffix)); err != nil || ticket.prepared {
		t.Fatal("fallback evaluated a dependent suffix out of source order")
	}
}
