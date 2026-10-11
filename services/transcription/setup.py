#!/usr/bin/env python3
"""Install the optional voice service into a separate private directory."""

import argparse
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys

from model_setup import PROFILES, verify_model
from private_files import load_env_file, private_directory, private_file, service_lock, write_private
from server import Configuration


def configure_environment(data_dir, *, language=None, profile_language="auto", cpu_threads=None, port=None):
    with service_lock(data_dir, "setup.lock"):
        return _configure_environment(data_dir, language=language, profile_language=profile_language,
                                      cpu_threads=cpu_threads, port=port)


def _configure_environment(data_dir, *, language, profile_language, cpu_threads, port):
    env_path = data_dir / "transcription.env"
    existing = load_env_file(env_path) if env_path.exists() else None
    values = dict(existing or {})
    configured = Configuration.from_env(values) if existing else None
    token = values.get("WA_TRANSCRIPTION_TOKEN") or secrets.token_urlsafe(32)
    if language is None:
        language = values.get("WA_TRANSCRIPTION_LANGUAGE", profile_language)
    if cpu_threads is None:
        cpu_threads = int(values.get("WA_TRANSCRIPTION_CPU_THREADS", "4"))
    if port is None:
        port = configured.listen_port if configured else 6971
    host = configured.listen_host if configured else "127.0.0.1"
    address = f"[{host}]:{port}" if host == "::1" else f"{host}:{port}"
    if existing and values.get("WA_TRANSCRIPTION_MODEL_DIR") != str(data_dir / "model"):
        raise ValueError("existing_configuration_conflict")
    values.update({
        "WA_TRANSCRIPTION_TOKEN": token,
        "WA_TRANSCRIPTION_LISTEN": address,
        "WA_TRANSCRIPTION_MODEL_DIR": str(data_dir / "model"),
        "WA_TRANSCRIPTION_LANGUAGE": language,
        "WA_TRANSCRIPTION_CPU_THREADS": str(cpu_threads),
        "WA_TRANSCRIPTION_BODY_TIMEOUT_SECONDS": values.get("WA_TRANSCRIPTION_BODY_TIMEOUT_SECONDS", "30"),
        "WA_INDEXER_TRANSCRIPTION_URL": f"http://{address}",
        "WA_INDEXER_TRANSCRIPTION_TOKEN": token,
        "WA_INDEXER_TRANSCRIPTION_LANGUAGE": language,
        "WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS": values.get("WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS", "600"),
    })
    Configuration.from_env(values)
    data = "".join(f"{key}={value}\n" for key, value in values.items()).encode()
    if existing is None:
        write_private(env_path, data)
    elif values != existing:
        temporary_env = data_dir / "transcription.env.new"
        if temporary_env.exists() or temporary_env.is_symlink():
            private_file(temporary_env, max_bytes=16384)
            temporary_env.unlink()
        write_private(temporary_env, data)
        temporary_env.replace(env_path)
    return env_path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-dir", required=True, type=Path)
    parser.add_argument("--python", default="3.11", help="Python 3.11 executable, or uv Python selector")
    parser.add_argument("--profile", choices=sorted(PROFILES), help="Fresh installations default to multilingual")
    parser.add_argument("--language", help="auto detects language; he/en/etc force a supported language")
    parser.add_argument("--cpu-threads", type=int)
    parser.add_argument("--port", type=int)
    parser.add_argument("--model")
    parser.add_argument("--revision")
    parser.add_argument("--model-sha256")
    args = parser.parse_args()
    source_dir = Path(__file__).resolve().parent
    if ((args.cpu_threads is not None and not 1 <= args.cpu_threads <= 32)
            or (args.port is not None and not 1024 <= args.port <= 65535)):
        parser.error("CPU threads must be 1..32 and the loopback port must be 1024..65535.")
    if args.language is not None and args.language != "auto" and (len(args.language) not in (2, 3) or not args.language.isascii() or not args.language.islower() or not args.language.isalpha()):
        parser.error("Choose an explicit language code such as he or en.")
    if any((args.model, args.revision, args.model_sha256)) and not all((args.model, args.revision, args.model_sha256)):
        parser.error("A custom model requires its immutable revision and model.bin SHA-256.")
    try:
        data_dir = private_directory(args.data_dir, create=True)
        existing = load_env_file(data_dir / "transcription.env") if (data_dir / "transcription.env").exists() else {}
        if args.profile is None and args.model is None and (data_dir / "model").exists():
            verified = verify_model(data_dir / "model")
            model, revision, model_hash = verified["model"], verified["revision"], verified["model_sha256"]
            profile_language = existing.get("WA_TRANSCRIPTION_LANGUAGE", "auto")
        else:
            pinned_model, pinned_revision, pinned_hash, _, profile_language = PROFILES[args.profile or "multilingual"]
            model, revision, model_hash = args.model or pinned_model, args.revision or pinned_revision, args.model_sha256 or pinned_hash
        language = args.language or existing.get("WA_TRANSCRIPTION_LANGUAGE", profile_language)
        venv = data_dir / "venv"
        uv = shutil.which("uv")
        if not venv.exists():
            if uv:
                subprocess.run([uv, "venv", "--python", args.python, str(venv)], check=True)
            else:
                selector = "python" + args.python if args.python in ("3.11", "3.12", "3.13") else args.python
                python = shutil.which(selector)
                if not python:
                    raise ValueError("python_not_found")
                subprocess.run([python, "-m", "venv", str(venv)], check=True)
        python = venv / "bin" / "python"
        subprocess.run([str(python), "-c", "import sys; assert (3,11) <= sys.version_info[:2] <= (3,13)"], check=True)
        if uv:
            subprocess.run([uv, "pip", "install", "--python", str(python), "-r", str(source_dir / "requirements.txt")], check=True)
        else:
            subprocess.run([str(python), "-m", "pip", "install", "-r", str(source_dir / "requirements.txt")], check=True)
        subprocess.run([str(python), "-c", "import sys; from faster_whisper.tokenizer import _LANGUAGE_CODES; assert sys.argv[1] == 'auto' or sys.argv[1] in _LANGUAGE_CODES", language], check=True)
        subprocess.run([str(python), str(source_dir / "model_setup.py"), "--data-dir", str(data_dir),
                        "--model", model, "--revision", revision, "--model-sha256", model_hash], check=True)
        env_path = configure_environment(data_dir, language=args.language, profile_language=profile_language,
                                         cpu_threads=args.cpu_threads, port=args.port)
    except Exception:
        print("Transcription setup failed. No credentials were printed. Check Python, file permissions, disk space, and network access.", file=sys.stderr)
        return 1
    print("Transcription is installed. Private configuration: " + str(env_path))
    print("Start the service with its venv Python and server.py --env-file pointing to that file.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
