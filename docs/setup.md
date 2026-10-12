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

The browser opens an authenticated owner window. The fixed local address is `http://127.0.0.1:64165`, but entering that address alone does not create an owner session. Keep this terminal open while using the archive; the server waits for requests in that terminal. The executable name remains `wa-indexer` for compatibility.

Optional: run `./bin/wa-indexer demo` instead to explore sample conversations. Demo data is separate from your real archive. Stop the demo with Ctrl+C before running `serve`.

## 3. Choose dates and start discovery

On the first run, choose **Set up your archive**, set a date range, and click **Link WhatsApp to discover chats**. The first-run flow discovers chat names without saving message bodies, so you can choose contacts after linking.

Messages received before you select chats are discarded and may not be delivered again. Selecting a longer date range cannot make WhatsApp deliver history it no longer provides.

## 4. Link your phone

Open **Link WhatsApp** and show the linking code. On your phone, open **WhatsApp → Linked devices → Link a device**, then scan the QR code.

Wait for the app to discover chats. “Connected” means a device session is open; it does not by itself prove that messages were imported.

Open **Chats and dates** and choose what to keep:

- **All chats** saves delivered messages within your date range.
- **Only chats I select** saves messages from your selected chats.

Click **Save choices**. Watch the saved-message count as readable messages arrive. These choices remain in effect after restarts.

## 5. Verify your import

Select WhatsApp's **Message yourself** chat, or include all chats. Send a harmless, distinctive message from your phone, such as `archivecheck2026 שלום`. Open **Archive**, search for `archivecheck2026`, and open the result to inspect its conversation context.

Stop the app with Ctrl+C, start it again with `./bin/wa-indexer serve`, and repeat the search. This checks fresh import, search, and local persistence. Older history is still limited to what WhatsApp delivered.

## Everyday use

To transcribe voice notes, follow the optional [Hebrew transcription setup](transcription.md). The basic setup above imports text; it does not install a speech model. Voice transcription works without configuring semantic search or image models.

Start the app with `./bin/wa-indexer serve`. It reuses the saved linked-device session when available. Ctrl+C stops the app; saved messages remain on disk.

To reopen the owner window while the app is already running, use a second terminal in the source directory:

```sh
./bin/wa-indexer serve --open-existing
```

Use the same `--data-dir` argument if you originally chose a custom directory. An expired owner window can be reopened this way without scanning a new QR code.

## Check Stats

The app opens on **Stats**; use **Archive** to search conversations. See the [Stats guide](stats.md) for filters, visible-only refresh, UTC activity, and the difference between saved-data counts and global processing status.

## Choose light or dark

Open **Appearance** on the login screen or in the app header, then choose **System**, **Light**, or **Dark** from the styled options. On mobile, use the Appearance icon in the compact header. **System** is the default and follows changes to your device's light or dark setting. An explicit choice stays in this browser for this app address; it does not change the archive, credentials, or other devices. Choose **System** again to follow your device. If browser storage is unavailable, the choice lasts only for the current page.

## Manage AI connection permissions

Follow the [MCP guide](mcp.md) to create a client connection. New connections start read-only. To change sending access, open **AI connections**, choose **Permissions** on an existing connection, then choose **Read-only**, **Selected chats**, or **All permitted chats** and click **Save permissions**.

Selected mode permits up to 100 chats. All mode includes current and future chats within that connection's read scope and your archive choices. Changing send permissions keeps the same token, read scope, and expiry; no client reconfiguration is needed. Saving permissions sends no messages. See [outbound messaging](clients/outbound-messaging.md) for scope, retries, and delivery limits.

## Where your data lives

| OS | Default data directory |
| --- | --- |
| macOS | `~/Library/Application Support/My WhatsApp` |
| Linux | `$XDG_CONFIG_HOME/My WhatsApp`, or `~/.config/My WhatsApp` when unset |

The data directory keeps its original `My WhatsApp` name so existing archives remain usable. The app keeps the archive, WhatsApp session, model files, media, and client credentials outside the source checkout. Directories use mode `0700`; ordinary private files use `0600`. The app rejects symlinks and unsafe permissions. Choose a new, empty local directory if using `--data-dir`; do not point it at your home directory or a folder of unrelated files.

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
| Owner session ended | Run `serve --open-existing` while the app is running. If it has stopped, run `serve`. |
| Port already in use | Reopen the existing app. If another program owns the port, stop it or choose a different literal loopback port with `--listen 127.0.0.1:64166`. |
| `unsafe_data_path` | Use the original data directory or a fresh empty local directory. Check ownership, private permissions, and symlinks; do not loosen permissions to bypass the check. |
| `reconciliation_required` | Keep the data directory unchanged. A durable source-safety failure such as `drain_failed` persists across restart and Resume; use the explicit [committed-snapshot recovery procedure](#recover-the-committed-archive-snapshot) only for an eligible profile. If recovery refuses it, preserve the backup and report the fixed code. |
| Partial history or sync gaps | Readable stored messages may still be searchable. Coverage warnings remain because unavailable messages and missed edits or deletions cannot be reconstructed reliably. |

For an offline diagnostic report, stop the app first, then run `./bin/wa-indexer doctor`. Review it before sharing; do not attach your data directory.
