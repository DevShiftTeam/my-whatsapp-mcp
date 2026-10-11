# Outbound MCP/API implementation evidence — 2026-10-11

## Scope and isolation

Implemented in the separate `codex/mcp-outbound` worktree based on `926db12`. The shared checkout was left untouched. No transcription workers, inference profiles, dashboard assets, charts, or dependency versions were changed. Review the archive migration number and the small owner routing/MCP listener edits when combining this branch with concurrent work.

The [client guide](../clients/outbound-messaging.md) describes `send_message`, `send_media`, `send_voice_note`, `get_send_status`, matching JSON endpoints, and owner permission setup. Media accepts canonical base64 up to 8 MiB decoded. Voice notes require an existing Ogg/Opus recording. This uses the existing linked connector, with no second session or microphone capture.

## Safety and focused qualification

Synthetic tests cover default-denied and finite chat permissions, credential and policy withdrawal, rechecking permission after an upload, owner polling/revocation during blocked provider work, accepted canceled permission withdrawal with durable denial after reopening, immediate safety-fence concurrency, request replay/conflict and restart persistence, uncertain provider outcomes, strict field/base64 validation, Ogg framing/CRC validation, and PTT metadata construction.

MCP/API tests cover authenticated discovery and JSON routing, invalid arguments rejected before provider work, read-specific adapter bounds, a 3.2-second delayed provider response, and an actual child process using inherited stdin/stdout to send an 80 KiB attachment unchanged. The latter exercises the native frame limit, rather than only in-memory SDK transport.

The code reviewer approved after corrections to owner revocation locking, stale permission/expiry checks, safety-map locking, credential mutation uncertainty classification, and stdio time/frame bounds. No critical or high-severity finding remains.

## Validation environment

Go 1.27.1; `sqlite_fts5` build tag; cached pinned dependencies; `GOMAXPROCS=2`; `-p 1`. Tests use an owned canonical private `TMPDIR` because macOS's default `/var` symlink path causes existing private-directory tests to reject test fixtures. Those path failures were reproduced on unchanged `926db12` before switching to the canonical temp directory.

Full regression command:

```sh
rtk proxy env TMPDIR=/absolute/private/canonical-test-directory GOTOOLCHAIN=local GOMAXPROCS=2 \
  GOMODCACHE=/absolute/pinned-module-cache /absolute/go1.27.1/bin/go \
  test -p 1 -tags sqlite_fts5 ./... -count=1
```

The full run has an existing `internal/inference` failure: `TestForegroundBypassesWaitingBackgroundAdmission` exceeds its 300 ms foreground deadline. The same test failed 10/10 attempts on untouched `926db12`. No inference source or test was changed to hide it.

Final results:

- Full non-race suite: every package except `internal/inference` passed. In particular, all changed integration packages and the new outbound package passed.
- `go vet -p 1 -tags sqlite_fts5 ./...`: passed.
- `go build -p 1 -tags sqlite_fts5 ./cmd/wa-indexer`: passed, with the binary kept outside the checkout.
- `git diff --check`: passed.
- Full race run across app, archive, ingestion, localui, mcpserver, and outbound: ingestion/localui/mcpserver/outbound passed; app and archive were not clean. App reported fixture races in `TestAcceptedDemoReplaySurvivesCallerCancellation`, `TestShutdownRetainsInstanceAndArchiveUntilNativeCleanupAcknowledgement`, and `TestShutdownRetainsArchiveUntilExtractionCleanupAcknowledged`. Matching fixture/worker races were reproduced on untouched `926db12` (test replacement of store/resource fields after background workers start).
- The full archive race run exceeded existing deadlines in `TestDerivedOriginJobRepairRestoresCascadedOriginLink`, `TestDerivedRootSeedingIsBoundedAndExistingWorkClaimsFirst`, `TestDerivedLegacyRecoverySkipsUnverifiableRowsAndKeepsOneBatchBound`, and `TestRootReaderSearchBoundsSelectedPinsEscapingAndCursor`. It reported no archive data race. Those source/tests were unchanged, and all four passed in a focused race run on untouched `926db12`; the full-run deadline failures remain an unresolved load/instrumentation limitation.
- Final focused race qualification of all 17 new outbound tests (including the inherited-pipe native media test): passed across app/archive/localui/mcpserver/outbound.

The focused race command was:

```sh
rtk proxy env TMPDIR=/absolute/private/canonical-test-directory GOTOOLCHAIN=local GOMAXPROCS=2 \
  GOMODCACHE=/absolute/pinned-module-cache /absolute/go1.27.1/bin/go \
  test -race -p 1 -tags sqlite_fts5 ./internal/app ./internal/archive ./internal/localui \
  ./internal/mcpserver ./internal/outbound \
  -run '^Test(Send|Outbound|Voice|StdioSending|RunStdioNativeBase64|OwnerWithdrawal|CanceledAcceptedPermissionWithdrawal|RequestRejects)' -count=1
```

The broad regression/race suite is **not globally green**. These exceptions were retained rather than modifying the other chat's inference/local-AI work.

## Proof boundary

No live WhatsApp messages, attachments, or voice notes were sent. Synthetic provider acknowledgement and SDK protocol tests do not prove actual WhatsApp delivery, media playback, recipient reads, or compatibility with a particular installed/hosted AI client. Permission controls are available through the owner API; dashboard sending controls remain outside this branch. There is no new automatic retry under a different key, recording/transcoding feature, or guarantee that sent messages are automatically indexed.
