#!/usr/bin/env python3
"""Authenticated CPU voice transcription on a literal loopback address."""

import argparse
from dataclasses import dataclass
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.metadata
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import signal
import socket
import sys
import tempfile
import threading

from model_setup import verify_model
from private_files import cleanup_uploads, hexadecimal, load_env_file, merge_env, private_directory, service_lock

MAX_AUDIO_BYTES = 25 * 1024 * 1024
MAX_AUDIO_SECONDS = 1200
MAX_TEXT_BYTES = 64 * 1024
FORMATS = {"wav": "wav", "flac": "flac", "mp3": "mp3", "m4a": "mov", "ogg": "ogg", "opus": "ogg"}
RUNTIME_VERSIONS = {"faster_whisper": "1.2.1", "ctranslate2": "4.8.2", "av": "18.0.0",
                    "onnxruntime": "1.31.0", "tokenizers": "0.22.1"}


@dataclass(frozen=True)
class Configuration:
    token: str
    listen_host: str
    listen_port: int
    model_dir: Path
    language: str = "auto"
    cpu_threads: int = 4
    body_timeout_seconds: int = 30

    @classmethod
    def from_env(cls, env):
        token = env.get("WA_TRANSCRIPTION_TOKEN", "")
        if not re.fullmatch(r"[A-Za-z0-9_-]{43,128}", token):
            raise ValueError("invalid_token")
        address = env.get("WA_TRANSCRIPTION_LISTEN", "127.0.0.1:6971")
        if address.startswith("["):
            host, separator, port = address[1:].partition("]:")
        else:
            host, separator, port = address.rpartition(":")
        if not separator or host not in ("127.0.0.1", "::1") or not ipaddress.ip_address(host).is_loopback:
            raise ValueError("invalid_listen_address")
        listen_port = int(port)
        if not 1024 <= listen_port <= 65535:
            raise ValueError("invalid_listen_port")
        language = env.get("WA_TRANSCRIPTION_LANGUAGE", "auto")
        if not re.fullmatch(r"auto|[a-z]{2,3}", language):
            raise ValueError("invalid_language")
        threads = int(env.get("WA_TRANSCRIPTION_CPU_THREADS", "4"))
        timeout = int(env.get("WA_TRANSCRIPTION_BODY_TIMEOUT_SECONDS", "30"))
        if not 1 <= threads <= 32 or not 1 <= timeout <= 300:
            raise ValueError("invalid_resource_limit")
        model_dir = Path(env.get("WA_TRANSCRIPTION_MODEL_DIR", ""))
        if not model_dir.is_absolute():
            raise ValueError("invalid_model_directory")
        return cls(token, host, listen_port, model_dir, language, threads, timeout)


@dataclass(frozen=True)
class Result:
    text: str
    duration: float
    language: str


class AudioError(Exception):
    """Input failed a bounded decode check; no private details escape."""


