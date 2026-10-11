# Development and code structure

Last updated: 2026-10-11.

The app uses Go, SQLite, and embedded HTML/CSS/JavaScript. The normal source build has no frontend build step and no native model dependency. Start with [local setup](setup.md).

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
| `internal/mcpserver` | Scoped read-only MCP tools, bearer authentication, and local stdio adapter. |
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

Startup and Resume retain connection ownership through request cancellation. The caller's context controls a new dial; cleanup has its own 30-second deadline. Stop admission and join accepted source, handler, and delivery work while their context and socket remain available, then cancel the run context, join the actual dial return, and join the single owned disconnect. Quiescence requires all source, dial, socket, QR, store, and optional-worker joins. A source or connection drain deadline retains the unsafe observation and cleanup ownership. Eligible repeated Resume calls join prior cleanup and recheck caller cancellation before dialing; Resume refuses while the source-safety fence remains. Request cancellation alone never clears that fence.

Learning a WhatsApp PN/LID alias invalidates only messages whose source chat, sender, reply, or reference metadata uses the mapped LID, along with their dependent publications and media authority. Unrelated retained originals, completed transcripts, and active extraction leases stay intact. The identity merge applies canonical tombstones atomically, using the maximum merged chat-clear cutoff and exact message keys. Covered sources lose base text, FTS entries, and generated/media authority, so deleted content cannot reappear through the merge. Preserve response epochs and stale-work guards.

An already committed PN/LID pair can return after verification in one bounded read snapshot, avoiding a redundant control write. This path checks the sole bound account and final safety epoch; a clean epoch change falls back to the writer, and unsafe reads remain denied. New or conflicting aliases still use the authoritative serialized writer.

Atomic alias merges use the internal `archive.Options.AliasDeadline`: 2 seconds by default, capped at 5 seconds, with an earlier caller deadline or cancellation taking precedence. Deletion and other controls retain their 500 ms default, and the extension response fence remains 100 ms. All commands still share one writer, so a long alias transaction can delay a deletion until its stricter deadline is missed. Uncertain outcomes remain denied; the separate alias budget adds no retries or relaxed authorization.

The local stdio adapter talks to the running app through a private Unix socket. It does not open another WhatsApp session, archive database, or model runtime.

Stats uses owner/password-admin `POST /api/stats` with the existing CSRF and exact-origin checks; it adds no MCP tool. Optional filters are `from` (inclusive Unix seconds), `until` (exclusive Unix seconds), `chat_id`, and `kind` (`all`, `text`, `image`, `voice`, or `other`). Counts, media, activity, and top chats share the filtered readable policy scope and active, unexpired source checks. A bounded response includes `sampled_at` and archive revision, at most 90 adaptive UTC activity buckets, and the top eight chats. Global import and transcription processing stays in `/api/status`, independent of Stats filters. The UI refreshes every five seconds only while Stats and the document are visible, aborts when hidden, and clears stale counts and names after an error. Custom date controls use inclusive local days before conversion to Unix bounds; UTC activity buckets remain separate. Do not present saved-data fractions as complete WhatsApp history or transcription quality. See [Stats](stats.md) for the user guide.

The optional voice service runs separately: retained recording → `internal/transcription` upload → local faster-whisper service → source-bound transcript commit → keyword search and scoped MCP. Its worker lifetime is independent of native previews. It has its own CPU and model-memory budget; cancelling HTTP does not immediately terminate inference. See [transcription](transcription.md) for its configuration and resource limits.

The owner-only voice retry action combines at most 32 guarded download candidates with at most eight exact primary-phone copy requests. Its `queued`, `skipped`, and `requested` counts contain no source identifiers; a request acknowledgement is not an imported recording. Fresh copies arrive through normal authenticated SDK events and remain subject to source, policy, expiry, tombstone, conflict, protocol, SHA, and physical-ownership checks. A later explicit owner action can queue eligible freshly returned references. Preserve completed-original evidence and transcripts; do not add automatic startup resets.

Phone-copy transport can use a saved SDK LID only when the archive's canonical mapping agrees. Canonical source authorization remains unchanged, including policy, expiry, tombstone, message direction, and connection-generation guards. Malformed or conflicting mappings, lookup errors, and direction mismatches are skipped. Group identity stays unchanged; phone-number transport is used only when no SDK mapping exists. A request acknowledgement remains distinct from receiving authenticated recording references.

`recover-snapshot` is an explicit offline owner operation. It opens only an existing profile under the instance and archive leases and settles stopped extraction pins. The archive core validates integrity and recovery eligibility, records an audited recovery epoch, and restores access to the last committed snapshot. A prepared audit keeps the archive unsafe until the explicit command revalidates and completes it. It does not construct the app, connect the SDK, or start processing workers. Preserve policies, client grants, tombstones, conflict barriers, expiry, and committed content; invalidate stale work leases. Recovery epochs fence stale responses as well as commits. The durable `owner_restored_snapshot` gap and unverified-phone-state labels are part of the response contract. See [the recovery procedure](setup.md#recover-the-committed-archive-snapshot).

`recover-original-cache` is a separate offline operation under both stopped profiles' instance leases and current archive/media ownership. A query-only backup reader supplies authenticated completion evidence and refuses nonempty WAL data or more than 256 qualifying originals before repair. The current archive uses a recovery factory that refuses database creation and migrations and does not run physical expiry cleanup. Existing bytes are verified before restoring an association against exact current source authority. Actual-clock expiry and source, policy, deletion, conflict, SHA, and physical-ownership guards remain enforced. Preserve completion markers and quota charges; do not copy or delete originals or mutate the backup. Per-artifact commits permit partial progress on failure, and retries are idempotent. Output contains aggregate counts or fixed diagnostics only. See [the original-cache procedure](setup.md#recover-associations-for-existing-original-recordings).

## Storage

`archive/archive.sqlite` contains messages, chat metadata, FTS indexes, policies, tombstones, sync gaps, client grants, jobs, media metadata, and optional vectors. Schema migrations live in `internal/archive/migrations`.

`session/session.sqlite` is the separate SDK-owned linked-device store. Its schema belongs to whatsmeow rather than the archive migrations. Ordinary private files, original media, model installations, and client configuration have separate directories under the data root.

Keyword search uses FTS5 with scope-local scoring and shared Hebrew normalization. Original logical-order text is preserved. Semantic retrieval uses authorized vector-distance scans; it does not use an ANN or HNSW index.

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
```

With Node.js 20 or newer, run `make ui-test` for synthetic Stats filter, request-race, logout, and rendering checks against the actual browser source. These use a small DOM fixture, without npm dependencies or a running app. They do not verify browser layout; check desktop and mobile separately.

Manually check keyboard access, dialogs, narrow screens, and Hebrew/English text when changing the browser interface. There is no npm install step.

Optional native tests use separate build tags and verified artifacts. A passing ordinary test suite does not establish inference quality, memory use on target hardware, or clean-machine bundle startup. See [native models](native-models.md) and [macOS packaging](../packaging/macos/README.md).
