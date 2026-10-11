package app

import (
	"github.com/Bodokh/my-whatsapp-mcp/internal/inference"
	"os"
)

// The holder keeps the same shared model-lock description alive after parent
// death. It is not another model acquisition or scheduler.
func (l *compositeComponents) WorkerGuard() (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.model == nil {
		return nil, inference.ErrUnavailable
	}
	return l.model.WorkerGuard()
}
