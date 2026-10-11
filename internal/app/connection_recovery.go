package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
)

type recoverableConnector interface {
	RecoveryRequest() (ingestion.RecoveryRequest, bool)
	Recover(context.Context, ingestion.RecoveryRequest) (bool, error)
}

type connectionRecoverySettings struct {
	pollInterval   time.Duration
	attemptTimeout time.Duration
	backoff        []time.Duration
	maxAttempts    int
}

func (s connectionRecoverySettings) defaults() connectionRecoverySettings {
	if s.pollInterval <= 0 {
		s.pollInterval = time.Second
	}
	if s.attemptTimeout <= 0 {
		s.attemptTimeout = 10 * time.Second
	}
	if len(s.backoff) == 0 {
		s.backoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	}
	if s.maxAttempts <= 0 {
		s.maxAttempts = 5
	}
	return s
}

func (s connectionRecoverySettings) delay(attempts int) time.Duration {
	index := attempts
	if index >= len(s.backoff) {
		index = len(s.backoff) - 1
	}
	return s.backoff[index]
}

// One joined owner holds at most one bounded connection attempt. Its callback
// wake channel is stable for the application's lifetime and never blocks SDK
// delivery. The private timing seam is for synthetic tests only.
type connectionRecoveryOwner struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	settings connectionRecoverySettings
}

// Call only during construction or while holding ops and data.Lock. The owner
// never takes ops: a lifecycle caller can cancel it before waiting for ops.
func (a *Application) startConnectionRecovery(startup bool) {
	if a.demo || a.offline || a.paused || a.closed || !a.authState.DesiredConnected || a.runtime == nil {
		return
	}
	a.connectionOwner.mu.Lock()
	defer a.connectionOwner.mu.Unlock()
	if done := a.connectionOwner.done; done != nil {
		select {
		case <-done:
		default:
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.connectionOwner.cancel, a.connectionOwner.done = cancel, done
	settings := a.connectionOwner.settings.defaults()
	go func() {
		defer close(done)
		defer cancel()
		a.runConnectionRecovery(ctx, startup, settings)
	}()
}

func (a *Application) stopConnectionRecovery(ctx context.Context) error {
	a.connectionOwner.mu.Lock()
	cancel, done := a.connectionOwner.cancel, a.connectionOwner.done
	if cancel != nil {
		cancel()
	}
	a.connectionOwner.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Application) wakeConnectionRecovery() {
	select {
	case a.connectionWake <- struct{}{}:
	default:
	}
}

func terminalConnectionDiagnostic(d ingestion.ConnectionDiagnostic) bool {
	switch d.Event {
	case "logged_out", "connect_failure", "client_outdated", "temporary_ban", "stream_error", "stream_replaced", "cat_refresh_error":
		return true
	}
	return d.ErrorClass == "device_deleted"
}

// Caller owns data.RLock (or data.Lock). All startup and retry paths repeat the
// archive, policy and matching saved-session gates; none may create a QR flow.
func (a *Application) connectionAllowed(ctx context.Context) bool {
	if ctx.Err() != nil || a.demo || a.offline || a.paused || a.closed || !a.authState.DesiredConnected || a.runtime == nil || a.store == nil {
		return false
	}
	status := a.runtime.Status()
	if !status.HasSession || status.Paused || status.ReconciliationRequired || terminalConnectionDiagnostic(status.ConnectionDiagnostic) {
		return false
	}
	linked := a.runtime.SourceAccount()
	bound, err := a.store.BoundAccount(ctx)
	if err != nil || linked == "" || bound == "" || linked != bound {
		return false
	}
	policy, err := a.store.Policy(ctx)
	if err != nil || policy.Generation == 0 || (policy.DateMode != archive.DateSince && policy.DateMode != archive.DateAllAvailable) || (policy.ChatMode != archive.ChatsAll && policy.ChatMode != archive.ChatsSelective) {
		return false
	}
	diagnostic, err := a.store.DiagnosticStatus(ctx)
	return err == nil && !diagnostic.ReconciliationRequired
}

func (a *Application) connectSavedSession(ctx context.Context) {
	if !a.lockConnectionRead(ctx) {
		return
	}
	defer a.data.RUnlock()
	if !a.connectionAllowed(ctx) {
		return
	}
	status := a.runtime.Status()
	if status.Connection != "" && status.Connection != "disconnected" {
		return
	}
	_ = a.runtime.Connect(ctx)
}

func (a *Application) connectionRequest(ctx context.Context) (ingestion.RecoveryRequest, bool) {
	if !a.lockConnectionRead(ctx) {
		return ingestion.RecoveryRequest{}, false
	}
	defer a.data.RUnlock()
	if ctx.Err() != nil || a.closed || a.paused || !a.authState.DesiredConnected {
		return ingestion.RecoveryRequest{}, false
	}
	runtime, ok := a.runtime.(recoverableConnector)
	if !ok {
		return ingestion.RecoveryRequest{}, false
	}
	request, ready := runtime.RecoveryRequest()
	if request.Token == 0 {
		return ingestion.RecoveryRequest{}, false
	}
	switch request.Reason {
	case "disconnected", "keepalive_timeout", "startup_failure":
	default:
		return ingestion.RecoveryRequest{}, false
	}
	if !ready || !a.connectionAllowed(ctx) {
		return ingestion.RecoveryRequest{}, false
	}
	return request, ready
}

func (a *Application) recoverConnection(ctx context.Context, request ingestion.RecoveryRequest) (attempted bool, retry bool) {
	if !a.lockConnectionRead(ctx) {
		return false, false
	}
	defer a.data.RUnlock()
	if !a.connectionAllowed(ctx) {
		return false, false
	}
	runtime, ok := a.runtime.(recoverableConnector)
	if !ok {
		return false, false
	}
	attempted, err := runtime.Recover(ctx, request)
	if !attempted {
		return false, false
	}
	// A nil dial can precede its durable Disconnected commit. Keep the same
	// finite chain eligible; the loop still requires a ready runtime witness
	// before another attempt, and authenticated progress retires that witness.
	if err == nil {
		return true, true
	}
	return true, !errors.Is(err, context.Canceled) && ingestion.TransientConnectionFailure(a.runtime.Status().ConnectionDiagnostic)
}

// A terminal event suppresses automatic restart as well as this process's
// attempts. Persistence belongs here, never in the SDK callback. data.Lock
// protects the same private state as all public lifecycle operations.
func (a *Application) suppressTerminalConnection(ctx context.Context) {
	// A content reader may be awaiting query cancellation, which Close performs
	// after joining this owner. Never queue an uncancelable writer behind it.
	if !a.data.TryLock() {
		return
	}
	defer a.data.Unlock()
	_ = a.persistTerminalConnectionIntent(ctx)
}

func (a *Application) lockConnectionRead(ctx context.Context) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		if a.data.TryRLock() {
			return true
		}
		wait := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			wait.Stop()
			return false
		case <-wait.C:
		}
	}
}

