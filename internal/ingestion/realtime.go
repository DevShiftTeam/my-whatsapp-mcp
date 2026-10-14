package ingestion

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Each admitted or bounded waiting callback has one ordering ticket. The two
// workers borrow the original admission ledger; these are not additional queues
// with independent payload budgets. Runtime.mu owns every ticket and its keys.
type sourceTicket struct {
	sequence                          uint64
	live, history, admitted, prepared bool
	queued, waiting                   bool
	receivedAt                        time.Time
	deadline                          time.Time
	bytes                             int64
	messageID                         string
	keys                              map[string]struct{}
}

type sourceTicketContextKey struct{}
type sourceLatencyContextKey struct{}
type sourceLatencyTrace struct {
	lane       latencyLane
	receivedAt time.Time
}

func (t *sourceTicket) latencyLane() latencyLane {
	if t == nil {
		return latencySource
	}
	if t.live {
		return latencyLive
	}
	if t.history {
		return latencyHistory
	}
	return latencySource
}

// Lock order is Runtime.mu then latency.mu. Only admitted tickets contribute to
// queued/in-flight diagnostics; separately bounded waiting callbacks stay in the
// waiting ledger. No private ordering keys enter the exported snapshot.
func (r *Runtime) IngestionLatency() LatencySnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := LatencySnapshot{Running: !r.quiesced, Unsafe: r.status.ReconciliationRequired, QueuedCount: r.admissions, QueuedBytes: r.queueBytes, WaitingCount: r.waitingAdmissions, WaitingBytes: r.waitingBytes}
	for _, ticket := range r.sourceTickets {
		if !ticket.admitted {
			continue
		}
		if ticket.live {
			s.LivePending++
			s.OldestLiveAgeMS = max(s.OldestLiveAgeMS, elapsedMS(time.Since(ticket.receivedAt)))
		}
		if ticket.history {
			s.HistoryPending++
			s.HistoryPrepared = s.HistoryPrepared || ticket.prepared
		}
	}
	s.Phases = r.latency.snapshot()
	return s
}

// A conservative cross-chat message-ID barrier survives canonical alias changes.
// Order-sensitive or unknown envelopes never receive a prepared ticket.
const preparedKeyCount = 4096
const preparedKeyBytes = 1 << 20

func historyNotification(event any) bool {
	m, ok := event.(*events.Message)
	return ok && m != nil && m.Message.GetProtocolMessage().GetHistorySyncNotification() != nil
}

// Only simple, ordinary text gets the additional worker. Recovery responses,
// mutations and wrappers with retention authority keep their original ordering.
func (r *Runtime) realtimePlaintext(event any) bool {
	if !r.realtimeTextShape(event) {
		return false
	}
	e := event.(*events.Message)
	return !hasAlternateIDs(e)
}

// Pure shape classification may run under Runtime.mu. Committed alternate-ID
// prerequisites are resolved only after the callback has an owned ordering
// ticket and bounded admission reservation, never under this lock.
func (r *Runtime) realtimeTextShape(event any) bool {
	e, ok := event.(*events.Message)
	if !ok || e == nil || e.Message == nil || e.SourceWebMsg != nil || e.UnavailableRequestID != "" || isMutation(e) || e.IsEphemeral || rebuildViewOnce(e) || e.Info.ID == "" || len(e.Info.ID) > 256 || !utf8.ValidString(e.Info.ID) || strings.ContainsRune(e.Info.ID, 0) {
		return false
	}
	if e.Info.Timestamp.IsZero() || r.config.Rebuild != nil && e.Info.Timestamp.Before(r.config.Rebuild.Boundary) {
		return false
	}
	if e.Message.Conversation == nil && e.Message.GetExtendedTextMessage() == nil {
		return false
	}
	for _, root := range []*waE2E.Message{e.Message, e.RawMessage} {
		if root != nil && !ordinaryContent(root, true) {
			return false
		}
	}
	return true
}

func hasAlternateIDs(e *events.Message) bool {
	return !e.Info.SenderAlt.IsEmpty() || !e.Info.RecipientAlt.IsEmpty()
}

const realtimeAliasProbeTimeout = 500 * time.Millisecond

