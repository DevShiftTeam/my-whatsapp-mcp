"""Explicit download step; the running service never downloads weights."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sys

from private_files import hexadecimal, private_directory, private_file, service_lock, write_private

PROFILES = {
    "multilingual": ("dropbox-dash/faster-whisper-large-v3-turbo",
                     "0a363e9161cbc7ed1431c9597a8ceaf0c4f78fcf",
                     "e76620f83d5f5b69efd3d87e3dc180c1bd21df9fbebacfd4335e5e1efcc018da", 1617884929, "auto"),
    "hebrew": ("ivrit-ai/whisper-large-v3-turbo-ct2",
               "72ad623a37947395efcc3933132353790e5a12f5",
               "db2a2265aa012c16c7db9edda3d699c99f984efdd3f2e22a72a8ce7e9720f3a2", 1617884968, "he"),
}
DEFAULT_MODEL, DEFAULT_REVISION, DEFAULT_MODEL_SHA256, DEFAULT_MODEL_BYTES, _ = PROFILES["multilingual"]
MAX_MODEL_BYTES = 8 * 1024 * 1024 * 1024
MAX_CONFIG_BYTES = 32 * 1024 * 1024


def file_digest(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def validate_model_name(model):
    if (not isinstance(model, str) or len(model) > 200
            or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", model)):
        raise ValueError("invalid_model")


def verify_model(model_dir):
    model_dir = private_directory(model_dir)
    manifest_path = model_dir / "manifest.json"
    private_file(manifest_path, max_bytes=32768)
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if set(manifest) != {"schema_version", "model", "revision", "files"} or manifest["schema_version"] != 1:
        raise ValueError("invalid_manifest")
    validate_model_name(manifest["model"])
    if not hexadecimal(manifest["revision"], 40) or not isinstance(manifest["files"], list):
        raise ValueError("invalid_manifest")
    if not 3 <= len(manifest["files"]) <= 32:
        raise ValueError("invalid_manifest")
    names = []
    model_hash = None
    for entry in manifest["files"]:
        if not isinstance(entry, dict) or set(entry) != {"path", "size", "sha256"}:
            raise ValueError("invalid_manifest")
        name, size, digest = entry["path"], entry["size"], entry["sha256"]
        if (not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", name)
                or name in names or name == "manifest.json" or not hexadecimal(digest, 64)
                or not isinstance(size, int) or isinstance(size, bool) or size <= 0
                or size > (MAX_MODEL_BYTES if name == "model.bin" else MAX_CONFIG_BYTES)):
            raise ValueError("invalid_manifest")
        info = private_file(model_dir / name)
        if info.st_size != size or file_digest(model_dir / name) != digest:
            raise ValueError("model_integrity_failed")
        names.append(name)
        if name == "model.bin":
            model_hash = digest
    if names != sorted(names) or not {"model.bin", "config.json", "tokenizer.json"}.issubset(names):
        raise ValueError("invalid_manifest")
    if {path.name for path in model_dir.iterdir()} != set(names) | {"manifest.json"}:
        raise ValueError("unexpected_model_file")
    return {
        "model": manifest["model"], "revision": manifest["revision"], "model_sha256": model_hash,
        "model_files": [{"path": entry["path"], "sha256": entry["sha256"]} for entry in manifest["files"]],
    }


def cleanup_staging(staging):
    staging = private_directory(staging)
    files = list(staging.iterdir())
    if len(files) > 33:
        raise ValueError("unexpected_download_file")
    for path in files:
        if not re.fullmatch(r"[A-Za-z0-9_.-]+\.(?:bin|json|txt)", path.name):
            raise ValueError("unexpected_download_file")
        private_file(path, max_bytes=MAX_MODEL_BYTES if path.name == "model.bin" else MAX_CONFIG_BYTES)
    for path in files:
        path.unlink()
    staging.rmdir()


def download_model(data_dir, model=DEFAULT_MODEL, revision=DEFAULT_REVISION, model_sha256=DEFAULT_MODEL_SHA256):
    data_dir = private_directory(data_dir)
    with service_lock(data_dir, "setup.lock"):
        return _download_model(data_dir, model, revision, model_sha256)


def _download_model(data_dir, model, revision, model_sha256):
    from huggingface_hub import HfApi, hf_hub_download

    data_dir = private_directory(data_dir)
    validate_model_name(model)
    if not hexadecimal(revision, 40) or not hexadecimal(model_sha256, 64):
        raise ValueError("invalid_model_pin")
    model_dir = data_dir / "model"
    if model_dir.exists():
        verified = verify_model(model_dir)
        if (verified["model"], verified["revision"], verified["model_sha256"]) != (model, revision, model_sha256):
            raise ValueError("model_already_configured")
        return model_dir
    staging = data_dir / "model.download"
    if staging.exists():
        cleanup_staging(staging)
    private_directory(staging, create=True)
    cache = private_directory(data_dir / "download-cache", create=True)
    try:
        repository_files = HfApi().list_repo_files(model, revision=revision)
        selected = sorted(name for name in repository_files
                          if "/" not in name and (name == "model.bin" or name.endswith(".json")
                                                  or re.fullmatch(r"vocabulary[\w.-]*\.txt", name)))
        if not {"model.bin", "config.json", "tokenizer.json"}.issubset(selected) or len(selected) > 32:
            raise ValueError("unsupported_model_layout")
        entries = []
        for name in selected:
            downloaded = Path(hf_hub_download(model, name, revision=revision, cache_dir=str(cache)))
            size = downloaded.stat().st_size
            limit = MAX_MODEL_BYTES if name == "model.bin" else MAX_CONFIG_BYTES
            if size <= 0 or size > limit:
                raise ValueError("model_file_too_large")
            destination = staging / name
            descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with downloaded.open("rb") as source, os.fdopen(descriptor, "wb") as output:
                shutil.copyfileobj(source, output, length=1024 * 1024)
            digest = file_digest(destination)
            if name == "model.bin":
                if digest != model_sha256:
                    raise ValueError("model_integrity_failed")
                for pinned_model, pinned_revision, _, pinned_size, _ in PROFILES.values():
                    if model == pinned_model and revision == pinned_revision and size != pinned_size:
                        raise ValueError("model_integrity_failed")
            entries.append({"path": name, "size": size, "sha256": digest})
        manifest = {"schema_version": 1, "model": model, "revision": revision, "files": entries}
        write_private(staging / "manifest.json", json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode())
        verify_model(staging)
        staging.rename(model_dir)
        # The verified regular copies are the runtime cache; avoid retaining a second 1.6GB download.
        shutil.rmtree(cache)
        return model_dir
    except BaseException:
        shutil.rmtree(staging, ignore_errors=True)
        raise


def main():
    parser = argparse.ArgumentParser(description="Download and verify a pinned CPU transcription model.")
    parser.add_argument("--data-dir", required=True, type=Path)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--revision", default=DEFAULT_REVISION)
    parser.add_argument("--model-sha256", default=DEFAULT_MODEL_SHA256)
    args = parser.parse_args()
    try:
        download_model(args.data_dir, args.model, args.revision, args.model_sha256)
    except Exception:
        print("Transcription model setup failed. Check the model pin, disk space, and network connection.", file=sys.stderr)
        return 1
    print("Pinned transcription model is ready.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
