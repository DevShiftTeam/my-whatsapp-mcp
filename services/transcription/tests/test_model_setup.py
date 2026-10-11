"""Integrity checks use tiny synthetic model files."""

import hashlib
import json
import os
import pwd
from pathlib import Path
import sys
import tempfile
import unittest
import subprocess

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from model_setup import cleanup_staging, verify_model
from private_files import cleanup_uploads, load_env_file, service_lock
from setup import configure_environment


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        files = []
        for name, content in [("model.bin", b"synthetic"), ("config.json", b"{}"), ("tokenizer.json", b"{}")]:
            path = self.root / name
            path.write_bytes(content)
            path.chmod(0o600)
            files.append({"path": name, "size": len(content), "sha256": hashlib.sha256(content).hexdigest()})
        self.manifest = {"schema_version": 1, "model": "synthetic/test-model", "revision": "a" * 40,
                         "files": sorted(files, key=lambda entry: entry["path"])}
        self.write_manifest()

    def tearDown(self):
        self.temporary.cleanup()

    def write_manifest(self):
        path = self.root / "manifest.json"
        path.write_text(json.dumps(self.manifest))
        path.chmod(0o600)

    def test_verifies_all_files_and_model_digest(self):
        model = verify_model(self.root)
        self.assertEqual(model["model_sha256"], hashlib.sha256(b"synthetic").hexdigest())
        self.assertEqual(len(model["model_files"]), 3)

    def test_rejects_changed_ancillary_file(self):
        (self.root / "tokenizer.json").write_bytes(b"changed")
        with self.assertRaises(ValueError):
            verify_model(self.root)

    def test_rejects_model_symlink_and_extra_file(self):
        (self.root / "unexpected.json").write_text("{}")
        (self.root / "unexpected.json").chmod(0o600)
        with self.assertRaises(ValueError):
            verify_model(self.root)
        (self.root / "unexpected.json").unlink()
        (self.root / "model.bin").unlink()
        (self.root / "model.bin").symlink_to(self.root / "config.json")
        with self.assertRaises(ValueError):
            verify_model(self.root)

    def test_rejects_manifest_traversal(self):
        self.manifest["files"][0]["path"] = "../outside.bin"
        self.write_manifest()
        with self.assertRaises(ValueError):
            verify_model(self.root)

    def test_rejects_permissive_model_file(self):
        (self.root / "model.bin").chmod(0o644)
        with self.assertRaises(ValueError):
            verify_model(self.root)


