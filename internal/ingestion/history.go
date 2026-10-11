package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func (r *Runtime) historyFrom(ctx context.Context, event *events.Message) error {
	if err := r.ensureAccount(ctx); err != nil {
		return err
	}
	own, err := r.canonical(ctx, r.SourceAccount())
	if err != nil {
		return err
	}
	sender, err := r.canonical(ctx, event.Info.Sender.ToNonAD().String())
	if err != nil {
		return err
	}
	lid := r.client.Store.LID.ToNonAD().String()
	verifiedLID := !r.client.Store.LID.IsEmpty() && event.Info.Sender.ToNonAD().String() == lid
	if !event.Info.IsFromMe || (sender != own && !verifiedLID) {
		return archive.ErrInvalidEvent
	}
	// Only the protected session's own identity may authorize mapping/history.
	if !r.client.Store.LID.IsEmpty() {
		if err := r.alias(ctx, r.SourceAccount(), lid, archive.AliasSessionMetadata); err != nil {
			return err
		}
	}
	notification := event.Message.GetProtocolMessage().GetHistorySyncNotification()
	if notification == nil {
		return archive.ErrInvalidEvent
	}
	return r.history(ctx, notification)
}
func (r *Runtime) history(ctx context.Context, notification *waE2E.HistorySyncNotification) error {
	var reader io.Reader
	var staged *boundedMemoryFile
	if inline := notification.GetInitialHistBootstrapInlinePayload(); len(inline) > 0 {
		if int64(len(inline)) > compressedMax {
			r.failClosed("history_limit_exceeded")
			return errLimitExceeded
		}
		reader = bytes.NewReader(inline)
	} else {
		if notification.GetFileLength() > uint64(compressedMax) {
			r.failClosed("history_limit_exceeded")
			return errLimitExceeded
		}
		var err error
		staged, err = newMemoryFile(ctx, compressedMax)
		if err != nil {
			return err
		}
		defer staged.Close()
		downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r.activeMu.Lock()
		r.activeCancel = cancel
		r.activeMu.Unlock()
		err = r.downloadAttachment(downloadCtx, notification, staged)
		cancel()
		r.activeMu.Lock()
		r.activeCancel = nil
		r.activeMu.Unlock()
		if err != nil {
			err = historyStage(err, "download", "no_stub")
			r.historyFailure(err)
			return err
		}
		if _, err = staged.Seek(0, io.SeekStart); err != nil {
			err = historyStage(err, "download", "no_stub")
			r.historyFailure(err)
			return err
		}
		reader = staged
	}
	raw, err := decodeHistory(ctx, reader, compressedMax, decodedMax)
	if err != nil {
		err = historyStage(err, "decode", "no_stub")
		r.historyFailure(err)
		return err
	}
	if staged != nil {
		_ = staged.Close()
	}
	defer clear(raw)
	if err = r.projectHistory(ctx, raw); err != nil {
		return err
	}
	r.mu.Lock()
	r.status.HistoryChunks++
	r.mu.Unlock()
	return r.requestBackfill(ctx)
}
func (r *Runtime) historyFailure(err error) {
	r.historyFailureObserved(err, !r.Status().ReconciliationRequired)
}

// Histories are consumed by the single runtime worker. Capture admission before
// projection, since a structural projection error can already fence the runtime.
func (r *Runtime) historyFailureObserved(err error, first bool) {
	if first {
		_, _ = os.Stderr.Write(historyFailureDiagnostic(err))
	}
	code := "history_corrupt"
	if errors.Is(err, errLimitExceeded) {
		code = "history_limit_exceeded"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = "reconciliation_required"
	}
	r.failClosed(code)
}

// Never format a provider error: it can contain message bodies, identities, or
// signed download URLs. Error and diagnostic output use closed constants only.
type historyStageError struct {
	stage, stub string
	stubType    int32
	cause       error
}

func (e *historyStageError) Error() string { return e.stage }
func (e *historyStageError) Unwrap() error { return e.cause }

func historyStage(err error, stage, stub string) error {
	if err == nil {
		return nil
	}
	var existing *historyStageError
	if errors.As(err, &existing) {
		return err
	}
	return &historyStageError{stage: closedHistoryStage(stage), stub: closedHistoryStub(stub), stubType: -1, cause: err}
}

func historyBodyMissing(message *waWeb.WebMessageInfo) error {
	return historyBodylessFailure(message, "record_body_missing")
}

func historyBodylessFailure(message *waWeb.WebMessageInfo, stage string) error {
	stubType := int32(-1)
	if message != nil && message.MessageStubType != nil {
		stubType = closedHistoryStubType(int32(message.GetMessageStubType()))
	}
	return &historyStageError{stage: closedHistoryStage(stage), stub: historyStubClass(message), stubType: stubType, cause: errHistoryCorrupt}
}

