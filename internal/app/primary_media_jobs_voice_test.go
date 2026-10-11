//go:build sqlite_fts5

package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestPrimaryVoicePermanentHTTPFailuresReleasePinsWithoutReclaim(t *testing.T) {
	for _, response := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(response), func(t *testing.T) {
			f := newVoiceHTTPFixture(t, true)
			f.rejectStatus.Store(int64(response))
			a, _ := voiceTestApplication(t, f)
			addVoiceTestOriginal(t, a, "permanent-voice-input-failure")
			waitVoiceCondition(t, func() bool {
				status, err := a.Status(context.Background())
				return err == nil && status.Media.Failed == 1 && status.Media.Pending == 0
			})
			pinned, err := a.store.ListPinnedExtractions(context.Background(), archive.ExtractionRecoveryRequest{Account: demoAccount, Limit: 64})
			if err != nil || len(pinned.Items) != 0 {
				t.Fatal("permanent voice failure retained a physical input pin")
			}
			a.resources.voice.mu.Lock()
			version := a.resources.voice.backend.Version()
			a.resources.voice.mu.Unlock()
			work, err := a.store.ClaimExtraction(context.Background(), archive.ExtractionClaim{Account: demoAccount, Version: version, TTL: time.Minute})
			if !errors.Is(err, archive.ErrNoWork) || work.ID != "" {
				t.Fatal("permanent rejected input remained eligible for repeated decoding")
			}
			if f.calls.Load() != 1 || len(voiceSearch(t, a).Hits) != 0 {
				t.Fatal("permanent rejected input was repeated or published a transcript")
			}
		})
	}
}
