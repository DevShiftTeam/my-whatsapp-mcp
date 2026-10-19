"""Synthetic HTTP checks; no model or private audio is needed."""

import hashlib
import http.client
import json
from pathlib import Path
import sys
import tempfile
import threading
import socket
import time
import types
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import server
from server import AudioError, Configuration, Result, TranscriptionService, WhisperEngine, make_server, recipe_identity


TOKEN = "synthetic_test_bearer_" + "a" * 40
AUDIO = b"synthetic encoded audio"


def recipe():
    return {
        "protocol_version": 1,
        "backend": "faster-whisper",
        "model": "synthetic/test-model",
        "revision": "a" * 40,
        "model_sha256": "b" * 64,
        "model_files": [{"path": "model.bin", "sha256": "b" * 64}],
        "runtime": {
            "faster_whisper": "1.2.1", "ctranslate2": "4.8.2", "av": "18.0.0",
            "onnxruntime": "1.31.0", "tokenizers": "0.22.1",
            "compute_type": "int8", "num_workers": 1,
            "language": "he", "vad_filter": True,
        },
    }


class FakeEngine:
    def __init__(self):
        self.calls = []
        self.result = Result("טקסט בדיקה", 4.5, "he")
        self.error = None
        self.started = threading.Event()
        self.release = None

    def transcribe(self, path, audio_format):
        self.calls.append((path, audio_format, path.read_bytes()))
        self.started.set()
        if self.release:
            self.release.wait(5)
        if self.error:
            raise self.error
        return self.result


class ServiceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.engine = FakeEngine()
        self.configuration = Configuration(
            token=TOKEN, listen_host="127.0.0.1", listen_port=0,
            model_dir=self.root, language="he", cpu_threads=4,
            body_timeout_seconds=1,
        )
        self.service = TranscriptionService(self.configuration, self.engine, recipe(), self.root / "uploads")
        self.server = make_server(self.service)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        if self.engine.release:
            self.engine.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(2)
        self.temporary.cleanup()

    def request(self, method="POST", path="/v1/transcribe", body=AUDIO, headers=None):
        merged = {
            "Authorization": "Bearer " + TOKEN,
            "Content-Type": "application/octet-stream",
            "X-Audio-SHA256": hashlib.sha256(body).hexdigest(),
            "X-Audio-Format": "ogg",
            "X-Transcription-Identity": self.service.identity,
        }
        merged.update(headers or {})
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        connection.request(method, path, body, merged)
        response = connection.getresponse()
        result = (response.status, json.loads(response.read()))
        connection.close()
        return result

    def test_info_is_authenticated_and_identity_binds_recipe(self):
        status, info = self.request("GET", "/v1/info", b"")
        self.assertEqual(status, 200)
        self.assertEqual(info, {"recipe": recipe(), "identity": recipe_identity(recipe())})
        status, error = self.request("GET", "/v1/info", b"", {"Authorization": "Bearer wrong"})
        self.assertEqual((status, error), (401, {"error": "unauthorized"}))

    def test_transcription_has_original_duration_and_hebrew(self):
        status, result = self.request()
        self.assertEqual(status, 200)
        self.assertEqual(result, {
            "identity": self.service.identity,
            "audio_sha256": hashlib.sha256(AUDIO).hexdigest(),
            "language": "he", "text": "טקסט בדיקה", "start": 0,
            "end": 4.5, "silent": False,
        })
        self.assertEqual(self.engine.calls[0][1:], ("ogg", AUDIO))
        self.assertFalse(self.engine.calls[0][0].exists())
        self.assertEqual(list(self.service.upload_dir.iterdir()), [])

    def test_silent_audio_is_a_success_with_empty_text(self):
        self.engine.result = Result("", 2.0, "he")
        status, result = self.request()
        self.assertEqual(status, 200)
        self.assertTrue(result["silent"])
        self.assertEqual(result["text"], "")
        self.assertEqual(result["end"], 2.0)

    def test_auto_language_returns_detected_language(self):
        self.service.configuration = Configuration(TOKEN, "127.0.0.1", 0, self.root, "auto", 4, 1)
        self.engine.result = Result("synthetic English speech", 3.0, "en")
        status, result = self.request()
        self.assertEqual(status, 200)
        self.assertEqual(result["language"], "en")

    def test_explicit_non_hebrew_language_is_supported(self):
        self.service.configuration = Configuration(TOKEN, "127.0.0.1", 0, self.root, "en", 4, 1)
        self.engine.result = Result("synthetic English speech", 3.0, "en")
        status, result = self.request()
        self.assertEqual(status, 200)
        self.assertEqual(result["language"], "en")

    def test_rejects_wrong_hash_before_inference(self):
        status, error = self.request(headers={"X-Audio-SHA256": "0" * 64})
        self.assertEqual((status, error), (400, {"error": "audio_hash_mismatch"}))
        self.assertEqual(self.engine.calls, [])
        self.assertEqual(list(self.service.upload_dir.iterdir()), [])

    def test_rejects_changed_recipe_before_upload(self):
        status, error = self.request(headers={"X-Transcription-Identity": "0" * 64})
        self.assertEqual((status, error), (409, {"error": "identity_mismatch"}))
        self.assertEqual(self.engine.calls, [])

    def test_rejects_unsupported_format_and_content_type(self):
        for headers, code in [({"X-Audio-Format": "playlist"}, "unsupported_format"),
                              ({"Content-Type": "application/json"}, "unsupported_content_type")]:
            with self.subTest(code=code):
                status, error = self.request(headers=headers)
                self.assertEqual((status, error), (415, {"error": code}))
        self.assertEqual(self.engine.calls, [])

    def test_rejects_length_before_reading_body(self):
        status, error = self.request(headers={"Content-Length": str(25 * 1024 * 1024 + 1)})
        self.assertEqual((status, error), (413, {"error": "audio_too_large"}))
        self.assertEqual(self.engine.calls, [])

    def test_engine_failure_does_not_expose_error_or_keep_audio(self):
        self.engine.error = RuntimeError("private path and transcript")
        status, error = self.request()
        self.assertEqual((status, error), (503, {"error": "transcription_unavailable"}))
        self.assertEqual(list(self.service.upload_dir.iterdir()), [])

    def test_unusable_audio_is_terminal_without_private_details(self):
        self.engine.error = AudioError("synthetic decoder details")
        status, error = self.request()
        self.assertEqual((status, error), (422, {"error": "invalid_audio"}))
        self.assertEqual(list(self.service.upload_dir.iterdir()), [])

    def test_bounds_are_enforced_on_engine_output(self):
        for result in [Result("a" * 65537, 3, "he"), Result("text", 1201, "he"),
                       Result("text", float("nan"), "he"), Result("text", 3, "en")]:
            with self.subTest(result=result):
                self.engine.result = result
                status, error = self.request()
                self.assertEqual((status, error), (422, {"error": "invalid_transcription"}))
        self.assertEqual(list(self.service.upload_dir.iterdir()), [])

    def test_busy_is_rejected_before_reading_body(self):
        self.engine.release = threading.Event()
        first = threading.Thread(target=self.request)
        first.start()
        self.assertTrue(self.engine.started.wait(2))
        status, error = self.request(headers={"Content-Length": "10000"})
        self.assertEqual((status, error), (429, {"error": "busy"}))
        self.engine.release.set()
        first.join(3)
        self.assertFalse(first.is_alive())
        self.assertEqual(len(self.engine.calls), 1)

    def test_info_is_not_ready_while_actual_inference_runs(self):
        self.engine.release = threading.Event()
        first = threading.Thread(target=self.request)
        first.start()
        self.assertTrue(self.engine.started.wait(2))
        status, error = self.request("GET", "/v1/info", b"")
        self.assertEqual((status, error), (429, {"error": "busy"}))
        self.engine.release.set()
        first.join(3)

    def test_incomplete_upload_releases_slot_and_removes_tempfile(self):
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        connection.request("POST", "/v1/transcribe", AUDIO, {
            "Authorization": "Bearer " + TOKEN, "Content-Type": "application/octet-stream",
            "X-Audio-SHA256": hashlib.sha256(AUDIO).hexdigest(), "X-Audio-Format": "ogg",
            "X-Transcription-Identity": self.service.identity, "Content-Length": str(len(AUDIO) + 1),
        })
        try:
            response = connection.getresponse()
            self.assertEqual(response.status, 400)
            self.assertEqual(json.loads(response.read()), {"error": "incomplete_audio"})
        except (http.client.RemoteDisconnected, ConnectionResetError):
            # An absolute reception deadline may close the connection before an error can be written.
            pass
        finally:
            connection.close()
        deadline = time.monotonic() + 2
        while True:
            status, _ = self.request("GET", "/v1/info", b"")
            if status == 200:
                break
            self.assertEqual(status, 429)
            if time.monotonic() >= deadline:
                self.fail("incomplete upload did not release its processing slot")
            time.sleep(0.01)
        self.assertEqual(self.engine.calls, [])
        self.assertEqual(list(self.service.upload_dir.iterdir()), [])
        status, _ = self.request()
        self.assertEqual(status, 200)

    def test_total_handler_admission_is_bounded(self):
        admitted = threading.Condition()
        handler_count = 0
        release = threading.Event()
        process_request_thread = self.server.process_request_thread

        def hold_handler(request, client_address):
            nonlocal handler_count
            with admitted:
                handler_count += 1
                admitted.notify_all()
            # Admission is under test, independently of the idle socket deadline.
            release.wait()
            process_request_thread(request, client_address)

        self.server.process_request_thread = hold_handler
        sockets = []
        try:
            for _ in range(8):
                sockets.append(socket.create_connection(self.server.server_address, timeout=2))
            with admitted:
                self.assertTrue(admitted.wait_for(lambda: handler_count == 8, timeout=5))
            self.assertEqual(self.server.handler_slots._value, 0)
            extra = socket.create_connection(self.server.server_address, timeout=2)
            try:
                self.assertEqual(extra.recv(1), b"")
            finally:
                extra.close()
        finally:
            release.set()
            for connection in sockets:
                connection.close()

    def test_idle_connection_expires_before_authentication(self):
        connection = socket.create_connection(self.server.server_address, timeout=3)
        try:
            self.assertEqual(connection.recv(1), b"")
        finally:
            connection.close()

    def test_slow_body_has_absolute_deadline(self):
        connection = socket.create_connection(self.server.server_address, timeout=3)
        headers = (
            "POST /v1/transcribe HTTP/1.1\r\nHost: 127.0.0.1\r\n"
            "Authorization: Bearer " + TOKEN + "\r\nContent-Type: application/octet-stream\r\n"
            "Content-Length: 1000\r\nX-Audio-SHA256: " + hashlib.sha256(AUDIO).hexdigest()
            + "\r\nX-Audio-Format: ogg\r\nX-Transcription-Identity: " + self.service.identity + "\r\n\r\n"
        )
        connection.sendall(headers.encode())
        started = time.monotonic()
        try:
            for _ in range(6):
                try:
                    connection.sendall(b"x")
                except OSError:
                    break
                time.sleep(0.25)
            try:
                response = connection.recv(4096)
            except ConnectionResetError:
                response = b""
            self.assertTrue(not response or b"400" in response)
            self.assertLess(time.monotonic() - started, 2.5)
        finally:
            connection.close()
        self.assertEqual(self.engine.calls, [])
        self.assertEqual(list(self.service.upload_dir.iterdir()), [])

    def test_disconnect_does_not_release_native_inference_slot(self):
        self.engine.release = threading.Event()
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        connection.request("POST", "/v1/transcribe", AUDIO, {
            "Authorization": "Bearer " + TOKEN, "Content-Type": "application/octet-stream",
            "X-Audio-SHA256": hashlib.sha256(AUDIO).hexdigest(), "X-Audio-Format": "ogg",
            "X-Transcription-Identity": self.service.identity,
        })
        self.assertTrue(self.engine.started.wait(2))
        connection.close()
        status, error = self.request()
        self.assertEqual((status, error), (429, {"error": "busy"}))
        self.engine.release.set()