func closedHistoryStubType(value int32) int32 {
	if _, known := waWeb.WebMessageInfo_StubType_name[value]; known {
		return value
	}
	return -1
}

func closedHistoryStage(stage string) string {
	switch stage {
	case "download", "decode", "sync_header", "mapping", "conversation_id", "record_decode", "record_body_missing", "record_safety_metadata", "record_jid_mismatch", "record_parse", "record_structure", "record_projection":
		return stage
	case "admin_key_invalid", "admin_chat_invalid", "admin_actor_invalid", "admin_author_invalid", "admin_target_missing", "admin_target_ambiguous", "admin_direction_mismatch", "admin_author_mismatch", "admin_envelope_invalid", "admin_store_failure":
		return stage
	default:
		return "unknown"
	}
}

func closedHistoryStub(stub string) string {
	switch stub {
	case "revoke", "admin_revoke", "ciphertext", "other_stub", "no_stub":
		return stub
	default:
		return "no_stub"
	}
}

func historyStubClass(message *waWeb.WebMessageInfo) string {
	if message == nil {
		return "no_stub"
	}
	switch message.GetMessageStubType() {
	case waWeb.WebMessageInfo_UNKNOWN:
		return "no_stub"
	case waWeb.WebMessageInfo_REVOKE:
		return "revoke"
	case waWeb.WebMessageInfo_ADMIN_REVOKE:
		return "admin_revoke"
	case waWeb.WebMessageInfo_CIPHERTEXT:
		return "ciphertext"
	default:
		return "other_stub"
	}
}

func historyFailureDiagnostic(err error) []byte {
	stage, stub := "unknown", "no_stub"
	stubType := int32(-1)
	var observed *historyStageError
	if errors.As(err, &observed) {
		stage, stub = closedHistoryStage(observed.stage), closedHistoryStub(observed.stub)
		stubType = closedHistoryStubType(observed.stubType)
	}
	encoded, _ := json.Marshal(struct {
		Event    string `json:"event"`
		Stage    string `json:"stage"`
		Stub     string `json:"stub"`
		StubType int32  `json:"stub_type"`
	}{Event: "history_failure", Stage: stage, Stub: stub, StubType: stubType})
	return append(encoded, '\n')
}

