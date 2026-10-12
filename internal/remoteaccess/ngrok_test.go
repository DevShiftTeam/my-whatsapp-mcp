package remoteaccess

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ngrok "golang.ngrok.com/ngrok/v2"
)

const ngrokTestOrigin = "https://assigned.ngrok-free.app"
const ngrokTestSecret = "synthetic-private-ngrok-secret"

func ngrokIntent() Intent {
	return Intent{Version: 1, Provider: Ngrok, Enabled: true, SavedOrigin: ngrokTestOrigin}
}

type fakeNgrokEndpoint struct {
	url      *url.URL
	done     chan struct{}
	once     sync.Once
	accepts  atomic.Int32
	closes   atomic.Int32
	closeErr error
	waitGate <-chan struct{}
}

func fakeEndpoint(raw string) *fakeNgrokEndpoint {
	u, _ := url.Parse(raw)
	return &fakeNgrokEndpoint{url: u, done: make(chan struct{})}
}
func (e *fakeNgrokEndpoint) URL() *url.URL { return e.url }
func (e *fakeNgrokEndpoint) Accept() (net.Conn, error) {
	e.accepts.Add(1)
	<-e.done
	return nil, errors.New(ngrokTestSecret)
}
func (e *fakeNgrokEndpoint) Addr() net.Addr { return &net.TCPAddr{} }
func (e *fakeNgrokEndpoint) Close() error   { return e.CloseWithContext(context.Background()) }
func (e *fakeNgrokEndpoint) CloseWithContext(context.Context) error {
	e.closes.Add(1)
	e.once.Do(func() { close(e.done) })
	return e.closeErr
}
func (e *fakeNgrokEndpoint) Done() <-chan struct{} { return e.done }
func (e *fakeNgrokEndpoint) Wait() {
	<-e.done
	if e.waitGate != nil {
		<-e.waitGate
	}
}

type fakeNgrokAgent struct {
	listen      func(context.Context, string) (ngrokEndpoint, error)
	disconnect  func() error
	disconnects atomic.Int32
}

func (a *fakeNgrokAgent) Listen(ctx context.Context, origin string) (ngrokEndpoint, error) {
	return a.listen(ctx, origin)
}
func (a *fakeNgrokAgent) Disconnect() error {
	a.disconnects.Add(1)
	if a.disconnect != nil {
		return a.disconnect()
	}
	return nil
}
func ngrokClose(t *testing.T, s Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

type fakeNgrokError struct{ code string }

func (e fakeNgrokError) Error() string { return "provider echoed " + ngrokTestSecret }
func (e fakeNgrokError) Code() string  { return e.code }

// A missing saved origin must never invoke a random/default SDK listener.
func TestNgrokRejectsMissingDomainAndCredentialBeforeSDK(t *testing.T) {
	a := &ngrokAdapter{newAgent: func(Secret, func(error)) (ngrokAgent, error) {
		t.Error("SDK constructed for invalid setup")
		return nil, ErrInvalidIntent
	}}
	for _, change := range []func(*Intent, *Secret){
		func(i *Intent, s *Secret) { i.SavedOrigin = "" },
		func(i *Intent, s *Secret) { i.SavedOrigin = "https://other.ngrok-free.app/" },
		func(i *Intent, s *Secret) { i.Enabled = false },
		func(i *Intent, s *Secret) { i.NodeName = "unused" },
		func(i *Intent, s *Secret) { i.Provider = TailscalePrivate },
		func(i *Intent, s *Secret) { *s = nil },
		func(i *Intent, s *Secret) { *s = Secret("token\n") },
	} {
		i, s := ngrokIntent(), Secret(ngrokTestSecret)
		change(&i, &s)
		session, err := a.Open(context.Background(), i, s, nil)
		if session != nil || err == nil {
			t.Fatal("invalid setup accepted")
		}
	}
	if validateIntent(Intent{Version: 1, Provider: Ngrok}) == nil {
		t.Fatal("ngrok provider record admitted without confirmed domain")
	}
}

// Changing the credential or URL argument on a retry is a credential/identity bug.
func TestNgrokExplicitTokenAndSavedDomainEveryReconnect(t *testing.T) {
	var endpoints []*fakeNgrokEndpoint
	var agents []*fakeNgrokAgent
	a := &ngrokAdapter{newAgent: func(secret Secret, callback func(error)) (ngrokAgent, error) {
		if string(secret) != ngrokTestSecret {
			t.Error("wrong explicit credential")
			return nil, ErrCredentialsRejected
		}
		e := fakeEndpoint(ngrokTestOrigin)
		endpoints = append(endpoints, e)
		agent := &fakeNgrokAgent{listen: func(ctx context.Context, origin string) (ngrokEndpoint, error) {
			if origin != ngrokTestOrigin {
				return nil, ErrIdentityChanged
			}
			return e, nil
		}}
		agents = append(agents, agent)
		return agent, nil
	}}
	for range 2 {
		s, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
		if err != nil || s.Origin() != ngrokTestOrigin || s.Listener() == nil {
			t.Fatal("fixed domain was not opened", err)
		}
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			if strings.Contains(fmt.Sprintf(format, s), ngrokTestSecret) {
				t.Fatal("session formatting leaked")
			}
		}
		ngrokClose(t, s)
	}
	for n, e := range endpoints {
		if e.accepts.Load() != 0 || e.closes.Load() != 1 || agents[n].disconnects.Load() != 1 {
			t.Fatal("unserved endpoint cleanup incomplete")
		}
	}
}

