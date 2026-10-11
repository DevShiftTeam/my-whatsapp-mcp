package app

import (
	"context"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
)

const productBackfillLimit = 32

type selectedBackfillRequester interface {
	RequestBackfill(context.Context, ingestion.BackfillRequest) error
}
type originalDownloadWaker interface{ WakeOriginalDownloads() }

// selectedBackfillRequest narrows a committed policy transition. An empty
// selective change never becomes an implicit all-chats request.
func selectedBackfillRequest(previous, current archive.ArchivePolicy) (ingestion.BackfillRequest, bool) {
	request := ingestion.BackfillRequest{PolicyGeneration: current.Generation, Limit: productBackfillLimit}
	if current.Generation == 0 {
		return request, false
	}
	expandedDates := previous.Generation != 0 && (current.DateMode == archive.DateAllAvailable && previous.DateMode != archive.DateAllAvailable || current.DateMode == archive.DateSince && previous.DateMode == archive.DateSince && current.ImportStart.Before(previous.ImportStart))
	if previous.Generation != 0 && previous.RollingRetention > 0 && (current.RollingRetention == 0 || current.RollingRetention > previous.RollingRetention) {
		expandedDates = true
	}
	if current.ChatMode == archive.ChatsAll {
		request.AllSelected = true
		return request, previous.ChatMode != archive.ChatsAll || previous.Generation == 0 || expandedDates
	}
	if current.ChatMode != archive.ChatsSelective {
		return request, false
	}
	old := make(map[archive.ChatID]bool, len(previous.IncludedChats))
	for _, id := range previous.IncludedChats {
		old[id] = true
	}
	seen := make(map[archive.ChatID]bool, productBackfillLimit)
	for _, id := range current.IncludedChats {
		if id == "" || seen[id] || !expandedDates && (previous.ChatMode == archive.ChatsAll || old[id]) {
			continue
		}
		seen[id] = true
		request.Chats = append(request.Chats, id)
		if len(request.Chats) == productBackfillLimit {
			break
		}
	}
	return request, len(request.Chats) != 0
}

func (a *Application) requestSelectedBackfill(ctx context.Context, previous, current archive.ArchivePolicy) {
	if a.runtime == nil || a.paused || a.resources == nil || a.resources.recoveryPending.Load() {
		return
	}
	requester, ok := a.runtime.(selectedBackfillRequester)
	if !ok {
		return
	}
	if request, needed := selectedBackfillRequest(previous, current); needed {
		// Optional media metadata failure must not invalidate the committed source
		// policy or quarantine the archive. Runtime owns its bounded network loop.
		_ = requester.RequestBackfill(ctx, request)
	}
}

func (a *Application) wakeOriginalDownloads() {
	if a.paused || a.resources == nil || a.resources.recoveryPending.Load() {
		return
	}
	if waker, ok := a.runtime.(originalDownloadWaker); ok {
		waker.WakeOriginalDownloads()
	}
}
