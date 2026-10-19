# Voice transcription

Voice transcription is optional. The normal app imports and searches message text without Python or model downloads. This setup adds a local batch service for voice recordings, independent of native image models and semantic search.

The default setup uses a multilingual OpenAI large-v3-turbo conversion with faster-whisper, CPU INT8, four threads, and one inference at a time. It detects the language automatically and enables VAD. An optional ivrit-ai profile specializes in Hebrew; you can force a known language through configuration. Beam size, temperature, and other decoding options use faster-whisper defaults. WhatsApp already identifies the sender, so speaker diarization is unnecessary here.

Transcripts are generated text and can contain errors. Recognition quality and processing time depend on the recording and hardware; inspect a real result before relying on it.

## Use the browser setup guide

In the local first-run **Set up your archive** window, choose **Voice notes → Hebrew transcription**. You can also open **Models and media → Set up Hebrew transcription** later, including after a restart. This guide is available in the local owner window, not remote administration, demo mode or read-only historical snapshots.

The guide provides three steps: install the Hebrew runtime/model, start the separate loopback service, and add the shared private env file to the archive's existing launch. Copy buttons copy commands only; the browser cannot install Python, run shell commands or restart either process. Run native commands on the archive computer from the source checkout. Docker users should use the [optional container service](#optional-docker-service) instead.

Keep your original archive data directory and all existing launch arguments. The guide copies the transcription argument separately so it cannot silently replace your custom launch settings. Its default-launch example is only for an original `./bin/wa-indexer serve` command. **Continue without voice setup** skips the guide without changing any running transcription configuration or archive choices.

After restarting, reopen the private owner window and return through **Models and media**. **Check running app** checks the voice worker attached to that running archive—not an unrelated service on an open port. **Hebrew transcription connected** requires the pinned ivrit-ai model, runtime versions and Hebrew language settings to match, with a successful authenticated health check. A busy response is labeled busy, with any previous worker verification distinguished from a fresh check; check again once it finishes. Missing configuration, connecting, unavailable, changed or mismatched service identities, and stopped workers are not reported as connected. The bounded check is read-only and returns no tokens, paths or service URL. Check again to refresh its result.

