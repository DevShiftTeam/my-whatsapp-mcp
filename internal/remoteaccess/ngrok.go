package remoteaccess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	ngrok "golang.ngrok.com/ngrok/v2"
)

// These narrow boundaries keep the SDK's secret-bearing status and options
// inside the provider wrapper. Adapters only expose approved ingress.
type ngrokEndpoint interface {
	net.Listener
	URL() *url.URL
	CloseWithContext(context.Context) error
	Done() <-chan struct{}
	Wait()
}
type ngrokAgent interface {
	Listen(context.Context, string) (ngrokEndpoint, error)
	Disconnect() error
}
type ngrokAdapter struct {
	newAgent func(Secret, func(error)) (ngrokAgent, error)
}

// NewNgrokAdapter is lazy. It never initializes or connects an agent until Open
// receives a separately stored owner authtoken and a confirmed saved domain.
func NewNgrokAdapter() Adapter { return &ngrokAdapter{newAgent: newNgrokSDKAgent} }
func (a *ngrokAdapter) Open(ctx context.Context, i Intent, key Secret, _ func(Notice)) (Session, error) {
	if ctx.Err() != nil {
		return nil, ErrTransient
	}
	if validateIntent(i) != nil || i.Provider != Ngrok || !i.Enabled || i.NodeName != "" {
		return nil, ErrInvalidIntent
	}
	if len(key) == 0 || len(key) > maxSecretBytes || strings.ContainsAny(string(key), " \t\r\n\x00") {
		return nil, ErrCredentialsRejected
	}
	runCtx, cancel := context.WithCancel(ctx)
	s := &ngrokSession{cancel: cancel, opened: make(chan struct{}), joined: make(chan struct{}), ready: make(chan error, 1), done: make(chan error, 1), events: make(chan struct{}, 1)}
	// Open returns cleanup ownership even on cancellation/failure. The context-free
	// constructor and Listen worker are joined before Disconnect can begin.
	go s.run(runCtx, i.SavedOrigin, append(Secret{}, key...), a.newAgent)
	select {
	case err := <-s.ready:
		if err != nil || ctx.Err() != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(ctx, shutdownTimeout)
			// A terminal initial event may precede Listen's return. Start cleanup but
			// return its ownership immediately; never wait for a context-ignoring SDK.
			select {
			case <-s.opened:
			default:
				cleanupCancel()
			}
			_ = s.Close(cleanupCtx)
			cleanupCancel()
			if err == nil {
				err = ErrTransient
			}
		}
		return s, err
	case <-ctx.Done():
		_ = s.Close(ctx)
		if failure := s.failureError(); failure == ErrCredentialsRejected || failure == ErrServiceLimit {
			return s, failure
		}
		return s, ErrTransient
	}
}

// Only reviewed typed provider codes are classified. Unknown codes and network
// errors are fixed transient failures; never inspect or propagate raw Error text.
func ngrokFailure(err error) error {
	// Internal callbacks may already contain fixed errors; keep classification
	// idempotent without retaining a wrapped provider error.
	if errors.Is(err, ErrCredentialsRejected) {
		return ErrCredentialsRejected
	}
	if errors.Is(err, ErrServiceLimit) {
		return ErrServiceLimit
	}
	var provider ngrok.Error
	if errors.As(err, &provider) {
		if fixed := ngrokCodeFailure(provider.Code()); fixed != nil {
			return fixed
		}
	}
	// The pinned SDK forwards legacy errors into disconnected events without
	// public wrapError. Read only the structural typed code, never Error or Msg.
	var legacy interface {
		error
		ErrorCode() string
	}
	if errors.As(err, &legacy) {
		if fixed := ngrokCodeFailure(legacy.ErrorCode()); fixed != nil {
			return fixed
		}
	}
	return ErrTransient
}

func ngrokCodeFailure(code string) error {
	switch code {
	case "ERR_NGROK_107":
		return ErrCredentialsRejected
	case "ERR_NGROK_108":
		return ErrServiceLimit
	default:
		return nil
	}
}

type ngrokSDKAgent struct {
	agent ngrok.Agent
	// v2.2.0 Disconnect clears the SDK session before attempting its Close. A
	// failed Close cannot be proved complete by a second now-disconnected call.
	// Fence replacement permanently rather than mistake that nil for cleanup.
	disconnectFailed bool
}

