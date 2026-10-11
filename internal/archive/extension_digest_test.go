//go:build sqlite_fts5

package archive

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func digestGoldenFixture() (IndexVersion, []SourceRef, []Document) {
	a := SourceRef{Account: "account", Version: SourceVersion{MessageID: "source-a", Revision: 2, PolicyGeneration: 7}, ContentHash: strings.Repeat("a", 64)}
	b := SourceRef{Account: "account", Version: SourceVersion{MessageID: "source-b", Revision: 9, PolicyGeneration: 7, ExpiresAt: time.UnixMilli(1800000000123).UTC()}, ContentHash: strings.Repeat("b", 64)}
	d := Document{ID: "window-a", Kind: DocumentWindow, Text: "שלום\ncaption", Sources: []SourceRef{b, a}, Spans: []SourceSpan{{Source: b, StartByte: 0, EndByte: 8, Field: "body"}, {Source: a, StartByte: 0, EndByte: 7, Field: "caption"}}, Provenance: DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"}, PassageTokens: 3}
	return indexTestVersion("golden-v1"), []SourceRef{b, a}, []Document{d}
}

func TestCanonicalWorkDigestGoldenAndOrderedIdentity(t *testing.T) {
	v, sources, documents := digestGoldenFixture()
	compute := func(refs []SourceRef, docs []Document) string {
		t.Helper()
		hash, err := CanonicalWorkDigest("generation-a", v, JobTextEmbedding, "", refs, docs)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	want := "d51bb1f0691cb4b21d3928254ad1f588d8be7a38099f909abc574a6bff1152ee"
	if got := compute(sources, documents); got != want {
		t.Fatalf("fixed work-digest-v1 fixture changed: %s", got)
	}
	if got := compute([]SourceRef{sources[1], sources[0]}, documents); got != want {
		t.Fatal("source set ordering changed digest")
	}
	reversed := extensionCloneDocuments(documents)
	reversed[0].Spans[0], reversed[0].Spans[1] = reversed[0].Spans[1], reversed[0].Spans[0]
	if compute(sources, reversed) == want {
		t.Fatal("original span ordering was discarded")
	}
	changed := extensionCloneDocuments(documents)
	changed[0].Text = "שלוםcaption"
	if compute(sources, changed) == want {
		t.Fatal("passage text identity was discarded")
	}
}

func TestCanonicalWorkDigestRejectsUnboundedOrAmbiguousMetadata(t *testing.T) {
	for _, name := range []string{"nested_sources", "nested_spans", "profile", "invalid_utf8", "duplicate_source", "terminal_source", "noncanonical_hash", "submillisecond_expiry", "span_membership"} {
		t.Run(name, func(t *testing.T) {
			v, sources, documents := digestGoldenFixture()
			want := ErrInvalidExtension
			switch name {
			case "nested_sources":
				documents[0].Sources = make([]SourceRef, extensionMaxSources)
				for i := range documents[0].Sources {
					documents[0].Sources[i] = sources[0]
				}
				documents = append(documents, documents[0])
				want = ErrLimitExceeded
			case "nested_spans":
				documents[0].Spans = make([]SourceSpan, extensionMaxSources)
				for i := range documents[0].Spans {
					documents[0].Spans[i] = SourceSpan{Source: sources[0], StartByte: 0, EndByte: 1, Field: "body"}
				}
				documents = append(documents, documents[0])
				want = ErrLimitExceeded
			case "profile":
				documents[0].Provenance.ProfileID = strings.Repeat("a", 129)
			case "invalid_utf8":
				documents[0].Provenance.PromptVersion = string([]byte{0xff})
			case "duplicate_source":
				sources = append(sources, sources[0])
			case "terminal_source":
				sources[0].Version.Deleted = true
			case "noncanonical_hash":
				sources[0].ContentHash = strings.Repeat("B", 64)
			case "submillisecond_expiry":
				sources[0].Version.ExpiresAt = sources[0].Version.ExpiresAt.Add(time.Nanosecond)
			case "span_membership":
				documents[0].Spans[0].Source.Version.MessageID = "unbound"
			}
			if _, err := CanonicalWorkDigest("generation-a", v, JobTextEmbedding, "", sources, documents); !errors.Is(err, want) {
				t.Fatalf("%s accepted: %v", name, err)
			}
		})
	}
}

func TestSourceMetadataHashGoldenAndStrictLabels(t *testing.T) {
	hash, err := sourceMetadataHash("שלום", "Sender")
	if err != nil || hex.EncodeToString(hash[:]) != "4e1b9cbfcd02e94892a3b42f4b450fb552cb98371bc3ad8404ef389768e45935" {
		t.Fatalf("display metadata v1 changed: %x %v", hash, err)
	}
	left, _ := sourceMetadataHash("ab", "c")
	right, _ := sourceMetadataHash("a", "bc")
	if left == right {
		t.Fatal("label boundaries are ambiguous")
	}
	if empty, e := sourceMetadataHash("", ""); e != nil || empty == ([32]byte{}) {
		t.Fatal("empty authoritative labels must produce a usable pin")
	}
	for _, invalid := range []string{strings.Repeat("a", 257), "contains\x00nul", string([]byte{0xff})} {
		if _, err := sourceMetadataHash(invalid, "valid"); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("invalid display metadata accepted: %v", err)
		}
	}
}

func TestCanonicalOriginWorkDigestGoldenAndCompleteIdentity(t *testing.T) {
	v, refs, _ := digestGoldenFixture()
	origin := DerivedOrigin{ID: "root-a", TextSHA256: strings.Repeat("c", 64), Bytes: 123, Sources: refs,
		Provenance: DerivedProvenance{Kind: DocumentOCR, MediaID: "media-a", ProfileID: "profile-a", ExtractorVersion: "extractor-v1", PromptVersion: "none-v1", OffsetKind: "none"}}
	compute := func(g IndexGenerationID, version IndexVersion, value DerivedOrigin) string {
		t.Helper()
		hash, err := CanonicalOriginWorkDigest(g, version, value)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	want := "deb1fdf21c37a3591b9646662fc13d81d688ec391d2aac9c5449d3393d9442d0"
	if got := compute("generation-a", v, origin); got != want {
		t.Fatalf("fixed origin-work-digest-v1 fixture changed: %s", got)
	}
	reordered := origin
	reordered.Sources = []SourceRef{refs[1], refs[0]}
	if compute("generation-a", v, reordered) != want {
		t.Fatal("source set ordering changed immutable origin identity")
	}
	for _, change := range []string{"generation", "version", "origin", "hash", "bytes", "media", "profile", "source", "expiry"} {
		t.Run(change, func(t *testing.T) {
			g, version, value := IndexGenerationID("generation-a"), v, origin
			value.Sources = append([]SourceRef(nil), origin.Sources...)
			switch change {
			case "generation":
				g = "generation-b"
			case "version":
				version.ChunkingVersion = "streaming-v2"
			case "origin":
				value.ID = "root-b"
			case "hash":
				value.TextSHA256 = strings.Repeat("d", 64)
			case "bytes":
				value.Bytes++
			case "media":
				value.Provenance.MediaID = "media-b"
			case "profile":
				value.Provenance.ProfileID = "profile-b"
			case "source":
				value.Sources[0].Version.Revision++
			case "expiry":
				value.Sources[0].Version.ExpiresAt = value.Sources[0].Version.ExpiresAt.Add(time.Millisecond)
			}
			if compute(g, version, value) == want {
				t.Fatal("immutable identity omitted " + change)
			}
		})
	}
	for _, invalid := range []string{"bounds", "empty", "duplicate", "terminal", "hash", "long_hash", "long_source_hash", "provenance", "expiry"} {
		t.Run(invalid, func(t *testing.T) {
			value := origin
			value.Sources = append([]SourceRef(nil), origin.Sources...)
			wantError := ErrInvalidExtension
			switch invalid {
			case "bounds":
				value.Bytes = (64 << 10) + 1
				wantError = ErrLimitExceeded
			case "empty":
				value.Bytes = 0
			case "duplicate":
				value.Sources = append(value.Sources, value.Sources[0])
			case "terminal":
				value.Sources[0].Version.Deleted = true
			case "hash":
				value.TextSHA256 = strings.Repeat("C", 64)
			case "long_hash":
				value.TextSHA256 = strings.Repeat("c", 4096)
			case "long_source_hash":
				value.Sources[0].ContentHash = strings.Repeat("c", 4096)
			case "provenance":
				value.Provenance.Kind = DocumentWindow
			case "expiry":
				value.Sources[0].Version.ExpiresAt = value.Sources[0].Version.ExpiresAt.Add(time.Nanosecond)
			}
			if _, err := CanonicalOriginWorkDigest("generation-a", v, value); !errors.Is(err, wantError) {
				t.Fatalf("invalid origin identity accepted: %v", err)
			}
		})
	}
}
