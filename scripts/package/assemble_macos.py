#!/usr/bin/env python3
"""Developer-only assembler. The delivered app has no Python dependency.

Usage: assemble_macos.py AUDITED_MANIFEST INPUT_ROOT NEW_OUTPUT.app
No building, downloading, signing, PATH lookup, or release-gate substitution.
"""
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import stat
import struct
import subprocess
import sys
import ctypes
import verify_cloudflared


REPOSITORY = pathlib.Path(__file__).resolve().parents[2]
RESERVED_PATHS = {"contents/info.plist", "contents/release-manifest.json", "contents/candidate-manifest.json"}
BASIC_PATHS = {"application": "Contents/MacOS/wa-indexer", "cloudflared": "Contents/MacOS/cloudflared",
               "license": "Contents/Resources/LICENSE", "logo": "Contents/Resources/logo.png",
               "cloudflared_manifest": "Contents/Resources/notices/cloudflared/manifest.json"}
BASIC_ROLES = set(BASIC_PATHS) | {"third_party_notices"}
BASIC_CORE_ROLES = BASIC_ROLES - {"cloudflared", "cloudflared_manifest"}


def fail():
    raise ValueError("bundle_input_not_audited")


def directory_has_no_acl(fd):
    # Match the bundled-AI consumer's fixed Darwin attrreference-header audit.
    # No ACL body or path reopen is needed; errors refuse the directory.
    if sys.platform != "darwin":
        return sys.platform == "linux"
    class Attrlist(ctypes.Structure):
        _fields_ = [("bitmapcount", ctypes.c_uint16), ("reserved", ctypes.c_uint16),
                    ("commonattr", ctypes.c_uint32), ("volattr", ctypes.c_uint32),
                    ("dirattr", ctypes.c_uint32), ("fileattr", ctypes.c_uint32),
                    ("forkattr", ctypes.c_uint32)]
    try:
        library = ctypes.CDLL("/usr/lib/libSystem.B.dylib", use_errno=True)
        library.fgetattrlist.argtypes = [ctypes.c_int, ctypes.POINTER(Attrlist),
                                        ctypes.c_void_p, ctypes.c_size_t, ctypes.c_uint]
        library.fgetattrlist.restype = ctypes.c_int
        attributes = Attrlist(5, 0, 0x00400000, 0, 0, 0, 0)
        buffer = ctypes.create_string_buffer(12)
        return (library.fgetattrlist(fd, ctypes.byref(attributes), buffer, len(buffer), 0) == 0
                and struct.unpack("=3I", buffer.raw) == (12, 8, 0))
    except (OSError, AttributeError):
        return False


def private_directory(directory):
    before = directory.lstat()
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        opened = os.fstat(fd)
        current = directory.lstat()
        if (any(not stat.S_ISDIR(entry.st_mode) or entry.st_uid != os.getuid()
                or stat.S_IMODE(entry.st_mode) != 0o700 for entry in (before, opened, current))
                or (before.st_dev, before.st_ino) != (opened.st_dev, opened.st_ino)
                or (current.st_dev, current.st_ino) != (opened.st_dev, opened.st_ino)
                or not directory_has_no_acl(fd)):
            fail()
    finally:
        os.close(fd)


def private_parents(bundle, destination):
    cursor = bundle
    private_directory(cursor)
    for part in destination.relative_to(bundle).parts:
        cursor = cursor / part
        try:
            cursor.mkdir(mode=0o700)
        except FileExistsError:
            pass
        private_directory(cursor)


