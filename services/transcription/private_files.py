"""Private files and an intentionally small, non-shell environment format."""

import os
from pathlib import Path
import re
import stat
import sys
import ctypes
import errno
import struct
import fcntl
from contextlib import contextmanager

ENV_KEYS = frozenset({
    "WA_TRANSCRIPTION_TOKEN", "WA_TRANSCRIPTION_LISTEN", "WA_TRANSCRIPTION_MODEL_DIR",
    "WA_TRANSCRIPTION_LANGUAGE", "WA_TRANSCRIPTION_CPU_THREADS", "WA_TRANSCRIPTION_BODY_TIMEOUT_SECONDS",
    "WA_INDEXER_TRANSCRIPTION_URL", "WA_INDEXER_TRANSCRIPTION_TOKEN",
    "WA_INDEXER_TRANSCRIPTION_LANGUAGE", "WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS",
})


def no_extra_acl(path):
    if sys.platform == "darwin":
        class Attributes(ctypes.Structure):
            _fields_ = [("bitmapcount", ctypes.c_ushort), ("reserved", ctypes.c_ushort),
                        ("commonattr", ctypes.c_uint32), ("volattr", ctypes.c_uint32),
                        ("dirattr", ctypes.c_uint32), ("fileattr", ctypes.c_uint32), ("forkattr", ctypes.c_uint32)]
        attributes = Attributes(bitmapcount=5, commonattr=0x00400000)
        buffer = ctypes.create_string_buffer(12)
        libc = ctypes.CDLL(None, use_errno=True)
        call = libc.fgetattrlist
        call.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_size_t, ctypes.c_ulong]
        call.restype = ctypes.c_int
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            if call(descriptor, ctypes.byref(attributes), buffer, 12, 0) != 0:
                raise ValueError("unsafe_acl")
        finally:
            os.close(descriptor)
        if struct.unpack("=III", buffer.raw) != (12, 8, 0):
            raise ValueError("unsafe_acl")
    elif sys.platform.startswith("linux"):
        for name in ("system.posix_acl_access", "system.posix_acl_default"):
            try:
                if os.getxattr(path, name, follow_symlinks=False):
                    raise ValueError("unsafe_acl")
            except OSError as error:
                if error.errno not in (errno.ENODATA, errno.ENOTSUP):
                    raise ValueError("unsafe_acl") from None
    else:
        raise ValueError("unsupported_private_files")


def private_directory(path, *, create=False):
    path = Path(path)
    if not path.is_absolute() or path.is_symlink() or any(ord(character) < 32 for character in str(path)):
        raise ValueError("unsafe_directory")
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("unsafe_directory")
    # A symlink in an ancestor must not redirect a private service path.
    if path.resolve() != path:
        raise ValueError("unsafe_directory")
    no_extra_acl(path)
    return path


def private_file(path, *, max_bytes=None):
    path = Path(path)
    if not path.is_absolute() or path.resolve() != path:
        raise ValueError("unsafe_file")
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
        raise ValueError("unsafe_file")
    if max_bytes is not None and info.st_size > max_bytes:
        raise ValueError("file_too_large")
    no_extra_acl(path)
    return info


def write_private(path, data):
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
    except BaseException:
        Path(path).unlink(missing_ok=True)
        raise


@contextmanager
def service_lock(data_dir, name="service.lock"):
    data_dir = private_directory(data_dir)
    if name not in ("service.lock", "setup.lock"):
        raise ValueError("invalid_lock_name")
    path = data_dir / name
    descriptor = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NONBLOCK | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        private_file(path, max_bytes=0)
        opened, named = os.fstat(descriptor), path.stat()
        if (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino):
            raise ValueError("unsafe_lock")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise ValueError("service_already_running") from None
        yield
    finally:
        os.close(descriptor)


def cleanup_uploads(upload_dir):
    upload_dir = private_directory(upload_dir, create=True)
    paths = list(upload_dir.iterdir())
    # Validate the entire directory before deleting only this service's previous uploads.
    for path in paths:
        if not re.fullmatch(r"voice-[A-Za-z0-9_-]+\.audio", path.name):
            raise ValueError("unexpected_upload_file")
        private_file(path, max_bytes=25 * 1024 * 1024)
    for path in paths:
        path.unlink()
    return upload_dir


def load_env_file(path):
    path = Path(path)
    private_directory(path.parent)
    private_file(path, max_bytes=16384)
    result = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if (not separator or key not in ENV_KEYS or key in result or not value
                or value != value.strip() or value.startswith(("'", '"'))
                or any(ord(character) < 32 for character in value)):
            raise ValueError("invalid_environment")
        result[key] = value
    return result


def merge_env(file_values, process_values):
    result = dict(file_values)
    for key in ENV_KEYS:
        value = process_values.get(key)
        if value:
            if key in result and result[key] != value:
                raise ValueError("environment_conflict")
            result[key] = value
    return result


def hexadecimal(value, length):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{" + str(length) + r"}", value) is not None
