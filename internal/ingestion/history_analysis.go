package ingestion

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type historyDisposition uint8

const (
	historyAuthority historyDisposition = iota + 1
	historyMetadata
	historyUnsupportedMetadata
	historyUnsupportedControl
	historyRecordPayload
	historyPositivePayload
	historyContainer
)

// Every pinned root field has an explicit disposition. Descriptor-driven wire
// validation is separate from semantic support: knowing a schema is not evidence
// that a control was enforced. All descendants of unsupported containers retain
// that disposition; their framing, scalar bounds and duplicate claims are checked.
var historyFieldDisposition = map[protoreflect.FullName]map[protoreflect.FieldNumber]historyDisposition{
	"WAWebProtobufsHistorySync.HistorySync": historyFields(
		historyAuthority, []int{1}, historyRecordPayload, []int{2}, historyMetadata, []int{5, 6}, historyContainer, []int{15},
		historyUnsupportedMetadata, []int{7, 9, 10, 11, 13, 14, 16, 17, 18, 19, 20, 21}, historyUnsupportedControl, []int{3, 8, 12}),
	"WAWebProtobufsHistorySync.Conversation": historyFields(
		historyAuthority, []int{1}, historyRecordPayload, []int{2}, historyMetadata, []int{5, 6, 7, 8, 11, 12, 13, 14, 15, 16, 18, 19, 24, 25, 27, 28, 31, 33, 34, 35, 36, 37, 38, 43, 44, 45, 48, 54, 55, 56, 58, 59, 60, 61, 62},
		historyUnsupportedMetadata, []int{21, 22, 26}, historyUnsupportedControl, []int{3, 4, 9, 10, 17, 20, 23, 29, 30, 32, 39, 40, 41, 42, 46, 47, 49, 50, 51, 52, 53, 57, 63, 64}),
	"WAWebProtobufsHistorySync.HistorySyncMsg": historyFields(historyContainer, []int{1}, historyMetadata, []int{2}),
	"WAWebProtobufsWeb.WebMessageInfo": historyFields(
		historyContainer, []int{1}, historyPositivePayload, []int{2}, historyAuthority, []int{5, 24, 51}, historyMetadata, []int{3, 4, 6, 16, 17, 18, 21, 22, 23, 26, 27, 28, 36, 48, 56, 57, 63, 65, 70, 78},
		historyUnsupportedMetadata, []int{19, 20, 29, 30, 31, 37, 38, 39, 40, 41, 42, 44, 45, 46, 47, 49, 55, 58, 59, 61, 62, 64, 66, 69, 71, 72, 73, 76, 79, 81, 82, 83},
		historyUnsupportedControl, []int{25, 32, 33, 34, 35, 43, 50, 52, 54, 67, 68, 74, 75, 77, 80}),
	"WACommon.MessageKey":                               historyFields(historyAuthority, []int{1, 2, 3, 4}),
	"WAWebProtobufsHistorySync.PhoneNumberToLIDMapping": historyFields(historyAuthority, []int{1, 2}),
}

func historyFields(pairs ...any) map[protoreflect.FieldNumber]historyDisposition {
	out := map[protoreflect.FieldNumber]historyDisposition{}
	for i := 0; i < len(pairs); i += 2 {
		for _, n := range pairs[i+1].([]int) {
			out[protoreflect.FieldNumber(n)] = pairs[i].(historyDisposition)
		}
	}
	return out
}

type historyWireField struct {
	Number int32 `json:"n"`
	Type   int8  `json:"t"`
}
type historyContainerShape struct {
	Layer     string             `json:"layer"`
	Path      []int32            `json:"path,omitempty"`
	Mask      [2]string          `json:"mask"`
	Unknown   []historyWireField `json:"unknown,omitempty"`
	Duplicate bool               `json:"duplicate,omitempty"`
	Complete  bool               `json:"complete"`
}
type historyDiagnosticV2 struct {
	Version    uint8                   `json:"version"`
	Schema     string                  `json:"schema"`
	Evidence   string                  `json:"evidence"`
	Layer      string                  `json:"layer,omitempty"`
	Reason     string                  `json:"reason,omitempty"`
	Tag        int32                   `json:"tag,omitempty"`
	Wire       int8                    `json:"wire,omitempty"`
	Overflow   bool                    `json:"overflow,omitempty"`
	Containers []historyContainerShape `json:"containers"`
}
type historyAnalysis struct {
	metadata, control bool
	diagnostic        historyDiagnosticV2
}

