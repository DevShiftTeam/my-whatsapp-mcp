package app

import (
	"context"
	"math"
	"net/http"
	"sync"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
)

// RemoteOwnerFactory binds owner authentication to this exact pinned authority.
// It runs outside all app/manager locks; ctx ends with the provider generation.
type RemoteOwnerFactory func(context.Context, remoteaccess.Intent, string, uint64) (http.Handler, error)
type remoteLifecycle struct {
	mu             sync.Mutex
	active, fenced bool
	manager        *remoteaccess.Manager
	generation     uint64
	adapters       remoteAdapterFactory
	owner          RemoteOwnerFactory
}

func (a *Application) beginRemoteOperation(shutdown bool) error {
	r := &a.remoteLifecycle
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active || (!shutdown && r.fenced) {
		return remoteaccess.ErrBusy
	}
	r.active = true
	if shutdown {
		r.fenced = true
	}
	return nil
}
func (a *Application) endRemoteOperation(resetComplete bool) {
	r := &a.remoteLifecycle
	r.mu.Lock()
	r.active = false
	if resetComplete {
		r.fenced = false
		r.manager = nil
	}
	r.mu.Unlock()
}

type remoteAdapterFactory func(appdata.Paths, *appdata.InstanceLock) map[remoteaccess.Provider]remoteaccess.Adapter

// NewDefaultRemoteAccessManager constructs lazy providers from the same paths and
// live lease as its manager. Only the successful serve composition calls it.
func (a *Application) NewDefaultRemoteAccessManager(owner RemoteOwnerFactory) (*remoteaccess.Manager, error) {
	return a.installRemoteAccess(func(p appdata.Paths, l *appdata.InstanceLock) map[remoteaccess.Provider]remoteaccess.Adapter {
		tailscale := remoteaccess.NewTailscaleAdapter(p, l)
		return map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.TailscalePrivate: tailscale, remoteaccess.TailscaleFunnel: tailscale, remoteaccess.Ngrok: remoteaccess.NewNgrokAdapter(), remoteaccess.Cloudflare: remoteaccess.NewCloudflareAdapter(p, l)}
	}, owner)
}

// NewRemoteAccessManager attaches one trusted provider manager to the app's
// archive lifetime. Callers own explicit Configure/Disable/Remove actions; app
// Close/Delete owns stop and join. No provider implementation is selected here.
func (a *Application) NewRemoteAccessManager(adapters map[remoteaccess.Provider]remoteaccess.Adapter, owner RemoteOwnerFactory) (*remoteaccess.Manager, error) {
	copied := make(map[remoteaccess.Provider]remoteaccess.Adapter, len(adapters))
	for k, v := range adapters {
		copied[k] = v
	}
	return a.installRemoteAccess(func(appdata.Paths, *appdata.InstanceLock) map[remoteaccess.Provider]remoteaccess.Adapter {
		return copied
	}, owner)
}
func (a *Application) installRemoteAccess(factory remoteAdapterFactory, owner RemoteOwnerFactory) (*remoteaccess.Manager, error) {
	if err := a.beginRemoteOperation(false); err != nil {
		return nil, err
	}
	var release func()
	defer func() {
		a.endRemoteOperation(false)
		if release != nil {
			release()
		}
	}()
	a.remoteLifecycle.mu.Lock()
	exists := a.remoteLifecycle.manager != nil
	a.remoteLifecycle.mu.Unlock()
	if exists {
		return nil, remoteaccess.ErrBusy
	}
	manager, ready, err := a.constructRemoteAccess(factory, owner)
	release = ready
	if err == nil {
		a.remoteLifecycle.mu.Lock()
		a.remoteLifecycle.adapters = factory
		a.remoteLifecycle.owner = owner
		a.remoteLifecycle.mu.Unlock()
	}
	return manager, err
}