// A scheduling optimization, not source authority. Misses, timeouts and invalid
// pairs fall back to ordered projection. No alias is installed here; the normal
// ApplyAlias path and final archive response guards remain mandatory.
func (r *Runtime) committedRealtimeAliases(parent context.Context, e *events.Message) bool {
	ctx, cancel := context.WithTimeout(parent, realtimeAliasProbeTimeout)
	defer cancel()
	account := r.SourceAccount()
	if account == "" || e.Info.IsGroup && !e.Info.RecipientAlt.IsEmpty() {
		return false
	}
	if _, err := r.store.SourcePolicy(ctx, account); err != nil {
		return false
	}
	var previousPN, previousLID string
	for _, pair := range [][2]types.JID{{e.Info.Sender, e.Info.SenderAlt}, {e.Info.Chat, e.Info.RecipientAlt}} {
		if pair[1].IsEmpty() {
			continue
		}
		a, b := pair[0].ToNonAD(), pair[1].ToNonAD()
		if a.Server == types.HiddenUserServer && b.Server == types.DefaultUserServer {
			a, b = b, a
		}
		if a.User == "" || b.User == "" || a.Server != types.DefaultUserServer || b.Server != types.HiddenUserServer {
			return false
		}
		pn, lid := a.String(), b.String()
		if len(pn) > 256 || len(lid) > 256 {
			return false
		}
		if pn == previousPN && lid == previousLID {
			continue
		}
		phoneCanonical, err := r.store.Canonical(ctx, account, pn)
		if err != nil || phoneCanonical != pn {
			return false
		}
		lidCanonical, err := r.store.Canonical(ctx, account, lid)
		if err != nil || lidCanonical != pn {
			return false
		}
		previousPN, previousLID = pn, lid
	}
	_, err := r.store.SourcePolicy(ctx, account)
	return err == nil && ctx.Err() == nil
}

// Whitelist populated content roots, not just the preferred body selected by the
// SDK. DeviceSent may wrap a normal RawMessage; messageContextInfo is noncontent.
func ordinaryContent(m *waE2E.Message, textOnly bool) bool {
	budget := 4096
	if !knownContentTree(m, 0, &budget) {
		return false
	}
	return ordinaryContentRoot(m, textOnly, 0)
}

func knownContentTree(m proto.Message, depth int, budget *int) bool {
	if m == nil || !m.ProtoReflect().IsValid() || depth > 32 {
		return false
	}
	root := m.ProtoReflect()
	if len(root.GetUnknown()) != 0 {
		return false
	}
	known := true
	root.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		*budget--
		if *budget < 0 {
			known = false
			return false
		}
		// Field presence, including explicit false/zero, is authoritative here.
		// Do not guess that a declared privacy/retention field is harmless merely
		// because its current scalar value is the protobuf default.
		if !ordinaryMetadataField(root.Descriptor().FullName(), field.Name()) {
			known = false
			return false
		}
		if field.Kind() == protoreflect.MessageKind && !field.IsMap() {
			if field.IsList() {
				list := value.List()
				for i := 0; i < list.Len(); i++ {
					if !knownContentTree(list.Get(i).Message().Interface(), depth+1, budget) {
						known = false
						break
					}
				}
			} else {
				known = knownContentTree(value.Message().Interface(), depth+1, budget)
			}
		}
		return known
	})
	return known
}

func ordinaryMetadataField(message protoreflect.FullName, field protoreflect.Name) bool {
	switch message {
	case "WAWebProtobufsE2E.MessageContextInfo":
		switch field {
		case "deviceListMetadata", "deviceListMetadataVersion", "messageSecret", "paddingBytes":
			return true
		default:
			return false
		}
	case "WAWebProtobufsE2E.ContextInfo":
		switch field {
		case "forwardingScore", "isForwarded", "mentionedJID":
			return true
		default:
			return false
		}
	}
	return true
}

func ordinaryContentRoot(m *waE2E.Message, textOnly bool, depth int) bool {
	if m == nil || depth > 32 {
		return false
	}
	content := 0
	valid := true
	m.ProtoReflect().Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch f.Name() {
		case "conversation", "extendedTextMessage":
			content++
		case "imageMessage", "audioMessage", "videoMessage", "documentMessage", "stickerMessage":
			content++
			valid = valid && !textOnly
		case "messageContextInfo":
			// Only the presence-sensitive metadata whitelist above qualifies.
		case "deviceSentMessage":
			content++
			valid = valid && ordinaryContentRoot(m.GetDeviceSentMessage().GetMessage(), textOnly, depth+1)
		default:
			valid = false
		}
		return valid
	})
	c := messageContext(m)
	return valid && content == 1 && (c == nil || c.Expiration == nil) && !hasViewOnceField(m)
}

