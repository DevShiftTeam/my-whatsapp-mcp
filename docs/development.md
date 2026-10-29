# Development and code structure

The app uses Go, SQLite, and embedded HTML/CSS/JavaScript. The normal source build has no frontend build step and no native model dependency. Start with [local setup](setup.md).

The product is My WhatsApp and the repository is `DevShiftTeam/my-whatsapp-mcp`. The existing Go module path, `github.com/Bodokh/my-whatsapp-mcp`, and the `wa-indexer` executable remain compatibility identifiers. A brand change must not migrate archive directories or rotate client credentials.

## Package ownership

| Directory | Responsibility |
| --- | --- |
| `cmd/wa-indexer` | CLI options, browser launch, loopback listeners, and shutdown. |
| `cmd/wa-indexer-worker` | Internal native worker process; not an independent user-facing service. |
| `internal/app` | Composition, connection recovery, client management, and resource lifetime. |
| `internal/appdata` | Private directories, instance ownership, and local socket paths. |
| `internal/ingestion` | WhatsApp session, QR pairing, history and event projection through whatsmeow. |
| `internal/archive` | SQLite schema, serialized writes, scoped reads, source invalidation, jobs, and credentials. |
| `internal/retrieval` | Keyword and optional semantic candidates, ranking, pagination, and response guards. |
| `internal/indexing` | Source-bound document windows and durable indexing jobs. |
| `internal/localui` | Owner HTTP API, optional password admin interface, and embedded browser assets. |
| `internal/mcpserver` | Scoped MCP read and send tools, bearer authentication, and local stdio adapter. |
| `internal/outbound` | Send validation, provider dispatch, and durable request/receipt semantics. |
| `internal/clientsetup` | Client configuration and instructions rendered from app-owned settings. |
| `internal/media` | Original downloads, format verification, OCR, image descriptions, and transcription. |
| `internal/transcription` | Authenticated loopback adapter for optional batch voice extraction. |
| `services/transcription` | Optional Python faster-whisper installer, service, and synthetic tests. |
| `internal/modelstore` | Pinned model catalog, downloads, integrity checks, and installation. |
| `internal/inference` | Optional native encoder and worker supervision. |
| `internal/compute`, `internal/diskspace`, `internal/workerlife` | Work admission, disk reservations, and child-process lifetime. |
| `internal/textnorm`, `internal/sqlitelex`, `internal/sqlitevec` | Shared text normalization and SQLite search extensions. |
| `scripts/app`, `scripts/package`, `packaging` | Source build, optional bundle assembly, manifests, and notices. |
| `Dockerfile`, `compose*.yaml`, `scripts/docker`, `scripts/acceptance` | Container image, Compose overlays, synthetic Docker acceptance and benchmark tooling, and the developer-only remote acceptance coordinator. |

## Data flow

```text
Phone → whatsmeow → ingestion → archive
                                 ↓
                 retrieval → owner UI / scoped MCP
                                 ↑
                  indexing / media → native workers
```

`app` constructs and owns these components. The archive has one serialized writer and a separate bounded read pool. Read paths apply the archive policy and client scope, then recheck source and credential validity before returning a response. Known deletions and conflicting source revisions withhold affected text; nonfatal sync gaps remain visible as partial coverage.

The local stdio adapter connects to the running app through a private Unix socket. It does not open another WhatsApp session or archive database.

Sending uses a separate owner-approved permission and a persistent request journal. Keep permission checks before transmission and after media upload. Retries must reuse the original request ID and payload; an uncertain result can mean the message was sent. See [outbound messaging](clients/outbound-messaging.md).

The optional voice service uploads retained recordings to a local faster-whisper service. Generated transcripts are stored separately from source text and enter keyword search. Its CPU and model-memory budget is separate from the archive. See [transcription](transcription.md).

Local browser first-run can guide Hebrew service setup, but cannot run an installer or change launch configuration. Its native-owner-only `GET /api/transcription/status` requires the owner session and CSRF capability, accepts no body or query parameters, and returns only `configured`, a fixed `state`, and `hebrew_ready`. Both remote-admin realms deny this route. Checks use the active voice owner's frozen adapter identity, recheck lifecycle and owner authority before buffered delivery, and have a three-second deadline, one active probe and a one-second admission cooldown. Busy responses distinguish earlier verification from a fresh model check. No model pins, private paths, URLs or credentials are returned.

