package app

import (
	"context"
	"os"
	"sync"

	"github.com/Bodokh/wa-indexer/internal/inference"
	"github.com/Bodokh/wa-indexer/internal/modelstore"
)

// compositeComponents retains every asset owner through supervisor cleanup.
// Even failed acquisition returns the partial owner for actor-managed retries.
type compositeComponents struct {
	mu      sync.Mutex
	model   *modelstore.Lease
	runtime inference.ComponentLease
	names   map[string]string
}

func (l *compositeComponents) Open(name string) (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if name == "runtime" && l.runtime != nil {
		return l.runtime.Open(name)
	}
	if l.model != nil && l.names[name] != "" {
		return l.model.Open(l.names[name])
	}
	return nil, inference.ErrUnavailable
}
func (l *compositeComponents) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runtime != nil {
		if err := l.runtime.Close(); err != nil {
			return err
		}
		l.runtime = nil
	}
	if l.model != nil {
		if err := l.model.Close(); err != nil {
			return err
		}
		l.model = nil
	}
	return nil
}
func (r *resources) acquireNative(ctx context.Context, cfg *NativeConfig, profile inference.Profile) (inference.ComponentLease, error) {
	owner := &compositeComponents{names: make(map[string]string)}
	var err error
	owner.model, err = r.models.Acquire(ctx, cfg.ModelProfileID)
	if err != nil {
		return owner, err
	}
	installed := owner.model.Profile()
	// Installation status never substitutes for exact immutable component hashes.
	for _, component := range installed.Components {
		switch component.Role {
		case "model":
			if component.SHA256 != profile.ModelSHA256 || owner.names["model"] != "" {
				return owner, inference.ErrUnavailable
			}
			owner.names["model"] = component.ID
		case "tokenizer":
			if component.SHA256 != profile.TokenizerSHA256 || owner.names["tokenizer"] != "" {
				return owner, inference.ErrUnavailable
			}
			owner.names["tokenizer"] = component.ID
		}
	}
	if owner.names["model"] == "" || owner.names["tokenizer"] == "" || cfg.RuntimeLease == nil {
		return owner, inference.ErrUnavailable
	}
	owner.runtime, err = cfg.RuntimeLease(ctx)
	if err != nil {
		return owner, err
	}
	if owner.runtime == nil {
		return owner, inference.ErrUnavailable
	}
	return owner, nil
}
