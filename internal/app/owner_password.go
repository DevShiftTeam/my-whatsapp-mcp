package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
	"sync"
)

type ownerPasswordLifetime struct {
	mu   sync.Mutex
	auth *localauth.Authenticator
}

// RemoteOwnerAuthenticator is trusted composition only. It is lazy so disabled
// remote access does not touch optional remote files or allocate hash workers.
// Factories capture this shared authenticator and their own generation context.
func (a *Application) RemoteOwnerAuthenticator() (*localauth.Authenticator, error) {
	a.ownerAuth.mu.Lock()
	defer a.ownerAuth.mu.Unlock()
	if a.remoteShutdownPending() {
		return nil, localauth.ErrState
	}
	a.data.RLock()
	closed, paths, lease := a.closed, a.paths, a.lease
	a.data.RUnlock()
	if closed {
		return nil, localauth.ErrState
	}
	if a.ownerAuth.auth == nil {
		auth, err := localauth.NewAuthenticator(paths, lease)
		if err != nil {
			return nil, err
		}
		a.ownerAuth.auth = auth
	}
	return a.ownerAuth.auth, nil
}

// SetRemoteOwnerPassword must be exposed only by authenticated native owner UI.
// Hashing holds no app/lease locks. Close serializes with the eventual commit,
// so a reset cannot be followed by a stale in-flight password write.
func (a *Application) SetRemoteOwnerPassword(ctx context.Context, password []byte) error {
	auth, err := a.RemoteOwnerAuthenticator()
	if err != nil {
		return err
	}
	return auth.SetPassword(ctx, password)
}
func (a *Application) closeOwnerPassword() {
	a.ownerAuth.mu.Lock()
	defer a.ownerAuth.mu.Unlock()
	if a.ownerAuth.auth != nil {
		a.ownerAuth.auth.Close()
		a.ownerAuth.auth = nil
	}
}
