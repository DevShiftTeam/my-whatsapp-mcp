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

Offline recovery commands operate on existing stopped profiles. Preserve archive policy, deletions, expiry, client permissions, and current source checks during recovery. See [snapshot recovery](setup.md#recover-the-committed-archive-snapshot) and [original-cache recovery](setup.md#recover-associations-for-existing-original-recordings).

## Storage

`archive/archive.sqlite` contains messages, chat metadata, FTS indexes, policies, tombstones, sync gaps, client grants, jobs, media metadata, and optional vectors. Schema migrations live in `internal/archive/migrations`.

`session/session.sqlite` is the separate SDK-owned linked-device store. Its schema belongs to whatsmeow rather than the archive migrations. Ordinary private files, original media, model installations, and client configuration have separate directories under the data root.

Keyword search uses FTS5 with scope-local scoring and shared Hebrew normalization. Original logical-order text is preserved. Semantic retrieval uses authorized vector-distance scans; it does not use an ANN or HNSW index.

## Browser appearance

`internal/localui/assets/theme.js` is a separate, self-hosted bootstrap loaded before the stylesheet. It applies the browser-local **System**, **Light**, or **Dark** choice before the page paints, then synchronizes the login and workspace **Appearance** controls. Missing, invalid, or unavailable storage defaults to System. Device theme changes update System; explicit Light or Dark choices stay fixed. Storage failures must not prevent login or archive startup.

`internal/localui/assets/selects.js` enhances single-choice selects with a dependency-free styled trigger and option panel. The original select remains the authority for its value, form submission, validation, and events; choosing a different option dispatches bubbling `input` and `change` events on that select. Keep the original control and its ID when changing the presentation.

After assigning a select's `.value` directly, call `window.MyWhatsAppSelects.refresh(select)` to synchronize its visible value; property writes do not trigger a DOM mutation or change event. Call `window.MyWhatsAppSelects.closeAll()` on authentication transitions before clearing or hiding owner data, so open panels discard copied option text. Preserve dynamic options, disabled states, form reset, keyboard navigation, selected-state announcements, focus return, and dismissal.

Appearance uses these controls and the same light and dark color tokens as the rest of the interface. The mobile header keeps an accessible Appearance icon instead of the full label.

Keep colors in semantic CSS tokens so charts, status states, search highlights, dialogs, and mobile navigation share the resolved theme. Preserve a white background around the pairing QR image. Theme selection makes no API calls and changes no archive or authentication configuration; it must work under the existing self-hosted-script content security policy.

## Checks

From the source root:

```sh
make test
make vet
make formatcheck
```

Go tests use the `fts5,sqlite_fts5` build tags. Test fixtures are synthetic. Keep any live test profile separate from the default user archive.

The portable build helper and optional macOS package tooling have Python tests. Python 3.11 or newer is a contributor dependency for these checks, not an app runtime requirement:

```sh
make script-test
```

The optional transcription service has synthetic standard-library tests that do not download a model:

```sh
python3 -m unittest discover -s services/transcription/tests
```

Exercise the retained-original, extraction, keyword search, MCP scope, source-invalidation, and restart boundaries when changing voice processing. A passing synthetic suite does not establish real Hebrew recognition quality or performance.

For browser JavaScript changes, Node.js can check syntax:

```sh
node --check internal/localui/assets/app.js
node --check internal/localui/assets/theme.js
node --check internal/localui/assets/selects.js
```

With Node.js 20 or newer, run `make ui-test` for synthetic Stats filter, request-race, logout, rendering, theme preference, and styled select checks against the actual browser source. When changing the select component, include keyboard selection, dynamic option updates, value and event synchronization, form reset, and copied-label cleanup cases. These use small DOM fixtures, without npm dependencies or a running app. They do not verify browser layout; check desktop and mobile separately in both light and dark, including login, dialogs, charts, and narrow screens. Check System under both device settings and verify that explicit overrides survive a reload.

Manually check keyboard access, dialogs, narrow screens, and Hebrew/English text when changing the browser interface. There is no npm install step.

Optional native tests use separate build tags and verified artifacts. A passing ordinary test suite does not establish inference quality, memory use on target hardware, or clean-machine bundle startup. See [native models](native-models.md) and [macOS packaging](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/packaging/macos/README.md).

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
