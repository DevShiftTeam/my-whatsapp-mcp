//go:build sqlite_fts5

package retrieval

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestSyntheticKeywordNormalizationPreservesOriginal(t *testing.T) {
	body, err := os.ReadFile("../../testdata/retrieval/scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			ID, Body, Query string
			Expected        string `json:"expected_original"`
		}
	}
	if err = json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, entry := range fixture.Cases {
		t.Run(entry.ID, func(t *testing.T) {
			f := newFixture(t)
			id := f.add(t, entry.ID, "chat", entry.Body)
			result, err := service(t, f.config(archive.IndexGeneration{})).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: entry.Query})
			if err != nil || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id || result.Page.Hits[0].Snippet != entry.Expected {
				t.Fatalf("original logical text lost: %#v %v", result.Page.Hits, err)
			}
		})
	}
}
