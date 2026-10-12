#!/usr/bin/env python3
"""Build/package-time pinned helper acquisition and verification; never startup.

The manifest pins upstream bytes, not signed or reproducible provenance. A
qualified path protects against other-user replacement; the trusted installing
owner can still replace its own executable between verification and exec.
"""
import argparse
import ctypes
import gzip
import hashlib
import io
import json
import os
import pathlib
import platform
import stat
import struct
import subprocess
import tarfile
import threading
import urllib.request

VERSION = "2026.10.0"
REVISION = "18cdfe0a6fc7b72a0702d255a1f984e776ce0498"
MAX_ASSET = 41_000_000


def fail():
    raise ValueError("cloudflared_not_qualified")


def unique_object(pairs):
    result = {}
    for k, v in pairs:
        if k in result:
            fail()
        result[k] = v
    return result


def validate_manifest(m):
    if not isinstance(m, dict) or m.get("schema_version") != 1 or m.get("version") != VERSION or m.get("source_revision") != REVISION:
        fail()
    assets = m.get("assets")
    if not isinstance(assets, list) or len(assets) != 3 or any(not isinstance(a, dict) for a in assets) or {a.get("platform") for a in assets} != {"linux/amd64", "linux/arm64", "darwin/arm64"}:
        fail()
    for a in assets:
        suffix = a["platform"].replace("/", "-") + (".tgz" if a["platform"] == "darwin/arm64" else "")
        if a.get("url") != f"https://github.com/cloudflare/cloudflared/releases/download/{VERSION}/cloudflared-{suffix}":
            fail()
        if a.get("archive_format") != ("tgz" if suffix.endswith(".tgz") else "executable"):
            fail()
        for prefix in ("archive", "executable"):
            size, digest = a.get(prefix+"_size"), a.get(prefix+"_sha256")
            if type(size) is not int or not 0 < size <= MAX_ASSET or not isinstance(digest, str) or len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
                fail()
    return m


def safe_acl(fd):
    if platform.system() != "Darwin":
        return True  # POSIX access ACL writes obey the group-class mode mask.
    # Same bounded Darwin attrreference/kauth_filesec contract as app verifier.
    class Attrlist(ctypes.Structure):
        _fields_ = [("count", ctypes.c_uint16), ("reserved", ctypes.c_uint16),
                    ("common", ctypes.c_uint32), ("volume", ctypes.c_uint32),
                    ("directory", ctypes.c_uint32), ("file", ctypes.c_uint32),
                    ("fork", ctypes.c_uint32)]
    attrs = Attrlist(5, 0, 0x00400000, 0, 0, 0, 0)
    buf = ctypes.create_string_buffer(12+44+24*128)
    libc = ctypes.CDLL("/usr/lib/libSystem.B.dylib", use_errno=True)
    libc.fgetattrlist.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_size_t, ctypes.c_ulong]
    if libc.fgetattrlist(fd, ctypes.byref(attrs), buf, len(buf), 0) != 0:
        return False
    length, offset, acl_length = struct.unpack_from("=III", buf.raw)
    if offset != 8 or length < 12 or length > len(buf) or acl_length+12 != length:
        return False
    if acl_length == 0:
        return True
    data = buf.raw[12:length]
    if len(data) < 44 or struct.unpack_from("=I", data)[0] != 0x012cc16d:
        return False
    entries = struct.unpack_from("=I", data, 36)[0]
    if entries == 0xffffffff:
        return len(data) == 44
    return entries <= 128 and len(data) == 44+24*entries and all(struct.unpack_from("=I", data, 44+24*i+16)[0] & 15 == 2 for i in range(entries))


