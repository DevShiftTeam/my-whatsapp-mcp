# Changelog

## Unreleased

### Added

- Optional MCP and JSON sending for text, media, and existing Ogg/Opus voice recordings, with separate owner-approved chat allowlists, read-only defaults, durable request IDs, and explicit unknown-outcome handling.
- An owner Stats dashboard with date, chat, and message-type filters, adaptive UTC activity, saved-media counts, separate archive processing status, and visible-only five-second refresh.
- Offline original-cache recovery against a stopped private backup, restoring only verified existing recordings' associations under current source authority, without copying recordings or changing the backup.
- Explicit offline committed-snapshot recovery for eligible source-safety pauses, preserving saved archive data and client grants while recording partial coverage and unverified phone state.
- Optional env-configured batch transcription through a separate authenticated loopback faster-whisper service, with a pinned multilingual default, a Hebrew ivrit-ai profile, configurable model and language selection, CPU INT8, private installation, and generated transcripts searchable through keyword search and scoped MCP.
- A transcription setup guide covering service configuration, original-media recovery limits, and CPU and disk requirements.
- An owner action to retry eligible voice downloads and request bounded missing voice-note copies from the primary phone, without resetting completed or evicted recordings.
- My WhatsApp MCP brand, an original generated logo, and updated browser interface branding.
- `AGENTS.md` and `CLAUDE.md` maintainer instructions for documentation, changelog updates, validation, privacy, and repository visibility.
- Local QR-linked WhatsApp archive with chat and date choices, Hebrew and English keyword search, and conversation context.
- Scoped, expiring read-only MCP connections for local clients and remote bearer access, with optional password-protected remote administration.
- Stored-snapshot coverage warnings and sync-gap reporting while readable messages remain accessible.
- Portable Go source build and contributor checks for macOS and Linux.
- Markdown setup, MCP, development, security, and contribution guides.

### Fixed

- Retry timed-out cleanup of expired derived data without treating the maintenance deadline as a WhatsApp source failure; keep actual expiry, credential checks, and authoritative source-safety fences enforced.
- Report a busy transcription service separately and leave jobs pending without claiming work or consuming retries while health checks report it occupied.
- Give atomic identity alias merges a separate bounded internal deadline while preserving other control deadlines, caller cancellation, and denial of uncertain outcomes.
- Route eligible phone voice-copy requests through verified SDK LID addresses while preserving canonical source permissions and keeping acknowledgements distinct from delivery.
- Avoid redundant control writes for already committed PN/LID aliases while retaining bound-account and final safety checks; new or conflicting aliases still use the serialized writer.
- Keep startup and repeated Resume cancellation under owned cleanup, drain accepted history before disconnecting, and join late dial completion before declaring quiescence.
- Limit PN/LID alias invalidation to affected message dependencies, preserving unrelated originals and transcripts while enforcing merged deletion barriers.
- Accept complete WhatsApp Ogg/Opus voice recordings without a final end-of-stream flag, while preserving framing, checksum, source SHA, and declared-size checks. Vorbis still requires the flag.

### Experimental

- Native semantic search, image OCR and descriptions, and whisper.cpp voice transcription. These require separately prepared runtimes and model files and are off in the normal source build. The separate faster-whisper service is opt-in and does not require a native bundle.
