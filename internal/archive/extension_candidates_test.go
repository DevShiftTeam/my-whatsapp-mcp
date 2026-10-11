//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

// These fixtures use real SQLite tables, including sqlite-vec distances and
// FTS. A fake reader cannot establish that authorization precedes top-k.
func candidateIndex(t *testing.T, s *Store) IndexGeneration {
	t.Helper()
	v := IndexVersion{ID: "candidate-v1", EncoderProfile: "synthetic", TokenizerSHA256: strings.Repeat("0", 64), TokenizerImplementation: "fixture", PrefixVersion: "1", ChunkingVersion: "1", NormalizationVersion: LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 256}
	b, _ := json.Marshal(v)
	if _, err := s.writeDB.Exec(`INSERT INTO extension_generations(id,version_json,state,created_at) VALUES('candidate-index',?,'active',1)`, string(b)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`UPDATE archive_meta SET index_generation=index_generation+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	return IndexGeneration{ID: "candidate-index", Version: v, State: "active"}
}

// candidateRoot models a bounded, verified legacy promotion. It creates no
// encoder/extraction version and uses the typed original publication path for
// the association's verified SHA marker.
func candidateRoot(t *testing.T, s *Store, p ArchivePolicy, id, text string, sources []MessageID, provenance DerivedProvenance) DerivedProvenance {
	t.Helper()
	if len(sources) == 0 {
		t.Fatal("root fixture needs members")
	}
	if provenance.Kind == "" {
		provenance.Kind = DocumentOCR
	}
	if provenance.ProfileID == "" {
		provenance.ProfileID = "fixture-ocr"
	}
	if provenance.ExtractorVersion == "" {
		provenance.ExtractorVersion = "1"
	}
	if provenance.OffsetKind == "" {
		provenance.OffsetKind = "none"
	}
	var source SourceRef
	var expiry int64
	if err := s.writeDB.QueryRow(`SELECT account,id,local_revision,content_hash,expires_at FROM messages WHERE id=?`, sources[0]).Scan(&source.Account, &source.Version.MessageID, &source.Version.Revision, &source.ContentHash, &expiry); err != nil {
		t.Fatal(err)
	}
	source.Version.PolicyGeneration, source.Version.ExpiresAt = p.Generation, fromMillis(expiry)
	kind, format := "image", "png"
	if provenance.Kind == DocumentTranscript {
		kind, format = "voice", "wav"
	}
	var media MediaID
	var marker string
	err := s.writeDB.QueryRow(`SELECT id,last_original_sha256 FROM extension_media_associations WHERE account=? AND message_id=? AND local_revision=? AND kind=? AND active=1 AND view_once=0 ORDER BY ordinal LIMIT 1`, source.Account, source.Version.MessageID, source.Version.Revision, kind).Scan(&media, &marker)
	if errors.Is(err, sql.ErrNoRows) {
		encoded, _ := json.Marshal(struct {
			Source SourceRef
			Kind   string
		}{source, kind})
		media = MediaID("candidate-media-" + extensionExtractionSHA(encoded))
		_, err = s.writeDB.Exec(`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,ordinal,kind,protocol_reference,mime_type,declared_bytes,view_once,availability,active) VALUES(?,?,?,?,?,?,?,(SELECT COALESCE(max(ordinal),-1)+1 FROM extension_media_associations WHERE message_id=? AND local_revision=?),?,?,'',64,0,'unavailable',1)`, media, source.Account, source.Version.MessageID, source.Version.Revision, p.Generation, source.ContentHash, expiry, source.Version.MessageID, source.Version.Revision, kind, []byte("synthetic-protocol-reference"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if marker == "" {
		payload := []byte("candidate synthetic original " + kind)
		reservation, err := s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: source, MediaID: media, Budget: BudgetOriginals, MaximumBytes: int64(len(payload) + 32), TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if accepted, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: reservation, SHA256: extensionExtractionSHA(payload), Bytes: int64(len(payload)), ActualFormat: format}); err != nil || !accepted {
			t.Fatalf("fixture original publication %v %v", accepted, err)
		}
	}
	provenance.MediaID = media
	encoded, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_roots(id,legacy_document_id,kind,text,text_sha256,text_bytes,media_id,provenance_json,source_count,created_at) VALUES(?,?,?,?,?,?,?,?,?,1)`, id, id, provenance.Kind, text, extensionExtractionSHA([]byte(text)), len(text), media, string(encoded), len(sources)); err != nil {
		t.Fatal(err)
	}
	for ordinal, idSource := range sources {
		if _, err = s.writeDB.Exec(`INSERT INTO extension_root_sources(root_id,account,message_id,local_revision,policy_generation,content_hash,expires_at,ordinal) SELECT ?,account,id,local_revision,?,content_hash,expires_at,? FROM messages WHERE id=?`, id, p.Generation, ordinal, idSource); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_root_fts(rowid,root_id,text) SELECT rowid,id,? FROM extension_roots WHERE id=?`, NormalizeLexical(text), id); err != nil {
		t.Fatal(err)
	}
	return provenance
}

func candidateDocument(t *testing.T, s *Store, p ArchivePolicy, generation IndexGeneration, id string, text string, sources []MessageID, vector []float32, originIDs ...DocumentID) {
	t.Helper()
	origin := DocumentID(id)
	if len(originIDs) == 1 {
		origin = originIDs[0]
	} else if len(originIDs) > 1 {
		t.Fatal("semantic fixture accepts at most one origin ID")
	}
	var kind DocumentKind
	var provenance, rootText string
	err := s.writeDB.QueryRow(`SELECT kind,provenance_json,text FROM extension_roots WHERE id=?`, origin).Scan(&kind, &provenance, &rootText)
	if errors.Is(err, sql.ErrNoRows) {
		rootProvenance := candidateRoot(t, s, p, string(origin), text, sources, DerivedProvenance{Kind: DocumentOCR})
		encoded, _ := json.Marshal(rootProvenance)
		kind, provenance, rootText = rootProvenance.Kind, string(encoded), text
	} else if err != nil {
		t.Fatal(err)
	}
	if rootText != text {
		t.Fatal("semantic fixture differs from its immutable root")
	}
	if _, err := s.writeDB.Exec(`INSERT INTO extension_documents(id,generation,kind,text,normalized_text,provenance_json,spans_json,passage_tokens,content_hash,source_count,origin_document_id,origin_end_byte) VALUES(?,?,?,?,?,?,'[]',4,?,?,?,?)`, id, generation.ID, kind, text, NormalizeLexical(text), provenance, extensionExtractionSHA([]byte(text)), len(sources), origin, len(text)); err != nil {
		t.Fatal(err)
	}
	for ordinal, source := range sources {
		if _, err := s.writeDB.Exec(`INSERT INTO extension_document_sources(document_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal) SELECT ?,'account',id,local_revision,?,expires_at,content_hash,? FROM messages WHERE id=?`, id, p.Generation, ordinal, source); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.writeDB.Exec(`INSERT INTO extension_document_fts(rowid,normalized_text) SELECT rowid,normalized_text FROM extension_documents WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if vector != nil {
		blob := make([]byte, 4*len(vector))
		for i, value := range vector {
			binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(value))
		}
		if _, err := s.writeDB.Exec(`INSERT INTO extension_vectors(document_id,generation,dimensions,values_blob) VALUES(?,?,?,?)`, id, generation.ID, len(vector), blob); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCandidateIndependentExtractionVisibleWithoutEncoder(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "original unrelated text", now.Add(-time.Minute)))
	// Prepare a verified original without creating an index, then publish the
	// actual keyword authority through the independent extraction port.
	candidateRoot(t, s, p, "setup", "setup unrelated text", []MessageID{id}, DerivedProvenance{})
	version := ExtractionVersion{ID: "keyword-ocr", Kind: JobOCR, ProfileID: "synthetic-ocr", ProfileIdentity: strings.Repeat("a", 64), RuntimeSHA256: strings.Repeat("b", 64), ExtractorVersion: "1", PromptVersion: "none-v1", Language: "auto"}
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: version, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{ID: "independent-root", Kind: DocumentOCR, Text: "canary extracted without encoder", Sources: []SourceRef{work.Source}, Provenance: DerivedProvenance{Kind: DocumentOCR, MediaID: work.MediaID, ProfileID: version.ProfileID, ExtractorVersion: version.ExtractorVersion, PromptVersion: version.PromptVersion, OffsetKind: "none"}}
	if accepted, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{doc}}); err != nil || !accepted {
		t.Fatalf("independent extraction publication %v %v", accepted, err)
	}
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "canary"}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 1 || len(pair.Semantic) != 0 || !pair.Keyword[0].Ref.rootReference {
		t.Fatalf("no-index keyword root %#v %v", pair, err)
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
	if err != nil || len(page.Documents) != 1 || page.Documents[0].Excerpts[0].Text != doc.Text || page.Guard.Generation != "" || len(page.Guard.roots) != 1 {
		t.Fatalf("independent root hydration %#v %v", page, err)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateIndependentRootSurvivesRetiredEncoderAndCacheEviction(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unrelated original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "retired-root", "canary durable extraction", []MessageID{id}, []float32{1, 0})
	if _, err := s.writeDB.Exec(`UPDATE extension_generations SET state='retired' WHERE id=?`, generation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`UPDATE extension_media_artifacts SET state='evicting',cleanup_ack=1 WHERE budget='originals'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`UPDATE extension_media_associations SET availability='unavailable',artifact_id=''`); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "canary"}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 1 || len(pair.Semantic) != 0 || pair.Keyword[0].Ref.ID != "retired-root" || !pair.Keyword[0].Ref.rootReference {
		t.Fatalf("encoder retirement or cache eviction hid keyword authority %#v %v", pair, err)
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
	if err != nil || len(page.Documents) != 1 || page.Documents[0].Excerpts[0].Text != "canary durable extraction" {
		t.Fatalf("evicted original root hydration %#v %v", page, err)
	}
}

func TestCandidateRootDenialsPrecedeKeywordTopK(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	allowed := readerApply(t, s, p, readerEvent("allowed", "allowed", "original unrelated", now.Add(-time.Minute)))
	denied := readerApply(t, s, p, readerEvent("denied", "denied", "original unrelated", now.Add(-time.Minute)))
	for _, id := range []string{"a-denied", "b-denied", "c-denied"} {
		candidateRoot(t, s, p, id, "canary", []MessageID{denied}, DerivedProvenance{})
	}
	candidateRoot(t, s, p, "d-mixed", "canary", []MessageID{allowed, denied}, DerivedProvenance{})
	candidateRoot(t, s, p, "z-allowed", "canary permitted longer text", []MessageID{allowed}, DerivedProvenance{})
	scope := readerScope(p, readerChat(t, s, allowed))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "canary", Limit: 1}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 1 || pair.Keyword[0].Ref.ID != "z-allowed" {
		t.Fatalf("denied roots consumed top-k or lost contributors %#v %v", pair, err)
	}
}

func TestCandidateSelectedCorruptRootFailsWholeRead(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unrelated original", now.Add(-time.Minute)))
	candidateRoot(t, s, p, "a-corrupt", "canary", []MessageID{id}, DerivedProvenance{})
	candidateRoot(t, s, p, "z-valid", "canary lower ranked truthful extraction", []MessageID{id}, DerivedProvenance{})
	if _, err := s.writeDB.Exec(`UPDATE extension_roots SET provenance_json='{' WHERE id='a-corrupt'`); err != nil {
		t.Fatal(err)
	}
	// The selected authority is corrupt. Fail the complete read instead of
	// silently dropping the selected root after LIMIT and returning partial data.
	pair, err := s.CandidatePair(context.Background(), readerScope(p, readerChat(t, s, id)), CandidatePairRequest{Search: SearchRequest{Query: "canary", Limit: 1}, KeywordOnly: true})
	if !errors.Is(err, ErrSourceUnavailable) || len(pair.Keyword) != 0 {
		t.Fatalf("selected corrupt root did not fail closed %#v %v", pair, err)
	}
}

func TestCandidateRootResponsePinsRejectOnlyContributingAuthorityChanges(t *testing.T) {
	for _, scenario := range []string{"unrelated_append", "delete", "provenance", "media_revoke", "media_sha", "media_kind", "contributor_edit", "expiry", "rolling"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("source", "chat", "original unrelated", now.Add(-time.Hour+time.Second))
			if scenario == "expiry" {
				e.ExpiresAt = now.Add(time.Second)
			}
			id := readerApply(t, s, p, e)
			provenance := candidateRoot(t, s, p, "root", "canary contributing text", []MessageID{id}, DerivedProvenance{})
			scope := readerScope(p, readerChat(t, s, id))
			if scenario == "rolling" {
				scope.Rolling = time.Hour
			}
			pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "canary"}, KeywordOnly: true})
			if err != nil || len(pair.Keyword) != 1 {
				t.Fatalf("root candidate %#v %v", pair, err)
			}
			ref := pair.Keyword[0].Ref
			page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{ref}, Snapshot: pair.Snapshot})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unrelated_append":
				candidateRoot(t, s, p, "other-root", "canary unrelated extraction", []MessageID{id}, provenance)
				_, err = s.writeDB.Exec(`UPDATE archive_meta SET archive_revision=archive_revision+1 WHERE id=1`)
			case "delete":
				_, err = s.writeDB.Exec(`DELETE FROM extension_roots WHERE id='root'`)
			case "provenance":
				provenance.ProfileID = "different-profile"
				encoded, _ := json.Marshal(provenance)
				_, err = s.writeDB.Exec(`UPDATE extension_roots SET provenance_json=? WHERE id='root'`, string(encoded))
			case "media_revoke":
				_, err = s.writeDB.Exec(`UPDATE extension_media_associations SET active=0 WHERE id=?`, provenance.MediaID)
			case "media_sha":
				_, err = s.writeDB.Exec(`UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?`, extensionExtractionSHA([]byte("changed actual original")), provenance.MediaID)
			case "media_kind":
				// Model a corrupt persisted kind despite the schema constraint:
				// final admission must match the exact image/voice allowlist too.
				conn, openErr := s.writeDB.Conn(context.Background())
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = conn.ExecContext(context.Background(), `PRAGMA ignore_check_constraints=ON`)
				if err == nil {
					_, err = conn.ExecContext(context.Background(), `UPDATE extension_media_associations SET kind='corrupt-kind' WHERE id=?`, provenance.MediaID)
				}
				_, resetErr := conn.ExecContext(context.Background(), `PRAGMA ignore_check_constraints=OFF`)
				conn.Close()
				if err == nil {
					err = resetErr
				}
			case "contributor_edit":
				e.Kind, e.SourceRevision, e.Body = EventEditContent, 2, "changed original"
				readerApply(t, s, p, e)
			case "expiry", "rolling":
				now = now.Add(2 * time.Second)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard})
			if scenario == "unrelated_append" {
				if err != nil {
					t.Fatalf("unrelated root append discarded a current response: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("accepted %s root authority change: %v", scenario, err)
			}
			if result, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{ref}, Snapshot: pair.Snapshot}); !errors.Is(err, ErrSourceUnavailable) || len(result.Documents) != 0 {
				t.Fatalf("rehydrated changed %s root %#v %v", scenario, result, err)
			}
		})
	}
}

func TestCandidateRootMembershipCopiesHaveOneGlobalBound(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var members []MessageID
	for i := 0; i < 33; i++ {
		members = append(members, readerApply(t, s, p, readerEvent(fmt.Sprintf("member-%d", i), "chat", "original unrelated", now.Add(-time.Minute))))
	}
	// Only 33 unique sources, but 63 roots contain 2079 membership copies.
	// A deduplicated frame/guard map must not authorize all these allocations.
	for i := 0; i < 63; i++ {
		candidateRoot(t, s, p, fmt.Sprintf("root-%03d", i), "canary repeated membership", members, DerivedProvenance{})
	}
	scope := readerScope(p, readerChat(t, s, members[0]))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "canary", Limit: 100}, KeywordOnly: true})
	if !errors.Is(err, ErrLimitExceeded) || len(pair.Keyword) != 0 {
		t.Fatalf("repeated membership bypassed global candidate bound %#v %v", pair, err)
	}
	// The hydration input preflight has the same logical bound before any
	// current-source query, even when each remembered source is repeated.
	refs := make([]DocumentRef, 63)
	for i := range refs {
		refs[i].Sources = make([]SourceRef, 33)
		refs[i].ReadSources = make([]SourceReadVersion, 33)
	}
	if page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: refs}); !errors.Is(err, ErrLimitExceeded) || len(page.Documents) != 0 {
		t.Fatalf("repeated hydration members bypassed global bound %#v %v", page, err)
	}
}

func TestCandidateHydrationPreservesMaterializedMemberLimitError(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	generation := candidateIndex(t, s)
	var members []MessageID
	for i := 0; i < 33; i++ {
		members = append(members, readerApply(t, s, p, readerEvent(fmt.Sprintf("member-%d", i), "chat", "original unrelated", now.Add(-time.Minute))))
	}
	scope := readerScope(p, readerChat(t, s, members[0]))
	var refs []DocumentRef
	var snapshot ReadSnapshot
	for i := 0; i < 32; i++ {
		id := DocumentID(fmt.Sprintf("fragment-%03d", i))
		candidateDocument(t, s, p, generation, string(id), "generated text", members, nil, DocumentID("origin-"+string(id)))
		f, err := s.beginRead(context.Background(), scope, nil, time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := f.extensionDocumentRef(id, DocumentOCR)
		snapshot = f.snapshot
		f.close()
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	// Input copies total1056, below2048. Revalidating both fragments and full
	// roots would materialize2112, so the deeper quota must stay limit_exceeded.
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: refs, Snapshot: snapshot})
	if !errors.Is(err, ErrLimitExceeded) || len(page.Documents) != 0 {
		t.Fatalf("materialized membership limit became unavailable/partial %#v %v", page, err)
	}
}

func TestCandidateSemanticFragmentKeepsAuthoritativeRootAnchorOrder(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	older := readerApply(t, s, p, readerEvent("older", "older-chat", "unrelated older original", now.Add(-time.Hour)))
	anchor := readerApply(t, s, p, readerEvent("anchor", "anchor-chat", "unrelated media original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "ordered-fragment", "canary rooted text", []MessageID{anchor, older}, []float32{1, 0}, "ordered-origin")
	scope := readerScope(p, readerChat(t, s, anchor))
	scope.AllChats, scope.Chats = true, nil
	request := CandidatePairRequest{Search: SearchRequest{Query: "canary"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}}
	pair, err := s.CandidatePair(context.Background(), scope, request)
	if err != nil || len(pair.Semantic) != 1 || pair.Semantic[0].Ref.ReadSources[0].Ref.Version.MessageID != anchor {
		t.Fatalf("initial rooted anchor %#v %v", pair, err)
	}
	ref := pair.Semantic[0].Ref
	if _, err = s.writeDB.Exec(`UPDATE extension_document_sources SET ordinal=1-ordinal WHERE document_id='ordered-fragment'`); err != nil {
		t.Fatal(err)
	}
	// Every source is still authorized and the immutable root is unchanged.
	// The malformed fragment anchor must be excluded before vector top-k.
	current, err := s.CandidatePair(context.Background(), scope, request)
	if err != nil || len(current.Semantic) != 0 || len(current.Keyword) != 1 || !current.Keyword[0].Ref.rootReference {
		t.Fatalf("fragment order changed authoritative anchor %#v %v", current, err)
	}
	if page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{ref}, Snapshot: pair.Snapshot}); !errors.Is(err, ErrSourceUnavailable) || len(page.Documents) != 0 {
		t.Fatalf("post-candidate ordinal change was rebound %#v %v", page, err)
	}
}

func TestCandidateSemanticRootRangesAreGatedBeforeTopK(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unrelated original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	for _, name := range []string{"bad-range-a", "bad-range-b", "bad-range-c"} {
		candidateDocument(t, s, p, generation, name, "invalid semantic range", []MessageID{id}, []float32{1, 0})
		if _, err := s.writeDB.Exec(`UPDATE extension_documents SET origin_end_byte=20000 WHERE id=?`, name); err != nil {
			t.Fatal(err)
		}
	}
	candidateDocument(t, s, p, generation, "valid-range", "authorized semantic range", []MessageID{id}, []float32{0, 1})
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "no-overlap", Limit: 1}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Semantic) != 1 || pair.Semantic[0].Ref.ID != "valid-range" {
		t.Fatalf("invalid root range consumed semantic top-k %#v %v", pair, err)
	}
}

func TestCandidateWholeLegacyBranchesHaveOneExactRootReference(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unrelated original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "legacy-root", "canary verified legacy text", []MessageID{id}, []float32{1, 0})
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "canary"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Keyword) != 1 || len(pair.Semantic) != 1 {
		t.Fatalf("legacy hybrid pair %#v %v", pair, err)
	}
	keyword, semantic := pair.Keyword[0].Ref, pair.Semantic[0].Ref
	if keyword.ID != "legacy-root" || semantic.ID != keyword.ID || !keyword.rootReference || !semantic.rootReference || !reflect.DeepEqual(keyword, semantic) {
		t.Fatalf("one legacy identity became conflicting refs keyword=%#v semantic=%#v", keyword, semantic)
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{semantic}, Snapshot: pair.Snapshot})
	if err != nil || len(page.Documents) != 1 || page.Documents[0].Excerpts[0].Text != "canary verified legacy text" {
		t.Fatalf("canonical legacy hydration %#v %v", page, err)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateLegacyCanonicalizationRequiresCompleteAuthorityProof(t *testing.T) {
	for _, scenario := range []string{"partial_range", "text", "provenance", "source_order", "generation", "sha"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			anchor := readerApply(t, s, p, readerEvent("anchor", "chat", "unrelated media original", now.Add(-time.Minute)))
			other := readerApply(t, s, p, readerEvent("other", "chat", "unrelated contributor", now.Add(-2*time.Minute)))
			generation := candidateIndex(t, s)
			candidateDocument(t, s, p, generation, "legacy-root", "canary verified legacy text", []MessageID{anchor, other}, []float32{1, 0})
			var err error
			switch scenario {
			case "partial_range":
				_, err = s.writeDB.Exec(`UPDATE extension_documents SET origin_end_byte=origin_end_byte-1 WHERE id='legacy-root'`)
			case "text":
				_, err = s.writeDB.Exec(`UPDATE extension_documents SET text='different stored text' WHERE id='legacy-root'`)
			case "provenance":
				_, err = s.writeDB.Exec(`UPDATE extension_documents SET provenance_json='{}' WHERE id='legacy-root'`)
			case "source_order":
				_, err = s.writeDB.Exec(`UPDATE extension_document_sources SET ordinal=1-ordinal WHERE document_id='legacy-root'`)
			case "generation":
				_, err = s.writeDB.Exec(`UPDATE extension_generations SET state='retired' WHERE id=?`, generation.ID)
				if err == nil {
					generation.ID = "replacement-index"
					encoded, _ := json.Marshal(generation.Version)
					_, err = s.writeDB.Exec(`INSERT INTO extension_generations(id,version_json,state,created_at) VALUES(?,?,'active',2)`, generation.ID, string(encoded))
				}
			case "sha":
				_, err = s.writeDB.Exec(`UPDATE extension_roots SET text_sha256=? WHERE id='legacy-root'`, extensionExtractionSHA([]byte("different real root text")))
			}
			if err != nil {
				t.Fatal(err)
			}
			scope := readerScope(p, readerChat(t, s, anchor))
			pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "canary"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
			if scenario == "sha" {
				if !errors.Is(err, ErrSourceUnavailable) || len(pair.Keyword)+len(pair.Semantic) != 0 {
					t.Fatalf("unverified root SHA became a canonical identity %#v %v", pair, err)
				}
				return
			}
			if err != nil || len(pair.Semantic) != 0 || len(pair.Keyword) != 1 || !pair.Keyword[0].Ref.rootReference {
				t.Fatalf("%s false legacy canonicalization %#v %v", scenario, pair, err)
			}
		})
	}
}

func TestCandidateDistinctWholeFragmentsKeepTheirOwnReference(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unrelated original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "fragment-id", "canary whole origin text", []MessageID{id}, []float32{1, 0}, "origin-id")
	pair, err := s.CandidatePair(context.Background(), readerScope(p, readerChat(t, s, id)), CandidatePairRequest{Search: SearchRequest{Query: "canary"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Keyword) != 1 || len(pair.Semantic) != 1 || pair.Keyword[0].Ref.ID != "origin-id" || pair.Semantic[0].Ref.ID != "fragment-id" || pair.Semantic[0].Ref.rootReference || pair.Semantic[0].Ref.root == nil || pair.Semantic[0].Ref.root.ID != "origin-id" {
		t.Fatalf("distinct whole fragment collapsed into root %#v %v", pair, err)
	}
}

func TestCandidateActiveIndexReturnsCurrentCompleteVersion(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, _ := readerFixture(t, &now)
	if absent, err := s.ActiveIndex(context.Background()); err != nil || absent != (IndexGeneration{}) {
		t.Fatalf("absent index %#v %v", absent, err)
	}
	active := candidateIndex(t, s)
	if got, err := s.ActiveIndex(context.Background()); err != nil || got != active {
		t.Fatalf("active version %#v %v, want %#v", got, err, active)
	}
	replacement := active
	replacement.ID = "replacement-index"
	replacement.Version.ID = "replacement-version"
	replacement.Version.EncoderProfile = "replacement-encoder"
	replacement.Version.TokenizerSHA256 = strings.Repeat("1", 64)
	replacement.Version.PrefixVersion = "2"
	replacement.Version.Dimensions = 3
	encoded, err := json.Marshal(replacement.Version)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.writeDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE extension_generations SET state='retired' WHERE id=?`, active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO extension_generations(id,version_json,state,created_at) VALUES(?,?,'active',2)`, replacement.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ActiveIndex(context.Background()); err != nil || got != replacement {
		t.Fatalf("replacement version %#v %v, want %#v", got, err, replacement)
	}
}

func TestCandidateResponseRechecksMetadataClockAndScope(t *testing.T) {
	for _, scenario := range []string{"canonical_chat", "sender", "date", "hash", "expiry", "rolling", "principal", "generation"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("edge", "chat", "keyword body", now.Add(-time.Hour+time.Millisecond))
			if scenario == "expiry" {
				e.ExpiresAt = now.Add(time.Millisecond)
			}
			id := readerApply(t, s, p, e)
			other := readerApply(t, s, p, readerEvent("other", "other", "other body", now.Add(-time.Minute)))
			generation := candidateIndex(t, s)
			scope := readerScope(p, readerChat(t, s, id))
			if scenario == "canonical_chat" {
				// Both canonical chats remain authorized: only the remembered
				// metadata detects this remap with an unchanged source revision.
				scope.AllChats, scope.Chats = true, nil
			}
			if scenario == "rolling" {
				scope.Rolling = time.Hour
			}
			pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
			if err != nil || len(pair.Keyword) != 1 {
				t.Fatalf("pair %#v %v", pair, err)
			}
			page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
			if err != nil {
				t.Fatal(err)
			}
			want := ErrSourceUnavailable
			switch scenario {
			case "canonical_chat":
				_, err = s.writeDB.Exec(`UPDATE messages SET chat_id=? WHERE id=?`, readerChat(t, s, other), id)
			case "sender":
				_, err = s.writeDB.Exec(`INSERT INTO identities(id,account,source_identity,name) VALUES('alternate-identity','account','alternate@s.whatsapp.net','')`)
				if err == nil {
					_, err = s.writeDB.Exec(`UPDATE messages SET sender_id='alternate-identity' WHERE id=?`, id)
				}
			case "date":
				_, err = s.writeDB.Exec(`UPDATE messages SET original_sent_at=original_sent_at-1 WHERE id=?`, id)
			case "hash":
				_, err = s.writeDB.Exec(`UPDATE messages SET content_hash='changed-hash' WHERE id=?`, id)
			case "expiry", "rolling":
				now = now.Add(2 * time.Millisecond)
			case "principal":
				scope.Principal = "different-principal"
				want = ErrUnauthorized
			case "generation":
				_, err = s.writeDB.Exec(`UPDATE extension_generations SET state='retired' WHERE id=?`, generation.ID)
				want = ErrCursorStale
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); !errors.Is(err, want) {
				t.Fatalf("response accepted changed %s: %v, want %v", scenario, err, want)
			}
		})
	}
}

func TestCandidateActiveIndexRejectsInvalidStoredVersion(t *testing.T) {
	for _, scenario := range []string{"oversized", "malformed", "tokenizer", "normalization", "passage_tokens"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, _ := readerFixture(t, &now)
			generation := candidateIndex(t, s)
			version := generation.Version
			var encoded []byte
			switch scenario {
			case "oversized":
				valid, err := json.Marshal(version)
				if err != nil {
					t.Fatal(err)
				}
				encoded = append(valid, []byte(strings.Repeat(" ", 4097-len(valid)))...)
			case "malformed":
				encoded = []byte(`{"ID":`)
			case "tokenizer":
				version.TokenizerSHA256 = strings.Repeat("z", 64)
			case "normalization":
				version.NormalizationVersion = "unsupported"
			case "passage_tokens":
				version.MaxPassageTokens = 257
			}
			if encoded == nil {
				var err error
				encoded, err = json.Marshal(version)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.writeDB.Exec(`UPDATE extension_generations SET version_json=? WHERE id=?`, string(encoded), generation.ID); err != nil {
				t.Fatal(err)
			}
			if got, err := s.ActiveIndex(context.Background()); !errors.Is(err, ErrArchiveUnavailable) || got != (IndexGeneration{}) {
				t.Fatalf("accepted invalid %s version: %#v %v", scenario, got, err)
			}
		})
	}
}

func TestCandidateHiddenEmbeddingInputsRemainPrivate(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "authorized original source", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "visible", "keyword visible text", []MessageID{id}, []float32{0, 1})
	// Forge both FTS and a nearer vector for the hidden input. The SQL gate,
	// rather than missing auxiliary rows, must keep this private material out.
	candidateDocument(t, s, p, generation, "hidden", "keyword private input", []MessageID{id}, []float32{1, 0}, "hidden-origin")
	scope := readerScope(p, readerChat(t, s, id))
	request := CandidatePairRequest{Search: SearchRequest{Query: "keyword", Limit: 1}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}}
	before, err := s.CandidatePair(context.Background(), scope, request)
	if err != nil || len(before.Semantic) != 1 || before.Semantic[0].Ref.ID != "hidden" {
		t.Fatalf("private fixture candidate %#v %v", before, err)
	}
	hiddenRef := before.Semantic[0].Ref
	// Its independent parent remains authoritative, but omit that public
	// root from this keyword fixture so the forged private FTS row is tested.
	if _, err = s.writeDB.Exec(`DELETE FROM extension_root_fts WHERE root_id='hidden-origin'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`UPDATE extension_documents SET embedding_input=1 WHERE id='hidden'`); err != nil {
		t.Fatal(err)
	}
	pair, err := s.CandidatePair(context.Background(), scope, request)
	if err != nil || len(pair.Keyword) != 1 || pair.Keyword[0].Ref.ID != "visible" || len(pair.Semantic) != 1 || pair.Semantic[0].Ref.ID != "visible" {
		t.Fatalf("hidden input entered public candidates: %#v %v", pair, err)
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{hiddenRef}, Snapshot: before.Snapshot})
	if !errors.Is(err, ErrSourceUnavailable) || len(page.Documents) != 0 {
		t.Fatalf("hidden input was hydrated: %#v %v", page, err)
	}
	f, err := s.beginRead(context.Background(), scope, nil, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	if ref, err := f.extensionDocumentRef("hidden", DocumentOCR); !errors.Is(err, ErrSourceUnavailable) || len(ref.ReadSources) != 0 {
		t.Fatalf("hidden input acquired public reference: %#v %v", ref, err)
	}
}

func TestCandidatePairScopesAllMembersBeforeSemanticTopK(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	allowed := readerApply(t, s, p, readerEvent("allowed", "allowed", "original keyword", now.Add(-time.Minute)))
	denied := readerApply(t, s, p, readerEvent("denied", "denied", "secret keyword", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	// Denied nearer vectors outnumber k. A global top-k followed by filtering
	// would return nothing, and an inner join would admit the mixed window.
	for _, id := range []string{"denied-1", "denied-2", "denied-3"} {
		candidateDocument(t, s, p, generation, id, "secret derived", []MessageID{denied}, []float32{1, 0})
	}
	candidateDocument(t, s, p, generation, "mixed", "mixed derived", []MessageID{allowed, denied}, []float32{1, 0})
	candidateDocument(t, s, p, generation, "allowed-doc", "semantic text with no lexical overlap", []MessageID{allowed}, []float32{0, 1})
	request := CandidatePairRequest{Search: SearchRequest{Query: "keyword", Limit: 1}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}}
	pair, err := s.CandidatePair(context.Background(), readerScope(p, readerChat(t, s, allowed)), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(pair.Semantic) != 1 || pair.Semantic[0].Ref.ID != "allowed-doc" {
		t.Fatalf("scoped semantic top-k: %#v", pair.Semantic)
	}
	if len(pair.Keyword) != 1 || pair.Keyword[0].Ref.MessageID != allowed {
		t.Fatalf("independent original keyword branch: %#v", pair.Keyword)
	}
}

func TestCandidateHydrationRevalidatesContributorsButAllowsUnrelatedInsert(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("allowed", "allowed", "original keyword", now.Add(-time.Minute))
	id := readerApply(t, s, p, e)
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "derived", "generated keyword", []MessageID{id}, []float32{1, 0})
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "generated"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Semantic) != 1 {
		t.Fatalf("pair %#v %v", pair, err)
	}
	readerApply(t, s, p, readerEvent("unrelated", "other", "unrelated insertion", now))
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Semantic[0].Ref}, Snapshot: pair.Snapshot})
	if err != nil || len(page.Documents) != 1 || page.Documents[0].Excerpts[0].Text != "generated keyword" || page.Documents[0].Excerpts[0].Provenance.Kind != DocumentOCR {
		t.Fatalf("unchanged source hydration %#v %v", page, err)
	}
	// Also insert after the guard exists. A global archive-revision check at
	// the final boundary would incorrectly reject this current response.
	readerApply(t, s, p, readerEvent("second-unrelated", "allowed", "another permitted insertion", now))
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); err != nil {
		t.Fatalf("unrelated insertion rejected final response: %v", err)
	}
	e.Kind, e.SourceRevision, e.Body = EventEditContent, 2, "changed body"
	readerApply(t, s, p, e)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("changed contributor final check: %v", err)
	}
	if _, err = s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Semantic[0].Ref}, Snapshot: pair.Snapshot}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("changed contributor hydration: %v", err)
	}
}

