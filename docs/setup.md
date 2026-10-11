# Local setup

## 1. Install the build tools

You need macOS 14 or newer, or Linux, plus Go 1.27.1, a C compiler, and a browser. Git is needed if you clone the source. Building downloads the Go dependencies listed in `go.mod`; running text search does not download models.

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
git clone https://github.com/Bodokh/my-whatsapp-mcp.git
cd my-whatsapp-mcp
```

If the repository is private, authenticate with a GitHub account that has access.

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

Select WhatsApp's **Message yourself** chat, or include all chats. Send a harmless, distinctive message from your phone, such as `archivecheck2026 שלום`. Search for `archivecheck2026` in the app and open the result to inspect its conversation context.

Stop the app with Ctrl+C, start it again with `./bin/wa-indexer serve`, and repeat the search. This checks fresh import, search, and local persistence. Older history is still limited to what WhatsApp delivered.

## Everyday use

To transcribe voice notes, follow the optional [Hebrew transcription setup](transcription.md). The basic setup above imports text; it does not install a speech model. Voice transcription works without configuring semantic search or image models.

Start the app with `./bin/wa-indexer serve`. It reuses the saved linked-device session when available. Ctrl+C stops the app; saved messages remain on disk.

To reopen the owner window while the app is already running, use a second terminal in the source directory:

```sh
./bin/wa-indexer serve --open-existing
```

Use the same `--data-dir` argument if you originally chose a custom directory. An expired owner window can be reopened this way without scanning a new QR code.

## Where your data lives

| OS | Default data directory |
| --- | --- |
| macOS | `~/Library/Application Support/WA Indexer` |
| Linux | `$XDG_CONFIG_HOME/WA Indexer`, or `~/.config/WA Indexer` when unset |

The data directory keeps its original `WA Indexer` name so existing archives remain usable. The app keeps the archive, WhatsApp session, model files, media, and client credentials outside the source checkout. Directories use mode `0700`; ordinary private files use `0600`. The app rejects symlinks and unsafe permissions. Choose a new, empty local directory if using `--data-dir`; do not point it at your home directory or a folder of unrelated files.

The databases are unencrypted. Enable OS disk encryption. Stop the app before copying the entire data directory for a backup, preserve its private permissions, and keep it out of public or shared folders. Do not open or modify its SQLite files while the app is running.

Disconnecting WhatsApp keeps the saved archive. **Chats and dates → Delete all local data** removes the archive, linked session, and client credentials on this computer; it does not delete phone messages.

## Troubleshooting

| Symptom | What to do |
| --- | --- |
| No chats yet | Check the phone's linked-device state, then use **Refresh discovered chats**. If no message bodies are selected, choose chats after discovery. |
| Connected, but search is empty | Confirm the chat is included, the message falls in the date range, and the saved-message count increased. Test with a new distinctive message. |
| Owner session ended | Run `serve --open-existing` while the app is running. If it has stopped, run `serve`. |
| Port already in use | Reopen the existing app. If another program owns the port, stop it or choose a different literal loopback port with `--listen 127.0.0.1:64166`. |
| `unsafe_data_path` | Use the original data directory or a fresh empty local directory. Check ownership, private permissions, and symlinks; do not loosen permissions to bypass the check. |
| `reconciliation_required` | Keep the data directory unchanged and report the fixed diagnostic codes. A durable source-safety failure such as `drain_failed` persists across restart and Resume; the app currently has no reconciliation command to clear it. Import and content reads remain blocked until verified recovery. |
| Partial history or sync gaps | Readable stored messages may still be searchable. Coverage warnings remain because unavailable messages and missed edits or deletions cannot be reconstructed reliably. |

For an offline diagnostic report, stop the app first, then run `./bin/wa-indexer doctor`. Review it before sharing; do not attach your data directory.