// Walk only the bounded decoded bytes. Unneeded fields are skipped without
// building the connector's potentially unbounded HistorySync object graph.
func walkWire(raw []byte, visit func(protowire.Number, protowire.Type, []byte, uint64) error) error {
	for len(raw) > 0 {
		number, typ, n := protowire.ConsumeTag(raw)
		if n < 0 || number < 1 {
			return errHistoryCorrupt
		}
		raw = raw[n:]
		var value []byte
		var integer uint64
		switch typ {
		case protowire.BytesType:
			value, n = protowire.ConsumeBytes(raw)
		case protowire.VarintType:
			integer, n = protowire.ConsumeVarint(raw)
		case protowire.Fixed32Type:
			n = 4
		case protowire.Fixed64Type:
			n = 8
		default:
			return errHistoryCorrupt
		}
		if n < 0 || n > len(raw) {
			return errHistoryCorrupt
		}
		if err := visit(number, typ, value, integer); err != nil {
			return err
		}
		raw = raw[n:]
	}
	return nil
}
func (r *Runtime) projectHistory(ctx context.Context, raw []byte) (err error) {
	first := !r.Status().ReconciliationRequired
	defer func() {
		if err != nil {
			r.historyFailureObserved(err, first)
		}
	}()
	if r.Status().ReconciliationRequired {
		return archive.ErrReconciliationRequired
	}
	if err = r.ensureAccount(ctx); err != nil {
		return historyStage(err, "mapping", "no_stub")
	}
	if int64(len(raw)) > decodedMax {
		return historyStage(errLimitExceeded, "decode", "no_stub")
	}
	syncType := -1
	err = walkWire(raw, func(number protowire.Number, typ protowire.Type, _ []byte, v uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if number == 1 {
			if typ != protowire.VarintType || v > 6 || syncType != -1 {
				return errHistoryCorrupt
			}
			syncType = int(v)
		}
		return nil
	})
	if err != nil {
		return historyStage(err, "sync_header", "no_stub")
	}
	if syncType < 0 {
		return historyStage(errHistoryCorrupt, "sync_header", "no_stub")
	}
	err = walkWire(raw, func(number protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if number != 15 {
			return nil
		}
		if typ != protowire.BytesType || len(value) > 1024 {
			return errHistoryCorrupt
		}
		var m waHistorySync.PhoneNumberToLIDMapping
		if err := (proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 32}).Unmarshal(value, &m); err != nil {
			return errHistoryCorrupt
		}
		return r.alias(ctx, m.GetPnJID(), m.GetLidJID(), archive.AliasConnectorMapping)
	})
	if err != nil {
		return historyStage(err, "mapping", "no_stub")
	}
	err = walkWire(raw, func(number protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if number != 2 {
			return nil
		}
		if typ != protowire.BytesType {
			return errHistoryCorrupt
		}
		return r.projectConversation(ctx, value)
	})
	if err != nil {
		return historyStage(err, "record_decode", "no_stub")
	}
	return historyStage(r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: r.SourceAccount(), CoverageUnknown: true}), "record_projection", "no_stub")
}
func (r *Runtime) projectConversation(ctx context.Context, raw []byte) error {
	var chat, name, displayName string
	if err := walkWire(raw, func(number protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if number == 1 {
			if typ != protowire.BytesType || len(value) > 256 || chat != "" {
				return errHistoryCorrupt
			}
			chat = string(value)
		}
		if (number == 13 || number == 38) && typ == protowire.BytesType && len(value) <= 256 {
			if number == 13 {
				name = boundedName(string(value))
			} else {
				displayName = boundedName(string(value))
			}
		}
		return nil
	}); err != nil {
		return historyStage(err, "conversation_id", "no_stub")
	}
	jid, err := types.ParseJID(chat)
	if err != nil || chat == "" || jid.User == "" {
		return historyStage(errHistoryCorrupt, "conversation_id", "no_stub")
	}
	if displayName != "" {
		name = displayName
	}
	if name != "" {
		canonical, err := r.canonical(ctx, chat)
		if err != nil {
			return historyStage(err, "mapping", "no_stub")
		}
		if err = r.store.SetSourceNames(ctx, archive.SourceNames{Account: r.SourceAccount(), Chat: canonical, ChatName: name}); err != nil {
			return historyStage(err, "record_projection", "no_stub")
		}
	}
	return walkWire(raw, func(number protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.Status().ReconciliationRequired {
			return archive.ErrReconciliationRequired
		}
		if number != 2 {
			return nil
		}
		if typ != protowire.BytesType {
			return historyStage(errHistoryCorrupt, "record_decode", "no_stub")
		}
		if len(value) > recordMax {
			return historyStage(errLimitExceeded, "record_decode", "no_stub")
		}
		var wrapper waHistorySync.HistorySyncMsg
		if err := (proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 32}).Unmarshal(value, &wrapper); err != nil {
			return historyStage(errHistoryCorrupt, "record_decode", "no_stub")
		}
		if wrapper.GetMessage() == nil {
			return historyBodyMissing(wrapper.GetMessage())
		}
		// ParseWebMessage accepts a supplied chat over a claimed key. Verify both.
		if claimed := wrapper.GetMessage().GetKey().GetRemoteJID(); claimed != "" {
			expected, err := r.canonical(ctx, chat)
			if err != nil {
				return historyStage(err, "record_jid_mismatch", "no_stub")
			}
			actual, err := r.canonical(ctx, claimed)
			if err != nil || expected != actual {
				return historyStage(errHistoryCorrupt, "record_jid_mismatch", "no_stub")
			}
		}
		if wrapper.GetMessage().GetMessage() == nil {
			return r.projectBodylessHistory(ctx, jid, wrapper.GetMessage())
		}
		event, err := r.client.ParseWebMessage(jid, wrapper.GetMessage())
		if err != nil {
			return historyStage(errHistoryCorrupt, "record_parse", "no_stub")
		}
		if err = validateMessageStructure(event); err != nil {
			return historyStage(err, "record_structure", "no_stub")
		}
		// Preserve original protocol authority: the pinned parser replaces edits with
		// their edited body. RawMessage retains the validated key and mutation order.
		if isMutation(event) {
			return historyStage(r.messageWithMediaOutcome(ctx, event), "record_projection", "no_stub")
		}
		return historyStage(r.messageWithMediaOutcome(ctx, event), "record_projection", "no_stub")
	})
}
func (r *Runtime) downloadAttachment(ctx context.Context, message whatsmeow.DownloadableMessage, file whatsmeow.File) error {
	if r.download != nil {
		return r.download(ctx, message, file)
	}
	return r.client.DownloadToFile(ctx, message, file)
}
func (r *Runtime) requestBackfill(ctx context.Context) error {
	if r.config.BackfillRequests == 0 {
		return nil
	}
	policy, err := r.store.SourcePolicy(ctx, r.SourceAccount())
	if err != nil {
		return mediaFailure(err, ErrMediaUnavailable)
	}
	return r.enqueueBackfill(BackfillRequest{PolicyGeneration: policy.Generation, AllSelected: true, Limit: 32}, true)
}
