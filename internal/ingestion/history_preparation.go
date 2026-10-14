package ingestion

import (
	"context"
	"strings"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Preparation is a complete second wire walk, not a projection or a protobuf
// graph of the chunk. Only supported positive-only records may be overtaken.
// Any negative, unsupported shape, prefix-dependent authority or key-budget
// exhaustion keeps the barrier closed until the original ordered projection
// finishes. In particular, edits never guess original metadata in this pass.
func (r *Runtime) prepareHistoryOvertaking(ctx context.Context, raw []byte) error {
	ticket, _ := ctx.Value(sourceTicketContextKey{}).(*sourceTicket)
	if ticket == nil || !ticket.history {
		return nil
	}
	eligible := true
	analysis, err := analyzeHistoryWire(ctx, raw, envelopeHistoryDescriptor())
	if err != nil || analysis.reason() != "" {
		return nil // original strict/partial handling retains its exact behavior
	}
	keys := make(map[string]struct{})
	keyBytes := 0
	addKey := func(id string) {
		if !eligible {
			return
		}
		if _, present := keys[id]; present {
			return
		}
		// Include conservative map/string bookkeeping, not just protocol bytes.
		if len(keys) >= preparedKeyCount || len(id)+64 > preparedKeyBytes-keyBytes {
			eligible = false
			keys = nil
			return
		}
		keys[strings.Clone(id)] = struct{}{}
		keyBytes += len(id) + 64
	}
	err = walkWire(raw, func(number protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if number != 2 {
			return nil
		}
		if !eligible {
			return nil
		}
		if typ != protowire.BytesType {
			return historyStage(errHistoryCorrupt, "record_decode", "no_stub")
		}
		analysis, scanErr := analyzeHistoryWire(ctx, value, conversationHistoryDescriptor())
		if scanErr != nil || analysis.reason() != "" {
			eligible = false
			return nil
		}
		var chat string
		if err := walkWire(value, func(n protowire.Number, t protowire.Type, v []byte, _ uint64) error {
			if n == 1 {
				if t != protowire.BytesType || len(v) > 256 || chat != "" {
					return errHistoryCorrupt
				}
				chat = string(v)
			}
			return ctx.Err()
		}); err != nil {
			return historyStage(err, "conversation_id", "no_stub")
		}
		jid, err := types.ParseJID(chat)
		if err != nil || chat == "" || jid.User == "" {
			return historyStage(errHistoryCorrupt, "conversation_id", "no_stub")
		}
		return walkWire(value, func(n protowire.Number, t protowire.Type, v []byte, _ uint64) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n != 2 {
				return nil
			}
			if !eligible {
				return nil
			}
			if t != protowire.BytesType {
				return historyStage(errHistoryCorrupt, "record_decode", "no_stub")
			}
			if len(v) > recordMax {
				return historyStage(errLimitExceeded, "record_decode", "no_stub")
			}
			analysis, scanErr := analyzeHistoryWire(ctx, v, recordHistoryDescriptor())
			if scanErr != nil || analysis.reason() != "" {
				eligible = false
				return nil
			}
			var wrapper waHistorySync.HistorySyncMsg
			if (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(v, &wrapper) != nil {
				return historyStage(errHistoryCorrupt, "record_decode", "no_stub")
			}
			web := wrapper.GetMessage()
			if web == nil {
				return historyBodyMissing(web)
			}
			if web.GetMessage() == nil {
				eligible = false // bodyless authority may depend on preceding originals
				return nil
			}
			event, err := r.client.ParseWebMessage(jid, web)
			if err != nil {
				return historyStage(errHistoryCorrupt, "record_parse", "no_stub")
			}
			if err = validateMessageStructure(event); err != nil {
				return historyStage(err, "record_structure", "no_stub")
			}
			// ParseWebMessage unwraps DeviceSent/BotInvoke and other containers.
			// Inspect the complete authenticated root and its raw representation,
			// not only the normalized body, before declaring the chunk positive-only.
			ordinary := ordinaryContent(web.Message, false) && ordinaryContent(event.Message, false)
			if event.RawMessage != nil {
				ordinary = ordinary && ordinaryContent(event.RawMessage, false)
			}
			if isMutation(event) || event.IsEphemeral || rebuildViewOnce(event) || !ordinary || web.GetMessageStubType() != waWeb.WebMessageInfo_UNKNOWN || historyRetentionMetadata(web) || !event.Info.SenderAlt.IsEmpty() || !event.Info.RecipientAlt.IsEmpty() {
				eligible = false
				return nil
			}
			if claimed := web.GetKey().GetRemoteJID(); claimed != "" {
				expected, err := r.canonical(ctx, chat)
				if err != nil {
					return err
				}
				actual, err := r.canonical(ctx, claimed)
				if err != nil || expected != actual {
					return historyStage(errHistoryCorrupt, "record_jid_mismatch", "no_stub")
				}
			}
			// Store lookups are allowed, but this pass never grants an original,
			// installs names, refreshes a delivered token or spends recovery credit.
			if eligible {
				key, err := r.envelopeKey(ctx, event.Info)
				if err != nil {
					return err
				}
				if !utf8.ValidString(key.Message) || strings.ContainsRune(key.Message, 0) {
					eligible = false
					return nil
				}
				addKey(key.Message)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if eligible {
		r.publishHistoryPreparation(ctx, keys)
	}
	return nil
}

func historyRetentionMetadata(web *waWeb.WebMessageInfo) bool {
	return web.EphemeralStartTimestamp != nil || web.EphemeralDuration != nil || web.EphemeralExpirationTimestamp != nil || web.EphemeralOffToOn != nil || web.EphemeralOutOfSync != nil || web.KeepInChat != nil || len(web.FutureproofData) != 0 || web.TargetMessageID != nil || len(web.MessageAddOns) != 0 || web.QuarantinedMessage != nil || web.GroupHistoryBundleInfo != nil
}
