# Changelog

## Unreleased

- Keep accepted history downloads independent of the media-cancellation slot, so received privacy controls do not cancel first-link history and falsely pause the archive. Controls remain immediate, download deadlines remain bounded, and genuine source failures still fail closed.
- Add explicit basic macOS ARM64 app-candidate packaging with the existing logo and license notices, without requiring optional AI runtimes or models. A local-only candidate can omit the tunnel helper explicitly; including it retains its pin and macOS compatibility checks. Preserve native-AI bundle rules and external archive paths; candidates remain unsigned and unqualified for public distribution until the separate runtime and release checks pass.
- Report Docker publisher-helper pull failures with bounded, fixed diagnostic codes instead of discarding every registry failure into one generic command error. Keep the pinned image and version check unchanged; diagnostics never print registry stderr or refresh a pin automatically.
- Allow native serving up to 45 seconds for one shared graceful shutdown, instead of applying Docker's shorter stop window. Explicit container serving keeps its eight-second budget; interrupted or failed drains remain fail-closed and never recover automatically.
- Add bounded, body-free ingestion latency measurements and a read-only native owner diagnostic for queue pressure and processing phases. Normal scoped MCP status does not expose these global counters.
- Give eligible fresh plaintext a bounded FIFO path alongside history processing, using shared memory/admission limits and prerequisite-safe reservation exchanges. Validated positive-only history can be overtaken; unprepared or sensitive history, new identity mappings and overlapping message IDs retain their ordering and source-safety checks.

- Add optional Hebrew transcription guidance to local browser first-run and Models and media, with copyable terminal steps and a bounded check of the running voice worker. The browser does not install software or restart the archive, and remote, demo and historical views do not offer native setup.
- Refuse transcription installation paths incompatible with the shared private configuration before provisioning; spaces and Hebrew paths remain supported.

- Consolidate overlapping history limitations into one coverage notice with expandable Details. Retain clearly labeled last-confirmed progress during transient refresh failures without showing an active-import spinner.
- Retry one short internal saved-Stats response-fence timeout while the owner request remains valid, preserving final source and authority checks and refusing genuine invalidations.

- Let expiry cleanup yield between records when a privacy control is waiting, so background cleanup does not consume that control's deadline. Expired content remains withheld until physical cleanup finishes; genuinely lost controls still pause protected reads.

- Refresh Archive and Stats in place with subtle loading feedback. Keep the selected conversation, loaded pages, filters, focus and scroll during bounded background updates, unless the selected message itself is unavailable; retain the last successful view on transient connection failures while preserving access and archive-safety checks.

- Clarify Docker source-revision labels, the completed synthetic native container checks and the remaining real-account/client limits. Correct setup labels and existing data-directory paths without changing stored data or credentials.

- Test live controls and scoped MCP reads during older-history import, and make the Stats accessibility fixture honor localized weekday names.

- Voice recovery can keep a private fixed reason code for a rejected phone-copy reply while a recording request is pending. Storage failures are not mistaken for rejected replies. The code is not exposed through status or MCP and is cleared when a valid reply is accepted.

- Saved chat lists and statistics share repeated metadata to read larger rebuilt archives within the existing memory limit.

- Owner voice retries can recover eligible missing recordings with fresh phone responses, optionally target one permitted chat, and show recovery progress separately from transcription. Phone-copy requests run before queued downloads are woken, and new request failures remain visible.

### Added

- Show All saved messages in owner Archive and Stats, including eligible captured-only history when configured. Archive opens recent messages without a search, labels captured rows, and preserves successful reads during ordinary import refresh. Current permissions and known message restrictions still apply; settings, AI connections and message actions remain current-archive operations.

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
