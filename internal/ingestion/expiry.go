package ingestion

import (
	"context"
	"errors"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow/types/events"
	"math"
	"time"
)

// Retain the tightest observed source expiry. Ingest/edited time never moves the
// original start date, and replay cannot lengthen a stored disappearing lifetime.
func (r *Runtime) sourceExpiry(ctx context.Context, e *archive.SourceEvent, event *events.Message) error {
	ephemeral := event.IsEphemeral
	choose := func(candidate time.Time) {
		if candidate.IsZero() {
			return
		}
		if e.ExpiresAt.IsZero() || candidate.Before(e.ExpiresAt) {
			e.ExpiresAt = candidate
		}
	}
	metadata, err := r.store.SourceMetadata(ctx, e.Key)
	if err == nil {
		choose(metadata.Version.ExpiresAt)
	} else if !errors.Is(err, archive.ErrSourceUnavailable) {
		return err
	}
	if web := event.SourceWebMsg; web != nil {
		expiration := web.GetEphemeralExpirationTimestamp()
		start := web.GetEphemeralStartTimestamp()
		duration := uint64(web.GetEphemeralDuration())
		if expiration > 0 || start > 0 || duration > 0 {
			ephemeral = true
		}
		if expiration > 0 {
			if expiration > math.MaxInt64 {
				return archive.ErrMutationMetadataMissing
			}
			choose(time.Unix(int64(expiration), 0))
		}
		if start > 0 && duration > 0 {
			if start > math.MaxInt64-duration {
				return archive.ErrMutationMetadataMissing
			}
			choose(time.Unix(int64(start+duration), 0))
		}
	}
	c := messageContext(event.Message)
	if c == nil {
		c = messageContext(mutationProtocol(event).GetEditedMessage())
	}
	if c != nil && c.GetExpiration() > 0 {
		ephemeral = true
		if e.OriginalDateKnown {
			choose(e.OriginalSentAt.Add(time.Duration(c.GetExpiration()) * time.Second))
		}
	}
	if ephemeral && e.ExpiresAt.IsZero() {
		r.failClosed("mutation_metadata_missing")
		return archive.ErrMutationMetadataMissing
	}
	return nil
}