func TestCandidateHydrationKeepsStoredGeneratedAnchor(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	older := readerApply(t, s, p, readerEvent("older", "older-chat", "earlier source", now.Add(-time.Hour)))
	newer := readerApply(t, s, p, readerEvent("anchor", "anchor-chat", "current media source", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "anchored", "generated attached text", []MessageID{newer, older}, []float32{1, 0}, "anchor-origin")
	scope := readerScope(p, readerChat(t, s, newer))
	scope.AllChats, scope.Chats = true, nil
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "generated"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Keyword) != 1 || len(pair.Semantic) != 1 {
		t.Fatalf("candidate pair %#v %v", pair, err)
	}
	for _, candidate := range []Candidate{pair.Keyword[0], pair.Semantic[0]} {
		ref := candidate.Ref
		if ref.ReadSources[0].Ref.Version.MessageID != newer || ref.rootReference != (candidate.Kind == CandidateKeyword) {
			t.Fatalf("candidate lost stored anchor or fragment namespace: %#v", ref)
		}
		page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{ref}, Snapshot: pair.Snapshot})
		if err != nil || len(page.Documents) != 1 {
			t.Fatalf("hydration %#v %v", page, err)
		}
		document := page.Documents[0]
		if document.ChatID != readerChat(t, s, newer) || !document.SentAt.Equal(ref.ReadSources[0].OriginalSentAt) || document.Ref.ReadSources[0].Ref.Version.MessageID != newer || document.Excerpts[0].Source.MessageID != newer || document.Kind != MessageText || document.Excerpts[0].Text != "generated attached text" {
			t.Fatalf("hydration changed generated anchor or authorized snippet: %#v", document)
		}
		if len(page.Guard.Sources) != 2 {
			t.Fatalf("generated anchor omitted contributor guard: %#v", page.Guard)
		}
		if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCandidateKeywordOnlyAndBoundaryWindow(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	old := readerApply(t, s, p, readerEvent("old", "chat", "keyword old", now.Add(-2*time.Hour)))
	current := readerApply(t, s, p, readerEvent("new", "chat", "keyword current", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "boundary", "keyword generated", []MessageID{old, current}, []float32{1, 0})
	scope := readerScope(p, readerChat(t, s, current))
	scope.From = now.Add(-time.Hour)
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Semantic) != 0 || len(pair.Keyword) != 1 || pair.Keyword[0].Ref.MessageID != current {
		t.Fatalf("date-boundary window %#v %v", pair, err)
	}
	pair, err = s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
	if err != nil || pair.VectorCode != "keyword_only" || len(pair.Semantic) != 0 || len(pair.Keyword) != 1 {
		t.Fatalf("keyword-only fallback %#v %v", pair, err)
	}
}