def qualified_path(path, leaf=True):
    path = pathlib.Path(path)
    if not path.is_absolute() or str(path) != os.path.normpath(path):
        fail()
    for index, current in enumerate([path, *path.parents]):
        st = current.lstat()
        if st.st_uid not in (0, os.geteuid()) or st.st_mode & 0o022 or stat.S_ISLNK(st.st_mode):
            fail()
        fd = os.open(current, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            opened = os.fstat(fd)
            if (opened.st_dev, opened.st_ino) != (st.st_dev, st.st_ino) or not safe_acl(fd):
                fail()
        finally:
            os.close(fd)
        if index == 0 and leaf:
            if not stat.S_ISREG(st.st_mode):
                fail()
        elif not stat.S_ISDIR(st.st_mode):
            fail()
    return path.lstat()


def read_private(path, limit):
    before = qualified_path(path)
    if before.st_size > limit:
        fail()
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as f:
        opened = os.fstat(f.fileno())
        if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            fail()
        data = f.read(limit+1)
        if len(data) > limit:
            fail()
        after = qualified_path(path)
        if (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
            fail()
        return data


def load_manifest(path):
    return validate_manifest(json.loads(read_private(path, 16384), object_pairs_hook=unique_object))


def checked_bytes(data, asset, prefix):
    if len(data) != asset[prefix+"_size"] or hashlib.sha256(data).hexdigest() != asset[prefix+"_sha256"]:
        fail()
    return data


def extract(data, size):
    # Bound decompression before tar parsing, including padding/metadata. Never
    # extract arbitrary filesystem entries or trust tarfile path handling.
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(data)) as gz:
            raw = gz.read(size+10241)
        if len(raw) > size+10240:
            fail()
        with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as tf:
            members = tf.getmembers()
            if len(members) != 1:
                fail()
            member = members[0]
            if member.name != "cloudflared" or not member.isreg() or member.size != size or member.pax_headers:
                fail()
            with tf.extractfile(member) as f:
                result = f.read(size+1)
            if len(result) != size:
                fail()
            return result
    except (OSError, EOFError, tarfile.TarError):
        fail()


def architecture(data, target):
    if target.startswith("linux/"):
        machine = 62 if target == "linux/amd64" else 183
        if len(data) < 64 or data[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", data, 18)[0] != machine:
            fail()
    elif target == "darwin/arm64":
        if len(data) < 32 or data[:4] != b"\xcf\xfa\xed\xfe" or struct.unpack_from("<I", data, 4)[0] != 0x100000c:
            fail()
    else:
        fail()


def probe(path, target):
    native = platform.system().lower()+"/"+{"aarch64":"arm64", "arm64":"arm64", "x86_64":"amd64"}.get(platform.machine(), "unsupported")
    if native != target:
        fail()
    qualified_path(path)
    cmd = subprocess.Popen([str(path), "--version"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env={"LANG":"C", "LC_ALL":"C"}, close_fds=True)
    captured = bytearray()
    def drain():
        while chunk := cmd.stdout.read(1024):
            captured.extend(chunk[:max(0, 1024-len(captured))])
    thread = threading.Thread(target=drain, daemon=True)
    thread.start()
    try:
        cmd.wait(timeout=3)
    except subprocess.TimeoutExpired:
        cmd.kill()
        cmd.wait()
        fail()
    finally:
        thread.join(timeout=1)
    if thread.is_alive() or cmd.returncode != 0 or not bytes(captured).startswith(("cloudflared version "+VERSION+" ").encode()):
        fail()
    return VERSION


def acquire(asset):
    # No ambient proxies, no owner credentials. Redirects remain HTTPS on the
    # official release hosts; payload length and bytes must exactly match pin.
    class Redirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            from urllib.parse import urlparse
            u = urlparse(newurl)
            if u.scheme != "https" or u.hostname not in ("github.com", "release-assets.githubusercontent.com") or u.username or u.password:
                fail()
            return super().redirect_request(req, fp, code, msg, headers, newurl)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), Redirect())
    with opener.open(asset["url"], timeout=30) as response:
        data = response.read(asset["archive_size"]+1)
    return checked_bytes(data, asset, "archive")


def install(manifest, target, output, asset_path=None, version_probe=False):
    a = next((a for a in manifest["assets"] if a["platform"] == target), None)
    if a is None:
        fail()
    data = checked_bytes(read_private(asset_path, a["archive_size"]), a, "archive") if asset_path else acquire(a)
    executable = extract(data, a["executable_size"]) if a["archive_format"] == "tgz" else data
    checked_bytes(executable, a, "executable")
    architecture(executable, target)
    qualified_path(output.parent, leaf=False)
    fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o755)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(executable)
            f.flush()
            os.fsync(f.fileno())
            os.fchmod(f.fileno(), 0o755)
        if version_probe:
            probe(output, target)
    except BaseException:
        output.unlink()
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", type=pathlib.Path)
    parser.add_argument("platform")
    parser.add_argument("output", type=pathlib.Path)
    parser.add_argument("--asset", type=pathlib.Path)
    parser.add_argument("--probe", action="store_true")
    args = parser.parse_args()
    try:
        install(load_manifest(args.manifest.absolute()), args.platform, args.output.absolute(), args.asset.absolute() if args.asset else None, args.probe)
    except (ValueError, OSError, StopIteration, KeyError, TypeError, json.JSONDecodeError):
        raise SystemExit("cloudflared_not_qualified") from None
    print("cloudflared_integrity_verified" + ("; native_version_verified" if args.probe else "; native_execution_not_run"))

if __name__ == "__main__":
    main()
