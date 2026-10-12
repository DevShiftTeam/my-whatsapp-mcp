package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

// Reflection keeps this initial regression executable against the pre-feature
// configuration, so its RED cause is missing trusted composition, not compile.
func configureHistoricalTest(t *testing.T, cfg *Config, root string, captured time.Time) {
	t.Helper()
	field := reflect.ValueOf(cfg).Elem().FieldByName("HistoricalView")
	if !field.IsValid() {
		t.Fatal("trusted historical view composition unavailable")
	}
	value := reflect.New(field.Type().Elem())
	value.Elem().FieldByName("DataDir").SetString(root)
	value.Elem().FieldByName("SnapshotID").SetString("0123456789abcdef")
	value.Elem().FieldByName("CapturedAt").Set(reflect.ValueOf(captured))
	field.Set(value)
}
func historicalLiveTestRoot(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := privateTestRoot(t)
	a, err := New(ctx, Config{DataDir: root, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: "snapshot-account", Chat: "snapshot-chat", Sender: "snapshot-sender", Message: "live"}, Kind: archive.EventOriginal, Body: "live-only canary", BodyKind: archive.MessageText, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestHistoricalViewApplicationSeparateSourceAndLifetime(t *testing.T) {
	ctx := context.Background()
	snapshot, captured := historicalFixture(t)
	live := historicalLiveTestRoot(t)
	original, err := os.ReadFile(filepath.Join(snapshot, "archive", "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: live, Offline: true}
	configureHistoricalTest(t, &cfg, snapshot, captured)
	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	api, ok := any(a).(localui.ArchiveReadApplication)
	if !ok {
		t.Fatal("owner historical read unavailable")
	}
	setup, err := a.Setup(ctx)
	if err != nil || setup.ArchiveView == nil || setup.HistoricalSnapshot != nil {
		t.Fatal("live owner lost source metadata or became read-only")
	}
	page, err := a.Search(ctx, archive.SearchRequest{Query: "historical"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatal("default source includes historical bodies")
	}
	result, err := api.ReadArchive(ctx, localui.ArchiveReadRequest{Kind: "search", Search: archive.SearchRequest{Query: "historical"}})
	if err != nil {
		t.Fatal(err)
	}
	historical := result.Value.(archive.SearchPage)
	if len(historical.Hits) != 1 || !archive.IsHistoricalHandle(string(historical.Hits[0].MessageID)) {
		result.Release()
		t.Fatal("historical source missing or unbound IDs")
	}
	if err = result.Check(ctx); err != nil {
		result.Release()
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- a.Close(ctx) }()
	select {
	case e := <-closeDone:
		result.Release()
		t.Fatalf("close released active historical result: %v", e)
	case <-time.After(25 * time.Millisecond):
	}
	result.Release()
	result.Release()
	select {
	case e := <-closeDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not join historical read")
	}
	after, err := os.ReadFile(filepath.Join(snapshot, "archive", "archive.sqlite"))
	if err != nil || sha256.Sum256(original) != sha256.Sum256(after) {
		t.Fatal("historical archive bytes changed")
	}
	// Both distinct leases are released only after the final result is discarded.
	reopened, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestHistoricalViewApplicationPolicyChangeFencesPendingRead(t *testing.T) {
	ctx := context.Background()
	snapshot, captured := historicalFixture(t)
	cfg := Config{DataDir: historicalLiveTestRoot(t), Offline: true}
	configureHistoricalTest(t, &cfg, snapshot, captured)
	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	api := any(a).(localui.ArchiveReadApplication)
	value, err := api.ReadArchive(ctx, localui.ArchiveReadRequest{Kind: "search", Search: archive.SearchRequest{Query: "historical"}})
	if err != nil {
		t.Fatal(err)
	}
	defer value.Release()
	p, err := a.store.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.DateMode = archive.DateSince
	p.ImportStart = time.Now()
	if _, err = a.store.SetPolicy(ctx, p, p.Generation); err != nil {
		t.Fatal(err)
	}
	if err = value.Check(ctx); err == nil {
		t.Fatal("historical response survived current live policy change")
	}
}
func TestHistoricalViewApplicationRejectsMissingAndSameSource(t *testing.T) {
	ctx := context.Background()
	root := historicalLiveTestRoot(t)
	cfg := Config{DataDir: root, Offline: true}
	configureHistoricalTest(t, &cfg, root, time.Now().Add(-time.Hour))
	if a, e := New(ctx, cfg); e == nil {
		a.Close(ctx)
		t.Fatal("live directory reopened as historical")
	}
	normal, e := New(ctx, Config{DataDir: root, Offline: true})
	if e != nil {
		t.Fatal(e)
	}
	defer normal.Close(ctx)
	api := any(normal).(localui.ArchiveReadApplication)
	if _, e = api.ReadArchive(ctx, localui.ArchiveReadRequest{Kind: "search"}); !errors.Is(e, archive.ErrArchiveUnavailable) {
		t.Fatal("disabled historical source fell back")
	}
}

func TestHistoricalViewLiveMethodsRejectForeignHandles(t *testing.T) {
	ctx := context.Background()
	a, e := New(ctx, Config{DataDir: historicalLiveTestRoot(t), Offline: true})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	for _, run := range []func() error{
		func() error {
			_, e := a.Search(ctx, archive.SearchRequest{Query: "canary", Cursor: "h1.0123456789abcdef.p.cursor"})
			return e
		},
		func() error {
			_, e := a.Search(ctx, archive.SearchRequest{Query: "canary", Chats: []archive.ChatID{"h1.0123456789abcdef.c.chat"}})
			return e
		},
		func() error {
			_, e := a.Context(ctx, archive.ContextRequest{MessageID: "h1.0123456789abcdef.m.message"})
			return e
		},
		func() error {
			_, e := a.Chats(ctx, archive.ChatListRequest{Cursor: "h1.0123456789abcdef.p.cursor"})
			return e
		},
		func() error {
			_, e := a.Stats(ctx, archive.StatsRequest{ChatID: "h1.0123456789abcdef.c.chat"})
			return e
		},
	} {
		if e := run(); !errors.Is(e, archive.ErrInvalidQuery) {
			t.Fatalf("foreign handle not rejected: %v", e)
		}
	}
}
func TestHistoricalViewDeleteResetsOnlyLiveAuthority(t *testing.T) {
	ctx := context.Background()
	snapshot, captured := historicalFixture(t)
	cfg := Config{DataDir: historicalLiveTestRoot(t), Offline: true}
	configureHistoricalTest(t, &cfg, snapshot, captured)
	before, e := os.ReadFile(filepath.Join(snapshot, "archive", "archive.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	a, e := New(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	if e = a.DeleteLocalData(ctx, true); e != nil {
		t.Fatal(e)
	}
	setup, e := a.Setup(ctx)
	if e != nil || setup.ArchiveView != nil || setup.Configured {
		t.Fatal("reset retained historical authority")
	}
	if _, e = a.ReadArchive(ctx, localui.ArchiveReadRequest{Kind: "status"}); !errors.Is(e, archive.ErrArchiveUnavailable) {
		t.Fatal("reset historical source remained readable")
	}
	after, e := os.ReadFile(filepath.Join(snapshot, "archive", "archive.sqlite"))
	if e != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("reset deleted or changed derivative")
	}
}

func TestHistoricalViewDerivativeMustStayOutsideEveryRetainedAncestor(t *testing.T) {
	for _, where := range []string{"original", "predecessor", "independent"} {
		t.Run(where, func(t *testing.T) {
			ctx := context.Background()
			original, predecessor, chainTip := syntheticForwardChain(t)
			guards, err := AcquireLiveRebuildSourceGuard(chainTip.Root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer guards.Close()
			live, err := New(ctx, Config{DataDir: historicalLiveTestRoot(t), Offline: true})
			if err != nil {
				t.Fatal(err)
			}
			defer live.Close(ctx)
			derivative, captured := historicalFixture(t)
			if where != "independent" {
				ancestor := original.Root
				if where == "predecessor" {
					ancestor = predecessor.Root
				}
				nested := filepath.Join(ancestor, "historical-derivative")
				if err = os.Rename(derivative, nested); err != nil {
					t.Fatal(err)
				}
				derivative = nested
			}
			inventory := func(root string) map[string][32]byte {
				got := map[string][32]byte{}
				err := filepath.WalkDir(root, func(path string, entry os.DirEntry, e error) error {
					if e != nil {
						return e
					}
					rel, e := filepath.Rel(root, path)
					if e != nil {
						return e
					}
					if entry.IsDir() {
						got[rel+"/"] = [32]byte{}
						return nil
					}
					data, e := os.ReadFile(path)
					if e != nil {
						return e
					}
					got[rel] = sha256.Sum256(data)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			beforeOriginal, beforePredecessor := inventory(original.Root), inventory(predecessor.Root)
			cfg := Config{DataDir: live.paths.Root, Offline: true}
			configureHistoricalTest(t, &cfg, derivative, captured)
			historical, err := openHistoricalView(ctx, live.paths, live.store, cfg, guards)
			if where == "independent" {
				if err != nil || historical == nil {
					t.Fatal("independent derivative refused", err)
				}
			} else if err == nil || historical != nil {
				t.Error("historical derivative entered retained ancestor namespace")
			}
			if historical != nil {
				if err := historical.close(); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(beforeOriginal, inventory(original.Root)) || !reflect.DeepEqual(beforePredecessor, inventory(predecessor.Root)) {
				t.Error("historical admission changed ancestor bytes or membership")
			}
			for _, paths := range []appdata.Paths{original, predecessor} {
				other, e := appdata.AcquireExisting(paths)
				if e == nil {
					other.Close()
					t.Fatal("historical admission released ancestor ownership")
				}
			}
		})
	}
}
