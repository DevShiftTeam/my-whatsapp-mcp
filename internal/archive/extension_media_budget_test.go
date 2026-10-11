//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func originalBudgetTest(t *testing.T, s *Store) OriginalBudget {
	t.Helper()
	budget, err := s.GetOriginalBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return budget
}

func TestOriginalBudgetCASChangesOnlyBudgetRevision(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("budget-readable", "budget setting preserves source authority", 1, EventOriginal), p.Generation)
	before := originalBudgetTest(t, s)
	if before != (OriginalBudget{MaximumBytes: DefaultOriginalBudgetBytes, Revision: 1}) {
		t.Fatalf("default budget %#v", before)
	}
	var archiveBefore, indexBefore, policyBefore int64
	if err := s.readDB.QueryRow("SELECT archive_revision,index_generation FROM archive_meta WHERE id=1").Scan(&archiveBefore, &indexBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow("SELECT generation FROM archive_policy WHERE id=1").Scan(&policyBefore); err != nil {
		t.Fatal(err)
	}
	s.safetyMu.RLock()
	epoch := s.safetyEpoch
	s.safetyMu.RUnlock()
	unchanged, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: before.MaximumBytes, ExpectedRevision: before.Revision})
	if err != nil || unchanged != before {
		t.Fatalf("same-value CAS changed revision %#v %v", unchanged, err)
	}
	changed, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: before.Revision})
	if err != nil || changed.MaximumBytes != MinimumOriginalBudgetBytes || changed.Revision != 2 {
		t.Fatalf("budget update %#v %v", changed, err)
	}
	if _, err = s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: before.Revision}); !errors.Is(err, ErrOriginalBudgetChanged) {
		t.Fatalf("stale same-value CAS succeeded %v", err)
	}
	if got := originalBudgetTest(t, s); got != changed {
		t.Fatalf("conflict mutated budget %#v", got)
	}
	var archiveAfter, indexAfter, policyAfter int64
	if err = s.readDB.QueryRow("SELECT archive_revision,index_generation FROM archive_meta WHERE id=1").Scan(&archiveAfter, &indexAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.readDB.QueryRow("SELECT generation FROM archive_policy WHERE id=1").Scan(&policyAfter); err != nil {
		t.Fatal(err)
	}
	s.safetyMu.RLock()
	epochAfter := s.safetyEpoch
	s.safetyMu.RUnlock()
	if archiveAfter != archiveBefore || indexAfter != indexBefore || policyAfter != policyBefore || epochAfter != epoch {
		t.Fatalf("budget update revoked source/index authority: %d/%d %d/%d %d/%d epoch%d/%d", archiveBefore, archiveAfter, indexBefore, indexAfter, policyBefore, policyAfter, epoch, epochAfter)
	}
	for _, invalid := range []OriginalBudgetUpdate{{MaximumBytes: MinimumOriginalBudgetBytes - 1, ExpectedRevision: 2}, {MaximumBytes: MaximumOriginalBudgetBytes + 1, ExpectedRevision: 2}, {MaximumBytes: MinimumOriginalBudgetBytes}, {MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: uint64(math.MaxInt64) + 1}} {
		if _, err = s.SetOriginalBudget(context.Background(), invalid); !errors.Is(err, ErrInvalidExtension) {
			t.Fatalf("invalid update accepted %#v %v", invalid, err)
		}
	}
	max, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MaximumOriginalBudgetBytes, ExpectedRevision: 2})
	if err != nil || max.MaximumBytes != MaximumOriginalBudgetBytes || max.Revision != 3 {
		t.Fatalf("inclusive maximum %#v %v", max, err)
	}
	ref, id := extensionTestMedia(t, s, p, "beyond-old-limit")
	extensionAdmit(t, s, ref, id, BudgetOriginals, DefaultOriginalBudgetBytes+1)
	if got := originalBudgetTest(t, s); got.ChargedBytes != DefaultOriginalBudgetBytes+1 || got.Pressure {
		t.Fatalf("old 2GiB constant still governs admission %#v", got)
	}
}