def required_roles(manifest):
    """Speech uses the application holder; encoder mappings need their worker."""
    profile = bundle_profile(manifest)
    explicit = manifest.get("required_roles", [])
    if not isinstance(explicit, list) or any(not isinstance(role, str) or not role for role in explicit):
        fail()
    if profile == "basic":
        remote_helper = manifest.get("remote_helper", "pinned")
        if remote_helper not in ("none", "pinned"):
            fail()
        required = BASIC_CORE_ROLES if remote_helper == "none" else BASIC_ROLES
        if (manifest.get("kind") != "unsigned_local_basic_candidate" or manifest.get("release_ready") is not False
                or manifest.get("selected_encoder_profile") is not None or manifest.get("ai_profiles", []) != []
                or not set(explicit).issubset(required)):
            fail()
        if remote_helper == "none":
            if manifest.get("remote_helper_version") is not None:
                fail()
        else:
            # Older BASIC manifests always required the pinned helper. Missing
            # selection must never become permission to silently strip it.
            legacy_version = verify_cloudflared.VERSION if "remote_helper" not in manifest else None
            if manifest.get("remote_helper_version", legacy_version) != verify_cloudflared.VERSION:
                fail()
            pins = verify_cloudflared.load_manifest(REPOSITORY / "packaging/cloudflared-manifest.json")
            pin = next(a for a in pins["assets"] if a["platform"] == "darwin/arm64")
        seen = set()
        for artifact in manifest["artifacts"]:
            role = artifact.get("role")
            path = artifact.get("relative_path")
            if role not in required:
                fail()
            if role == "third_party_notices":
                if (not isinstance(path, str) or (path != "Contents/Resources/THIRD_PARTY_NOTICES"
                        and (remote_helper == "none" or not path.startswith("Contents/Resources/notices/cloudflared/")))):
                    fail()
            elif path != BASIC_PATHS[role] or role in seen:
                fail()
            seen.add(role)
            native = role in ("application", "cloudflared")
            if artifact.get("macho") is not native or artifact.get("executable") is not native:
                fail()
            if role == "cloudflared" and (artifact.get("size_bytes") != pin["executable_size"]
                    or artifact.get("sha256") != pin["executable_sha256"]):
                fail()
        if not any(a.get("role") == "third_party_notices" and a.get("relative_path") == "Contents/Resources/THIRD_PARTY_NOTICES"
                   for a in manifest["artifacts"]):
            fail()
        if remote_helper == "pinned" and not any(a.get("role") == "third_party_notices"
                and a.get("relative_path") == "Contents/Resources/notices/cloudflared/LICENSE" for a in manifest["artifacts"]):
            fail()
        return required
    required = {"application", "native_runtime", "license", "third_party_notices"}
    required.update(explicit)
    selected = manifest.get("selected_encoder_profile")
    if selected is not None and not isinstance(selected, str):
        fail()
    profiles = manifest.get("ai_profiles", [])
    if not isinstance(profiles, list) or len(profiles) > 8:
        fail()
    encoder = bool(selected) or any(a.get("role") == "encoder_profile" for a in manifest["artifacts"])
    for profile in profiles:
        if not isinstance(profile, dict):
            fail()
        kind = profile.get("kind")
        if not isinstance(kind, str) or kind not in {"encoder", "ocr", "caption", "speech"}:
            fail()
        runtime = profile.get("runtime_artifacts", {})
        if not isinstance(runtime, dict):
            fail()
        encoder = encoder or kind == "encoder" or "ort" in runtime
    if encoder:
        required.add("native_worker")
    return required


def bundle_profile(manifest):
    profile = manifest.get("bundle_profile", "native-ai")
    if profile not in ("basic", "native-ai"):
        fail()
    return profile


def manifest_destination(manifest):
    return "Contents/candidate-manifest.json" if bundle_profile(manifest) == "basic" else "Contents/release-manifest.json"


def relative(value):
    if not isinstance(value, str):
        fail()
    p = pathlib.PurePosixPath(value)
    if p.is_absolute() or not p.parts or any(x in (".", "..") for x in p.parts) or str(p) != value:
        fail()
    return p


