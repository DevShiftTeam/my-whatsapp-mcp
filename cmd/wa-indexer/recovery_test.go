package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestRecoverSnapshotRequiresExplicitExistingProfile(t *testing.T) {
	const dataDir = "/private/existing owner archive"
	opts, err := parseArgs([]string{"recover-snapshot", "--data-dir", dataDir})
	if err != nil || opts.command != "recover_snapshot" || opts.dataDir != dataDir || opts.demo || opts.listen != "" {
		t.Fatal("explicit offline recovery command unavailable")
	}
	for _, args := range [][]string{
		{"recover-snapshot"},
		{"recover-snapshot", "--data-dir", "relative/archive"},
		{"recover-snapshot", "--data-dir", "/private/../other"},
		{"recover-snapshot", "--data-dir", dataDir, "--listen", "127.0.0.1:64165"},
		{"recover-snapshot", "--data-dir", dataDir, "--mcp-listen", "127.0.0.1:6968"},
		{"recover-snapshot", "--data-dir", dataDir, "--admin-env-file", "/private/admin.env"},
		{"recover-snapshot", "--data-dir", dataDir, "--transcription-env-file", "/private/voice.env"},
		{"recover-snapshot", "--data-dir", dataDir, "--originals-budget", "1GiB"},
		{"recover-snapshot", "--data-dir", dataDir, "--demo"},
		{"recover-snapshot", "--data-dir", dataDir, "--open-existing"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("offline recovery accepted unrelated or implicit settings")
		}
	}
}

func TestRecoverSnapshotMissingProfileDoesNotCreateOrRevealPath(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "private-recovery-path-SENTINEL")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"recover-snapshot", "--data-dir", path}, &stdout, &stderr); code != 1 {
		t.Fatal("missing archive did not fail as an explicit recovery operation")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("recovery created or claimed a missing profile")
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), path) || strings.Contains(stderr.String(), "SENTINEL") {
		t.Fatal("recovery exposed private inputs or emitted a success receipt")
	}
}

func TestRecoverSnapshotInvalidArgumentsStayPrivate(t *testing.T) {
	const secret = "PRIVATE-RECOVERY-SENTINEL"
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"recover-snapshot", "--account", secret}, &stdout, &stderr); code != 2 {
		t.Fatal("unexpected recovery arguments were accepted")
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), secret) {
		t.Fatal("invalid recovery arguments were exposed")
	}
}

func TestRecoverSnapshotCommandPreservesClientAndPrintsOnlyReceipt(t *testing.T) {
	ctx := context.Background()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(filepath.Join(parent, "PRIVATE-PROFILE-SENTINEL"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := appdata.WithWorking(paths, lease, func(root *os.Root) error {
		file, err := root.OpenFile("application-state.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString(`{"version":1,"mode":"real","paused":true}`)
		return errors.Join(writeErr, file.Close())
	}); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(paths.SessionDir, "session.sqlite")
	if err := os.WriteFile(sessionPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sql.Open("sqlite3", sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	_, sessionErr := session.Exec("CREATE TABLE synthetic_session(secret TEXT)")
	if err := errors.Join(sessionErr, session.Close()); err != nil {
		t.Fatal(err)
	}
	store, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	policy, err := store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	const account = "PRIVATE-ACCOUNT-SENTINEL"
	if _, err := store.ApplySourceEvent(ctx, archive.SourceEvent{
		Key:  archive.SourceKey{Account: account, Chat: "synthetic-chat", Sender: "synthetic-sender", Message: "synthetic-message"},
		Kind: archive.EventOriginal, Body: "PRIVATE-BODY-SENTINEL", BodyKind: archive.MessageText,
		OriginalSentAt: time.Now().UTC().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1,
	}, policy.Generation); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("PRIVATE-TOKEN-SENTINEL"))
	credential, err := store.CreateCredential(ctx, archive.CredentialCreate{
		Label: "Synthetic client", Principal: "client:synthetic-cli", Digest: digest,
		Policy: archive.CredentialPolicy{Account: account, AllChats: true}, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnsafe(ctx, account, "drain_failed"); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(store.Close(), lease.Close()); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"recover-snapshot", "--data-dir", paths.Root}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatal("explicit recovery command did not finish")
	}
	var receipt map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil || len(receipt) != 6 {
		t.Fatal("recovery did not print the bounded metadata receipt")
	}
	for _, key := range []string{"recovery_epoch", "archive_revision", "mode", "code", "coverage", "current_phone_state_verified"} {
		if _, ok := receipt[key]; !ok {
			t.Fatal("recovery receipt omitted fixed metadata")
		}
	}
	if string(receipt["mode"]) != `"committed_snapshot"` || string(receipt["code"]) != `"owner_restored_snapshot"` || string(receipt["coverage"]) != `"partial"` || string(receipt["current_phone_state_verified"]) != "false" || bytes.Contains(stdout.Bytes(), []byte("SENTINEL")) {
		t.Fatal("recovery receipt exposed private data or claimed verified phone state")
	}
	lease, err = appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	store, err = archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	grant, err := store.ResolveCredential(ctx, digest)
	if err != nil || grant.CredentialID != credential.ID || grant.CredentialRevision != credential.Revision {
		t.Fatal("explicit recovery command changed the saved client")
	}
}