func TestNgrokMismatchClosesUnserved(t *testing.T) {
	for _, raw := range []string{"https://random.ngrok-free.app", "https://assigned.ngrok-free.app/", "http://assigned.ngrok-free.app", "https://assigned.ngrok-free.app?secret=bad", ""} {
		t.Run(raw, func(t *testing.T) {
			e := fakeEndpoint(raw)
			agent := &fakeNgrokAgent{listen: func(context.Context, string) (ngrokEndpoint, error) { return e, nil }}
			a := &ngrokAdapter{newAgent: func(Secret, func(error)) (ngrokAgent, error) { return agent, nil }}
			s, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
			if err == nil || s == nil || s.Listener() != nil || s.Origin() != "" {
				t.Fatal("mismatch exposed listener")
			}
			ngrokClose(t, s)
			if e.accepts.Load() != 0 || e.closes.Load() != 1 || agent.disconnects.Load() != 1 {
				t.Fatal("mismatched endpoint leaked")
			}
		})
	}
}

func TestNgrokTypedFailuresNeverLeakProviderText(t *testing.T) {
	for _, tc := range []struct {
		code  string
		want  error
		state string
	}{
		{"ERR_NGROK_107", ErrCredentialsRejected, "credentials_rejected"},
		{"ERR_NGROK_108", ErrServiceLimit, "service_limit_reached"},
		{"ERR_NGROK_UNKNOWN", ErrTransient, "reconnecting"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			agent := &fakeNgrokAgent{listen: func(context.Context, string) (ngrokEndpoint, error) {
				return nil, fmt.Errorf("wrapped: %w", fakeNgrokError{tc.code})
			}}
			a := &ngrokAdapter{newAgent: func(Secret, func(error)) (ngrokAgent, error) { return agent, nil }}
			s, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("wrong safe error: %v", err)
			}
			state, code, _ := diagnostic(err)
			if state != tc.state || strings.Contains(code, ngrokTestSecret) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), ngrokTestSecret) {
				t.Fatal("unsafe diagnostic")
			}
			ngrokClose(t, s)
		})
	}
}