func TestCandidateDerivedLexicalRequiresCompleteCurrentMembership(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	allowed := readerApply(t, s, p, readerEvent("allowed", "allowed", "unrelated original", now.Add(-time.Minute)))
	denied := readerApply(t, s, p, readerEvent("denied", "denied", "unrelated original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "allowed-lexical", "lexical derived canary", []MessageID{allowed}, nil)
	candidateDocument(t, s, p, generation, "denied-lexical", "lexical private canary", []MessageID{denied}, nil)
	candidateDocument(t, s, p, generation, "mixed-lexical", "lexical mixed canary", []MessageID{allowed, denied}, nil)
	candidateDocument(t, s, p, generation, "missing-member", "lexical missing canary", []MessageID{allowed, denied}, nil)
	if _, err := s.writeDB.Exec(`DELETE FROM extension_root_sources WHERE root_id='missing-member' AND message_id=?`, denied); err != nil {
		t.Fatal(err)
	}
	candidateDocument(t, s, p, generation, "stale-member", "lexical stale canary", []MessageID{allowed}, nil)
	if _, err := s.writeDB.Exec(`UPDATE extension_root_sources SET local_revision=local_revision+1 WHERE root_id='stale-member'`); err != nil {
		t.Fatal(err)
	}
	pair, err := s.CandidatePair(context.Background(), readerScope(p, readerChat(t, s, allowed)), CandidatePairRequest{Search: SearchRequest{Query: "lexical"}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 1 || pair.Keyword[0].Ref.ID != "allowed-lexical" || len(pair.Semantic) != 0 {
		t.Fatalf("derived lexical membership gate %#v %v", pair, err)
	}
}

func TestCandidatePairBranchesShareOneSQLiteSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	old := readerApply(t, s, p, readerEvent("old", "chat", "keyword original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "old-semantic", "semantic old", []MessageID{old}, []float32{0, 1})
	scope := readerScope(p, readerChat(t, s, old))
	s.readDB.SetMaxOpenConns(1)
	s.readDB.SetMaxIdleConns(1)
	connection, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reading, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	var selectionInTransaction bool
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	resetAuthorizer := func() error {
		connection, err := s.readDB.Conn(context.Background())
		if err != nil {
			return err
		}
		defer connection.Close()
		return connection.Raw(func(raw any) error {
			raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(nil)
			return nil
		})
	}
	defer func() {
		releaseRead()
		if err := resetAuthorizer(); err != nil {
			t.Error(err)
		}
	}()
	// beginRead has already read archive metadata in this transaction before
	// SQLite prepares the lexical scorer. Permit a committed insertion here;
	// a second transaction for semantic selection would see the nearer new hit.
	err = connection.Raw(func(raw any) error {
		sqliteConnection := raw.(*sqlite3.SQLiteConn)
		sqliteConnection.RegisterAuthorizer(func(op int, a, b, c string) int {
			if op == sqlite3.SQLITE_FUNCTION && b == "wa_doc_token_rank_v1" {
				once.Do(func() {
					selectionInTransaction = !sqliteConnection.AutoCommit()
					close(reading)
					<-release
				})
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		pair CandidatePair
		err  error
	}
	request := CandidatePairRequest{Search: SearchRequest{Query: "keyword", Limit: 1}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}}
	out := make(chan result, 1)
	go func() {
		pair, err := s.CandidatePair(context.Background(), scope, request)
		out <- result{pair, err}
	}()
	select {
	case <-reading:
	case <-time.After(2 * time.Second):
		releaseRead()
		t.Fatal("lexical selection did not reach snapshot interleaving")
	}
	if !selectionInTransaction {
		t.Fatal("lexical selection prepared outside the read transaction")
	}
	newID := readerApply(t, s, p, readerEvent("new", "chat", "keyword new", now))
	candidateDocument(t, s, p, generation, "new-semantic", "semantic new", []MessageID{newID}, []float32{1, 0})
	releaseRead()
	r := <-out
	if err := resetAuthorizer(); err != nil {
		t.Fatal(err)
	}
	if r.err != nil || len(r.pair.Keyword) != 1 || r.pair.Keyword[0].Ref.MessageID != old || len(r.pair.Semantic) != 1 || r.pair.Semantic[0].Ref.ID != "old-semantic" {
		t.Fatalf("branches mixed SQLite snapshots %#v %v", r.pair, r.err)
	}
	fresh, err := s.CandidatePair(context.Background(), scope, request)
	if err != nil || len(fresh.Keyword) != 1 || fresh.Keyword[0].Ref.MessageID != newID || len(fresh.Semantic) != 1 || fresh.Semantic[0].Ref.ID != "new-semantic" {
		t.Fatalf("fresh snapshot did not see committed candidates %#v %v", fresh, err)
	}
}

func TestCandidateCheckResponsePoolAdmissionPrecedesCommitFence(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "keyword original", now.Add(-time.Minute)))
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
	if err != nil {
		t.Fatal(err)
	}
	s.readDB.SetMaxOpenConns(1)
	held, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before := s.readDB.Stats().WaitCount
	out := make(chan error, 1)
	go func() {
		_, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard})
		out <- err
	}()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for s.readDB.Stats().WaitCount == before {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("final response did not wait for pool admission")
		}
	}
	control, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	policyResult := make(chan error, 1)
	go func() {
		_, err := s.SetPolicy(control, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
		policyResult <- err
	}()
	select {
	case err = <-policyResult:
		if err != nil {
			held.Close()
			<-out
			t.Fatalf("pool admission held commit fence: %v", err)
		}
	case <-time.After(time.Second):
		// Release the pool resource before reporting the regression. If the
		// implementation held RLock while waiting, this unblocks both the
		// response and accepted policy command rather than hanging cleanup.
		held.Close()
		<-out
		<-policyResult
		t.Fatal("pool admission blocked the writer commit fence")
	}
	held.Close()
	if err = <-out; !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("fresh fence missed committed policy change: %v", err)
	}
}

func TestCandidateResponseCredentialCurrentness(t *testing.T) {
	for _, scenario := range []string{"revoke", "scope", "expiry", "unsafe"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			id := readerApply(t, s, p, readerEvent("source", "chat", "keyword original", now.Add(-time.Minute)))
			chat := readerChat(t, s, id)
			credential := createCredential(t, s, scenario, CredentialPolicy{Account: "account", Chats: []ChatID{chat}})
			grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			pair, err := s.CandidatePair(context.Background(), grant.Scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			page, err := s.HydrateDocuments(context.Background(), grant.Scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: page.Guard, Credential: &grant}); err != nil {
				t.Fatalf("current credential denied: %v", err)
			}
			want := ErrUnauthorized
			switch scenario {
			case "revoke":
				err = s.RevokeCredential(context.Background(), credential.ID, credential.Revision)
			case "scope":
				_, err = s.SetCredentialPolicy(context.Background(), credential.ID, CredentialPolicy{Account: "account", Chats: []ChatID{chat}, From: now}, credential.Revision)
			case "expiry":
				_, err = s.writeDB.Exec(`UPDATE credentials SET expires_at=? WHERE id=?`, unixms(now.Add(time.Second)), credential.ID)
				now = now.Add(time.Second)
			case "unsafe":
				err = s.MarkUnsafe(context.Background(), "account", UnsafeReason("mutation_metadata_missing"))
				want = ErrReconciliationRequired
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: page.Guard, Credential: &grant}); !errors.Is(err, want) {
				t.Fatalf("accepted %s credential: %v", scenario, err)
			}
		})
	}
}