func (a historyAnalysis) reason() archive.HistoryUncertaintyReason {
	if a.control {
		return archive.HistoryUncertaintyUnsupportedControl
	}
	if a.metadata {
		return archive.HistoryUncertaintyUnsupportedMetadata
	}
	return ""
}
func historyLayer(desc protoreflect.MessageDescriptor) string {
	switch desc.FullName() {
	case "WAWebProtobufsHistorySync.HistorySync":
		return "envelope"
	case "WAWebProtobufsHistorySync.Conversation":
		return "conversation"
	case "WAWebProtobufsHistorySync.HistorySyncMsg":
		return "wrapper"
	case "WAWebProtobufsWeb.WebMessageInfo":
		return "web"
	case "WACommon.MessageKey":
		return "key"
	case "WAWebProtobufsHistorySync.PhoneNumberToLIDMapping":
		return "mapping"
	default:
		return "nested"
	}
}
func analyzeHistoryWire(ctx context.Context, raw []byte, desc protoreflect.MessageDescriptor) (historyAnalysis, error) {
	a := historyAnalysis{diagnostic: historyDiagnosticV2{Version: 2, Schema: "whatsmeow-6bb48c0f1ff0", Evidence: "exact_wire"}}
	budget := 4096
	err := a.walk(ctx, raw, desc, 0, nil, historyMetadata, &budget)
	return a, err
}
func (a *historyAnalysis) walk(ctx context.Context, raw []byte, desc protoreflect.MessageDescriptor, depth int, path []int32, inherited historyDisposition, budget *int) error {
	layer := historyLayer(desc)
	index := -1
	if len(a.diagnostic.Containers) < 8 {
		index = len(a.diagnostic.Containers)
		a.diagnostic.Containers = append(a.diagnostic.Containers, historyContainerShape{Layer: layer, Path: append([]int32(nil), path...), Mask: [2]string{"0000000000000000", "0000000000000000"}})
	} else {
		a.diagnostic.Overflow = true
	}
	fail := func(reason string, n protowire.Number, t protowire.Type, cause error) error {
		a.diagnostic.Layer = layer
		a.diagnostic.Reason = reason
		a.diagnostic.Tag = int32(n)
		a.diagnostic.Wire = int8(t)
		return cause
	}
	if depth > 32 {
		return fail("payload_limit", 0, 0, errLimitExceeded)
	}
	seen := map[protoreflect.FieldNumber]bool{}
	var mask [2]uint64
	err := walkWire(raw, func(n protowire.Number, t protowire.Type, value []byte, v uint64) error {
		if err := ctx.Err(); err != nil {
			return fail("canceled", n, t, err)
		}
		*budget--
		if *budget < 0 {
			return fail("field_budget", n, t, errLimitExceeded)
		}
		field := desc.Fields().ByNumber(protoreflect.FieldNumber(n))
		if field == nil {
			a.control = true
			if a.diagnostic.Reason == "" {
				a.diagnostic.Layer = layer
				a.diagnostic.Reason = "unknown_wire_field"
				a.diagnostic.Tag = int32(n)
				a.diagnostic.Wire = int8(t)
			}
			if index >= 0 && len(a.diagnostic.Containers[index].Unknown) < 4 {
				a.diagnostic.Containers[index].Unknown = append(a.diagnostic.Containers[index].Unknown, historyWireField{int32(n), int8(t)})
			} else {
				a.diagnostic.Overflow = true
			}
			return nil
		}
		if n < 128 {
			mask[n/64] |= uint64(1) << uint(n%64)
		} else {
			a.diagnostic.Overflow = true
		}
		if index >= 0 {
			a.diagnostic.Containers[index].Mask = [2]string{fmt.Sprintf("%016x", mask[0]), fmt.Sprintf("%016x", mask[1])}
		}
		if !field.IsList() && seen[field.Number()] {
			if index >= 0 {
				a.diagnostic.Containers[index].Duplicate = true
			}
			return fail("duplicate_singular", n, t, errHistoryCorrupt)
		}
		seen[field.Number()] = true
		disposition := inherited
		if root, ok := historyFieldDisposition[desc.FullName()]; ok {
			var found bool
			disposition, found = root[field.Number()]
			if !found {
				disposition = historyUnsupportedControl
			}
		}
		if disposition == historyUnsupportedControl {
			a.control = true
		}
		if disposition == historyUnsupportedMetadata {
			a.metadata = true
		}
		if (disposition == historyUnsupportedMetadata || disposition == historyUnsupportedControl) && a.diagnostic.Reason == "" {
			a.diagnostic.Layer = layer
			a.diagnostic.Reason = "unsupported_known_field"
			a.diagnostic.Tag = int32(n)
			a.diagnostic.Wire = int8(t)
		}
		expected := protowire.VarintType
		switch field.Kind() {
		case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind:
			expected = protowire.BytesType
		case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
			expected = protowire.Fixed32Type
		case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
			expected = protowire.Fixed64Type
		}
		validateScalar := func(scalar uint64) error {
			switch field.Kind() {
			case protoreflect.BoolKind:
				if scalar > 1 {
					return fail("out_of_range", n, t, errHistoryCorrupt)
				}
			case protoreflect.Uint32Kind, protoreflect.Sint32Kind:
				if scalar > math.MaxUint32 {
					return fail("out_of_range", n, t, errHistoryCorrupt)
				}
			case protoreflect.Int32Kind, protoreflect.EnumKind:
				if scalar > math.MaxInt32 && scalar < uint64(0xffffffff80000000) {
					return fail("out_of_range", n, t, errHistoryCorrupt)
				}
				if field.Kind() == protoreflect.EnumKind && field.Enum().Values().ByNumber(protoreflect.EnumNumber(int32(scalar))) == nil {
					a.control = true
				}
			}
			return nil
		}
		// Protobuf accepts packed and unpacked representations of any repeated
		// primitive field, regardless of the generator's preferred encoding.
		if field.IsList() && t == protowire.BytesType && expected != protowire.BytesType {
			for len(value) > 0 {
				*budget--
				if *budget < 0 {
					return fail("field_budget", n, t, errLimitExceeded)
				}
				if err := ctx.Err(); err != nil {
					return fail("canceled", n, t, err)
				}
				var scalar uint64
				var consumed int
				switch expected {
				case protowire.VarintType:
					scalar, consumed = protowire.ConsumeVarint(value)
				case protowire.Fixed32Type:
					consumed = 4
				case protowire.Fixed64Type:
					consumed = 8
				default:
					return fail("wrong_wire_type", n, t, errHistoryCorrupt)
				}
				if consumed < 0 || consumed > len(value) {
					return fail("malformed_wire", n, t, errHistoryCorrupt)
				}
				if err := validateScalar(scalar); err != nil {
					return err
				}
				value = value[consumed:]
			}
			return nil
		}
		if t != expected {
			return fail("wrong_wire_type", n, t, errHistoryCorrupt)
		}
		if err := validateScalar(v); err != nil {
			return err
		}
		switch field.Kind() {
		case protoreflect.StringKind:
			if !utf8.Valid(value) {
				return fail("invalid_text", n, t, errHistoryCorrupt)
			}
			if (layer == "conversation" && n == 1 || layer == "mapping") && !partialHistoryJID(string(value), layer == "conversation") {
				return fail("invalid_jid", n, t, errHistoryCorrupt)
			}
		case protoreflect.MessageKind:
			if disposition == historyRecordPayload || disposition == historyPositivePayload {
				return nil
			}
			childDisposition := disposition
			if childDisposition == historyContainer {
				childDisposition = historyMetadata
			}
			childPath := append(append([]int32(nil), path...), int32(n))
			if len(childPath) > 8 {
				childPath = childPath[:8]
				a.diagnostic.Overflow = true
			}
			return a.walk(ctx, value, field.Message(), depth+1, childPath, childDisposition, budget)
		}
		return nil
	})
	if err != nil {
		if a.diagnostic.Reason == "" || a.diagnostic.Reason == "unknown_wire_field" || a.diagnostic.Reason == "unsupported_known_field" {
			return fail("malformed_wire", 0, 0, err)
		}
		return err
	}
	if index >= 0 {
		a.diagnostic.Containers[index].Complete = true
	}
	return nil
}
func historyAnalysisError(err error, a historyAnalysis, web *waWeb.WebMessageInfo) error {
	if err == nil {
		return nil
	}
	var stage *historyStageError
	if !errors.As(err, &stage) {
		err = historyStage(err, "record_safety_metadata", historyStubClass(web))
		errors.As(err, &stage)
	}
	if stage != nil {
		copy := *stage
		diagnostic := a.diagnostic
		copy.diagnosticV2 = &diagnostic
		if web != nil {
			copy.presence = historyPresence(web)
			if web.MessageStubType != nil {
				copy.rawStub = int32(web.GetMessageStubType())
				copy.stubPresent = true
				copy.stubType = closedHistoryStubType(copy.rawStub)
			}
		}
		return &copy
	}
	return err
}
func envelopeHistoryDescriptor() protoreflect.MessageDescriptor {
	return (&waHistorySync.HistorySync{}).ProtoReflect().Descriptor()
}
func conversationHistoryDescriptor() protoreflect.MessageDescriptor {
	return (&waHistorySync.Conversation{}).ProtoReflect().Descriptor()
}
func recordHistoryDescriptor() protoreflect.MessageDescriptor {
	return (&waHistorySync.HistorySyncMsg{}).ProtoReflect().Descriptor()
}

// Prepend already-validated enclosing shapes without retaining enclosing bytes.
// Truncation is explicit: omitted containers never establish field absence.
func prependHistoryDiagnostic(err error, parent *historyAnalysis) error {
	if err == nil || parent == nil {
		return err
	}
	var stage *historyStageError
	if !errors.As(err, &stage) {
		return historyAnalysisError(err, *parent, nil)
	}
	if stage.diagnosticV2 == nil {
		return historyAnalysisError(err, *parent, nil)
	}
	copy := *stage
	d := *stage.diagnosticV2
	containers := append(append([]historyContainerShape(nil), parent.diagnostic.Containers...), d.Containers...)
	if len(containers) > 8 {
		containers = containers[:8]
		d.Overflow = true
	}
	d.Containers = containers
	d.Overflow = d.Overflow || parent.diagnostic.Overflow
	copy.diagnosticV2 = &d
	return &copy
}
