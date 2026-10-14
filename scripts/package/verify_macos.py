#!/usr/bin/env python3
"""Developer-only static bundle audit; never executes an unverified application."""
import hashlib
import importlib.util
import json
import os
import pathlib
import stat
import subprocess
import sys

spec = importlib.util.spec_from_file_location("assembler", pathlib.Path(__file__).with_name("assemble_macos.py"))
assembler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assembler)


def verify(bundle_path):
    bundle = pathlib.Path(bundle_path)
    if not bundle.is_absolute() or bundle.is_symlink() or not bundle.is_dir():
        assembler.fail()
    assembler.private_directory(bundle)
    assembler.private_directory(bundle / "Contents")
    manifests = [bundle / name for name in ("Contents/release-manifest.json", "Contents/candidate-manifest.json")
                 if (bundle / name).exists() or (bundle / name).is_symlink()]
    if len(manifests) != 1:
        assembler.fail()
    manifest_path = manifests[0]
    fd = os.open(manifest_path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o400 or info.st_size > 65536:
            assembler.fail()
        data = source.read(65537)
        after = os.fstat(source.fileno())
        if len(data) != info.st_size or (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
            assembler.fail()
        manifest = json.loads(data, object_pairs_hook=assembler.verify_cloudflared.unique_object)
        basic = assembler.bundle_profile(manifest) == "basic"
        if basic and not assembler.directory_has_no_acl(source.fileno()):
            assembler.fail()
    if manifest.get("schema_version") != 1 or manifest.get("platform") != "darwin-arm64" or manifest.get("bundle_ready") is not True:
        assembler.fail()
    if manifest_path != bundle / assembler.manifest_destination(manifest):
        assembler.fail()
    artifacts = manifest.get("artifacts")
    if not isinstance(artifacts, list) or not artifacts:
        assembler.fail()
    required = assembler.required_roles(manifest)
    roles = set()
    machos = []
    declared = set()
    folded_declared = set()
    for artifact in artifacts:
        relative = assembler.relative(artifact["relative_path"])
        if (relative.parts[0] != "Contents" or str(relative).casefold() in folded_declared
                or str(relative).casefold() in assembler.RESERVED_PATHS):
            assembler.fail()
        declared.add(str(relative))
        folded_declared.add(str(relative).casefold())
        cursor = bundle
        for part in relative.parts:
            cursor = cursor / part
            if cursor.is_symlink():
                assembler.fail()
            if cursor != bundle / relative:
                assembler.private_directory(cursor)
        fd = os.open(cursor, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as source:
            info = os.fstat(source.fileno())
            expected_mode = 0o500 if artifact.get("executable") else 0o400
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != expected_mode or info.st_size != artifact["size_bytes"]:
                assembler.fail()
            if basic and not assembler.directory_has_no_acl(source.fileno()):
                assembler.fail()
            if assembler.audited_digest(source, artifact["size_bytes"], macho=artifact.get("macho")) != artifact["sha256"]:
                assembler.fail()
            after = os.fstat(source.fileno())
            if (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
                assembler.fail()
        roles.add(artifact["role"])
        if artifact["role"] in ("application", "native_worker", "native_runtime") and not artifact.get("macho"):
            assembler.fail()
        if artifact.get("macho"):
            machos.append(cursor)
    if not required.issubset(roles) or "Contents/MacOS/wa-indexer" not in declared:
        assembler.fail()
    plist = bundle / "Contents/Info.plist"
    fd = os.open(plist, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o400 or info.st_size > 65536:
            assembler.fail()
        if basic and not assembler.directory_has_no_acl(source.fileno()):
            assembler.fail()
        data = source.read(65537)
        if len(data) != info.st_size:
            assembler.fail()
        assembler.launcher(manifest["artifacts"], data)
        after = os.fstat(source.fileno())
        if (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
            assembler.fail()
    native_paths = {path.resolve() for path in machos}
    for native in machos:
        assembler.native_dependencies(native, bundle, native_paths)
    if assembler.bundle_profile(manifest) == "basic":
        expected = declared | {"Contents/Info.plist", assembler.manifest_destination(manifest)}
        directories = {str(parent) for name in expected for parent in pathlib.PurePosixPath(name).parents
                       if str(parent) != "."}
        for entry in bundle.rglob("*"):
            name = str(entry.relative_to(bundle))
            if entry.is_symlink() or name not in (directories if entry.is_dir() else expected):
                assembler.fail()
    return bundle


if __name__ == "__main__":
    try:
        if len(sys.argv) != 2:
            assembler.fail()
        verify(sys.argv[1])
        print("Static bundle integrity audited. Runtime and clean-machine gates remain NOT_RUN.")
    except Exception:
        print("Bundle verification refused: exact audited private inputs required.", file=sys.stderr)
        sys.exit(1)
