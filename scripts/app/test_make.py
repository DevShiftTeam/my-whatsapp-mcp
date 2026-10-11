"""Formatting check tests with a fake formatter; no Go tools are invoked."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


MAKEFILE = Path(__file__).resolve().parents[2] / "Makefile"


class MakeFormatTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="wa-make-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        shutil.copyfile(MAKEFILE, self.root / "Makefile")
        self.formatter = self.root / "fake-formatter"

    def invoke(self, body):
        self.formatter.write_text("#!/bin/sh\n" + body)
        self.formatter.chmod(0o700)
        return subprocess.run(["make", "formatcheck", "GOFMT=" + str(self.formatter)],
                              cwd=self.root, env={"PATH": "/usr/bin:/bin"},
                              capture_output=True, text=True, timeout=5)

    def test_formatted_sources_pass(self):
        self.assertEqual(self.invoke("exit 0\n").returncode, 0)

    def test_unformatted_sources_fail_with_file_names(self):
        result = self.invoke("printf '%s\\n' internal/example.go\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("internal/example.go", result.stdout)

    def test_formatter_failure_is_propagated(self):
        self.assertNotEqual(self.invoke("exit 7\n").returncode, 0)


if __name__ == "__main__":
    unittest.main()
