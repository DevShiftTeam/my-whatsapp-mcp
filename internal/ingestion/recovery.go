package ingestion

import (
	"context"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types/events"
)

// RecoveryRequest is a body-free witness for one owned recovery chain. Token
// identifies a runtime-issued request; Reason is a finite operational code.
type RecoveryRequest struct {
	Token  uint64
	Reason string
}

type recoveryWitness struct {
	request         RecoveryRequest
	run, epoch      uint64
	claimed         bool // old-run health cannot abandon an irreversible joined stop
	ready           bool // true only after durable loss or a strictly cleaned transient start failure
	needsOfflineGap bool // qualifying keepalive metadata requires a durable gap before stop
	keepAlive       bool // current-run advisory timeout authority
	disconnected    bool // current-run durable disconnect outranks advisory restoration
}

// Match the owned post-pair lifecycle operation's deadline. Caller cancellation
// does not narrow this strict bound for accepted source/control work.
const recoveryDrainTimeout = 30 * time.Second

func (r *Runtime) invalidateRecoveryLocked() {
	r.recovery = nil
	r.recoveryEpoch++
}

func (r *Runtime) setRecoveryDesired(desired bool) {
	r.mu.Lock()
	r.recoveryDesired = desired
	r.invalidateRecoveryLocked()
	r.mu.Unlock()
}

func (r *Runtime) recoveryAllowedLocked() bool {
	d := r.status.ConnectionDiagnostic
	return r.recoveryDesired && !r.closed && !r.sessionCloseFailed && !r.sessionDeleted &&
		!r.status.Paused && !r.status.ReconciliationRequired && r.status.HasSession &&
		!terminalConnectionEvent(d.Event) &&
		(d.ErrorClass == "" || d.ErrorClass == "none" || TransientConnectionFailure(d))
}

func (r *Runtime) issueRecoveryLocked(reason string) {
	if r.recovery != nil || !r.recoveryAllowedLocked() {
		return
	}
	r.recoveryNextToken++
	r.recovery = &recoveryWitness{request: RecoveryRequest{Token: r.recoveryNextToken, Reason: reason}, run: r.recoveryRun, epoch: r.recoveryEpoch, ready: true, needsOfflineGap: reason == "keepalive_timeout", keepAlive: reason == "keepalive_timeout"}
}

// SDK callbacks retain only bounded metadata and use the existing status lock.
// They never poll the socket, stop workers, or wait for a recovery consumer.
func (r *Runtime) noteRecoverySignal(event any, run uint64) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run != r.recoveryRun {
		return r.recoveryEpoch
	}
	switch event := event.(type) {
	case *events.Connected:
		// Before claim, restored health cancels recovery. Once stop is owned,
		// the old socket will retire and must receive its safe replacement.
		// Terminal events, unsafe markers and owner withdrawal still revoke it.
		if r.recovery == nil || !r.recovery.claimed {
			r.invalidateRecoveryLocked()
		}
		// The SDK starts keepalive before delivering Connected. Callback wall
		// time cannot be a lower bound for a valid last-success timestamp.
		r.recoveryRestoredAt = time.Time{}
		r.recoveryTimeoutSuccess = time.Time{}
		r.recoveryDisconnectPending = false
	case *events.Disconnected:
		// A received source loss outranks advisory restoration immediately,
		// while recovery readiness still waits for its durable gap commit.
		r.recoveryDisconnectPending = true
		if r.recovery != nil && !r.recovery.disconnected {
			r.recovery.ready = false
		}
	case *events.KeepAliveRestored:
		if !r.recoveryTimeoutSuccess.IsZero() {
			// Restoration carries no source timestamp and dispatch may reorder.
			// Use the first observed SDK success in this timeout wave, never
			// callback wall time or a newer wave's later success timestamp.
			r.recoveryRestoredAt = r.recoveryTimeoutSuccess
			r.recoveryTimeoutSuccess = time.Time{}
		}
		// Untimed restoration cannot cancel a fresh suspended startup chain or
		// an independently committed disconnect. During an owned stop it also
		// cannot abandon the replacement of the retiring old socket.
		if r.recovery != nil && r.recovery.keepAlive && !r.recovery.disconnected && !r.recovery.claimed && !r.recoveryDisconnectPending {
			r.invalidateRecoveryLocked()
		}
	case *events.KeepAliveTimeout:
		if event == nil || event.ErrorCount < 1 || event.LastSuccess.IsZero() {
			break
		}
		now := time.Now()
		age := now.Sub(event.LastSuccess)
		if age < 0 || event.LastSuccess.Before(r.recoveryRunStartedAt) || (!r.recoveryRestoredAt.IsZero() && !event.LastSuccess.After(r.recoveryRestoredAt)) {
			break
		}
		if r.recoveryTimeoutSuccess.IsZero() || event.LastSuccess.Before(r.recoveryTimeoutSuccess) {
			r.recoveryTimeoutSuccess = event.LastSuccess
		}
		if age >= 3*time.Minute {
			r.issueRecoveryLocked("keepalive_timeout")
			if r.recovery != nil {
				r.recovery.ready = !r.recoveryDisconnectPending || r.recovery.disconnected
				r.recovery.needsOfflineGap = true
				r.recovery.keepAlive = true
			}
		}
	}
	return r.recoveryEpoch
}

func (r *Runtime) noteConsumedDisconnect(run, epoch uint64) {
	r.mu.Lock()
	if run == r.recoveryRun && epoch == r.recoveryEpoch {
		r.recoveryDisconnectPending = false
		r.issueRecoveryLocked("disconnected")
		if r.recovery != nil {
			r.recovery.ready = true
			r.recovery.disconnected = true
		}
	}
	r.mu.Unlock()
}

