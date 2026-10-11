package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the launcher with a fake Go executable and no native/model setup.
func TestLauncherBuildsOnlyAppAndForwardsArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX developer launcher")
	}
	root := t.TempDir()
	toolsDir := filepath.Join(root, "tools")
	scriptsDir := filepath.Join(root, "scripts", "app")
	for _, dir := range []string{toolsDir, scriptsDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"build.sh", "run.sh"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "app", name))
		if err != nil {
			t.Fatal("application launcher is missing")
		}
		writeTestExecutable(t, filepath.Join(scriptsDir, name), string(data))
	}
	writeTestExecutable(t, filepath.Join(toolsDir, "go"), `#!/bin/sh
[ "$CGO_ENABLED" = 1 ] || exit 62
[ "$#" = 7 ] || exit 63
[ "$1" = build ] && [ "$2" = -tags ] && [ "$3" = fts5,sqlite_fts5 ] || exit 64
[ "$4" = -trimpath ] && [ "$5" = -o ] && [ "$6" = bin/wa-indexer ] && [ "$7" = ./cmd/wa-indexer ] || exit 65
[ ! -d .artifacts ] || exit 66
cat > bin/wa-indexer <<'BIN'
#!/bin/sh
[ "$#" = 4 ] || exit 67
[ "$1" = serve ] && [ "$2" = --demo ] && [ "$3" = --no-open ] && [ "$4" = '--data-dir=/private/launch sentinel' ] || exit 68
printf 'launcher-ok\n'
BIN
chmod 700 bin/wa-indexer
`)
	command := exec.Command("sh", filepath.Join(scriptsDir, "run.sh"), "--demo", "--no-open", "--data-dir=/private/launch sentinel")
	command.Env = append(os.Environ(),
		"PATH="+toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GO="+filepath.Join(toolsDir, "go"),
	)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "launcher-ok\n" {
		t.Fatalf("launcher did not build and run only the app: exit=%v", err)
	}
	if strings.Contains(string(output), "sentinel") {
		t.Fatal("launcher printed a private argument")
	}
}

func writeTestExecutable(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
}