// Caller holds only lifecycle admission. The ready gate is opened after that
// admission ends so restart Prepare cannot contend with its own constructor.
func (a *Application) constructRemoteAccess(factory remoteAdapterFactory, owner RemoteOwnerFactory) (*remoteaccess.Manager, func(), error) {
	a.data.RLock()
	closed := a.closed
	manual := a.mcpConfig.Remote != nil && !a.mcpConfig.Remote.Managed
	paths, lease := a.paths, a.lease
	a.data.RUnlock()
	if closed {
		return nil, nil, remoteaccess.ErrClosed
	}
	if manual {
		return nil, nil, localui.ErrRemoteManual
	}
	ready := make(chan struct{})
	manager, err := remoteaccess.NewManager(remoteaccess.ManagerConfig{Store: remoteaccess.NewStore(paths, lease), Adapters: factory(paths, lease), MCPReady: a.ManagedMCPReady, Prepare: func(ctx context.Context, intent remoteaccess.Intent, origin string, generation uint64) (remoteaccess.Handlers, error) {
		select {
		case <-ready:
		case <-ctx.Done():
			return remoteaccess.Handlers{}, ctx.Err()
		}
		if err := a.beginRemoteOperation(false); err != nil {
			return remoteaccess.Handlers{}, err
		}
		defer a.endRemoteOperation(false)
		// Manager attempts restart at one after reconstruction. App generations
		// never reset, including failed Prepare, disable and archive deletion.
		a.remoteLifecycle.mu.Lock()
		if a.remoteLifecycle.generation == math.MaxUint64 {
			a.remoteLifecycle.mu.Unlock()
			return remoteaccess.Handlers{}, remoteaccess.ErrState
		}
		a.remoteLifecycle.generation++
		generation = a.remoteLifecycle.generation
		a.remoteLifecycle.mu.Unlock()
		// Transport availability never declares an owner account/org capability.
		// Eligible Claude connectors retain the explicit manual launch path.
		if err := a.configureManagedMCP(ctx, &ManagedMCPConfig{PublicURL: origin + "/mcp", HeaderAuthAvailable: false, Generation: generation}); err != nil {
			return remoteaccess.Handlers{}, err
		}
		handlers := remoteaccess.Handlers{MCP: a.ManagedMCPHandler(), Release: func(ctx context.Context) error { return a.ClearManagedMCP(ctx, generation) }}
		if intent.ExposeBrowser {
			if owner == nil {
				return handlers, remoteaccess.ErrPrepare
			}
			ownerHandler, ownerErr := owner(ctx, intent, origin, generation)
			handlers.Owner = ownerHandler
			if ownerErr != nil || handlers.Owner == nil {
				return handlers, remoteaccess.ErrPrepare
			}
		}
		return handlers, nil
	}})
	if err != nil {
		close(ready)
		return nil, nil, err
	}
	a.remoteLifecycle.mu.Lock()
	a.remoteLifecycle.manager = manager
	a.remoteLifecycle.mu.Unlock()
	return manager, func() { close(ready) }, nil
}

// The caller holds lifecycle admission, never ops/data/stateMu. A failed or
// pending manager Close leaves the fence set and prohibits archive teardown.
func (a *Application) stopRemoteAccess(ctx context.Context) error {
	a.remoteLifecycle.mu.Lock()
	manager := a.remoteLifecycle.manager
	a.remoteLifecycle.mu.Unlock()
	if manager != nil {
		if err := manager.Close(ctx); err != nil {
			return err
		}
	}
	a.managedMCP.mu.Lock()
	generation := a.managedMCP.generation
	a.managedMCP.mu.Unlock()
	return a.ClearManagedMCP(ctx, generation)
}

func (a *Application) remoteShutdownPending() bool {
	a.remoteLifecycle.mu.Lock()
	defer a.remoteLifecycle.mu.Unlock()
	return a.remoteLifecycle.fenced
}

