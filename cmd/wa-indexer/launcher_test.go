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
	for _, dir := range []string{toolsDir, scriptsDir, filepath.Join(root, "scripts", "package"), filepath.Join(root, "packaging", "cloudflared-notices")} {
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
	for _, name := range []string{"LICENSE", "THIRD_PARTY_NOTICES", "packaging/cloudflared-manifest.json", "packaging/cloudflared-notices/LICENSE", "scripts/package/verify_cloudflared.py"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("synthetic packaging fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The fake packaging command admits only the existing pinned-helper call.
	// It performs no network, provider login or real helper verification.
	writeTestExecutable(t, filepath.Join(toolsDir, "python3"), `#!/bin/sh
[ "$#" = 5 ] || exit 71
[ "$1" = scripts/package/verify_cloudflared.py ] && [ "$2" = packaging/cloudflared-manifest.json ] && [ "$3" = darwin/arm64 ] && [ "$5" = --probe ] || exit 72
case "$4" in "$PWD"/bin/.cloudflared.*/cloudflared) ;; *) exit 73 ;; esac
[ -f "$1" ] && [ -f "$2" ] || exit 74
printf '#!/bin/sh\nexit 0\n' > "$4"
chmod 700 "$4"
`)
	writeTestExecutable(t, filepath.Join(toolsDir, "go"), `#!/bin/sh
[ "$CGO_ENABLED" = 1 ] || exit 62
if [ "$#" = 2 ] && [ "$1" = env ]; then
 case "$2" in GOOS) printf 'darwin\n'; exit 0 ;; GOARCH) printf 'arm64\n'; exit 0 ;; *) exit 69 ;; esac
fi
[ "$#" = 7 ] || exit 63
[ "$1" = build ] && [ "$2" = -tags ] && [ "$3" = fts5,sqlite_fts5,ts_omit_logtail ] || exit 64
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
		"PYTHON="+filepath.Join(toolsDir, "python3"),
		"WA_INDEXER_CLOUDFLARED_ASSET=",
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
