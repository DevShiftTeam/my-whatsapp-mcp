//go:build cgo && (darwin || linux)

package appdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"
)

type sqliteDMSLock struct {
	Type int16 `json:"type"`
	PID  int32 `json:"pid"`
}

// SQLite's UNIX_SHM_BASE is (22+SQLITE_SHM_NLOCK)*4; its DMS lock is
// SQLITE_SHM_NLOCK bytes later. SQLITE_SHM_NLOCK is 8 in the pinned driver.
const sqliteDMSLockOffset = 128

// A second process is required: F_GETLK does not report the caller's own
// process-wide POSIX locks. It only queries the lock and never opens SQLite,
// acquires a lock, changes file length, or reads database contents.
func TestSQLiteDMSLockProbeHelper(t *testing.T) {
	path := os.Getenv("WA_INDEXER_SQLITE_DMS_PROBE")
	if path == "" {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		os.Exit(78)
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: unix.SEEK_SET, Start: sqliteDMSLockOffset, Len: 1}
	if err := unix.FcntlFlock(file.Fd(), unix.F_GETLK, &lock); err != nil {
		file.Close()
		os.Exit(78)
	}
	if err := file.Close(); err != nil {
		os.Exit(78)
	}
	if err := json.NewEncoder(os.Stdout).Encode(sqliteDMSLock{Type: lock.Type, PID: lock.Pid}); err != nil {
		os.Exit(78)
	}
	os.Exit(0)
}

func requireSQLiteDMSLock(t *testing.T, path, stage string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteDMSLockProbeHelper$")
	cmd.Env = append(os.Environ(), "WA_INDEXER_SQLITE_DMS_PROBE="+path)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: DMS lock probe: %v", stage, err)
	}
	var lock sqliteDMSLock
	if err := json.Unmarshal(output, &lock); err != nil {
		t.Fatalf("%s: DMS lock probe response: %v", stage, err)
	}
	if lock.Type != unix.F_RDLCK || lock.PID != int32(os.Getpid()) {
		t.Fatalf("%s: SQLite DMS lock type=%d pid=%d, want shared lock held by this process", stage, lock.Type, lock.PID)
	}
}

func TestOwnedDirectoryValidationPreservesSQLiteDMSLock(t *testing.T) {
	for _, fixture := range []struct{ database, directory string }{
		{"archive", "clients"}, {"archive", "working"},
		{"session", "clients"}, {"session", "working"},
	} {
		t.Run(fixture.database+"/"+fixture.directory, func(t *testing.T) {
			paths := syntheticPaths(t)
			lease, err := Acquire(paths)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			databaseDir := paths.ArchiveDir
			if fixture.database == "session" {
				databaseDir = paths.SessionDir
			}
			path := filepath.Join(databaseDir, fixture.database+".sqlite")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=1000")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			keeper, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer keeper.Close()
			if _, err := keeper.ExecContext(ctx, "CREATE TABLE synthetic (value INTEGER); INSERT INTO synthetic VALUES (1)"); err != nil {
				t.Fatal(err)
			}
			shm := path + "-shm"
			requireSQLiteDMSLock(t, shm, "before validation")
			use := func(*os.Root) error { return nil }
			if fixture.directory == "clients" {
				err = WithClients(paths, lease, use)
			} else {
				err = WithWorking(paths, lease, use)
			}
			if err != nil {
				t.Fatal(err)
			}
			requireSQLiteDMSLock(t, shm, "after validation")
		})
	}
}
