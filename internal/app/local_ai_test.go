package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

func TestLocalAIRefusalPreservesQueriesAndDesiredRevision(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	query := a.queryContext
	for _, update := range []localui.LocalAIUpdate{
		{ExpectedRevision: 2, Choice: localui.LocalAIChoice{Mode: "off"}},
		{ExpectedRevision: 1, Choice: localui.LocalAIChoice{Mode: "preview", OCRProfile: "tesseract-fast"}},
		{ExpectedRevision: 1, Choice: localui.LocalAIChoice{Mode: "accepted", OCRProfile: "tesseract-fast"}},
	} {
		if _, err := a.SetLocalAI(context.Background(), update); err == nil {
			t.Fatal("unavailable/conflicting selection succeeded")
		}
		if query.Err() != nil || a.queryContext != query {
			t.Fatal("refused selection cancelled or replaced query ownership")
		}
		state, err := a.LocalAIState(context.Background())
		if err != nil || state.Revision != 1 || state.Choice.Mode != "off" {
			t.Fatalf("refusal changed desired state: %+v %v", state, err)
		}
	}
}

type activationExecutableLease struct {
	closes int
	fail   bool
}

func (l *activationExecutableLease) Path() string { return "/private/owned-worker" }
func (l *activationExecutableLease) Close() error {
	l.closes++
	if l.fail {
		return errors.New("cleanup_pending")
	}
	return nil
}

func TestLocalAIDrainRetainsExecutableUntilBackgroundOwnerAcknowledges(t *testing.T) {
	done := make(chan struct{})
	lease := &activationExecutableLease{fail: true}
	r := &resources{runDone: done, workerExecutable: lease, workerExecutableReady: true}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := r.drainAI(ctx); err == nil {
		t.Fatal("live background owner was treated as drained")
	}
	if lease.closes != 0 || r.workerExecutable != lease || r.runDone != done {
		t.Fatal("cleanup lost a live owner")
	}
	close(done)
	if err := r.drainAI(context.Background()); err == nil {
		t.Fatal("failed executable cleanup was acknowledged")
	}
	if r.workerExecutable != lease || !r.workerExecutableReady {
		t.Fatal("failed cleanup lost executable ownership")
	}
	lease.fail = false
	if err := r.drainAI(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.finishAIDrain()
	if r.workerExecutable != nil || r.runDone != nil || lease.closes != 2 {
		t.Fatal("successful retry did not retire exact owners")
	}
}

func TestLocalAIOfflineOffSelectionAcknowledgesWithoutApplyingForever(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	state, err := a.SetLocalAI(context.Background(), localui.LocalAIUpdate{ExpectedRevision: 1, Choice: localui.LocalAIChoice{Mode: "off"}})
	if err != nil || state.Revision != 2 || state.State != "off" {
		t.Fatalf("offline off selection: %+v %v", state, err)
	}
	_, err = a.SetLocalAI(context.Background(), localui.LocalAIUpdate{ExpectedRevision: 1, Choice: localui.LocalAIChoice{Mode: "off"}})
	if !errors.Is(err, localui.ErrLocalAIConflict) {
		t.Fatalf("CAS did not reject old revision: %v", err)
	}
}

func TestBundledAILoaderDoesNotInventEligibilityForDeveloperExecutable(t *testing.T) {
	native, media, err := LoadBundledAI("/private/unbundled/wa-indexer")
	if err == nil || native != nil || media != nil {
		t.Fatal("unbundled executable became eligible")
	}
	if err := CloseBundledAI(native, media); err != nil {
		t.Fatal(err)
	}
}