func TestOriginalBudgetLoweringKeepsLeaseChargeAndSurvivesReopen(t *testing.T) {
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
	ref, id := extensionTestMedia(t, s, p, "budget-existing")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 80<<20)
	other, otherID := extensionTestMedia(t, s, p, "budget-new")
	budget, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: 1})
	if err != nil || budget.ChargedBytes != 80<<20 || !budget.Pressure {
		t.Fatalf("decrease forgave reservation %#v %v", budget, err)
	}
	if _, err = s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: other, MediaID: otherID, Budget: BudgetOriginals, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("lowered limit did not stop new original %v", err)
	}
	if _, err = s.RenewMedia(context.Background(), r, time.Minute); err != nil {
		t.Fatalf("decrease revoked existing lease %v", err)
	}
	// Exact stored maximum, rather than the new limit, bounds prior publication.
	extensionPublish(t, s, r, 80<<20)
	extensionAdmit(t, s, other, otherID, BudgetDecoded, 128<<20)
	if _, err = s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: other, MediaID: otherID, Budget: BudgetDecoded, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("decoded pool changed with original budget %v", err)
	}
	now = now.Add(2 * time.Minute)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if got := originalBudgetTest(t, s); got != budget {
		t.Fatalf("expiry/reopen changed persisted charge %#v want %#v", got, budget)
	}
	apply(t, s, event("budget-existing", "", 2, EventDelete), p.Generation)
	items, err := s.ReconcileMedia(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var original MediaArtifact
	for _, item := range items {
		if item.ID == r.ArtifactID {
			original = item
		}
	}
	if original.ID == "" {
		t.Fatalf("source revoke did not produce cleanup inventory %#v", items)
	}
	if got := originalBudgetTest(t, s); got.ChargedBytes != 80<<20 || !got.Pressure {
		t.Fatalf("begin cleanup released charge %#v", got)
	}
	cleanup := MediaCleanup{ReservationID: original.ReservationID, ArtifactID: original.ID, LeaseID: original.CleanupLease, Code: "removed"}
	if err = s.FinishMediaCleanup(context.Background(), cleanup); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishMediaCleanup(context.Background(), cleanup); err != nil {
		t.Fatal(err)
	}
	if got := originalBudgetTest(t, s); got.MaximumBytes != MinimumOriginalBudgetBytes || got.Revision != 2 || got.ChargedBytes != 0 || got.Pressure {
		t.Fatalf("ack did not release only exact physical charge %#v", got)
	}
}

func TestOriginalBudgetEqualityAndAllPhysicalStatesRemainCharged(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "all-charge-states")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, MinimumOriginalBudgetBytes)
	budget, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: 1})
	if err != nil || budget.ChargedBytes != budget.MaximumBytes || !budget.Pressure {
		t.Fatalf("equality pressure %#v %v", budget, err)
	}
	for _, state := range []string{"reserved", "staged", "retained", "evicting"} {
		if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET state=?,lease_until=0 WHERE id=?", state, r.ArtifactID); err != nil {
			t.Fatal(err)
		}
		if got := originalBudgetTest(t, s); got != budget {
			t.Fatalf("state %s forgave charge %#v", state, got)
		}
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,created_at,updated_at) SELECT 'extra-physical','extra-reservation',media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,'extra-lease',0,'evicting',created_at,updated_at FROM extension_media_artifacts WHERE id=?`, r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if got := originalBudgetTest(t, s); got.ChargedBytes != 2*MinimumOriginalBudgetBytes || !got.Pressure {
		t.Fatalf("duplicate physical charge collapsed by shared hash/media %#v", got)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET cleanup_ack=1 WHERE id='extra-physical'"); err != nil {
		t.Fatal(err)
	}
	if got := originalBudgetTest(t, s); got.ChargedBytes != MinimumOriginalBudgetBytes {
		t.Fatalf("acknowledged artifact remained charged %#v", got)
	}
}

func TestOriginalBudgetCompetingCASAndOrderedLostReplyAdmission(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "ordered-budget-admission")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, bytes := range []int64{128 << 20, 256 << 20} {
		wg.Add(1)
		go func(n int64) {
			defer wg.Done()
			<-start
			_, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: n, ExpectedRevision: 1})
			results <- err
		}(bytes)
	}
	close(start)
	wg.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else if errors.Is(err, ErrOriginalBudgetChanged) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("CAS competition %d winners %d conflicts", winners, conflicts)
	}
	current := originalBudgetTest(t, s)
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
		_, err := s.submitExtension(ctx, extensionHeldReply{inner: extensionOriginalBudgetCommand{update: &OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: current.Revision}}, committed: committed, release: release}, 64)
		response <- err
	}()
	select {
	case result := <-committed:
		if result.err != nil {
			t.Fatal(result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("budget commit did not finish")
	}
	cancel()
	if err := <-response; !errors.Is(err, context.Canceled) {
		t.Fatalf("lost budget reply %v", err)
	}
	admitted := make(chan error, 1)
	go func() {
		_, err := s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: ref, MediaID: id, Budget: BudgetOriginals, MaximumBytes: 80 << 20, TTL: time.Minute})
		admitted <- err
	}()
	select {
	case err := <-admitted:
		t.Fatalf("admission bypassed prior budget command %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	released = true
	select {
	case err := <-admitted:
		if !errors.Is(err, ErrQuotaExceeded) {
			t.Fatalf("new admission ignored committed limit %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordered admission did not finish")
	}
	got := originalBudgetTest(t, s)
	if got.MaximumBytes != MinimumOriginalBudgetBytes || got.Revision != current.Revision+1 || got.ChargedBytes != 0 {
		t.Fatalf("ordered reread failed %#v", got)
	}
}

func TestOriginalBudgetMetadataRecoveryWorksUnsafeWithoutCancellationPoison(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("budget-cancel-safe", "budget cancellation preserves source", 1, EventOriginal), p.Generation)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetOriginalBudget(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled get %v", err)
	}
	if _, err := s.SetOriginalBudget(ctx, OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled set %v", err)
	}
	if got := originalBudgetTest(t, s); got.Revision != 1 || got.MaximumBytes != DefaultOriginalBudgetBytes {
		t.Fatalf("cancelled preadmission setting changed %#v", got)
	}
	if source, err := s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "cancellation", Limit: 10}); err != nil || len(source.Hits) != 1 {
		t.Fatalf("budget cancellation quarantined source %#v %v", source, err)
	}
	s.safetyMu.Lock()
	s.unsafe["account"] = true
	s.unclean = true
	s.safetyMu.Unlock()
	got, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: 1})
	if err != nil || got.Revision != 2 {
		t.Fatalf("unsafe source blocked private metadata setting %#v %v", got, err)
	}
	if read := originalBudgetTest(t, s); read != got {
		t.Fatalf("unsafe metadata read %#v", read)
	}
}

func TestOriginalBudgetCorruptionAndRevisionOverflowFailClosed(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "corrupt-budget")
	if _, err := s.writeDB.Exec("DELETE FROM extension_original_budget"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOriginalBudget(context.Background()); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("missing row defaulted %v", err)
	}
	if _, err := s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: ref, MediaID: id, Budget: BudgetOriginals, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("missing row admission defaulted %v", err)
	}
	if _, err := s.writeDB.Exec("INSERT INTO extension_original_budget(id,maximum_bytes,revision) VALUES(1,?,?)", MinimumOriginalBudgetBytes, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	noOp, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: uint64(math.MaxInt64)})
	if err != nil || noOp.Revision != uint64(math.MaxInt64) {
		t.Fatalf("max revision no-op %#v %v", noOp, err)
	}
	if _, err = s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MaximumOriginalBudgetBytes, ExpectedRevision: uint64(math.MaxInt64)}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("revision overflow accepted %v", err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_original_budget SET maximum_bytes=?,revision=1", DefaultOriginalBudgetBytes); err != nil {
		t.Fatal(err)
	}
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 1)
	if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET maximum_bytes=?,charged_bytes=? WHERE id=?", int64(math.MaxInt64), int64(math.MaxInt64), r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,created_at,updated_at) SELECT 'overflow-artifact','overflow-reservation',media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,'overflow-lease',0,'reserved',created_at,updated_at FROM extension_media_artifacts WHERE id=?`, r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetOriginalBudget(context.Background()); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("charge overflow accepted %v", err)
	}
	if _, err = s.writeDB.Exec("DELETE FROM extension_media_artifacts WHERE id='overflow-artifact'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET maximum_bytes=128,charged_bytes=1.5 WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetOriginalBudget(context.Background()); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("fractional charge coerced %v", err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET charged_bytes=1 WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_original_budget SET maximum_bytes=67108864.5"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetOriginalBudget(context.Background()); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("fractional limit coerced %v", err)
	}
}

