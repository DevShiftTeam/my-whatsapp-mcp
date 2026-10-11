"""Synthetic deployment admission; Apple tools are not executed by this suite."""
import hashlib
import importlib.util
import json
import os
import pathlib
import shutil
import struct
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock


HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("deployment_assembler", HERE / "assemble_macos.py")
assembler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assembler)
verify_spec = importlib.util.spec_from_file_location("deployment_verifier", HERE / "verify_macos.py")
verifier = importlib.util.module_from_spec(verify_spec)
verify_spec.loader.exec_module(verifier)


def build_version(minimum=0x000E0000, sdk=0x001B0000, platform=1,
                  endian="<", tools=()):
    return struct.pack(endian + "6I", 0x32, 24 + 8 * len(tools), platform,
                       minimum, sdk, len(tools)) + b"".join(
                           struct.pack(endian + "2I", *tool) for tool in tools)


def legacy_version(minimum=0x000E0000, sdk=0x001B0000, endian="<"):
    return struct.pack(endian + "4I", 0x24, 16, minimum, sdk)


def thin(commands=None, endian="<", bits=64, cpu=0x0100000C,
         subtype=0, count=None, extent=None):
    commands = [build_version(endian=endian)] if commands is None else commands
    body = b"".join(commands)
    header = [0xFEEDFACF if bits == 64 else 0xFEEDFACE, cpu, subtype, 2,
              len(commands) if count is None else count,
              len(body) if extent is None else extent, 0]
    if bits == 64:
        header.append(0)
    return struct.pack(endian + str(len(header)) + "I", *header) + body


def fat(slices, endian=">", bits=32):
    entry_size = 20 if bits == 32 else 32
    start = 8 + entry_size * len(slices)
    table, payload = [], bytearray()
    for cpu, subtype, body in slices:
        offset = start + len(payload)
        if bits == 32:
            table.append(struct.pack(endian + "5I", cpu, subtype, offset, len(body), 0))
        else:
            table.append(struct.pack(endian + "2I2Q2I", cpu, subtype, offset, len(body), 0, 0))
        payload.extend(body)
    magic = 0xCAFEBABE if bits == 32 else 0xCAFEBABF
    return struct.pack(endian + "2I", magic, len(slices)) + b"".join(table) + payload


def loader_tool(command, **unused):
    if command[0] == "/usr/bin/lipo":
        return SimpleNamespace(stdout="arm64\n")
    if command[0] == "/usr/bin/otool":
        return SimpleNamespace(stdout=str(command[-1]) + ":\n\t/usr/lib/libSystem.B.dylib (compatibility version 1.0)\n")
    raise AssertionError("unexpected external operation")


