"""Hebrew installer checks with the real setup.py and synthetic weights.

Provisioning tools are fakes at the uv, Python, venv, pip, and Hugging Face
boundaries. These tests never download a model, install packages, or use the
network. A data directory that the app's private configuration parser would
refuse must stop the installer before any provisioning starts.
"""

import ast
import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SERVICE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SERVICE))
import model_setup
import server
import setup
from private_files import load_env_file

HEBREW_MODEL = "ivrit-ai/whisper-large-v3-turbo-ct2"
HEBREW_REVISION = "72ad623a37947395efcc3933132353790e5a12f5"
HEBREW_SHA256 = "db2a2265aa012c16c7db9edda3d699c99f984efdd3f2e22a72a8ce7e9720f3a2"
SYNTHETIC_MODEL = "synthetic/test-model"
SYNTHETIC_REVISION = "a" * 40
SYNTHETIC_WEIGHTS = b"synthetic weights for installer tests"
SYNTHETIC_SHA256 = hashlib.sha256(SYNTHETIC_WEIGHTS).hexdigest()
SYNTHETIC_ARGS = ["--model", SYNTHETIC_MODEL, "--revision", SYNTHETIC_REVISION, "--model-sha256", SYNTHETIC_SHA256]
GO_CONFIG = SERVICE.parents[1] / "cmd" / "wa-indexer" / "transcription_config.go"
VALID_DIRECTORY = "Voice תמלול (ivrit) & café 50%"

PRELUDE = """import json, os, pathlib, sys
REAL = {real!r}
DISPATCHER = {dispatcher!r}


def record(*values):
    with open(os.environ["WA_TEST_LOG"], "a", encoding="utf-8") as log:
        log.write(json.dumps(list(values)) + "\\n")


"""

VENV_PYTHON = """argv = sys.argv[1:]
record("venv-python", *argv)
if argv[:2] == ["-m", "pip"]:
    sys.exit(0)
if argv[:1] == ["-c"] and "sys.version_info" in argv[1]:
    sys.version_info = tuple(int(part) for part in os.environ["WA_TEST_PY_VERSION"].split(".")) + ("final", 0)
    exec(argv[1], {"__name__": "__main__"})
    sys.exit(0)
os.execve(REAL, [REAL, *argv], os.environ)
"""

UV = """argv = sys.argv[1:]
record("uv", *argv)
if argv[:1] == ["venv"]:
    venv = pathlib.Path(argv[-1])
    (venv / "bin").mkdir(parents=True)
    (venv / "bin" / "python").symlink_to(DISPATCHER)
    sys.exit(0)
sys.exit(0 if argv[:2] == ["pip", "install"] else 2)
"""

PYTHON311 = """argv = sys.argv[1:]
record("python3.11", *argv)
if argv[:2] == ["-m", "venv"]:
    venv = pathlib.Path(argv[2])
    (venv / "bin").mkdir(parents=True)
    (venv / "bin" / "python").symlink_to(DISPATCHER)
    sys.exit(0)
sys.exit(2)
"""

HUB_STUB = """import json, os
from pathlib import Path


def _record(*values):
    with open(os.environ["WA_TEST_LOG"], "a", encoding="utf-8") as log:
        log.write(json.dumps(["hf", *values]) + "\\n")


class HfApi:
    def list_repo_files(self, repo_id, revision=None):
        _record("list_repo_files", repo_id, revision)
        return sorted(path.name for path in Path(os.environ["WA_TEST_HUB"]).iterdir())


def hf_hub_download(repo_id, filename, revision=None, cache_dir=None):
    _record("hf_hub_download", repo_id, filename, revision)
    return str(Path(os.environ["WA_TEST_HUB"]) / filename)
"""


def write_tool(path, body, dispatcher):
    prelude = PRELUDE.format(real=sys.executable, dispatcher=str(dispatcher))
    path.write_text("#!" + sys.executable + "\n" + prelude + body, encoding="utf-8")
    path.chmod(0o755)


