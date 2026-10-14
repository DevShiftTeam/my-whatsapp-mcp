import hashlib
import importlib.util
import io
import json
import os
import pathlib
import stat
import tarfile
import subprocess
import sys
import tempfile
import unittest
import contextlib
from unittest import mock

import verify_cloudflared
import test_assemble_macos as fixtures
from test_assemble_macos import valid_macho


class BuildBasicMacOSTests(unittest.TestCase):
    def builder(self):
        path = pathlib.Path(__file__).with_name("build-basic-macos.py")
        self.assertTrue(path.is_file(), "basic candidate builder is missing")
        spec = importlib.util.spec_from_file_location("basic_builder", path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def fixture(self, root):
        app = root / "audited-application"
        body = valid_macho()
        app.write_bytes(body)
        app.chmod(0o700)
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w:gz") as archive:
            member = tarfile.TarInfo("cloudflared")
            member.size = len(body)
            archive.addfile(member, io.BytesIO(body))
        asset = root / "pinned-helper.tgz"
        asset.write_bytes(stream.getvalue())
        asset.chmod(0o600)
        pin = {"platform": "darwin/arm64", "archive_format": "tgz",
               "archive_size": asset.stat().st_size, "archive_sha256": hashlib.sha256(asset.read_bytes()).hexdigest(),
               "executable_size": len(body), "executable_sha256": hashlib.sha256(body).hexdigest()}
        return app, asset, {"assets": [pin]}, hashlib.sha256(body).hexdigest()

    def test_builder_emits_only_unsigned_basic_candidate_using_offline_pinned_asset(self):
        builder = self.builder()
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            app, asset, pins, digest = self.fixture(root)
            output = root / "My WhatsApp.new.app"
            original = app.stat()
            with mock.patch.object(verify_cloudflared, "load_manifest", return_value=pins), mock.patch.object(
                    verify_cloudflared, "acquire", side_effect=AssertionError("network forbidden")), mock.patch.object(
                    verify_cloudflared, "probe", side_effect=AssertionError("native execution forbidden")), mock.patch.object(
                    builder.assembler.subprocess, "run", fixtures.ASRRoleAdmissionTests().tool):
                builder.build(app, digest, "a" * 40, asset, output)
                builder.verifier.verify(str(output))
            manifest = json.loads((output / "Contents/candidate-manifest.json").read_bytes())
            self.assertEqual(manifest["bundle_profile"], "basic")
            self.assertIs(manifest["release_ready"], False)
            self.assertEqual(manifest["runtime_qualification"], "NOT_RUN")
            self.assertEqual(manifest["source_revision"], "a" * 40)
            self.assertEqual(manifest["ui"], "embedded_in_application")
            self.assertEqual((output / "Contents/MacOS/wa-indexer").read_bytes(), app.read_bytes())
            self.assertEqual(stat.S_IMODE(app.stat().st_mode), stat.S_IMODE(original.st_mode))
            self.assertEqual(app.stat().st_mtime_ns, original.st_mtime_ns)
            self.assertFalse((output / "Contents/release-manifest.json").exists())
            self.assertFalse((output / "Contents/Frameworks").exists())
            source = pathlib.Path(__file__).resolve().parents[2]
            self.assertEqual((output / "Contents/Resources/logo.png").read_bytes(), (source / "docs/assets/logo.png").read_bytes())
            self.assertTrue((output / "Contents/Resources/notices/cloudflared/LICENSE").is_file())
            self.assertEqual(list(root.glob(".basic-package-*")), [])

    def test_wrong_hash_unpinned_helper_invalid_revision_and_existing_output_refuse(self):
        builder = self.builder()
        for fault in ("hash", "helper", "revision", "digest_shape", "existing", "source_symlink", "source_hardlink", "private_parent"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                app, asset, pins, digest = self.fixture(root)
                output = root / "My WhatsApp.new.app"
                revision = "a" * 40
                if fault == "hash":
                    digest = "0" * 64
                elif fault == "helper":
                    pins["assets"][0]["archive_sha256"] = "0" * 64
                elif fault == "revision":
                    revision = "main"
                elif fault == "digest_shape":
                    digest = "not-a-hash"
                elif fault == "existing":
                    output.mkdir()
                    (output / "keep").write_bytes(b"existing user output")
                elif fault == "source_symlink":
                    link = root / "linked-app"
                    link.symlink_to(app)
                    app = link
                elif fault == "source_hardlink":
                    os.link(app, root / "hardlinked-app")
                elif fault == "private_parent":
                    root.chmod(0o755)
                with mock.patch.object(verify_cloudflared, "load_manifest", return_value=pins), mock.patch.object(
                        verify_cloudflared, "acquire", side_effect=AssertionError("network forbidden")), mock.patch.object(
                        builder.assembler.subprocess, "run", fixtures.ASRRoleAdmissionTests().tool):
                    with self.assertRaises(ValueError):
                        builder.build(app, digest, revision, asset, output)
                if fault == "existing":
                    self.assertEqual((output / "keep").read_bytes(), b"existing user output")
                else:
                    self.assertFalse(output.exists())
                root.chmod(0o700)
                self.assertEqual(list(root.glob(".basic-package-*")), [])

    def test_shell_interface_documents_inputs_and_rejects_relative_python(self):
        shell = pathlib.Path(__file__).with_name("build-basic-macos.sh")
        self.assertTrue(shell.is_file())
        result = subprocess.run(["/bin/sh", str(shell), "--help"], capture_output=True, text=True,
                                env={"LC_ALL": "C", "WA_INDEXER_PACKAGE_PYTHON": sys.executable})
        self.assertEqual(result.returncode, 0)
        for flag in ("--application", "--application-sha256", "--source-revision", "--cloudflared-asset", "--cloudflared", "--output"):
            self.assertIn(flag, result.stdout)
        result = subprocess.run(["/bin/sh", str(shell), "--help"], capture_output=True, text=True,
                                env={"LC_ALL": "C", "WA_INDEXER_PACKAGE_PYTHON": "python3"})
        self.assertEqual(result.returncode, 2)
        self.assertIn("Absolute developer Python path required.", result.stderr)

    def test_extracted_helper_is_pinned_and_never_modified_or_executed(self):
        builder = self.builder()
        for fault in (None, "pin", "size", "both", "neither"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                app, asset, pins, digest = self.fixture(root)
                helper = root / "already-extracted-cloudflared"
                helper.write_bytes(valid_macho())
                helper.chmod(0o755)
                original = helper.stat()
                output = root / "Extracted Helper.new.app"
                if fault == "pin":
                    pins["assets"][0]["executable_sha256"] = "0" * 64
                elif fault == "size":
                    pins["assets"][0]["executable_size"] += 1
                with mock.patch.object(verify_cloudflared, "load_manifest", return_value=pins), mock.patch.object(
                        verify_cloudflared, "install", side_effect=AssertionError("archive install forbidden")), mock.patch.object(
                        verify_cloudflared, "acquire", side_effect=AssertionError("network forbidden")), mock.patch.object(
                        verify_cloudflared, "probe", side_effect=AssertionError("native execution forbidden")), mock.patch.object(
                        builder.assembler.subprocess, "run", fixtures.ASRRoleAdmissionTests().tool):
                    kwargs = {"cloudflared": None if fault == "neither" else helper}
                    args = (app, digest, "a" * 40, asset if fault == "both" else None, output)
                    if fault:
                        with self.assertRaises(ValueError):
                            builder.build(*args, **kwargs)
                        self.assertFalse(output.exists())
                    else:
                        builder.build(*args, **kwargs)
                        builder.verifier.verify(str(output))
                        self.assertEqual((output / "Contents/MacOS/cloudflared").read_bytes(), helper.read_bytes())
                self.assertEqual(helper.stat().st_mode, original.st_mode)
                self.assertEqual(helper.stat().st_mtime_ns, original.st_mtime_ns)
                self.assertEqual(list(root.glob(".basic-package-*")), [])

    def test_cli_reports_only_static_candidate_digest_or_fixed_refusal(self):
        builder = self.builder()
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            output = root / "Synthetic CLI.app"
            (output / "Contents").mkdir(parents=True)
            manifest_bytes = b"SYNTHETIC CLI OUTPUT; NOT A BUNDLE OR RUNTIME PROOF"
            (output / "Contents/candidate-manifest.json").write_bytes(manifest_bytes)
            args = ["build-basic-macos.py", "--application", str(root / "app"), "--application-sha256", "a" * 64,
                    "--source-revision", "b" * 40, "--cloudflared", str(root / "helper"), "--output", str(output)]
            captured = io.StringIO()
            with mock.patch.object(sys, "argv", args), mock.patch.object(builder, "build", return_value=output) as build, contextlib.redirect_stdout(captured):
                builder.main()
            build.assert_called_once_with(root / "app", "a" * 64, "b" * 40, None, output, cloudflared=root / "helper")
            self.assertIn("runtime_qualification=NOT_RUN", captured.getvalue())
            self.assertIn("candidate_manifest_sha256=" + hashlib.sha256(manifest_bytes).hexdigest(), captured.getvalue())
            self.assertNotIn(str(root), captured.getvalue())
            with mock.patch.object(sys, "argv", args), mock.patch.object(builder, "build", side_effect=ValueError(str(root))):
                with self.assertRaises(SystemExit) as refusal:
                    builder.main()
            self.assertNotIn(str(root), str(refusal.exception))


if __name__ == "__main__":
    unittest.main()
