//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type extensionHeldResult struct {
	value any
	err   error
}

// Hold a response after a real writer commit, modeling a lost accepted reply.
type extensionHeldReply struct {
	inner     extensionCommand
	committed chan<- extensionHeldResult
	release   <-chan struct{}
}

func (c extensionHeldReply) priority() bool                    { return c.inner.priority() }
func (c extensionHeldReply) account() string                   { return c.inner.account() }
func (c extensionHeldReply) sensitivity() extensionSensitivity { return extensionBackground }
func (c extensionHeldReply) run(s *Store, ctx context.Context) (any, error) {
	v, err := c.inner.run(s, ctx)
	c.committed <- extensionHeldResult{v, err}
	<-c.release
	return v, err
}

func TestExtractionRecoveryFollowsCancelledCommittedClaim(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	_, media, reservation := extractionTestOriginal(t, s, p, "lost-claim-reply", JobOCR)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	committed := make(chan extensionHeldResult, 1)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	response := make(chan error, 1)
	go func() {
		_, err := s.submitExtension(ctx, extensionHeldReply{inner: extensionExtractionCommand{op: extensionExtractionClaim, claim: ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute}}, committed: committed, release: release}, 2048)
		response <- err
	}()
	var result extensionHeldResult
	select {
	case result = <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not commit")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	work := result.value.(ExtractionWork)
	cancel()
	if err := <-response; !errors.Is(err, context.Canceled) {
		t.Fatalf("lost reply: %v", err)
	}
	type listed struct {
		page ExtractionRecoveryPage
		err  error
	}
	listing := make(chan listed, 1)
	go func() {
		page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account"})
		listing <- listed{page, err}
	}()
	select {
	case got := <-listing:
		t.Fatalf("recovery bypassed earlier FIFO claim: %#v %v", got.page, got.err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	released = true
	var got listed
	select {
	case got = <-listing:
	case <-time.After(5 * time.Second):
		t.Fatal("ordered recovery did not finish")
	}
	if got.err != nil || len(got.page.Items) != 1 || got.page.NextAfter != "" || got.page.Items[0] != (PinnedExtraction{Account: "account", Lease: work.Lease, MediaID: media, ArtifactID: reservation.ArtifactID}) {
		t.Fatalf("lost committed ownership absent: %#v %v", got.page, got.err)
	}
	charge := extensionMediaCharge(t, s, BudgetOriginals)
	ack := JobRetry{Lease: got.page.Items[0].Lease, Code: "cancelled", After: time.Second}
	if err := s.RetryExtraction(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryExtraction(context.Background(), ack); err != nil {
		t.Fatalf("recovery replay: %v", err)
	}
	page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account"})
	if err != nil || len(page.Items) != 0 || extensionMediaCharge(t, s, BudgetOriginals) != charge {
		t.Fatalf("ack changed physical quota or retained ownership: %#v %v", page, err)
	}
	if source, err := s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "synthetic", Limit: 10}); err != nil || len(source.Hits) == 0 {
		t.Fatalf("lost reply quarantined source: %#v %v", source, err)
	}
}

func TestExtractionRecoveryPagesAllStatesAndSeparatesAccounts(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "paged-recovery", JobOCR)
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET id='pinned-000' WHERE id=?", work.ID); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 65; i++ {
		state := []string{"pending", "leased", "complete", "failed", "cancelled"}[i%5]
		_, err = s.writeDB.Exec(`INSERT INTO extension_extraction_jobs(id,account,version_id,kind,media_id,artifact_id,original_sha256,message_id,local_revision,policy_generation,content_hash,source_expires_at,work_hash,state,attempts,lease_id,lease_until,input_pinned,created_at,updated_at) SELECT ?,account,version_id,kind,media_id,artifact_id,?,message_id,local_revision,policy_generation,content_hash,source_expires_at,work_hash,?,attempts,?,0,1,created_at,updated_at FROM extension_extraction_jobs WHERE id='pinned-000'`, fmt.Sprintf("pinned-%03d", i), fmt.Sprintf("%064x", i), state, fmt.Sprintf("lease-%03d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	// Account inventory is independent of source and artifact authorization.
	if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET active=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET state='evicting'"); err != nil {
		t.Fatal(err)
	}
	s.safetyMu.Lock()
	s.unsafe["account"] = true
	s.unclean = true
	s.safetyMu.Unlock()
	var after JobID
	var ids []JobID
	for _, want := range []int{32, 32, 1} {
		page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account", After: after})
		if err != nil || len(page.Items) != want {
			t.Fatalf("default page %#v %v", page, err)
		}
		for _, item := range page.Items {
			ids = append(ids, item.Lease.JobID)
		}
		after = page.NextAfter
	}
	if after != "" || len(ids) != 65 {
		t.Fatalf("paging never completed: %q %#v", after, ids)
	}
	for i, id := range ids {
		if string(id) != fmt.Sprintf("pinned-%03d", i) {
			t.Fatalf("gap/order at %d: %s", i, id)
		}
	}
	page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account", Limit: 64})
	if err != nil || len(page.Items) != 64 || page.NextAfter != "pinned-063" {
		t.Fatalf("maximum page %#v %v", page, err)
	}
	other, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "other-account"})
	if err != nil || len(other.Items) != 0 || other.NextAfter != "" {
		t.Fatalf("account crossed recovery boundary: %#v %v", other, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 64 {
		t.Fatal("listing changed charge")
	}
}

func TestExtractionRecoveryRejectsMalformedMetadataAndRequests(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "invalid-recovery", JobOCR)
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []ExtractionRecoveryRequest{{}, {Account: "account", After: JobID(strings.Repeat("a", 129))}, {Account: "account", After: JobID("bad\x00cursor")}, {Account: "account", After: JobID(string([]byte{255}))}, {Account: "account", Limit: -1}} {
		if _, err = s.ListPinnedExtractions(context.Background(), request); !errors.Is(err, ErrInvalidExtension) {
			t.Fatalf("invalid recovery accepted: %#v %v", request, err)
		}
	}
	if _, err = s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account", Limit: 65}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("oversized page: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.ListPinnedExtractions(ctx, ExtractionRecoveryRequest{Account: "account"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listing: %v", err)
	}
	if err = s.RetryExtraction(ctx, JobRetry{Lease: work.Lease, Code: "cancelled", After: time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acknowledgement: %v", err)
	}
	for _, badLease := range []string{"", strings.Repeat("x", 257), "bad\x00lease"} {
		if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET lease_id=? WHERE id=?", badLease, work.ID); err != nil {
			t.Fatal(err)
		}
		page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account"})
		if !errors.Is(err, ErrArchiveUnavailable) || len(page.Items) != 0 || page.NextAfter != "" {
			t.Fatalf("corrupt pin silently skipped/partially returned: %#v %v", page, err)
		}
	}
	if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET lease_id=? WHERE id=?", work.Lease.LeaseID, work.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account"})
	if err != nil || len(page.Items) != 1 || extensionMediaCharge(t, s, BudgetOriginals) != 64 {
		t.Fatalf("cancellation released pin/charge: %#v %v", page, err)
	}
	if source, err := s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "synthetic", Limit: 10}); err != nil || len(source.Hits) == 0 {
		t.Fatalf("metadata cancellation quarantined source: %#v %v", source, err)
	}
}