def audit_macho_deployment(source, size):
    """Bounded metadata on this held descriptor; SDK never raises the minimum."""
    if type(size) is not int or size < 0 or size > 8 * 1024**3:
        fail()
    thin_formats = {b"\xce\xfa\xed\xfe": ("<", 28), b"\xfe\xed\xfa\xce": (">", 28),
                    b"\xcf\xfa\xed\xfe": ("<", 32), b"\xfe\xed\xfa\xcf": (">", 32)}
    fat_formats = {b"\xca\xfe\xba\xbe": (">", 20), b"\xbe\xba\xfe\xca": ("<", 20),
                   b"\xca\xfe\xba\xbf": (">", 32), b"\xbf\xba\xfe\xca": ("<", 32)}
    command_budget = 1024**2

    def read(offset, amount, end):
        if amount < 0 or amount > 1024**2 or offset < 0 or end > size or offset > end - amount:
            fail()
        source.seek(offset)
        body = source.read(amount)
        if len(body) != amount:
            fail()
        return body

    def image(start, length, architecture=None):
        nonlocal command_budget
        end = start + length
        magic = read(start, 4, end)
        if magic not in thin_formats:
            fail()
        endian, header_size = thin_formats[magic]
        words = struct.unpack(endian + ("8I" if header_size == 32 else "7I"),
                              read(start, header_size, end))
        cpu, subtype, kind, count, extent = words[1:6]
        if (kind not in (1, 2, 6, 8) or count > 4096 or extent > command_budget
                or (header_size == 32 and words[7] != 0)
                or (architecture is not None and architecture != (cpu, subtype))):
            fail()
        command_budget -= extent
        commands = read(start + header_size, extent, end)
        offset = 0
        version = None
        for unused in range(count):
            if offset > extent - 8:
                fail()
            command, command_size = struct.unpack_from(endian + "2I", commands, offset)
            if command_size < 8 or command_size % (8 if header_size == 32 else 4) or command_size > extent - offset:
                fail()
            if command in (0x25, 0x2F, 0x30):  # Other Apple platform legacy minima.
                fail()
            if command in (0x24, 0x32):
                if version is not None:
                    fail()
                if command == 0x24:
                    if command_size != 16:
                        fail()
                    minimum, sdk = struct.unpack_from(endian + "2I", commands, offset + 8)
                else:
                    if command_size < 24:
                        fail()
                    platform, minimum, sdk, tools = struct.unpack_from(endian + "4I", commands, offset + 8)
                    if platform != 1 or tools > 4096 or command_size != 24 + 8 * tools:
                        fail()
                if minimum == 0 or minimum > 0x000E0000:
                    fail()
                version = (minimum, sdk)
            offset += command_size
        if offset != extent or version is None:
            fail()
        return version

    magic = read(0, 4, size)
    if magic in thin_formats:
        return [image(0, size)]
    if magic not in fat_formats:
        fail()
    endian, entry_size = fat_formats[magic]
    count = struct.unpack(endian + "2I", read(0, 8, size))[1]
    if count == 0 or count > 16:
        fail()
    table_end = 8 + count * entry_size
    table = read(8, count * entry_size, size)
    slices = []
    architectures = set()
    for index in range(count):
        entry = struct.unpack_from(endian + ("5I" if entry_size == 20 else "2I2Q2I"),
                                   table, index * entry_size)
        cpu, subtype, offset, length, alignment = entry[:5]
        if (alignment > 31 or offset < table_end or length == 0 or offset > size - length
                or offset % (1 << alignment) or (entry_size == 32 and entry[5] != 0)
                or (cpu, subtype) in architectures):
            fail()
        architectures.add((cpu, subtype))
        slices.append((offset, length, (cpu, subtype)))
    previous_end = table_end
    for offset, length, unused in sorted(slices):
        if offset < previous_end:
            fail()
        previous_end = offset + length
    return [image(offset, length, architecture) for offset, length, architecture in slices]


def audited_digest(source, size, destination=None, macho=False):
    # Exactly the declared allocation can be written, even if a producer keeps
    # appending to this same inode. One extra byte only probes for overflow.
    if type(size) is not int or size < 0 or size > 8 * 1024**3:
        fail()
    if macho:
        before = os.fstat(source.fileno())
        if (not stat.S_ISREG(before.st_mode) or before.st_nlink != 1
                or before.st_uid != os.getuid() or before.st_size != size):
            fail()
    remaining = size
    digest = hashlib.sha256()
    while remaining:
        chunk = source.read(min(remaining, 1024 * 1024))
        if not chunk or len(chunk) > remaining:
            fail()
        remaining -= len(chunk)
        digest.update(chunk)
        if destination is not None:
            destination.write(chunk)
    if source.read(1):
        fail()
    if macho:
        audit_macho_deployment(source, size)
        after = os.fstat(source.fileno())
        fields = ("st_dev", "st_ino", "st_mode", "st_nlink", "st_uid", "st_size",
                  "st_mtime_ns", "st_ctime_ns")
        if any(getattr(before, field) != getattr(after, field) for field in fields):
            fail()
    return digest.hexdigest()


