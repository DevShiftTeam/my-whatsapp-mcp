package ingestion

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const (
	optionalPoll        = 5 * time.Second
	optionalCooldown    = 10 * time.Second
	optionalTailTimeout = 2 * time.Second
	originalLeaseTTL    = 2 * time.Minute
)

type optionalCommand struct {
	wake      bool
	request   BackfillRequest
	automatic bool
}
type optionalTail struct {
	lease   archive.OriginalDownloadLease
	outcome MediaError
	cleanup mediaTransferCleanup
}
type optionalNetwork struct {
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	commands        chan optionalCommand
	accepting       bool   // Runtime.optionalMu
	backfillPending bool   // Runtime.optionalMu
	namesPending    bool   // Runtime.optionalMu; coalesced optional label refresh
	namesAgain      bool   // Runtime.optionalMu; a cache change arrived during a scan
	namesAfter      string // Runtime.optionalMu; bounded existing-row keyset cursor
	activeMu        sync.Mutex
	activeCancel    context.CancelFunc
	tail            *optionalTail // sole loop owner; stop accesses only after done
}
type optionalBackfill struct {
	request   BackfillRequest
	automatic bool
	anchors   []archive.SourceAnchor
	loaded    bool
}

func validateBackfillRequest(q BackfillRequest) error {
	if q.PolicyGeneration == 0 || q.Limit < 0 || q.Limit > 32 || q.AllSelected && len(q.Chats) != 0 || !q.AllSelected && (len(q.Chats) == 0 || len(q.Chats) > 32) {
		return ErrMediaInvalidAdmission
	}
	seen := map[archive.ChatID]bool{}
	for _, id := range q.Chats {
		if id == "" || len(id) > 128 || !utf8.ValidString(string(id)) || strings.ContainsRune(string(id), 0) || seen[id] {
			return ErrMediaInvalidAdmission
		}
		seen[id] = true
	}
	return nil
}
func (r *Runtime) RequestBackfill(ctx context.Context, q BackfillRequest) error {
	if ctx == nil {
		return ErrMediaInvalidAdmission
	}
	if err := ctx.Err(); err != nil {
		return mediaFailure(err, ErrMediaCanceled)
	}
	if err := validateBackfillRequest(q); err != nil {
		return err
	}
	q.Chats = append([]archive.ChatID(nil), q.Chats...)
	if q.Limit == 0 {
		q.Limit = 32
	}
	return r.enqueueBackfill(q, false)
}
func (r *Runtime) enqueueBackfill(q BackfillRequest, automatic bool) error {
	r.optionalMu.Lock()
	defer r.optionalMu.Unlock()
	n := r.optional
	if n == nil || !n.accepting || r.config.BackfillRequests == 0 {
		return ErrMediaCanceled
	}
	if n.backfillPending {
		if automatic {
			return nil
		}
		return ErrMediaLimit
	}
	if !automatic && !r.lastBackfillExplicit.IsZero() && time.Since(r.lastBackfillExplicit) < optionalCooldown {
		return ErrMediaLimit
	}
	if automatic && r.backfillRequests >= r.config.BackfillRequests {
		return nil
	}
	// Explicit metadata may replace an unneeded coalesced wake, never another
	// selection. Durable download work remains reachable through the fixed poll.
	select {
	case queued := <-n.commands:
		if !queued.wake {
			n.commands <- queued
			return ErrMediaLimit
		}
	default:
	}
	select {
	case n.commands <- optionalCommand{request: q, automatic: automatic}:
		n.backfillPending = true
		if !automatic {
			r.lastBackfillExplicit = time.Now()
		}
		return nil
	default:
		return ErrMediaLimit
	}
}
func (r *Runtime) WakeOriginalDownloads() {
	r.optionalMu.Lock()
	defer r.optionalMu.Unlock()
	if n := r.optional; n != nil && n.accepting {
		select {
		case n.commands <- optionalCommand{wake: true}:
		default:
		}
	}
}
func (r *Runtime) startOptional() {
	ctx, cancel := context.WithCancel(context.Background())
	n := &optionalNetwork{ctx: ctx, cancel: cancel, done: make(chan struct{}), commands: make(chan optionalCommand, 1), accepting: true}
	r.optionalMu.Lock()
	r.optional = n
	r.optionalMu.Unlock()
	r.mu.Lock()
	r.optionalJoined = false
	r.mu.Unlock()
	go r.optionalWork(n)
	r.WakeOriginalDownloads()
}
func (r *Runtime) stopOptional() {
	r.optionalMu.Lock()
	n := r.optional
	if n != nil {
		n.accepting = false
	}
	r.optionalMu.Unlock()
	if n != nil {
		n.cancel()
		n.cancelAttempt()
	}
}
func (n *optionalNetwork) cancelAttempt() {
	n.activeMu.Lock()
	cancel := n.activeCancel
	n.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (r *Runtime) cancelOptionalAttempt() {
	r.optionalMu.Lock()
	n := r.optional
	r.optionalMu.Unlock()
	if n != nil {
		n.cancelAttempt()
	}
}
func (n *optionalNetwork) attempt(ttl time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(n.ctx, ttl)
	n.activeMu.Lock()
	n.activeCancel = cancel
	n.activeMu.Unlock()
	return ctx, func() { cancel(); n.activeMu.Lock(); n.activeCancel = nil; n.activeMu.Unlock() }
}
func (r *Runtime) joinOptional(ctx context.Context) error {
	r.optionalMu.Lock()
	n := r.optional
	r.optionalMu.Unlock()
	if n == nil {
		return nil
	}
	select {
	case <-n.done:
	case <-ctx.Done():
		r.optionalOutcome(ErrMediaCleanupIncomplete)
		return ErrMediaCleanupIncomplete
	}
	if n.tail != nil {
		return ErrMediaCleanupIncomplete
	}
	r.mu.Lock()
	r.optionalJoined = true
	r.mu.Unlock()
	return nil
}
func (r *Runtime) optionalOutcome(err error) {
	if err != nil {
		r.recordMediaOutcome(mediaFailure(err, ErrMediaUnavailable))
	} else {
		r.mu.Lock()
		r.status.MediaCode = ""
		r.mu.Unlock()
	}
	r.notify()
}
func (r *Runtime) optionalReady() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status.Connection == "connected" && !r.status.Paused && !r.status.ReconciliationRequired && r.admissions == 0
}
func (r *Runtime) optionalWork(n *optionalNetwork) {
	defer close(n.done)
	timer := time.NewTicker(optionalPoll)
	defer timer.Stop()
	var backfill *optionalBackfill
	for {
		select {
		case <-n.ctx.Done():
			if n.tail != nil {
				if r.settleOptionalTail(n.tail) != nil {
					r.optionalOutcome(ErrMediaCleanupIncomplete)
					// One existing loop retains failed stopped handles and bounded
					// tail metadata. No caller executes a possibly blocking Close.
					<-timer.C
					continue
				}
				n.tail = nil
			}
			return
		case cmd := <-n.commands:
			if !cmd.wake {
				backfill = &optionalBackfill{request: cmd.request, automatic: cmd.automatic}
			}
		case <-timer.C:
		}
		if n.ctx.Err() != nil {
			continue
		}
		if n.tail != nil {
			if err := r.settleOptionalTail(n.tail); err != nil {
				r.optionalOutcome(ErrMediaCleanupIncomplete)
				continue
			}
			n.tail = nil
		}
		if !r.optionalReady() {
			continue
		}
		if backfill != nil {
			done, err := r.optionalBackfillStep(n, backfill)
			if err != nil {
				r.optionalOutcome(err)
			}
			if done {
				backfill = nil
				r.optionalMu.Lock()
				n.backfillPending = false
				r.optionalMu.Unlock()
			}
			if r.optionalReady() {
				r.WakeOriginalDownloads()
			}
			continue // one bounded send; source admission is checked next turn
		}
		r.optionalNamesStep(n)
		if n.ctx.Err() == nil && r.optionalReady() {
			r.optionalOriginalStep(n)
		}
	}
}
func staleOptional(err error) bool {
	return errors.Is(err, archive.ErrNoWork) || errors.Is(err, archive.ErrSourceUnavailable) || errors.Is(err, archive.ErrPolicyChanged)
}
func (r *Runtime) settleOptionalTail(tail *optionalTail) error {
	if !nilMediaPort(tail.cleanup.admission) {
		if err := tail.cleanup.admission.Close(); err != nil {
			return ErrMediaCleanupIncomplete
		}
		tail.cleanup.admission = nil
	}
	store, ok := r.store.(OriginalDownloadStore)
	if !ok {
		return ErrMediaCleanupIncomplete
	}
	ctx, cancel := context.WithTimeout(context.Background(), optionalTailTimeout)
	defer cancel()
	finished, err := store.FinishOriginalDownload(ctx, tail.lease)
	if staleOptional(err) {
		return nil
	}
	if err != nil {
		return ErrMediaCleanupIncomplete
	}
	if finished {
		r.optionalOutcome(nil)
		return nil
	}
	code := tail.outcome
	if !code.valid() {
		code = ErrMediaUnavailable
	}
	after := time.Minute
	if code == ErrMediaCanceled || code == ErrMediaSourceChanged {
		after = time.Second
	}
	terminal := code == ErrMediaAuthentication || code == ErrMediaInvalidAdmission || code == ErrMediaLimit
	err = store.RetryOriginalDownload(ctx, archive.OriginalDownloadRetry{Lease: tail.lease, Code: code.Error(), After: after, Terminal: terminal})
	if staleOptional(err) {
		return nil
	}
	if err != nil {
		return ErrMediaCleanupIncomplete
	}
	return nil
}
func validOriginalWork(work archive.OriginalDownloadWork, account string) bool {
	_, err := hex.DecodeString(work.Source.ContentHash)
	return account != "" && work.Source.Account == account && work.Lease.MediaID != "" && len(work.Lease.MediaID) <= 128 && work.Lease.LeaseID != "" && len(work.Lease.LeaseID) <= 128 && work.Source.Version.MessageID != "" && work.Source.Version.Revision > 0 && work.Source.Version.PolicyGeneration > 0 && !work.Source.Version.Deleted && !work.Source.Version.Excluded && len(work.Source.ContentHash) == 64 && err == nil && work.Attempts > 0 && !work.LeaseUntil.IsZero() && work.DeclaredBytes >= 0 && (work.Kind == archive.MessageImage || work.Kind == archive.MessageVoice)
}
func decodeOriginal(input archive.OriginalDownloadInput, maximum int64) (*pendingMedia, error) {
	work := input.Work
	if len(input.ProtocolReference) == 0 || len(input.ProtocolReference) > 8192 || input.Key.Account != work.Source.Account || input.Key.Message == "" || input.Key.Chat == "" || input.Key.Sender == "" || maximum < 1 || work.DeclaredBytes > maximum {
		return nil, ErrMediaInvalidAdmission
	}
	options := proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 8}
	var attachment pendingMedia
	if work.Kind == archive.MessageImage {
		var m waE2E.ImageMessage
		defer func() { clear(m.MediaKey); clear(m.FileSHA256); clear(m.FileEncSHA256) }()
		if options.Unmarshal(input.ProtocolReference, &m) != nil {
			return nil, ErrMediaInvalidAdmission
		}
		attachment.attachment = &waE2E.ImageMessage{DirectPath: proto.String(m.GetDirectPath()), MediaKey: append([]byte(nil), m.GetMediaKey()...), FileSHA256: append([]byte(nil), m.GetFileSHA256()...), FileEncSHA256: append([]byte(nil), m.GetFileEncSHA256()...), FileLength: proto.Uint64(m.GetFileLength())}
	} else if work.Kind == archive.MessageVoice {
		var m waE2E.AudioMessage
		defer func() { clear(m.MediaKey); clear(m.FileSHA256); clear(m.FileEncSHA256) }()
		if options.Unmarshal(input.ProtocolReference, &m) != nil {
			return nil, ErrMediaInvalidAdmission
		}
		attachment.attachment = &waE2E.AudioMessage{DirectPath: proto.String(m.GetDirectPath()), MediaKey: append([]byte(nil), m.GetMediaKey()...), FileSHA256: append([]byte(nil), m.GetFileSHA256()...), FileEncSHA256: append([]byte(nil), m.GetFileEncSHA256()...), FileLength: proto.Uint64(m.GetFileLength())}
	} else {
		return nil, ErrMediaInvalidAdmission
	}
	// Reuse the production authenticated-field validator without applying a
	// source or allowing caption/context/display fields into the descriptor.
	if !validMediaDescriptor(attachment.attachment, maximum) || attachment.attachment.(interface{ GetFileLength() uint64 }).GetFileLength() != uint64(work.DeclaredBytes) {
		wipePending(&attachment)
		return nil, ErrMediaInvalidAdmission
	}
	attachment.key = input.Key
	attachment.version = work.Source.Version
	attachment.mediaID = work.Lease.MediaID
	attachment.kind = work.Kind
	attachment.hash = append([]byte(nil), attachment.attachment.GetFileSHA256()...)
	return &attachment, nil
}
func wipePending(p *pendingMedia) {
	if p == nil {
		return
	}
	clear(p.hash)
	if p.attachment != nil {
		clear(p.attachment.GetMediaKey())
		clear(p.attachment.GetFileSHA256())
		clear(p.attachment.GetFileEncSHA256())
	}
}
func (r *Runtime) optionalOriginalStep(n *optionalNetwork) {
	store, ok := r.store.(OriginalDownloadStore)
	if !ok || r.callbacks.MediaAdmitter == nil {
		return
	}
	account := r.SourceAccount()
	ctx, done := n.attempt(30 * time.Second)
	defer done()
	work, err := store.ClaimOriginalDownload(ctx, archive.OriginalDownloadClaim{Account: account, TTL: originalLeaseTTL})
	if errors.Is(err, archive.ErrNoWork) {
		return
	}
	if err != nil {
		r.optionalOutcome(err)
		return
	}
	tail := &optionalTail{lease: work.Lease, outcome: ErrMediaInvalidAdmission}
	n.tail = tail
	if validOriginalWork(work, account) {
		input, loadErr := store.LoadOriginalDownload(ctx, work.Lease)
		if loadErr == nil && reflect.DeepEqual(input.Work, work) {
			p, decodeErr := decodeOriginal(input, r.config.MediaMax)
			clear(input.ProtocolReference)
			if decodeErr == nil {
				err = r.transferMedia(ctx, p, &tail.cleanup)
				wipePending(p)
				if err == nil {
					tail.outcome = 0
				} else {
					tail.outcome = mediaFailure(err, ErrMediaUnavailable)
				}
			} else {
				tail.outcome = mediaFailure(decodeErr, ErrMediaInvalidAdmission)
			}
		} else {
			clear(input.ProtocolReference)
			if loadErr != nil {
				tail.outcome = mediaFailure(loadErr, ErrMediaUnavailable)
			}
		}
	}
	if tail.outcome.valid() {
		r.optionalOutcome(tail.outcome)
	}
	if err = r.settleOptionalTail(tail); err != nil {
		r.optionalOutcome(ErrMediaCleanupIncomplete)
	} else {
		n.tail = nil
		r.WakeOriginalDownloads()
	}
}

