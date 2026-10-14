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
import verify_cloudflared

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


class BasicProfileTests(unittest.TestCase):
    """Synthetic candidate inventories, not runtime or distribution evidence."""
    def fixture(self, root, profile="basic"):
        entries = [("application", "Contents/MacOS/wa-indexer", True),
                   ("cloudflared", "Contents/MacOS/cloudflared", True),
                   ("license", "Contents/Resources/LICENSE", False),
                   ("third_party_notices", "Contents/Resources/THIRD_PARTY_NOTICES", False),
                   ("logo", "Contents/Resources/logo.png", False),
                   ("cloudflared_manifest", "Contents/Resources/notices/cloudflared/manifest.json", False),
                   ("third_party_notices", "Contents/Resources/notices/cloudflared/LICENSE", False)]
        artifacts = []
        for index, (role, destination, native) in enumerate(entries):
            body = valid_macho() if native else b"SYNTHETIC BASIC PACKAGE FIXTURE"
            name = "input-" + str(index)
            source = root / name
            source.write_bytes(body)
            source.chmod(0o500 if native else 0o400)
            artifacts.append({"role": role, "input_path": name, "relative_path": destination,
                              "macho": native, "executable": native, "size_bytes": len(body),
                              "sha256": hashlib.sha256(body).hexdigest()})
        manifest = {"schema_version": 1, "platform": "darwin-arm64", "bundle_ready": True,
                    "kind": "unsigned_local_basic_candidate", "bundle_profile": profile,
                    "release_ready": False, "runtime_qualification": "NOT_RUN",
                    "selected_encoder_profile": None, "ai_profiles": [], "required_roles": [],
                    "artifacts": artifacts}
        return manifest

    def pins(self, manifest):
        helper = next(a for a in manifest["artifacts"] if a["role"] == "cloudflared")
        return {"assets": [{"platform": "darwin/arm64", "executable_size": helper["size_bytes"],
                            "executable_sha256": helper["sha256"]}]}

    def boundary(self, root, manifest, boundary):
        if boundary == "assembly":
            path = root / "candidate.json"
            path.write_text(json.dumps(manifest))
            path.chmod(0o600)
            bundle = root / "Synthetic Basic.app"
            assembler.assemble(str(path), str(root), str(bundle))
            return bundle
        bundle = ASRRoleAdmissionTests().saved_bundle(root, manifest)
        if manifest.get("bundle_profile") == "basic":
            (bundle / "Contents/release-manifest.json").rename(bundle / "Contents/candidate-manifest.json")
        return verifier.verify(str(bundle))

    def test_basic_accepts_without_native_ai_and_uses_separate_candidate_manifest(self):
        for boundary in ("assembly", "verification"):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root)
                with mock.patch.object(verify_cloudflared, "load_manifest", return_value=self.pins(manifest)), mock.patch.object(
                        assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                    bundle = self.boundary(root, manifest, boundary)
                self.assertTrue((bundle / "Contents/candidate-manifest.json").is_file())
                self.assertFalse((bundle / "Contents/release-manifest.json").exists())
                self.assertFalse((bundle / "Contents/Frameworks").exists())
                self.assertEqual(stat.S_IMODE(bundle.stat().st_mode), 0o700)

    def test_explicit_no_helper_basic_accepts_without_reading_helper_pins(self):
        for boundary in ("assembly", "verification"):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root)
                manifest["remote_helper"] = "none"
                manifest["artifacts"] = [a for a in manifest["artifacts"] if a["role"] not in ("cloudflared", "cloudflared_manifest")
                                         and not a["relative_path"].startswith("Contents/Resources/notices/cloudflared/")]
                with mock.patch.object(verify_cloudflared, "load_manifest", side_effect=AssertionError("helper pins must not be read")), mock.patch.object(
                        assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                    bundle = self.boundary(root, manifest, boundary)
                self.assertFalse((bundle / "Contents/MacOS/cloudflared").exists())
                self.assertFalse((bundle / "Contents/Resources/notices/cloudflared").exists())
                self.assertEqual(stat.S_IMODE((bundle / "Contents/MacOS/wa-indexer").stat().st_mode), 0o500)

    def test_remote_helper_selection_cannot_lie_or_silently_strip_pinned_inventory(self):
        faults = ("none_with_helper", "none_with_notice", "none_requires_helper", "none_with_version",
                  "unknown", "missing_pinned_helper", "missing_legacy_helper", "missing_pinned_version", "wrong_version", "missing_helper_notice")
        for fault in faults:
            for boundary in ("assembly", "verification"):
                with self.subTest(fault=fault, boundary=boundary), tempfile.TemporaryDirectory() as temp:
                    root = pathlib.Path(temp)
                    manifest = self.fixture(root)
                    pins = self.pins(manifest)
                    manifest["remote_helper"] = "pinned"
                    manifest["remote_helper_version"] = verify_cloudflared.VERSION
                    if fault.startswith("none_"):
                        manifest["remote_helper"] = "none"
                        del manifest["remote_helper_version"]
                        if fault != "none_with_helper":
                            manifest["artifacts"] = [a for a in manifest["artifacts"] if a["role"] not in ("cloudflared", "cloudflared_manifest")
                                                     and not a["relative_path"].startswith("Contents/Resources/notices/cloudflared/")]
                        if fault == "none_with_notice":
                            manifest["artifacts"].append({**manifest["artifacts"][0], "role": "third_party_notices", "macho": False,
                                                          "executable": False, "relative_path": "Contents/Resources/notices/cloudflared/LICENSE"})
                        elif fault == "none_requires_helper":
                            manifest["required_roles"] = ["cloudflared"]
                        elif fault == "none_with_version":
                            manifest["remote_helper_version"] = verify_cloudflared.VERSION
                    elif fault == "unknown":
                        manifest["remote_helper"] = "automatic"
                    elif fault in ("missing_pinned_helper", "missing_legacy_helper"):
                        manifest["artifacts"] = [a for a in manifest["artifacts"] if a["role"] != "cloudflared"]
                        if fault == "missing_legacy_helper":
                            del manifest["remote_helper"]
                            del manifest["remote_helper_version"]
                    elif fault == "missing_pinned_version":
                        del manifest["remote_helper_version"]
                    elif fault == "wrong_version":
                        manifest["remote_helper_version"] = "latest"
                    else:
                        manifest["artifacts"] = [a for a in manifest["artifacts"]
                                                 if a["relative_path"] != "Contents/Resources/notices/cloudflared/LICENSE"]
                    with mock.patch.object(verify_cloudflared, "load_manifest", return_value=pins), mock.patch.object(
                            assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                        with self.assertRaises(ValueError):
                            self.boundary(root, manifest, boundary)

    def test_correctly_pinned_helper_with_minimum_15_still_refuses_both_boundaries(self):
        for boundary in ("assembly", "verification"):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root)
                helper = manifest["artifacts"][1]
                body = valid_macho()[:-12] + struct.pack("<3I", 0x000F0000, 0x000F0200, 0)
                (root / helper["input_path"]).chmod(0o700)
                (root / helper["input_path"]).write_bytes(body)
                helper["sha256"] = hashlib.sha256(body).hexdigest()
                with mock.patch.object(verify_cloudflared, "load_manifest", return_value=self.pins(manifest)), mock.patch.object(
                        assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                    with self.assertRaises(ValueError):
                        self.boundary(root, manifest, boundary)

    def test_omitted_and_native_ai_profiles_still_require_native_runtime(self):
        for profile in (None, "native-ai"):
            for boundary in ("assembly", "verification"):
                with self.subTest(profile=profile, boundary=boundary), tempfile.TemporaryDirectory() as temp:
                    root = pathlib.Path(temp)
                    manifest = self.fixture(root, profile)
                    if profile is None:
                        del manifest["bundle_profile"]
                    with self.assertRaises(ValueError):
                        self.boundary(root, manifest, boundary)

    def test_explicit_native_ai_preserves_existing_speech_profile_admission(self):
        for boundary in ("assembly", "verification"):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = ASRRoleAdmissionTests().fixture(root)
                old_required = assembler.required_roles(manifest)
                manifest["bundle_profile"] = "native-ai"
                self.assertEqual(assembler.required_roles(manifest), old_required)
                with mock.patch.object(assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                    bundle = self.boundary(root, manifest, boundary)
                self.assertTrue((bundle / "Contents/release-manifest.json").is_file())

    def test_unknown_bundle_profile_refuses_otherwise_valid_native_inventory(self):
        for profile in ("basik", "BASIC", None, 1):
            for boundary in ("assembly", "verification"):
                with self.subTest(profile=profile, boundary=boundary), tempfile.TemporaryDirectory() as temp:
                    root = pathlib.Path(temp)
                    manifest = ASRRoleAdmissionTests().fixture(root)
                    manifest["bundle_profile"] = profile
                    with mock.patch.object(assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                        with self.assertRaises(ValueError):
                            self.boundary(root, manifest, boundary)

    def test_basic_rejects_ai_selection_unknown_roles_and_release_claims(self):
        faults = ("selected_encoder", "ai_profiles", "native_runtime", "native_worker", "encoder_profile",
                  "model", "unknown_role", "required_ai", "release_ready", "wrong_kind", "missing_helper", "missing_notices")
        for fault in faults:
            for boundary in ("assembly", "verification"):
                with self.subTest(fault=fault, boundary=boundary), tempfile.TemporaryDirectory() as temp:
                    root = pathlib.Path(temp)
                    manifest = self.fixture(root)
                    pins = self.pins(manifest)
                    if fault == "selected_encoder":
                        manifest["selected_encoder_profile"] = "selected"
                    elif fault == "ai_profiles":
                        manifest["ai_profiles"] = [{"kind": "speech"}]
                    elif fault == "required_ai":
                        manifest["required_roles"] = ["native_runtime"]
                    elif fault == "release_ready":
                        manifest["release_ready"] = True
                    elif fault == "wrong_kind":
                        manifest["kind"] = "release"
                    elif fault == "missing_helper":
                        manifest["artifacts"] = [a for a in manifest["artifacts"] if a["role"] != "cloudflared"]
                    elif fault == "missing_notices":
                        manifest["artifacts"][3]["relative_path"] = "Contents/Resources/notices/cloudflared/only-one-notice"
                    else:
                        manifest["artifacts"].append({**manifest["artifacts"][0], "role": fault,
                                                      "relative_path": "Contents/Resources/extra"})
                    with mock.patch.object(verify_cloudflared, "load_manifest", return_value=pins), mock.patch.object(
                            assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                        with self.assertRaises(ValueError):
                            self.boundary(root, manifest, boundary)

    def test_basic_rejects_file_and_manifest_acl_or_nonprivate_manifest(self):
        for fault in ("artifact_acl", "manifest_acl", "manifest_mode"):
            for boundary in ("assembly", "verification"):
                with self.subTest(fault=fault, boundary=boundary), tempfile.TemporaryDirectory() as temp:
                    root = pathlib.Path(temp)
                    manifest = self.fixture(root)
                    if boundary == "assembly":
                        path = root / "candidate.json"
                        path.write_text(json.dumps(manifest))
                        path.chmod(0o600)
                        module = assembler
                        call = lambda: assembler.assemble(str(path), str(root), str(root / "Refused.app"))
                    else:
                        bundle = ASRRoleAdmissionTests().saved_bundle(root, manifest)
                        path = bundle / "Contents/candidate-manifest.json"
                        (bundle / "Contents/release-manifest.json").rename(path)
                        module = verifier.assembler
                        call = lambda: verifier.verify(str(bundle))
                    if fault == "manifest_mode":
                        path.chmod(0o644)
                    no_acl = module.directory_has_no_acl
                    inode = path.stat().st_ino
                    def acl(fd):
                        info = os.fstat(fd)
                        denied = info.st_ino == inode if fault == "manifest_acl" else info.st_ino != inode
                        return False if stat.S_ISREG(info.st_mode) and denied else no_acl(fd)
                    with mock.patch.object(verify_cloudflared, "load_manifest", return_value=self.pins(manifest)), mock.patch.object(
                            module, "directory_has_no_acl", side_effect=acl if fault != "manifest_mode" else no_acl):
                        with self.assertRaises(ValueError):
                            call()

    def test_basic_keeps_hash_macho_owner_private_and_helper_pin_checks(self):
        for fault in ("hash", "macho", "deployment", "helper_pin", "private_file", "owner"):
            for boundary in ("assembly", "verification"):
                with self.subTest(fault=fault, boundary=boundary), tempfile.TemporaryDirectory() as temp:
                    root = pathlib.Path(temp)
                    manifest = self.fixture(root)
                    app = manifest["artifacts"][0]
                    pins = self.pins(manifest)
                    if fault == "hash":
                        app["sha256"] = "0" * 64
                    elif fault == "macho":
                        app["macho"] = False
                    elif fault == "deployment":
                        body = valid_macho()[:-12] + struct.pack("<3I", 0x000F0000, 0x001B0000, 0)
                        (root / app["input_path"]).chmod(0o700)
                        (root / app["input_path"]).write_bytes(body)
                        app["sha256"] = hashlib.sha256(body).hexdigest()
                    elif fault == "helper_pin":
                        pins["assets"][0]["executable_sha256"] = "0" * 64
                    elif fault == "private_file":
                        (root / app["input_path"]).chmod(0o755)
                    with mock.patch.object(verify_cloudflared, "load_manifest", return_value=pins), mock.patch.object(
                            assembler.subprocess, "run", ASRRoleAdmissionTests().tool), mock.patch.object(
                            assembler.os, "getuid", return_value=os.getuid() + 1 if fault == "owner" else os.getuid()):
                        if fault == "private_file" and boundary == "verification":
                            bundle = ASRRoleAdmissionTests().saved_bundle(root, manifest)
                            (bundle / "Contents/release-manifest.json").rename(bundle / "Contents/candidate-manifest.json")
                            (bundle / app["relative_path"]).chmod(0o755)
                            with self.assertRaises(ValueError):
                                verifier.verify(str(bundle))
                        else:
                            with self.assertRaises(ValueError):
                                self.boundary(root, manifest, boundary)

    def test_candidate_metadata_is_reserved_and_manifest_profile_cannot_be_relabelled(self):
        for fault in ("reserved", "wrong_manifest_path", "both_manifests", "undeclared_ai"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp)
                manifest = self.fixture(root)
                with mock.patch.object(verify_cloudflared, "load_manifest", return_value=self.pins(manifest)), mock.patch.object(
                        assembler.subprocess, "run", ASRRoleAdmissionTests().tool):
                    if fault == "reserved":
                        manifest["artifacts"].append({**manifest["artifacts"][2],
                                                      "relative_path": "Contents/CANDIDATE-MANIFEST.JSON"})
                        with self.assertRaises(ValueError):
                            self.boundary(root, manifest, "assembly")
                        self.assertFalse((root / "Synthetic Basic.app").exists())
                    else:
                        bundle = self.boundary(root, manifest, "assembly")
                        candidate = bundle / "Contents/candidate-manifest.json"
                        if fault == "wrong_manifest_path":
                            candidate.rename(bundle / "Contents/release-manifest.json")
                        elif fault == "both_manifests":
                            shutil.copyfile(candidate, bundle / "Contents/release-manifest.json")
                        else:
                            (bundle / "Contents/Resources/unlisted-model").write_bytes(b"not declared")
                        with self.assertRaises(ValueError):
                            verifier.verify(str(bundle))


if __name__ == "__main__":
    unittest.main()
