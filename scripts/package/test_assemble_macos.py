import importlib.util
import json
import hashlib
import io
import plistlib
from unittest import mock
from types import SimpleNamespace
import pathlib
import tempfile
import struct
import shutil
import os
import stat
import unittest

spec = importlib.util.spec_from_file_location("assembler", pathlib.Path(__file__).with_name("assemble_macos.py"))
assembler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assembler)
verify_spec = importlib.util.spec_from_file_location("role_verifier", pathlib.Path(__file__).with_name("verify_macos.py"))
verifier = importlib.util.module_from_spec(verify_spec)
verify_spec.loader.exec_module(verifier)


def valid_macho():
    # Thin ARM64, macOS minimum14.0, SDK27.0, one exact build-version command.
    return struct.pack("<8I", 0xFEEDFACF, 0x0100000C, 0, 2, 1, 24, 0, 0) + struct.pack(
        "<6I", 0x32, 24, 1, 0x000E0000, 0x001B0000, 0)


class BundleAdmissionTests(unittest.TestCase):
    def test_generated_metadata_collisions_refuse_complete_manifest_without_output(self):
        for reserved in ("Contents/Info.plist", "Contents/release-manifest.json", "Contents/info.plist", "Contents/RELEASE-MANIFEST.JSON"):
            with self.subTest(reserved=reserved), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                artifacts = []
                entries = (
                    ("application", "Contents/MacOS/wa-indexer", True),
                    ("native_worker", "Contents/MacOS/worker", True),
                    ("native_runtime", "Contents/Frameworks/runtime.dylib", True),
                    ("license", "Contents/Resources/LICENSE", False),
                    ("third_party_notices", "Contents/Resources/NOTICES", False),
                    ("metadata_collision", reserved, False),
                )
                for index, (role, destination, native) in enumerate(entries):
                    data = valid_macho() if native else ("audited fixture " + role).encode()
                    name = "input-" + str(index)
                    (root / name).write_bytes(data)
                    artifacts.append({"role": role, "input_path": name,
                                      "relative_path": destination, "macho": native,
                                      "executable": role in ("application", "native_worker"),
                                      "size_bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()})
                manifest = root / "manifest.json"
                manifest.write_text(json.dumps({"schema_version": 1, "platform": "darwin-arm64",
                                                "bundle_ready": True, "required_roles": [],
                                                "artifacts": artifacts}))
                output = root / "WA Indexer.app"
                with mock.patch.object(pathlib.Path, "mkdir", side_effect=AssertionError("output creation reached")), mock.patch.object(assembler, "native_dependencies"), mock.patch.object(
                        assembler.shutil, "disk_usage", return_value=SimpleNamespace(free=16 * 1024**3)):
                    with self.assertRaises(ValueError):
                        assembler.assemble(str(manifest), str(root), str(output))
                self.assertFalse(output.exists())

    def test_incomplete_frozen_manifest_creates_no_bundle(self):
        manifest = pathlib.Path(__file__).resolve().parents[2] / "packaging/release-manifest.json"
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            output = root / "WA Indexer.app"
            with self.assertRaises(ValueError):
                assembler.assemble(str(manifest), str(root), str(output))
            self.assertFalse(output.exists())

    def test_changed_artifact_hash_fails_before_output_creation(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            source = root / "application"
            source.write_bytes(valid_macho())
            manifest = root / "manifest.json"
            manifest.write_text(json.dumps({
                "schema_version": 1, "platform": "darwin-arm64", "bundle_ready": True,
                "required_roles": ["application"],
                "artifacts": [{"role": "application", "input_path": "application",
                               "relative_path": "Contents/MacOS/wa-indexer",
                               "macho": True, "executable": True,
                               "size_bytes": source.stat().st_size, "sha256": "0" * 64}]
            }))
            output = root / "WA Indexer.app"
            with self.assertRaises(ValueError):
                assembler.assemble(str(manifest), str(root), str(output))
            self.assertFalse(output.exists())

    def test_escaping_bundle_relative_path_is_rejected(self):
        for value in ("/tmp/external", "../external", "Contents/../external", "Contents//MacOS"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                assembler.relative(value)

    def test_growing_source_never_writes_beyond_declared_allocation(self):
        class Growing:
            read_bytes = 0
            def read(self, amount):
                self.read_bytes += amount
                return b"x" * amount
        source = Growing()
        output = io.BytesIO()
        with self.assertRaises(ValueError):
            assembler.audited_digest(source, 7, output)
        self.assertEqual(output.getvalue(), b"x" * 7)
        self.assertEqual(source.read_bytes, 8)

    def test_launcher_rejects_tampered_plist_and_wrong_role(self):
        data = (pathlib.Path(__file__).resolve().parents[2] / "packaging/macos/Info.plist").read_bytes()
        app = {"role": "application", "relative_path": "Contents/MacOS/wa-indexer", "macho": True, "executable": True}
        assembler.launcher([app], data)
        tampered = plistlib.loads(data)
        tampered["CFBundleExecutable"] = "unreviewed"
        with self.assertRaises(ValueError):
            assembler.launcher([app], plistlib.dumps(tampered))
        with self.assertRaises(ValueError):
            assembler.launcher([{**app, "role": "native_worker"}], data)

    def test_loader_path_accepts_only_existing_declared_inside_bundle(self):
        with tempfile.TemporaryDirectory() as temp:
            bundle = pathlib.Path(temp) / "Test.app"
            native = bundle / "Contents/MacOS/wa-indexer"
            library = bundle / "Contents/Frameworks/runtime.dylib"
            native.parent.mkdir(parents=True)
            library.parent.mkdir()
            native.write_bytes(valid_macho())
            library.write_bytes(valid_macho())
            def run(command, **kwargs):
                text = "arm64" if command[0].endswith("lipo") else str(native) + ":\n\t" + dependency + " (compatibility version 1.0)\n"
                return SimpleNamespace(stdout=text)
            for dependency, accepted in (("@loader_path/../Frameworks/runtime.dylib", True), ("@loader_path/../../../outside", False), ("/usr/lib/../../tmp/evil.dylib", False), ("@loader_path/../Frameworks/missing.dylib", False)):
                with self.subTest(dependency=dependency), mock.patch.object(assembler.subprocess, "run", run):
                    if accepted:
                        assembler.native_dependencies(native, bundle, {native.resolve(), library.resolve()})
                    else:
                        with self.assertRaises(ValueError):
                            assembler.native_dependencies(native, bundle, {native.resolve(), library.resolve()})


class ASRRoleAdmissionTests(unittest.TestCase):
    """Role policy only: synthetic headers/evidence, never native qualification."""
    def fixture(self, root, worker=False):
        entries = [("application", "Contents/MacOS/wa-indexer", True, True),
                   ("native_runtime", "Contents/Frameworks/speech", True, True),
                   ("native_runtime", "Contents/Frameworks/decoder", True, True),
                   ("license", "Contents/Resources/LICENSE", False, False),
                   ("third_party_notices", "Contents/Resources/NOTICES", False, False),
                   ("smoke_evidence", "Contents/Resources/synthetic-smoke.json", False, False)]
        if worker:
            entries.append(("native_worker", "Contents/MacOS/worker", True, True))
        artifacts = []
        for index, (role, destination, native, executable) in enumerate(entries):
            body = valid_macho() if native else b"SYNTHETIC ROLE-POLICY UNIT FIXTURE; NOT NATIVE EVIDENCE"
            name = "input-" + str(index)
            (root / name).write_bytes(body)
            artifacts.append({"role": role, "input_path": name, "relative_path": destination,
                              "macho": native, "executable": executable,
                              "size_bytes": len(body), "sha256": hashlib.sha256(body).hexdigest()})
        return {"schema_version": 1, "platform": "darwin-arm64", "bundle_ready": True,
                "release_ready": False, "selected_encoder_profile": None,
                "required_roles": ["application", "native_runtime", "license", "third_party_notices", "smoke_evidence"],
                "artifacts": artifacts,
                "ai_profiles": [{"kind": "speech", "profile_id": "ivrit-whisper-large-v3-turbo",
                                 "model_profile_id": "ivrit-whisper-large-v3-turbo", "language": "he",
                                 "runtime_artifacts": {"speech": "Contents/Frameworks/speech",
                                                       "decoder": "Contents/Frameworks/decoder"}}]}

    def saved_bundle(self, root, manifest):
        bundle = root / "Synthetic Role Fixture.app"
        for artifact in manifest["artifacts"]:
            destination = bundle / artifact["relative_path"]
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(root / artifact["input_path"], destination)
            destination.chmod(0o500 if artifact["executable"] else 0o400)
        plist = bundle / "Contents/Info.plist"
        plist.write_bytes((pathlib.Path(__file__).resolve().parents[2] / "packaging/macos/Info.plist").read_bytes())
        plist.chmod(0o400)
        saved = bundle / "Contents/release-manifest.json"
        saved.write_text(json.dumps(manifest))
        saved.chmod(0o400)
        # These independent fixtures meet the loader's private-directory gate;
        # role/deployment tests must reach their intended later admission gate.
        bundle.chmod(0o700)
        for directory in bundle.rglob("*"):
            if directory.is_dir():
                directory.chmod(0o700)
        return bundle

    def tool(self, command, **kwargs):
        if command[0] == "/usr/bin/lipo":
            text = "arm64\n"
        elif command[0] == "/usr/bin/otool":
            text = str(command[-1]) + ":\n\t/usr/lib/libSystem.B.dylib (compatibility version 1.0)\n"
        else:
            raise AssertionError("unexpected external tool")
        return SimpleNamespace(stdout=text)

    def test_every_bundle_ancestor_is_private_before_loading(self):
        for boundary in ("assembly", "verify_contents", "verify_nested_resources"):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root)
                manifest["artifacts"][-1]["relative_path"] = "Contents/Resources/ASR/synthetic-smoke.json"
                manifest["artifacts"][3]["relative_path"] = "Contents/Resources/licenses/native/whisper/LICENSE"
                with mock.patch.object(assembler.subprocess, "run", self.tool), mock.patch.object(
                        assembler.shutil, "disk_usage", return_value=SimpleNamespace(free=16 * 1024**3)):
                    if boundary == "assembly":
                        path = root / "manifest.json"
                        path.write_text(json.dumps(manifest))
                        bundle = root / "Synthetic Private Fixture.app"
                        previous_umask = os.umask(0o022)
                        try:
                            assembler.assemble(str(path), str(root), str(bundle))
                        finally:
                            os.umask(previous_umask)
                        directory_modes = {str(p.relative_to(bundle)): stat.S_IMODE(p.stat().st_mode)
                                           for p in [bundle, *bundle.rglob("*")] if p.is_dir()}
                        self.assertTrue(all(mode == 0o700 for mode in directory_modes.values()),
                                        "bundle_ancestor_not_private")
                        verifier.verify(str(bundle))
                    else:
                        bundle = self.saved_bundle(root, manifest)
                        bad = bundle / ("Contents" if boundary == "verify_contents" else "Contents/Resources")
                        bad.chmod(0o755)
                        with self.assertRaisesRegex(ValueError, "^bundle_input_not_audited$"):
                            verifier.verify(str(bundle))

    def test_asr_only_inventory_accepts_application_holder_without_encoder_worker(self):
        for boundary in ("assembly", "verification"):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root)
                with mock.patch.object(assembler.subprocess, "run", self.tool), mock.patch.object(
                        assembler.shutil, "disk_usage", return_value=SimpleNamespace(free=16 * 1024**3)):
                    try:
                        if boundary == "assembly":
                            path = root / "manifest.json"
                            path.write_text(json.dumps(manifest))
                            bundle = root / "Synthetic Role Fixture.app"
                            assembler.assemble(str(path), str(root), str(bundle))
                        else:
                            bundle = self.saved_bundle(root, manifest)
                            verifier.verify(str(bundle))
                    except ValueError:
                        self.fail("asr_role_inventory_refused_without_encoder_worker")
                self.assertTrue((bundle / "Contents/MacOS/wa-indexer").is_file())
                self.assertFalse((bundle / "Contents/MacOS/worker").exists())

    def test_encoder_indicators_and_explicit_worker_requirement_still_deny_missing_worker(self):
        for indicator in ("kind", "selected", "runtime", "profile_artifact", "explicit_role"):
            with self.subTest(indicator=indicator), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root)
                if indicator == "kind":
                    manifest["ai_profiles"][0]["kind"] = "encoder"
                elif indicator == "selected":
                    manifest["selected_encoder_profile"] = "darwin-arm64-fp32-feasibility"
                elif indicator == "runtime":
                    manifest["ai_profiles"][0]["runtime_artifacts"]["ort"] = "Contents/Frameworks/speech"
                elif indicator == "profile_artifact":
                    manifest["artifacts"][-1]["role"] = "encoder_profile"
                    manifest["required_roles"].remove("smoke_evidence")
                else:
                    manifest["required_roles"].append("native_worker")
                bundle = self.saved_bundle(root, manifest)
                with mock.patch.object(verifier.assembler.subprocess, "run", self.tool):
                    with self.assertRaisesRegex(ValueError, "^bundle_input_not_audited$"):
                        verifier.verify(str(bundle))

    def test_unknown_profile_and_required_roles_never_satisfy_closed_requirements(self):
        for fault in ("unknown_kind", "unknown_required_role", "worker_role_typo"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root, worker=fault in ("unknown_kind", "worker_role_typo"))
                if fault == "unknown_kind":
                    manifest["ai_profiles"][0]["kind"] = "speech_future"
                elif fault == "unknown_required_role":
                    manifest["required_roles"].append("future_required_role")
                else:
                    manifest["ai_profiles"][0]["kind"] = "encoder"
                    manifest["artifacts"][-1]["role"] = "native_workerr"
                bundle = self.saved_bundle(root, manifest)
                with mock.patch.object(verifier.assembler.subprocess, "run", self.tool):
                    with self.assertRaisesRegex(ValueError, "^bundle_input_not_audited$"):
                        verifier.verify(str(bundle))


if __name__ == "__main__":
    unittest.main()
