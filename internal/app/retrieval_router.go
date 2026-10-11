package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/retrieval"
	"sync/atomic"
)

// A stable borrowed reader lets existing listeners observe a later verified
// encoder configuration without owning its lifecycle or caching plaintext.
type retrievalRouter struct {
	service atomic.Pointer[retrieval.Service]
}

func (r *retrievalRouter) set(service *retrieval.Service) { r.service.Store(service) }
func (r *retrievalRouter) Search(c context.Context, s archive.ReadScope, q archive.SearchRequest) (archive.SearchPage, error) {
	return r.service.Load().Search(c, s, q)
}
func (r *retrievalRouter) SearchDetails(c context.Context, s archive.ReadScope, q archive.SearchRequest) (retrieval.SearchResult, error) {
	return r.service.Load().SearchDetails(c, s, q)
}
func (r *retrievalRouter) SearchGuarded(c context.Context, s archive.ReadScope, q archive.SearchRequest) (archive.SearchPage, archive.ResponseGuard, error) {
	return r.service.Load().SearchGuarded(c, s, q)
}
func (r *retrievalRouter) Context(c context.Context, s archive.ReadScope, q archive.ContextRequest) (archive.ContextPage, error) {
	return r.service.Load().Context(c, s, q)
}
func (r *retrievalRouter) ContextGuarded(c context.Context, s archive.ReadScope, q archive.ContextRequest) (archive.ContextPage, archive.ResponseGuard, error) {
	return r.service.Load().ContextGuarded(c, s, q)
}
func (r *retrievalRouter) ListChats(c context.Context, s archive.ReadScope, q archive.ChatListRequest) (archive.ChatPage, error) {
	return r.service.Load().ListChats(c, s, q)
}
func (r *retrievalRouter) ListChatsGuarded(c context.Context, s archive.ReadScope, q archive.ChatListRequest) (archive.ChatPage, archive.ResponseGuard, error) {
	return r.service.Load().ListChatsGuarded(c, s, q)
}
func (r *retrievalRouter) Status(c context.Context, s archive.ReadScope) (archive.StatusSnapshot, error) {
	return r.service.Load().Status(c, s)
}
func (r *retrievalRouter) StatusGuarded(c context.Context, s archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	return r.service.Load().StatusGuarded(c, s)
}
func (r *retrievalRouter) CheckResponse(c context.Context, s archive.ReadScope, q archive.ResponseCheck) (archive.ReadSnapshot, error) {
	return r.service.Load().CheckResponse(c, s, q)
}
