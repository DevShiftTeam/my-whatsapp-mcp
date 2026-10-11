package ingestion

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waWa6"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// openSession opens local credentials only. The runtime owns container.Close
// and makes the separate, explicit decision to connect or pair the client.
func openSession(ctx context.Context, sessionDir string) (*sqlstore.Container, *whatsmeow.Client, error) {
	if err := privateDirectory(sessionDir, true); err != nil {
		return nil, nil, err
	}
	before, err := os.Lstat(sessionDir)
	if err != nil {
		return nil, nil, archive.ErrUnsafeDataPath
	}
	root, err := os.OpenRoot(sessionDir)
	if err != nil {
		return nil, nil, archive.ErrUnsafeDataPath
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		return nil, nil, archive.ErrUnsafeDataPath
	}
	if err := checkSessionFiles(root, false); err != nil {
		return nil, nil, err
	}
	// Pre-creation gives SQLite's DB and WAL/SHM sidecars owner-only permissions
	// independent of the process umask. Never repair an existing unsafe file.
	if _, err := root.Lstat("session.sqlite"); errors.Is(err, os.ErrNotExist) {
		// Never open and close an existing SQLite inode outside SQLite itself.
		file, err := root.OpenFile("session.sqlite", os.O_CREATE|os.O_EXCL|os.O_RDWR|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, nil, archive.ErrUnsafeDataPath
		}
		info, infoErr := file.Stat()
		current, currentErr := root.Lstat("session.sqlite")
		closeErr := file.Close()
		if infoErr != nil || currentErr != nil || !privateSessionFile(info) || !os.SameFile(info, current) {
			return nil, nil, archive.ErrUnsafeDataPath
		}
		if closeErr != nil {
			return nil, nil, archive.ErrSourceUnavailable
		}
	} else if err != nil {
		return nil, nil, archive.ErrUnsafeDataPath
	}
	if err := checkSessionFiles(root, true); err != nil {
		return nil, nil, err
	}
	databaseInfo, err := root.Lstat("session.sqlite")
	if err != nil || !privateSessionFile(databaseInfo) {
		return nil, nil, archive.ErrUnsafeDataPath
	}
	address := url.URL{Scheme: "file", Path: filepath.Join(sessionDir, "session.sqlite")}
	address.RawQuery = url.Values{
		"_foreign_keys": {"on"},
		"_journal_mode": {"WAL"},
		"_busy_timeout": {"1000"},
		"_synchronous":  {"FULL"},
	}.Encode()
	db, err := sql.Open("sqlite3", address.String())
	if err != nil {
		return nil, nil, archive.ErrSourceUnavailable
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	fail := func(err error) (*sqlstore.Container, *whatsmeow.Client, error) {
		_ = container.Close()
		return nil, nil, err
	}
	if err := container.Upgrade(ctx); err != nil {
		return fail(archive.ErrSourceUnavailable)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fail(archive.ErrSourceUnavailable)
	}
	if err := privateDirectory(sessionDir, false); err != nil {
		return fail(err)
	}
	current, err := os.Lstat(sessionDir)
	if err != nil || !os.SameFile(opened, current) {
		return fail(archive.ErrUnsafeDataPath)
	}
	if err := checkSessionFiles(root, true); err != nil {
		return fail(err)
	}
	currentDatabase, err := root.Lstat("session.sqlite")
	if err != nil || !privateSessionFile(currentDatabase) || !os.SameFile(databaseInfo, currentDatabase) {
		return fail(archive.ErrUnsafeDataPath)
	}
	client := whatsmeow.NewClient(device, waLog.Noop)
	client.EnableDecryptedEventBuffer = false
	client.UseRetryMessageStore = false
	client.ManualHistorySyncDownload = true
	client.DisableManualHistorySyncReceipt = true
	client.EnableAutoReconnect = false
	client.DisableLoginAutoReconnect = true
	client.EmitAppStateEventsOnFullSync = true
	return container, client, nil
}

// configureInitialHistoryPayload snapshots the admitted date window before
// connecting. It requests at most 90 days; phone delivery remains best-effort
// and the archive's current policy still filters every received record.
func configureInitialHistoryPayload(client *whatsmeow.Client, policy archive.ArchivePolicy, now time.Time) error {
	days := uint32(90)
	if policy.DateMode == archive.DateSince {
		age := now.Sub(policy.ImportStart)
		day := 24 * time.Hour
		days = 1
		if age >= 89*day {
			days = 90
		} else if age > 0 {
			days = uint32((age+day-1)/day) + 1 // round up, plus a timezone-edge day
		}
	}
	props := proto.Clone(wmstore.DeviceProps).(*waCompanionReg.DeviceProps)
	props.RequireFullSync = proto.Bool(true)
	if props.HistorySyncConfig == nil {
		props.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{}
	}
	props.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(days)
	encoded, err := proto.Marshal(props)
	if err != nil {
		return archive.ErrSourceUnavailable
	}
	// The SDK callback runs under its socket lock. It does no archive lookup or
	// connection operation, and never mutates shared SDK device properties.
	client.GetClientPayload = func() *waWa6.ClientPayload {
		payload := client.Store.GetClientPayload()
		if payload.DevicePairingData != nil {
			payload.DevicePairingData.DeviceProps = append([]byte(nil), encoded...)
		}
		return payload // saved-session login retains the exact SDK defaults
	}
	return nil
}

// privateDirectory requires a canonical absolute path and never follows links.
// Ordinary read-only ancestors are allowed; only the selected data directory
// must be owner-only. Newly created components are private as well.
func privateDirectory(path string, create bool) error {
	if path == "" || !filepath.IsAbs(path) || path != filepath.Clean(path) || strings.IndexByte(path, 0) >= 0 {
		return archive.ErrUnsafeDataPath
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		created := false
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return archive.ErrUnsafeDataPath
			}
			created = true
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return archive.ErrUnsafeDataPath
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
			return archive.ErrUnsafeDataPath
		}
		if current == path {
			if info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Geteuid()) {
				return archive.ErrUnsafeDataPath
			}
		} else if info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			// Root-owned sticky temporary roots protect existing child names.
			return archive.ErrUnsafeDataPath
		}
		file, err := os.OpenFile(current, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return archive.ErrUnsafeDataPath
		}
		opened, statErr := file.Stat()
		var safeACL bool
		if current == path || created {
			safeACL = sessionNoExtendedACL(file)
		} else {
			safeACL = sessionSafeAncestorACL(file)
		}
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !os.SameFile(info, opened) || !safeACL {
			return archive.ErrUnsafeDataPath
		}
	}
	return nil
}

