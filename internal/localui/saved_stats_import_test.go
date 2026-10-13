//go:build sqlite_fts5

package localui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/mattn/go-sqlite3"
)

type savedStatsImportApp struct {
	*fakeApplication
	store       *archive.Store
	scope       archive.ReadScope
	beforeCheck func()
	checkBudget time.Duration
	checks      int
	checkErr    error
}

func (a *savedStatsImportApp) ReadSavedArchive(ctx context.Context, q ArchiveReadRequest) (ArchiveReadResult, error) {
	reader, err := archive.NewSavedOwnerRead(ctx, a.store, nil, a.scope, a.store)
	if err != nil {
		return ArchiveReadResult{}, err
	}
	page, guard, err := reader.Stats(ctx, q.Stats)
	if err != nil {
		return ArchiveReadResult{}, err
	}
	return ArchiveReadResult{
		Value:         page,
		SavedMetadata: &archive.SavedViewMetadata{Source: "saved", Coverage: "partial", CurrentPhoneState: "unknown"},
		Check: func(c context.Context) error {
			a.checks++
			if a.checks == 2 && a.beforeCheck != nil {
				a.beforeCheck()
			}
			if a.checks == 2 && a.checkBudget > 0 {
				var cancel context.CancelFunc
				c, cancel = context.WithTimeout(c, a.checkBudget)
				defer cancel()
			}
			a.checkErr = reader.Check(c, guard)
			return a.checkErr
		},
		Release: func() {},
	}, nil
}

