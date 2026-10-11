//go:build darwin

package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

func grantCredentialTestACL(t *testing.T, path string, inherit bool) {
	t.Helper()
	grant := "everyone allow read,write,execute"
	if inherit {
		grant += ",file_inherit,directory_inherit"
	}
	if err := exec.Command("/bin/chmod", "+a", grant, path).Run(); err != nil {
		t.Fatal("could not create synthetic credential ACL")
	}
}

func TestIssueCredentialFileRefusesGrantingRootACLBeforePersistence(t *testing.T) {
	root, dir := privateTestRoot(t)
	grantCredentialTestACL(t, dir, true)
	store := new(fileCredentialStore)
	if _, err := IssueCredentialFile(context.Background(), store, archive.CredentialCreate{}, root, "adapter.json", fileNativeEndpoint()); !errors.Is(err, ErrCredentialFile) || len(store.created) != 0 {
		t.Fatal("issued credential into ACL-bearing root")
	}
}

func TestReadCredentialFileRefusesGrantingAndInheritedACL(t *testing.T) {
	for _, kind := range []string{"root", "file", "inherited-file"} {
		t.Run(kind, func(t *testing.T) {
			_, dir := privateTestRoot(t)
			file := filepath.Join(dir, "adapter.json")
			if kind == "inherited-file" {
				grantCredentialTestACL(t, dir, true)
			}
			data, _ := json.Marshal(credentialFile{Version: 1, CredentialID: "synthetic", Endpoint: "http://127.0.0.1:9876/mcp", Token: base64.RawURLEncoding.EncodeToString(make([]byte, 32))})
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "root":
				grantCredentialTestACL(t, dir, false)
			case "file":
				grantCredentialTestACL(t, file, false)
			case "inherited-file":
				if err := exec.Command("/bin/chmod", "-N", dir).Run(); err != nil {
					t.Fatal("could not remove synthetic root ACL")
				}
			}
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("ACL fixture changed mode bits")
			}
			if _, err := ReadCredentialFile(file); !errors.Is(err, ErrCredentialFile) {
				t.Fatal("read native token through granting ACL")
			}
		})
	}
}
