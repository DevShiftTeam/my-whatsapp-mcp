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

The local stdio adapter talks to the running app through a private Unix socket. It does not open another WhatsApp session, archive database, or model runtime.

The optional voice service runs separately: retained recording → `internal/transcription` upload → local faster-whisper service → source-bound transcript commit → keyword search and scoped MCP. Its worker lifetime is independent of native previews. It has its own CPU and model-memory budget; cancelling HTTP does not immediately terminate inference. See [transcription](transcription.md) for its configuration and resource limits.

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

Manually check keyboard access, dialogs, narrow screens, and Hebrew/English text when changing the browser interface. There is no npm install step.

Optional native tests use separate build tags and verified artifacts. A passing ordinary test suite does not establish inference quality, memory use on target hardware, or clean-machine bundle startup. See [native models](native-models.md) and [macOS packaging](../packaging/macos/README.md).
