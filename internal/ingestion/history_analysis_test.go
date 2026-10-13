package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestControlFirstDescriptorInventoryComplete(t *testing.T) {
	for _, m := range []proto.Message{&waHistorySync.HistorySync{}, &waHistorySync.Conversation{}, &waHistorySync.HistorySyncMsg{}, &waHistorySync.PhoneNumberToLIDMapping{}, &waWeb.WebMessageInfo{}, &waCommon.MessageKey{}} {
		d := m.ProtoReflect().Descriptor()
		inventory := historyFieldDisposition[d.FullName()]
		if len(inventory) != d.Fields().Len() {
			t.Fatalf("incomplete pinned field inventory %s: %d/%d", d.FullName(), len(inventory), d.Fields().Len())
		}
		for i := 0; i < d.Fields().Len(); i++ {
			f := d.Fields().Get(i)
			if disposition := inventory[f.Number()]; disposition < historyAuthority || disposition > historyContainer {
				t.Fatalf("missing disposition %s.%s", d.FullName(), f.Name())
			}
		}
	}
}
func TestControlFirstBenignStarredDisplayMetadata(t *testing.T) {
	for _, starred := range []bool{false, true} {
		r, s := controlFirstFixture()
		m := bodylessHistoryStub("300@g.us", "display-only", waWeb.WebMessageInfo_GROUP_CREATE)
		m.Message.Starred = proto.Bool(starred)
		m.Message.MessageStubParameters = []string{"never-an-alias"}
		if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(m))); err != nil {
			t.Fatal(err)
		}
		if len(s.reasons) != 0 || len(s.events) != 0 || len(s.names) != 0 || len(s.aliases) != 0 {
			t.Fatal("reviewed display metadata produced authority or uncertainty")
		}
	}
}
func TestControlFirstWireIntegrityAndDiagnosticV2(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    []byte
		reason string
	}{
		{"wrong_type", protowire.AppendBytes(protowire.AppendTag(nil, 17, protowire.BytesType), []byte("PRIVATE-CANARY")), "wrong_wire_type"},
		{"bad_bool", protowire.AppendVarint(protowire.AppendTag(nil, 17, protowire.VarintType), 2), "out_of_range"},
		{"duplicate", []byte{0x88, 0x01, 0, 0x88, 0x01, 1}, "duplicate_singular"},
		{"framing", []byte{0xff}, "malformed_wire"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := analyzeHistoryWire(context.Background(), tc.raw, (&waWeb.WebMessageInfo{}).ProtoReflect().Descriptor())
			if err == nil || a.diagnostic.Reason != tc.reason {
				t.Fatalf("wrong typed failure: %v %s", err, a.diagnostic.Reason)
			}
			encoded := historyFailureDiagnostic(historyAnalysisError(err, a, nil))
			if len(encoded) > 4096 || strings.Contains(string(encoded), "PRIVATE-CANARY") {
				t.Fatal("unbounded or private diagnostic")
			}
			var decoded map[string]any
			if json.Unmarshal(encoded, &decoded) != nil || decoded["v2"] == nil {
				t.Fatal("missing bounded v2")
			}
		})
	}
}
func TestControlFirstDiagnosticAllFieldsUnknownAndOverflow(t *testing.T) {
	raw := protowire.AppendVarint(protowire.AppendTag(nil, 80, protowire.VarintType), 0)
	for n := protowire.Number(19000); n < 19010; n++ {
		raw = protowire.AppendBytes(protowire.AppendTag(raw, n, protowire.BytesType), []byte("PRIVATE-VALUE-CANARY"))
	}
	a, err := analyzeHistoryWire(context.Background(), raw, (&waWeb.WebMessageInfo{}).ProtoReflect().Descriptor())
	if err != nil || !a.control || !a.diagnostic.Overflow {
		t.Fatal("unknown future fields became corruption or truncation hidden")
	}
	if a.diagnostic.Containers[0].Mask[1] != "0000000000010000" || len(a.diagnostic.Containers[0].Unknown) != 4 {
		t.Fatal("high known presence or bounded unknown shape lost")
	}
	encoded := historyFailureDiagnostic(historyAnalysisError(archive.ErrArchiveUnavailable, a, nil))
	if len(encoded) > 4096 || strings.Contains(string(encoded), "PRIVATE-VALUE-CANARY") {
		t.Fatal("diagnostic leaked field values")
	}
}
func TestControlFirstEnvelope500RecordsAndFieldBudget(t *testing.T) {
	c := unknownHistoryChunk(unknownHistoryRecord())
	for len(c.Conversations[0].Messages) < 500 {
		c.Conversations[0].Messages = append(c.Conversations[0].Messages, unknownHistoryRecord())
	}
	if _, err := analyzeHistoryWire(context.Background(), unknownHistoryWire(t, c), envelopeHistoryDescriptor()); err != nil {
		t.Fatal("bounded500 envelope rejected", err)
	}
	raw := []byte{}
	for i := 0; i < 4097; i++ {
		raw = protowire.AppendVarint(protowire.AppendTag(raw, 19000, protowire.VarintType), 0)
	}
	a, err := analyzeHistoryWire(context.Background(), raw, recordHistoryDescriptor())
	if !errors.Is(err, errLimitExceeded) || a.diagnostic.Reason != "field_budget" {
		t.Fatal("wire budget not enforced")
	}
}
func TestControlFirstObservationCapturedOnce(t *testing.T) {
	r, s := controlFirstFixture()
	r.observation = "initial-observation"
	ctx := r.captureHistoryObservation(context.Background())
	r.observation = "later-observation"
	if got := ctx.Value(historyObservationContextKey{}).(archive.ObservationID); got != "initial-observation" {
		t.Fatal("capture changed")
	}
	if err := r.projectHistory(ctx, unknownHistoryWire(t, unknownHistoryChunk(unknownHistoryRecord()))); err != nil {
		t.Fatal(err)
	}
	if len(s.observations) != 1 || s.observations[0] != "initial-observation" {
		t.Fatal("gap refreshed captured observation")
	}
}
func TestControlFirstMappingUnknownFieldsCannotGrantAliases(t *testing.T) {
	r, s := controlFirstFixture()
	mapping := &waHistorySync.PhoneNumberToLIDMapping{PnJID: proto.String("200@s.whatsapp.net"), LidJID: proto.String("900@lid")}
	addUnknownHistoryField(mapping)
	c := unknownHistoryChunk(unknownHistoryRecord())
	c.PhoneNumberToLidMappings = []*waHistorySync.PhoneNumberToLIDMapping{mapping}
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, c)); err != nil {
		t.Fatal(err)
	}
	if len(s.aliases) != 0 || len(s.reasons) < 2 {
		t.Fatal("unsupported mapping granted identity or lost parent uncertainty")
	}
}
func TestControlFirstCanonicalAuthorityFields(t *testing.T) {
	for _, d := range []protoreflect.MessageDescriptor{conversationHistoryDescriptor(), (&waHistorySync.PhoneNumberToLIDMapping{}).ProtoReflect().Descriptor()} {
		raw := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte("200:0@s.whatsapp.net"))
		a, err := analyzeHistoryWire(context.Background(), raw, d)
		if err == nil || a.diagnostic.Reason != "invalid_jid" {
			t.Fatal("used authority normalized malformed wire")
		}
	}
}