func TestCandidateHydrationLimitsAndMalformedRefs(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", strings.Repeat("keyword ", 300), now.Add(-time.Minute)))
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	request := HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot, MaxBytes: 32}
	if page, err := s.HydrateDocuments(context.Background(), scope, request); !errors.Is(err, ErrLimitExceeded) || len(page.Documents) != 0 {
		t.Fatalf("partial body returned beyond limit %#v %v", page, err)
	}
	request.MaxBytes = 0
	request.Documents[0].ReadSources = nil
	if _, err = s.HydrateDocuments(context.Background(), scope, request); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("unguarded source accepted: %v", err)
	}
	for _, query := range []string{" ", string([]byte{0xff}), strings.Repeat("a", 1025)} {
		if _, err = s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: query}, KeywordOnly: true}); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("malformed query accepted: %v", err)
		}
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
	if err != nil {
		t.Fatal(err)
	}
	nameHash := page.Guard.Sources[0].MetadataHash
	page.Guard.Sources[0].MetadataHash = [32]byte{}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("unguarded display metadata accepted: %v", err)
	}
	page.Guard.Sources[0].MetadataHash = nameHash
	page.Guard.Sources = make([]SourceReadVersion, maxResponseSources+1)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("unbounded contributor guard accepted: %v", err)
	}
}

