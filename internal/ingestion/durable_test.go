package ingestion

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const durableAccount = "100@s.whatsapp.net"

func durableStore(t *testing.T, dir string) *archive.Store {
	t.Helper()
	s, err := archive.Open(context.Background(), dir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close durable archive: %v", err)
		}
	})
	return s
}

func durableFixture(t *testing.T) (*archive.Store, string) {
	t.Helper()
	// On macOS t.TempDir can be below the /var symlink; archive roots must
	// have real directory ancestors, so resolve the fixture before opening it.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := durableStore(t, dir)
	if _, err := s.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func durableRuntime(t *testing.T, store archive.ConnectorStore) *Runtime {
	t.Helper()
	jid := types.NewJID("100", types.DefaultUserServer)
	client := whatsmeow.NewClient(&wmstore.Device{ID: &jid}, waLog.Noop)
	r := newRuntime(Config{BodyQueueCount: 8, BodyQueueBytes: 8 << 20, MediaMax: 4 << 20}, store, Callbacks{}, client, &fakeConnection{})
	t.Cleanup(func() {
		if err := r.Close(); err != nil && !errors.Is(err, archive.ErrReconciliationRequired) {
			t.Errorf("close durable runtime: %v", err)
		}
	})
	return r
}

func durableSearch(t *testing.T, s *archive.Store, query string) (archive.SearchPage, error) {
	t.Helper()
	p, err := s.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s.Search(context.Background(), archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: p.Generation}, archive.SearchRequest{Query: query, Limit: 20})
}

func durableAssertAbsent(t *testing.T, dir string, canaries ...string) {
	t.Helper()
	for i, name := range []string{"archive.sqlite", "archive.sqlite-wal", "archive.sqlite-shm"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if i > 0 && errors.Is(err, os.ErrNotExist) {
			// SQLite removes sidecars after a clean close. Every sidecar still
			// present is inspected, including while the writer remains open.
			continue
		}
		if err != nil {
			t.Fatalf("read %s for plaintext audit: %v", name, err)
		}
		for _, canary := range canaries {
			if bytes.Contains(data, []byte(canary)) {
				t.Errorf("excluded plaintext %q reached %s", canary, name)
			}
		}
	}
}

func TestDurableLiveHistoryDuplicatesKeepOneStableResult(t *testing.T) {
	s, _ := durableFixture(t)
	r := durableRuntime(t, s)
	ctx := context.Background()
	if err := r.message(ctx, liveMessage("overlap", "durableoverlap searchable receipt")); err != nil {
		t.Fatal(err)
	}
	before, err := durableSearch(t, s, "durableoverlap")
	if err != nil || len(before.Hits) != 1 {
		t.Fatalf("initial search = %+v, error %v", before.Hits, err)
	}
	for i := 0; i < 2; i++ {
		if err := r.message(ctx, liveMessage("overlap", "durableoverlap searchable receipt")); err != nil {
			t.Fatal(err)
		}
		if err := r.projectHistory(ctx, historyWire(historyRecord("overlap", "durableoverlap searchable receipt"))); err != nil {
			t.Fatal(err)
		}
	}
	after, err := durableSearch(t, s, "durableoverlap")
	if err != nil || len(after.Hits) != 1 {
		t.Fatalf("live/history replay search = %+v, error %v", after.Hits, err)
	}
	if after.Hits[0].MessageID != before.Hits[0].MessageID {
		t.Fatalf("live/history overlap replaced source identity: %s -> %s", before.Hits[0].MessageID, after.Hits[0].MessageID)
	}
	if after.Hits[0].Snippet != "durableoverlap searchable receipt" {
		t.Fatalf("live/history overlap changed body: %q", after.Hits[0].Snippet)
	}
}