class MachODeploymentTests(unittest.TestCase):
    def audit(self, body):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            native = root / "fixture"
            native.write_bytes(body)
            native.chmod(0o500)
            with mock.patch.object(assembler.subprocess, "run", loader_tool):
                assembler.native_dependencies(native, root, {native.resolve()})

    def refuses(self, body):
        with self.assertRaisesRegex(ValueError, "^bundle_input_not_audited$"):
            self.audit(body)

    def test_accepts_minimum_14_with_sdk_27_and_lower_legacy_minima(self):
        # SDK selection is not the minimum. These literals are independent of
        # the production parser and include the current captured SDK27 shape.
        for minimum in (0x000E0000, 0x000D0601):
            for command in (build_version(minimum=minimum, tools=((3, 0x00110000),)),
                            legacy_version(minimum=minimum)):
                with self.subTest(minimum=minimum, command=command[:4]):
                    self.audit(thin([command]))

    def test_rejects_current_minimum_27_and_any_minimum_above_14(self):
        for minimum in (0x001B0000, 0x000F0000, 0x000E0001, 0xFFFFFFFF):
            for command in (build_version(minimum=minimum, tools=((3, 0x00110000),)),
                            legacy_version(minimum=minimum)):
                with self.subTest(minimum=minimum, command=command[:4]):
                    self.refuses(thin([command]))

    def test_rejects_missing_duplicate_and_conflicting_minimum_commands(self):
        valid = build_version()
        for commands in ([], [valid, valid], [valid, legacy_version()],
                         [legacy_version(), legacy_version()],
                         [valid, build_version(minimum=0x001B0000)]):
            with self.subTest(commands=len(commands)):
                self.refuses(thin(commands))

    def test_rejects_non_macos_platforms_and_zero_minimum(self):
        for platform in (0, 2, 6, 7, 0xFFFFFFFF):
            with self.subTest(platform=platform):
                self.refuses(thin([build_version(platform=platform)]))
        for command in (build_version(minimum=0), legacy_version(minimum=0)):
            with self.subTest(zero=command[:4]):
                self.refuses(thin([command]))

    def test_accepts_both_thin_word_sizes_and_endian_encodings(self):
        for endian in ("<", ">"):
            for bits, cpu in ((64, 0x0100000C), (32, 12)):
                with self.subTest(endian=endian, bits=bits):
                    self.audit(thin([legacy_version(endian=endian)],
                                    endian=endian, bits=bits, cpu=cpu))

    def test_variable_commands_and_tool_records_are_validated(self):
        other = struct.pack("<4I", 0x1B, 24, 1, 2) + b"\x00" * 8
        self.audit(thin([other, build_version(tools=((1, 0x001B0000), (3, 0x00110000)))]))
        for command in (struct.pack("<6I", 0x32, 24, 1, 0x000E0000, 0x001B0000, 1),
                        struct.pack("<6I", 0x32, 24, 1, 0x000E0000, 0x001B0000, 0xFFFFFFFF),
                        struct.pack("<4I", 0x24, 24, 0x000E0000, 0x001B0000) + b"\x00" * 8,
                        struct.pack("<6I", 0x32, 20, 1, 0x000E0000, 0x001B0000, 0),
                        struct.pack("<2I", 0x1B, 7)):
            with self.subTest(command=command.hex()):
                self.refuses(thin([command]))

    def test_rejects_truncated_headers_and_command_extents(self):
        valid = thin()
        for body in (b"", valid[:4], valid[:28], valid[:-1],
                     thin(count=2), thin(count=0), thin(extent=32),
                     thin(count=4097), thin(extent=0x100001),
                     thin(extent=0xFFFFFFFF)):
            with self.subTest(length=len(body)):
                self.refuses(body)

    def test_accepts_fat_encodings_and_checks_every_slice(self):
        arm = (0x0100000C, 0, thin())
        intel = (0x01000007, 3, thin(cpu=0x01000007, subtype=3))
        for endian in ("<", ">"):
            for bits in (32, 64):
                with self.subTest(endian=endian, bits=bits):
                    self.audit(fat([arm, intel], endian=endian, bits=bits))

    def test_rejects_higher_or_wrong_platform_in_any_fat_slice(self):
        good = (0x0100000C, 0, thin())
        for bad in (thin([build_version(minimum=0x001B0000)], cpu=0x01000007, subtype=3),
                    thin([build_version(platform=2)], cpu=0x01000007, subtype=3),
                    thin([], cpu=0x01000007, subtype=3)):
            for bits in (32, 64):
                for order in ([good, (0x01000007, 3, bad)], [(0x01000007, 3, bad), good]):
                    with self.subTest(bits=bits, bad=bad.hex()[:32]):
                        self.refuses(fat(order, bits=bits))

    def test_rejects_fat_table_bounds_overlap_and_architecture_mismatch(self):
        slices = [(0x0100000C, 0, thin()), (0x01000007, 3, thin(cpu=0x01000007, subtype=3))]
        for bits in (32, 64):
            good = fat(slices, bits=bits)
            entry_size = 20 if bits == 32 else 32
            offset_field = 16
            offset_format = ">I" if bits == 32 else ">Q"
            for offset in (0, 8, len(good) + 1, (1 << bits) - 1):
                damaged = bytearray(good)
                struct.pack_into(offset_format, damaged, offset_field, offset)
                with self.subTest(bits=bits, offset=offset):
                    self.refuses(bytes(damaged))
            # The second slice overlaps the first even though each header is valid.
            damaged = bytearray(good)
            first_offset = 8 + entry_size * 2
            struct.pack_into(offset_format, damaged, offset_field + entry_size, first_offset)
            with self.subTest(bits=bits, fault="overlap"):
                self.refuses(bytes(damaged))
            damaged = bytearray(good)
            struct.pack_into(">I", damaged, 8, 0x01000007)
            with self.subTest(bits=bits, fault="architecture"):
                self.refuses(bytes(damaged))
            with self.subTest(bits=bits, fault="truncated table"):
                self.refuses(good[:8 + entry_size * 2 - 1])
        for magic, count in ((0xCAFEBABE, 0), (0xCAFEBABF, 0xFFFFFFFF)):
            with self.subTest(magic=magic, count=count):
                self.refuses(struct.pack(">2I", magic, count))

    def test_metadata_reads_are_bounded_instead_of_reading_the_payload(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            native = root / "fixture"
            with native.open("wb") as destination:
                destination.write(thin())
                destination.truncate(8 * 1024**2)
            native.chmod(0o500)
            real_fdopen = os.fdopen
            reads = {"calls": 0, "bytes": 0}
            class BoundedFile:
                def __init__(self, source):
                    self.source = source
                def __enter__(self):
                    return self
                def __exit__(self, *args):
                    self.source.close()
                def read(self, amount=-1):
                    if amount < 0 or amount > 1024**2:
                        raise AssertionError("unbounded Mach-O payload read")
                    result = self.source.read(amount)
                    reads["calls"] += 1
                    reads["bytes"] += len(result)
                    if reads["bytes"] > 4096:
                        raise AssertionError("Mach-O payload streamed beyond metadata budget")
                    return result
                def __getattr__(self, name):
                    return getattr(self.source, name)
            def fdopen(fd, *args, **kwargs):
                return BoundedFile(real_fdopen(fd, *args, **kwargs))
            with mock.patch.object(assembler.os, "fdopen", fdopen), mock.patch.object(
                    assembler.subprocess, "run", loader_tool):
                assembler.native_dependencies(native, root, {native.resolve()})
            self.assertGreater(reads["calls"], 0)
            self.assertLessEqual(reads["bytes"], 4096)

    def test_deployment_is_read_from_the_open_inode_after_path_replacement(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            native = root / "fixture"
            native.write_bytes(thin([build_version(minimum=0x001B0000)]))
            native.chmod(0o500)
            replacement = root / "replacement"
            replacement.write_bytes(thin())
            replacement.chmod(0o500)
            real_fdopen = os.fdopen
            class ReplacingFile:
                def __init__(self, source):
                    self.source = source
                    self.first = True
                def __enter__(self):
                    return self
                def __exit__(self, *args):
                    self.source.close()
                def read(self, amount=-1):
                    if self.first:
                        self.first = False
                        os.replace(replacement, native)
                    return self.source.read(amount)
                def __getattr__(self, name):
                    return getattr(self.source, name)
            def fdopen(fd, *args, **kwargs):
                return ReplacingFile(real_fdopen(fd, *args, **kwargs))
            with mock.patch.object(assembler.os, "fdopen", fdopen), mock.patch.object(
                    assembler.subprocess, "run", loader_tool):
                with self.assertRaisesRegex(ValueError, "^bundle_input_not_audited$"):
                    assembler.native_dependencies(native, root, {native.resolve()})


class DeploymentBundleBoundaryTests(unittest.TestCase):
    def fixture(self, root, application_body):
        entries = (("application", "Contents/MacOS/wa-indexer", application_body, True),
                   ("native_worker", "Contents/MacOS/worker", thin(), True),
                   ("native_runtime", "Contents/Frameworks/runtime.dylib", thin(), False),
                   ("license", "Contents/Resources/LICENSE", b"synthetic license", False),
                   ("third_party_notices", "Contents/Resources/NOTICES", b"synthetic notices", False))
        artifacts = []
        for index, (role, destination, body, executable) in enumerate(entries):
            name = "artifact-" + str(index)
            (root / name).write_bytes(body)
            artifacts.append({"role": role, "input_path": name, "relative_path": destination,
                              "macho": role in ("application", "native_worker", "native_runtime"),
                              "executable": executable, "size_bytes": len(body),
                              "sha256": hashlib.sha256(body).hexdigest()})
        manifest = {"schema_version": 1, "platform": "darwin-arm64", "bundle_ready": True,
                    "required_roles": [], "artifacts": artifacts}
        path = root / "manifest.json"
        path.write_text(json.dumps(manifest))
        return path, manifest

    def test_assembly_rejects_hash_pinned_minimum_27_before_creating_output(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            manifest_path, unused = self.fixture(root, thin([build_version(minimum=0x001B0000)]))
            output = root / "WA Indexer.app"
            with mock.patch.object(assembler.subprocess, "run", loader_tool), mock.patch.object(
                    assembler.shutil, "disk_usage", return_value=SimpleNamespace(free=16 * 1024**3)), mock.patch.object(
                        pathlib.Path, "mkdir", side_effect=AssertionError("output created before deployment audit")):
                with self.assertRaisesRegex(ValueError, "^bundle_input_not_audited$"):
                    assembler.assemble(str(manifest_path), str(root), str(output))
            self.assertFalse(output.exists())

    def test_independent_verifier_rejects_hash_pinned_minimum_27(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            unused, manifest = self.fixture(root, thin([build_version(minimum=0x001B0000)]))
            bundle = root / "Already Assembled.app"
            for artifact in manifest["artifacts"]:
                destination = bundle / artifact["relative_path"]
                destination.parent.mkdir(parents=True, exist_ok=True)
                # A deployment regression must reach the MachO gate, rather
                # than fail on a fixture-only nonprivate ancestor first.
                for directory in [bundle, *bundle.rglob("*")]:
                    if directory.is_dir():
                        directory.chmod(0o700)
                shutil.copyfile(root / artifact["input_path"], destination)
                destination.chmod(0o500 if artifact["executable"] else 0o400)
            plist = bundle / "Contents/Info.plist"
            plist.write_bytes((HERE.parents[1] / "packaging/macos/Info.plist").read_bytes())
            plist.chmod(0o400)
            saved_manifest = bundle / "Contents/release-manifest.json"
            saved_manifest.write_text(json.dumps(manifest))
            saved_manifest.chmod(0o400)
            with mock.patch.object(verifier.assembler.subprocess, "run", loader_tool):
                with self.assertRaisesRegex(ValueError, "^bundle_input_not_audited$"):
                    verifier.verify(str(bundle))


if __name__ == "__main__":
    unittest.main()