func TestNgrokCancellationOwnsLateListenerAndJoinsBeforeDisconnect(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	e := fakeEndpoint(ngrokTestOrigin)
	agent := &fakeNgrokAgent{listen: func(ctx context.Context, origin string) (ngrokEndpoint, error) {
		close(entered)
		<-release
		return e, nil
	}}
	a := &ngrokAdapter{newAgent: func(Secret, func(error)) (ngrokAgent, error) { return agent, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan Session, 1)
	go func() { s, _ := a.Open(ctx, ngrokIntent(), Secret(ngrokTestSecret), nil); result <- s }()
	<-entered
	cancel()
	s := <-result
	if s == nil {
		t.Fatal("lost cleanup ownership")
	}
	deadline, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if s.Close(deadline) == nil {
		t.Fatal("cleanup declared joined before Listen returned")
	}
	if agent.disconnects.Load() != 0 {
		t.Fatal("Disconnect raced Listen")
	}
	close(release)
	ngrokClose(t, s)
	if e.closes.Load() != 1 || agent.disconnects.Load() != 1 || s.Listener() != nil {
		t.Fatal("late cancelled listener leaked")
	}
}

func TestNgrokCloseTimeoutJoinsSameWorkerAndRetriesFailedDisconnect(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := fakeEndpoint(ngrokTestOrigin)
	agent := &fakeNgrokAgent{listen: func(context.Context, string) (ngrokEndpoint, error) { return e, nil }, disconnect: func() error {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-release
			return errors.New(ngrokTestSecret)
		}
		return nil
	}}
	a := &ngrokAdapter{newAgent: func(Secret, func(error)) (ngrokAgent, error) { return agent, nil }}
	s, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if s.Close(ctx) == nil {
		t.Fatal("blocked Disconnect ignored deadline")
	}
	<-entered
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if s.Close(ctx2) == nil || calls.Load() != 1 {
		t.Fatal("overlapping cleanup")
	}
	close(release)
	// Join the failed attempt without letting timing choose whether a retry starts.
	raw := s.(*ngrokSession)
	<-raw.closeAttempt.done
	if !errors.Is(raw.closeAttempt.err, ErrState) || strings.Contains(fmt.Sprint(raw.closeAttempt.err), ngrokTestSecret) {
		t.Fatal("unsafe cleanup error")
	}
	ngrokClose(t, s)
	ngrokClose(t, s)
	if calls.Load() != 2 {
		t.Fatal("failed disconnect not retried idempotently")
	}
}