func TestDurableTerminalDeleteRejectsStaleBackfillAfterReopen(t *testing.T) {
	s, dir := durableFixture(t)
	r := durableRuntime(t, s)
	ctx := context.Background()
	if err := r.message(ctx, liveMessage("deleted", "durabledeleted original content")); err != nil {
		t.Fatal(err)
	}
	if err := r.message(ctx, mutation("deleted", waE2E.ProtocolMessage_REVOKE)); err != nil {
		t.Fatal(err)
	}
	stale := historyWire(historyRecord("deleted", "durabledeleted original content"))
	if err := r.projectHistory(ctx, stale); err != nil {
		t.Fatal(err)
	}
	page, err := durableSearch(t, s, "durabledeleted")
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("deleted content revived before reopen: %+v, %v", page.Hits, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	replayed := durableRuntime(t, reopened)
	if err := replayed.projectHistory(ctx, stale); err != nil {
		t.Fatal(err)
	}
	page, err = durableSearch(t, reopened, "durabledeleted")
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("stale history revived durable terminal delete: %+v, %v", page.Hits, err)
	}
	metadata, err := reopened.SourceMetadata(ctx, archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "deleted"})
	if err != nil || !metadata.Version.Deleted {
		t.Fatalf("terminal source state was lost on reopen: %+v, %v", metadata, err)
	}
}

func TestDurableInitialExclusionsNeverPersistPlaintext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy archive.ArchivePolicy
	}{
		{"date", archive.ArchivePolicy{DateMode: archive.DateSince, ImportStart: time.Unix(1100, 0), ChatMode: archive.ChatsAll}},
		{"selective", archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := durableFixture(t)
			current, err := s.Policy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetPolicy(context.Background(), tc.policy, current.Generation); err != nil {
				t.Fatal(err)
			}
			r := durableRuntime(t, s)
			body := "durableexcludedbody" + tc.name
			caption := "durableexcludedcaption" + tc.name
			if err := r.message(context.Background(), liveMessage("excluded-live", body)); err != nil {
				t.Fatal(err)
			}
			image := historyRecord("excluded-history", "")
			image.Message.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String(caption)}}
			if err := r.projectHistory(context.Background(), historyWire(image)); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{body, caption} {
				page, err := durableSearch(t, s, query)
				if err != nil || len(page.Hits) != 0 {
					t.Fatalf("excluded content searchable: %+v, %v", page.Hits, err)
				}
			}
			durableAssertAbsent(t, dir, body, caption)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			durableAssertAbsent(t, dir, body, caption)
			reopened := durableStore(t, dir)
			for _, query := range []string{body, caption} {
				page, err := durableSearch(t, reopened, query)
				if err != nil || len(page.Hits) != 0 {
					t.Fatalf("excluded content returned after reopen: %+v, %v", page.Hits, err)
				}
			}
			for _, id := range []string{"excluded-live", "excluded-history"} {
				metadata, err := reopened.SourceMetadata(context.Background(), archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: id})
				if err != nil || !metadata.OriginalDateKnown || !metadata.OriginalSentAt.Equal(time.Unix(1000, 0)) {
					t.Fatalf("excluded source anchor lost after reopen: %+v, %v", metadata, err)
				}
			}
			durableAssertAbsent(t, dir, body, caption)
		})
	}
}

// The policy change happens after projection/admission and immediately before
// the real writer sees the original generation. This is a genuine stale write
// rejection, not a fake store that simply records whatever the runtime passes.
type durablePolicyRaceStore struct {
	archive.ConnectorStore
	narrow   archive.ArchivePolicy
	attempts []archive.SourceEvent
	gens     []archive.Generation
	changed  archive.ArchivePolicy
	firstErr error
}

func (s *durablePolicyRaceStore) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	s.attempts = append(s.attempts, e)
	s.gens = append(s.gens, generation)
	first := len(s.attempts) == 1
	if first {
		var err error
		s.changed, err = s.SetPolicy(ctx, s.narrow, generation)
		if err != nil {
			return archive.ApplyResult{}, err
		}
	}
	result, err := s.ConnectorStore.ApplySourceEvent(ctx, e, generation)
	if first {
		s.firstErr = err
	}
	return result, err
}

