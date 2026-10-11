package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/modelstore"
)

// Models exposes pinned public catalog metadata, never installed private paths.
func (a *Application) Models(ctx context.Context) ([]modelstore.Profile, error) {
	a.data.RLock()
	defer a.data.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.closed {
		return nil, archive.ErrArchiveUnavailable
	}
	catalog, err := modelstore.DefaultCatalog()
	if err != nil {
		return nil, err
	}
	return catalog.Profiles, nil
}
func (a *Application) InstallModel(ctx context.Context, id string) error {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.resources == nil || a.resources.models == nil {
		return archive.ErrArchiveUnavailable
	}
	operation, cancel := a.query(ctx)
	defer cancel()
	return a.resources.models.Install(operation, id, nil)
}
func (a *Application) VerifyModel(ctx context.Context, id string) error {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.resources == nil || a.resources.models == nil {
		return archive.ErrArchiveUnavailable
	}
	operation, cancel := a.query(ctx)
	defer cancel()
	_, err := a.resources.models.Verify(operation, id)
	return err
}
func (a *Application) RemoveModel(ctx context.Context, id string) error {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.resources == nil || a.resources.models == nil {
		return archive.ErrArchiveUnavailable
	}
	operation, cancel := a.query(ctx)
	defer cancel()
	return a.resources.models.Remove(operation, id)
}
