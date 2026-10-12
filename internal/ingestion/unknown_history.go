package ingestion

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/encoding/protowire"
)

type unknownHistoryGapStore interface {
	RecordUnknownHistoryGap(context.Context, string, int32) error
}

func undeclaredHistoryStub(web *waWeb.WebMessageInfo) bool {
	if web == nil || web.MessageStubType == nil || int32(web.GetMessageStubType()) <= 0 {
		return false
	}
	_, known := waWeb.WebMessageInfo_StubType_name[int32(web.GetMessageStubType())]
	return !known
}

// Only projectConversationContext calls this after validating the entire record
// and its enclosing metadata. Direct phone-copy and live projections stay strict.
func (r *Runtime) projectUnknownHistoryGap(ctx context.Context, chat types.JID, web *waWeb.WebMessageInfo) error {
	fail := func() error { return historyBodylessFailure(web, "record_structure") }
	if !undeclaredHistoryStub(web) || web.GetMessage() != nil || web.GetKey() == nil {
		return fail()
	}
	key := web.GetKey()
	if key.ID == nil || key.GetID() == "" || key.RemoteJID == nil || key.GetRemoteJID() == "" || key.FromMe == nil {
		return fail()
	}
	canonicalChat, err := r.canonical(ctx, chat.ToNonAD().String())
	if err != nil {
		return historyStage(err, "record_jid_mismatch", "other_stub")
	}
	canonicalJID, err := types.ParseJID(canonicalChat)
	if err != nil || canonicalJID.User == "" || (canonicalJID.Server != types.GroupServer && canonicalJID.Server != types.DefaultUserServer && canonicalJID.Server != types.HiddenUserServer) {
		return fail()
	}
	remote, err := r.canonical(ctx, key.GetRemoteJID())
	if err != nil || remote != canonicalChat {
		return fail()
	}
	var author string
	for _, claim := range []*string{key.Participant, web.Participant} {
		if claim == nil {
			continue
		}
		canonical, err := r.historyUserClaim(ctx, *claim)
		if err != nil || (author != "" && author != canonical) {
			return fail()
		}
		author = canonical
	}
	if author != "" {
		own, err := r.historyUserClaim(ctx, r.SourceAccount())
		if err != nil || key.GetFromMe() != (author == own) {
			return fail()
		}
		if !key.GetFromMe() && canonicalJID.Server != types.GroupServer && author != canonicalChat {
			return fail()
		}
	}
	store, ok := r.store.(unknownHistoryGapStore)
	if !ok {
		return historyBodylessFailure(web, "record_projection")
	}
	if err := store.RecordUnknownHistoryGap(ctx, r.SourceAccount(), int32(web.GetMessageStubType())); err != nil {
		return historyStage(err, "record_projection", "other_stub")
	}
	r.mu.Lock()
	found := false
	for _, code := range r.status.Codes {
		found = found || code == archive.UnknownHistoryGapCode
	}
	if !found {
		r.status.Codes = append(r.status.Codes, archive.UnknownHistoryGapCode)
	}
	r.mu.Unlock()
	return nil
}

// A present claim must already be a canonical wire JID. ParseJID and ToNonAD
// are intentionally permissive; normalization must not rescue malformed claims.
func partialHistoryJID(raw string, allowGroup bool) bool {
	if raw == "" || len(raw) > 256 || !utf8.ValidString(raw) || strings.Count(raw, "@") != 1 || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" || jid.String() != raw || jid.ToNonAD() != jid {
		return false
	}
	return jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer || (allowGroup && jid.Server == types.GroupServer)
}

type partialHistoryShape uint8

const (
	partialHistoryEnvelope partialHistoryShape = iota
	partialHistoryConversation
	partialHistoryRecord
	partialHistoryWebMessage
	partialHistoryKey
	partialHistoryMapping
)

// This fixed-depth wire walk checks every PRESENT field, including explicit zero
// values and duplicate singular claims which protobuf would otherwise overwrite.
// Only reviewed scalar metadata and the wrapper/web/key chain are eligible. All
// other known or unknown fields reject partial handling, including nested controls.
// Conversation message payloads are checked individually under the record limit;
// no whole-history object graph or raw record is retained.
func partialHistoryWire(ctx context.Context, raw []byte, shape partialHistoryShape) bool {
	budget := 4096
	return validatePartialHistoryWire(ctx, raw, shape, &budget) == nil
}

func validatePartialHistoryWire(ctx context.Context, raw []byte, shape partialHistoryShape, budget *int) error {
	var seen uint64
	return walkWire(raw, func(number protowire.Number, typ protowire.Type, value []byte, integer uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*budget -= 1
		if *budget < 0 || number < 1 || number >= 64 {
			return errHistoryCorrupt
		}
		repeated := (shape == partialHistoryEnvelope && (number == 2 || number == 15)) || (shape == partialHistoryConversation && number == 2)
		mask := uint64(1) << uint(number)
		if !repeated && seen&mask != 0 {
			return errHistoryCorrupt
		}
		seen |= mask
		varint := func(max uint64) error {
			if typ != protowire.VarintType || integer > max {
				return errHistoryCorrupt
			}
			return nil
		}
		text := func(max int) error {
			if typ != protowire.BytesType || len(value) > max || !utf8.Valid(value) || strings.IndexByte(string(value), 0) >= 0 {
				return errHistoryCorrupt
			}
			return nil
		}
		jid := func(allowGroup bool) error {
			if err := text(256); err != nil || !partialHistoryJID(string(value), allowGroup) {
				return errHistoryCorrupt
			}
			return nil
		}
		payload := func(max int) error {
			if typ != protowire.BytesType || len(value) > max {
				return errHistoryCorrupt
			}
			return nil
		}
		child := func(next partialHistoryShape, max int) error {
			if err := payload(max); err != nil {
				return err
			}
			return validatePartialHistoryWire(ctx, value, next, budget)
		}
		switch shape {
		case partialHistoryEnvelope:
			switch number {
			case 1:
				return varint(6)
			case 2:
				return payload(int(decodedMax))
			case 5, 6:
				return varint(1<<32 - 1)
			case 15:
				return child(partialHistoryMapping, 1024)
			}
		case partialHistoryConversation:
			switch number {
			case 1:
				return jid(true)
			case 2:
				return payload(recordMax)
			case 5, 12, 25, 31:
				return varint(^uint64(0))
			case 6, 18, 24, 45:
				return varint(1<<32 - 1)
			case 8, 15, 16, 19:
				return varint(1)
			case 11:
				return varint(3)
			case 13, 33, 38, 43:
				return text(4096)
			}
		case partialHistoryRecord:
			switch number {
			case 1:
				return child(partialHistoryWebMessage, recordMax)
			case 2:
				return varint(^uint64(0))
			}
		case partialHistoryWebMessage:
			switch number {
			case 1:
				return child(partialHistoryKey, 1024)
			case 3, 6:
				return varint(^uint64(0))
			case 4:
				return varint(5)
			case 5:
				return jid(false)
			case 24:
				return varint(1<<31 - 1)
			}
		case partialHistoryKey:
			switch number {
			case 1:
				return jid(true)
			case 3:
				return text(256)
			case 4:
				return jid(false)
			case 2:
				return varint(1)
			}
		case partialHistoryMapping:
			if number == 1 || number == 2 {
				return jid(false)
			}
		}
		return errHistoryCorrupt
	})
}
