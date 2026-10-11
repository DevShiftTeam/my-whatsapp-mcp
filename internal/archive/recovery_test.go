//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const diskTestAccount = "synthetic-recovery-account@local.invalid"

var diskTestSentAt = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

func diskTestOriginal(body string, sentAt time.Time) SourceEvent {
	return SourceEvent{
		Key:  SourceKey{Account: diskTestAccount, Chat: "synthetic-chat@local.invalid", Sender: "synthetic-sender@local.invalid", Message: "synthetic-message-1"},
		Kind: EventOriginal, OriginalSentAt: sentAt, OriginalDateKnown: true,
		Body: body, BodyKind: MessageText, SourceRevision: 1,
	}
}

func TestArchiveCleanReopenPreservesSourceAndSearch(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(diskTestTempDir(t), "archive")
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if policy.Generation != 1 {
		_ = s.Close()
		t.Fatalf("first policy generation = %d, want 1", policy.Generation)
	}
	event := diskTestOriginal("persistneedle synthetic original body", diskTestSentAt)
	event.Checkpoint = &SyncCheckpoint{Kind: "history", ID: "synthetic-committed-checkpoint"}
	result, err := s.ApplySourceEvent(ctx, event, 1)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if result.Outcome != OutcomeApplied || result.MessageID == "" {
		_ = s.Close()
		t.Fatalf("initial source result = %+v", result)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gotPolicy, err := reopened.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotPolicy.Generation != 1 || gotPolicy.DateMode != DateAllAvailable || gotPolicy.ChatMode != ChatsAll {
		t.Fatalf("reopened policy = %+v", gotPolicy)
	}
	metadata, err := reopened.SourceMetadata(ctx, event.Key)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version.MessageID != result.MessageID || metadata.Version.Revision != result.Version.Revision || !metadata.OriginalSentAt.Equal(event.OriginalSentAt) {
		t.Fatalf("reopened source metadata = %+v, committed result = %+v", metadata, result)
	}
	if _, err := reopened.readSafety(diskTestAccount); err != nil {
		t.Fatal(err)
	}
	state := diskTestReadState(t, reopened.readDB)
	if state.body != event.Body || state.archiveRevision != uint64(result.ArchiveRevision) || state.checkpointID != event.Checkpoint.ID {
		t.Fatalf("reopened disk state = %+v", state)
	}
	if got := diskTestFTSMatches(t, reopened.readDB, "persistneedle"); got != 1 {
		t.Fatalf("reopened FTS matches = %d, want 1", got)
	}
}

func TestArchiveSIGKILLRollsBackMessageFTSCheckpointAndDirtyWindows(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(diskTestTempDir(t), "archive")
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	event := diskTestOriginal("rollbackneedle committed original body", diskTestSentAt)
	event.Checkpoint = &SyncCheckpoint{Kind: "history", ID: "synthetic-before-transaction"}
	result, err := s.ApplySourceEvent(ctx, event, policy.Generation)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	before := diskTestReadState(t, s.readDB)
	if len(before.dirty) == 0 || before.checkpointID != event.Checkpoint.ID {
		_ = s.Close()
		t.Fatalf("fixture lacks committed dirty range/checkpoint: %+v", before)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	diskTestKillHelper(t, dir, "transaction")
	reopened, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := diskTestReadState(t, reopened.readDB)
	if !reflect.DeepEqual(after, before) {
		t.Errorf("SIGKILL changed committed state\nbefore: %+v\nafter:  %+v", before, after)
	}
	metadata, err := reopened.SourceMetadata(ctx, event.Key)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version.MessageID != result.MessageID || metadata.Version.Revision != result.Version.Revision {
		t.Errorf("SIGKILL changed source version: %+v", metadata.Version)
	}
	if got := diskTestFTSMatches(t, reopened.readDB, "rollbackneedle"); got != 1 {
		t.Errorf("committed original FTS matches = %d, want 1", got)
	}
	if got := diskTestFTSMatches(t, reopened.readDB, "uncommittedneedle"); got != 0 {
		t.Errorf("uncommitted FTS matches = %d, want 0", got)
	}
	if _, err := reopened.readSafety(diskTestAccount); err != nil {
		t.Errorf("rolled-back transaction incorrectly made clean source unavailable: %v", err)
	}
}

type diskTestDirtyWindow struct {
	chatID                       string
	generation, from, until, rev int64
}

type diskTestState struct {
	body, caption, normalizedBody, normalizedCaption string
	localRevision                                    uint64
	archiveRevision, indexGeneration                 uint64
	checkpointKind, checkpointID                     string
	dirty                                            []diskTestDirtyWindow
}

func diskTestReadState(t *testing.T, db *sql.DB) diskTestState {
	t.Helper()
	var state diskTestState
	if err := db.QueryRow("SELECT body,caption,normalized_body,normalized_caption,local_revision FROM messages WHERE account=?", diskTestAccount).Scan(&state.body, &state.caption, &state.normalizedBody, &state.normalizedCaption, &state.localRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT archive_revision,index_generation FROM archive_meta WHERE id=1").Scan(&state.archiveRevision, &state.indexGeneration); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT checkpoint_kind,checkpoint_id FROM sync_state WHERE account=?", diskTestAccount).Scan(&state.checkpointKind, &state.checkpointID); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT chat_id,generation,from_time,until_time,archive_revision FROM dirty_windows ORDER BY chat_id,generation")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var dirty diskTestDirtyWindow
		if err := rows.Scan(&dirty.chatID, &dirty.generation, &dirty.from, &dirty.until, &dirty.rev); err != nil {
			t.Fatal(err)
		}
		state.dirty = append(state.dirty, dirty)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return state
}

func diskTestFTSMatches(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM message_fts WHERE message_fts MATCH ?", query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestArchiveObservationCrashFailsClosed(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(diskTestTempDir(t), "archive")
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	_, err = s.ApplySourceEvent(ctx, diskTestOriginal("crashneedle synthetic original body", diskTestSentAt), policy.Generation)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	diskTestKillHelper(t, dir, "observation")
	reopened, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.readSafety(diskTestAccount); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("observation crash read safety error = %v, want %v", err, ErrReconciliationRequired)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.readSafety(diskTestAccount); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("second reopen cleared crash marker: %v", err)
	}
}

func diskTestKillHelper(t *testing.T, dir, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestArchiveCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "WA_ARCHIVE_CRASH_HELPER="+mode, "WA_ARCHIVE_CRASH_DIR="+dir)
	if body, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("crash helper exited normally: %s", body)
	} else {
		if ctx.Err() != nil {
			t.Fatalf("crash helper timed out: %s", body)
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("crash helper launch failed: %v; %s", err, body)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("crash helper did not die from SIGKILL: %v; %s", err, body)
		}
		if !bytes.Contains(body, []byte("WA_ARCHIVE_CRASH_READY\n")) {
			t.Fatalf("crash helper died before its fixture was ready: %s", body)
		}
	}
}

