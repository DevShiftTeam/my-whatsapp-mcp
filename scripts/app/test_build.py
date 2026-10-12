"""Build script tests with a fake compiler; no Go build or native assets."""
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("build.sh")


class BuildTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="wa-build-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / "project with spaces"
        self.tools = self.root / "tools"
        self.scripts = self.root / "scripts" / "app"
        self.tools.mkdir(parents=True)
        self.scripts.mkdir(parents=True)
        shutil.copyfile(SCRIPT, self.scripts / "build.sh")
        self.record = self.root / "compiler.json"
        (self.root / "LICENSE").write_text("synthetic license")
        (self.root / "THIRD_PARTY_NOTICES").write_text("synthetic notices")
        helper_notices = self.root / "packaging/cloudflared-notices"
        helper_notices.mkdir(parents=True)
        (helper_notices / "LICENSE").write_text("synthetic helper license")
        (self.root / "packaging/cloudflared-manifest.json").write_text("{}")
        self.executable("python3", "#!" + sys.executable + "\nimport pathlib,sys\npathlib.Path(sys.argv[4]).write_text('synthetic helper')\n")
        self.executable("uname", '#!/bin/sh\nprintf "%s\\n" "$TEST_PLATFORM"\n')
        self.executable("rtk", "#!/bin/sh\nexit 99\n")
        self.executable("go", "#!" + sys.executable + "\n"
                        "import json,os,pathlib,sys\n"
                        "if sys.argv[1:2]==['env']: print({'GOOS':'linux','GOARCH':'arm64'}[sys.argv[2]]); sys.exit(0)\n"
                        "pathlib.Path(os.environ['TEST_RECORD']).write_text(json.dumps({"
                        "'args':sys.argv[1:],'cwd':os.getcwd(),'env':dict(os.environ)}))\n")
        self.env = {"PATH": str(self.tools) + ":/usr/bin:/bin",
                    "TEST_RECORD": str(self.record), "TEST_PLATFORM": "Linux",
                    "CGO_ENABLED": "0", "CGO_CFLAGS": "-O3", "CGO_CXXFLAGS": "-O1",
                    "CGO_LDFLAGS": "-L/custom", "GOTOOLCHAIN": "local"}

    def executable(self, name, source):
        path = self.tools / name
        path.write_text(source)
        path.chmod(0o700)

    def invoke(self, *args):
        return subprocess.run(["/bin/sh", str(self.scripts / "build.sh"), *args],
                              env=self.env, capture_output=True, text=True, timeout=5)

    def test_linux_uses_fts5_and_preserves_user_compiler_flags(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        record = json.loads(self.record.read_text())
        self.assertEqual(record["args"], ["build", "-tags", "fts5,sqlite_fts5,ts_omit_logtail",
                                         "-trimpath", "-o", "bin/wa-indexer",
                                         "./cmd/wa-indexer"])
        self.assertEqual(record["cwd"], str(self.root.resolve()))
        self.assertEqual(record["env"]["CGO_ENABLED"], "1")
        self.assertEqual(record["env"]["CGO_CFLAGS"], "-O3")
        self.assertEqual(record["env"]["CGO_CXXFLAGS"], "-O1")
        self.assertEqual(record["env"]["CGO_LDFLAGS"], "-L/custom")
        self.assertEqual(record["env"]["GOTOOLCHAIN"], "local")
        self.assertNotIn("MACOSX_DEPLOYMENT_TARGET", record["env"])

    def test_darwin_applies_deployment_target_only_on_mac(self):
        self.env.update(TEST_PLATFORM="Darwin", MACOSX_DEPLOYMENT_TARGET="15.0")
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        env = json.loads(self.record.read_text())["env"]
        self.assertEqual(env["MACOSX_DEPLOYMENT_TARGET"], "15.0")
        for key in ("CGO_CFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS"):
            self.assertIn("-mmacosx-version-min=15.0", env[key])

    def test_arguments_are_rejected_before_build(self):
        self.assertEqual(self.invoke("unexpected").returncode, 2)
        self.assertFalse(self.record.exists())


if __name__ == "__main__":
    unittest.main()
