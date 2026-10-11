//go:build darwin

package workerlife

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func namespaceTestGrantACL(t *testing.T, path string) {
	t.Helper()
	if err := exec.Command("/bin/chmod", "+a", "everyone allow read,write,execute", path).Run(); err != nil {
		t.Fatal("could not create synthetic granting ACL")
	}
}

func TestExecutableNamespaceNoExtendedACLUsesHeldDescriptor(t *testing.T) {
	file := namespaceTestSource(t, namespaceTestMachO())
	if !namespaceNoExtendedACL(file) {
		t.Fatal("private file rejected before ACL")
	}
	namespaceTestGrantACL(t, file.Name())
	if namespaceNoExtendedACL(file) {
		t.Fatal("granting ACL accepted through held descriptor")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if namespaceNoExtendedACL(file) {
		t.Fatal("closed descriptor accepted")
	}
}

func TestExecutableNamespaceAnchorACLRefusesBeforeAdmission(t *testing.T) {
	calls := 0
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { calls++; return &namespaceTestGrowth{}, nil })
	namespaceTestGrantACL(t, root)
	if lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20); lease != nil || !errors.Is(err, ErrCleanup) {
		t.Fatalf("lease=%v error=%v", lease, err)
	}
	if calls != 0 {
		t.Fatal("ACL-bearing anchor reached admission")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
	if err := exec.Command("/bin/chmod", "-N", root).Run(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutableNamespaceSourceACLRefusesBeforeAdmissionOrCopy(t *testing.T) {
	calls := 0
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { calls++; return &namespaceTestGrowth{}, nil })
	source := namespaceTestSource(t, namespaceTestMachO())
	namespaceTestGrantACL(t, source.Name())
	ns.clone = func(*os.File, *os.File, string) error { t.Fatal("ACL-bearing source reached clone"); return nil }
	ns.copy = func(context.Context, *os.File, *os.File, int64) error {
		t.Fatal("ACL-bearing source reached copy")
		return nil
	}
	if lease, err := ns.Prepare(context.Background(), source, 64<<20); lease != nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("lease=%v error=%v", lease, err)
	}
	if calls != 0 {
		t.Fatal("ACL-bearing source reached admission")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
}

func TestExecutableNamespaceCloneACLRefusesPublicationAndRetainsCleanup(t *testing.T) {
	var growth namespaceTestGrowth
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
	var clonePath string
	ns.clone = func(source, directory *os.File, name string) error {
		output, err := namespaceOpenFile(directory, name, true)
		if err != nil {
			return err
		}
		if err := namespaceCopy(context.Background(), source, output, int64(len(namespaceTestMachO()))); err != nil {
			_ = output.Close()
			return err
		}
		if err := output.Close(); err != nil {
			return err
		}
		for lease := range ns.leases {
			clonePath = filepath.Join(root, lease.name, name)
		}
		namespaceTestGrantACL(t, clonePath)
		return nil
	}
	prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if prepared == nil || !errors.Is(err, ErrCleanup) || growth.calls != 0 {
		t.Fatalf("lease=%v error=%v releases=%d", prepared, err, growth.calls)
	}
	if prepared.Path() != "" {
		t.Fatal("ACL-bearing clone published")
	}
	if _, err := os.Stat(clonePath); err != nil {
		t.Fatalf("uncertain clone removed: %v", err)
	}
	lease := prepared.(*namespaceExecutableLease)
	if lease.executable != nil {
		_ = lease.executable.Close()
	}
	_ = lease.manifestFile.Close()
	_ = lease.directory.Close()
	_ = ns.anchor.file.Close()
}

func TestExecutableNamespaceCopiedACLRefusesFinalSealAndPublication(t *testing.T) {
	var growth namespaceTestGrowth
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
	ns.clone = func(*os.File, *os.File, string) error { return errNamespaceCloneUnsupported }
	var outputPath string
	ns.copy = func(ctx context.Context, source, output *os.File, bytes int64) error {
		if err := namespaceCopy(ctx, source, output, bytes); err != nil {
			return err
		}
		for lease := range ns.leases {
			outputPath = filepath.Join(root, lease.name, "helper")
		}
		namespaceTestGrantACL(t, outputPath)
		return nil
	}
	prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if prepared == nil || !errors.Is(err, ErrCleanup) || growth.calls != 0 {
		t.Fatalf("lease=%v error=%v releases=%d", prepared, err, growth.calls)
	}
	if prepared.Path() != "" {
		t.Fatal("ACL-bearing sealed copy published")
	}
	info, err := os.Stat(outputPath)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("sealed copy=%v error=%v", info, err)
	}
	lease := prepared.(*namespaceExecutableLease)
	_ = lease.executable.Close()
	_ = lease.manifestFile.Close()
	_ = lease.directory.Close()
	_ = ns.anchor.file.Close()
}

func TestExecutableNamespaceGrantingACLRefusesPathAndCleanup(t *testing.T) {
	for _, kind := range []string{"anchor", "directory", "manifest", "helper"} {
		t.Run(kind, func(t *testing.T) {
			var growth namespaceTestGrowth
			ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
			prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			lease := prepared.(*namespaceExecutableLease)
			helper := lease.Path()
			path := helper
			switch kind {
			case "anchor":
				path = root
			case "directory":
				path = filepath.Dir(helper)
			case "manifest":
				path = filepath.Join(filepath.Dir(helper), "manifest.json")
			}
			namespaceTestGrantACL(t, path)
			if lease.Path() != "" {
				t.Fatal("ACL-bearing private image returned executable path")
			}
			if err := lease.Close(); !errors.Is(err, ErrCleanup) || growth.calls != 0 {
				t.Fatalf("Close=%v releases=%d", err, growth.calls)
			}
			if _, err := os.Stat(helper); err != nil {
				t.Fatalf("uncertain helper removed: %v", err)
			}
			// The deliberate security fence retains ownership; release only the
			// synthetic test's descriptors without claiming reservation cleanup.
			_ = lease.executable.Close()
			_ = lease.manifestFile.Close()
			_ = lease.directory.Close()
			_ = ns.anchor.file.Close()
		})
	}
}

func TestExecutableNamespaceRestartRefusesACLEmptyDirectory(t *testing.T) {
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) {
		t.Fatal("Recover admitted growth")
		return nil, ErrProtocol
	})
	path := filepath.Join(root, "workerlife-exec-0123456789abcdef0123456789abcdef")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	namespaceTestGrantACL(t, path)
	restarted := namespaceTestRestart(t, ns, root)
	if err := restarted.Recover(context.Background()); !errors.Is(err, ErrCleanup) {
		t.Fatalf("Recover=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ACL-bearing empty directory removed: %v", err)
	}
}