Completing a guide step or downloading weights is not proof of an enabled worker. Compare a real Hebrew recording with its generated text as described in [step 4](#_4-check-a-recording-through-mcp); synthetic setup checks do not establish recognition quality.

## 1. Install the optional runtime and model

Run these commands from the source checkout. Install Python 3.11 using the [official Python downloads](https://www.python.org/downloads/) or your package manager, then choose a private directory outside the checkout:

```sh
TRANSCRIPTION_DIR="$HOME/.local/share/my-whatsapp-mcp/transcription"
python3.11 services/transcription/setup.py \
  --data-dir "$TRANSCRIPTION_DIR" \
  --python python3.11
```

If you already use [uv](https://docs.astral.sh/uv/), the installer can obtain Python 3.11 for you instead:

```sh
TRANSCRIPTION_DIR="$HOME/.local/share/my-whatsapp-mcp/transcription"
python3 services/transcription/setup.py --data-dir "$TRANSCRIPTION_DIR"
```

The installer creates a private virtual environment, downloads the pinned model, verifies its files, and generates `transcription.env` with a strong service token. The default model files occupy about 1.6 GB, in addition to the Python environment and original recordings. Allow space for another model copy in the download cache during installation, about 1.6 GB for the default model. Installation needs internet access; the running service uses its installed files and does not fetch models.

Python 3.11 is the supported runtime for this path. It does not require torch, torchaudio, pyannote, Docker, a GPU, or an external `ffmpeg` command. PyAV supplies FFmpeg libraries for decoding containers such as WhatsApp's Ogg/Opus recordings.

Complete WhatsApp Ogg/Opus recordings are supported without a final end-of-stream flag. Page checksums, stream sequence, continuation, packet framing, source SHA, and declared size are still verified.

## 2. Start the transcription service

Keep a separate terminal open, in the source checkout:

```sh
TRANSCRIPTION_DIR="$HOME/.local/share/my-whatsapp-mcp/transcription"
"$TRANSCRIPTION_DIR/venv/bin/python" services/transcription/server.py \
  --env-file "$TRANSCRIPTION_DIR/transcription.env"
```

The default address is `http://127.0.0.1:6971`. Requests require the generated bearer token, including the service's information endpoint. Keep this port out of Cloudflare tunnels and other remote routes. Only the archive's existing MCP and admin listeners need remote exposure.

## 3. Restart the archive with transcription enabled

Stop the archive with Ctrl+C, rebuild it, then start it with the shared private env file:

```sh
TRANSCRIPTION_DIR="$HOME/.local/share/my-whatsapp-mcp/transcription"
./scripts/app/build.sh
./bin/wa-indexer serve \
  --transcription-env-file "$TRANSCRIPTION_DIR/transcription.env"
```

If you already use a custom `--data-dir`, admin password file, or remote MCP arguments, keep those same arguments and add `--transcription-env-file`. Use the existing data directory to preserve the archive, linked-device session, and client grants. A new QR scan is unnecessary while the saved WhatsApp session is valid. The local owner port remains 64165.

`serve --open-existing` reopens a window; it cannot change a running process's configuration. Apply changes by restarting the service or app that owns them. Turning off native model previews does not turn off env-configured transcription.

## 4. Check a recording through MCP

First confirm that message import and search are working. Send a short Hebrew voice note in an included chat. Keep WhatsApp connected while the original recording downloads, then wait for extraction. Search a distinctive spoken phrase and open the surrounding conversation.

An AI client can search that phrase with `search_messages` and inspect `get_context`. The transcript is in `derived[].text`, with `source: "generated_voice_transcript"` and `generated: true`; the original `body` may be empty. There is no top-level `transcript` field. Semantic search is not required for Hebrew keyword search.

Listen to the recording and compare the text. Then restart the archive with the same arguments and repeat the search to check persistence. A successful import or ready service alone does not establish transcription quality.

## Configuration

The installer generates one env file shared by the service and app. Its directory must be owned by you with mode `0700`, and the file must have mode `0600`, no ACL entries, no symlinks, and a single hardlink. Pass its absolute path to the app. The file contains literal `KEY=value` lines: do not quote values, add `export`, or source it in a shell. Paths containing spaces and Hebrew remain literal values. Choose paths without quotes, backticks, dollar signs, control characters or line breaks, or leading/trailing spaces; the installer refuses incompatible paths before provisioning rather than leaving a configuration that the app cannot load.

| App variable | Default or purpose |
| --- | --- |
| `WA_INDEXER_TRANSCRIPTION_URL` | Required; the installer writes `http://127.0.0.1:6971`. Only a literal loopback HTTP address is accepted. |
| `WA_INDEXER_TRANSCRIPTION_TOKEN` | Required generated bearer token; must match the service token. |
| `WA_INDEXER_TRANSCRIPTION_LANGUAGE` | `auto` for detection; use a supported language code such as `he`, `en`, `ru`, or `ar` to force a known language. |
| `WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS` | `600`; permitted range 10–1800 seconds. |

These app variables can also come from the process environment. Incomplete configuration or a value conflicting with the env file fails startup with a fixed diagnostic such as `transcription_url_missing` or `transcription_env_conflict`. Without any transcription configuration, the app keeps its normal text-only processing.

| Service variable | Default or purpose |
| --- | --- |
| `WA_TRANSCRIPTION_TOKEN` | The same generated bearer token. |
| `WA_TRANSCRIPTION_LISTEN` | `127.0.0.1:6971`; loopback only. |
| `WA_TRANSCRIPTION_MODEL_DIR` | Verified local model directory created by the installer. |
| `WA_TRANSCRIPTION_LANGUAGE` | `auto`, or a supported language code such as `he`. Keep it consistent with the app. |
| `WA_TRANSCRIPTION_CPU_THREADS` | `4`; applies to CTranslate2 inference (1–32). Changing it does not change the extraction recipe or re-transcribe saved recordings. |
| `WA_TRANSCRIPTION_BODY_TIMEOUT_SECONDS` | `30`; upload deadline and socket read timeout while receiving a recording. |

## Choose a model and language

The installer offers `--profile multilingual` (the default, with automatic language detection) and `--profile hebrew` (ivrit-ai, with forced Hebrew). For the specialized Hebrew profile, choose a separate private installation directory:

```sh
TRANSCRIPTION_DIR="$HOME/.local/share/my-whatsapp-mcp/transcription-hebrew"
python3.11 services/transcription/setup.py \
  --data-dir "$TRANSCRIPTION_DIR" \
  --python python3.11 \
  --profile hebrew \
  --language he
```

In steps 2 and 3, replace the `TRANSCRIPTION_DIR` value with this directory to use its env file. Stop the previous service first: only one configured model and one service are needed. The installer refuses to replace an existing model; switching profiles needs a new directory.

For varied languages, use the general multilingual profile. The Hebrew-tuned ivrit-ai model has not been qualified here for other languages. `auto` enables Whisper's language detection; forcing a known language can avoid detection mistakes. Change both language values together in the shared env file, then restart the service and archive. Rerunning the installer in the same directory with the same profile and model pins preserves existing settings; an explicit `--language` updates both language values. A language change cannot add capabilities absent from the installed model.

For a different model, use a new private installation directory and pass `--model`, `--revision`, and `--model-sha256` together. Use an immutable revision and the expected SHA-256 of its `model.bin`; check the model's language support and license. The extraction recipe records the installed model and language selection. A successful model load does not establish recognition accuracy.

The general model is [dropbox-dash/faster-whisper-large-v3-turbo](https://huggingface.co/dropbox-dash/faster-whisper-large-v3-turbo), revision `0a363e9161cbc7ed1431c9597a8ceaf0c4f78fcf`, `model.bin` SHA-256 `e76620f83d5f5b69efd3d87e3dc180c1bd21df9fbebacfd4335e5e1efcc018da`, under MIT. The Hebrew model is [ivrit-ai/whisper-large-v3-turbo-ct2](https://huggingface.co/ivrit-ai/whisper-large-v3-turbo-ct2), revision `72ad623a37947395efcc3933132353790e5a12f5`, `model.bin` SHA-256 `db2a2265aa012c16c7db9edda3d699c99f984efdd3f2e22a72a8ce7e9720f3a2`, under Apache-2.0. Python dependencies are pinned in `services/transcription/requirements.txt`, including faster-whisper 1.2.1, CTranslate2 4.8.2, and PyAV 18.0.0.

## Storage and resource limits

Original recordings use the archive's [configurable media budget](native-models.md#storage), initially 2 GiB. Model files and the Python environment are separate from that budget. The service accepts recordings up to 25 MiB and 20 minutes, with one active inference. Its CPU and model memory are additional to the Go app's budget. CPU INT8 reduces model memory; measure peak memory and processing time on your hardware.

The app uploads retained, verified recording bytes. The service does not fetch arbitrary audio URLs. Uploaded temporary copies are private and are removed after processing. A request timeout stops the app's wait; it does not immediately stop CTranslate2 inference. The service keeps its single-job guard until inference finishes, so retries may wait rather than start another model execution.

The request timeout is finite: 10 minutes by default, with a configurable maximum of 30 minutes. Inference can finish after the request times out or is cancelled, but that request cannot publish its later result.

The archive commits generated text with its source revision and extraction provenance. Transcripts become searchable without embeddings and can remain available after the original recording is evicted, while the source is still valid. Deletion, expiry, archive selection, and MCP scope checks still apply.

## Existing voice notes and failures

- A source-safety pause blocks voice downloads and source-bound transcription as well as message import and content reads. A durable `drain_failed` state reported with `reconciliation_required` remains after restart or Resume. Follow the offline [committed-snapshot recovery procedure](setup.md#recover-the-committed-archive-snapshot) for an eligible profile before testing voice processing. Enabling transcription alone cannot repair that state.
- Retained originals are eligible for the new extraction recipe automatically.
- If original files remain in the current profile but their associations were invalidated, follow [offline original-cache recovery](setup.md#recover-associations-for-existing-original-recordings) using a stopped private backup. It verifies and restores eligible associations without copying recordings; missing files still need an available download or phone copy.
- Pending downloads can use saved download references if WhatsApp still serves the recording and the linked session is connected.
- `media_availability: "not_retained"` describes the original recording. Check `derived` separately for generated text; a cached original and a transcript are different things.
- `transcription_busy` means the service is occupied, rather than unavailable. While its health check reports busy, jobs stay pending without being claimed or consuming retry attempts.
- With an otherwise readable source archive, transcription jobs can wait or retry when the service is unavailable or busy; text import and search stay available. Keep both terminals running and check the app's import status for fixed diagnostic codes. Avoid sharing logs or data files containing private recordings, tokens, or chat contents.

### Retry downloads or ask the phone for missing recordings

1. Keep the archive linked and your primary phone online. Open **Models and media → Failed voice downloads** and click **Retry voice downloads**.
2. Check the feedback. The action considers at most 32 download retries and can ask the primary phone for at most eight exact missing voice-note copies. Current chat and date choices and the media storage budget still apply.
3. If it asked your phone for copies, keep WhatsApp open on the phone. Recovery of previously completed recordings queues automatically after the matching response. Other returned voice notes may need another click on **Retry voice downloads** to queue them. The dialog shows waiting for phone, cleanup, download, and original completion separately. Phone-copy requests share a 10-second cooldown with manual history/backfill requests. A later click can request another bounded batch.
4. Wait for retained originals and completed transcripts, then search a spoken phrase and inspect `get_context` as described above.

| Result count | What it means |
| --- | --- |
| `queued` | Eligible original downloads were scheduled. Download and transcription still have to complete. |
| `requested` | The SDK acknowledged requests for exact copies from the primary phone. This does not prove that a copy arrived or was transcribed. |
| `skipped` | Download candidates were not queued by this action. The total of `queued` and `skipped` is at most 32. |

Fresh copies must pass the same source, policy, expiry, deletion, conflict, and recording checks as other imports. Existing transcripts and known original SHA records remain intact. An explicit retry can recover eligible missing originals that were downloaded previously, after a fresh authenticated phone response confirms the same recording. It keeps the archived message and prior download evidence unchanged. View-once messages, deleted or expired sources, and excluded chats remain ineligible. Unavailable or mismatched phone copies can remain unreadable. This action does not clear the restored-snapshot coverage gap, guarantee older recordings are still available, or reset downloads automatically on startup.

The owner API also accepts `POST /api/voice/retry` with `{"chat_id":"returned-chat-id"}` to narrow a retry to one permitted conversation. Omitting `chat_id` retains the current owner scope. It does not change saved chat or date choices. `GET /api/voice/recovery?chat_id=returned-chat-id` returns aggregate recovery status for that scope. Both require the owner session and its CSRF capability; MCP credentials cannot invoke them. Recovery requests expire after five minutes. Restart resumes accepted download work but does not resend phone requests. A completed original does not prove a completed transcript.

Phone requests also check the saved WhatsApp identity and message direction against the archive. If the identities disagree, the request is skipped. A successful request acknowledgement still does not establish that the recording arrived.

A timed-out request does not prove that the recording is missing from your phone. The phone may not have replied, or its reply may have failed the recording's verification checks. A still-pending request can keep a private reason code for its last rejected reply; the code is cleared on acceptance and is not shown in status or MCP. Expired requests, changed sources and local processing failures may leave no reply code, so an absent code does not prove that no reply arrived. Verification and permissions are not bypassed. Do not reset your archive or replace your AI connection just to retry a recording.

For read-only AI access, see [MCP setup](mcp.md). For native image and semantic previews, see [native models](native-models.md).

## Optional Docker service

The native setup above remains supported. The default archive Compose file has no transcription service or model dependency. This optional container recipe is in preview. Platform support, memory use, and recording quality have not yet been tested. Its image is separate from the minimal archive image. Dependency/model acquisition happens during explicit provisioning, not ordinary archive startup.

Build the optional Python 3.11 provisioning image, then install the runtime and model into a separate private volume. Use the image's Linux environment rather than copying a macOS virtual environment:

```sh
docker build -f packaging/transcription.Dockerfile -t my-whatsapp-transcription:local .
docker volume create wa-indexer-transcription
docker run --rm --mount source=wa-indexer-transcription,target=/var/lib/transcription \
  my-whatsapp-transcription:local python3.11 /opt/transcription/setup.py \
  --data-dir /var/lib/transcription/runtime --python python3.11
```

Provisioning downloads dependencies and the verified model and creates a mode-0600 private env file inside mode-0700 directories owned by UID/GID 10001. That file includes both `WA_TRANSCRIPTION_TOKEN` and matching `WA_INDEXER_TRANSCRIPTION_TOKEN`; do not print it or put it in the checkout. Keep the generated URL/listen pair `http://127.0.0.1:6971` and `127.0.0.1:6971`. Set `TRANSCRIPTION_BASE` to the pinned base image you intend to use before building this optional image.

For Hebrew in a fresh transcription volume, append `--profile hebrew --language he` to the provisioning command above. This writes Hebrew to both service and archive language settings. Do not replace an existing multilingual model in place; use a separate private volume and update the transcription volume mapping deliberately, retaining the existing archive volume.

```sh
docker compose -f compose.yaml -f compose.build.yaml -f compose.transcription.yaml \
  --profile transcription up -d --build
```

The optional service uses `network_mode: service:wa-indexer`, so both processes share literal loopback. It publishes no transcription port. The archive mounts the same token file read-only; the service mounts its private runtime read/write. A separate network hostname such as `http://transcription:6971` is incompatible with the existing loopback-only adapter. No `depends_on` makes transcription a default boot requirement. When using this override, start the optional service deliberately, wait for processing, then compare an actual recording to the generated text. HTTP readiness alone does not qualify transcription.

If the archive container is recreated, recreate the transcription service too so it joins the new network namespace. Stop both before backing up the transcription volume. Its model/runtime/token volume is separate from the archive volume and should remain private. To return to text-only startup, use only `compose.yaml` and `compose.build.yaml`; retain both volumes unless you explicitly intend to remove them.
