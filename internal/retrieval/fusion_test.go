package retrieval

import (
	"math"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

func ref(id string, kind archive.DocumentKind, members ...string) archive.DocumentRef {
	r := archive.DocumentRef{ID: archive.DocumentID(id), Kind: kind}
	for _, member := range members {
		source := archive.SourceRef{Account: "a", Version: archive.SourceVersion{MessageID: archive.MessageID(member), Revision: 1, PolicyGeneration: 1}, ContentHash: "hash"}
		r.Sources = append(r.Sources, source)
		r.ReadSources = append(r.ReadSources, archive.SourceReadVersion{Ref: source, ChatID: "chat", SenderID: "sender", OriginalSentAt: time.Unix(1, 0)})
	}
	return r
}
func TestReciprocalRankFusionIndependentBranchesAndDeterminism(t *testing.T) {
	a, b, c := ref("a", archive.DocumentOCR, "one"), ref("b", archive.DocumentOCR, "two"), ref("c", archive.DocumentOCR, "three")
	got, e := fuse([]archive.Candidate{{Ref: a}, {Ref: b}}, []archive.Candidate{{Ref: b}, {Ref: c}})
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 3 || got[0].ref.ID != "b" || got[1].ref.ID != "a" || got[2].ref.ID != "c" {
		t.Fatalf("fusion order %#v", got)
	}
	if math.Abs(got[0].score-(1./62+1./61)) > 1e-12 {
		t.Fatalf("RRF score %v", got[0].score)
	}
}
func TestDistinctWindowsKeepFragmentsAndDerivedLabels(t *testing.T) {
	a, b, duplicate, ocr := ref("a", archive.DocumentWindow, "one", "two"), ref("b", archive.DocumentWindow, "two", "three"), ref("duplicate", archive.DocumentWindow, "one"), ref("ocr", archive.DocumentOCR, "one")
	got, e := fuse([]archive.Candidate{{Ref: a}, {Ref: b}, {Ref: duplicate}, {Ref: ocr}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 4 || got[0].ref.ID != "a" || got[1].ref.ID != "b" || got[2].ref.ID != "duplicate" || got[3].ref.ID != "ocr" {
		t.Fatalf("source-preserving dedup %#v", got)
	}
}
func TestOriginalKeywordMapsToAuthorizedWindowIdentity(t *testing.T) {
	window := ref("window", archive.DocumentWindow, "one", "two")
	message := ref("", archive.DocumentWindow, "one")
	message.MessageID = "one"
	got, e := fuse([]archive.Candidate{{Ref: message}, {Ref: window}}, []archive.Candidate{{Ref: window}})
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 1 || got[0].ref.ID != "window" || len(got[0].ref.Sources) != 2 || math.Abs(got[0].score-2./61) > 1e-12 {
		t.Fatalf("comparable identity %#v", got)
	}
}

func TestFusionTieUsesStableOpaqueIdentity(t *testing.T) {
	a, b := ref("a", archive.DocumentOCR, "one"), ref("b", archive.DocumentOCR, "two")
	got, err := fuse([]archive.Candidate{{Ref: b}}, []archive.Candidate{{Ref: a}})
	if err != nil || len(got) != 2 || got[0].ref.ID != "a" || got[1].ref.ID != "b" || got[0].score != got[1].score {
		t.Fatalf("unstable RRF tie: %#v %v", got, err)
	}
}

func TestLexicalAnchorDoesNotMapIntoUnmatchedSemanticFragment(t *testing.T) {
	fragment := ref("fragment", archive.DocumentWindow, "one")
	original := ref("", archive.DocumentWindow, "one")
	original.MessageID = "one"
	got, err := fuse([]archive.Candidate{{Ref: original}}, []archive.Candidate{{Ref: fragment}})
	if err != nil || len(got) != 2 {
		t.Fatalf("lexical source collapsed into unrelated fragment %#v %v", got, err)
	}
	seen := map[string]bool{}
	for _, hit := range got {
		seen[hit.key] = true
	}
	if !seen["message:one"] || !seen["document:fragment"] {
		t.Fatalf("fragment identities lost %#v", got)
	}
}
