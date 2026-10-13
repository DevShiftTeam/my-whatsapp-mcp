package app

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
)

var errTranscriptionStatusUnavailable = errors.New("transcription status unavailable")

var _ localui.TranscriptionApplication = (*Application)(nil)

func sameTranscriptionQueryContext(current, captured context.Context) bool {
	if current == nil || captured == nil {
		return current == nil && captured == nil
	}
	// Application query contexts are standard WithCancel pointers. Fail closed
	// for a custom context whose identity cannot be compared without a panic.
	return reflect.ValueOf(current).Comparable() && reflect.ValueOf(captured).Comparable() && current == captured
}

func transcriptionOwnerStopped(owner *voiceOwner) bool {
	if owner.ctx == nil || owner.ctx.Err() != nil || owner.done == nil {
		return true
	}
	select {
	case <-owner.done:
		return true
	default:
		return false
	}
}

// TranscriptionStatus inspects the current running voice adapter, never a new
// connection inferred from private configuration. No archive/source reads or
// processing controls are changed by this health-only check.
func (a *Application) TranscriptionStatus(ctx context.Context) (localui.TranscriptionStatus, error) {
	if ctx == nil {
		return localui.TranscriptionStatus{}, errTranscriptionStatusUnavailable
	}
	if err := ctx.Err(); err != nil {
		return localui.TranscriptionStatus{}, err
	}
	// Lifecycle writers may be waiting for network or worker cleanup. Status
	// must not wait behind them beyond its short request budget.
	if !a.data.TryRLock() {
		return localui.TranscriptionStatus{}, errTranscriptionStatusUnavailable
	}
	config := a.transcriptionConfig
	value := localui.TranscriptionStatus{Configured: config != nil, State: "not_configured"}
	if a.closed || a.offline || a.snapshot != nil {
		a.data.RUnlock()
		value.State = "stopped"
		return value, nil
	}
	if config == nil {
		a.data.RUnlock()
		return value, nil
	}
	value.State = "connecting"
	r := a.resources
	if r == nil || r.voice == nil || r.voiceStatus.Load() != r.voice {
		a.data.RUnlock()
		return value, nil
	}
	owner := r.voice
	if transcriptionOwnerStopped(owner) {
		a.data.RUnlock()
		value.State = "stopped"
		return value, nil
	}
	if !owner.mu.TryLock() {
		a.data.RUnlock()
		return localui.TranscriptionStatus{}, errTranscriptionStatusUnavailable
	}
	backend := owner.backend
	owner.mu.Unlock()
	language := config.Language
	queryContext := a.queryContext
	a.data.RUnlock()
	if backend == nil {
		return value, nil
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stopOwner := context.AfterFunc(owner.ctx, cancel)
	defer stopOwner()
	if queryContext != nil {
		stopQueries := context.AfterFunc(queryContext, cancel)
		defer stopQueries()
	}
	var ready bool
	var err error
	if language != "he" {
		err = transcription.ErrModelChanged
	} else {
		ready, err = backend.HebrewStatus(bounded)
	}
	// Do not hold application lifecycle locks across the authenticated request.
	// Retired/replaced owners cannot publish a successful result afterwards.
	if !a.data.TryRLock() {
		return localui.TranscriptionStatus{}, errTranscriptionStatusUnavailable
	}
	defer a.data.RUnlock()
	if a.closed || a.resources != r || a.transcriptionConfig != config || r.voice != owner || r.voiceStatus.Load() != owner || transcriptionOwnerStopped(owner) {
		value.State = "stopped"
		return value, nil
	}
	if !owner.mu.TryLock() {
		return localui.TranscriptionStatus{}, errTranscriptionStatusUnavailable
	}
	current := owner.backend == backend
	owner.mu.Unlock()
	if !current {
		value.State = "stopped"
		return value, nil
	}
	// AfterFunc propagates cancellation asynchronously. Under the same final
	// ownership lock, check the captured lifetime itself and its identity rather
	// than relying only on its callback having cancelled the bounded request.
	if !sameTranscriptionQueryContext(a.queryContext, queryContext) {
		return localui.TranscriptionStatus{}, errTranscriptionStatusUnavailable
	}
	if queryContext != nil {
		if interrupted := queryContext.Err(); interrupted != nil {
			return localui.TranscriptionStatus{}, interrupted
		}
	}
	if interrupted := ctx.Err(); interrupted != nil {
		return localui.TranscriptionStatus{}, interrupted
	}
	if interrupted := bounded.Err(); interrupted != nil {
		return localui.TranscriptionStatus{}, interrupted
	}
	switch {
	case errors.Is(err, transcription.ErrStopped):
		value.State = "stopped"
	case errors.Is(err, transcription.ErrModelChanged), errors.Is(err, transcription.ErrInvalidResponse), errors.Is(err, transcription.ErrInvalidRequest):
		value.State = "mismatch"
	case errors.Is(err, transcription.ErrBusy):
		value.State, value.HebrewReady = "busy", ready
	case err != nil:
		value.State = "unavailable"
	default:
		value.State, value.HebrewReady = "ready", ready
	}
	return value, nil
}
