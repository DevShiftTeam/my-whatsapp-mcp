package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/store/sqlstore/upgrades"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var ErrLiveRebuildBootstrap = errors.New("live_rebuild_bootstrap_unavailable")

const (
	liveRebuildPendingFile        = ".live-rebuild-pending.json"
	liveRebuildReceiptFile        = ".live-rebuild-ready.json"
	LiveRebuildBackupManifestFile = "live-rebuild-backup.json"
)

// LiveRebuildBootstrapConfig is private, offline operator input. A backup and
// external stopped-source guard must already exist; bootstrap never creates a
// backup, clears a fence, connects a device, or modifies the original profile.
type LiveRebuildBootstrapConfig struct {
	PrivateConfigFiles       []LiveRebuildPrivateConfig `json:"private_config_files,omitempty"`
	AncestorLockFDs          []int                      `json:"ancestor_lock_fds,omitempty"`
	SourceLockFD             *int                       `json:"source_lock_fd,omitempty"`
	SourceProfileDir         string                     `json:"source_profile_dir"`
	CurrentBackupDir         string                     `json:"current_backup_dir"`
	DestinationProfileDir    string                     `json:"destination_profile_dir"`
	ExpectedBackupSHA256     string                     `json:"expected_backup_sha256"`
	ExpectedAccountID        string                     `json:"expected_account_id"`
	ExpectedDeviceID         string                     `json:"expected_device_id"`
	SourceStoppedGuardPath   string                     `json:"source_stopped_guard_path"`
	SourceStoppedGuardSHA256 string                     `json:"source_stopped_guard_sha256"`
	Boundary                 time.Time                  `json:"boundary"`
	PolicyStartDate          string                     `json:"policy_start_date,omitempty"`
	PolicyYear               int                        `json:"policy_year,omitempty"`
	PolicyTimezone           string                     `json:"policy_timezone,omitempty"`
}

// Explicit external configuration is copied only into a reviewed metadata
// basename. It never authorizes copying a directory, model artifact or job.
type LiveRebuildPrivateConfig struct {
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
}

// The manifest contains digests only. Files is the exact fixed metadata
// projection, not a general-purpose directory copy or include glob.
type LiveRebuildBackupManifest struct {
	Version          int               `json:"version"`
	SourceProfileDir string            `json:"source_profile_dir"`
	Account          string            `json:"account"`
	DeviceID         string            `json:"device_id"`
	CapturedAt       time.Time         `json:"captured_at"`
	Files            map[string]string `json:"files"`
}

type LiveRebuildSourceGuard struct {
	Version               int    `json:"version"`
	SourceProfileDir      string `json:"source_profile_dir"`
	DestinationProfileDir string `json:"destination_profile_dir"`
	DeviceID              string `json:"device_id"`
	BackupSHA256          string `json:"backup_sha256"`
	SourceStopped         bool   `json:"source_stopped"`
}

type LiveRebuildBootstrapReceipt struct {
	SourceReceiptSHA256           string                 `json:"source_receipt_sha256,omitempty"`
	ConfigSHA256                  string                 `json:"config_sha256"`
	Version                       int                    `json:"version"`
	SourceProfileDir              string                 `json:"source_profile_dir"`
	DestinationProfileDir         string                 `json:"destination_profile_dir"`
	GuardSHA256                   string                 `json:"guard_sha256"`
	BackupSHA256                  string                 `json:"backup_sha256"`
	Archive                       archive.RebuildReceipt `json:"archive"`
	Files                         map[string]string      `json:"files"`
	OriginalPolicySHA256          string                 `json:"original_policy_sha256"`
	ResultPolicySHA256            string                 `json:"result_policy_sha256"`
	PolicyStartUTC                time.Time              `json:"policy_start_utc,omitempty"`
	RollingRetentionExcludesStart bool                   `json:"rolling_retention_excludes_start"`
}

func liveRebuildPolicyDate(cfg LiveRebuildBootstrapConfig) (time.Time, error) {
	if cfg.PolicyStartDate == "" && cfg.PolicyYear == 0 && cfg.PolicyTimezone == "" {
		return time.Time{}, nil
	}
	if cfg.PolicyYear < 1970 || cfg.PolicyYear > 9999 || cfg.PolicyTimezone == "" || cfg.PolicyStartDate == "" {
		return time.Time{}, ErrLiveRebuildBootstrap
	}
	zone, err := time.LoadLocation(cfg.PolicyTimezone)
	if err != nil {
		return time.Time{}, ErrLiveRebuildBootstrap
	}
	date, err := time.ParseInLocation("2006-01-02", fmt.Sprintf("%04d-%s", cfg.PolicyYear, cfg.PolicyStartDate), zone)
	if err != nil {
		return time.Time{}, ErrLiveRebuildBootstrap
	}
	return date.UTC(), nil
}

func liveRebuildDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func liveRebuildValidDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func liveRebuildReadPrivate(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !privateStateFile(before) || before.Size() > limit {
		return nil, ErrLiveRebuildBootstrap
	}
	f, err := os.OpenFile(path, os.O_RDONLY|stateNoFollow(), 0)
	if err != nil {
		return nil, ErrLiveRebuildBootstrap
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) || !stateNoExtendedACL(f) {
		return nil, ErrLiveRebuildBootstrap
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	after, ae := f.Stat()
	named, ne := os.Lstat(path)
	if err != nil || ae != nil || ne != nil || int64(len(data)) > limit || !os.SameFile(held, named) || held.Size() != after.Size() || !held.ModTime().Equal(after.ModTime()) {
		return nil, ErrLiveRebuildBootstrap
	}
	return data, nil
}

func liveRebuildDecode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return ErrLiveRebuildBootstrap
	}
	return nil
}

func liveRebuildReadGuard(cfg LiveRebuildBootstrapConfig) error {
	if cfg.SourceStoppedGuardPath != appdata.LiveRebuildSourceGuardPath(cfg.SourceProfileDir) || !liveRebuildValidDigest(cfg.SourceStoppedGuardSHA256) {
		return ErrLiveRebuildBootstrap
	}
	data, err := liveRebuildReadPrivate(cfg.SourceStoppedGuardPath, 4096)
	if err != nil || liveRebuildDigest(data) != cfg.SourceStoppedGuardSHA256 {
		return ErrLiveRebuildBootstrap
	}
	var guard LiveRebuildSourceGuard
	if liveRebuildDecode(data, &guard) != nil || guard.Version != 1 || !guard.SourceStopped || guard.SourceProfileDir != cfg.SourceProfileDir || guard.DestinationProfileDir != cfg.DestinationProfileDir || guard.DeviceID != cfg.ExpectedDeviceID || guard.BackupSHA256 != cfg.ExpectedBackupSHA256 {
		return ErrLiveRebuildBootstrap
	}
	return nil
}

