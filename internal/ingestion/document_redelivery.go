package ingestion

import (
	"context"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type ownerDocumentRedeliveryStore interface {
	DocumentRedeliverySources(context.Context, archive.ReadScope, []archive.MessageID) ([]archive.DocumentRedeliverySource, error)
	phoneRedeliverySourceStore
}

// RequestDocumentRedelivery asks the primary phone for at most eight exact
// archived messages selected by the owner. The acknowledgement is not an
// import: authenticated SDK events must provide the document references.
func (r *Runtime) RequestDocumentRedelivery(ctx context.Context, scope archive.ReadScope, ids []archive.MessageID) (int, error) {
	if ctx == nil || scope.Principal != "owner" || scope.Account != r.SourceAccount() || scope.PolicyGeneration == 0 {
		return 0, ErrMediaInvalidAdmission
	}
	targets, err := documentRedeliveryTargets(ids)
	if err != nil {
		return 0, err
	}
	if err = ctx.Err(); err != nil {
		return 0, mediaFailure(err, ErrMediaCanceled)
	}
	r.op.Lock()
	defer r.op.Unlock()
	r.mu.Lock()
	ready := !r.closed && r.status.Connection == "connected" && !r.status.Paused && !r.status.ReconciliationRequired && r.admissions == 0
	runCtx := r.runCtx
	r.mu.Unlock()
	if !ready || runCtx == nil {
		return 0, ErrMediaCanceled
	}
	if r.client.Store.ID == nil || r.client.Store.ID.ToNonAD().String() != scope.Account {
		return 0, ErrMediaAuthentication
	}
	if r.sendHistoryRequest == nil && (!r.client.IsConnected() || !r.client.IsLoggedIn()) {
		return 0, ErrMediaCanceled
	}
	store, ok := r.store.(ownerDocumentRedeliveryStore)
	if !ok {
		return 0, ErrMediaUnavailable
	}
	batch, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(runCtx, cancel)
	defer stop()
	items, err := store.DocumentRedeliverySources(batch, scope, targets)
	if err != nil {
		return 0, mediaFailure(err, ErrMediaUnavailable)
	}
	if err = validateDocumentRedeliverySources(targets, items); err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	r.optionalMu.Lock()
	limited := !r.lastBackfillExplicit.IsZero() && time.Since(r.lastBackfillExplicit) < optionalCooldown
	r.optionalMu.Unlock()
	if limited {
		return 0, ErrMediaLimit
	}
	requested := 0
	authorized := false
	for _, item := range items {
		if batch.Err() != nil {
			return requested, mediaFailure(batch.Err(), ErrMediaDeadline)
		}
		// The shared source guard checks canonical archive identity, current
		// source and policy, dates, expiry, and verified SDK transport mappings.
		source := archive.VoiceRedeliverySource{Key: item.Key, Source: item.Source, OriginalSentAt: item.OriginalSentAt}
		chat, sender, sourceErr := r.voiceRedeliveryJIDs(batch, scope, store, source)
		if sourceErr != nil {
			continue
		}
		request := r.client.BuildUnavailableMessageRequest(chat, sender, item.Key.Message)
		keys := request.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
		if len(keys) != 1 || keys[0].GetMessageKey().GetFromMe() != item.Key.FromMe {
			continue
		}
		if !r.optionalReady() {
			return requested, ErrMediaCanceled
		}
		if !authorized {
			r.optionalMu.Lock()
			if !r.lastBackfillExplicit.IsZero() && time.Since(r.lastBackfillExplicit) < optionalCooldown {
				r.optionalMu.Unlock()
				return requested, ErrMediaLimit
			}
			r.lastBackfillExplicit = time.Now()
			r.optionalMu.Unlock()
			authorized = true
		}
		attempt, done := context.WithTimeout(batch, 2*time.Second)
		if r.sendHistoryRequest != nil {
			err = r.sendHistoryRequest(attempt, request)
		} else {
			_, err = r.client.SendPeerMessage(attempt, request)
		}
		done()
		if err != nil {
			return requested, mediaFailure(err, ErrMediaUnavailable)
		}
		requested++
	}
	return requested, nil
}

func documentRedeliveryTargets(ids []archive.MessageID) ([]archive.MessageID, error) {
	if len(ids) < 1 || len(ids) > 8 {
		return nil, ErrMediaInvalidAdmission
	}
	for i, id := range ids {
		if len(id) == 0 || len(id) > 128 || !utf8.ValidString(string(id)) || strings.ContainsRune(string(id), 0) {
			return nil, ErrMediaInvalidAdmission
		}
		for _, previous := range ids[:i] {
			if previous == id {
				return nil, ErrMediaInvalidAdmission
			}
		}
	}
	return append([]archive.MessageID(nil), ids...), nil
}

func validateDocumentRedeliverySources(targets []archive.MessageID, items []archive.DocumentRedeliverySource) error {
	if len(items) > len(targets) {
		return ErrMediaInvalidAdmission
	}
	for i, item := range items {
		selected := false
		for _, target := range targets {
			selected = selected || item.Source.Version.MessageID == target
		}
		if !selected || item.Source.Version.Revision == 0 || len(item.Source.ContentHash) != 64 {
			return ErrMediaInvalidAdmission
		}
		if _, hashErr := hex.DecodeString(item.Source.ContentHash); hashErr != nil {
			return ErrMediaInvalidAdmission
		}
		for _, previous := range items[:i] {
			if previous.Key == item.Key || previous.Source.Version.MessageID == item.Source.Version.MessageID {
				return ErrMediaInvalidAdmission
			}
		}
	}
	return nil
}
