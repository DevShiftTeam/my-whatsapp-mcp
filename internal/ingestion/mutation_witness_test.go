package ingestion

import (
	"context"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type witnessOrderStore struct {
	*recordingStore
	path      string
	witnessed bool
}

func (s *witnessOrderStore) MarkUnsafe(ctx context.Context, account string, reason archive.UnsafeReason) error {
	_, err := os.Stat(s.path)
	s.witnessed = err == nil
	return s.recordingStore.MarkUnsafe(ctx, account, reason)
}
func TestMutationWitnessBeforeFenceNoBody(t *testing.T) {
	r := testRuntime(fixtureStore())
	r.config.DiagnosticDir, _ = filepath.EvalSymlinks(t.TempDir())
	if err := os.Chmod(r.config.DiagnosticDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.config.DiagnosticDir, "mutation-edit_protocol_missing.json")
	s := &witnessOrderStore{recordingStore: fixtureStore(), path: path}
	r.store = s
	e := liveMessage("synthetic-id", "SECRET_BODY_CANARY")
	e.IsEdit = true
	if err := r.message(context.Background(), e); err == nil {
		t.Fatal("missing protocol accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.witnessed || strings.Contains(string(data), "SECRET_BODY_CANARY") {
		t.Fatal("witness absent before fence or body leaked")
	}
	var w mutationWitness
	if json.Unmarshal(data, &w) != nil || w.Branch != "edit_protocol_missing" || w.TargetMappingVerified {
		t.Fatal("invalid witness claims")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("witness not private")
	}
}
func TestMutationWitnessFailureStillFences(t *testing.T) {
	r := testRuntime(fixtureStore())
	r.config.DiagnosticDir = filepath.Join(t.TempDir(), "absent")
	e := liveMessage("id", "body")
	e.IsEdit = true
	_ = r.message(context.Background(), e)
	if !r.Status().ReconciliationRequired {
		t.Fatal("witness failure suppressed fence")
	}
}