func TestSavedStatsImportResponseFence(t *testing.T) {
	// Instrument only connections to this synthetic Store. The preparation
	// barrier is reached inside CheckResponse's short SQL deadline, not during
	// request admission, aggregation, HTTP serialization, or owner auth.
	db, err := sql.Open("wa_indexer_archive_reader", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	driver := db.Driver().(*sqlite3.SQLiteDriver)
	db.Close()
	original := driver.ConnectHook
	defer func() { driver.ConnectHook = original }()

	for _, scenario := range []string{"positive", "persistent", "delete", "policy", "unsafe", "owner", "cancel", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			var stalls, interrupted atomic.Int32
			entered := make(chan struct{})
			driver.ConnectHook = func(c *sqlite3.SQLiteConn) error {
				if err := original(c); err != nil {
					return err
				}
				c.RegisterAuthorizer(func(op int, table, column, database string) int {
					if op == sqlite3.SQLITE_READ && table == "archive_meta" && column == "archive_revision" {
						if remaining := stalls.Load(); remaining > 0 && stalls.CompareAndSwap(remaining, remaining-1) {
							if interrupted.Add(1) == 1 {
								close(entered)
								// Let the concurrent writer prepare before releasing
								// this shared response fence. Its commit still uses
								// the Store's real serialized writer and safety lock.
							}
							// CheckResponse has already created its internal
							// 100ms deadline. Stall that exact SQL preparation
							// past it; no race with request-start timing.
							timer := time.NewTimer(200 * time.Millisecond)
							defer timer.Stop()
							<-timer.C
						}
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			store, err := archive.Open(context.Background(), root, archive.Options{ReadConnections: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			policy, err := store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
			if err != nil {
				t.Fatal(err)
			}
			event := archive.SourceEvent{Key: archive.SourceKey{Account: "synthetic-account", Chat: "synthetic-chat", Sender: "synthetic-sender", Message: "initial"}, Kind: archive.EventOriginal, Body: "synthetic message", BodyKind: archive.MessageText, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1}
			if _, err = store.ApplySourceEvent(context.Background(), event, policy.Generation); err != nil {
				t.Fatal(err)
			}
			a := &savedStatsImportApp{fakeApplication: &fakeApplication{}, store: store, scope: archive.ReadScope{Account: event.Key.Account, Principal: "owner", AllChats: true, PolicyGeneration: policy.Generation}}
			if scenario == "deadline" {
				a.checkBudget = 50 * time.Millisecond
			}
			server, err := New(Config{Origin: testOrigin}, a)
			if err != nil {
				t.Fatal(err)
			}
			owner := login(t, server)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mutation := make(chan error, 1)
			a.beforeCheck = func() {
				stalls.Store(1)
				if scenario == "persistent" {
					stalls.Store(2)
				}
				go func() {
					<-entered
					var e error
					switch scenario {
					case "positive", "persistent":
						fresh := event
						fresh.Key.Message = "ordinary-import"
						_, e = store.ApplySourceEvent(context.Background(), fresh, policy.Generation)
					case "delete":
						negative := event
						negative.Kind, negative.SourceRevision, negative.Body = archive.EventDelete, 2, ""
						_, e = store.ApplySourceEvent(context.Background(), negative, policy.Generation)
					case "policy":
						_, e = store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateSince, ImportStart: time.Now(), ChatMode: archive.ChatsAll}, policy.Generation)
					case "unsafe":
						e = store.MarkUnsafe(context.Background(), event.Key.Account, archive.UnsafeReason("mutation_metadata_missing"))
					case "owner":
						server.RevokeSessions()
					case "cancel":
						cancel()
					}
					mutation <- e
				}()
			}
			req := httptest.NewRequest("POST", testOrigin+"/api/stats", strings.NewReader(`{"source":"saved"}`)).WithContext(ctx)
			req.RemoteAddr = "127.0.0.1:54321"
			req.Header.Set("Origin", testOrigin)
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(owner.cookie)
			req.Header.Set("X-CSRF-Token", owner.csrf)
			rr := httptest.NewRecorder()
			server.Handler().ServeHTTP(rr, req)
			select {
			case err = <-mutation:
			case <-time.After(2 * time.Second):
				t.Fatal("concurrent mutation did not drain")
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "positive" {
				if ctx.Err() != nil || interrupted.Load() != 1 {
					t.Fatal("fixture did not isolate one internal fence timeout")
				}
				if rr.Code != 200 {
					t.Fatalf("ordinary import discarded valid Stats: HTTP %d, code=%s, reader error=%v", rr.Code, strings.TrimSpace(rr.Body.String()), a.checkErr)
				}
				var page archive.StatsSnapshot
				if json.Unmarshal(rr.Body.Bytes(), &page) != nil || page.Messages != 1 {
					t.Fatal("captured aggregate changed during delivery")
				}
				fresh := request(server, "POST", "/api/stats", `{"source":"saved"}`, &owner, nil)
				if fresh.Code != 200 || json.Unmarshal(fresh.Body.Bytes(), &page) != nil || page.Messages != 2 {
					t.Fatal("subsequent Stats did not include ordinary import")
				}
			} else {
				wantStatus, wantCode := 409, "policy_changed"
				if scenario == "persistent" {
					wantStatus, wantCode = 503, "request_interrupted"
					if ctx.Err() != nil || interrupted.Load() != 2 || !errors.Is(a.checkErr, context.DeadlineExceeded) {
						t.Fatalf("internal retry was unbounded or changed error: attempts=%d err=%v", interrupted.Load(), a.checkErr)
					}
				} else if scenario == "unsafe" {
					wantStatus, wantCode = 409, "reconciliation_required"
				} else if scenario == "owner" {
					wantStatus, wantCode = 401, "unauthorized"
				} else if scenario == "cancel" {
					wantStatus, wantCode = 503, "request_interrupted"
					if !errors.Is(a.checkErr, context.Canceled) {
						t.Fatalf("parent cancellation changed error: %v", a.checkErr)
					}
				} else if scenario == "deadline" {
					wantStatus, wantCode = 503, "request_interrupted"
					if interrupted.Load() != 1 || !errors.Is(a.checkErr, context.DeadlineExceeded) {
						t.Fatalf("expired caller budget retried or changed error: attempts=%d err=%v", interrupted.Load(), a.checkErr)
					}
				}
				if rr.Code != wantStatus || !strings.Contains(rr.Body.String(), wantCode) || strings.Contains(rr.Body.String(), "saved_view") {
					t.Fatalf("negative change escaped final check: HTTP %d %s", rr.Code, rr.Body.String())
				}
			}
		})
	}
}
