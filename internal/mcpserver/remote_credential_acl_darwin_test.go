//go:build darwin

package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRemoteCredentialFileRefusesRootAndFileACL(t *testing.T) {
	for _, kind := range []string{"root", "file", "inherited-file"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := privateTestRoot(t)
			if kind != "file" {
				grantCredentialTestACL(t, dir, kind == "inherited-file")
			}
			if kind == "root" {
				store := new(remoteFileCredentialStore)
				if _, err := IssueRemoteCredentialFile(context.Background(), store, remoteFileRequest(), root, "remote.json", remoteFileEndpoint()); err != ErrCredentialFile || len(store.created) != 0 {
					t.Fatal("remote issuance accepted root ACL")
				}
			}
			path := filepath.Join(dir, "remote.json")
			if err := os.WriteFile(path, []byte(remoteFileJSON()), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				grantCredentialTestACL(t, path, false)
			}
			if kind == "inherited-file" {
				if exec.Command("/bin/chmod", "-N", dir).Run() != nil {
					t.Fatal("synthetic root ACL cleanup failed")
				}
			}
			if _, id, err := ReadRemoteCredentialFileRoot(root, "remote.json"); err != ErrCredentialFile || id != "" {
				t.Fatal("remote reader accepted extended ACL")
			}
		})
	}
}

func TestRemoteCredentialFilePartialPublicationRetainsDurabilityFailure(t *testing.T) {
	root, dir := privateTestRoot(t)
	store := &remoteFileCredentialStore{afterCreate: func() { grantCredentialTestACL(t, dir, true) }}
	credential, err := IssueRemoteCredentialFile(context.Background(), store, remoteFileRequest(), root, "remote.json", remoteFileEndpoint())
	if err != ErrCredentialCleanup || credential.ID != remoteFileFixtureID || credential.Revision != 1 || len(store.revoked) != 1 {
		t.Fatal("partial publication falsely claimed durable cleanup")
	}
	// The created inode is removed, but root ACL refusal prevents a trustworthy
	// directory durability acknowledgement. The app must keep pending recovery.
	if _, err := root.Lstat("remote.json"); !os.IsNotExist(err) {
		t.Fatal("partial remote secret file remained published")
	}
}