func newNgrokSDKAgent(secret Secret, disconnected func(error)) (ngrokAgent, error) {
	return newNgrokSDKAgentWithFactory(secret, disconnected, ngrok.NewAgent)
}

// The constructor boundary permits exercising actual SDK callbacks with an
// injected socket-free dialer. Production always uses the official constructor.
func newNgrokSDKAgentWithFactory(secret Secret, disconnected func(error), constructor func(...ngrok.AgentOption) (ngrok.Agent, error)) (ngrokAgent, error) {
	// v2.2.0 legacy logging defaults to discard. Set an explicit per-agent discard
	// logger too, so no provider diagnostics can reach a process-global logger.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := constructor(ngrok.WithAuthtoken(string(secret)), ngrok.WithLogger(logger), ngrok.WithEventHandler(func(event ngrok.Event) {
		if e, ok := event.(*ngrok.EventAgentDisconnected); ok {
			disconnected(ngrokFailure(e.Error))
		}
	}))
	if err != nil {
		return nil, ngrokFailure(err)
	}
	return &ngrokSDKAgent{agent: agent}, nil
}
func (a *ngrokSDKAgent) Listen(ctx context.Context, origin string) (ngrokEndpoint, error) {
	return a.agent.Listen(ctx, ngrok.WithURL(origin))
}
func (a *ngrokSDKAgent) Disconnect() error {
	if a.disconnectFailed {
		return ErrState
	}
	if a.agent.Disconnect() != nil {
		a.disconnectFailed = true
		return ErrState
	}
	return nil
}
func (*ngrokSDKAgent) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte("[ngrok agent]")) }

type ngrokSession struct {
	cancel                    context.CancelFunc
	opened, joined            chan struct{}
	ready, done               chan error
	events                    chan struct{}
	failure                   atomic.Uint32
	openErr                   error
	mu                        sync.Mutex
	agent                     ngrokAgent
	endpoint                  ngrokEndpoint
	origin                    string
	listener                  net.Listener
	closeAttempt              *ngrokCloseAttempt
	disconnected, fullyClosed bool
}

func (s *ngrokSession) Origin() string           { s.mu.Lock(); defer s.mu.Unlock(); return s.origin }
func (s *ngrokSession) Listener() net.Listener   { s.mu.Lock(); defer s.mu.Unlock(); return s.listener }
func (s *ngrokSession) Done() <-chan error       { return s.done }
func (*ngrokSession) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte("[ngrok session]")) }