func TestNgrokDisconnectEventAndAcceptErrorsAreSanitized(t *testing.T) {
	e := fakeEndpoint(ngrokTestOrigin)
	var disconnected func(error)
	agent := &fakeNgrokAgent{listen: func(context.Context, string) (ngrokEndpoint, error) { return e, nil }}
	a := &ngrokAdapter{newAgent: func(s Secret, f func(error)) (ngrokAgent, error) { disconnected = f; return agent, nil }}
	s, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	disconnected(fakeNgrokError{"ERR_NGROK_107"})
	select {
	case err := <-s.Done():
		if err != ErrCredentialsRejected {
			t.Fatal("unsanitized disconnect", err)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not terminate session")
	}
	ln := s.Listener()
	ngrokClose(t, s)
	_, err = ln.Accept()
	if err == nil || strings.Contains(err.Error(), ngrokTestSecret) {
		t.Fatal("accept error leaked provider text")
	}
}

// Real constructor has no network side effects; characterize our explicit SDK
// options through reflected values without importing SDK-internal option types.
func TestNgrokRealSDKConstructorUsesExplicitTokenAndPrivateLogger(t *testing.T) {
	t.Setenv("NGROK_AUTHTOKEN", "inherited-credential-must-not-be-used")
	sdk, err := newNgrokSDKAgent(Secret(ngrokTestSecret), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	raw := reflect.ValueOf(sdk.(*ngrokSDKAgent).agent).Elem().FieldByName("opts").Elem()
	if raw.FieldByName("authtoken").String() != ngrokTestSecret || raw.FieldByName("logger").IsNil() {
		t.Fatal("explicit token/logger missing")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", sdk, sdk), ngrokTestSecret) {
		t.Fatal("SDK wrapper formatting leaked")
	}
	if err := sdk.Disconnect(); err != nil {
		t.Fatal(err)
	}
}

type captureSDKAgent struct {
	ngrok.Agent
	url string
}

func (a *captureSDKAgent) Listen(ctx context.Context, opts ...ngrok.EndpointOption) (ngrok.EndpointListener, error) {
	if len(opts) != 1 {
		return nil, errors.New("expected exactly one fixed URL option")
	}
	fn := reflect.ValueOf(opts[0])
	value := reflect.New(fn.Type().In(0).Elem())
	fn.Call([]reflect.Value{value})
	a.url = value.Elem().FieldByName("url").String()
	return nil, ErrTransient
}
func TestNgrokSDKWrapperPassesWithURLOnEveryListen(t *testing.T) {
	raw := &captureSDKAgent{}
	sdk := &ngrokSDKAgent{agent: raw}
	for range 2 {
		sdk.Listen(context.Background(), ngrokTestOrigin)
		if raw.url != ngrokTestOrigin {
			t.Fatal("SDK default/random URL requested")
		}
	}
}

func TestNgrokAlreadySanitizedSDKEventsKeepCredentialClassification(t *testing.T) {
	e := fakeEndpoint(ngrokTestOrigin)
	var disconnected func(error)
	a := &ngrokAdapter{newAgent: func(_ Secret, f func(error)) (ngrokAgent, error) {
		disconnected = f
		return &fakeNgrokAgent{listen: func(context.Context, string) (ngrokEndpoint, error) { return e, nil }}, nil
	}}
	session, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ngrokClose(t, session)
	disconnected(ErrCredentialsRejected)
	select {
	case err := <-session.Done():
		if err != ErrCredentialsRejected {
			t.Fatalf("lost SDK fixed classification: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing event")
	}
}
func TestNgrokMismatchCleanedBeforeOpenReturns(t *testing.T) {
	e := fakeEndpoint("https://random.ngrok-free.app")
	agent := &fakeNgrokAgent{listen: func(context.Context, string) (ngrokEndpoint, error) { return e, nil }}
	a := &ngrokAdapter{newAgent: func(Secret, func(error)) (ngrokAgent, error) { return agent, nil }}
	session, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
	defer ngrokClose(t, session)
	if err != ErrIdentityChanged || e.closes.Load() != 1 || agent.disconnects.Load() != 1 {
		t.Fatal("Open returned an unclosed wrong-domain endpoint")
	}
}
func TestNgrokCloseWaitsForEndpointWorkerJoin(t *testing.T) {
	gate := make(chan struct{})
	e := fakeEndpoint(ngrokTestOrigin)
	e.waitGate = gate
	agent := &fakeNgrokAgent{listen: func(context.Context, string) (ngrokEndpoint, error) { return e, nil }}
	a := &ngrokAdapter{newAgent: func(Secret, func(error)) (ngrokAgent, error) { return agent, nil }}
	session, err := a.Open(context.Background(), ngrokIntent(), Secret(ngrokTestSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if session.Close(ctx) == nil {
		t.Fatal("cleanup completed before endpoint Wait joined")
	}
	close(gate)
	ngrokClose(t, session)
	if agent.disconnects.Load() != 1 {
		t.Fatal("timeout overlapped Disconnect")
	}
}

type failureSDKAgent struct {
	ngrok.Agent
	calls atomic.Int32
}

func (a *failureSDKAgent) Disconnect() error {
	if a.calls.Add(1) == 1 {
		return errors.New(ngrokTestSecret)
	}
	return nil
}
func TestNgrokSDKDisconnectFailureCannotMasqueradeAsJoinedOnRetry(t *testing.T) {
	raw := &failureSDKAgent{}
	sdk := &ngrokSDKAgent{agent: raw}
	if err := sdk.Disconnect(); err != ErrState {
		t.Fatal("SDK cleanup error was not fixed")
	}
	if err := sdk.Disconnect(); err != ErrState || raw.calls.Load() != 1 {
		t.Fatal("SDK drops its session before failed Close; retry cannot prove shutdown")
	}
}

// Real v2.2.0 disconnected events carry the legacy ErrorCode shape unchanged.
type legacyNgrokEventError struct{ code string }

func (e legacyNgrokEventError) Error() string     { return "legacy echoed " + ngrokTestSecret }
func (e legacyNgrokEventError) Msg() string       { return ngrokTestSecret }
func (e legacyNgrokEventError) ErrorCode() string { return e.code }

func TestNgrokLegacyEventErrorCodesNeverLeak(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"ERR_NGROK_107", ErrCredentialsRejected}, {"ERR_NGROK_108", ErrServiceLimit}, {"ERR_NGROK_UNKNOWN", ErrTransient}} {
		err := ngrokFailure(fmt.Errorf("secret wrapper %s: %w", ngrokTestSecret, legacyNgrokEventError{tc.code}))
		if err != tc.want || strings.Contains(fmt.Sprintf("%v %#v", err, err), ngrokTestSecret) {
			t.Errorf("legacy %s classification: %v", tc.code, err)
		}
	}
}

// The injected SDK dialer never opens a socket. The actual pinned SDK emits its
// actual registered disconnected handler while Listen is retrying connection.
type failingNgrokDialer struct {
	failure error
	calls   atomic.Int32
}

func (d *failingNgrokDialer) Dial(string, string) (net.Conn, error) {
	d.calls.Add(1)
	return nil, d.failure
}
func (d *failingNgrokDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	d.calls.Add(1)
	return nil, d.failure
}
func TestNgrokActualRegisteredSDKHandlerNormalizesTypedEvents(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"ERR_NGROK_107", ErrCredentialsRejected}, {"ERR_NGROK_108", ErrServiceLimit}, {"ERR_NGROK_UNKNOWN", ErrTransient}} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%v", tc.code, legacy), func(t *testing.T) {
				var failure error = fakeNgrokError{tc.code}
				if legacy {
					failure = legacyNgrokEventError{tc.code}
				}
				dialer := &failingNgrokDialer{failure: fmt.Errorf("wrapped %s: %w", ngrokTestSecret, failure)}
				events := make(chan error, 1)
				sdk, err := newNgrokSDKAgentWithFactory(Secret(ngrokTestSecret), func(err error) {
					select {
					case events <- err:
					default:
					}
				}, func(opts ...ngrok.AgentOption) (ngrok.Agent, error) {
					return ngrok.NewAgent(append(opts, ngrok.WithDialer(dialer))...)
				})
				if err != nil {
					t.Fatal("actual no-network constructor", err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				joined := make(chan struct{})
				go func() { defer close(joined); sdk.Listen(ctx, ngrokTestOrigin) }()
				select {
				case got := <-events:
					if got != tc.want || strings.Contains(fmt.Sprintf("%v %#v", got, got), ngrokTestSecret) {
						t.Errorf("actual registered handler classification: %v", got)
					}
				case <-ctx.Done():
					t.Error("actual SDK handler did not receive dial failure")
				}
				cancel()
				<-joined
				if dialer.calls.Load() == 0 {
					t.Error("test did not exercise actual SDK callback")
				}
				if err := sdk.Disconnect(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestNgrokPendingListenFailureCancelsRetriesAndOwnsLateEndpoint(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"ERR_NGROK_107", ErrCredentialsRejected}, {"ERR_NGROK_108", ErrServiceLimit}} {
		t.Run(tc.code, func(t *testing.T) {
			emitted, cancelled, late := make(chan struct{}), make(chan struct{}), make(chan struct{})
			endpoint := fakeEndpoint(ngrokTestOrigin)
			var agent *fakeNgrokAgent
			a := &ngrokAdapter{newAgent: func(_ Secret, event func(error)) (ngrokAgent, error) {
				agent = &fakeNgrokAgent{listen: func(ctx context.Context, origin string) (ngrokEndpoint, error) {
					if origin != ngrokTestOrigin {
						return nil, ErrIdentityChanged
					}
					event(fmt.Errorf("%w", legacyNgrokEventError{tc.code}))
					close(emitted)
					<-ctx.Done()
					close(cancelled)
					<-late
					return endpoint, nil
				}}
				return agent, nil
			}}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			type outcome struct {
				s   Session
				err error
			}
			result := make(chan outcome, 1)
			go func() {
				s, err := a.Open(parent, ngrokIntent(), Secret(ngrokTestSecret), nil)
				result <- outcome{s, err}
			}()
			<-emitted
			var got outcome
			select {
			case got = <-result:
			case <-time.After(200 * time.Millisecond):
				cancel()
				got = <-result
				t.Error("terminal event did not finish initial Open")
			}
			if got.err != tc.want || got.s == nil {
				t.Errorf("terminal initial event classification/ownership: %v", got.err)
			}
			<-cancelled
			short, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer stop()
			if got.s.Close(short) == nil || agent.disconnects.Load() != 0 {
				t.Error("cleanup raced pending Listen or lost ownership")
			}
			close(late)
			ngrokClose(t, got.s)
			if got.s.Listener() != nil || got.s.Origin() != "" || endpoint.accepts.Load() != 0 || endpoint.closes.Load() != 1 || agent.disconnects.Load() != 1 {
				t.Error("late terminal listener served or leaked")
			}
		})
	}
}

func TestNgrokEarlierTransientDoesNotMaskLaterInitialTerminal(t *testing.T) {
	for _, want := range []error{ErrCredentialsRejected, ErrServiceLimit} {
		t.Run(want.Error(), func(t *testing.T) {
			a := &ngrokAdapter{newAgent: func(_ Secret, event func(error)) (ngrokAgent, error) {
				return &fakeNgrokAgent{listen: func(ctx context.Context, _ string) (ngrokEndpoint, error) {
					event(errors.New(ngrokTestSecret))
					// Emit after the earlier failure has been consumed and cancelled retries.
					select {
					case <-ctx.Done():
					case <-time.After(200 * time.Millisecond):
					}
					event(want)
					return nil, ctx.Err()
				}}, nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			session, err := a.Open(ctx, ngrokIntent(), Secret(ngrokTestSecret), nil)
			ngrokClose(t, session)
			if err != want {
				t.Errorf("earlier transient masked terminal: %v", err)
			}
		})
	}
}

func TestNgrokActualSDKInitialEventsFinishAdapterOpenWithoutNetwork(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"ERR_NGROK_107", ErrCredentialsRejected}, {"ERR_NGROK_108", ErrServiceLimit}, {"ERR_NGROK_UNKNOWN", ErrTransient}} {
		t.Run(tc.code, func(t *testing.T) {
			dialer := &failingNgrokDialer{failure: fmt.Errorf("%w", legacyNgrokEventError{tc.code})}
			a := &ngrokAdapter{newAgent: func(secret Secret, event func(error)) (ngrokAgent, error) {
				return newNgrokSDKAgentWithFactory(secret, event, func(opts ...ngrok.AgentOption) (ngrok.Agent, error) {
					return ngrok.NewAgent(append(opts, ngrok.WithDialer(dialer))...)
				})
			}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			session, err := a.Open(ctx, ngrokIntent(), Secret(ngrokTestSecret), nil)
			if err != tc.want || session == nil || session.Listener() != nil {
				t.Errorf("actual initial SDK event: %v", err)
			}
			if session != nil {
				ngrokClose(t, session)
			}
			if dialer.calls.Load() == 0 || ctx.Err() != nil {
				t.Error("initial event required caller timeout or bypassed SDK")
			}
		})
	}
}