func TestDurablePolicyRaceRetriesWithoutExcludedPlaintext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy archive.ArchivePolicy
	}{
		{"date", archive.ArchivePolicy{DateMode: archive.DateSince, ImportStart: time.Unix(1100, 0), ChatMode: archive.ChatsAll}},
		{"selective", archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := durableFixture(t)
			wrapper := &durablePolicyRaceStore{ConnectorStore: s, narrow: tc.policy}
			r := durableRuntime(t, wrapper)
			canary := "durableraceexcluded" + tc.name
			if err := r.message(context.Background(), liveMessage("raced", canary)); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(wrapper.firstErr, archive.ErrPolicyChanged) {
				t.Fatalf("real stale-generation rejection = %v", wrapper.firstErr)
			}
			if len(wrapper.attempts) != 2 || wrapper.attempts[0].Body != canary {
				t.Fatalf("policy race did not exercise projected body plus retry: %+v", wrapper.attempts)
			}
			retry := wrapper.attempts[1]
			if retry.Body != "" || retry.Caption != "" || len(retry.Media) != 0 || retry.ReplyTo != nil || wrapper.gens[1] != wrapper.changed.Generation || wrapper.gens[0] == wrapper.gens[1] {
				t.Fatalf("retry retained excluded content or stale generation: %+v, generations %v", retry, wrapper.gens)
			}
			if !retry.OriginalDateKnown || !retry.OriginalSentAt.Equal(time.Unix(1000, 0)) || retry.Key.Message != "raced" {
				t.Fatalf("bodyless retry lost source anchor: %+v", retry)
			}
			page, err := durableSearch(t, s, canary)
			if err != nil || len(page.Hits) != 0 {
				t.Fatalf("raced excluded content searchable: %+v, %v", page.Hits, err)
			}
			durableAssertAbsent(t, dir, canary)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			durableAssertAbsent(t, dir, canary)
			reopened := durableStore(t, dir)
			page, err = durableSearch(t, reopened, canary)
			if err != nil || len(page.Hits) != 0 {
				t.Fatalf("raced excluded content returned on reopen: %+v, %v", page.Hits, err)
			}
			metadata, err := reopened.SourceMetadata(context.Background(), retry.Key)
			if err != nil || !metadata.OriginalDateKnown || !metadata.OriginalSentAt.Equal(retry.OriginalSentAt) {
				t.Fatalf("bodyless retry source anchor lost after reopen: %+v, %v", metadata, err)
			}
			durableAssertAbsent(t, dir, canary)
		})
	}
}

func TestDurableControlCoverageFailureImmediatelyDeniesAndSurvivesReopen(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*Runtime)
	}{
		{"archive_unavailable", func(r *Runtime) { r.failClosed("archive_unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := durableFixture(t)
			r := durableRuntime(t, s)
			ctx := context.Background()
			if err := r.message(ctx, liveMessage("permitted", "durablesafety permitted receipt")); err != nil {
				t.Fatal(err)
			}
			if err := r.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			observationPath := filepath.Join(dir, "observation.marker")
			observation, err := os.ReadFile(observationPath)
			if err != nil || len(observation) == 0 {
				t.Fatalf("connect did not persist observation: %q, %v", observation, err)
			}
			tc.fail(r)
			if _, err := durableSearch(t, s, "durablesafety"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Errorf("search remained readable immediately after control coverage failure: %v", err)
			}
			if !r.Status().ReconciliationRequired {
				t.Error("control coverage failure did not deny runtime")
			}
			if _, err := os.Stat(filepath.Join(dir, "unsafe.marker")); err != nil {
				t.Errorf("control coverage failure did not persist unsafe marker: %v", err)
			}
			if err := r.Disconnect(ctx); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Errorf("unsafe disconnect = %v", err)
			}
			retained, err := os.ReadFile(observationPath)
			if err != nil || !bytes.Equal(retained, observation) {
				t.Errorf("unsafe disconnect cleared/replaced observation: %q, %v", retained, err)
			}
			if err := r.Connect(ctx); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Errorf("unsafe runtime restarted: %v", err)
			}
			if err := r.Close(); err != nil && !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := durableStore(t, dir)
			if _, err := durableSearch(t, reopened, "durablesafety"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Errorf("reopen cleared durable safety denial: %v", err)
			}
			fresh := durableRuntime(t, reopened)
			if err := fresh.Connect(ctx); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Errorf("new runtime connected with retained unsafe marker: %v", err)
			}
			retained, err = os.ReadFile(observationPath)
			if err != nil || !bytes.Equal(retained, observation) {
				t.Errorf("reopen/restart cleared original observation: %q, %v", retained, err)
			}
		})
	}
}