func checkSessionFiles(root *os.Root, requireDatabase bool) (result error) {
	parent, err := root.Open(".")
	if err != nil {
		return archive.ErrUnsafeDataPath
	}
	defer func() {
		if err := parent.Close(); err != nil && result == nil {
			result = archive.ErrSourceUnavailable
		}
	}()
	heldParent, err := parent.Stat()
	if err != nil || !privateSessionDirectory(heldParent) || !sessionNoExtendedACL(parent) {
		return archive.ErrUnsafeDataPath
	}
	for _, name := range []string{"session.sqlite", "session.sqlite-wal", "session.sqlite-shm"} {
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if requireDatabase && name == "session.sqlite" {
				return archive.ErrUnsafeDataPath
			}
			continue
		}
		if err != nil || !privateSessionFile(info) {
			return archive.ErrUnsafeDataPath
		}
		if !sessionNoExtendedACLAt(parent, name, info) {
			return archive.ErrUnsafeDataPath
		}
		after, afterErr := root.Lstat(name)
		if afterErr != nil || !privateSessionFile(after) || !os.SameFile(info, after) {
			return archive.ErrUnsafeDataPath
		}
	}
	currentParent, err := root.Lstat(".")
	if err != nil || !privateSessionDirectory(currentParent) || !os.SameFile(heldParent, currentParent) || !sessionNoExtendedACL(parent) {
		return archive.ErrUnsafeDataPath
	}
	return nil
}

func privateSessionDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func privateSessionFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}
