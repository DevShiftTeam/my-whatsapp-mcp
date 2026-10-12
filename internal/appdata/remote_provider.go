package appdata

import (
	"errors"
	"os"
	"path/filepath"
)

// RemoteProvider selects fixed application-owned state, never an owner-supplied path.
type RemoteProvider string

const (
	TailscaleProvider  RemoteProvider = "tailscale"
	CloudflareProvider RemoteProvider = "cloudflare"
)

var ErrRemoteProviderActive = errors.New("remote_provider_active")

// RemoteProviderDirectory owns independent anchors and registers a live SDK/helper
// lifetime. Stop and join the provider before Close. Reset, removal, and lease
// release refuse while registered. SDK pathname opens remain snapshot validated:
// these anchors cannot force a third-party SDK to use descriptor-relative opens.
type RemoteProviderDirectory struct {
	paths            Paths
	lease            *InstanceLock
	provider         RemoteProvider
	app, remote, dir *os.Root
	closed           bool
}

func OpenRemoteProviderDirectory(paths Paths, lease *InstanceLock, provider RemoteProvider) (*RemoteProviderDirectory, error) {
	if provider != TailscaleProvider && provider != CloudflareProvider {
		return nil, ErrUnsafeDataPath
	}
	if lease == nil {
		return nil, ErrLockRequired
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.remoteProviders[provider] != nil {
		return nil, ErrRemoteProviderActive
	}
	app, err := openLeaseRoot(paths, lease)
	if err != nil {
		return nil, err
	}
	d := &RemoteProviderDirectory{paths: paths, lease: lease, provider: provider, app: app}
	success := false
	defer func() {
		if !success {
			d.closeRoots()
		}
	}()
	if err := verifyRemoteTree(app, lease); err != nil {
		return nil, err
	}
	remoteBefore, err := app.Lstat("remote")
	if err != nil || !privateDirectory(remoteBefore) {
		return nil, ErrUnsafeDataPath
	}
	remote, err := app.OpenRoot("remote")
	if err != nil {
		return nil, ErrUnsafeDataPath
	}
	d.remote = remote
	remoteOpened, err := remote.Stat(".")
	if err != nil || !os.SameFile(remoteBefore, remoteOpened) {
		return nil, ErrUnsafeDataPath
	}
	if err := remote.Mkdir(string(provider), 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, ErrDataUnavailable
	}
	before, err := remote.Lstat(string(provider))
	if err != nil || !privateDirectory(before) {
		return nil, ErrUnsafeDataPath
	}
	dir, err := remote.OpenRoot(string(provider))
	if err != nil {
		return nil, ErrUnsafeDataPath
	}
	d.dir = dir
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrUnsafeDataPath
	}
	if err := d.validateLocked(); err != nil {
		return nil, err
	}
	if err := verifyRemoteTree(app, lease); err != nil {
		return nil, err
	}
	if err := syncDirectory(remote); err != nil {
		return nil, err
	}
	if lease.remoteProviders == nil {
		lease.remoteProviders = make(map[RemoteProvider]*RemoteProviderDirectory)
	}
	lease.remoteProviders[provider] = d
	success = true
	return d, nil
}
func (d *RemoteProviderDirectory) validateLocked() error {
	if d.closed {
		return ErrLockRequired
	}
	current, err := openLeaseRoot(d.paths, d.lease)
	if err != nil {
		return err
	}
	defer current.Close()
	for _, a := range []struct {
		root *os.Root
		name string
	}{{d.app, "."}, {d.remote, "remote"}, {d.dir, "remote/" + string(d.provider)}} {
		held, e := a.root.Stat(".")
		now, n := current.Lstat(a.name)
		if e != nil || n != nil || !privateDirectory(held) || !privateDirectory(now) || !os.SameFile(held, now) {
			return ErrUnsafeDataPath
		}
		if err := verifyNoExtendedACL(current, a.name, now); err != nil {
			return err
		}
	}
	return nil
}
func (d *RemoteProviderDirectory) Validate() error {
	if d == nil || d.lease == nil {
		return ErrLockRequired
	}
	d.lease.mu.Lock()
	defer d.lease.mu.Unlock()
	return d.validateLocked()
}
func (d *RemoteProviderDirectory) Path() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	return filepath.Join(d.paths.RemoteDir, string(d.provider)), nil
}

// OpenFile grants the caller ownership of a private direct-child descriptor.
// Creation is exclusive. The caller writes/syncs and closes before provider Close.
func (d *RemoteProviderDirectory) OpenFile(name string, create bool) (*os.File, error) {
	if d == nil || d.lease == nil {
		return nil, ErrLockRequired
	}
	d.lease.mu.Lock()
	defer d.lease.mu.Unlock()
	if err := d.validateLocked(); err != nil {
		return nil, err
	}
	return OpenRemoteAccessFile(d.dir, name, create)
}
func (d *RemoteProviderDirectory) closeRoots() error {
	var failed bool
	for _, root := range []*os.Root{d.dir, d.remote, d.app} {
		if root != nil && root.Close() != nil {
			failed = true
		}
	}
	if failed {
		return ErrDataUnavailable
	}
	return nil
}
func (d *RemoteProviderDirectory) Close() error {
	if d == nil || d.lease == nil {
		return nil
	}
	d.lease.mu.Lock()
	defer d.lease.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	err := d.closeRoots()
	delete(d.lease.remoteProviders, d.provider)
	return err
}

// RemoveFile removes only a qualified private direct-child snapshot. Callers
// must stop and join any child using it before replacing immutable launch files.
func (d *RemoteProviderDirectory) RemoveFile(name string) error {
	if d == nil || d.lease == nil {
		return ErrLockRequired
	}
	d.lease.mu.Lock()
	defer d.lease.mu.Unlock()
	if err := d.validateLocked(); err != nil {
		return err
	}
	file, err := OpenRemoteAccessFile(d.dir, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return ErrDataUnavailable
	}
	if err := d.dir.Remove(name); err != nil {
		return ErrDataUnavailable
	}
	return syncDirectory(d.dir)
}