class ConfigurationTests(unittest.TestCase):
    def test_literal_loopback_and_strong_token_are_required(self):
        for env in [
            {"WA_TRANSCRIPTION_TOKEN": TOKEN, "WA_TRANSCRIPTION_MODEL_DIR": "/tmp", "WA_TRANSCRIPTION_LISTEN": "localhost:6971"},
            {"WA_TRANSCRIPTION_TOKEN": TOKEN, "WA_TRANSCRIPTION_MODEL_DIR": "/tmp", "WA_TRANSCRIPTION_LISTEN": "0.0.0.0:6971"},
            {"WA_TRANSCRIPTION_TOKEN": "short", "WA_TRANSCRIPTION_MODEL_DIR": "/tmp"},
        ]:
            with self.subTest(env=env):
                with self.assertRaises(ValueError):
                    Configuration.from_env(env)

    def test_defaults_auto_language_cpu_threads_four(self):
        configuration = Configuration.from_env({
            "WA_TRANSCRIPTION_TOKEN": TOKEN, "WA_TRANSCRIPTION_MODEL_DIR": "/tmp",
        })
        self.assertEqual((configuration.language, configuration.cpu_threads), ("auto", 4))
        self.assertEqual((configuration.listen_host, configuration.listen_port), ("127.0.0.1", 6971))

    def test_auto_and_explicit_language_codes_are_configurable(self):
        for language in ("auto", "he", "en", "ru", "ar"):
            configuration = Configuration.from_env({
                "WA_TRANSCRIPTION_TOKEN": TOKEN, "WA_TRANSCRIPTION_MODEL_DIR": "/tmp",
                "WA_TRANSCRIPTION_LANGUAGE": language,
            })
            self.assertEqual(configuration.language, language)

    def test_cpu_threads_configures_model_without_changing_recipe_identity(self):
        verified = {
            "model": "synthetic/test-model", "revision": "a" * 40, "model_sha256": "b" * 64,
            "model_files": [{"path": "model.bin", "sha256": "b" * 64}],
        }
        versions = {("faster-whisper" if k == "faster_whisper" else k): v for k, v in server.RUNTIME_VERSIONS.items()}
        calls = []
        fake_fw = types.ModuleType("faster_whisper")
        fake_fw.WhisperModel = lambda *args, **kwargs: calls.append((args, kwargs)) or object()
        fake_tokenizer = types.ModuleType("faster_whisper.tokenizer")
        fake_tokenizer._LANGUAGE_CODES = ("he", "en")
        with mock.patch.object(server, "verify_model", return_value=verified), \
                mock.patch.object(server.importlib.metadata, "version", side_effect=versions.__getitem__), \
                mock.patch.dict(sys.modules, {"faster_whisper": fake_fw, "faster_whisper.tokenizer": fake_tokenizer}):
            four = WhisperEngine(Configuration(TOKEN, "127.0.0.1", 6971, Path("/tmp/model"), "he", 4, 30))
            eight = WhisperEngine(Configuration(TOKEN, "127.0.0.1", 6971, Path("/tmp/model"), "he", 8, 30))
        self.assertNotIn("cpu_threads", four.recipe["runtime"])
        self.assertEqual(four.recipe, eight.recipe)
        self.assertEqual(recipe_identity(four.recipe), recipe_identity(eight.recipe))
        self.assertEqual(calls[0][1]["cpu_threads"], 4)
        self.assertEqual(calls[1][1]["cpu_threads"], 8)


if __name__ == "__main__":
    unittest.main()