func TestControlFirstPackedScalarCompatibility(t *testing.T) {
	for _, packed := range []bool{true, false} {
		t.Run(map[bool]string{true: "packed", false: "unpacked"}[packed], func(t *testing.T) {
			var raw []byte
			if packed {
				values := protowire.AppendVarint(nil, 0)
				values = protowire.AppendVarint(values, 4294967295)
				raw = protowire.AppendBytes(protowire.AppendTag(nil, 82, protowire.BytesType), values)
			} else {
				raw = protowire.AppendVarint(protowire.AppendTag(nil, 82, protowire.VarintType), 0)
				raw = protowire.AppendVarint(protowire.AppendTag(raw, 82, protowire.VarintType), 4294967295)
			}
			if _, err := analyzeHistoryWire(context.Background(), raw, (&waE2E.ContextInfo{}).ProtoReflect().Descriptor()); err != nil {
				t.Fatalf("valid repeated uint32 wire fenced: %v", err)
			}
		})
	}
	r, s := controlFirstFixture()
	m := unknownHistoryRecord()
	m.Message.FinalLiveLocation = &waE2E.LiveLocationMessage{ContextInfo: &waE2E.ContextInfo{ExperienceIDs: []uint32{0, 1, 4294967295}}}
	if err := r.projectHistory(context.Background(), unknownHistoryWire(t, unknownHistoryChunk(m, controlFirstTail()...))); err != nil {
		t.Fatalf("SDK generated packed metadata blocked strict suffix: %v", err)
	}
	if len(s.reasons) == 0 || len(s.events) == 0 {
		t.Fatal("packed unsupported metadata lost durable gap or suffix")
	}
}
func TestControlFirstMalformedPackedScalarRemainsHard(t *testing.T) {
	for _, values := range [][]byte{{0x80}, protowire.AppendVarint(nil, 4294967296)} {
		raw := protowire.AppendBytes(protowire.AppendTag(nil, 82, protowire.BytesType), values)
		if _, err := analyzeHistoryWire(context.Background(), raw, (&waE2E.ContextInfo{}).ProtoReflect().Descriptor()); err == nil {
			t.Fatal("malformed or out-of-range packed uint32 accepted")
		}
	}
}

func TestControlFirstDiagnosticV2MaximumBound(t *testing.T) {
	a := historyAnalysis{diagnostic: historyDiagnosticV2{Version: 2, Schema: "whatsmeow-6bb48c0f1ff0", Evidence: "exact_wire", Layer: "conversation", Reason: "unsupported_known_field", Tag: 536870911, Wire: 5, Overflow: true}}
	for i := 0; i < 8; i++ {
		shape := historyContainerShape{Layer: "conversation", Mask: [2]string{"ffffffffffffffff", "ffffffffffffffff"}, Duplicate: true, Complete: true}
		for j := 0; j < 8; j++ {
			shape.Path = append(shape.Path, 536870911)
		}
		for j := 0; j < 4; j++ {
			shape.Unknown = append(shape.Unknown, historyWireField{536870911, 5})
		}
		a.diagnostic.Containers = append(a.diagnostic.Containers, shape)
	}
	encoded := historyFailureDiagnostic(historyAnalysisError(archive.ErrArchiveUnavailable, a, unknownHistoryRecord().Message))
	if len(encoded) > 4096 {
		t.Fatalf("maximum bounded v2 exceeded4096: %d", len(encoded))
	}
	var decoded map[string]any
	if json.Unmarshal(encoded, &decoded) != nil || len(decoded) != 9 || decoded["raw_stub"] != float64(syntheticUnknownStub) {
		t.Fatal("v2 changed legacy fields or JSON shape")
	}
}