// Failure notifications are coalesced, but a terminal failure supersedes a
// transient in atomic state even if the notification channel is already full.
// SDK callbacks never wait for the adapter or retain raw errors/status objects.
func (s *ngrokSession) signalFailure(err error) {
	code := uint32(1)
	switch ngrokFailure(err) {
	case ErrCredentialsRejected:
		code = 2
	case ErrServiceLimit:
		code = 3
	}
	for {
		old := s.failure.Load()
		if old >= 2 || old == code || s.failure.CompareAndSwap(old, code) {
			break
		}
	}
	select {
	case s.events <- struct{}{}:
	default:
	}
}
func (s *ngrokSession) failureError() error {
	switch s.failure.Load() {
	case 1:
		return ErrTransient
	case 2:
		return ErrCredentialsRejected
	case 3:
		return ErrServiceLimit
	default:
		return nil
	}
}
func (s *ngrokSession) open(ctx context.Context, origin string, key Secret, factory func(Secret, func(error)) (ngrokAgent, error)) {
	// opened marks completion of all owned factory/Listen work. Cleanup must wait
	// for it before touching Disconnect or any late returned endpoint.
	defer close(s.opened)
	agent, err := factory(key, s.signalFailure)
	clear(key)
	s.mu.Lock()
	s.agent = agent
	s.mu.Unlock()
	if err != nil || agent == nil {
		s.openErr = ngrokFailure(err)
		return
	}
	if ctx.Err() != nil {
		s.openErr = ErrTransient
		return
	}
	endpoint, err := agent.Listen(ctx, origin)
	s.mu.Lock()
	s.endpoint = endpoint
	s.mu.Unlock()
	if err != nil {
		s.openErr = ngrokFailure(err)
	}
}
func (s *ngrokSession) run(ctx context.Context, origin string, key Secret, factory func(Secret, func(error)) (ngrokAgent, error)) {
	defer close(s.joined)
	go s.open(ctx, origin, key, factory)
	waitContext := ctx.Done()
	for {
		select {
		case <-s.events:
			failure := s.failureError()
			s.cancel() // Stop the SDK's internal initial Connect/Listen retry loop.
			if failure == ErrCredentialsRejected || failure == ErrServiceLimit {
				s.ready <- failure
				return
			}
			// A transient stops retries too, but join the pending result while still
			// consuming terminal events. Cancellation's return must not mask a later
			// terminal event emitted before that Listen worker completes.
			waitContext = nil
		case <-waitContext:
			failure := s.failureError()
			if failure == nil {
				failure = ErrTransient
			}
			s.ready <- failure
			return
		case <-s.opened:
			failure := s.failureError()
			if failure == ErrCredentialsRejected || failure == ErrServiceLimit {
				s.ready <- failure
				return
			}
			if s.openErr != nil {
				s.ready <- s.openErr
				return
			}
			if failure != nil || ctx.Err() != nil {
				s.ready <- ErrTransient
				return
			}
			s.mu.Lock()
			endpoint := s.endpoint
			s.mu.Unlock()
			if endpoint == nil || endpoint.URL() == nil {
				s.ready <- ErrInvalidOrigin
				return
			}
			// Compare the complete canonical origin before exposing any listener.
			pinned, err := PinOrigin(origin, endpoint.URL().String())
			if err != nil {
				s.ready <- err
				return
			}
			s.mu.Lock()
			s.origin = pinned
			s.listener = &ngrokListener{session: s, endpoint: endpoint}
			s.mu.Unlock()
			s.ready <- nil
			select {
			case <-ctx.Done():
			case <-endpoint.Done():
				s.done <- ErrTransient
			case <-s.events:
				s.cancel()
				s.done <- s.failureError()
			}
			return
		}
	}
}

type ngrokCloseAttempt struct {
	done chan struct{}
	err  error
}

// Close retains one owned cleanup worker across caller timeouts. Listen is
// joined before Disconnect (the SDK must not race createListener with clearing
// its session), then endpoint Wait and the monitor are joined before replacement.
func (s *ngrokSession) Close(ctx context.Context) error {
	s.cancel()
	s.mu.Lock()
	if s.fullyClosed {
		s.mu.Unlock()
		return nil
	}
	attempt := s.closeAttempt
	if attempt != nil {
		select {
		case <-attempt.done:
			attempt = nil
		default:
		}
	}
	if attempt == nil {
		attempt = &ngrokCloseAttempt{done: make(chan struct{})}
		s.closeAttempt = attempt
		go s.drain(attempt)
	}
	s.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		return ErrTransient
	}
}
func (s *ngrokSession) drain(attempt *ngrokCloseAttempt) {
	defer close(attempt.done)
	<-s.opened
	s.mu.Lock()
	endpoint, agent, disconnected := s.endpoint, s.agent, s.disconnected
	s.mu.Unlock()
	if !disconnected {
		if endpoint != nil {
			// Never expose SDK Close() directly. In v2.2.0 the legacy
			// tunnel currently ignores CloseWithContext cancellation too; the
			// retained worker bounds callers and prevents premature replacement.
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_ = endpoint.CloseWithContext(ctx)
			cancel()
		}
		// Disconnect is context-free. It remains owned by this single worker if a
		// caller times out; retries join it rather than creating overlapping agents.
		if agent != nil && agent.Disconnect() != nil {
			attempt.err = ErrState
			return
		}
		s.mu.Lock()
		s.disconnected = true
		s.mu.Unlock()
	}
	if endpoint != nil {
		endpoint.Wait()
	}
	<-s.joined
	s.mu.Lock()
	s.fullyClosed = true
	s.mu.Unlock()
}

type ngrokListener struct {
	session  *ngrokSession
	endpoint ngrokEndpoint
}

func (l *ngrokListener) Accept() (net.Conn, error) {
	conn, err := l.endpoint.Accept()
	if err != nil {
		return nil, ngrokFailure(err)
	}
	return conn, nil
}
func (l *ngrokListener) Addr() net.Addr { return l.endpoint.Addr() }
func (l *ngrokListener) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return l.session.Close(ctx)
}
func (*ngrokListener) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte("[ngrok listener]")) }
