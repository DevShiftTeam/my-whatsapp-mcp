package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/modelstore"
	"testing"
	"time"
)

func TestModelOperationStopCancelsButRequiresWorkerAcknowledgement(t *testing.T) {
	work, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m := &modelOperations{active: "operation", cancel: cancel, done: done}
	short, stop := context.WithCancel(context.Background())
	stop()
	if err := m.stop(short); !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown accepted outstanding model owner")
	}
	select {
	case <-work.Done():
	default:
		t.Fatal("shutdown did not request model cancellation")
	}
	m.mu.Lock()
	if !m.stopping {
		t.Fatal("shutdown did not fence new model operations")
	}
	m.mu.Unlock()
	close(done)
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestModelSnapshotIsBodyFreeAndCannotMutateOwnedState(t *testing.T) {
	a := pairedNativeTestApp(t)
	profiles, err := a.Models(context.Background())
	if err != nil || len(profiles) == 0 {
		t.Fatal(err)
	}
	id := profiles[0].ID
	verified := time.Now().UTC()
	operation := localui.ModelOperation{ID: "opaque", ProfileID: id, Kind: "verify", State: "completed", Code: "completed"}
	a.resources.modelOperations.states = map[string]localui.ModelState{id: {ProfileID: id, InstallationState: "verified", VerifiedAt: &verified, Operation: &operation}}
	snapshot, err := a.ModelState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range snapshot.Items {
		state := &snapshot.Items[i]
		if state.ProfileID == id {
			state.Operation.Code = "caller mutation"
			*state.VerifiedAt = time.Time{}
		}
	}
	state := a.resources.modelOperations.states[id]
	if state.Operation.Code != "completed" || state.VerifiedAt.IsZero() {
		t.Fatal("snapshot leaked mutable owner pointers")
	}
	if _, err = a.StartModelOperation(context.Background(), localui.ModelOperationRequest{ProfileID: "untrusted", Kind: "install"}); err == nil {
		t.Fatal("unknown catalog download admitted")
	}
	if _, err = a.StartModelOperation(context.Background(), localui.ModelOperationRequest{ProfileID: id, Kind: "activate"}); err == nil {
		t.Fatal("HTTP selected native candidate activation")
	}
}

func TestFailedIntegrityCheckInvalidatesPreviouslyVerifiedSnapshot(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{modelstore.ErrHashMismatch, "unknown"},
		{modelstore.ErrSizeMismatch, "unknown"},
		{modelstore.ErrNotInstalled, "absent"},
		{modelstore.ErrPlatform, "unavailable"},
	} {
		verified := time.Now().UTC()
		state := localui.ModelState{ProfileID: "profile", InstallationState: "verified", VerifiedAt: &verified}
		invalidateModelVerification(&state, test.err)
		if state.VerifiedAt != nil || state.InstallationState != test.want {
			t.Fatal("failed integrity check retained verified claim")
		}
	}
}
