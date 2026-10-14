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
	ctx = r.captureHistoryObservation(ctx)
	delivered, err := r.captureDeliveredHistory(ctx)
	if err != nil {
		return err
	}
	return r.historyWithDelivered(ctx, notification, delivered)
}
func (r *Runtime) history(ctx context.Context, notification *waE2E.HistorySyncNotification) error {
	return r.historyWithDelivered(ctx, notification, nil)
}
func (r *Runtime) historyWithDelivered(ctx context.Context, notification *waE2E.HistorySyncNotification, delivered *deliveredHistoryChunk) error {
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
		// A history download owns source records, including controls we have not
		// seen yet. Received privacy controls may interrupt a media transfer, but
		// must not cancel this source envelope through the media cancellation slot.
		// Its bounded context still follows run cancellation on a genuine failure.
		downloadStarted := time.Now()
		err = r.downloadAttachment(downloadCtx, notification, staged)
		r.latency.record(latencyHistory, latencyDownload, time.Since(downloadStarted))
		cancel()
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
	decodeStarted := time.Now()
	raw, err := decodeHistory(ctx, reader, compressedMax, decodedMax)
	r.latency.record(latencyHistory, latencyDecode, time.Since(decodeStarted))
	if err != nil {
		err = historyStage(err, "decode", "no_stub")
		r.historyFailure(err)
		return err
	}
	if staged != nil {
		_ = staged.Close()
	}
	defer clear(raw)
	if err = r.projectHistoryWithDelivered(ctx, raw, delivered); err != nil {
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
// signed download URLs. Diagnostics use closed codes and bounded protocol scalars.
type historyStageError struct {
	stage, stub  string
	stubType     int32
	rawStub      int32
	stubPresent  bool
	presence     uint32
	diagnosticV2 *historyDiagnosticV2
	cause        error
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
	witness := &historyStageError{stage: closedHistoryStage(stage), stub: historyStubClass(message), stubType: -1, cause: errHistoryCorrupt}
	if message != nil && message.MessageStubType != nil {
		witness.rawStub = int32(message.GetMessageStubType())
		witness.stubPresent = true
		witness.stubType = closedHistoryStubType(witness.rawStub)
	}
	witness.presence = historyPresence(message)
	return witness
}

// Bit positions are a fixed private diagnostic schema. Only presence is retained:
// 0 web, 1 key, 2 body, 3 key ID, 4 remote JID, 5 key participant,
// 6 web participant, 7 from-me, 8 stub parameters, 9 ephemeral start,
// 10 duration, 11 expiration, 12 off-to-on, 13 out-of-sync, 14 keep-in-chat,
// 15 futureproof, 16 target ID, 17 add-ons, 18 quarantined, 19 history bundle.
// Pointer presence includes explicit zero values; repeated/byte fields use length.
func historyPresence(message *waWeb.WebMessageInfo) uint32 {
	if message == nil {
		return 0
	}
	key := message.GetKey()
	flags := [...]bool{
		true, key != nil, message.Message != nil,
		key != nil && key.ID != nil, key != nil && key.RemoteJID != nil,
		key != nil && key.Participant != nil, message.Participant != nil,
		key != nil && key.FromMe != nil, len(message.MessageStubParameters) != 0,
		message.EphemeralStartTimestamp != nil, message.EphemeralDuration != nil,
		message.EphemeralExpirationTimestamp != nil, message.EphemeralOffToOn != nil,
		message.EphemeralOutOfSync != nil, message.KeepInChat != nil,
		len(message.FutureproofData) != 0, message.TargetMessageID != nil,
		len(message.MessageAddOns) != 0, message.QuarantinedMessage != nil,
		message.GroupHistoryBundleInfo != nil,
	}
	var presence uint32
	for bit, present := range flags {
		if present {
			presence |= 1 << bit
		}
	}
	return presence
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
	var rawStub int32
	var stubPresent, stubKnown bool
	var presence uint32
	var observed *historyStageError
	if errors.As(err, &observed) {
		stage, stub = closedHistoryStage(observed.stage), closedHistoryStub(observed.stub)
		stubType = closedHistoryStubType(observed.stubType)
		stubPresent = observed.stubPresent
		if stubPresent {
			rawStub = observed.rawStub
			_, stubKnown = waWeb.WebMessageInfo_StubType_name[rawStub]
		}
		presence = observed.presence & ((1 << 20) - 1)
	}
	// Fixed strings, booleans and at most 32-bit numbers keep this first-failure
	// legacy record below 1 KiB. Optional v2 has at most eight containers and
	// four unknown tags each, bounded to 4 KiB without any provider values.
	encoded, _ := json.Marshal(struct {
		Event        string               `json:"event"`
		Stage        string               `json:"stage"`
		Stub         string               `json:"stub"`
		StubType     int32                `json:"stub_type"`
		RawStub      int32                `json:"raw_stub"`
		StubPresent  bool                 `json:"stub_present"`
		StubKnown    bool                 `json:"stub_known"`
		Presence     uint32               `json:"presence"`
		DiagnosticV2 *historyDiagnosticV2 `json:"v2,omitempty"`
	}{Event: "history_failure", Stage: stage, Stub: stub, StubType: stubType,
		RawStub: rawStub, StubPresent: stubPresent, StubKnown: stubKnown, Presence: presence, DiagnosticV2: func() *historyDiagnosticV2 {
			if observed != nil {
				return observed.diagnosticV2
			}
			return nil
		}()})
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
func (r *Runtime) projectHistory(ctx context.Context, raw []byte) error {
	return r.projectHistoryWithDelivered(ctx, raw, nil)
}
func (r *Runtime) projectHistoryWithDelivered(ctx context.Context, raw []byte, delivered *deliveredHistoryChunk) (err error) {
	preparationStarted := time.Now()
	preparationRecorded := false
	defer func() {
		if !preparationRecorded {
			r.latency.record(latencyHistory, latencyPreparation, time.Since(preparationStarted))
		}
	}()
	ctx = r.captureHistoryObservation(ctx)
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
	partialContext := r.config.UnknownHistoryPolicy == UnknownHistoryPartial
	var envelopeAnalysis *historyAnalysis
	if partialContext {
		analysis, scanErr := analyzeHistoryWire(ctx, raw, envelopeHistoryDescriptor())
		envelopeAnalysis = &analysis
		if scanErr != nil {
			return historyAnalysisError(scanErr, analysis, nil)
		}
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
		return prependHistoryDiagnostic(historyStage(err, "sync_header", "no_stub"), envelopeAnalysis)
	}
	if syncType < 0 {
		return prependHistoryDiagnostic(historyStage(errHistoryCorrupt, "sync_header", "no_stub"), envelopeAnalysis)
	}
	if envelopeAnalysis != nil {
		if reason := envelopeAnalysis.reason(); reason != "" {
			if gapErr := r.recordHistoryUncertainty(ctx, reason); gapErr != nil {
				return historyAnalysisError(gapErr, *envelopeAnalysis, nil)
			}
		}
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
		if partialContext {
			mapping, scanErr := analyzeHistoryWire(ctx, value, (&waHistorySync.PhoneNumberToLIDMapping{}).ProtoReflect().Descriptor())
			if scanErr != nil {
				return historyAnalysisError(scanErr, mapping, nil)
			}
			if mapping.reason() != "" {
				return nil
			} // parent uncertainty already committed; this mapping grants no identity
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
	err = r.prepareHistoryOvertaking(ctx, raw)
	r.latency.record(latencyHistory, latencyPreparation, time.Since(preparationStarted))
	preparationRecorded = true
	if err != nil {
		return err
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
		return prependHistoryDiagnostic(r.projectConversationDelivered(ctx, value, partialContext, delivered), envelopeAnalysis)
	})
	if err != nil {
		return historyStage(err, "record_decode", "no_stub")
	}
	if err = r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: r.SourceAccount(), CoverageUnknown: true}); err != nil {
		return historyStage(err, "record_projection", "no_stub")
	}
	return historyStage(r.commitDeliveredFrontiers(ctx, delivered), "record_projection", "no_stub")
}
func (r *Runtime) projectConversation(ctx context.Context, raw []byte) error {
	// A standalone conversation lacks an authenticated complete history context.
	return r.projectConversationContext(ctx, raw, false)
}
func (r *Runtime) projectConversationContext(ctx context.Context, raw []byte, partialContext bool) error {
	return r.projectConversationDelivered(ctx, raw, partialContext, nil)
}
func (r *Runtime) projectConversationDelivered(ctx context.Context, raw []byte, partialContext bool, delivered *deliveredHistoryChunk) (result error) {
	var conversationAnalysis *historyAnalysis
	defer func() { result = prependHistoryDiagnostic(result, conversationAnalysis) }()
	if partialContext {
		analysis, err := analyzeHistoryWire(ctx, raw, conversationHistoryDescriptor())
		conversationAnalysis = &analysis
		if err != nil {
			return historyAnalysisError(err, analysis, nil)
		}
		if reason := analysis.reason(); reason != "" {
			if err := r.recordHistoryUncertainty(ctx, reason); err != nil {
				return historyAnalysisError(err, analysis, nil)
			}
		}
	}
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
		var recordAnalysis historyAnalysis
		if partialContext {
			var scanErr error
			recordAnalysis, scanErr = analyzeHistoryWire(ctx, value, recordHistoryDescriptor())
			if scanErr != nil {
				return historyAnalysisError(scanErr, recordAnalysis, nil)
			}
		}
		var wrapper waHistorySync.HistorySyncMsg
		if err := (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(value, &wrapper); err != nil {
			failure := historyStage(errHistoryCorrupt, "record_decode", "no_stub")
			if partialContext {
				return historyAnalysisError(failure, recordAnalysis, nil)
			}
			return failure
		}
		if wrapper.GetMessage() == nil {
			return historyBodyMissing(wrapper.GetMessage())
		}
		if partialContext && wrapper.GetMessage().GetMessage() == nil {
			return historyAnalysisError(r.projectPartialHistoryRecord(ctx, jid, wrapper.GetMessage(), recordAnalysis), recordAnalysis, wrapper.GetMessage())
		}
		if r.config.UnknownHistoryPolicy == UnknownHistoryPartial && undeclaredHistoryStub(wrapper.GetMessage()) {
			return historyBodylessFailure(wrapper.GetMessage(), "record_safety_metadata")
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
		if delivered != nil {
			return historyStage(r.projectDeliveredHistoryMessage(ctx, event, delivered), "record_projection", "no_stub")
		}
		// Preserve original protocol authority: the pinned parser replaces edits with
		// their edited body. RawMessage retains the validated key and mutation order.
		if r.config.Rebuild != nil {
			if isMutation(event) {
				return historyStage(r.rebuildHistoryMutation(ctx, event), "record_projection", "no_stub")
			}
			return historyStage(r.rebuildDiscovery(ctx, event), "record_projection", "no_stub")
		}
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
	if r.config.Rebuild != nil {
		r.WakeRebuild()
		return nil
	}
	if r.config.BackfillRequests == 0 {
		return nil
	}
	policy, err := r.store.SourcePolicy(ctx, r.SourceAccount())
	if err != nil {
		return mediaFailure(err, ErrMediaUnavailable)
	}
	return r.enqueueBackfill(BackfillRequest{PolicyGeneration: policy.Generation, AllSelected: true, Limit: 32}, true)
}