def launcher(artifacts, plist_bytes):
    entries = [a for a in artifacts if a.get("role") == "application"]
    if len(entries) != 1 or entries[0].get("relative_path") != "Contents/MacOS/wa-indexer" or entries[0].get("macho") is not True or entries[0].get("executable") is not True:
        fail()
    info = plistlib.loads(plist_bytes)
    expected = plistlib.loads((pathlib.Path(__file__).resolve().parents[2] / "packaging/macos/Info.plist").read_bytes())
    if (info != expected or info.get("CFBundleExecutable") != "wa-indexer"
            or info.get("LSMinimumSystemVersion") != "14.0"):
        fail()


def native_dependencies(native, bundle, native_paths):
    # Independent defensive metadata gate for direct callers. The authoritative
    # assembly/verifier gate runs on their SAME hash-audited descriptor.
    def identity(info):
        return (info.st_dev, info.st_ino, info.st_mode, info.st_nlink, info.st_uid,
                info.st_size, info.st_mtime_ns, info.st_ctime_ns)

    fd = os.open(native, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        # The buffered wrapper borrows this raw descriptor; its constructor or
        # close cannot suppress the sole explicit descriptor-close attempt.
        with os.fdopen(fd, "rb", closefd=False) as source:
            before = os.fstat(fd)
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_uid != os.getuid():
                fail()
            if identity(os.stat(native, follow_symlinks=False)) != identity(before):
                fail()
            audit_macho_deployment(source, before.st_size)
            if (identity(os.fstat(fd)) != identity(before)
                    or identity(os.stat(native, follow_symlinks=False)) != identity(before)):
                fail()
    finally:
        os.close(fd)
    arch = subprocess.run(["/usr/bin/lipo", "-archs", str(native)], check=True, capture_output=True, text=True).stdout.strip()
    if arch != "arm64":
        fail()
    lines = subprocess.run(["/usr/bin/otool", "-L", str(native)], check=True, capture_output=True, text=True).stdout.splitlines()[1:]
    for line in lines:
        dep = line.strip().split(" (", 1)[0]
        if dep.startswith(("/usr/lib/", "/System/Library/")):
            path = pathlib.PurePosixPath(dep)
            if str(path) != dep or any(part in (".", "..") for part in path.parts):
                fail()
            continue
        if dep.startswith("@loader_path/"):
            resolved = (native.parent / dep[len("@loader_path/"):]).resolve()
            if resolved.is_relative_to(bundle.resolve()) and resolved.is_file() and resolved in native_paths:
                continue
        fail()


def assemble(manifest_path, input_root, output):
    fd = os.open(manifest_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as manifest_file:
        manifest_info = os.fstat(fd)
        if not stat.S_ISREG(manifest_info.st_mode) or manifest_info.st_nlink != 1 or manifest_info.st_size > 65536:
            fail()
        manifest_bytes = manifest_file.read(65537)
        after = os.fstat(fd)
        if (len(manifest_bytes) != manifest_info.st_size or (manifest_info.st_dev, manifest_info.st_ino,
                manifest_info.st_size, manifest_info.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)):
            fail()
        manifest = json.loads(manifest_bytes, object_pairs_hook=verify_cloudflared.unique_object)
        if bundle_profile(manifest) == "basic" and (manifest_info.st_uid != os.getuid()
                or stat.S_IMODE(manifest_info.st_mode) != 0o600 or not directory_has_no_acl(fd)):
            fail()
    if manifest.get("schema_version") != 1 or manifest.get("platform") != "darwin-arm64" or manifest.get("bundle_ready") is not True:
        fail()
    artifacts = manifest.get("artifacts")
    if not isinstance(artifacts, list) or not artifacts:
        fail()
    required = required_roles(manifest)
    basic = bundle_profile(manifest) == "basic"
    root = pathlib.Path(input_root)
    target = pathlib.Path(output)
    if not root.is_absolute() or root.is_symlink() or not root.is_dir() or target.exists() or target.is_symlink() or not target.is_absolute() or target.suffix != ".app":
        fail()
    if basic:
        private_directory(root)
        private_directory(target.parent)
    verified = []
    roles = set()
    destinations = set()
    folded_destinations = set()
    for a in artifacts:
        src = root.joinpath(*relative(a["input_path"]).parts)
        dst = relative(a["relative_path"])
        if str(dst).casefold() in RESERVED_PATHS or str(dst).casefold() in folded_destinations or dst.parts[0] != "Contents":
            fail()
        destinations.add(str(dst))
        folded_destinations.add(str(dst).casefold())
        cursor = root
        for part in src.relative_to(root).parts:
            cursor = cursor / part
            if cursor.is_symlink():
                fail()
            if basic and cursor != src:
                private_directory(cursor)
        fd = os.open(src, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as source:
            before = os.fstat(source.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size != a["size_bytes"]:
                fail()
            if basic and (before.st_uid != os.getuid() or stat.S_IMODE(before.st_mode) not in (
                    (0o500, 0o700) if a["executable"] else (0o400, 0o600)) or not directory_has_no_acl(source.fileno())):
                fail()
            digest = audited_digest(source, a["size_bytes"], macho=a.get("macho"))
            after = os.fstat(source.fileno())
            if digest != a["sha256"] or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
                fail()
        if a["role"] in ("application", "native_worker", "native_runtime") and a.get("macho") is not True:
            fail()
        roles.add(a["role"])
        verified.append((src, dst, a, (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)))
    if not required.issubset(roles) or "Contents/MacOS/wa-indexer" not in destinations:
        fail()
    plist = pathlib.Path(__file__).resolve().parents[2] / "packaging/macos/Info.plist"
    plist_bytes = plist.read_bytes()
    launcher(artifacts, plist_bytes)
    growth = sum(a["size_bytes"] for _, _, a, _ in verified) + len(plist_bytes) + len(manifest_bytes)
    if growth > 8 * 1024**3 or growth + 256 * 1024**2 > shutil.disk_usage(target.parent).free:
        fail()
    # A new private directory is created only after every input passes its audit.
    target.mkdir(mode=0o700)
    try:
        for src, dst, a, identity in verified:
            out = target.joinpath(*dst.parts)
            private_parents(target, out.parent)
            fd = os.open(src, os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(fd, "rb") as source:
                current = os.fstat(source.fileno())
                if (current.st_dev, current.st_ino, current.st_size, current.st_mtime_ns) != identity:
                    fail()
                with out.open("xb") as dest:
                    if audited_digest(source, a["size_bytes"], dest) != a["sha256"]:
                        fail()
                    dest.flush()
                    os.fsync(dest.fileno())
            after = os.stat(src, follow_symlinks=False)
            if (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) != identity:
                fail()
            out.chmod(0o500 if a.get("executable") else 0o400)
        (target / "Contents/Info.plist").write_bytes(plist_bytes)
        (target / "Contents/Info.plist").chmod(0o400)
        # Native library references must resolve inside this exact bundle or be
        # Apple system dependencies. Ambient @rpath lookup is never accepted.
        native_paths = {target.joinpath(*dst.parts).resolve() for _, dst, a, _ in verified if a.get("macho")}
        for native in native_paths:
            native_dependencies(native, target, native_paths)
        saved_manifest = target / manifest_destination(manifest)
        saved_manifest.write_bytes(manifest_bytes)
        saved_manifest.chmod(0o400)
    except Exception:
        # Only this newly created output is owned by this invocation.
        shutil.rmtree(target)
        raise


if __name__ == "__main__":
    try:
        if len(sys.argv) != 4:
            fail()
        assemble(*sys.argv[1:])
    except Exception:
        print("Package assembly refused: audited complete inputs are required.", file=sys.stderr)
        sys.exit(1)
