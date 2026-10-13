# Changelog

## Unreleased

### Added

- In explicit partial-history mode, record unsupported noncontent metadata, notices and control shapes as uncertainty while retaining supported source controls and current permissions. Keep readable Archive, Stats and MCP data available with persistent partial-history warnings; genuine source-safety failures remain visible.

- Add explicit rebuilt-profile delivered-history import with bounded page and body budgets, durable startup selection, preserved client/session authority and negative controls, and honest partial-snapshot labels. Individually correlated recovery remains the default; no full-history guarantee is implied.

- Show earlier-history import activity in Archive and Stats, with accepted copies separate from readable Live messages, explicit waiting and paused states, and reduced-motion support. Progress does not imply a complete import or reliable finish time.

- Add explicit `serve --unknown-history-policy partial` opt-in with strict mode remaining the default and persistent account-wide warnings about potentially missed edits, deletions and retention changes. Add separate protected offline `recover-partial-history --config` acceptance of an eligible committed partial snapshot; preserve current sessions, AI-client grants, known controls and spent allowances without claiming recovery of the unvisited remainder or current phone state.

- Continue importing when reviewed roster, administration and display notices have no message body, while retaining source-safety checks and local permissions.

- Add explicit offline acceptance of a committed partial history snapshot for only the reviewed participant-number-change (`33`/`475`), username-change (`193`/`411`) and group member link-mode (`217`/`411`) notice shapes, bound to fresh per-operation append-only private evidence; retain conflict quarantines and preserve the latest session, grants, send journal and recovery allowances, with current phone state still unverified.

- Add an optional captured historical view alongside Live, with source selection on six existing MCP read tools and Archive/Stats, current live authorization and source-safety checks, and historical IDs excluded from sending and original retrieval. Retain the twelve-tool catalog and live Settings, importing and send controls.

- Add durable background recovery beyond the first request window, with owner-only progress, bounded allowances, pause/resume and explicit extension; retain spent requests across restart and destination upgrades.
- Add offline preparation of a separate live recovery profile with preserved clients, source ownership, guarded phone-copy recovery, and honest partial coverage. Preparation does not activate a device or switch public access.

- Add an explicit, operator-attested historical snapshot serving mode with query-only archive access, persistent capture labels, current-time scoped reads, and disabled syncing, sending, original delivery, and processing. Normal serving remains unchanged.

- Docker Compose source builds, saved remote-access settings, and a separate password for phone browser access. These setup options are in preview.
- Searchable documentation on GitHub Pages, with per-page Markdown downloads. Docker CI checks target Linux ARM64 and AMD64; live-provider and client tests are still pending.

- System-default light and dark appearance with a browser-local override on the login screen and across the responsive archive interface.
- A richer Stats view with UTC weekly rhythm and saved-history highlights, interactive activity charts, and sortable, paginated conversation and sender breakdowns under the same archive filters.
- A refreshed GitHub overview with product illustrations, current feature limits, and linked guides; a prioritized roadmap for easier semantic setup, document text extraction, and local message presets.
- Document retention up to 8 MiB per file, scoped `search_attachments` and `get_attachment` read tools, and two-minute original-download URLs that recheck credential and source authority. Search covers metadata and nearby permitted messages, without extracting PDF body text.
- An owner permissions editor for existing AI connections, with read-only, selected-chat, and all-permitted-chat sending modes, separate permission revisions for stale-edit detection, and no change to existing tokens, read scopes, or expiry.
- Contact and group name search through scoped MCP read tools, plus query and chat-type filters for `list_chats`, with case, spacing, punctuation, and Hebrew-mark normalization.
- Optional MCP and JSON sending for text, media, and existing Ogg/Opus voice recordings, with separate owner-approved send modes, read-only defaults, durable request IDs, and explicit unknown-outcome handling.
- An owner Stats dashboard with date, chat, and message-type filters, adaptive UTC activity, saved-media counts, separate archive processing status, and visible-only five-second refresh.
- Offline original-cache recovery against a stopped private backup, restoring only verified existing recordings' associations under current source authority, without copying recordings or changing the backup.
- Explicit offline committed-snapshot recovery for eligible source-safety pauses, preserving saved archive data and client grants while recording partial coverage and unverified phone state.
- Optional env-configured batch transcription through a separate authenticated loopback faster-whisper service, with a pinned multilingual default, a Hebrew ivrit-ai profile, configurable model and language selection, CPU INT8, private installation, and generated transcripts searchable through keyword search and scoped MCP.
- A transcription setup guide covering service configuration, original-media recovery limits, and CPU and disk requirements.
- An owner action to retry eligible voice downloads and request bounded missing voice-note copies from the primary phone, without resetting completed or evicted recordings.
- An original generated logo and updated browser interface branding.
- Local QR-linked WhatsApp archive with chat and date choices, Hebrew and English keyword search, and conversation context.
- Scoped, expiring read-only MCP connections for local clients and remote bearer access, with optional password-protected remote administration.
- Stored-snapshot coverage warnings and sync-gap reporting while readable messages remain accessible.
- Portable Go source build and contributor checks for macOS and Linux.
- Markdown setup, MCP, development, security, and contribution guides.

### Changed

- Rewrote public guides in plain language, replaced the raw verification record with a short setup notice, and added a favicon to the documentation site.

- Styled dropdown options with keyboard access and a compact Appearance control in the mobile header.
- Removed the proposed official Business Platform adapter from the roadmap; local message presets remain proposed.
- Product name is now My WhatsApp, retaining the existing logo. MCP read and permitted send access remains an optional feature. Repository links use `DevShiftTeam/my-whatsapp-mcp`; executable, Go module, data paths, and client credentials keep their compatibility identifiers.

### Fixed

- Handle validated bodyless pinned-message notices without stopping history; retain strict control-field checks. Add separately audited owner-accepted partial recovery for an eligible older pinned-notice stop without treating its incomplete diagnostic as proof that control fields were absent.

- Keep long selected chat names within mobile filters so they cannot widen the page or clip settings dialogs.
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