func TestCandidateHydrationRejectsInvalidDerivedProvenance(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unrelated original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "derived", "derived keyword", []MessageID{id}, []float32{1, 0}, "derived-origin")
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Semantic) != 1 {
		t.Fatalf("pair %#v %v", pair, err)
	}
	// OCR has no audio word positions. Invalid stored labels must not be
	// serialized as credible generated provenance even for a permitted source.
	invalid, _ := json.Marshal(DerivedProvenance{Kind: DocumentOCR, MediaID: "fixture-media", ProfileID: "fixture", ExtractorVersion: "1", OffsetKind: "word", StartMS: 1, EndMS: 2})
	if _, err = s.writeDB.Exec(`UPDATE extension_documents SET provenance_json=? WHERE id='derived'`, string(invalid)); err != nil {
		t.Fatal(err)
	}
	if page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Semantic[0].Ref}, Snapshot: pair.Snapshot}); !errors.Is(err, ErrSourceUnavailable) || len(page.Documents) != 0 {
		t.Fatalf("invalid derived provenance escaped %#v %v", page, err)
	}
}

func TestCandidateHydrationRejectsOversizedStoredDerivedText(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unrelated original", now.Add(-time.Minute)))
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "derived", "derived keyword", []MessageID{id}, []float32{1, 0}, "derived-origin")
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, Generation: generation.ID, Version: generation.Version, Query: []float32{1, 0}})
	if err != nil || len(pair.Semantic) != 1 {
		t.Fatalf("pair %#v %v", pair, err)
	}
	if _, err = s.writeDB.Exec(`UPDATE extension_documents SET text=? WHERE id='derived'`, strings.Repeat("x", maxDerivedBytes+1)); err != nil {
		t.Fatal(err)
	}
	if page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Semantic[0].Ref}, Snapshot: pair.Snapshot}); !errors.Is(err, ErrLimitExceeded) || len(page.Documents) != 0 {
		t.Fatalf("oversized stored derived text escaped %#v %v", page, err)
	}
}

