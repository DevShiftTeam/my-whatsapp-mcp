"""Native build wrapper tests with fake Git and CMake, without native builds."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("build-media-native.sh")


class NativeBuildTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="wa-native-wrapper-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.source = self.root / "source"
        self.source.mkdir()
        self.output = self.root / "new output"
        self.record = self.root / "cmake.jsonl"
        self.executable("uname", '#!/bin/sh\ncase "$1" in -s) echo "$TEST_PLATFORM";; -m) echo "$TEST_ARCH";; esac\n')
        self.executable("rtk", "#!/bin/sh\nexit 99\n")
        self.executable("git", "#!/bin/sh\n"
                        '[ "$3" = rev-parse ] && echo 927cfce34f31707e17f2bff35c349632fb9e2c3a\n'
                        "exit 0\n")
        self.executable("cmake", "#!" + sys.executable + "\n"
                        "import json,os,sys\n"
                        "with open(os.environ['TEST_RECORD'],'a') as f:"
                        " f.write(json.dumps(sys.argv[1:])+'\\n')\n")
        self.env = {"PATH": str(self.tools) + ":/usr/bin:/bin",
                    "TEST_PLATFORM": "Darwin", "TEST_ARCH": "arm64",
                    "TEST_RECORD": str(self.record), "WA_MEDIA_BUILD_ADMITTED": "yes",
                    "WA_MEDIA_CMAKE": str(self.tools / "cmake")}

    def executable(self, name, source):
        path = self.tools / name
        path.write_text(source)
        path.chmod(0o700)

    def invoke(self, *args):
        return subprocess.run(["/bin/sh", str(SCRIPT), *args], env=self.env,
                              capture_output=True, text=True, timeout=5)

    def test_help_does_not_create_output_or_launch_tools(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0)
        self.assertIn("Experimental", result.stdout)
        self.assertFalse(self.output.exists())
        self.assertFalse(self.record.exists())

    def test_unsupported_platform_refuses_before_creating_output(self):
        for system, arch in (("Linux", "arm64"), ("Darwin", "x86_64")):
            self.env.update(TEST_PLATFORM=system, TEST_ARCH=arch)
            result = self.invoke("whisper.cpp", str(self.source), str(self.output))
            self.assertEqual(result.returncode, 2)
            self.assertIn("requires macOS arm64", result.stderr)
            self.assertFalse(self.output.exists())
            self.assertFalse(self.record.exists())

    def test_whisper_build_passes_cpu_options_without_wrapper_dependencies(self):
        result = self.invoke("whisper.cpp", str(self.source), str(self.output))
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [json.loads(line) for line in self.record.read_text().splitlines()]
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0][:4], ["-S", str(self.source), "-B", str(self.output / "build")])
        self.assertIn("-DGGML_METAL=OFF", calls[0])
        self.assertIn("-DWHISPER_CURL=OFF", calls[0])
        self.assertEqual(calls[1], ["--build", str(self.output / "build"), "--parallel", "2", "--target", "whisper-cli"])


if __name__ == "__main__":
    unittest.main()
