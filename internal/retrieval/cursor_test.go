//go:build sqlite_fts5

package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

func TestBoundedOpaqueCursorRecomputesWithoutDuplicates(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 7; i++ {
		f.add(t, fmt.Sprintf("page-%d", i), "chat", "keyword page body")
	}
	s := service(t, f.config(archive.IndexGeneration{}))
	request := archive.SearchRequest{Query: "keyword", Limit: 2}
	seen := map[archive.MessageID]bool{}
	for count := 0; count < 5; count++ {
		result, e := s.SearchDetails(context.Background(), f.scope, request)
		if e != nil {
			t.Fatal(e)
		}
		for _, hit := range result.Page.Hits {
			if seen[hit.MessageID] {
				t.Fatal("cursor repeated an already returned source")
			}
			seen[hit.MessageID] = true
		}
		request.Cursor = result.Page.NextCursor
		if request.Cursor == "" {
			break
		}
	}
	if len(seen) != 7 {
		t.Fatalf("cursor lost sources: got %d", len(seen))
	}
}
func TestCursorRejectsDifferentPrincipalQueryAndChangedArchive(t *testing.T) {
	f := newFixture(t)
	f.add(t, "one", "chat", "keyword body")
	f.add(t, "two", "chat", "keyword body")
	s := service(t, f.config(archive.IndexGeneration{}))
	first, e := s.SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword", Limit: 1})
	if e != nil || first.Page.NextCursor == "" {
		t.Fatalf("initial cursor %v", e)
	}
	request := archive.SearchRequest{Query: "keyword", Limit: 1, Cursor: first.Page.NextCursor}
	for _, scenario := range []string{"principal", "query", "tamper", "archive"} {
		t.Run(scenario, func(t *testing.T) {
			scope := f.scope
			r := request
			switch scenario {
			case "principal":
				scope.Principal = "another-principal"
			case "query":
				r.Query = "body"
			case "tamper":
				r.Cursor = "x" + r.Cursor
			case "archive":
				f.add(t, "new", "chat", "keyword insertion")
			}
			if _, e := s.SearchDetails(context.Background(), scope, r); !errors.Is(e, archive.ErrCursorStale) {
				t.Fatalf("invalid cursor accepted: %v", e)
			}
		})
	}
}
func TestPrivateGuardAndCandidateMetadataNeverSerialize(t *testing.T) {
	f := newFixture(t)
	f.add(t, "json", "chat", "keyword body")
	result, e := service(t, f.config(archive.IndexGeneration{})).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := json.Marshal(result)
	if e != nil {
		t.Fatal(e)
	}
	for _, private := range []string{"ContentHash", "ScopeIdentity", "ReadSources", "Sources\": [{\"Account", "Guard", "Documents"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private result field serialized: %s", private)
		}
	}
}

func TestCursorClockTickStableButAccessLossStales(t *testing.T) {
	f := newFixture(t)
	f.add(t, "one", "chat", "keyword body")
	f.add(t, "two", "chat", "keyword body")
	scope := f.scope
	scope.Rolling = time.Minute + 10*time.Second
	s := service(t, f.config(archive.IndexGeneration{}))
	request := archive.SearchRequest{Query: "keyword", Limit: 1}
	first, err := s.SearchDetails(context.Background(), scope, request)
	if err != nil || first.Page.NextCursor == "" {
		t.Fatalf("first page %v", err)
	}
	request.Cursor = first.Page.NextCursor
	f.clock.Add(1)
	if next, err := s.SearchDetails(context.Background(), scope, request); err != nil || len(next.Page.Hits) != 1 || next.Page.Hits[0].MessageID == first.Page.Hits[0].MessageID {
		t.Fatalf("clock tick staled current cursor: %#v %v", next, err)
	}
	f.clock.Add(10000)
	if _, err = s.SearchDetails(context.Background(), scope, request); !errors.Is(err, archive.ErrCursorStale) {
		t.Fatalf("expired ranking kept cursor: %v", err)
	}
}

func TestCursorIndexActivationDuringHydrationRejectsInsteadOfFallback(t *testing.T) {
	f := newFixture(t)
	f.add(t, "one", "chat", "keyword body")
	f.add(t, "two", "chat", "keyword body")
	g := f.index(t, nil)
	c := f.config(g)
	reader := &interleavedReader{CandidateReader: f.store}
	c.Candidates = reader
	s := service(t, c)
	request := archive.SearchRequest{Query: "keyword", Limit: 1}
	first, err := s.SearchDetails(context.Background(), f.scope, request)
	if err != nil || first.Page.NextCursor == "" {
		t.Fatalf("first hybrid cursor %v", err)
	}
	request.Cursor = first.Page.NextCursor
	reader.beforeHydrate = func() {
		replacement := g.Version
		replacement.ID = "cursor-replacement-v1"
		replacement.EncoderProfile = "replacement-profile"
		f.indexVersion(t, replacement, nil)
	}
	result, err := s.SearchDetails(context.Background(), f.scope, request)
	if !errors.Is(err, archive.ErrCursorStale) || len(result.Page.Hits) != 0 {
		t.Fatalf("cursor silently changed ranking mode %#v %v", result, err)
	}
}

func TestCursorCandidateSourceRaceIsStale(t *testing.T) {
	f := newFixture(t)
	f.add(t, "one", "chat", "keyword body")
	f.add(t, "two", "chat", "keyword body")
	c := f.config(archive.IndexGeneration{})
	reader := &interleavedReader{CandidateReader: f.store}
	c.Candidates = reader
	s := service(t, c)
	request := archive.SearchRequest{Query: "keyword", Limit: 1}
	first, err := s.SearchDetails(context.Background(), f.scope, request)
	if err != nil || first.Page.NextCursor == "" {
		t.Fatalf("first cursor %v", err)
	}
	request.Cursor = first.Page.NextCursor
	reader.beforePair = func() error { return archive.ErrSourceUnavailable }
	result, err := s.SearchDetails(context.Background(), f.scope, request)
	if !errors.Is(err, archive.ErrCursorStale) || len(result.Page.Hits) != 0 {
		t.Fatalf("cursor race did not normalize %#v %v", result, err)
	}
}