func TestCandidateCheckResponseExpiryCrossedDuringFencedSQL(t *testing.T) {
	for _, scenario := range []string{"source", "credential"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("source", "chat", "keyword original", now.Add(-time.Minute))
			if scenario == "source" {
				e.ExpiresAt = now.Add(10 * time.Millisecond)
			}
			id := readerApply(t, s, p, e)
			scope := readerScope(p, readerChat(t, s, id))
			var grant *CredentialGrant
			if scenario == "credential" {
				credential := createCredential(t, s, "fenced-expiry", CredentialPolicy{Account: "account", Chats: scope.Chats})
				resolved, err := s.ResolveCredentialID(context.Background(), credential.ID)
				if err != nil {
					t.Fatal(err)
				}
				grant, scope = &resolved, resolved.Scope
				if _, err = s.writeDB.Exec(`UPDATE credentials SET expires_at=? WHERE id=?`, unixms(now.Add(10*time.Millisecond)), credential.ID); err != nil {
					t.Fatal(err)
				}
			}
			pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
			if err != nil {
				t.Fatal(err)
			}
			s.readDB.SetMaxOpenConns(1)
			s.readDB.SetMaxIdleConns(1)
			connection, err := s.readDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var delayed atomic.Bool
			// Delay SQLite preparation after the service-clock sample and
			// fence acquisition. The injected service clock stays fixed; the
			// elapsed final horizon must include this authoritative SQL work.
			err = connection.Raw(func(raw any) error {
				raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, a, b, c string) int {
					if delayed.CompareAndSwap(false, true) {
						time.Sleep(20 * time.Millisecond)
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			})
			connection.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := ErrSourceUnavailable
			if scenario == "credential" {
				want = ErrUnauthorized
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard, Credential: grant}); !errors.Is(err, want) {
				t.Fatalf("accepted %s expiry crossing during fenced SQL: %v", scenario, err)
			}
		})
	}
}

