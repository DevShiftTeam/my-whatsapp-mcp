# Local setup

## 1. Install the build tools

You need macOS 14 or newer, or Linux, plus Go 1.27.1, a C compiler, Python 3 for build-time helper verification, and a browser. Git is needed if you clone the source. Building downloads the Go dependencies listed in `go.mod`; running text search does not download models.

Install Go using the [official instructions](https://go.dev/doc/install), then check:

```sh
go version
```

On macOS, install Apple's command-line tools if they are missing:

```sh
xcode-select --install
```

On Debian or Ubuntu Linux, install the C build tools:

```sh
sudo apt-get update
sudo apt-get install build-essential
```

On other Linux distributions, install their C compiler and Make packages. A graphical Linux session also needs `xdg-open` to open the browser automatically.

## 2. Build and start

Clone the source repository and enter its directory:

```sh
git clone https://github.com/DevShiftTeam/my-whatsapp-mcp.git
cd my-whatsapp-mcp
```

Build and start the app:

```sh
./scripts/app/build.sh
./bin/wa-indexer serve
```

The source build downloads its Go dependencies and, on supported platforms, a verified Cloudflare helper from GitHub. To build with an already downloaded helper asset, set `WA_INDEXER_CLOUDFLARED_ASSET` to its absolute file path before running the build; the asset must still match the pinned platform and checksum. Normal application startup does not download build tools or models.

The browser opens an authenticated owner window. The fixed local address is `http://127.0.0.1:64165`, but entering that address alone does not create an owner session. Keep this terminal open while using the archive; the server waits for requests in that terminal. The executable name remains `wa-indexer` for compatibility.

Contributors can also assemble a basic macOS 14+ ARM64 `.app` candidate from audited, already-built inputs using the [macOS package helper](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/packaging/macos/README.md#basic-unsigned-candidate). It uses the same browser onboarding and data-directory defaults and does not require an AI runtime for text search. A local-only candidate supports local archive and stdio MCP without a tunnel helper; helper inclusion is an explicit choice subject to its pin and macOS compatibility checks. The delivered basic app needs no developer compiler, Go, Node.js or Python; optional faster-whisper transcription still has its own requirements. This is local unsigned candidate tooling, not a signed or notarized public download or completed clean-machine qualification.

Optional: run `./bin/wa-indexer demo` instead to explore sample conversations. Demo data is separate from your real archive. Stop the demo with Ctrl+C before running `serve`.

## 3. Choose dates and start discovery

On the first run, choose **Set up your archive**, set a date range, and click **Link WhatsApp to discover chats**. The first-run flow discovers chat names without saving message bodies, so you can choose contacts after linking.

For optional Hebrew voice notes, choose **Voice notes → Hebrew transcription** in that local window. The guide supplies commands for the separate service and verifies the running archive's Hebrew worker. It does not install software or restart the app from the browser. You can skip it and return through **Models and media** later; see [browser transcription setup](transcription.md#use-the-browser-setup-guide).

Messages received before you select chats are discarded and may not be delivered again. Selecting a longer date range cannot make WhatsApp deliver history it no longer provides.

## 4. Link your phone

Open **Link WhatsApp** and show the linking code. On your phone, open **WhatsApp → Linked devices → Link a device**, then scan the QR code.

Wait for the app to discover chats. “Connected” means a device session is open; it does not by itself prove that messages were imported.

Open **Chats and dates** and choose what to keep:

- **All chats** saves delivered messages within your date range.
- **Only chats I select** saves messages from your selected chats.

Click **Save choices**. Watch the saved-message count as readable messages arrive. These choices remain in effect after restarts.

Reviewed roster, administration and display notices without message bodies are handled without stopping import. They do not create message content or change local permissions. Unsupported records follow the selected history policy below; source-safety checks remain active.

## 5. Verify your import

Select WhatsApp's **Message yourself** chat, or include all chats. Send a harmless, distinctive message from your phone, such as `archivecheck2026 שלום`. Open **Archive**, search for `archivecheck2026`, and open the result to inspect its conversation context.

Stop the app with Ctrl+C, start it again with `./bin/wa-indexer serve`, and repeat the search. This checks fresh import, search, and local persistence. Older history is still limited to what WhatsApp delivered.

## Choose a rebuild history strategy

Rebuilt profiles default to `serve --rebuild-history-policy selected-original`. This strategy admits older message bodies only through individually correlated recovery requests. A trusted operator can instead explicitly select `--rebuild-history-policy delivered-history` at startup to import validated history pages delivered through the linked session. Keep the same profile, linked-device session, client grants and existing launch arguments.

Delivered history is a **partial snapshot**, without individual request proof or a guarantee of current phone state. Supported deletions, edits, expiry and view-once restrictions, plus current chat/date permissions, still apply. This setting is independent of the unknown-history policy below; it does not bypass source-safety checks.

The selection is recorded before connection and cannot change through a browser, MCP call or attachment to an existing process. Restart with the explicit delivered-history flag after selecting it; omitting the flag must not silently change a stored selection. Explicitly switching back to selected-original retains the partial-snapshot warning and prior accounting.

Paging starts from the newest usable anchor for each selected chat and requests up to 500 messages per page, with at least one second between serial page requests. The linked phone may return fewer messages or respond slowly; these request settings do not guarantee import throughput. Requests and accepted historical bodies have finite, persisted budgets. Existing exact-message requests and spent credits remain intact; a restart does not refund them. Owner progress distinguishes readable messages, delivered bodies and page attempts from individually recovered copies. Missing anchors, a stalled page, exhaustion or reaching the selected date do not establish complete coverage. WhatsApp may deliver less history than requested, and no date setting guarantees a full-year import.

## Choose an unknown-history policy

`serve --unknown-history-policy strict` is the default. A trusted operator may explicitly choose `--unknown-history-policy partial` on the existing `serve` launch. Keep the current data directory, linked-device session, AI-client configuration and other launch arguments. Changing this option requires stopping and restarting the app; there is no browser or MCP toggle.

Partial mode processes supported source controls before handling unsupported noncontent metadata, notices or control shapes. It records unsupported effects as history gaps so readable messages can continue importing and remain available to Archive, Stats and MCP. Supported deletion, edit, expiry and view-once controls, current chat/date policy and client permissions remain enforced. Skipped noncontent data does not generate message text, and genuine source-safety failures still stop protected reads.

Choosing partial mode accepts that unsupported metadata, edits, deletions or retention changes may be unreflected. When a gap is encountered, the owner UI and archive reads, including MCP, carry a persistent account-wide warning: history is partial and current phone state is unknown. This warning does not mean every event defined by the WhatsApp SDK is supported. Restarting, switching back to strict or upgrading the recovery destination does not clear an existing warning. This does not establish complete history.

## Everyday use

To transcribe voice notes, open **Models and media → Set up Hebrew transcription**, or follow the optional [Hebrew transcription setup](transcription.md). The basic text setup does not install a speech model. Voice transcription works without configuring semantic search or image models.

Start the app with `./bin/wa-indexer serve`. It reuses the saved linked-device session when available. Ctrl+C stops the app; saved messages remain on disk.

Native shutdown allows up to 45 seconds for HTTP requests, accepted source work and workers to drain under one shared deadline. If using your own native process supervisor, allow at least 60 seconds before forced termination; a longer supervisor timeout does not extend the app's deadline or guarantee a clean stop. Explicit container serving keeps its existing eight-second budget within the image's ten-second stop window. Interrupted or failed drains still require the eligible offline recovery procedure below, never automatic recovery.

To reopen the owner window while the app is already running, use a second terminal in the source directory:

```sh
./bin/wa-indexer serve --open-existing
```

Use the same `--data-dir` argument if you originally chose a custom directory. An expired owner window can be reopened this way without scanning a new QR code.

## Inspect message-ingestion latency

The local owner API provides a read-only `GET /api/ingestion/status` diagnostic. Use the current local owner session and its `X-CSRF-Token` capability. It accepts no query parameters or request body. Remote admin sessions and MCP credentials cannot use this native diagnostic; keep the local owner listener out of tunnels.

The response reports bounded queue counts and bytes, pending fresh text, the oldest pending live-event age, history preparation, and aggregate phase timings. It contains no messages, chat identifiers, credentials or private paths. Offline, demo and read-only historical modes do not provide active ingestion diagnostics. `keyword.pending = 0` in normal index status does **not** mean that the ingestion queue is empty: messages may still be waiting before their archive transaction begins.

Eligible fresh text can pass validated positive-only history work using a separate bounded worker. History that is still downloading or being verified, records containing sensitive or unsupported effects, and overlapping message IDs retain their original ordering. Those cases, network delivery, source-safety pauses or a busy database can still delay a message. The change does not reset the archive, request a full resync or replace client credentials.

Already established PN/LID identity mappings are checked before their text uses the priority path. Messages requiring a new or conflicting mapping keep their original source ordering; the fast path cannot grant identity or message authority.

`queued_count` includes queued and in-flight source work; `waiting_count` is the separate callback-capacity ledger. `live_pending` and its oldest-age value describe admitted live work, not callbacks still waiting for capacity.

Compare `admission_wait`, `queue_wait`, `source_commit` and `callback_to_commit` for the live lane. Admission includes callback validation and capacity waiting. Queue time includes preparation or ordering barriers. Source commit includes waiting for the writer, not just SQLite execution. The last phase measures successful text application from the SDK callback, not from a phone's timestamp. History download, decode and preparation have separate timings. Percentiles are approximate fixed-histogram values accumulated in the running process; they are not a guarantee of phone-to-search delivery time. The diagnostic itself changes no archive choices or processing settings.

## Check Stats

The app opens on **Stats**. Stats and Archive show **All saved messages** within your current archive choices. Archive opens a recent-message list without requiring a search; clearing the search returns to that list. When captured history is configured, eligible messages missing from the current archive appear with a **Captured** date and **Read-only** label. Current copies take precedence, and current permissions and known message restrictions still apply. Captured rows cannot be used for sending, current reply targets, original downloads or processing jobs. Settings, AI connections and import progress always describe the current archive. Saved history may be partial and does not establish your phone's current state. See the [Stats guide](stats.md) for filters, visible-only refresh, UTC activity, and the difference between saved-data counts and global processing status.

## Choose light or dark

Open **Appearance** on the login screen or in the app header, then choose **System**, **Light**, or **Dark** from the styled options. On mobile, use the Appearance icon in the compact header. **System** is the default and follows changes to your device's light or dark setting. An explicit choice stays in this browser for this app address; it does not change the archive, credentials, or other devices. Choose **System** again to follow your device. If browser storage is unavailable, the choice lasts only for the current page.

## Manage AI connection permissions

Follow the [MCP guide](mcp.md) to create a client connection. New connections start read-only. To change sending access, open **AI connections**, choose **Permissions** on an existing connection, then choose **Read-only**, **Selected chats**, or **All permitted chats** and click **Save permissions**.

Selected mode permits up to 100 chats. All mode includes current and future chats within that connection's read scope and your archive choices. Changing send permissions keeps the same token, read scope, and expiry; no client reconfiguration is needed. Saving permissions sends no messages. See [outbound messaging](clients/outbound-messaging.md) for scope, retries, and delivery limits.

## Where your data lives

| OS | Default data directory |
| --- | --- |
| macOS | `~/Library/Application Support/WA Indexer` |
| Linux | `$XDG_CONFIG_HOME/WA Indexer`, or `~/.config/WA Indexer` when unset |

The data directory keeps its original `WA Indexer` name so existing archives remain usable. Keep the same explicit `--data-dir` if you use another location. The app keeps the archive, WhatsApp session, model files, media, and client credentials outside the source checkout. Directories use mode `0700`; ordinary private files use `0600`. The app rejects symlinks and unsafe permissions. Choose a new, empty local directory if using `--data-dir`; do not point it at your home directory or a folder of unrelated files.

The databases are unencrypted. Enable OS disk encryption. Stop the app before copying the entire data directory for a backup, preserve its private permissions, and keep it out of public or shared folders. Do not open or modify its SQLite files while the app is running.

Disconnecting WhatsApp keeps the saved archive. **Chats and dates → Delete all local data** removes the archive, linked session, and client credentials on this computer; it does not delete phone messages.

## Recover the committed archive snapshot

A durable source-safety pause, such as `reconciliation_required` with `drain_failed`, survives an ordinary restart or **Resume**. If the archive passes integrity and recovery checks, the explicit offline recovery command can restore access to its last committed snapshot while keeping a durable partial-coverage warning.

1. Stop the app. Recovery requires exclusive access to the same existing data directory; it cannot run beside a live server.
2. Make a private backup of the entire data directory, preserving its permissions. Keep the archive, linked-device session, and client files together.
3. Run the updated executable with your existing directory's canonical absolute path:

```sh
./bin/wa-indexer recover-snapshot \
  --data-dir "/absolute/path/to/existing/archive"
```

Use only `--data-dir` for this command. It does not connect to WhatsApp or start model and media workers. It validates the existing profile and archive, records an audited recovery, and preserves committed messages, known deletions and conflicts, expiry, archive choices, the linked-device store, and client grants.

Recovery accepts eligible `drain_failed` and `unclean_shutdown` interruptions. It refuses unexplained unsafe markers, identity conflicts, other unsafe reasons, and failed structural checks. If the command itself was interrupted, rerunning it can complete its prepared audit after revalidation; a newer unsafe state remains refused.

A successful receipt reports `mode: "committed_snapshot"`, `code: "owner_restored_snapshot"`, `coverage: "partial"`, and `current_phone_state_verified: false`, together with a recovery epoch and archive revision. Restart `serve` with the same data directory, listener settings, private env files, and other launch arguments. Check the saved-message count and a known search result, then test an existing MCP connection.

This procedure does not reconcile missing WhatsApp edits or deletions, reconstruct undelivered history, or prove the current phone state. The `owner_restored_snapshot` gap remains visible to MCP clients. Known source restrictions still apply. Ordinary startup never runs recovery automatically.

If recovery refuses the profile or reports `reconciliation_required`, keep the backup and report the fixed diagnostic code. Unsupported unsafe reasons or failed structural checks require diagnosis; do not change archive files to bypass the refusal.

## Accept a classified partial history snapshot

`recover-classified-history` is a separate offline operator action for an existing live-recovery profile with an explicitly reviewed `history_corrupt` pause. It admits only three reviewed notice shapes: participant-number change (stub `33`, presence `475`), username change (stub `193`, presence `411`) and group member link-mode change (stub `217`, presence `411`). The existing `33` and `193` cases remain unchanged; the shared non-content-notice classification admits only the exact `193` and `217` shapes, not arbitrary notices. Unknown stubs, other unsafe reasons or changed evidence remain refused. This is owner acceptance of the already committed **partial** snapshot, not replay, reconstruction or proof of current phone history.

Stop the current destination and every writer first, then make a coherent private backup. A trusted operator must review the matching private evidence and provide its private JSON configuration; do not construct acceptance from a log line alone. The command requires exclusive ownership and evidence bound to the current namespace, safety state and recorded stopped runtime. A new operation after a completed recovery requires fresh matching evidence, recorded append-only; evidence from the completed operation cannot authorize a later safety pause.

```sh
./bin/wa-indexer recover-classified-history \
  --config "/absolute/private/recovery.json"
```

Use only `--config`; there is no force option or HTTP, MCP or Settings recovery route. The operation preserves the latest destination's linked-device session, current grants, send journal, archive choices and spent recovery allowances. It does not restore any of them from an older profile. Only exact reviewed committed-message-conflict diagnostics can accompany username-change or group member link-mode acceptance; conflicting messages remain quarantined, and spent requests are not refilled. Never clear safety markers with SQL or roll back to a stale session to bypass refusal.

Success reports `mode: "committed_snapshot"`, `code: "owner_restored_snapshot"`, `coverage: "partial"`, `current_phone_state_verified: false` and a recovery epoch. Restart the same destination with its existing launch settings, then verify known reads and current permissions. Explicit acceptance is never an automatic startup action or a general guard-clear switch; the partial-coverage warning remains, and later source-safety failures still block access.

## Accept unknown partial history offline

`recover-partial-history` is a separate explicit owner acceptance of an eligible already committed **partial** snapshot after an eligible history pause. It admits only the narrowly reviewed unknown-stub case (`229`) or a pinned-message notice stop (`177`, presence `411`) from its specifically reviewed producer, with matching protected evidence. The older pinned-message diagnostic does not establish the absence of every control field: accepting it records unresolved history uncertainty, not proof of a benign shape. Neither case proves recovery of the unvisited remainder. It does not classify the unknown record as a benign notice; the known `33`/`193`/`217` acceptance above remains separate. Neither `recover-snapshot` nor `recover-classified-history` accepts either of these accepted-gap cases.

Stop the app and every writer, retain a coherent private backup, and have a trusted operator review the current protected evidence and private configuration. Exclusive ownership and checks against the exact current archive state remain required. Both the recovery witness and operator acceptance must explicitly acknowledge unknown-history gaps; a log line alone is insufficient. Later operations still need fresh append-only evidence.

```sh
./bin/wa-indexer recover-partial-history \
  --config "/absolute/private/recovery.json"
```

Use only `--config`. The command preserves the current linked-device session, AI-client grants, send journal, archive choices, deletion and conflict barriers, and spent allowances. It accepts the committed snapshot and uncertainty about the unvisited remainder of the failed batch; it does not replay that record, recover that remainder, or prove that the older record meets today's partial-ingestion checks. It never enables partial ingestion automatically. Restart with the existing launch settings, adding `--unknown-history-policy partial` only if explicitly choosing that ongoing policy. The persistent account-wide warning remains; current phone state is unknown. There is no force option or HTTP, MCP or Settings recovery route, and no automatic guard clearing.

## Recover associations for existing original recordings

If verified original recordings are still in the current profile but their cache associations were invalidated, an offline command can check them against a stopped private backup. This repairs eligible metadata; it cannot recover missing files or copy recordings out of the backup.

1. Stop the app. Both the current profile and backup must be existing private profiles with their instance-lock files, and neither may have a running owner.
2. Keep the backup unchanged. Use distinct canonical absolute paths for the current profile and the stopped backup:

```sh
./bin/wa-indexer recover-original-cache \
  --data-dir "/absolute/path/to/existing/archive" \
  --backup-dir "/absolute/path/to/stopped/private/backup"
```

The backup must be checkpointed: a nonempty SQLite WAL is refused without modification. More than 256 qualifying originals in the backup is also refused before any repair. The current archive must already use the schema expected by the executable.

Use only these two flags. Recovery verifies the existing recording bytes and authenticated completion evidence against matching current source authority. Current policy, deletion, conflict, and expiry checks still apply. It preserves messages, client grants, completion evidence, and media quota charges, and changes only eligible cache associations. It creates no profile or database, performs no migrations or expiry cleanup, starts no network or model work, and copies or deletes no recordings. The backup remains unchanged.

Success returns only aggregate `restored` and `skipped` counts. A missing, changed, or invalid individual recording is skipped; ownership or structural failures stop the command. Repairs commit separately for each artifact, so a later failure can leave earlier associations restored. Repeating the command is safe and idempotent after resolving the reported fixed diagnostic.

Restart the archive with its existing data directory and launch arguments. With transcription configured, eligible restored recordings can be processed normally. Check the generated text through search and MCP; cache recovery itself does not transcribe audio, repair missing phone history, or clear coverage gaps.

## View a historical snapshot

Use this advanced mode to read a **separate, stopped copy** of an archive without syncing or changing it. It shows the capture time and partial coverage. It cannot confirm the current state of messages on your phone.

Before starting, check that the copy has the current archive policy and AI-client permissions. A backup must not restore a revoked connection or widen a client's access. Repeat this check before each activation, and stop the snapshot if permissions change. Client expiry and source expiry still use the current time.

Prepare and recover the separate profile before sealing it. Keep the original archive unchanged. If you copied native client configuration, rebind it to the copy before sealing. Snapshot startup requires the expected database schema, safe archive state, and ready client journals; it does not repair or migrate the copy.

Save `archive/historical-snapshot.json` inside the copied data directory. It must be a regular, privately owned file with mode `0600`, one hard link, and at most 2048 bytes. Use these exact field names, replacing the example timestamps with the copy's actual capture time and the time you checked its current permissions:

```json
{
  "version": 1,
  "captured_at": "2026-01-02T03:04:05Z",
  "authorization_verified_at": "2026-01-02T04:04:05Z",
  "current_authorization_verified": true
}
```

The permission check time must be no earlier than capture and no later than startup. Set the final field to `true` only after checking permissions; the app cannot compare them with another profile for you. Extra fields and mismatched timestamps are rejected.

Launch with the copied directory's absolute path and the **same capture timestamp** as the marker:

```sh
./bin/wa-indexer serve \
  --data-dir "/absolute/path/to/private/snapshot" \
  --historical-snapshot-at "2026-01-02T03:04:05Z"
```

You can search saved text and existing transcripts, view Stats, and use permitted MCP reads, including contact, group, and attachment metadata. Syncing, sending, original-file downloads, model installation, processing, and archive edits are disabled. Tools that require those actions return `historical_snapshot_read_only`.

Managed remote-provider setup and reconnection are disabled. Existing explicit manual MCP or password-admin listener settings can still be used, with current access checks. The snapshot does not open a WhatsApp session. Demo mode, `--open-existing`, and original-media budget changes are incompatible with this mode. The marker also prevents ordinary serving and mutation commands from opening the copy.

## Troubleshooting

| Symptom | What to do |
| --- | --- |
| No chats yet | Check the phone's linked-device state, then use **Refresh discovered chats**. If no message bodies are selected, choose chats after discovery. |
| Connected, but search is empty | Confirm the chat is included, the message falls in the date range, and the saved-message count increased. Test with a new distinctive message. |
| Fresh text arrives slowly | Check the local [ingestion diagnostic](#inspect-message-ingestion-latency) to distinguish capacity/queue waits from history preparation and source commits. A ready keyword index alone does not establish that incoming work has drained. |
| Owner session ended | Run `serve --open-existing` while the app is running. If it has stopped, run `serve`. |
| Port already in use | Reopen the existing app. If another program owns the port, stop it or choose a different literal loopback port with `--listen 127.0.0.1:64166`. |
| `unsafe_data_path` | Use the original data directory or a fresh empty local directory. Check ownership, private permissions, and symlinks; do not loosen permissions to bypass the check. |
| `reconciliation_required` | Keep the data directory unchanged. A durable source-safety failure such as `drain_failed` persists across restart and Resume; use the explicit [committed-snapshot recovery procedure](#recover-the-committed-archive-snapshot) only for an eligible profile. If recovery refuses it, preserve the backup and report the fixed code. |
| Partial history or sync gaps | Readable stored messages may still be searchable. Coverage warnings remain because unavailable messages and missed edits or deletions cannot be reconstructed reliably. |

For an offline diagnostic report, stop the app first, then run `./bin/wa-indexer doctor`. Review it before sharing; do not attach your data directory.

## Prepare a separate live recovery profile

`wa-indexer rebuild-live --config ABSOLUTE_PRIVATE_CONFIG` is an offline operator operation. It creates a new independent profile from a coherent current backup, preserving the existing account, client grants and tokens, send permissions, send-request journal, canonical chat identities, ready connection records, and saved linked device. Keep the original profile and historical data intact. The command does not connect WhatsApp or switch a public route.

Preparation requires the stopped source's exclusive instance lease, a private external startup guard, an exact backup manifest and fingerprints, and a nonexistent destination. The private operator configuration supplies the expected account/device identity and an explicit date and timezone when changing the archive start date. Native configurations are rebound only in the destination with a durable journal; existing host launchers must be switched explicitly after readiness. Unsupported or pending connection records refuse preparation.

The destination is a separate fresh archive namespace: it starts with zero copied message bodies and no old media, derived text, or processing jobs. Known deletion, clear, expiry, view-once, and edit barriers remain enforced. New live messages use normal ingestion. Earlier content becomes readable only when an authenticated primary-phone response matches a newly registered exact-key request and current policy. Unmatched history contributes discovery metadata only. Recovery starts with a durable allowance of 10,000 exact phone-copy attempts and 512 history-discovery requests. Small windows continue automatically through known eligible candidates, including after restart; the window size is not a lifetime message limit. Requests remain rate-limited, so a large recovery can take many hours. Failed or uncertain requests consume allowance and are never retried automatically. Archive and Stats show history-import activity without opening Settings, refreshing every five seconds while the page is visible. Recent request activity animates; waiting, paused, disconnected, exhausted and unavailable states stay explicit. Accepted history copies are separate from readable messages in Live, queued requests and uncertain outcomes; neither count is a percentage or guarantee of full history. Settings retains detailed attempts and both allowances. Pause or resume without resetting progress; when exhausted, explicitly allow another bounded increment. Saving an earlier date revises the same recovery operation without resetting spent allowance. Coverage remains partial; choosing January or any earlier date does not establish complete history.

Keep existing searchable historical access available while qualifying the new archive on separate local ports. Do not switch public search merely because the new device connects; fresh phone copies may not restore all earlier searchable history. Start the prepared destination with ordinary `serve` options on separate local ports. Use its preserved current linked-device session; preparation alone is not a connection, and a valid saved session can be reused without a new QR scan. It retains the source profile's lease while active. A trusted supervisor that already holds this lock may pass the verified descriptor with `--rebuild-source-lock-fd`; this lets an independent historical reader stay available during preparation. The descriptor is an operator capability and is never accepted from HTTP input. Preserve the same canonical public authority and verify normal owner access, saved MCP authentication, scopes, expiry, and source safety before the operator switches upstream routing. Keep historical access owner-only after cutover, or enforce the live authority on every historical read; frozen historical grants must not retain independent bearer access. Never run the saved linked device in both profiles or automatically roll back to a stale session copy after connection.

Date, chat, retention, live sync and client controls remain available. Deleting a recovery profile requires an explicit session ownership transition first; Settings explains this and disables that destructive action. Detailed provenance and mutation diagnostics stay in private files. Terminal output contains only fixed preparation status and never proves live activation or complete phone history.

Upgrading an already connected recovery profile uses that same destination and saved session after a clean stop and coherent destination backup. Rebuild operation tables are added only to the destination; existing pending and consumed requests count against its initial allowance. Never bootstrap it again from the original saved session.

### Attach a captured historical view

A trusted operator can attach a distinct captured, query-only historical directory to the live app with `--historical-view-dir`, `--historical-view-id` and `--historical-view-at`. Use verified capture metadata and a directory separate from both the live destination and its retained source namespaces. These are operator launch settings, not paths or permissions that a browser or MCP client can supply. Keep its contents as a captured snapshot and preserve its private permissions; the app opens the archive data query-only. Do not start its saved linked device alongside the live profile.

With the view configured, **View archive → Live / Historical** switches Archive and Stats. Live is the default. The historical view is a partial captured snapshot, not a current phone mirror, and does not supply originals, media fetches or sending. Settings, date/chat choices, recovery allowances, live importing and send controls always act on Live. Existing live credentials and current policy govern historical access, including final response delivery; an unsafe live account cannot fall back to historical reads. See [MCP source selection](mcp.md#choose-live-or-historical-reads).

For forward recovery after live activation, preserve the latest connected destination, linked-device state, current grants and send journal. Continue the same recovery operation with its finite allowances, or explicitly authorize a further bounded increment; do not reset spent requests or bootstrap from a stale predecessor session. Keep the retained historical capture separate. Connecting the device, extending a budget or selecting an earlier date does not prove complete recovered history.
