package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/retrieval"
	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
)

// SnapshotConfig is trusted launch configuration, never owner HTTP input.
// The operator must establish current authorization equivalence before sealing.
type SnapshotConfig struct{ CapturedAt time.Time }

type snapshotMarker struct {
	Version                      int       `json:"version"`
	CapturedAt                   time.Time `json:"captured_at"`
	AuthorizationVerifiedAt      time.Time `json:"authorization_verified_at"`
	CurrentAuthorizationVerified bool      `json:"current_authorization_verified"`
}

func validateHistoricalSnapshot(paths appdata.Paths, cfg Config) error {
	if cfg.Snapshot == nil {
		return archive.RefuseHistoricalSnapshot(paths.ArchiveDir)
	}
	at := cfg.Snapshot.CapturedAt
	if at.IsZero() || at.After(time.Now()) || cfg.DataDir == "" || !filepath.IsAbs(cfg.DataDir) || cfg.Demo || cfg.Offline || cfg.OriginalBudgetBytes != nil || cfg.Native != nil || cfg.Media != nil || (cfg.MCP != nil && cfg.MCP.Remote != nil && cfg.MCP.Remote.Managed) {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	path := filepath.Join(paths.ArchiveDir, archive.HistoricalSnapshotMarker)
	info, err := os.Lstat(path)
	if err != nil || !privateStateFile(info) || info.Size() > 2048 {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	f, err := os.OpenFile(path, os.O_RDONLY|stateNoFollow(), 0)
	if err != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(info, held) {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	var marker snapshotMarker
	decoder := json.NewDecoder(io.LimitReader(f, 2049))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&marker); err != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || marker.Version != 1 || !marker.CapturedAt.Equal(at) || !marker.CurrentAuthorizationVerified || marker.AuthorizationVerifiedAt.Before(at) || marker.AuthorizationVerifiedAt.After(time.Now()) {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	return nil
}

func (a *Application) HistoricalSnapshotTime() time.Time {
	if a.snapshot == nil {
		return time.Time{}
	}
	return a.snapshot.CapturedAt
}
func (a *Application) snapshotMetadata() *archive.HistoricalSnapshot {
	if a.snapshot == nil {
		return nil
	}
	return &archive.HistoricalSnapshot{CapturedAt: a.snapshot.CapturedAt, ReadOnly: true, PhoneState: "unknown"}
}

func openHistoricalResources(ctx context.Context, paths appdata.Paths, store *archive.Store, cfg Config) (*resources, error) {
	r := &resources{paths: paths, instanceGuard: cfg.instanceGuard}
	service, err := retrieval.NewService(retrieval.Config{Reader: r.archiveReader(store), Candidates: store, Index: store.ActiveIndex})
	if err != nil {
		return r, err
	}
	r.retrieval = &retrievalRouter{}
	r.retrieval.set(service)
	// This probe performs only authenticated service metadata requests. It never
	// creates a voice owner, claims a job, opens audio, or downloads a model.
	if cfg.Transcription != nil {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		backend, err := transcription.New(bounded, *cfg.Transcription)
		if err == nil {
			r.snapshotTranscriptionReady = backend.Ready(bounded) == nil
			_ = backend.Close(context.Background())
		}
	}
	return r, nil
}

// Publish only already-ready, verified records. No repair, retirement, transport
// migration or journal write is permitted. Pending/orphan records fence the
// entire family until the operator prepares a newly verified copy.
func (a *Application) publishHistoricalClients(ctx context.Context, native bool) error {
	account, err := a.store.BoundAccount(ctx)
	if err != nil || account == "" {
		return errNativeRecovery
	}
	if native {
		a.nativeRegistry.replace(nil)
		a.nativeRecoveryPending = true
		if a.authState.NativeInvalid {
			return errNativeRecovery
		}
		metadata, err := a.listNative(ctx, account, "")
		if err != nil {
			return err
		}
		bindings := []mcpserver.NativeClientBinding{}
		seen := map[archive.CredentialID]bool{}
		for _, r := range a.authState.NativeClients {
			if r.Account != account || r.State != "ready" || a.verifyNativeRecord(ctx, r, metadata) != nil {
				return errNativeRecovery
			}
			seen[r.CredentialID] = true
			bindings = append(bindings, nativeBinding(r))
		}
		for _, r := range metadata {
			if !r.Revoked && !seen[r.ID] {
				return errNativeRecovery
			}
		}
		if a.nativeCredentialID != "" && !seen[a.nativeCredentialID] {
			return errNativeRecovery
		}
		a.nativeRegistry.replace(bindings)
		a.nativeRecoveryPending = false
		return nil
	}
	a.remoteRegistry.replace(nil)
	a.remoteRecoveryPending = true
	if a.authState.RemoteInvalid {
		return errRemoteRecovery
	}
	metadata, err := a.listRemote(ctx, account, "")
	if err != nil {
		return err
	}
	bindings := []mcpserver.RemoteClientBinding{}
	seen := map[archive.CredentialID]bool{}
	for _, r := range a.authState.RemoteClients {
		if r.Account != account || r.State != "ready" || a.verifyRemoteRecord(ctx, r, metadata) != nil {
			return errRemoteRecovery
		}
		seen[r.CredentialID] = true
		bindings = append(bindings, remoteBinding(r))
	}
	for _, r := range metadata {
		if !r.Revoked && !seen[r.ID] {
			return errRemoteRecovery
		}
	}
	a.remoteRegistry.replace(bindings)
	a.remoteRecoveryPending = false
	return nil
}