func (r *Runtime) noteStartupRecoveryFailure(phase string) {
	r.mu.Lock()
	if (phase == "connect" || phase == "recovery_start") && r.quiesced && TransientConnectionFailure(r.status.ConnectionDiagnostic) {
		r.issueRecoveryLocked("startup_failure")
		if r.recovery != nil {
			r.recovery.ready = true
		}
	}
	r.mu.Unlock()
}

func (r *Runtime) RecoveryRequest() (RecoveryRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recovery == nil || !r.recovery.ready || !r.recoveryAllowedLocked() || r.recovery.run != r.recoveryRun || r.recovery.epoch != r.recoveryEpoch {
		return RecoveryRequest{}, false
	}
	return r.recovery.request, true
}

func (r *Runtime) recoveryRequestCurrent(request RecoveryRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return request.Token != 0 && r.recovery != nil && r.recovery.ready && request == r.recovery.request &&
		r.recovery.run == r.recoveryRun && r.recovery.epoch == r.recoveryEpoch && r.recoveryAllowedLocked()
}

func (r *Runtime) claimRecovery(request RecoveryRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recovery == nil || !r.recovery.ready || request != r.recovery.request || r.recovery.run != r.recoveryRun || r.recovery.epoch != r.recoveryEpoch || !r.recoveryAllowedLocked() {
		return false
	}
	r.recovery.claimed = true
	return true
}

func (r *Runtime) recoveryStartAuthorizedLocked(request RecoveryRequest) bool {
	return r.recovery != nil && request == r.recovery.request && r.recovery.run == r.recoveryRun && r.recovery.epoch == r.recoveryEpoch && r.recoveryAllowedLocked()
}

func (r *Runtime) recoveryStartAuthorized(request RecoveryRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recoveryStartAuthorizedLocked(request)
}

func (r *Runtime) recoveryPolicy(ctx context.Context) error {
	account := r.SourceAccount()
	if account == "" {
		return r.connectionFailure("recovery_policy", archive.ErrSourceUnavailable)
	}
	if _, err := r.store.SourcePolicy(ctx, account); err != nil {
		return r.connectionFailure("recovery_policy", err)
	}
	return nil
}

// Recover reports whether a fresh owned connection attempt was entered, even
// when that attempt returned an error. A stale/refused request never dials.
func (r *Runtime) Recover(ctx context.Context, request RecoveryRequest) (bool, error) {
	r.op.Lock()
	defer r.op.Unlock()
	if !r.recoveryRequestCurrent(request) || r.client.Store.ID == nil || (r.observation != "" && r.runCtx != nil && r.runCtx.Err() != nil) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := r.recoveryPolicy(ctx); err != nil {
		return false, err
	}
	if !r.recoveryRequestCurrent(request) {
		return false, nil
	}
	// Once stop begins, cancellation of a supervisor or HTTP operation must
	// not cancel accepted source/control commits. The independent deadline
	// remains strict if the owned drain genuinely cannot finish.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), recoveryDrainTimeout)
	defer cancelDrain()
	r.mu.Lock()
	needsOfflineGap := r.recovery != nil && r.recovery.request == request && r.recovery.needsOfflineGap
	r.mu.Unlock()
	if needsOfflineGap {
		if err := r.store.SetSyncStatus(drainCtx, archive.SyncUpdate{Account: r.SourceAccount(), Connection: "disconnected", GapCode: "offline_gap", CoverageUnknown: true}); err != nil {
			r.failClosed("archive_unavailable")
			return false, err
		}
		if !r.recoveryRequestCurrent(request) {
			return false, nil
		}
		r.mu.Lock()
		if r.recovery != nil && r.recovery.request == request {
			r.recovery.needsOfflineGap = false
		}
		r.mu.Unlock()
	}
	if !r.claimRecovery(request) {
		return false, nil
	}
	if err := r.stop(drainCtx); err != nil {
		return false, err
	}
	// A terminal event or durable unsafe marker may have been recorded while
	// handler removal, accepted callbacks, queue work, and store controls joined.
	if !r.recoveryRequestCurrent(request) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := r.recoveryPolicy(ctx); err != nil {
		return false, err
	}
	if !r.recoveryRequestCurrent(request) || r.client.Store.ID == nil {
		return false, nil
	}
	r.mu.Lock()
	priorAttempts := r.connectionAttempts
	r.mu.Unlock()
	err := r.startConnection(ctx, false, "recovery_start", request)
	r.mu.Lock()
	attempted := r.connectionAttempts != priorAttempts
	// A nil socket startup result is awaiting authentication, not connection
	// progress. Keep the suspended chain until Connected; a new-run disconnect
	// becomes ready only when its gap commits, preserving the original budget.
	if (err != nil && !TransientConnectionFailure(r.status.ConnectionDiagnostic)) || !r.recoveryAllowedLocked() {
		r.recovery = nil
	}
	r.mu.Unlock()
	return attempted, err
}

// TransientConnectionFailure recognizes only typed bounded retry outcomes.
func TransientConnectionFailure(d ConnectionDiagnostic) bool {
	if terminalConnectionEvent(d.Event) {
		return false
	}
	switch d.ErrorClass {
	case "deadline", "network_timeout", "network_unavailable":
		return true
	case "http_status":
		return d.Reason == 408 || (d.Reason >= 500 && d.Reason <= 504)
	default:
		return false
	}
}
