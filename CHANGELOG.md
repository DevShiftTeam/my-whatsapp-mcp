# Changelog

## Unreleased

### Added

- Optional env-configured batch transcription through a separate authenticated loopback faster-whisper service, with a pinned multilingual default, a Hebrew ivrit-ai profile, configurable model and language selection, CPU INT8, private installation, and generated transcripts searchable through keyword search and scoped MCP.
- A transcription setup guide covering service configuration, original-media recovery limits, and CPU and disk requirements.
- An owner action to retry eligible failed voice downloads without relinking WhatsApp or resetting completed and evicted recordings.
- My WhatsApp MCP brand, an original generated logo, and updated browser interface branding.
- `AGENTS.md` and `CLAUDE.md` maintainer instructions for documentation, changelog updates, validation, privacy, and repository visibility.
- Local QR-linked WhatsApp archive with chat and date choices, Hebrew and English keyword search, and conversation context.
- Scoped, expiring read-only MCP connections for local clients and remote bearer access, with optional password-protected remote administration.
- Stored-snapshot coverage warnings and sync-gap reporting while readable messages remain accessible.
- Portable Go source build and contributor checks for macOS and Linux.
- Markdown setup, MCP, development, security, and contribution guides.

### Experimental

- Native semantic search, image OCR and descriptions, and whisper.cpp voice transcription. These require separately prepared runtimes and model files and are off in the normal source build. The separate faster-whisper service is opt-in and does not require a native bundle.
