//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExcludedContentNeverReachesArchiveFiles(t *testing.T) {
	cases := []struct {
		name   string
		policy ArchivePolicy
		sentAt time.Time
	}{
		{
			name:   "selective_without_selections",
			policy: ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective},
			sentAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
		},
		{
			name:   "older_than_inclusive_start",
			policy: ArchivePolicy{DateMode: DateSince, ImportStart: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), ChatMode: ChatsAll},
			sentAt: time.Date(2026, 1, 2, 23, 59, 59, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := filepath.Join(diskTestTempDir(t), "archive")
			s, err := Open(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			policy, err := s.SetPolicy(ctx, tc.policy, 0)
			if err != nil {
				t.Fatal(err)
			}
			bodyCanary := "neverpersistbody" + strings.ReplaceAll(tc.name, "_", "") + "940733"
			captionCanary := "neverpersistcaption" + strings.ReplaceAll(tc.name, "_", "") + "940733"
			event := diskTestOriginal(bodyCanary, tc.sentAt)
			event.Caption = captionCanary
			event.Checkpoint = &SyncCheckpoint{Kind: "history", ID: "synthetic-excluded-checkpoint"}
			result, err := s.ApplySourceEvent(ctx, event, policy.Generation)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != OutcomeFiltered {
				t.Fatalf("excluded event outcome = %q, want %q", result.Outcome, OutcomeFiltered)
			}
			diskTestAssertSQLiteSidecars(t, dir)
			diskTestAssertAbsentFromFiles(t, dir, bodyCanary, captionCanary)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			diskTestAssertAbsentFromFiles(t, dir, bodyCanary, captionCanary)
			reopened, err := Open(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			var storedBodies int
			if err := reopened.readDB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE body<>'' OR caption<>''").Scan(&storedBodies); err != nil {
				t.Fatal(err)
			}
			if storedBodies != 0 {
				t.Fatalf("excluded body contributes %d stored bodies", storedBodies)
			}
			diskTestAssertAbsentFromFiles(t, dir, bodyCanary, captionCanary)
		})
	}
}

func TestArchiveDirectoryAndFilePermissions(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(diskTestTempDir(t), "private-archive")
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	policy, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplySourceEvent(ctx, diskTestOriginal("synthetic permission fixture", diskTestSentAt), policy.Generation); err != nil {
		t.Fatal(err)
	}
	diskTestAssertSQLiteSidecars(t, dir)
	files := 0
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0600)
		if entry.IsDir() {
			want = 0700
		} else {
			files++
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s permissions = %04o, want %04o", filepath.Base(path), info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("archive created no on-disk files")
	}
}

func TestArchiveRejectsSymlinkDataPaths(t *testing.T) {
	ctx := context.Background()
	t.Run("directory", func(t *testing.T) {
		parent := diskTestTempDir(t)
		target := filepath.Join(parent, "target")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, "archive-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		s, err := Open(ctx, link, Options{})
		if s != nil {
			_ = s.Close()
		}
		if !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("Open symlink directory error = %v, want %v", err, ErrUnsafeDataPath)
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatal("rejected root symlink created files in its target")
		}
	})
	t.Run("directory_ancestor", func(t *testing.T) {
		parent := diskTestTempDir(t)
		target := filepath.Join(parent, "target-parent")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, "parent-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		s, err := Open(ctx, filepath.Join(link, "archive"), Options{})
		if s != nil {
			_ = s.Close()
		}
		if !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("Open symlink ancestor error = %v, want %v", err, ErrUnsafeDataPath)
		}
		if _, err := os.Stat(filepath.Join(target, "archive")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("rejected symlink ancestor created an archive: %v", err)
		}
	})
	for _, suffix := range []string{"", "-wal", "-shm"} {
		name := "database"
		if suffix != "" {
			name = strings.TrimPrefix(suffix, "-")
		}
		t.Run(name, func(t *testing.T) {
			parent := diskTestTempDir(t)
			dir := filepath.Join(parent, "archive")
			s, err := Open(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			dbPath := diskTestFindSQLiteFile(t, dir)
			linkPath := dbPath + suffix
			want := []byte("synthetic external target must stay untouched")
			if suffix == "" {
				// A valid SQLite target prevents a malformed-database failure
				// from masquerading as rejection of the symlink itself.
				want, err = os.ReadFile(dbPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(linkPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			target := filepath.Join(parent, "unrelated-private-file")
			if err := os.WriteFile(target, want, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, linkPath); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, dir, Options{})
			if reopened != nil {
				_ = reopened.Close()
			}
			if !errors.Is(err, ErrUnsafeDataPath) {
				t.Errorf("Open %s symlink error = %v, want %v", name, err, ErrUnsafeDataPath)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("opening archive modified a symlink target")
			}
		})
	}
}

func diskTestFindSQLiteFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(body, []byte("SQLite format 3\x00")) {
			return path
		}
	}
	t.Fatal("no SQLite archive file found")
	return ""
}

func diskTestTempDir(t *testing.T) string {
	t.Helper()
	// macOS's usual /var temp root is itself a symlink. Resolve the trusted
	// test root before intentionally testing the archive's no-symlink boundary.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func diskTestAssertSQLiteSidecars(t *testing.T, dir string) {
	t.Helper()
	path := diskTestFindSQLiteFile(t, dir)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if err != nil {
			t.Fatalf("archive file %s unavailable for on-disk assertion: %v", filepath.Base(path+suffix), err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			t.Errorf("archive file %s is not a populated regular file", filepath.Base(path+suffix))
		}
	}
}

func diskTestAssertAbsentFromFiles(t *testing.T, dir string, canaries ...string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, canary := range canaries {
			if bytes.Contains(body, []byte(canary)) {
				t.Errorf("excluded canary reached on-disk file %s", filepath.Base(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