func TestArchiveCrashHelper(t *testing.T) {
	mode := os.Getenv("WA_ARCHIVE_CRASH_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(context.Background(), os.Getenv("WA_ARCHIVE_CRASH_DIR"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if mode == "observation" {
		if _, err := s.BeginObservation(context.Background(), diskTestAccount); err != nil {
			t.Fatal(err)
		}
	} else if mode == "transaction" {
		if _, err := s.writeDB.Exec("PRAGMA cache_size=1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec("PRAGMA cache_spill=ON"); err != nil {
			t.Fatal(err)
		}
		tx, err := s.writeDB.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		// Force more dirty pages than the tiny cache can hold, then verify
		// uncommitted plaintext reached the WAL before killing the process.
		// This exercises recovery from actual on-disk uncommitted frames.
		body := strings.Repeat("uncommittedneedle transaction body ", 4096)
		result, err := tx.Exec("UPDATE messages SET body=?,caption='uncommittedcaption',normalized_body=?,normalized_caption='uncommittedcaption',local_revision=local_revision+1 WHERE account=?", body, body, diskTestAccount)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			t.Fatalf("transaction body update count %d, error %v", count, err)
		}
		statements := []string{
			"DELETE FROM message_fts WHERE rowid IN (SELECT rowid FROM messages WHERE account='" + diskTestAccount + "')",
			"INSERT INTO message_fts(rowid,normalized_body,normalized_caption) SELECT rowid,normalized_body,normalized_caption FROM messages WHERE account='" + diskTestAccount + "'",
			"UPDATE sync_state SET checkpoint_kind='history',checkpoint_id='synthetic-uncommitted-checkpoint' WHERE account='" + diskTestAccount + "'",
			"UPDATE dirty_windows SET generation=generation+7,from_time=from_time-1000,until_time=until_time+1000,archive_revision=archive_revision+100",
			"UPDATE archive_meta SET archive_revision=archive_revision+100,index_generation=index_generation+7 WHERE id=1",
		}
		for _, statement := range statements {
			result, err := tx.Exec(statement)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := result.RowsAffected(); err != nil || count == 0 {
				t.Fatalf("transaction did not modify fixture: %q, count %d, err %v", statement, count, err)
			}
		}
		wal, err := os.ReadFile(filepath.Join(os.Getenv("WA_ARCHIVE_CRASH_DIR"), "archive.sqlite-wal"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(wal, []byte("uncommittedneedle")) {
			t.Fatal("transaction did not spill uncommitted message data to the WAL")
		}
		// Deliberately hold this transaction open through SIGKILL. No Commit,
		// Rollback, or Store.Close may execute before the process dies.
	} else {
		t.Fatalf("unknown crash mode %q", mode)
	}
	if _, err := fmt.Fprintln(os.Stdout, "WA_ARCHIVE_CRASH_READY"); err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Signal delivery is asynchronous; the parent validates actual SIGKILL.
	for {
		time.Sleep(time.Second)
	}
}
