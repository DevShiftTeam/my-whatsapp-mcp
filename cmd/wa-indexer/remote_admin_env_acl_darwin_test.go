//go:build darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRemoteAdminEnvRejectsGrantingAndInheritedACL(t *testing.T) {
	for _, kind := range []string{"parent", "file", "inherited-file"} {
		t.Run(kind, func(t *testing.T) {
			path := privateAdminTestEnv(t, adminPasswordHashEnv+"="+validCLIAdminHash()+"\n")
			directory := filepath.Dir(path)
			target, grant := path, "everyone allow read,write,execute"
			if kind == "parent" {
				target = directory
			}
			if kind == "inherited-file" {
				if os.Remove(path) != nil {
					t.Fatal("could not prepare inherited ACL fixture")
				}
				target, grant = directory, grant+",file_inherit,directory_inherit"
			}
			if exec.Command("/bin/chmod", "+a", grant, target).Run() != nil {
				t.Fatal("could not prepare synthetic admin ACL fixture")
			}
			if kind == "inherited-file" {
				if os.WriteFile(path, []byte(adminPasswordHashEnv+"="+validCLIAdminHash()+"\n"), 0600) != nil {
					t.Fatal("could not create synthetic inherited admin env")
				}
				if exec.Command("/bin/chmod", "-N", directory).Run() != nil {
					t.Fatal("could not remove synthetic directory ACL")
				}
			}
			if _, err := readAdminEnvFile(path); !errors.Is(err, errAdminConfig) {
				t.Fatal("admin env with extended ACL was read")
			}
		})
	}
}