func TestOriginalBudgetMalformedCleanupCannotHidePhysicalCharges(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, id := extensionTestMedia(t, s, p, "bad-cleanup-charge")
	r := extensionAdmit(t, s, ref, id, BudgetOriginals, 1024)
	other, otherID := extensionTestMedia(t, s, p, "bad-cleanup-new")
	assertDenied := func(t *testing.T) {
		t.Helper()
		if _, err := s.GetOriginalBudget(context.Background()); !errors.Is(err, ErrArchiveUnavailable) {
			t.Fatalf("corrupt ownership was reported recovered: %v", err)
		}
		if _, err := s.SetOriginalBudget(context.Background(), OriginalBudgetUpdate{MaximumBytes: MinimumOriginalBudgetBytes, ExpectedRevision: 1}); !errors.Is(err, ErrArchiveUnavailable) {
			t.Fatalf("corrupt ownership permitted settings mutation: %v", err)
		}
		if _, err := s.AdmitMedia(context.Background(), MediaAdmissionRequest{Source: other, MediaID: otherID, Budget: BudgetOriginals, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrArchiveUnavailable) {
			t.Fatalf("corrupt ownership permitted new bytes: %v", err)
		}
		var revision, maximum, artifacts int64
		if err := s.readDB.QueryRow("SELECT revision,maximum_bytes FROM extension_original_budget WHERE id=1").Scan(&revision, &maximum); err != nil {
			t.Fatal(err)
		}
		if err := s.readDB.QueryRow("SELECT count(*) FROM extension_media_artifacts").Scan(&artifacts); err != nil {
			t.Fatal(err)
		}
		if revision != 1 || maximum != DefaultOriginalBudgetBytes || artifacts != 1 {
			t.Fatalf("failed metadata guard mutated state: revision%d limit%d artifacts%d", revision, maximum, artifacts)
		}
	}
	for _, bad := range []any{[]byte("0"), 0.5, int64(2), int64(-1)} {
		if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET cleanup_ack=? WHERE id=?", bad, r.ArtifactID); err != nil {
			t.Fatal(err)
		}
		assertDenied(t)
	}
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET cleanup_ack=0 WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatal(err)
	}
	defer s.writeDB.Exec("PRAGMA ignore_check_constraints=OFF")
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET budget=CAST('originals' AS BLOB) WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	assertDenied(t)
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET budget='originals' WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if got := originalBudgetTest(t, s); got.ChargedBytes != 1024 || got.Revision != 1 {
		t.Fatalf("corruption fixture restoration %#v", got)
	}
}