Offline recovery commands operate on existing stopped profiles. Preserve archive policy, deletions, expiry, client permissions, and current source checks during recovery. See [snapshot recovery](setup.md#recover-the-committed-archive-snapshot) and [original-cache recovery](setup.md#recover-associations-for-existing-original-recordings).

## Storage

`archive/archive.sqlite` contains messages, chat metadata, FTS indexes, policies, tombstones, sync gaps, client grants, jobs, media metadata, and optional vectors. Schema migrations live in `internal/archive/migrations`.

Expiry maintenance yields between records when a priority privacy control is queued, committing its completed prefix and leaving the remaining work pending. Actual-clock read guards still withhold expired messages before physical scrubbing. A genuinely missed control deadline remains fail-closed; the yield does not clear an existing safety pause or extend deadlines.

`session/session.sqlite` is the separate SDK-owned linked-device store. Its schema belongs to whatsmeow rather than the archive migrations. Ordinary private files, original media, model installations, and client configuration have separate directories under the data root.

Production ingestion opens an owner-only encrypted history spool beneath the existing persistent `working` directory. Verified compressed bytes and the download notification are durably captured before decompression or projection. Raw payloads and media credentials never enter logs or HTTP/MCP responses. A successful synchronous projection removes only its own capture; failures, cancellation and newly recorded unknown-history/decryption gaps retain it. Reopening does not replay or purge pending captures, widen policy, install source authority or clear unsafe markers. A failed or interrupted download does not become a complete capture and keeps its original typed failure. The key and encrypted batches belong in the same stopped-profile backup; the adjacent key is not protection against same-user compromise. Explicit deletion of all local data includes the working subtree.

Bodyless ordinary group revocations from authenticated own-device history may lack both author claims and any retained mutation target. Only that incoming, unknown-target case uses the negative-only unresolved-deletion guard. `RecordUnresolvedDeletionIfUnknown` rechecks all durable target states inside the serialized writer transaction before saving the exact canonical account/chat/message guard and scoped coverage gap. Later originals and edits from any author remain withheld, without fabricated identity, dates or discovery anchors. Connectors without this atomic capability, explicit invalid claims, ambiguous targets and persistence failures retain strict failure behavior. Known-author and administrative revocations keep their existing paths.

An authenticated bodyless `REVOKE` in a direct PN/LID chat may carry a broadcast marker in `WebMessageInfo.Participant`. Only with explicit incoming direction, no key-author claim, and a bounded well-formed non-device broadcast marker does that field stay non-author metadata. Require exactly one `@`, no spaces or control characters, and an exact parsed-string round trip; the SDK parser alone can discard trailing segments or zero device/agent suffixes. The raw record is unchanged; the pinned SDK derives the original author from the direct chat. Normal terminal deletion, target checks and persistence remain mandatory. Group/admin records, omitted or outgoing direction, conflicting key claims and unresolved retention metadata do not use this compatibility path. This compatibility is for the default strict handling; explicit partial-history handling retains its separate, stricter authority validation.

Ordinary bodyless-revocation failures report a closed `revoke_*` stage distinguishing author claims, direction, retained targets, parsing and guard persistence. The diagnostic retains the actual bounded stub enum and field-presence mask, not identities, values, message text or provider error strings. Older generic `record_structure` records with default scalar fields do not establish which branch failed or which source fields were absent. More precise diagnostics do not relax import checks or make an unsafe profile eligible for recovery.

History envelope and conversation wire budgets scale with their already bounded byte lengths; wrappers and individual records keep the 4,096-field limit, and all layers retain framing, duplicate-field, scalar, depth and cancellation checks. Explicit partial mode discards only reviewed bodyless group-create (`20`) and participant-add (`27`) notices with presence `507`, a nonzero timestamp and the allowed display fields. Scope, key and actor syntax still validate, but discarded actors do not need to establish a consistent message author. This path commits `unsupported_notice` uncertainty before continuing, retains the encrypted raw batch and creates no source, identity, name or frontier. Known controls and strict-mode handling keep their existing author checks.

Wire-verified strict uncertainty notices (`72` `CHANGE_EPHEMERAL_SETTING`, `130` `DISAPPEARING_MODE`, `202` `BIZ_COEX_PRIVACY_TRANSITION`, `216` `CHANGE_LIMIT_SHARING`, `229`, and `263`) remain outside `benignHistoryStub`. Default strict handling admits them only when `analyzeHistoryWire` verifies their exact phone-delivered wrapper, field sets, chat kind, participant/parameter bounds and `historyPresence` mask, with `isMentionedInStatus == false` and no retention, control or unknown wire fields. Admission records durable `unsupported_notice` uncertainty (`UnknownHistoryGap = true`, `Coverage = "partial"`), invalidates in-flight read guards, retains the encrypted raw capture in `working/history-spool` (validated as a private subdirectory during startup `media.Reconcile`), and projects no message, alias, name or anchor. Unverified shapes or failed uncertainty persistence still fail closed.

Live group (`@g.us`) and broadcast (`@broadcast`) stanzas can deliver a standalone `SenderKeyDistributionMessage` (`pkmsg`) alongside the actual payload (`skmsg`) under the same stanza ID and timestamp. After recording sender identity metadata, `messageWithDeliveredAdmission` ignores a standalone `SenderKeyDistributionMessage` with no user-visible content or protocol fields so it does not collide with the companion message. Paused-and-resumed WhatsApp Ogg Opus voice recordings may emit a mid-stream single-lace `BOS` (`granule = 0`) audio page when continuing the same logical stream (`serial` unchanged, monotonic `sequence`); `verifyWhatsAppOpus` permits that segment boundary while rejecting chained streams or duplicate headers.

Keyword search uses FTS5 with scope-local scoring and shared Hebrew normalization. Original logical-order text is preserved. Semantic retrieval uses authorized vector-distance scans; it does not use an ANN or HNSW index.

## Ingestion timing diagnostics

Ordinary fresh plaintext has one FIFO worker alongside the existing source/history worker. Both use the same queued/in-flight count and byte ledger and the same separately bounded waiting-callback ledger. Earlier lifecycle/source items, unprepared history and overlapping history message IDs retain their ordering. The history preparation pass admits overtaking only for completely validated positive-only chunks; sensitive, unknown or dependency-sensitive chunks keep their original ordered projection. Its private cross-chat message-ID barrier has a finite count/byte budget; exhausting that budget disables overtaking without dropping the remaining records. Callback control invalidations and final archive/MCP guards remain unchanged.

An earlier unqueued callback owns the next enqueue turn. A later callback may reserve otherwise unused capacity, but cannot enqueue ahead of that prerequisite. Capacity release normally exchanges the oldest waiter with those parked reservations. If that exchange would fragment the byte budgets and no queued or in-flight work remains, both complete bounded reservation sets exchange atomically instead. Neither ledger grows, no queued ticket moves, and the original callback ordering and deadlines remain intact. Reserved history headroom is opportunistic; it cannot prevent idle prerequisite work from progressing.

Scheduling checks complete authenticated, raw and normalized content roots, including field presence rather than just scalar values. Declared but unreviewed privacy/context fields keep the original path even when they contain explicit false or zero values. Plaintext with alternate PN/LID identifiers can use the priority worker only after bounded read-only checks prove the mapping already committed. Those checks run outside the runtime mutex after registering the ordering ticket and repeat before publication; a miss uses the same ticket to wait for its complete original source prefix. New or conflicting aliases are never installed as a scheduling shortcut.

Stopping removes callback admission, joins callbacks and both workers, then cancels the run and stops the socket before ending observation. Accepted history is neither cancelled nor discarded to make plaintext faster. Existing genuine drain and source-safety failures still fail closed. The optional media scheduler remains idle while either lane owns source work. This is a scheduling improvement, not a guarantee that phone delivery, history verification or database writes always finish within one second.

Received privacy controls apply immediately and can cancel optional or legacy media transfers, but do not own an admitted history download's cancellation. History may contain source controls that have not been read yet. Its download retains the bounded deadline and run lifetime, and genuine source failure still cancels that run; successfully applying an unrelated control must not turn download cancellation into an account-wide safety pause.

Native `serve` gives all HTTP, provider/helper and archive joins one shared 45-second shutdown deadline; explicit `--container` keeps eight seconds inside the delivery image's ten-second envelope. The budget starts at the first stop intent and is never renewed by a later cleanup attempt. A clean process exit alone cannot certify restart custody: supervisors must also verify exclusive stopped ownership and archive safety. Timeout, uncertain cleanup and unsafe-source outcomes must not mint clean-start authorization or trigger automatic snapshot recovery.

`internal/ingestion/latency.go` retains a fixed matrix of three lanes and eight phases, with fixed duration buckets. Samples use monotonic elapsed durations. The snapshots expose only finite labels, counts, bytes, approximate percentiles, maxima and saturation—not event payloads, source IDs, URLs or credentials. Histogram quantiles are upper bounds clamped to the observed maximum. Counters saturate rather than wrap, and snapshots copy their values before returning them.

The native-owner-only `GET /api/ingestion/status` endpoint uses the currently owned connector, requires its owner cookie and CSRF capability, accepts no body or query parameters, and rechecks owner/lifecycle authority before buffered delivery. It makes no network request and does not add global queue counters to scoped MCP status. Queue wait must include any history-preparation or lifecycle barrier; do not start latency measurement after that waiting period.

The realtime tests use the real Store, runtime admission and scoped MCP handlers. Fresh independent text must become readable before a held prepared historical apply is released; same-message ordering, history-tail controls, combined queue budgets, policy narrowing and buffered-response invalidation remain separately qualified. Use synthetic fixtures, not a duplicate linked session or a live profile. Passing these tests does not establish a production phone-to-search percentile.

## Owner saved reads

Owner Archive and Stats explicitly request `source=saved`. The request-local archive reader supplements current data with eligible historical candidates, without copying bodies or changing MCP defaults. It applies current original-message dominance before search matching and rechecks the contributing sources before buffered delivery. Current Settings, AI permissions, DiscoveryChats and operational status keep their existing APIs.

An empty Archive query uses `POST /api/archive/browse`; keyword search still requires a query. Search and browse use RFC3339 date strings, while Stats uses Unix-second date bounds. Message kind filters apply to Stats only. Historical `h1.` message handles retain their provenance and cannot become current mutation targets; `s1.` cursors belong to saved reads. Typed busy and stale-cursor responses are separate from an expired owner session.

Browser requests use sequence checks so superseded responses cannot replace a newer view. Ordinary revision polling coalesces a guarded refresh without clearing displayed messages or aborting pending reads. Policy, recovery, unsafe-source and authentication changes clear withheld data. This is polling freshness, not an instant push notification of every deletion or edit.

Archive refresh stages up to four previously loaded pages and the selected conversation before a keyed DOM update. A batch has a fifteen-second deadline and at most ten reads, including one stale-cursor restart. A deeper view retains its existing rows with an explicit stale notice instead of an unbounded rebuild or silent truncation. Transient transport failures retain the last successful Stats and Archive view; malformed successful data and known access/source invalidations remain distinct.

Only a background selected-message context read can handle an exact HTTP 404 `source_unavailable` locally: the unavailable message and its excerpts are removed, its conversation is deselected, and unrelated guarded results remain. Authentication, policy, historical-source, reconciliation and whole-archive failures still invalidate the view. Policy changes replay only the last submitted query with its prior chat filter removed; unsent draft text is not submitted automatically.

Saved aggregate delivery retries a live response-fence deadline once when the owning request is still valid. Each attempt retains the existing short check budget and rechecks authority; the final contributor fence remains mandatory. Cancelled callers and source, policy or credential denials are not converted into successful reads. `internal/localui/saved_stats_import_test.go` exercises this boundary with a real Store and authenticated owner HTTP requests during synthetic import and invalidation.

`make ui-test` includes `scripts/test-saved-archive-ui.mjs`, which evaluates the shipped browser script with synthetic DOM and deferred responses. It covers initial browsing, request races, import refresh, captured provenance and source/auth invalidation. Geometry and native browser behavior require separate synthetic browser journeys.

With the locked docs Playwright dependency and Chromium installed, `node scripts/test-refresh-browser.mjs` checks the actual owner markup and scripts in a real browser with synthetic, intercepted APIs. It covers background refresh, loaded pages, selection/focus/scroll, transient gateway failures, light/dark loaders, reduced motion and late responses after owner expiry. It never connects to a real archive or provider and runs in the documentation CI job when the owner assets change.

## Browser appearance

`internal/localui/assets/theme.js` is a separate, self-hosted bootstrap loaded before the stylesheet. It applies the browser-local **System**, **Light**, or **Dark** choice before the page paints, then synchronizes the login and workspace **Appearance** controls. Missing, invalid, or unavailable storage defaults to System. Device theme changes update System; explicit Light or Dark choices stay fixed. Storage failures must not prevent login or archive startup.

`internal/localui/assets/selects.js` enhances single-choice selects with a dependency-free styled trigger and option panel. The original select remains the authority for its value, form submission, validation, and events; choosing a different option dispatches bubbling `input` and `change` events on that select. Keep the original control and its ID when changing the presentation.

After assigning a select's `.value` directly, call `window.MyWhatsAppSelects.refresh(select)` to synchronize its visible value; property writes do not trigger a DOM mutation or change event. Call `window.MyWhatsAppSelects.closeAll()` on authentication transitions before clearing or hiding owner data, so open panels discard copied option text. Preserve dynamic options, disabled states, form reset, keyboard navigation, selected-state announcements, focus return, and dismissal.

Appearance uses these controls and the same light and dark color tokens as the rest of the interface. The mobile header keeps an accessible Appearance icon instead of the full label.

Keep colors in semantic CSS tokens so charts, status states, search highlights, dialogs, and mobile navigation share the resolved theme. Preserve a white background around the pairing QR image. Theme selection makes no API calls and changes no archive or authentication configuration; it must work under the existing self-hosted-script content security policy.

## Checks

Go commands need Go 1.27.1. If `go env GOTOOLCHAIN` prints `local` and your `go` is older, prefix Go commands with `GOTOOLCHAIN=go1.27.1`. Go then uses the toolchain already in your module cache or downloads it once. Offline use works only when that toolchain is already cached.

On macOS, `TMPDIR` usually goes through the `/var` → `/private/var` symlink. CI runs Go tests in a physical, owner-only temporary directory, so resolve it before running the tests locally, outside the repository:

```sh
export TMPDIR="$(python3 -c 'import os, tempfile; print(os.path.realpath(tempfile.gettempdir()))')"
```

From the source root:

```sh
make test
make vet
make formatcheck
```

Go tests use the `fts5,sqlite_fts5,ts_omit_logtail` build tags from the Makefile. Test fixtures are synthetic. Keep any live test profile separate from the default user archive.

The portable build helper and optional macOS package tooling have Python tests. Python 3.11 or newer is a contributor dependency for these checks, not an app runtime requirement:

```sh
make script-test
```

The optional transcription service has synthetic standard-library tests that do not download a model:

```sh
python3 -m unittest discover -s services/transcription/tests
```

Exercise the retained-original, extraction, keyword search, MCP scope, source-invalidation, and restart boundaries when changing voice processing. A passing synthetic suite does not establish real Hebrew recognition quality or performance.

`cmd/wa-indexer/transcription_installer_test.go` passes the installer's actual synthetic private env output through the Go launch loader without installing dependencies or a model. The Python Hebrew setup tests fake provisioning tools while exercising the real CLI. `scripts/ui/test_transcription_setup.mjs` covers the shipped guide script's finite readiness states, commands, cancellation, cooldown, clipboard fallback and owner expiry. With the docs Playwright dependencies installed, `node scripts/test-transcription-setup-browser.mjs` checks first-run and returning-owner flows in a real browser with intercepted synthetic APIs. Keep any screenshots and local results outside the tracked tree.

For browser JavaScript changes, Node.js can check syntax:

```sh
node --check internal/localui/assets/app.js
node --check internal/localui/assets/theme.js
node --check internal/localui/assets/selects.js
```

With Node.js 20 or newer, run `make ui-test` for synthetic Stats filter, request-race, logout, rendering, theme preference, and styled select checks against the actual browser source. When changing the select component, include keyboard selection, dynamic option updates, value and event synchronization, form reset, and copied-label cleanup cases. These use small DOM fixtures, without npm dependencies or a running app. They do not verify browser layout; check desktop and mobile separately in both light and dark, including login, dialogs, charts, and narrow screens. Check System under both device settings and verify that explicit overrides survive a reload.

Manually check keyboard access, dialogs, narrow screens, and Hebrew/English text when changing the browser interface. There is no npm install step.

Optional native tests use separate build tags and verified artifacts. A passing ordinary test suite does not establish inference quality, memory use on target hardware, or clean-machine bundle startup. See [native models](native-models.md) and [macOS packaging](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/packaging/macos/README.md).

## Docker source-build checks

The Dockerfile, Compose overlays, and synthetic container acceptance are described in the [Docker tooling notes](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/scripts/docker/README.md). Those checks need Docker Engine 28+, Compose 2.20.2+, Node.js 24, the locked documentation Playwright dependency with Chromium, Go 1.27.1, and a separately built harness image. Public CI runs the native checks on Linux ARM64 and AMD64 runners for pull requests and pushes to `main`. The checks use synthetic fixtures; they do not use real WhatsApp accounts, live tunnel providers, end-user AI clients, or physical phones. The CI helper steps in `scripts/docker/ci.mjs` depend on the hosted runner's preparation step, so a local run is a developer check rather than CI evidence.

## Documentation website

The public guides live under `docs/`. The website, search, and Markdown exports use the pages listed in `docs/.vitepress/public-pages.mjs`. Write instructions for users; keep local reports, planning notes, and asset prompts outside the repository.

Use Node.js 24 for the documentation tools:

```sh
cd docs/.vitepress
npm ci
npm test
npm run build
npm run test:browser
```

The preview command serves the built static site on loopback. See the documentation build-tool security notes in the source checkout for the build-tool restrictions and known advisories.

## Live rebuild boundary

A rebuild profile has an independent archive identity and durable provenance. Offline bootstrap preserves authorization and discovery metadata while excluding old content and derived work. Inherited negative metadata is enforced before writes. The app composes the same live runtime and MCP features with optional request provenance; ordinary profiles keep existing behavior. Public status labels recovery as partial and phone history unknown without disabling normal date controls.

The documented SDK unavailable-message response identifier is the admission correlation. Register a chosen peer request ID durably before sending; protocol source requests never consume client send permission or enter the outgoing human-message journal. History batches without qualified correlation supply metadata only. Synthetic tests cover exact-key mismatch, replay, expiry, policy changes, known negatives, interrupted preparation, native rebind, original-byte preservation, and source-owner exclusion. Actual device activation and upstream switching are separate operator work.

Recovery operation state is destination-only. It reuses body-free source anchors, counts registration before every copy/discovery send, and retains unknown attempts across policy changes and restart. The scheduler uses bounded windows and exact discovery cursors, with no inferred batch-response association. Owner progress and controls stay outside scoped MCP status. A legacy recovery archive upgrades additively while retaining provenance, session, guards and request history; original coherent backup schemas remain read-only.

## Delivered-history admission

Rebuilt profiles retain their provenance and negative guards under either startup strategy. The default selected-original strategy uses the existing exact-request ledger. Explicit delivered-history selection is durably audited before connector creation and remains fixed for the store lifetime. A separate internal admission value binds authenticated history work to the captured strategy epoch, observation generation and policy generation; policy retry must not refresh positive authority.

The writer applies known negative restrictions before the delivered positive-admission branch and atomically records distinct admitted revisions with their finite body budget. Paused, stale or exhausted positives must not discard subsequent controls. Separate newest-first page attempts retain existing discovery spending and record attempts before network dispatch; observed frontiers are hints, not request-to-batch correlation. Legacy pending requests are neither retried nor converted into delivered-body proof.

Synthetic qualification covers multi-message authenticated chunks, canonical keys, known controls, policy races, duplicate accounting, newest-to-older paging, restart and scoped final-read guards. Historical derivatives remain query-only under exact supported schema validation. Forward conversion of delivered-history profiles must fail explicitly unless all strategy audits, provenance and accounting can be preserved.

Delivered paging uses a fixed request count of 500 and a one-second minimum interval enforced by both the scheduler and durable registration. The pinned SDK recommends 50 but does not enforce a maximum; a count of 500 is an application request, not a documented phone delivery guarantee. Selected-original requests retain their ten-second interval. Measure real SQLite/FTS writer throughput separately from phone delivery latency; neither rate proves complete history coverage.

## Partial history compatibility

Strict history handling remains the default. Under explicit partial handling, authenticated history is analyzed before projection: structural framing and required authority remain strict, supported negative controls run first, and unsupported metadata or noncontent records add durable account-wide uncertainty. Discarded records cannot create bodies, identities, aliases, names, discovery anchors or request authority. Failed control or gap persistence still stops processing.

Generic uncertainty counts are separate from legacy undeclared-enum counts. Their checked coverage epoch invalidates in-flight live and historical responses; new reads retain partial-history and unknown-current-phone warnings. Owner diagnostics may include bounded categories and value-free wire-shape summaries; scoped MCP responses do not expose those counters or raw field details. Existing unsafe-state acceptance requires the exact reviewed producer and diagnostic pair, current evidence, and explicit owner consent. It does not establish that the old record was harmless or recover an unvisited suffix.