func hasViewOnceField(m *waE2E.Message) bool {
	for _, child := range []proto.Message{m.GetExtendedTextMessage(), m.GetImageMessage(), m.GetAudioMessage(), m.GetVideoMessage()} {
		root := child.ProtoReflect()
		if root.IsValid() {
			field := root.Descriptor().Fields().ByName("viewOnce")
			if field != nil && root.Has(field) {
				return true
			}
		}
	}
	return false
}

func (r *Runtime) scheduleChangedLocked() {
	if r.sourceChanged != nil {
		close(r.sourceChanged)
	}
	r.sourceChanged = make(chan struct{})
	r.wakeAdmissionLocked()
}

func (r *Runtime) earlierUnqueuedLocked(ticket *sourceTicket) bool {
	for sequence, earlier := range r.sourceTickets {
		// An earlier unqueued callback owns the next admission turn across both
		// lanes. Later live bodies cannot occupy their prerequisite's capacity.
		if sequence < ticket.sequence && !earlier.queued {
			return true
		}
	}
	return false
}

func (r *Runtime) reserveAdmissionLocked(ticket *sourceTicket) {
	if ticket.waiting {
		r.waitingAdmissions--
		r.waitingBytes -= ticket.bytes
		ticket.waiting = false
	}
	r.admissions++
	r.queueBytes += ticket.bytes
	ticket.admitted = true
}

// Transfer the oldest waiting callback into the same queued/in-flight ledger
// synchronously with release. The callback still owns its payload and performs
// enqueue; a slow wake cannot let another lane steal the newly available slot.
func (r *Runtime) reserveWaitingLocked() {
	if r.status.ReconciliationRequired {
		return
	}
	var oldest *sourceTicket
	for _, ticket := range r.sourceTickets {
		if !ticket.queued && (oldest == nil || ticket.sequence < oldest.sequence) {
			oldest = ticket
		}
	}
	if oldest == nil || oldest.admitted || !oldest.waiting || !time.Now().Before(oldest.deadline) {
		return
	}
	// Later callbacks may occupy otherwise unused in-flight capacity while the
	// waiting ledger is full. They cannot enqueue ahead of this prerequisite.
	// Exchange those parked reservations atomically with the oldest waiter;
	// neither ledger ever exceeds its original count or byte bound.
	var parked []*sourceTicket
	parkedBytes := int64(0)
	for _, ticket := range r.sourceTickets {
		if ticket.sequence > oldest.sequence && ticket.admitted && !ticket.queued {
			parked = append(parked, ticket)
			parkedBytes += ticket.bytes
		}
	}
	queuedCount := r.admissions - len(parked) + 1
	queuedBytes := r.queueBytes - parkedBytes + oldest.bytes
	waitingCount := r.waitingAdmissions - 1 + len(parked)
	waitingBytes := r.waitingBytes - oldest.bytes + parkedBytes
	if queuedCount > r.config.BodyQueueCount || queuedBytes > r.config.BodyQueueBytes || waitingCount > r.config.BodyQueueCount || waitingBytes > r.config.BodyQueueBytes {
		if r.admissions != len(parked) {
			return // queued/in-flight work can still release capacity
		}
		// With both workers idle, exchanging only one waiter can fragment the
		// ledgers: waiting 500+300 and parked 800 each fit a 1024-byte ledger,
		// but moving only 500 would leave 1100 waiting bytes. Swap both complete
		// bounded sets instead. No queued ticket moves, retained totals stay
		// fixed, and the oldest callback owns an admission reservation again.
		// Reservations are not execution: the existing unqueued-prefix gate
		// still permits only the oldest callback to enqueue next. Source/live
		// headroom is opportunistic, never a prerequisite for idle progress.
		for _, ticket := range r.sourceTickets {
			if ticket.waiting {
				ticket.admitted, ticket.waiting = true, false
			} else if ticket.admitted {
				ticket.admitted, ticket.waiting = false, true
			}
		}
		r.admissions, r.waitingAdmissions = r.waitingAdmissions, r.admissions
		r.queueBytes, r.waitingBytes = r.waitingBytes, r.queueBytes
		return
	}
	if oldest.history && r.config.BodyQueueCount >= 2 {
		sourceCount, sourceBytes := 0, int64(0)
		for _, ticket := range r.sourceTickets {
			if ticket.admitted && !ticket.live && ticket.queued {
				sourceCount++
				sourceBytes += ticket.bytes
			}
		}
		reserve := min(int64(64<<10), r.config.BodyQueueBytes/8)
		if sourceCount >= r.config.BodyQueueCount-1 || oldest.bytes <= r.config.BodyQueueBytes-reserve && oldest.bytes > r.config.BodyQueueBytes-reserve-sourceBytes {
			return
		}
	}
	for _, ticket := range parked {
		ticket.admitted, ticket.waiting = false, true
	}
	oldest.admitted, oldest.waiting = true, false
	r.admissions, r.queueBytes = queuedCount, queuedBytes
	r.waitingAdmissions, r.waitingBytes = waitingCount, waitingBytes
}