// Resolve the current manager under app lifecycle admission, then use its own
// nonqueueing operation gate. Reset retains only trusted constructor inputs.
func (a *Application) withRemoteManager(ctx context.Context, action func(*remoteaccess.Manager) error) error {
	manager, err := a.currentRemoteManager(ctx)
	if err != nil {
		return err
	}
	// Manager admission serializes this with Close. If reset won after lookup,
	// this exact old manager is permanently closed and rejects the operation.
	// Release app admission before Configure can start a Prepare callback.
	return action(manager)
}
func (a *Application) currentRemoteManager(ctx context.Context) (*remoteaccess.Manager, error) {
	if ctx.Err() != nil {
		return nil, remoteaccess.ErrTransient
	}
	// Existing managers own their operation/close admission. A lookup must not
	// reserve app admission and make an unrelated worker Prepare fail busy.
	a.remoteLifecycle.mu.Lock()
	current, blocked := a.remoteLifecycle.manager, a.remoteLifecycle.active || a.remoteLifecycle.fenced
	a.remoteLifecycle.mu.Unlock()
	if blocked {
		return nil, remoteaccess.ErrBusy
	}
	if current != nil {
		return current, nil
	}
	if err := a.beginRemoteOperation(false); err != nil {
		return nil, err
	}
	var release func()
	defer func() {
		a.endRemoteOperation(false)
		if release != nil {
			release()
		}
	}()
	a.data.RLock()
	manual := a.mcpConfig.Remote != nil && !a.mcpConfig.Remote.Managed
	closed := a.closed
	a.data.RUnlock()
	if manual {
		return nil, localui.ErrRemoteManual
	}
	if closed {
		return nil, remoteaccess.ErrClosed
	}
	a.remoteLifecycle.mu.Lock()
	manager, factory, owner := a.remoteLifecycle.manager, a.remoteLifecycle.adapters, a.remoteLifecycle.owner
	a.remoteLifecycle.mu.Unlock()
	if manager == nil {
		if factory == nil {
			return nil, remoteaccess.ErrState
		}
		var err error
		manager, release, err = a.constructRemoteAccess(factory, owner)
		if err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func (a *Application) RemoteAccessStatus(ctx context.Context) (localui.RemoteAccessSnapshot, error) {
	var result localui.RemoteAccessSnapshot
	a.data.RLock()
	manual := a.mcpConfig.Remote != nil && !a.mcpConfig.Remote.Managed
	a.data.RUnlock()
	if manual {
		return localui.RemoteAccessSnapshot{State: "manual", Code: "remote_manual_mode", Manual: true}, nil
	}
	e := a.withRemoteManager(ctx, func(m *remoteaccess.Manager) error {
		s := m.Status()
		result = localui.RemoteAccessSnapshot{Provider: s.Provider, State: s.State, Code: s.Code, SavedOrigin: s.SavedOrigin, BrowserEnabled: s.BrowserEnabled, MCPReady: s.MCPReady, AuthorizationReady: a.ManagedMCPReady()}
		a.data.RLock()
		paths, lease := a.paths, a.lease
		a.data.RUnlock()
		intent, secret, err := remoteaccess.NewStore(paths, lease).Load(ctx)
		clear(secret)
		if err == nil {
			result.NodeName = intent.NodeName
		}
		auth, err := a.RemoteOwnerAuthenticator()
		if err == nil {
			result.PasswordConfigured = auth.Generation() != 0
		}
		return nil
	})
	return result, e
}
func (a *Application) ConfigureRemoteAccess(ctx context.Context, v localui.RemoteAccessSubmission) error {
	if e := localui.ValidateRemoteSubmission(v); e != nil {
		return e
	}
	return a.withRemoteManager(ctx, func(m *remoteaccess.Manager) error {
		if v.Intent.ExposeBrowser {
			auth, e := a.RemoteOwnerAuthenticator()
			if e != nil || auth.Generation() == 0 {
				return localui.ErrRemotePasswordRequired
			}
		}
		secret := append(remoteaccess.Secret{}, v.Secret...)
		defer func() { clear(secret) }()
		if len(secret) == 0 && (v.Intent.Provider == remoteaccess.Ngrok || v.Intent.Provider == remoteaccess.Cloudflare) {
			a.data.RLock()
			paths, lease := a.paths, a.lease
			a.data.RUnlock()
			old, saved, e := remoteaccess.NewStore(paths, lease).Load(ctx)
			if e != nil || old.Provider != v.Intent.Provider {
				clear(saved)
				return remoteaccess.ErrInvalidIntent
			}
			secret = saved
			if len(secret) == 0 {
				return remoteaccess.ErrInvalidIntent
			}
		}
		return m.Configure(ctx, v.Intent, secret)
	})
}
func (a *Application) DisableRemoteAccess(ctx context.Context) error {
	return a.withRemoteManager(ctx, func(m *remoteaccess.Manager) error { return m.Disable(ctx) })
}
func (a *Application) RemoveRemoteAccess(ctx context.Context) error {
	return a.withRemoteManager(ctx, func(m *remoteaccess.Manager) error { return m.Remove(ctx) })
}
func (a *Application) RemoteAccessSignIn(ctx context.Context) (string, error) {
	var url string
	e := a.withRemoteManager(ctx, func(m *remoteaccess.Manager) error { url = m.OwnerNotice().AuthURL; return nil })
	return url, e
}