class Toolchain:
    def __init__(self, root):
        self.root = root
        self.data = root / "transcription"
        self.home = root / "home"
        self.hub = root / "hub"
        self.site = root / "site"
        self.tools = root / "tools"
        self.log = root / "calls.jsonl"
        for directory in (self.home, self.hub, self.tools):
            directory.mkdir(mode=0o700)
        (self.hub / "model.bin").write_bytes(SYNTHETIC_WEIGHTS)
        (self.hub / "config.json").write_text("{}", encoding="utf-8")
        (self.hub / "tokenizer.json").write_text("{}", encoding="utf-8")
        (self.hub / "README.md").write_text("synthetic", encoding="utf-8")
        (self.site / "huggingface_hub").mkdir(parents=True)
        (self.site / "huggingface_hub" / "__init__.py").write_text(HUB_STUB, encoding="utf-8")
        (self.site / "faster_whisper").mkdir()
        (self.site / "faster_whisper" / "__init__.py").write_text("", encoding="utf-8")
        (self.site / "faster_whisper" / "tokenizer.py").write_text(
            '_LANGUAGE_CODES = ("ar", "en", "he", "ru")\n', encoding="utf-8")
        dispatcher = self.tools / "venv-python"
        write_tool(dispatcher, VENV_PYTHON, dispatcher)
        write_tool(self.tools / "uv", UV, dispatcher)
        write_tool(self.tools / "python3.11", PYTHON311, dispatcher)

    def path_for(self, tools):
        directory = self.root / ("path-" + ("+".join(tools) or "none"))
        directory.mkdir(exist_ok=True)
        for tool in tools:
            if not (directory / tool).is_symlink():
                (directory / tool).symlink_to(self.tools / tool)
        return directory

    def run(self, *args, data=None, tools=("uv",), version="3.11"):
        return subprocess.run(
            [sys.executable, str(SERVICE / "setup.py"), "--data-dir", str(data or self.data), *args],
            env={
                "PATH": str(self.path_for(tools)),
                "HOME": str(self.home),
                "PYTHONPATH": str(self.site),
                "PYTHONDONTWRITEBYTECODE": "1",
                "HF_HUB_OFFLINE": "1",
                "WA_TEST_LOG": str(self.log),
                "WA_TEST_HUB": str(self.hub),
                "WA_TEST_PY_VERSION": version,
            },
            capture_output=True, text=True, timeout=120, check=False,
        )

    def calls(self):
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text(encoding="utf-8").splitlines()]

    def downloads(self):
        return [call for call in self.calls() if call[0] == "hf"]

    def model_steps(self):
        return [call for call in self.calls()
                if len(call) > 1 and call[0] == "venv-python" and call[1].endswith("model_setup.py")]


class HebrewProfilePinTests(unittest.TestCase):
    def test_hebrew_profile_is_the_pinned_ivrit_model_forced_to_hebrew(self):
        model, revision, sha256, size, language = model_setup.PROFILES["hebrew"]
        self.assertEqual((model, revision, sha256, language), (HEBREW_MODEL, HEBREW_REVISION, HEBREW_SHA256, "he"))
        self.assertEqual(size, 1617884968)

    def test_multilingual_profile_remains_the_automatic_default(self):
        self.assertEqual(model_setup.PROFILES["multilingual"][4], "auto")
        self.assertEqual(sorted(model_setup.PROFILES), ["hebrew", "multilingual"])

    def test_guide_publishes_the_same_hebrew_pins_and_flags(self):
        guide = (SERVICE.parents[1] / "docs" / "transcription.md").read_text(encoding="utf-8")
        for expected in (HEBREW_MODEL, HEBREW_REVISION, HEBREW_SHA256, "--profile hebrew", "--language he"):
            self.assertIn(expected, guide)


class RuntimePinTests(unittest.TestCase):
    def test_requirements_match_the_versions_the_service_enforces(self):
        pins = {}
        for line in (SERVICE / "requirements.txt").read_text(encoding="utf-8").splitlines():
            name, separator, version = line.partition("==")
            if separator:
                pins[name.strip()] = version.strip()
        for name, version in server.RUNTIME_VERSIONS.items():
            distribution = "faster-whisper" if name == "faster_whisper" else name
            self.assertEqual(pins.get(distribution), version, distribution)
        self.assertIn("huggingface-hub", pins)

    def test_running_service_source_does_not_import_the_download_client(self):
        self.assertNotIn("huggingface", (SERVICE / "server.py").read_text(encoding="utf-8"))

    @unittest.skipUnless(importlib.util.find_spec("faster_whisper"), "pinned runtime is not installed")
    def test_installed_faster_whisper_accepts_the_hebrew_language_code(self):
        from faster_whisper.tokenizer import _LANGUAGE_CODES
        self.assertIn("he", _LANGUAGE_CODES)


class HebrewInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        self.data = self.root / "transcription"
        self.provisioned = []

    def tearDown(self):
        self.temporary.cleanup()

    def provision(self, args, **_):
        self.provisioned.append([str(item) for item in args])
        return subprocess.CompletedProcess(args, 0)

    def which(self, name):
        return "/synthetic/bin/uv" if name == "uv" else None

    def install(self, *args, data=None):
        argv = ["setup.py", "--data-dir", str(data or self.data), *args]
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()), \
                mock.patch.object(sys, "argv", argv), \
                mock.patch.object(setup.subprocess, "run", side_effect=self.provision), \
                mock.patch.object(setup.shutil, "which", side_effect=self.which):
            return setup.main()

    def model_step(self):
        steps = [call for call in self.provisioned if call[1].endswith("model_setup.py")]
        self.assertEqual(len(steps), 1)
        return steps[0]

    def env(self):
        return load_env_file(self.data / "transcription.env")

    def write_verified_model(self, model, revision):
        directory = self.data / "model"
        directory.mkdir(mode=0o700)
        files = []
        for name, content in (("config.json", b"{}"), ("model.bin", SYNTHETIC_WEIGHTS), ("tokenizer.json", b"{}")):
            path = directory / name
            path.write_bytes(content)
            path.chmod(0o600)
            files.append({"path": name, "size": len(content), "sha256": hashlib.sha256(content).hexdigest()})
        manifest = directory / "manifest.json"
        manifest.write_text(json.dumps({"schema_version": 1, "model": model, "revision": revision, "files": files}),
                            encoding="utf-8")
        manifest.chmod(0o600)

    def test_hebrew_profile_passes_pins_and_writes_matching_he_env(self):
        self.assertEqual(self.install("--profile", "hebrew"), 0)
        self.assertEqual(self.model_step()[-6:],
                         ["--model", HEBREW_MODEL, "--revision", HEBREW_REVISION, "--model-sha256", HEBREW_SHA256])
        env = self.env()
        self.assertEqual(env["WA_TRANSCRIPTION_LANGUAGE"], "he")
        self.assertEqual(env["WA_INDEXER_TRANSCRIPTION_LANGUAGE"], "he")
        self.assertEqual(env["WA_TRANSCRIPTION_LISTEN"], "127.0.0.1:6971")
        self.assertEqual(env["WA_INDEXER_TRANSCRIPTION_URL"], "http://127.0.0.1:6971")
        self.assertEqual(env["WA_TRANSCRIPTION_MODEL_DIR"], str(self.data / "model"))
        self.assertTrue(env["WA_INDEXER_TRANSCRIPTION_TOKEN"] == env["WA_TRANSCRIPTION_TOKEN"],
                        "app and service tokens differ")
        self.assertEqual(stat.S_IMODE((self.data / "transcription.env").stat().st_mode), 0o600)

    def test_default_install_uses_the_multilingual_automatic_profile(self):
        self.assertEqual(self.install(), 0)
        self.assertIn("dropbox-dash/faster-whisper-large-v3-turbo", self.model_step())
        self.assertEqual(self.env()["WA_TRANSCRIPTION_LANGUAGE"], "auto")

    def test_explicit_hebrew_rerun_leaves_the_environment_unchanged(self):
        self.assertEqual(self.install("--profile", "hebrew", "--language", "he"), 0)
        before = (self.data / "transcription.env").read_bytes()
        self.assertEqual(self.install("--profile", "hebrew", "--language", "he"), 0)
        self.assertTrue((self.data / "transcription.env").read_bytes() == before, "rerun rewrote the environment")

    def test_rerun_without_flags_keeps_the_installed_hebrew_identity(self):
        self.assertEqual(self.install("--profile", "hebrew", "--language", "he"), 0)
        self.write_verified_model(HEBREW_MODEL, HEBREW_REVISION)
        before = (self.data / "transcription.env").read_bytes()
        self.provisioned.clear()
        self.assertEqual(self.install(), 0)
        self.assertEqual(self.model_step()[-6:-2], ["--model", HEBREW_MODEL, "--revision", HEBREW_REVISION])
        self.assertTrue((self.data / "transcription.env").read_bytes() == before, "rerun rewrote the environment")

    def test_rerun_repairs_app_token_url_and_language_from_service_values(self):
        self.assertEqual(self.install("--profile", "hebrew", "--language", "he"), 0)
        path = self.data / "transcription.env"
        values = self.env()
        service_token = values["WA_TRANSCRIPTION_TOKEN"]
        values.update({
            "WA_INDEXER_TRANSCRIPTION_TOKEN": "x" * 43,
            "WA_INDEXER_TRANSCRIPTION_URL": "http://127.0.0.1:6999",
            "WA_INDEXER_TRANSCRIPTION_LANGUAGE": "auto",
        })
        path.write_text("".join(f"{key}={value}\n" for key, value in values.items()), encoding="utf-8")
        self.assertEqual(self.install(), 0)
        repaired = self.env()
        self.assertTrue(repaired["WA_TRANSCRIPTION_TOKEN"] == service_token, "service token changed")
        self.assertTrue(repaired["WA_INDEXER_TRANSCRIPTION_TOKEN"] == service_token, "app token was not repaired")
        self.assertEqual(repaired["WA_INDEXER_TRANSCRIPTION_URL"], "http://127.0.0.1:6971")
        self.assertEqual(repaired["WA_INDEXER_TRANSCRIPTION_LANGUAGE"], "he")

    def test_nul_in_data_dir_is_refused_before_provisioning(self):
        with self.assertRaises(SystemExit) as refused:
            self.install("--profile", "hebrew", data=self.root / "bad\x00dir" / "transcription")
        self.assertEqual(refused.exception.code, 2)
        self.assertEqual(self.provisioned, [])


class HebrewInstallerCliTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)

    def tearDown(self):
        self.temporary.cleanup()

    def toolchain(self, name):
        root = self.root / name
        root.mkdir(mode=0o700)
        return Toolchain(root)

    def test_hebrew_profile_reaches_the_pinned_download_and_refuses_unpinned_weights(self):
        toolchain = self.toolchain("hebrew")
        result = toolchain.run("--profile", "hebrew", "--language", "he")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(result.stdout, "")
        calls = toolchain.calls()
        self.assertIn(["hf", "list_repo_files", HEBREW_MODEL, HEBREW_REVISION], calls)
        self.assertIn(["hf", "hf_hub_download", HEBREW_MODEL, "model.bin", HEBREW_REVISION], calls)
        self.assertEqual(toolchain.model_steps()[-1][-6:],
                         ["--model", HEBREW_MODEL, "--revision", HEBREW_REVISION, "--model-sha256", HEBREW_SHA256])
        self.assertTrue(any(call[0] == "venv-python" and call[1:2] == ["-c"] and call[-1] == "he" for call in calls))
        for name in ("transcription.env", "model", "model.download"):
            self.assertFalse((toolchain.data / name).exists(), name)

    def test_wrong_model_sha256_pin_is_refused_without_installing(self):
        toolchain = self.toolchain("wrong-pin")
        result = toolchain.run("--language", "he", "--model", SYNTHETIC_MODEL,
                               "--revision", SYNTHETIC_REVISION, "--model-sha256", "0" * 64)
        self.assertEqual(result.returncode, 1, result.stderr)
        for name in ("transcription.env", "model", "model.download"):
            self.assertFalse((toolchain.data / name).exists(), name)

    def test_unsupported_venv_python_is_refused_before_pip_or_download(self):
        for version in ("3.10", "3.14"):
            with self.subTest(version=version):
                toolchain = self.toolchain(version)
                result = toolchain.run("--profile", "hebrew", "--language", "he", version=version)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertFalse(any(call[0] == "venv-python" and call[1:3] == ["-m", "pip"]
                                     for call in toolchain.calls()))
                self.assertEqual(toolchain.downloads(), [])
                self.assertFalse((toolchain.data / "transcription.env").exists())

    def test_supported_python_versions_install_pinned_requirements_and_matching_env(self):
        for version in ("3.11", "3.13"):
            with self.subTest(version=version):
                toolchain = self.toolchain(version)
                result = toolchain.run("--language", "he", *SYNTHETIC_ARGS, version=version)
                self.assertEqual(result.returncode, 0, result.stderr)
                env = load_env_file(toolchain.data / "transcription.env")
                self.assertEqual(env["WA_TRANSCRIPTION_LANGUAGE"], "he")
                self.assertEqual(env["WA_INDEXER_TRANSCRIPTION_LANGUAGE"], "he")
                self.assertTrue(env["WA_INDEXER_TRANSCRIPTION_TOKEN"] == env["WA_TRANSCRIPTION_TOKEN"],
                                "app and service tokens differ")
                self.assertEqual(env["WA_INDEXER_TRANSCRIPTION_URL"], "http://127.0.0.1:6971")
                self.assertEqual(env["WA_TRANSCRIPTION_MODEL_DIR"], str(toolchain.data / "model"))
                self.assertFalse(env["WA_TRANSCRIPTION_TOKEN"] in result.stdout + result.stderr,
                                 "service credential was printed")
                venv = toolchain.data / "venv"
                calls = toolchain.calls()
                self.assertIn(["uv", "venv", "--python", "3.11", str(venv)], calls)
                self.assertIn(["uv", "pip", "install", "--python", str(venv / "bin" / "python"),
                               "-r", str(SERVICE / "requirements.txt")], calls)

    def test_python_module_fallback_matches_the_container_provisioning_path(self):
        toolchain = self.toolchain("fallback")
        result = toolchain.run("--language", "he", *SYNTHETIC_ARGS, tools=("python3.11",))
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = toolchain.calls()
        self.assertIn(["python3.11", "-m", "venv", str(toolchain.data / "venv")], calls)
        self.assertIn(["venv-python", "-m", "pip", "install", "-r", str(SERVICE / "requirements.txt")], calls)
        self.assertFalse(any(call[0] == "uv" for call in calls))

    def test_missing_uv_and_python311_is_refused_before_creating_a_venv(self):
        toolchain = self.toolchain("missing")
        result = toolchain.run("--profile", "hebrew", tools=())
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(toolchain.calls(), [])
        self.assertFalse((toolchain.data / "venv").exists())

    def test_rerun_keeps_the_environment_and_does_not_redownload_weights(self):
        toolchain = self.toolchain("rerun")
        self.assertEqual(toolchain.run("--language", "he", *SYNTHETIC_ARGS).returncode, 0)
        before = (toolchain.data / "transcription.env").read_bytes()
        downloads = toolchain.downloads()
        result = toolchain.run("--language", "he", *SYNTHETIC_ARGS)
        self.assertEqual(result.returncode, 0, result.stderr)
        result = toolchain.run()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((toolchain.data / "transcription.env").read_bytes() == before, "rerun rewrote the environment")
        self.assertEqual(toolchain.downloads(), downloads)

    def test_installed_model_is_not_replaced_by_the_hebrew_profile(self):
        toolchain = self.toolchain("switch")
        self.assertEqual(toolchain.run("--language", "he", *SYNTHETIC_ARGS).returncode, 0)
        before = (toolchain.data / "transcription.env").read_bytes()
        downloads = toolchain.downloads()
        result = toolchain.run("--profile", "hebrew", "--language", "he")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertTrue((toolchain.data / "transcription.env").read_bytes() == before,
                        "failed switch changed the environment")
        self.assertEqual(toolchain.downloads(), downloads)
        manifest = json.loads((toolchain.data / "model" / "manifest.json").read_text(encoding="utf-8"))
        self.assertEqual((manifest["model"], manifest["revision"]), (SYNTHETIC_MODEL, SYNTHETIC_REVISION))

    def test_interrupted_download_staging_is_discarded_before_a_clean_retry(self):
        toolchain = self.toolchain("staging")
        toolchain.data.mkdir(mode=0o700)
        staging = toolchain.data / "model.download"
        staging.mkdir(mode=0o700)
        partial = staging / "model.bin"
        partial.write_bytes(b"partial")
        partial.chmod(0o600)
        result = toolchain.run("--language", "he", *SYNTHETIC_ARGS)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(staging.exists())
        self.assertEqual((toolchain.data / "model" / "model.bin").read_bytes(), SYNTHETIC_WEIGHTS)

    def test_group_readable_or_symlinked_data_directory_is_refused(self):
        toolchain = self.toolchain("readable")
        toolchain.data.mkdir()
        toolchain.data.chmod(0o755)
        result = toolchain.run("--profile", "hebrew")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(toolchain.calls(), [])

        toolchain = self.toolchain("symlink")
        target = toolchain.root / "private-target"
        target.mkdir(mode=0o700)
        toolchain.data.symlink_to(target)
        result = toolchain.run("--profile", "hebrew")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(toolchain.calls(), [])

    def assertRefusedBeforeProvisioning(self, result, toolchain, data):
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("data directory", result.stderr)
        self.assertNotIn(str(data), result.stderr)
        self.assertEqual(toolchain.calls(), [], "a provisioning tool ran")
        self.assertFalse(data.exists(), "the data directory was created")

    def test_quote_backtick_and_dollar_in_data_dir_are_refused_before_provisioning(self):
        for character in ("'", '"', "`", "$"):
            with self.subTest(character=character):
                toolchain = self.toolchain(f"char-{ord(character)}")
                data = toolchain.root / f"Jane{character}transcription"
                result = toolchain.run("--profile", "hebrew", data=data)
                self.assertRefusedBeforeProvisioning(result, toolchain, data)

    def test_carriage_return_and_line_feed_in_data_dir_are_refused_before_provisioning(self):
        for character in ("\r", "\n"):
            with self.subTest(character=repr(character)):
                toolchain = self.toolchain(f"break-{ord(character)}")
                data = toolchain.root / f"Jane{character}transcription"
                result = toolchain.run("--profile", "hebrew", data=data)
                self.assertRefusedBeforeProvisioning(result, toolchain, data)

    def test_valid_spaces_hebrew_and_unicode_path_is_accepted_for_the_he_profile(self):
        toolchain = self.toolchain("valid-path")
        data = toolchain.root / VALID_DIRECTORY / "transcription"
        result = toolchain.run("--language", "he", *SYNTHETIC_ARGS, data=data)
        self.assertEqual(result.returncode, 0, result.stderr)
        env = load_env_file(data / "transcription.env")
        self.assertEqual(env["WA_TRANSCRIPTION_LANGUAGE"], "he")
        self.assertEqual(env["WA_INDEXER_TRANSCRIPTION_LANGUAGE"], "he")
        self.assertEqual(env["WA_TRANSCRIPTION_MODEL_DIR"], str(data / "model"))
        self.assertEqual(env["WA_INDEXER_TRANSCRIPTION_URL"], "http://127.0.0.1:6971")
        self.assertTrue(env["WA_INDEXER_TRANSCRIPTION_TOKEN"] == env["WA_TRANSCRIPTION_TOKEN"],
                        "app and service tokens differ")
        self.assertFalse(env["WA_TRANSCRIPTION_TOKEN"] in result.stdout + result.stderr,
                         "service credential was printed")

    def test_hebrew_profile_with_valid_path_reaches_the_pinned_download(self):
        toolchain = self.toolchain("valid-hebrew")
        data = toolchain.root / VALID_DIRECTORY / "transcription"
        result = toolchain.run("--profile", "hebrew", "--language", "he", data=data)
        # The synthetic hub cannot satisfy the real Hebrew SHA-256 pin.
        self.assertEqual(result.returncode, 1, result.stderr)
        calls = toolchain.calls()
        self.assertIn(["hf", "list_repo_files", HEBREW_MODEL, HEBREW_REVISION], calls)
        self.assertEqual(toolchain.model_steps()[-1][-6:],
                         ["--model", HEBREW_MODEL, "--revision", HEBREW_REVISION, "--model-sha256", HEBREW_SHA256])
        self.assertTrue(any(call[0] == "venv-python" and call[1:2] == ["-c"] and call[-1] == "he"
                            for call in calls))
        self.assertFalse((data / "transcription.env").exists())

    def test_existing_safe_install_is_preserved_and_unsafe_sibling_is_refused(self):
        toolchain = self.toolchain("existing")
        data = toolchain.root / VALID_DIRECTORY / "transcription"
        self.assertEqual(toolchain.run("--language", "he", *SYNTHETIC_ARGS, data=data).returncode, 0)
        env_bytes = (data / "transcription.env").read_bytes()
        token = load_env_file(data / "transcription.env")["WA_TRANSCRIPTION_TOKEN"]
        downloads = toolchain.downloads()
        result = toolchain.run(data=data)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((data / "transcription.env").read_bytes() == env_bytes, "rerun changed the environment")
        self.assertEqual(toolchain.downloads(), downloads)
        self.assertFalse(token in result.stdout + result.stderr, "service credential was printed")

        calls = toolchain.calls()
        unsafe = toolchain.root / "Jane's transcription"
        result = toolchain.run("--language", "he", *SYNTHETIC_ARGS, data=unsafe)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertEqual(toolchain.calls(), calls, "an unsafe sibling reached provisioning")
        self.assertFalse(unsafe.exists())
        self.assertTrue((data / "transcription.env").read_bytes() == env_bytes,
                        "an unsafe sibling changed the safe install")

    def test_existing_env_with_a_quote_in_a_preserved_value_is_refused_before_provisioning(self):
        toolchain = self.toolchain("tampered")
        data = toolchain.root / VALID_DIRECTORY / "transcription"
        self.assertEqual(toolchain.run("--language", "he", *SYNTHETIC_ARGS, data=data).returncode, 0)
        path = data / "transcription.env"
        path.write_text(path.read_text(encoding="utf-8").replace(
            "WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS=600\n", "WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS=600'\n"),
            encoding="utf-8")
        before = path.read_bytes()
        calls = toolchain.calls()
        result = toolchain.run(data=data)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(toolchain.calls(), calls, "a provisioning tool ran before the unsafe value was refused")
        self.assertTrue(path.read_bytes() == before, "the unsafe environment was rewritten")


