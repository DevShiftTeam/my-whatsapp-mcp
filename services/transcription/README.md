# Optional voice transcription service

See the [transcription guide](../../docs/transcription.md) for installation, model profiles, environment configuration, and starting the archive with this service.

`setup.py` creates a private Python environment, downloads a pinned model once, verifies its weights, and writes a private configuration file. `server.py` loads the verified local model and provides authenticated loopback HTTP endpoints. PyAV supplies FFmpeg decoding; this service does not need Torch, diarization, an external FFmpeg executable, or Docker.

Only one upload and transcription runs at a time. Inputs are raw audio bytes, never URLs or caller-supplied filesystem paths. Uploaded temporary files are deleted before a response. If a client disconnects during native inference, the service retains the occupied slot until inference finishes.

Run the synthetic tests without downloading models:

```sh
python3 -m unittest discover -s services/transcription/tests -v
```

Decoder tests also run when the optional PyAV and NumPy dependencies are installed. Model and transcription quality require separate audio checks.