func TestExtractionRecoveryAfterReopenAcknowledgesCancelledExpiredPin(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	p := testPolicy(t, s)
	_, _, r := extractionTestOriginal(t, s, p, "reopen-recovery", JobOCR)
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, event("reopen-recovery", "", 2, EventDelete), p.Generation)
	now = now.Add(2 * time.Minute)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	s.safetyMu.Lock()
	s.unsafe["account"] = true
	s.unclean = true
	s.safetyMu.Unlock()
	page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Lease != work.Lease || page.Items[0].ArtifactID != r.ArtifactID {
		t.Fatalf("reopen lost exact pin: %#v %v", page, err)
	}
	if items, err := s.ReconcileMedia(context.Background(), 10); err != nil || len(items) != 0 {
		t.Fatalf("reconciliation bypassed recovered owner: %#v %v", items, err)
	}
	ack := JobRetry{Lease: page.Items[0].Lease, Code: "cancelled", After: time.Second}
	if err = s.RetryExtraction(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryExtraction(context.Background(), ack); err != nil {
		t.Fatalf("reopen ack replay %v", err)
	}
	var state string
	var pinned bool
	if err = s.readDB.QueryRow("SELECT state,input_pinned FROM extension_extraction_jobs WHERE id=?", work.ID).Scan(&state, &pinned); err != nil || state != "cancelled" || pinned {
		t.Fatalf("ack resurrected cancelled authority: %s %v %v", state, pinned, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 64 {
		t.Fatal("ownership acknowledgement erased physical accounting")
	}
	items, err := s.ReconcileMedia(context.Background(), 10)
	if err != nil || len(items) != 1 || items[0].ID != r.ArtifactID {
		t.Fatalf("recovery did not unblock cleanup: %#v %v", items, err)
	}
}

func TestExtractionRecoveryRejectsBLOBOwnershipIncludingContinuation(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "blob-recovery", JobOCR)
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// This fixture models corrupt persisted affinity; production writes bind TEXT.
	if _, err = s.writeDB.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	defer s.writeDB.Exec("PRAGMA foreign_keys=ON")
	for _, field := range []struct{ name, value string }{{"id", string(work.ID)}, {"account", work.Source.Account}, {"lease_id", string(work.Lease.LeaseID)}, {"media_id", string(work.MediaID)}, {"artifact_id", string(work.ArtifactID)}} {
		if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET "+field.name+"=CAST(? AS BLOB)", field.value); err != nil {
			t.Fatal(err)
		}
		for _, after := range []JobID{"", JobID("zzzz-after-all-text-ids")} {
			// BLOB IDs sort after every TEXT cursor, so continuation must reject
			// them rather than repeating a seemingly valid Go string forever.
			if after != "" && field.name != "id" {
				continue
			}
			page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account", After: after})
			if !errors.Is(err, ErrArchiveUnavailable) || len(page.Items) != 0 || page.NextAfter != "" {
				t.Fatalf("BLOB %s escaped recovery: %#v %v", field.name, page, err)
			}
		}
		if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET "+field.name+"=?", field.value); err != nil {
			t.Fatal(err)
		}
	}
	if page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account"}); err != nil || len(page.Items) != 1 {
		t.Fatalf("corrupt fixture restoration lost ownership: %#v %v", page, err)
	}
}