func (r *Runtime) optionalBackfillStep(n *optionalNetwork, b *optionalBackfill) (bool, error) {
	store, ok := r.store.(SelectedBackfillStore)
	if !ok {
		return true, ErrMediaUnavailable
	}
	ctx, done := n.attempt(10 * time.Second)
	defer done()
	if !b.loaded {
		anchors, err := store.SelectedBackfillAnchors(ctx, archive.BackfillSelection{Account: r.SourceAccount(), PolicyGeneration: b.request.PolicyGeneration, AllSelected: b.request.AllSelected, Chats: b.request.Chats, Limit: b.request.Limit})
		if err != nil {
			return true, mediaFailure(err, ErrMediaUnavailable)
		}
		if len(anchors) > b.request.Limit || len(anchors) > 32 {
			return true, ErrMediaLimit
		}
		b.anchors = anchors
		b.loaded = true
	}
	if len(b.anchors) == 0 {
		return true, nil
	}
	if b.automatic {
		r.optionalMu.Lock()
		exhausted := r.backfillRequests >= r.config.BackfillRequests
		r.optionalMu.Unlock()
		if exhausted {
			return true, nil
		}
	}
	anchor := b.anchors[0]
	policy, err := r.store.SourcePolicy(ctx, r.SourceAccount())
	if err != nil {
		return true, mediaFailure(err, ErrMediaUnavailable)
	}
	if policy.Generation != b.request.PolicyGeneration {
		return true, ErrMediaSourceChanged
	}
	if anchor.Key.Account != r.SourceAccount() || !policyPermits(policy, archive.SourceEvent{Key: anchor.Key, OriginalSentAt: anchor.OriginalSentAt, OriginalDateKnown: !anchor.OriginalSentAt.IsZero()}, time.Now()) {
		return true, ErrMediaSourceChanged
	}
	chat, err := types.ParseJID(anchor.Key.Chat)
	if err != nil {
		return true, ErrMediaInvalidAdmission
	}
	sender, err := types.ParseJID(anchor.Key.Sender)
	if err != nil {
		return true, ErrMediaInvalidAdmission
	}
	info := types.MessageInfo{ID: anchor.Key.Message, Timestamp: anchor.OriginalSentAt, MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: anchor.Key.FromMe}}
	// Metadata reads may have waited while a new source body was accepted.
	// Retain this exact anchor and yield before consuming a request allowance.
	if !r.optionalReady() {
		return false, nil
	}
	if b.automatic {
		r.optionalMu.Lock()
		r.backfillRequests++
		r.optionalMu.Unlock()
	}
	request := r.client.BuildHistorySyncRequest(&info, 50)
	if r.sendHistoryRequest != nil {
		err = r.sendHistoryRequest(ctx, request)
	} else {
		_, err = r.client.SendPeerMessage(ctx, request)
	}
	code := "backfill_requested_coverage_unknown"
	if err != nil {
		code = "backfill_request_failed"
	}
	statusErr := r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: anchor.Key.Account, Chat: anchor.Key.Chat, GapCode: code, CoverageUnknown: true})
	b.anchors = b.anchors[1:]
	if statusErr != nil {
		return true, mediaFailure(statusErr, ErrMediaUnavailable)
	}
	if err != nil {
		return len(b.anchors) == 0, mediaFailure(err, ErrMediaUnavailable)
	}
	return len(b.anchors) == 0, nil
}