// BootstrapLiveRebuild publishes only a fully verified destination. A failed
// attempt remains pending and cannot be resumed by ordinary app startup. The
// operator must inspect/discard that unpublished profile before a fresh attempt.
func BootstrapLiveRebuild(ctx context.Context, cfg LiveRebuildBootstrapConfig) (receipt LiveRebuildBootstrapReceipt, result error) {
	if ctx == nil || ctx.Err() != nil {
		return receipt, ErrLiveRebuildBootstrap
	}
	for _, p := range []string{cfg.SourceProfileDir, cfg.CurrentBackupDir, cfg.DestinationProfileDir} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return receipt, ErrLiveRebuildBootstrap
		}
	}
	for _, pair := range [][2]string{{cfg.SourceProfileDir, cfg.CurrentBackupDir}, {cfg.SourceProfileDir, cfg.DestinationProfileDir}, {cfg.CurrentBackupDir, cfg.DestinationProfileDir}} {
		if pair[0] == pair[1] || strings.HasPrefix(pair[0], pair[1]+string(os.PathSeparator)) || strings.HasPrefix(pair[1], pair[0]+string(os.PathSeparator)) {
			return receipt, ErrLiveRebuildBootstrap
		}
	}
	date, err := liveRebuildPolicyDate(cfg)
	if err != nil {
		return receipt, err
	}
	if !liveRebuildValidDigest(cfg.ExpectedBackupSHA256) || cfg.ExpectedAccountID == "" || cfg.ExpectedDeviceID == "" || cfg.Boundary.IsZero() || cfg.Boundary.After(time.Now()) || (!date.IsZero() && date.After(cfg.Boundary)) {
		return receipt, ErrLiveRebuildBootstrap
	}
	if err := liveRebuildReadGuard(cfg); err != nil {
		return receipt, err
	}
	identityConfig := cfg
	identityConfig.SourceLockFD = nil
	identityConfig.AncestorLockFDs = nil
	configBytes, _ := json.Marshal(identityConfig)
	if _, err := os.Lstat(cfg.DestinationProfileDir); err == nil {
		if _, err := os.Lstat(filepath.Join(cfg.DestinationProfileDir, liveRebuildPendingFile)); !errors.Is(err, os.ErrNotExist) {
			return receipt, ErrLiveRebuildBootstrap
		}
		data, err := liveRebuildReadPrivate(filepath.Join(cfg.DestinationProfileDir, liveRebuildReceiptFile), 131072)
		if err != nil || liveRebuildDecode(data, &receipt) != nil || receipt.Version != 1 || receipt.ConfigSHA256 != liveRebuildDigest(configBytes) || receipt.SourceProfileDir != cfg.SourceProfileDir || receipt.DestinationProfileDir != cfg.DestinationProfileDir {
			return LiveRebuildBootstrapReceipt{}, ErrLiveRebuildBootstrap
		}
		chain, chainErr := liveRebuildResolveChain(cfg.DestinationProfileDir)
		if chainErr != nil || chain == nil || chain.verify() != nil {
			return LiveRebuildBootstrapReceipt{}, ErrLiveRebuildBootstrap
		}
		return receipt, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return receipt, ErrLiveRebuildBootstrap
	}
	source, err := appdata.ExistingPaths(cfg.SourceProfileDir)
	if err != nil {
		return receipt, err
	}
	var sourceLease *appdata.InstanceLock
	if cfg.SourceLockFD != nil {
		sourceLease, err = appdata.AcquireInheritedExisting(source, *cfg.SourceLockFD)
	} else {
		sourceLease, err = appdata.AcquireExisting(source)
	}
	if err != nil {
		return receipt, err
	}
	defer func() { result = errors.Join(result, sourceLease.Close()) }()
	parent, parentRaw, err := liveRebuildLoadReady(source.Root)
	if err != nil || len(cfg.AncestorLockFDs) >= archive.MaxForwardAncestorProfiles {
		return receipt, ErrLiveRebuildBootstrap
	}
	var ancestorFD *int
	if len(cfg.AncestorLockFDs) == 1 {
		ancestorFD = &cfg.AncestorLockFDs[0]
	}
	ancestors, err := AcquireLiveRebuildSourceGuard(source.Root, ancestorFD)
	if err != nil {
		return receipt, err
	}
	defer func() { result = errors.Join(result, ancestors.Close()) }()
	if parent != nil && (!date.IsZero() || len(parent.Archive.Ancestors) != 0 || parent.Archive.Account != cfg.ExpectedAccountID || parent.Archive.DeviceID != cfg.ExpectedDeviceID || parent.Archive.Boundary.After(cfg.Boundary)) {
		return receipt, ErrLiveRebuildBootstrap
	}
	if ancestors != nil {
		for _, root := range ancestors.roots {
			if liveRebuildRootsOverlap(root, cfg.DestinationProfileDir) || liveRebuildRootsOverlap(root, cfg.CurrentBackupDir) {
				return receipt, ErrLiveRebuildBootstrap
			}
		}
	}
	parentDigest := ""
	if parent != nil {
		parentDigest = liveRebuildDigest(parentRaw)
	}
	backup, err := appdata.ExistingPaths(cfg.CurrentBackupDir)
	if err != nil {
		return receipt, err
	}
	backupLease, err := appdata.AcquireExisting(backup)
	if err != nil {
		return receipt, err
	}
	defer func() { result = errors.Join(result, backupLease.Close()) }()
	manifestBytes, err := liveRebuildReadPrivate(filepath.Join(backup.Root, LiveRebuildBackupManifestFile), 65536)
	if err != nil || liveRebuildDigest(manifestBytes) != cfg.ExpectedBackupSHA256 {
		return receipt, ErrLiveRebuildBootstrap
	}
	var manifest LiveRebuildBackupManifest
	if liveRebuildDecode(manifestBytes, &manifest) != nil || manifest.Version != 1 || manifest.SourceProfileDir != source.Root || manifest.Account != cfg.ExpectedAccountID || manifest.DeviceID != cfg.ExpectedDeviceID || manifest.CapturedAt.IsZero() || manifest.CapturedAt.After(cfg.Boundary) {
		return receipt, ErrLiveRebuildBootstrap
	}
	state, exists, err := readState(backup, backupLease)
	if err != nil || !exists || state.Mode != "real" || state.NativeInvalid || state.RemoteInvalid {
		return receipt, ErrLiveRebuildBootstrap
	}
	required := map[string]bool{"archive/archive.sqlite": true, "archive/credentials-denied.marker": true, "session/session.sqlite": true, "working/" + stateFile: true}
	if parent != nil {
		required[liveRebuildReceiptFile] = true
	}
	for _, r := range state.NativeClients {
		if r.State != "ready" || r.Account != cfg.ExpectedAccountID {
			return receipt, ErrLiveRebuildBootstrap
		}
		required["clients/"+r.ConfigName] = true
	}
	for _, r := range state.RemoteClients {
		if r.State != "ready" || r.Account != cfg.ExpectedAccountID {
			return receipt, ErrLiveRebuildBootstrap
		}
		required["clients/"+r.ConfigName] = true
	}
	optional := map[string]bool{"remote/" + localauth.RecordFilename: true, "admin.env": true, "working/admin.env": true, "working/runtime.env": true, "working/model-config.json": true, "working/transcription.env": true}
	external := map[string]string{}
	for _, item := range cfg.PrivateConfigFiles {
		if !optional[item.DestinationPath] || item.DestinationPath == "remote/"+localauth.RecordFilename || external[item.DestinationPath] != "" || !filepath.IsAbs(item.SourcePath) || filepath.Clean(item.SourcePath) != item.SourcePath {
			return receipt, ErrLiveRebuildBootstrap
		}
		if _, err := os.Lstat(filepath.Join(source.Root, item.DestinationPath)); !errors.Is(err, os.ErrNotExist) {
			return receipt, ErrLiveRebuildBootstrap
		}
		for part := filepath.Dir(item.SourcePath); ; part = filepath.Dir(part) {
			info, err := os.Lstat(part)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return receipt, ErrLiveRebuildBootstrap
			}
			if filepath.Dir(part) == part {
				break
			}
		}
		external[item.DestinationPath] = item.SourcePath
		required[item.DestinationPath] = true
	}
	for name := range optional {
		_, se := os.Lstat(filepath.Join(source.Root, name))
		_, be := os.Lstat(filepath.Join(backup.Root, name))
		if se == nil || be == nil {
			required[name] = true
		} else if !errors.Is(se, os.ErrNotExist) || !errors.Is(be, os.ErrNotExist) {
			return receipt, ErrLiveRebuildBootstrap
		}
	}
	if len(required) != len(manifest.Files) {
		return receipt, ErrLiveRebuildBootstrap
	}
	if required["remote/"+localauth.RecordFilename] {
		if err := appdata.WithRemoteAccess(source, sourceLease, localauth.ValidateRecordFile); err != nil {
			return receipt, ErrLiveRebuildBootstrap
		}
		if err := appdata.WithRemoteAccess(backup, backupLease, localauth.ValidateRecordFile); err != nil {
			return receipt, ErrLiveRebuildBootstrap
		}
	}
	if parent != nil && manifest.Files[liveRebuildReceiptFile] != parentDigest {
		return receipt, ErrLiveRebuildBootstrap
	}
	for name := range required {
		if !liveRebuildValidDigest(manifest.Files[name]) {
			return receipt, ErrLiveRebuildBootstrap
		}
	}
	if err := liveRebuildVerifyFiles(ctx, source.Root, backup.Root, manifest.Files, external); err != nil {
		return receipt, err
	}
	for _, p := range []string{source.Root, backup.Root} {
		for _, rel := range []string{"archive/archive.sqlite", "session/session.sqlite"} {
			if err := liveRebuildNoWAL(filepath.Join(p, rel)); err != nil {
				return receipt, err
			}
		}
	}
	if err := liveRebuildSession(ctx, filepath.Join(backup.SessionDir, "session.sqlite"), cfg.ExpectedAccountID, cfg.ExpectedDeviceID); err != nil {
		return receipt, err
	}
	// Exclusive create reserves the destination name before any sensitive copy.
	if err := os.Mkdir(cfg.DestinationProfileDir, 0700); err != nil {
		return receipt, ErrLiveRebuildBootstrap
	}
	// Prepare requires an empty new root; its generated directories contain no
	// source data and a crash before the pending marker contains no saved device.
	destination, err := appdata.Prepare(cfg.DestinationProfileDir)
	if err != nil {
		return receipt, err
	}
	if err := liveRebuildWriteNew(filepath.Join(destination.Root, liveRebuildPendingFile), []byte("{\"version\":1,\"state\":\"pending\"}\n")); err != nil {
		return receipt, err
	}
	lease, err := appdata.Acquire(destination)
	if err != nil {
		return receipt, err
	}
	defer func() { result = errors.Join(result, lease.Close()) }()
	if err := os.Remove(destination.ArchiveDir); err != nil {
		return receipt, ErrLiveRebuildBootstrap
	}
	ar, err := archive.BootstrapRebuild(ctx, backup.ArchiveDir, destination.ArchiveDir, archive.RebuildOptions{ExpectedAccount: cfg.ExpectedAccountID, ExpectedSourceSHA256: manifest.Files["archive/archive.sqlite"], DeviceID: cfg.ExpectedDeviceID, Boundary: cfg.Boundary})
	if err != nil {
		return receipt, err
	}
	expectedAncestors := []archive.RebuildProvenance(nil)
	if parent != nil {
		expectedAncestors = []archive.RebuildProvenance{parent.Archive.RebuildProvenance}
	}
	if !reflect.DeepEqual(ar.Ancestors, expectedAncestors) {
		return receipt, ErrLiveRebuildBootstrap
	}
	for name := range required {
		if name == liveRebuildReceiptFile || strings.HasPrefix(name, "archive/") {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(destination.Root, name)), 0700); err != nil {
			return receipt, ErrLiveRebuildBootstrap
		}
		if err := liveRebuildCopyPrivate(ctx, filepath.Join(backup.Root, name), filepath.Join(destination.Root, name), manifest.Files[name], liveRebuildFileLimit(name)); err != nil {
			return receipt, err
		}
	}
	store, err := archive.Open(ctx, destination.ArchiveDir, archive.Options{})
	if err != nil {
		return receipt, err
	}
	defer func() { result = errors.Join(result, store.Close()) }()
	// Reuse read-only client grant verification, never startup reconciliation:
	// reconciliation could retire records, mint replacements or modify journals.
	a := &Application{paths: destination, lease: lease, store: store, authState: state, nativeCredentialID: state.NativeCredentialID}
	if err := a.publishHistoricalClients(ctx, true); err != nil {
		return receipt, err
	}
	if err := a.publishHistoricalClients(ctx, false); err != nil {
		return receipt, err
	}
	if err := rebindLiveRebuildNative(ctx, destination, lease, state.NativeClients, filepath.Join(source.WorkingDir, "native.sock"), filepath.Join(destination.WorkingDir, "native.sock")); err != nil {
		return receipt, err
	}
	policy, err := store.Policy(ctx)
	if err != nil {
		return receipt, err
	}
	original, _ := json.Marshal(policy)
	receipt = LiveRebuildBootstrapReceipt{SourceReceiptSHA256: parentDigest, ConfigSHA256: liveRebuildDigest(configBytes), Version: 1, SourceProfileDir: source.Root, DestinationProfileDir: destination.Root, GuardSHA256: cfg.SourceStoppedGuardSHA256, BackupSHA256: cfg.ExpectedBackupSHA256, Archive: ar, Files: manifest.Files, OriginalPolicySHA256: liveRebuildDigest(original), PolicyStartUTC: date}
	if !date.IsZero() {
		policy.DateMode = archive.DateSince
		policy.ImportStart = date
		policy, err = store.SetPolicy(ctx, policy, policy.Generation)
		if err != nil {
			return receipt, err
		}
		receipt.RollingRetentionExcludesStart = policy.RollingRetention > 0 && date.Before(cfg.Boundary.Add(-policy.RollingRetention))
	}
	final, _ := json.Marshal(policy)
	receipt.ResultPolicySHA256 = liveRebuildDigest(final)
	if err := store.Close(); err != nil {
		return receipt, err
	}
	if err := liveRebuildVerifyFiles(ctx, source.Root, backup.Root, manifest.Files, external); err != nil {
		return receipt, err
	}
	if err := liveRebuildReadGuard(cfg); err != nil {
		return receipt, err
	}
	if parent != nil {
		chain, err := liveRebuildResolveChain(source.Root)
		if err != nil || chain == nil || chain.verify() != nil {
			return receipt, ErrLiveRebuildBootstrap
		}
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return receipt, ErrLiveRebuildBootstrap
	}
	if err := liveRebuildWriteNew(filepath.Join(destination.Root, liveRebuildReceiptFile), encoded); err != nil {
		return receipt, err
	}
	if err := os.Remove(filepath.Join(destination.Root, liveRebuildPendingFile)); err != nil {
		return receipt, ErrLiveRebuildBootstrap
	}
	if err := liveRebuildSyncDir(destination.Root); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// SQLite/session copies use a bounded buffer. Never read a whole saved device
