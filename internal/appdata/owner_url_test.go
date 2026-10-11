//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const syntheticAttachFragment = "#attach=00112233445566778899aabbccddeeff.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func syntheticAttachURL() string {
	return "http://127.0.0.1:43210/" + syntheticAttachFragment
}

func TestOwnerURLIsPrivateAndAtomicallyReplaced(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	for _, ownerURL := range []string{syntheticAttachURL(), "http://[::1]:43211/" + syntheticAttachFragment} {
		if err := WriteOwnerURL(paths, lease, ownerURL); err != nil {
			t.Fatal(err)
		}
		assertMode(t, paths.OwnerURLFile, 0600)
		content, err := os.ReadFile(paths.OwnerURLFile)
		if err != nil || string(content) != ownerURL+"\n" {
			t.Fatal("private owner URL was not replaced")
		}
	}
	if err := RemoveOwnerURL(paths, lease); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwnerURL(paths, lease); err != nil {
		t.Fatal("removal was not idempotent")
	}
	if _, err := os.Lstat(paths.OwnerURLFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owner URL remains")
	}
	entries, err := os.ReadDir(paths.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".owner-url-") {
			t.Fatal("temporary attach material remains")
		}
	}
}

func TestOwnerURLRejectsNetworkAndUnexpectedCapabilities(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	valid := syntheticAttachURL()
	if err := WriteOwnerURL(paths, lease, valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range invalidAttachURLs() {
		if err := WriteOwnerURL(paths, lease, invalid); !errors.Is(err, ErrInvalidOwnerURL) {
			t.Fatalf("unexpected fixed error = %v", err)
		}
		content, err := os.ReadFile(paths.OwnerURLFile)
		if err != nil || string(content) != valid+"\n" {
			t.Fatal("invalid input replaced private URL")
		}
	}
}

func TestOwnerURLRejectsSymlinkAndPreservesOutsideFile(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	outside := filepath.Join(syntheticParent(t), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, paths.OwnerURLFile); err != nil {
		t.Fatal(err)
	}
	if err := WriteOwnerURL(paths, lease, syntheticAttachURL()); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("write = %v", err)
	}
	if err := RemoveOwnerURL(paths, lease); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("remove = %v", err)
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "outside" {
		t.Fatal("outside file changed")
	}
}

func TestOwnerURLRejectsReplacedLeasedRoot(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.Rename(paths.Root, paths.Root+"-original"); err != nil {
		t.Fatal(err)
	}
	replacement, err := Prepare(paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	replacementLease, err := Acquire(replacement)
	if err != nil {
		t.Fatal(err)
	}
	defer replacementLease.Close()
	valid := syntheticAttachURL()
	if err := WriteOwnerURL(replacement, replacementLease, valid); err != nil {
		t.Fatal(err)
	}
	if err := WriteOwnerURL(paths, lease, "http://127.0.0.1:43211/"+syntheticAttachFragment); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("stale write = %v", err)
	}
	if err := RemoveOwnerURL(paths, lease); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("stale removal = %v", err)
	}
	content, err := os.ReadFile(replacement.OwnerURLFile)
	if err != nil || string(content) != valid+"\n" {
		t.Fatal("replacement owner's capability changed")
	}
}

func invalidAttachURLs() []string {
	fragment := syntheticAttachFragment
	return []string{
		"http://example.com:43210/" + fragment,
		"http://192.168.0.1:43210/" + fragment,
		"http://localhost:43210/" + fragment,
		"https://127.0.0.1:43210/" + fragment,
		"http://127.0.0.1/" + fragment,
		"http://127.0.0.1:0/" + fragment,
		"http://127.0.0.1:65536/" + fragment,
		"http://127.0.0.1:043210/" + fragment,
		"http://127.0.0.1:43210/other" + fragment,
		"http://user:synthetic@127.0.0.1:43210/" + fragment,
		"http://127.0.0.1:43210/?synthetic=yes" + fragment,
		"http://127.0.0.1:43210/?" + fragment,
		"http://[::1%25lo0]:43210/" + fragment,
		syntheticAttachURL() + "&extra=yes",
		syntheticAttachURL() + "\n",
		strings.Replace(syntheticAttachURL(), "001122", "A01122", 1),
		strings.Replace(syntheticAttachURL(), "001122", "00112", 1),
		strings.Replace(syntheticAttachURL(), ".AAAA", ".%41AAA", 1),
		strings.TrimSuffix(syntheticAttachURL(), "A"),
		strings.TrimSuffix(syntheticAttachURL(), "A") + "B", // nonzero unused base64 bits
		syntheticAttachURL() + "=",
		"http://127.0.0.1:43210/#bootstrap=synthetic_legacy",
		"http://127.0.0.1:43210/#attach=",
		"http://127.0.0.1:43210/#token=synthetic",
	}
}