func (r *Runtime) admissionRoomLocked(ticket *sourceTicket) bool {
	if r.admissions >= r.config.BodyQueueCount || ticket.bytes > r.config.BodyQueueBytes-r.queueBytes {
		return false
	}
	if !ticket.history || r.config.BodyQueueCount < 2 {
		return true
	}
	// Reserve against source usage, not total usage: a later live item waiting
	// on this history must never consume the very source slot it needs to run.
	count, bytes := 0, int64(0)
	for _, pending := range r.sourceTickets {
		if pending.admitted && !pending.live {
			count++
			bytes += pending.bytes
		}
	}
	if count >= r.config.BodyQueueCount-1 {
		return false
	}
	reserve := min(int64(64<<10), r.config.BodyQueueBytes/8)
	return ticket.bytes > r.config.BodyQueueBytes-reserve || ticket.bytes <= r.config.BodyQueueBytes-reserve-bytes
}

func (r *Runtime) sourceTurnAllowedLocked(ticket *sourceTicket) bool {
	for sequence, earlier := range r.sourceTickets {
		if sequence >= ticket.sequence {
			continue
		}
		if !ticket.live { // Every ordinary source/lifecycle item keeps its prefix.
			if earlier.live {
				return false
			}
			continue
		}
		if earlier.live || !earlier.history || !earlier.prepared {
			return false
		}
		if _, touched := earlier.keys[ticket.messageID]; touched {
			return false
		}
	}
	return true
}

func (r *Runtime) awaitSourceTurn(ticket *sourceTicket) bool {
	return r.awaitSourceOrder(ticket, false)
}

func (r *Runtime) awaitSourceOrder(ticket *sourceTicket, completePrefix bool) bool {
	if ticket == nil {
		return true
	} // isolated direct-projection fixtures
	for {
		r.mu.Lock()
		if r.status.ReconciliationRequired {
			r.mu.Unlock()
			return false
		}
		allowed := r.sourceTurnAllowedLocked(ticket)
		if completePrefix {
			for sequence := range r.sourceTickets {
				if sequence < ticket.sequence {
					allowed = false
					break
				}
			}
		}
		if allowed {
			r.mu.Unlock()
			return true
		}
		changed, ctx := r.sourceChanged, r.runCtx
		r.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

func (r *Runtime) finishSourceTicket(ticket *sourceTicket) {
	if ticket == nil {
		return
	}
	r.mu.Lock()
	if ticket.waiting {
		r.waitingAdmissions--
		r.waitingBytes -= ticket.bytes
	} else if ticket.admitted && !ticket.queued {
		r.admissions--
		r.queueBytes -= ticket.bytes
	}
	delete(r.sourceTickets, ticket.sequence)
	ticket.keys = nil
	r.reserveWaitingLocked()
	r.scheduleChangedLocked()
	r.mu.Unlock()
}

func (r *Runtime) completeSourceItem(item queuedEvent) {
	r.mu.Lock()
	r.queueBytes -= item.bytes
	r.admissions--
	if item.ticket != nil {
		delete(r.sourceTickets, item.ticket.sequence)
		item.ticket.keys = nil
	}
	r.reserveWaitingLocked()
	idle := r.admissions == 0
	r.scheduleChangedLocked()
	r.mu.Unlock()
	if idle {
		r.WakeOriginalDownloads()
	}
}

func (r *Runtime) workLive() {
	defer close(r.liveWorkerDone)
	for item := range r.liveQueue {
		r.consume(item)
	}
}

func (r *Runtime) publishHistoryPreparation(ctx context.Context, keys map[string]struct{}) {
	ticket, _ := ctx.Value(sourceTicketContextKey{}).(*sourceTicket)
	if ticket == nil || !ticket.history {
		return
	}
	r.mu.Lock()
	if r.sourceTickets[ticket.sequence] == ticket && !r.status.ReconciliationRequired {
		ticket.keys, ticket.prepared = keys, true
		r.scheduleChangedLocked()
	}
	r.mu.Unlock()
}