func TestCandidateWindowHydrationReconstructsAuthoritativeOriginalSpans(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	original := "שָׁלוֹם current original keyword"
	e := readerEvent("source", "chat", original, now.Add(-time.Minute))
	id := readerApply(t, s, p, e)
	generation := candidateIndex(t, s)
	candidateDocument(t, s, p, generation, "window", "fabricated stored keyword", []MessageID{id}, nil)
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	spans, _ := json.Marshal([]SourceSpan{{Source: ref, StartByte: 0, EndByte: len(original), Field: "body"}})
	provenance, _ := json.Marshal(DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"})
	if _, err = s.writeDB.Exec(`UPDATE extension_documents SET kind='text_window',spans_json=?,provenance_json=? WHERE id='window'`, string(spans), string(provenance)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`DELETE FROM extension_root_fts WHERE root_id='window'`); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var window DocumentRef
	for _, candidate := range pair.Keyword {
		if candidate.Ref.ID == "window" {
			window = candidate.Ref
		}
	}
	if window.ID == "" {
		t.Fatal("window keyword candidate missing")
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{window}, Snapshot: pair.Snapshot})
	if err != nil || len(page.Documents) != 1 || page.Documents[0].Excerpts[0].Text != original || page.Documents[0].Excerpts[0].Provenance.Kind != DocumentWindow {
		t.Fatalf("window did not reconstruct original logical spans: %#v %v", page, err)
	}
	// A byte offset in the middle of Hebrew UTF-8 must fail closed.
	spans, _ = json.Marshal([]SourceSpan{{Source: ref, StartByte: 1, EndByte: len(original), Field: "body"}})
	if _, err = s.writeDB.Exec(`UPDATE extension_documents SET spans_json=? WHERE id='window'`, string(spans)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{window}, Snapshot: pair.Snapshot}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("non-UTF8 logical span accepted: %v", err)
	}
}