// database into memory, and never carry source WAL or SHM into the destination.
func liveRebuildCopyPrivate(ctx context.Context, source, destination, expected string, limit int64) error {
	before, err := os.Lstat(source)
	if err != nil || !privateStateFile(before) || before.Size() > limit {
		return ErrLiveRebuildBootstrap
	}
	f, err := os.OpenFile(source, os.O_RDONLY|stateNoFollow(), 0)
	if err != nil {
		return ErrLiveRebuildBootstrap
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) || !stateNoExtendedACL(f) {
		return ErrLiveRebuildBootstrap
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL|stateNoFollow(), 0600)
	if err != nil {
		return ErrLiveRebuildBootstrap
	}
	h := sha256.New()
	buffer := make([]byte, 64*1024)
	var copyErr error
	for {
		if ctx.Err() != nil {
			copyErr = ctx.Err()
			break
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			if _, err := io.MultiWriter(out, h).Write(buffer[:n]); err != nil {
				copyErr = err
				break
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			copyErr = readErr
			break
		}
	}
	after, statErr := f.Stat()
	named, namedErr := os.Lstat(source)
	syncErr, closeErr := out.Sync(), out.Close()
	if copyErr != nil || statErr != nil || namedErr != nil || syncErr != nil || closeErr != nil || !os.SameFile(held, named) || held.Size() != after.Size() || !held.ModTime().Equal(after.ModTime()) || hex.EncodeToString(h.Sum(nil)) != expected {
		return ErrLiveRebuildBootstrap
	}
	return liveRebuildSyncDir(filepath.Dir(destination))
}

func liveRebuildFileLimit(name string) int64 {
	if name == "archive/archive.sqlite" || name == "session/session.sqlite" {
		return 64 << 30
	}
	return 65536
}

func liveRebuildVerifyFiles(ctx context.Context, source, backup string, files map[string]string, external map[string]string) error {
	// A crash-durable pending fence is negative authority even when the final
	// marker and SQLite bytes still match the backup. It must not be omitted by
	// the fixed file projection. Both callers retain the source and backup leases.
	if err := liveRebuildNoPendingCredentialDenial(source, backup); err != nil {
		return err
	}
	for name, expected := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, root := range []string{source, backup} {
			// Stream potentially large SQLite files; never materialize old bodies.
			path := filepath.Join(root, name)
			if root == source && external[name] != "" {
				path = external[name]
			}
			info, err := os.Lstat(path)
			if err != nil || !privateStateFile(info) || info.Size() > liveRebuildFileLimit(name) {
				return ErrLiveRebuildBootstrap
			}
			f, err := os.OpenFile(path, os.O_RDONLY|stateNoFollow(), 0)
			if err != nil {
				return ErrLiveRebuildBootstrap
			}
			held, e := f.Stat()
			if e != nil || !os.SameFile(info, held) || !stateNoExtendedACL(f) {
				f.Close()
				return ErrLiveRebuildBootstrap
			}
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			after, statErr := f.Stat()
			closeErr := f.Close()
			named, nameErr := os.Lstat(path)
			if copyErr != nil || statErr != nil || closeErr != nil || nameErr != nil || !os.SameFile(held, named) || held.Size() != after.Size() || !held.ModTime().Equal(after.ModTime()) || hex.EncodeToString(h.Sum(nil)) != expected {
				return ErrLiveRebuildBootstrap
			}
		}
	}
	return liveRebuildNoPendingCredentialDenial(source, backup)
}

