"""Exercise the installed FFmpeg decoder without loading model weights."""

import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
import wave

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from server import AudioError, WhisperEngine


@unittest.skipUnless(importlib.util.find_spec("av") and importlib.util.find_spec("numpy"), "optional decoder runtime is not installed")
class DecodeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.engine = WhisperEngine.__new__(WhisperEngine)

    def tearDown(self):
        self.temporary.cleanup()

    def wav(self, seconds, rate=8000):
        path = self.root / "synthetic.wav"
        with wave.open(str(path), "wb") as output:
            output.setnchannels(1)
            output.setsampwidth(2)
            output.setframerate(rate)
            for _ in range(seconds):
                output.writeframes(b"\0\0" * rate)
        return path

    def test_silent_wav_preserves_full_duration_at_model_sample_rate(self):
        audio, duration = self.engine.decode(self.wav(2), "wav")
        self.assertEqual(duration, 2.0)
        self.assertEqual(audio.shape, (32000,))
        self.assertEqual(float(audio.max()), 0.0)
        self.assertEqual(str(audio.dtype), "float32")

    def test_rejects_audio_over_twenty_minutes_before_inference(self):
        with self.assertRaises(AudioError):
            self.engine.decode(self.wav(1201), "wav")

    def test_rejects_playlist_and_mismatched_declared_format(self):
        path = self.root / "playlist.audio"
        path.write_text("#EXTM3U\nhttps://example.invalid/private-audio\n")
        with self.assertRaises(AudioError):
            self.engine.decode(path, "m4a")
        with self.assertRaises(AudioError):
            self.engine.decode(self.wav(1), "ogg")


if __name__ == "__main__":
    unittest.main()
