//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func aliasBudgetFixture(t *testing.T) (*Store, ArchivePolicy, SourceAlias, SourceKey, MessageID) {
	t.Helper()
	s, p, _, _ := aliasNoopFixture(t)
	a := SourceAlias{Account: "account", PhoneJID: "789@s.whatsapp.net", LID: "890@lid", Provenance: AliasConnectorMapping}
	e := event("alias-budget-source", "pending identity merge", 1, EventOriginal)
	e.Key.Chat, e.Key.Sender = a.LID, a.LID
	r := apply(t, s, e, p.Generation)
	return s, p, a, e.Key, r.MessageID
}

func blockAliasBudgetWriter(t *testing.T, s *Store) (func(), <-chan error) {
	t.Helper()
	barrier := aliasNoopBarrierCommand{entered: make(chan struct{}), release: make(chan struct{})}
	blocked := make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	t.Cleanup(release)
	go func() { _, err := s.submitExtension(context.Background(), barrier, 0); blocked <- err }()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not enter the alias budget barrier")
	}
	return release, blocked
}

func admitAliasBudgetCommand(t *testing.T, s *Store, ctx context.Context, a SourceAlias) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.ApplyAlias(ctx, a) }()
	select {
	case queued := <-s.controls:
		c, ok := queued.payload.(aliasCommand)
		if !ok || c.alias != a {
			t.Fatal("alias budget fixture admitted a different control")
		}
		s.controls <- queued
	case err := <-done:
		t.Fatalf("alias returned before authoritative writer admission: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("new alias did not reach the authoritative writer")
	}
	return done
}

func joinAliasBudgetBarrier(t *testing.T, blocked <-chan error) {
	t.Helper()
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alias writer barrier did not join")
	}
}

func TestNewAliasUsesIndependentWriterBudget(t *testing.T) {
	s, _, a, key, id := aliasBudgetFixture(t)
	s.options.ControlDeadline = 30 * time.Millisecond
	release, blocked := blockAliasBudgetWriter(t, s)
	defer release()
	done := admitAliasBudgetCommand(t, s, context.Background(), a)

	returned := false
	select {
	case err := <-done:
		returned = true
		t.Errorf("new alias inherited the short deletion-control budget: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	release()
	joinAliasBudgetBarrier(t, blocked)
	if returned {
		if err := s.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("new alias did not complete after the writer resumed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new alias exceeded its bounded writer budget")
	}
	if _, err := s.readSafety(a.Account); err != nil {
		t.Fatalf("completed alias left the archive denied: %v", err)
	}
	ref, err := s.CurrentSource(context.Background(), key)
	if err != nil || ref.Version.MessageID != id {
		t.Fatalf("identity merge did not preserve the existing source: %v", err)
	}
	var mapped int
	if err = s.readDB.QueryRow("SELECT count(*) FROM messages WHERE id=? AND account=? AND source_chat=? AND source_sender=? AND state='active'", id, a.Account, a.PhoneJID, a.PhoneJID).Scan(&mapped); err != nil || mapped != 1 {
		t.Fatalf("identity merge did not atomically canonicalize the source: %d, %v", mapped, err)
	}
}

func TestAliasWriterBudgetHonorsCallerCancellationDurably(t *testing.T) {
	s, _, a, _, _ := aliasBudgetFixture(t)
	id, err := s.BeginObservation(context.Background(), a.Account)
	if err != nil {
		t.Fatal(err)
	}
	release, blocked := blockAliasBudgetWriter(t, s)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := admitAliasBudgetCommand(t, s, ctx, a)
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("alias replaced earlier caller cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepted alias ignored caller cancellation")
	}
	if _, err = s.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("cancelled accepted alias did not deny reads: %v", err)
	}
	if _, err = os.Stat(filepath.Join(s.privateDir, "unsafe.marker")); err != nil {
		t.Fatalf("cancelled accepted alias did not retain its durable unsafe witness: %v", err)
	}
	release()
	joinAliasBudgetBarrier(t, blocked)
	if err = s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.EndObservation(context.Background(), id); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("cancelled alias allowed observation cleanup: %v", err)
	}
	dir := s.privateDir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("cancelled accepted alias became readable after restart: %v", err)
	}
}

func TestAliasWriterBudgetHonorsEarlierCallerDeadline(t *testing.T) {
	s, _, a, _, _ := aliasBudgetFixture(t)
	s.options.ControlDeadline = time.Second
	release, blocked := blockAliasBudgetWriter(t, s)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := admitAliasBudgetCommand(t, s, ctx, a)
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("alias did not retain the earlier caller deadline: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepted alias ignored the earlier caller deadline")
	}
	if _, err := s.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("deadline-expired alias did not deny reads: %v", err)
	}
	release()
	joinAliasBudgetBarrier(t, blocked)
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAliasOwnWriterDeadlineStillDeniesDurably(t *testing.T) {
	s, _, a, _, _ := aliasBudgetFixture(t)
	s.options.ControlDeadline = time.Second
	s.options.AliasDeadline = 30 * time.Millisecond
	release, blocked := blockAliasBudgetWriter(t, s)
	defer release()
	ctx := context.Background()
	done := admitAliasBudgetCommand(t, s, ctx, a)
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			t.Fatalf("alias did not retain its independent owned deadline: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("alias inherited the longer generic control deadline")
	}
	if _, err := s.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("alias own deadline did not deny reads: %v", err)
	}
	release()
	joinAliasBudgetBarrier(t, blocked)
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("alias own deadline became readable after restart: %v", err)
	}
}

func TestDeleteControlBudgetStaysShortBehindAcceptedAlias(t *testing.T) {
	s, p, a, key, _ := aliasBudgetFixture(t)
	s.options.ControlDeadline = 30 * time.Millisecond
	release, blocked := blockAliasBudgetWriter(t, s)
	defer release()
	aliasDone := admitAliasBudgetCommand(t, s, context.Background(), a)
	deletion := event(key.Message, "", 2, EventDelete)
	deletion.Key = key
	deleteDone := make(chan error, 1)
	go func() { _, err := s.ApplySourceEvent(context.Background(), deletion, p.Generation); deleteDone <- err }()
	select {
	case err := <-deleteDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("accepted deletion lost its strict control deadline: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("deletion inherited the longer alias writer budget")
	}
	if _, err := s.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("uncertain deletion did not deny reads: %v", err)
	}
	release()
	joinAliasBudgetBarrier(t, blocked)
	select {
	case err := <-aliasDone:
		if !errors.Is(err, ErrReconciliationRequired) {
			t.Fatalf("pending alias ignored the deletion safety fence: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending alias did not join after strict deletion denial")
	}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAliasDeadlineOptionsStayIndependentAndBounded(t *testing.T) {
	o, err := normalizedOptions(Options{ControlDeadline: 30 * time.Millisecond})
	if err != nil || o.ControlDeadline != 30*time.Millisecond || o.AliasDeadline != 2*time.Second {
		t.Fatalf("alias default changed the ordinary control budget: %v", err)
	}
	for _, deadline := range []time.Duration{time.Millisecond, 5 * time.Second} {
		o, err = normalizedOptions(Options{AliasDeadline: deadline})
		if err != nil || o.AliasDeadline != deadline || o.ControlDeadline != 500*time.Millisecond {
			t.Fatalf("explicit alias budget changed ordinary controls: %v", err)
		}
	}
	for _, deadline := range []time.Duration{-time.Millisecond, 5*time.Second + time.Nanosecond} {
		if _, err = normalizedOptions(Options{AliasDeadline: deadline}); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("out-of-bounds alias writer budget was accepted: %v", err)
		}
	}
}
