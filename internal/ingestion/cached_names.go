package ingestion

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types"
)

const cachedNamesPage = 64

// Optional implementations refresh labels only for rows the archive already
// owns. Recording fixtures and alternate connector stores need not implement it.
type existingSourceNameStore interface {
	ExistingNameTargets(context.Context, string, string, int) ([]archive.ExistingNameTarget, string, error)
	RefreshExistingSourceNames(context.Context, []archive.SourceNames) error
}

func (r *Runtime) wakeSourceNames() {
	if _, ok := r.store.(existingSourceNameStore); !ok || r.client.Store.Contacts == nil {
		return
	}
	r.optionalMu.Lock()
	defer r.optionalMu.Unlock()
	if n := r.optional; n != nil && n.accepting {
		if n.namesPending {
			n.namesAgain = true
		} else {
			n.namesPending = true
		}
		// Retain any queued backfill selection. The pending bit also remains
		// reachable through the existing fixed poll if its wake is coalesced.
		select {
		case n.commands <- optionalCommand{wake: true}:
		default:
		}
	}
}

func validNameTargets(targets []archive.ExistingNameTarget, after, next string) bool {
	if len(targets) > cachedNamesPage || len(next) > 256 || !utf8.ValidString(next) || strings.ContainsRune(next, 0) {
		return false
	}
	last := after
	for _, target := range targets {
		if len(target.Key) == 0 || len(target.Key) > 256 || !utf8.ValidString(target.Key) || strings.ContainsRune(target.Key, 0) || target.Key <= last || (target.Chat == "") == (target.Sender == "") {
			return false
		}
		if target.Chat != "" && !strings.HasPrefix(target.Key, "c:") || target.Sender != "" && !strings.HasPrefix(target.Key, "i:") {
			return false
		}
		last = target.Key
	}
	return next == "" || len(targets) > 0 && next == last
}

func (r *Runtime) optionalNamesStep(n *optionalNetwork) {
	r.optionalMu.Lock()
	pending, after := n.namesPending, n.namesAfter
	r.optionalMu.Unlock()
	if !pending {
		return
	}
	store, ok := r.store.(existingSourceNameStore)
	if !ok || r.client.Store.Contacts == nil {
		return
	}
	account := r.SourceAccount()
	if account == "" || r.client.Store.ID == nil || r.client.Store.ID.ToNonAD().String() != account {
		r.namesRefreshOutcome(false)
		return
	}
	ctx, done := n.attempt(2 * time.Second)
	defer done()
	targets, next, err := store.ExistingNameTargets(ctx, account, after, cachedNamesPage)
	if err == nil && !validNameTargets(targets, after, next) {
		err = archive.ErrInvalidEvent
	}
	updates := make([]archive.SourceNames, 0, len(targets))
	if err == nil {
		for _, target := range targets {
			if !r.optionalReady() || ctx.Err() != nil {
				return
			}
			source := target.Chat
			if source == "" {
				source = target.Sender
			}
			jid, parseErr := types.ParseJID(source)
			if parseErr != nil || !contactJID(jid) {
				continue // Group subjects and other identities are not contacts.
			}
			name, lookupErr := r.cachedContactName(ctx, account, jid)
			if lookupErr != nil {
				err = lookupErr
				break
			}
			if name == "" || name == target.Name {
				continue
			}
			update := archive.SourceNames{Account: account, Chat: target.Chat, Sender: target.Sender}
			if target.Chat != "" {
				update.ChatName = name
			} else {
				update.SenderName = name
			}
			updates = append(updates, update)
		}
	}
	if err == nil && len(updates) != 0 {
		if !r.optionalReady() || ctx.Err() != nil {
			return
		}
		err = store.RefreshExistingSourceNames(ctx, updates)
	}
	if n.ctx.Err() != nil {
		return
	}
	if err != nil {
		// The page cursor stays put; the fixed five-second poll retries. Optional
		// label failures never establish missed source content or authority.
		r.namesRefreshOutcome(false)
		return
	}
	r.namesRefreshOutcome(true)
	r.optionalMu.Lock()
	if next != "" {
		n.namesAfter = next
	} else if n.namesAgain {
		n.namesAfter, n.namesAgain = "", false
	} else {
		n.namesAfter, n.namesPending = "", false
	}
	more := n.namesPending && n.accepting
	r.optionalMu.Unlock()
	if more {
		r.WakeOriginalDownloads()
	}
}

func (r *Runtime) namesRefreshOutcome(success bool) {
	const code = "names_refresh_deferred"
	r.mu.Lock()
	found := false
	for i, value := range r.status.Codes {
		if value == code {
			found = true
			if success {
				r.status.Codes = append(r.status.Codes[:i], r.status.Codes[i+1:]...)
			}
			break
		}
	}
	if !success && !found {
		r.status.Codes = append(r.status.Codes, code)
	}
	r.mu.Unlock()
	if found == success {
		r.notify()
	}
}