class PrivateEnvironmentTests(unittest.TestCase):
    def test_environment_is_data_not_shell_and_rejects_unknown_keys(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            root.chmod(0o700)
            path = root / "transcription.env"
            path.write_text("WA_TRANSCRIPTION_MODEL_DIR=/private path/model\nWA_TRANSCRIPTION_TOKEN=" + "x" * 43 + "\n")
            path.chmod(0o600)
            env = load_env_file(path)
            self.assertEqual(env["WA_TRANSCRIPTION_MODEL_DIR"], "/private path/model")
            path.write_text("export WA_TRANSCRIPTION_TOKEN=not-shell\n")
            with self.assertRaises(ValueError):
                load_env_file(path)
            path.write_text("HOME=/changed\n")
            with self.assertRaises(ValueError):
                load_env_file(path)

    def test_rejects_group_readable_environment(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary).resolve() / "transcription.env"
            path.write_text("WA_TRANSCRIPTION_TOKEN=" + "x" * 43 + "\n")
            path.chmod(0o640)
            with self.assertRaises(ValueError):
                load_env_file(path)

    @unittest.skipUnless(sys.platform == "darwin", "Darwin extended ACL check")
    def test_rejects_acl_grants_even_with_private_mode(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            path = root / "transcription.env"
            path.write_text("WA_TRANSCRIPTION_TOKEN=" + "x" * 43 + "\n")
            path.chmod(0o600)
            username = pwd.getpwuid(os.getuid()).pw_name
            subprocess.run(["/bin/chmod", "+a", username + " allow read", str(path)], check=True)
            try:
                with self.assertRaises(ValueError):
                    load_env_file(path)
            finally:
                subprocess.run(["/bin/chmod", "-N", str(path)], check=True)


class ServiceLifetimeTests(unittest.TestCase):
    def test_lock_excludes_second_service_and_preserves_lock_inode(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            with service_lock(root):
                inode = (root / "service.lock").stat().st_ino
                with self.assertRaises(ValueError):
                    with service_lock(root):
                        pass
            with service_lock(root):
                self.assertEqual((root / "service.lock").stat().st_ino, inode)

    def test_safe_startup_removes_only_private_stale_uploads(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            uploads = root / "uploads"
            uploads.mkdir(mode=0o700)
            stale = uploads / "voice-synthetic.audio"
            stale.write_bytes(b"synthetic")
            stale.chmod(0o600)
            cleanup_uploads(uploads)
            self.assertEqual(list(uploads.iterdir()), [])
            unexpected = uploads / "other.txt"
            unexpected.write_text("synthetic")
            unexpected.chmod(0o600)
            with self.assertRaises(ValueError):
                cleanup_uploads(uploads)
            self.assertTrue(unexpected.exists())

    def test_startup_cleanup_refuses_symlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            uploads = root / "uploads"
            uploads.mkdir(mode=0o700)
            target = root / "synthetic.audio"
            target.write_bytes(b"synthetic")
            target.chmod(0o600)
            (uploads / "voice-synthetic.audio").symlink_to(target)
            with self.assertRaises(ValueError):
                cleanup_uploads(uploads)
            self.assertTrue(target.exists())


class InstallerEnvironmentTests(unittest.TestCase):
    def test_rerun_preserves_token_and_explicit_flags_update_fixed_fields(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            path = configure_environment(root, profile_language="he")
            initial = load_env_file(path)
            self.assertEqual(initial["WA_TRANSCRIPTION_LANGUAGE"], "he")
            self.assertEqual(initial["WA_TRANSCRIPTION_CPU_THREADS"], "4")
            configure_environment(root)
            self.assertEqual(load_env_file(path), initial)
            configure_environment(root, language="auto", cpu_threads=2, port=6972)
            changed = load_env_file(path)
            self.assertEqual(changed["WA_TRANSCRIPTION_TOKEN"], initial["WA_TRANSCRIPTION_TOKEN"])
            self.assertEqual(changed["WA_INDEXER_TRANSCRIPTION_TOKEN"], initial["WA_TRANSCRIPTION_TOKEN"])
            self.assertEqual(changed["WA_TRANSCRIPTION_LANGUAGE"], "auto")
            self.assertEqual(changed["WA_INDEXER_TRANSCRIPTION_LANGUAGE"], "auto")
            self.assertEqual(changed["WA_TRANSCRIPTION_CPU_THREADS"], "2")
            self.assertEqual(changed["WA_TRANSCRIPTION_LISTEN"], "127.0.0.1:6972")
            self.assertEqual(changed["WA_INDEXER_TRANSCRIPTION_URL"], "http://127.0.0.1:6972")

    def test_rerun_preserves_an_explicit_ipv6_loopback_listener(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            path = configure_environment(root)
            values = load_env_file(path)
            values["WA_TRANSCRIPTION_LISTEN"] = "[::1]:6971"
            values["WA_INDEXER_TRANSCRIPTION_URL"] = "http://[::1]:6971"
            path.write_text("".join(f"{key}={value}\n" for key, value in values.items()))
            configure_environment(root)
            self.assertEqual(load_env_file(path), values)

    def test_interrupted_environment_update_can_retry_without_rotating_token(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            path = configure_environment(root)
            original = load_env_file(path)
            partial = root / "transcription.env.new"
            partial.write_bytes(b"partial synthetic configuration")
            partial.chmod(0o600)
            configure_environment(root, language="he")
            changed = load_env_file(path)
            self.assertEqual(changed["WA_TRANSCRIPTION_TOKEN"], original["WA_TRANSCRIPTION_TOKEN"])
            self.assertEqual(changed["WA_TRANSCRIPTION_LANGUAGE"], "he")
            self.assertFalse(partial.exists())

    def test_interrupted_download_can_clean_known_private_staging(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            staging = root / "model.download"
            staging.mkdir(mode=0o700)
            partial = staging / "model.bin"
            partial.write_bytes(b"partial synthetic model")
            partial.chmod(0o600)
            cleanup_staging(staging)
            self.assertFalse(staging.exists())
            staging.mkdir(mode=0o700)
            unrelated = staging / "unrelated.secret"
            unrelated.write_text("synthetic")
            unrelated.chmod(0o600)
            with self.assertRaises(ValueError):
                cleanup_staging(staging)
            self.assertTrue(unrelated.exists())


if __name__ == "__main__":
    unittest.main()