class EnvValueContractTests(unittest.TestCase):
    def test_refuses_each_character_or_edge_that_breaks_the_app_or_service_parser(self):
        for value in ("a'b", 'a"b', "a`b", "a$b", "a\x00b", "a\rb", "a\nb", "a\x0bb", "a\x0cb",
                      "a\x85b", "a\u2028b", "a\u2029b", "a\tb", " ab", "ab ", "\u3000ab", "", "a\udcffb"):
            with self.subTest(value=repr(value)):
                self.assertFalse(setup.env_value_compatible(value))

    def test_accepts_spaces_hebrew_unicode_and_ordinary_private_paths(self):
        for value in ("/Users/Ana/Library/Application Support/My WhatsApp/transcription",
                      VALID_DIRECTORY, "/private/var/x/תמלול עברית/transcription", "/tmp/a=b#c~d!e,f"):
            with self.subTest(value=value):
                self.assertTrue(setup.env_value_compatible(value))


@unittest.skipUnless(GO_CONFIG.exists(), "the Go parser source is not in this checkout")
class GoEnvContractTests(unittest.TestCase):
    def test_validator_refuses_every_value_the_go_parser_refuses(self):
        source = GO_CONFIG.read_text(encoding="utf-8")
        start = source.index("func parseTranscriptionEnv(")
        function = source[start:source.index("\n}\n", start)]
        match = re.search(r'strings\.ContainsAny\(value, ("(?:[^"\\]|\\.)*")\)', function)
        self.assertIsNotNone(match, "parseTranscriptionEnv changed shape")
        refused = set(ast.literal_eval(match.group(1)))
        self.assertEqual(refused, {"\x00", "\r", "\n", '"', "'", "`", "$"})
        self.assertIn('value == ""', function)
        self.assertIn("strings.TrimSpace(value) != value", function)
        for character in refused:
            with self.subTest(character=repr(character)):
                self.assertFalse(setup.env_value_compatible("a" + character + "b"))


if __name__ == "__main__":
    unittest.main()