def recipe_identity(recipe):
    canonical = json.dumps(recipe, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


class WhisperEngine:
    def __init__(self, configuration):
        verified = verify_model(configuration.model_dir)
        for name, version in RUNTIME_VERSIONS.items():
            distribution = "faster-whisper" if name == "faster_whisper" else name
            if importlib.metadata.version(distribution) != version:
                raise ValueError("runtime_version_mismatch")
        from faster_whisper import WhisperModel
        from faster_whisper.tokenizer import _LANGUAGE_CODES
        if configuration.language != "auto" and configuration.language not in _LANGUAGE_CODES:
            raise ValueError("invalid_language")
        self.configuration = configuration
        self.recipe = {
            "protocol_version": 1, "backend": "faster-whisper", **verified,
            "runtime": {**RUNTIME_VERSIONS, "compute_type": "int8", "cpu_threads": configuration.cpu_threads,
                        "num_workers": 1, "language": configuration.language, "vad_filter": True},
        }
        self.model = WhisperModel(str(configuration.model_dir), device="cpu", compute_type="int8",
                                  cpu_threads=configuration.cpu_threads, num_workers=1, local_files_only=True)

    def decode(self, path, audio_format):
        import av
        import numpy
        # Explicit demuxers exclude playlists. MOV external data references stay disabled.
        options = {"protocol_whitelist": "file", "enable_drefs": "0", "use_absolute_path": "0"}
        chunks = []
        samples = 0
        try:
            with av.open(str(path), mode="r", format=FORMATS[audio_format], options=options) as container:
                if len(container.streams.audio) != 1 or len(container.streams.video) > 0:
                    raise AudioError()
                resampler = av.AudioResampler(format="s16", layout="mono", rate=16000)
                for frame in container.decode(audio=0):
                    for converted in resampler.resample(frame):
                        samples += converted.samples
                        if samples > MAX_AUDIO_SECONDS * 16000:
                            raise AudioError()
                        chunks.append(converted.to_ndarray().reshape(-1))
                for converted in resampler.resample(None):
                    samples += converted.samples
                    if samples > MAX_AUDIO_SECONDS * 16000:
                        raise AudioError()
                    chunks.append(converted.to_ndarray().reshape(-1))
        except AudioError:
            raise
        except Exception:
            raise AudioError() from None
        if not samples:
            raise AudioError()
        audio = numpy.concatenate(chunks).astype(numpy.float32) / 32768.0
        return audio, samples / 16000.0

    def transcribe(self, path, audio_format):
        audio, duration = self.decode(path, audio_format)
        # Leave beam size, temperature, and other decoder settings at faster-whisper defaults.
        language = None if self.configuration.language == "auto" else self.configuration.language
        segments, information = self.model.transcribe(audio, language=language, vad_filter=True)
        text_parts = []
        text_bytes = 0
        for segment in segments:
            part = segment.text.strip()
            if part:
                text_bytes += len(part.encode("utf-8")) + (1 if text_parts else 0)
                if text_bytes > MAX_TEXT_BYTES:
                    raise AudioError()
                text_parts.append(part)
        return Result(" ".join(text_parts), duration, information.language)


class TranscriptionService:
    def __init__(self, configuration, engine, recipe, upload_dir=None):
        self.configuration = configuration
        self.engine = engine
        self.recipe = recipe
        self.identity = recipe_identity(recipe)
        self.slot = threading.BoundedSemaphore(1)
        self.upload_dir = private_directory(upload_dir or configuration.model_dir.parent / "uploads", create=True)

    def authorized(self, authorization):
        return hmac.compare_digest(authorization or "", "Bearer " + self.configuration.token)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "MyWhatsAppTranscription"
    sys_version = ""

    @property
    def service(self):
        return self.server.service

    def log_message(self, *_):
        # Access logs would otherwise include untrusted paths and headers.
        pass

    def send_error(self, code, message=None, explain=None):
        self.reply(code, {"error": "invalid_request"})

    def setup(self):
        super().setup()
        self.deadline = None
        self.set_deadline(min(15, self.service.configuration.body_timeout_seconds))

    def set_deadline(self, seconds):
        self.cancel_deadline()
        self.connection.settimeout(seconds)
        def expire():
            try:
                self.connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.deadline = threading.Timer(seconds, expire)
        self.deadline.daemon = True
        self.deadline.start()

    def cancel_deadline(self):
        if self.deadline is not None:
            self.deadline.cancel()
            self.deadline = None

    def finish(self):
        self.cancel_deadline()
        super().finish()

    def handle_expect_100(self):
        self.close_connection = True
        self.reply(417, {"error": "unsupported_expectation"})
        return False

    def reply(self, status, payload):
        self.cancel_deadline()
        data = json.dumps(payload, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        try:
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass
        self.close_connection = True

    def authenticated(self):
        if (len(self.headers.get_all("Authorization", [])) != 1
                or not self.service.authorized(self.headers.get("Authorization"))):
            self.reply(401, {"error": "unauthorized"})
            return False
        return True

    def do_HEAD(self):
        self.reply(405, {"error": "method_not_allowed"})

    do_OPTIONS = do_HEAD
    do_PUT = do_HEAD
    do_PATCH = do_HEAD
    do_DELETE = do_HEAD

    def do_GET(self):
        if not self.authenticated():
            return
        if self.path != "/v1/info":
            self.reply(404, {"error": "not_found"})
            return
        if not self.service.slot.acquire(blocking=False):
            self.reply(429, {"error": "busy"})
            return
        self.service.slot.release()
        self.reply(200, {"recipe": self.service.recipe, "identity": self.service.identity})

    def do_POST(self):
        if not self.authenticated():
            return
        if self.path != "/v1/transcribe":
            self.reply(404, {"error": "not_found"})
            return
        if self.headers.get("Transfer-Encoding") or len(self.headers.get_all("Content-Length", [])) != 1:
            self.reply(400, {"error": "invalid_content_length"})
            return
        try:
            encoded_length = self.headers.get("Content-Length", "")
            if not re.fullmatch(r"[0-9]{1,10}", encoded_length):
                raise ValueError()
            length = int(encoded_length)
        except ValueError:
            self.reply(400, {"error": "invalid_content_length"})
            return
        if length <= 0 or length > MAX_AUDIO_BYTES:
            self.reply(413, {"error": "audio_too_large"})
            return
        if self.headers.get("Content-Type") != "application/octet-stream":
            self.reply(415, {"error": "unsupported_content_type"})
            return
        if any(len(self.headers.get_all(name, [])) != 1 for name in
               ("Content-Type", "X-Audio-Format", "X-Audio-SHA256", "X-Transcription-Identity")):
            self.reply(400, {"error": "duplicate_header"})
            return
        audio_format = self.headers.get("X-Audio-Format", "")
        if audio_format not in FORMATS:
            self.reply(415, {"error": "unsupported_format"})
            return
        audio_sha256 = self.headers.get("X-Audio-SHA256", "")
        if not hexadecimal(audio_sha256, 64):
            self.reply(400, {"error": "invalid_audio_hash"})
            return
        if self.headers.get("X-Transcription-Identity") != self.service.identity:
            self.reply(409, {"error": "identity_mismatch"})
            return
        if not self.service.slot.acquire(blocking=False):
            self.reply(429, {"error": "busy"})
            return
        self.set_deadline(self.service.configuration.body_timeout_seconds)
        status, payload = 422, {"error": "transcription_failed"}
        try:
            status, payload = self.process_audio(length, audio_format, audio_sha256)
        finally:
            self.cancel_deadline()
            # The synchronous native call completes even when the HTTP client disconnects.
            self.service.slot.release()
        self.reply(status, payload)

    def process_audio(self, length, audio_format, audio_sha256):
        path = None
        try:
            with tempfile.NamedTemporaryFile(dir=self.service.upload_dir, prefix="voice-", suffix=".audio", delete=False) as uploaded:
                path = Path(uploaded.name)
                os.chmod(path, 0o600)
                remaining = length
                digest = hashlib.sha256()
                while remaining:
                    chunk = self.rfile.read(min(65536, remaining))
                    if not chunk:
                        raise EOFError()
                    digest.update(chunk)
                    uploaded.write(chunk)
                    remaining -= len(chunk)
            if not hmac.compare_digest(digest.hexdigest(), audio_sha256):
                return 400, {"error": "audio_hash_mismatch"}
            self.cancel_deadline()
            try:
                result = self.service.engine.transcribe(path, audio_format)
            except AudioError:
                return 422, {"error": "invalid_audio"}
            except Exception:
                return 503, {"error": "transcription_unavailable"}
            if (not isinstance(result.text, str) or len(result.text.encode("utf-8")) > MAX_TEXT_BYTES
                    or not isinstance(result.duration, (int, float)) or not math.isfinite(result.duration)
                    or not 0 < result.duration <= MAX_AUDIO_SECONDS
                    or not re.fullmatch(r"[a-z]{2,3}", result.language)
                    or (self.service.configuration.language != "auto"
                        and result.language != self.service.configuration.language)):
                return 422, {"error": "invalid_transcription"}
            return 200, {"identity": self.service.identity, "audio_sha256": audio_sha256,
                         "language": result.language, "text": result.text, "start": 0,
                         "end": result.duration, "silent": not bool(result.text)}
        except (TimeoutError, EOFError, OSError):
            return 400, {"error": "incomplete_audio"}
        finally:
            if path is not None:
                path.unlink(missing_ok=True)


class Server(ThreadingHTTPServer):
    daemon_threads = False
    block_on_close = True

    def __init__(self, *args, **kwargs):
        self.handler_slots = threading.BoundedSemaphore(8)
        super().__init__(*args, **kwargs)

    def process_request(self, request, client_address):
        if not self.handler_slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.handler_slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.handler_slots.release()

    def handle_error(self, *_):
        # Never print request exceptions containing filenames or private audio information.
        pass


def make_server(service):
    server_type = Server
    if service.configuration.listen_host == "::1":
        class IPv6Server(Server):
            address_family = socket.AF_INET6
        server_type = IPv6Server
    server = server_type((service.configuration.listen_host, service.configuration.listen_port), Handler)
    server.service = service
    return server


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env-file", type=Path)
    args = parser.parse_args()
    lock = None
    try:
        env = merge_env(load_env_file(args.env_file) if args.env_file else {}, os.environ)
        configuration = Configuration.from_env(env)
        lock = service_lock(configuration.model_dir.parent)
        lock.__enter__()
        uploads = cleanup_uploads(configuration.model_dir.parent / "uploads")
        engine = WhisperEngine(configuration)
        service = TranscriptionService(configuration, engine, engine.recipe, uploads)
        server = make_server(service)
    except Exception:
        if lock is not None:
            lock.__exit__(None, None, None)
        print("Transcription service could not start. Check its private configuration, pinned model, and runtime versions.", file=sys.stderr)
        return 1
    def stop(*_):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    print("Transcription service is ready on its authenticated loopback listener.", flush=True)
    try:
        server.serve_forever()
    finally:
        try:
            server.server_close()
        finally:
            lock.__exit__(None, None, None)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