func liveRebuildNoPendingCredentialDenial(roots ...string) error {
	for _, root := range roots {
		if _, err := os.Lstat(filepath.Join(root, "archive/credentials-denied.marker.pending")); !errors.Is(err, os.ErrNotExist) {
			return ErrLiveRebuildBootstrap
		}
	}
	return nil
}

func liveRebuildNoWAL(path string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		i, e := os.Lstat(path + suffix)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil || !privateStateFile(i) || i.Size() != 0 {
			return ErrLiveRebuildBootstrap
		}
	}
	return nil
}

func liveRebuildSession(ctx context.Context, path, account, deviceID string) error {
	address := url.URL{Scheme: "file", Path: path}
	address.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}, "_query_only": {"1"}}.Encode()
	db, err := sql.Open("sqlite3", address.String())
	if err != nil {
		return ErrLiveRebuildBootstrap
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	var integrity string
	if db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity) != nil || integrity != "ok" {
		return ErrLiveRebuildBootstrap
	}
	var schemaVersion, schemaRows int
	if db.QueryRowContext(ctx, "SELECT count(*),coalesce(max(version),0) FROM whatsmeow_version").Scan(&schemaRows, &schemaVersion) != nil || schemaRows != 1 || schemaVersion != len(upgrades.Table) {
		return ErrLiveRebuildBootstrap
	}
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	devices, err := container.GetAllDevices(ctx)
	if err != nil || len(devices) != 1 || devices[0].ID == nil || devices[0].ID.String() != deviceID || devices[0].ID.ToNonAD().String() != account || devices[0].Account == nil {
		return ErrLiveRebuildBootstrap
	}
	return nil
}

func liveRebuildWriteNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|stateNoFollow(), 0600)
	if err != nil {
		return ErrLiveRebuildBootstrap
	}
	_, we := f.Write(data)
	se := f.Sync()
	ce := f.Close()
	if we != nil || se != nil || ce != nil {
		return ErrLiveRebuildBootstrap
	}
	return liveRebuildSyncDir(filepath.Dir(path))
}
func liveRebuildSyncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return ErrLiveRebuildBootstrap
	}
	se := f.Sync()
	ce := f.Close()
	if se != nil || ce != nil {
		return ErrLiveRebuildBootstrap
	}
	return nil
}

type liveRebuildRebindEntry struct {
	Name         string               `json:"name"`
	CredentialID archive.CredentialID `json:"credential_id"`
	BeforeSHA256 string               `json:"before_sha256"`
}

func rebindLiveRebuildNative(ctx context.Context, paths appdata.Paths, lease *appdata.InstanceLock, records []nativeClientRecord, oldSocket, newSocket string) error {
	entries := []liveRebuildRebindEntry{}
	for _, r := range records {
		if r.State != "ready" {
			return ErrLiveRebuildBootstrap
		}
		data, e := liveRebuildReadPrivate(filepath.Join(paths.ClientDir, r.ConfigName), 4096)
		if e != nil {
			return e
		}
		entries = append(entries, liveRebuildRebindEntry{r.ConfigName, r.CredentialID, liveRebuildDigest(data)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	journal := struct {
		Version   int                      `json:"version"`
		State     string                   `json:"state"`
		OldSocket string                   `json:"old_socket"`
		NewSocket string                   `json:"new_socket"`
		Entries   []liveRebuildRebindEntry `json:"entries"`
	}{1, "pending", oldSocket, newSocket, entries}
	data, _ := json.Marshal(journal)
	pending := filepath.Join(paths.WorkingDir, "live-rebuild-native-pending.json")
	if err := liveRebuildWriteNew(pending, data); err != nil {
		return err
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := appdata.WithClients(paths, lease, func(root *os.Root) error {
			config, id, e := mcpserver.ReadCredentialFileRoot(root, entry.Name)
			if e != nil || id != entry.CredentialID || config.NativeSocketPath != oldSocket || config.Endpoint != mcpserver.NativeAdapterEndpoint {
				return ErrLiveRebuildBootstrap
			}
			before, e := liveRebuildReadPrivate(filepath.Join(paths.ClientDir, entry.Name), 4096)
			if e != nil || liveRebuildDigest(before) != entry.BeforeSHA256 {
				return ErrLiveRebuildBootstrap
			}
			if e := mcpserver.RewriteCredentialFileRoot(root, entry.Name, id, config, mcpserver.NativeEndpoint{SocketPath: newSocket}); e != nil {
				return e
			}
			after, afterID, e := mcpserver.ReadCredentialFileRoot(root, entry.Name)
			if e != nil || afterID != id || after.Token != config.Token || after.NativeSocketPath != newSocket || after.Endpoint != config.Endpoint {
				return ErrLiveRebuildBootstrap
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	journal.State = "ready"
	data, _ = json.Marshal(journal)
	return liveRebuildWriteNew(filepath.Join(paths.WorkingDir, "live-rebuild-native-ready.json"), data)
}