// Caller owns data.Lock; re-read the current runtime diagnostic here so a stale
// sample cannot suppress a newer explicit connection choice.
func (a *Application) persistTerminalConnectionIntent(ctx context.Context) error {
	if ctx.Err() == nil && !a.closed && a.runtime != nil && a.authState.DesiredConnected && terminalConnectionDiagnostic(a.runtime.Status().ConnectionDiagnostic) {
		return a.persistConnectionIntent(false)
	}
	return nil
}

func (a *Application) runConnectionRecovery(ctx context.Context, startup bool, settings connectionRecoverySettings) {
	if startup {
		attemptCtx, cancel := context.WithTimeout(ctx, settings.attemptTimeout)
		a.connectSavedSession(attemptCtx)
		cancel()
	}
	ticker := time.NewTicker(settings.pollInterval)
	defer ticker.Stop()
	var token uint64
	var attempts int
	var initialAttempts int
	var next time.Time
	retired := false
	for {
		if ctx.Err() != nil {
			return
		}
		a.suppressTerminalConnection(ctx)
		request, ready := a.connectionRequest(ctx)
		if ready && request.Token != token {
			token, attempts, initialAttempts, retired = request.Token, 0, 0, false
			// The initial explicit/startup Connect already consumed one attempt.
			if request.Reason == "startup_failure" {
				attempts, initialAttempts = 1, 1
			}
			retired = attempts >= settings.maxAttempts
			next = time.Now().Add(settings.delay(0))
		}
		if ready && !retired && !time.Now().Before(next) {
			attemptCtx, cancel := context.WithTimeout(ctx, settings.attemptTimeout)
			attempted, retry := a.recoverConnection(attemptCtx, request)
			cancel()
			if attempted {
				attempts++
			}
			retired = !retry || attempts >= settings.maxAttempts
			if !retired {
				next = time.Now().Add(settings.delay(attempts - initialAttempts))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-a.connectionWake:
		case <-ticker.C:
		}
	}
}