func TestExtractionRecoveryAcknowledgesCompletePinWithoutChangingCompletion(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "completed-pin-recovery", JobOCR)
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	doc := extractionTestDocument(work, "completed-recovery-root", "completed authoritative recovery text")
	result := ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{doc}}
	if ok, err := s.CommitExtraction(context.Background(), result); err != nil || !ok {
		t.Fatalf("completion %v %v", ok, err)
	}
	ack := JobRetry{Lease: work.Lease, Code: "cancelled", After: time.Second}
	if err = s.RetryExtraction(context.Background(), ack); !errors.Is(err, ErrNoWork) {
		t.Fatalf("ordinary completed work was modified %v", err)
	}
	var receipt string
	if err = s.readDB.QueryRow("SELECT completion_digest FROM extension_extraction_jobs WHERE id=?", work.ID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET input_pinned=1 WHERE id=?", work.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListPinnedExtractions(context.Background(), ExtractionRecoveryRequest{Account: "account"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Lease != work.Lease {
		t.Fatalf("completed ownership not inventoried %#v %v", page, err)
	}
	if err = s.RetryExtraction(context.Background(), JobRetry{Lease: JobLease{JobID: work.ID, LeaseID: "wrong"}, Code: "cancelled", After: time.Second}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("wrong completed owner %v", err)
	}
	if err = s.RetryExtraction(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryExtraction(context.Background(), ack); err != nil {
		t.Fatalf("complete ownership replay %v", err)
	}
	var state, outcome, afterReceipt string
	var pinned bool
	var deadline int64
	if err = s.readDB.QueryRow("SELECT state,outcome,completion_digest,input_pinned,lease_until FROM extension_extraction_jobs WHERE id=?", work.ID).Scan(&state, &outcome, &afterReceipt, &pinned, &deadline); err != nil || state != "complete" || outcome != "text" || afterReceipt != receipt || pinned || deadline != 0 {
		t.Fatalf("pin ack changed completion: %s %s %s %v %d %v", state, outcome, afterReceipt, pinned, deadline, err)
	}
	if root, err := s.extensionLoadRoot(context.Background(), s.readDB, doc.ID); err != nil || root.Text != doc.Text {
		t.Fatalf("pin settlement erased root: %#v %v", root, err)
	}
	if ok, err := s.CommitExtraction(context.Background(), result); err != nil || !ok {
		t.Fatalf("pin settlement erased receipt: %v %v", ok, err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 64 {
		t.Fatal("pin settlement erased physical charge")
	}
}
