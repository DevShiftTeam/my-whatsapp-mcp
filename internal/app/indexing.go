package app

import (
	"context"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/indexing"
)

type indexLifetime struct {
	mu      sync.Mutex
	service *indexing.Service
}

func (r *resources) startIndexing(store *archive.Store, version archive.IndexVersion) {
	if r.native == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.runCancel = cancel
	r.runDone = make(chan struct{})
	go func() {
		defer close(r.runDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			account, err := store.BoundAccount(ctx)
			if err == nil && account != "" {
				r.index.mu.Lock()
				service, e := indexing.NewService(indexing.ServiceConfig{Store: store, Account: account, Version: version, Tokenizer: r.native, Encoder: r.native, Paused: r.compute.Snapshot().Paused})
				if e != nil {
					r.index.mu.Unlock()
					return
				}
				r.index.service = service
				r.index.mu.Unlock()
				_ = service.Run(ctx)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (r *resources) setPaused(paused bool) {
	r.compute.SetPaused(paused)
	r.index.mu.Lock()
	defer r.index.mu.Unlock()
	if r.index.service != nil {
		r.index.service.SetPaused(paused)
	}
}