func TestCandidateResponseDisplayNamePinsAreContributorSpecific(t *testing.T) {
	for _, scenario := range []string{"chat", "sender", "unrelated"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("source", "chat", "keyword original", now.Add(-time.Minute))
			id := readerApply(t, s, p, e)
			other := readerEvent("other", "other-chat", "other original", now.Add(-time.Minute))
			readerApply(t, s, p, other)
			scope := readerScope(p, readerChat(t, s, id))
			pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard}); err != nil {
				t.Fatalf("current source denied: %v", err)
			}
			names := SourceNames{Account: "account"}
			switch scenario {
			case "chat":
				names.Chat, names.ChatName = e.Key.Chat, "Renamed contributing chat"
			case "sender":
				names.Sender, names.SenderName = e.Key.Sender, "Renamed contributing sender"
			case "unrelated":
				names.Chat, names.ChatName = other.Key.Chat, "Unrelated renamed chat"
			}
			if err = s.SetSourceNames(context.Background(), names); err != nil {
				t.Fatal(err)
			}
			_, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: page.Guard})
			if scenario == "unrelated" {
				if err != nil {
					t.Fatalf("unrelated display rename discarded valid contributor: %v", err)
				}
			} else if !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("changed contributing %s display name accepted: %v", scenario, err)
			}
		})
	}
}

func TestCandidateWindowSpansFollowChronologyIndependentOfMembershipOrder(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	old := readerEvent("older", "chat", "older body keyword", now.Add(-2*time.Minute))
	old.Caption = "older caption"
	oldID := readerApply(t, s, p, old)
	newer := readerEvent("newer", "chat", "newer body keyword", now.Add(-time.Minute))
	newID := readerApply(t, s, p, newer)
	generation := candidateIndex(t, s)
	// Sources are a membership set; the publisher accepts either membership
	// order while requiring original chronological body/caption span order.
	candidateDocument(t, s, p, generation, "window", "stored keyword", []MessageID{newID, oldID}, nil)
	oldRef, err := s.CurrentSource(context.Background(), old.Key)
	if err != nil {
		t.Fatal(err)
	}
	newRef, err := s.CurrentSource(context.Background(), newer.Key)
	if err != nil {
		t.Fatal(err)
	}
	spans, _ := json.Marshal([]SourceSpan{
		{Source: oldRef, StartByte: 0, EndByte: len(old.Body), Field: "body"},
		{Source: oldRef, StartByte: 0, EndByte: len(old.Caption), Field: "caption"},
		{Source: newRef, StartByte: 0, EndByte: len(newer.Body), Field: "body"},
	})
	provenance, _ := json.Marshal(DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"})
	if _, err = s.writeDB.Exec(`UPDATE extension_documents SET kind='text_window',spans_json=?,provenance_json=? WHERE id='window'`, string(spans), string(provenance)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`DELETE FROM extension_root_fts WHERE root_id='window'`); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, oldID))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "keyword"}, KeywordOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var window DocumentRef
	for _, candidate := range pair.Keyword {
		if candidate.Ref.ID == "window" {
			window = candidate.Ref
		}
	}
	if window.ID == "" {
		t.Fatal("window candidate missing")
	}
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Documents: []DocumentRef{window}, Snapshot: pair.Snapshot})
	if err != nil || len(page.Documents) != 1 || page.Documents[0].Excerpts[0].Text != "older body keyword\nolder caption\nnewer body keyword" || len(page.Guard.Sources) != 2 {
		t.Fatalf("membership order changed original span chronology %#v %v", page, err)
	}
}
