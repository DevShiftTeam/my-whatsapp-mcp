package retrieval

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// Generated OCR/caption/speech must never occupy an original-text snippet.
func TestDerivedExcerptsKeepLabelsOffsetsAndExactSources(t *testing.T) {
	for _, kind := range []archive.DocumentKind{archive.DocumentOCR, archive.DocumentCaption, archive.DocumentTranscript} {
		t.Run(string(kind), func(t *testing.T) {
			r := ref("derived", kind, "source")
			provenance := archive.DerivedProvenance{Kind: kind, MediaID: "media", ProfileID: "fixture-profile", ExtractorVersion: "fixture-v1", OffsetKind: "none"}
			if kind == archive.DocumentTranscript {
				provenance.OffsetKind = "slice"
				provenance.StartMS = 30000
				provenance.EndMS = 60000
			}
			document := archive.RetrievedDocument{Ref: r, Kind: archive.MessageImage, ChatID: "chat", ChatName: "כּותרת", SenderName: "sender", SentAt: time.Unix(1, 0), Excerpts: []archive.DerivedExcerpt{{DocumentID: r.ID, Source: r.Sources[0].Version, Text: "generated untrusted text", Provenance: provenance}}}
			hit, e := searchHit(document, rankedCandidate{ref: r, score: 1. / 61})
			if e != nil {
				t.Fatal(e)
			}
			if hit.Snippet != "" || len(hit.Derived) != 1 || hit.Derived[0].Provenance != provenance || len(hit.Sources) != 1 || hit.Sources[0] != r.Sources[0].Version {
				t.Fatalf("provenance lost %#v", hit)
			}
			encoded, _ := json.Marshal(hit)
			if strings.Contains(string(encoded), "ContentHash") || strings.Contains(string(encoded), "Account") {
				t.Fatal("private source identity leaked")
			}
		})
	}
}
func TestHydrationCannotSubstituteSourceMembership(t *testing.T) {
	wanted := ref("window", archive.DocumentWindow, "one", "two")
	changed := ref("window", archive.DocumentWindow, "one")
	if _, e := searchHit(archive.RetrievedDocument{Ref: changed}, rankedCandidate{ref: wanted}); !errors.Is(e, archive.ErrSourceUnavailable) {
		t.Fatalf("membership substitution accepted: %v", e)
	}
}
func TestExpiredAndCrossServiceCursorRejected(t *testing.T) {
	s := &Service{}
	s.cursorKey[0] = 1
	now := time.Now().Add(-6 * time.Minute)
	token, e := s.encodeCursor(cursorPayload{Schema: 1, Offset: 1, IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(cursorLifetime).UnixMilli()})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.decodeCursor(token); !errors.Is(e, archive.ErrCursorStale) {
		t.Fatalf("expired cursor accepted %v", e)
	}
	now = time.Now()
	token, e = s.encodeCursor(cursorPayload{Schema: 1, Offset: 1, IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(cursorLifetime).UnixMilli()})
	if e != nil {
		t.Fatal(e)
	}
	other := &Service{}
	other.cursorKey[0] = 2
	if _, e = other.decodeCursor(token); !errors.Is(e, archive.ErrCursorStale) {
		t.Fatalf("cross-service cursor accepted %v", e)
	}
}

// A config with unsafe dependency/limits must fail before any source is read.
func TestInvalidConfiguration(t *testing.T) {
	if _, e := NewService(Config{}); !errors.Is(e, ErrConfiguration) {
		t.Fatal(e)
	}
}