func TestReadOwnerURLWhileLeaseHeldAndAfterRelease(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := WriteOwnerURL(paths, lease, syntheticAttachURL()); err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("second acquire = %v", err)
	}
	for _, held := range []bool{true, false} {
		if !held {
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
		}
		got, err := ReadOwnerURL(paths)
		if err != nil || got != syntheticAttachURL() {
			t.Fatalf("private read held=%t fixed error=%v", held, err)
		}
	}
	// A stale private file is only a handoff candidate, never proof of liveness.
	assertMode(t, paths.OwnerURLFile, 0600)
}

func TestReadOwnerURLDoesNotAcquireOrInspectArchive(t *testing.T) {
	paths := syntheticPaths(t)
	if err := os.WriteFile(paths.OwnerURLFile, []byte(syntheticAttachURL()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.ArchiveDir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(syntheticParent(t), "outside-archive")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, paths.ArchiveDir); err != nil {
		t.Fatal(err)
	}
	got, err := ReadOwnerURL(paths)
	if err != nil || got != syntheticAttachURL() {
		t.Fatalf("read fixed error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Root, instanceLockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created a lock")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatal("read accessed outside archive")
	}
}

func TestReadOwnerURLRefusesUnsafeFilesAndPaths(t *testing.T) {
	for _, kind := range []string{"missing", "owner-mode", "root-mode", "marker-mode", "bad-marker", "symlink", "hardlink", "root-symlink", "tampered-path", "oversize", "legacy", "extra-line"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			content := []byte(syntheticAttachURL() + "\n")
			if kind != "missing" {
				if err := os.WriteFile(paths.OwnerURLFile, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			expected := ErrUnsafeDataPath
			switch kind {
			case "missing":
				expected = ErrDataUnavailable
			case "owner-mode":
				if err := os.Chmod(paths.OwnerURLFile, 0644); err != nil {
					t.Fatal(err)
				}
			case "root-mode":
				if err := os.Chmod(paths.Root, 0755); err != nil {
					t.Fatal(err)
				}
			case "marker-mode":
				if err := os.Chmod(filepath.Join(paths.Root, ownerMarker), 0644); err != nil {
					t.Fatal(err)
				}
			case "bad-marker":
				if err := os.WriteFile(filepath.Join(paths.Root, ownerMarker), []byte("unowned\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink", "hardlink":
				outside := filepath.Join(syntheticParent(t), "outside-owner")
				if err := os.WriteFile(outside, content, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(paths.OwnerURLFile); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink(outside, paths.OwnerURLFile); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Link(outside, paths.OwnerURLFile); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					if got, err := os.ReadFile(outside); err != nil || string(got) != string(content) {
						t.Error("outside owner material changed")
					}
				})
			case "root-symlink":
				if err := os.Rename(paths.Root, paths.Root+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(paths.Root+"-moved", paths.Root); err != nil {
					t.Fatal(err)
				}
			case "tampered-path":
				paths.OwnerURLFile = filepath.Join(paths.Root, "different")
			case "oversize":
				expected = ErrInvalidOwnerURL
				if err := os.WriteFile(paths.OwnerURLFile, []byte(strings.Repeat("A", 4097)), 0600); err != nil {
					t.Fatal(err)
				}
			case "legacy", "extra-line":
				expected = ErrInvalidOwnerURL
				invalid := "http://127.0.0.1:43210/#bootstrap=synthetic_legacy\n"
				if kind == "extra-line" {
					invalid = syntheticAttachURL() + "\nextra\n"
				}
				if err := os.WriteFile(paths.OwnerURLFile, []byte(invalid), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ReadOwnerURL(paths)
			if got != "" || !errors.Is(err, expected) {
				t.Fatalf("read fixed error = %v", err)
			}
		})
	}
}
