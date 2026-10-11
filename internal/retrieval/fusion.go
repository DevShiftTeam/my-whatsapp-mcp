package retrieval

import (
	"reflect"
	"sort"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

type rankedCandidate struct {
	ref   archive.DocumentRef
	score float64
	key   string
}

func sameRef(a, b archive.DocumentRef) bool { return reflect.DeepEqual(a, b) }
func candidateKey(ref archive.DocumentRef) string {
	if ref.ID != "" {
		return "document:" + string(ref.ID)
	}
	if ref.MessageID != "" {
		return "message:" + string(ref.MessageID)
	}
	return ""
}

// Original lexical hits share rank identity only with an allowed indexed
// window known to match the lexical query. A semantic-only fragment may refer
// to a different span of the same message and cannot replace its lexical hit.
func comparable(ref archive.DocumentRef, windows []archive.DocumentRef) archive.DocumentRef {
	if ref.ID != "" || ref.Kind != archive.DocumentWindow || ref.MessageID == "" {
		return ref
	}
	var best *archive.DocumentRef
	for i := range windows {
		window := &windows[i]
		found := false
		for _, source := range window.Sources {
			if source.Version.MessageID == ref.MessageID {
				for _, original := range ref.Sources {
					if source == original {
						found = true
					}
				}
			}
		}
		if found && (best == nil || len(window.Sources) < len(best.Sources) || (len(window.Sources) == len(best.Sources) && window.ID < best.ID)) {
			best = window
		}
	}
	if best != nil {
		return *best
	}
	return ref
}
func fuse(keyword, semantic []archive.Candidate) ([]rankedCandidate, error) {
	if len(keyword) > 100 || len(semantic) > 100 {
		return nil, archive.ErrLimitExceeded
	}
	var windows []archive.DocumentRef
	for _, candidate := range keyword {
		if candidate.Ref.ID != "" && candidate.Ref.Kind == archive.DocumentWindow {
			windows = append(windows, candidate.Ref)
		}
	}
	byID := map[string]*rankedCandidate{}
	for _, branch := range [][]archive.Candidate{keyword, semantic} {
		seen := map[string]bool{}
		for position, candidate := range branch {
			ref := comparable(candidate.Ref, windows)
			key := candidateKey(ref)
			if key == "" || len(ref.Sources) == 0 {
				return nil, archive.ErrSourceUnavailable
			}
			current, exists := byID[key]
			if exists && !sameRef(current.ref, ref) {
				return nil, archive.ErrSourceUnavailable
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			if !exists {
				current = &rankedCandidate{ref: ref, key: key}
				byID[key] = current
			}
			current.score += 1. / float64(60+position+1)
		}
	}
	ranked := make([]rankedCandidate, 0, len(byID))
	for _, candidate := range byID {
		ranked = append(ranked, *candidate)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].key < ranked[j].key
	})
	// Source membership is not a span-coverage proof. Distinct window IDs may
	// hold separate fragments of one long source, so only exact document
	// identities above are deduplicated; all individual memberships survive.
	return ranked, nil
}
