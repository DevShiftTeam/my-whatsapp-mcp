package app

import (
	"github.com/Bodokh/wa-indexer/internal/mcpserver"
	"sync"
)

// Remote publication is independent of native authentication and contains no
// token, path, policy copy or I/O capability.
type publishedRemoteRegistry struct {
	mu       sync.RWMutex
	bindings map[mcpserver.RemoteClientBinding]bool
}

func (r *publishedRemoteRegistry) PublishedRemoteClient(binding mcpserver.RemoteClientBinding) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bindings[binding]
}

func (r *publishedRemoteRegistry) replace(bindings []mcpserver.RemoteClientBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings = make(map[mcpserver.RemoteClientBinding]bool, len(bindings))
	for _, binding := range bindings {
		r.bindings[binding] = true
	}
}

func remoteBinding(r remoteClientRecord) mcpserver.RemoteClientBinding {
	return mcpserver.RemoteClientBinding{Account: r.Account, Principal: r.Principal, CredentialID: r.CredentialID}
}
