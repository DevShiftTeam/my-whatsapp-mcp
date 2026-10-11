<img src="docs/assets/logo.png" alt="My WhatsApp MCP logo" width="128">

# My WhatsApp MCP

**Your WhatsApp, searchable by your AI.**

Find a detail in your conversations or let Codex, Claude Code, or Composio read the chats you choose. My WhatsApp MCP links by QR code, keeps an archive on your computer, and gives each AI client controlled, read-only access. Hebrew and English keyword search work without a model download or paid API.

- **Keep it local.** One Go executable, an embedded browser interface, and SQLite storage. No Docker, Node.js, or Python needed for the basic app.
- **Choose the scope.** Select chats and dates for your archive, then set separate permissions and expiry for each AI client.
- **Find the context.** Search saved messages and open the surrounding conversation.
- **Check your archive.** Filter saved-message and media stats by date, chat, or type, and see processing status. See [Stats](docs/stats.md).
- **Connect your tools.** Use a local MCP adapter or expose a scoped bearer endpoint through your own HTTPS tunnel.

## Start locally

Install Go 1.27.1 and a C compiler, then clone and start:

```sh
git clone https://github.com/Bodokh/my-whatsapp-mcp.git
cd my-whatsapp-mcp
./scripts/app/build.sh
./bin/wa-indexer serve
```

The app opens an authenticated owner window at `http://127.0.0.1:64165`. Keep the terminal running. Follow the [step-by-step setup guide](docs/setup.md) to link your phone and verify an import.

To try synthetic Hebrew and English conversations first, run `./bin/wa-indexer demo`. The demo uses a separate archive and never connects to WhatsApp. Stop it with Ctrl+C before starting your own archive. The executable name remains `wa-indexer` for compatibility.

## Optional models

For voice messages, add an optional local [batch transcription service](docs/transcription.md). It uses faster-whisper on CPU with a multilingual model and automatic language detection, plus an optional Hebrew-specific ivrit-ai profile. Language and model are configurable. It stores generated transcripts separately from message text and makes them available to keyword search and MCP. It needs Python; the default model files occupy about 1.6 GB. The ordinary app runs without it.

Semantic search, image OCR and descriptions, and the native whisper.cpp transcription path require a separately prepared native bundle and remain experimental. See [optional native models](docs/native-models.md) before preparing a bundle. No model downloads start automatically in the basic setup.

Build from source on macOS 14 or newer, or Linux. Windows is unsupported.

## Limits and privacy

WhatsApp may deliver only part of your history. The app reports received coverage and sync gaps; it cannot promise a complete backup or exact current phone state. It uses the unofficial [whatsmeow](https://github.com/tulir/whatsmeow) library, so WhatsApp changes can interrupt linking or import.

Your archive and linked-device credentials stay in a private local data directory. SQLite files are **not encrypted by the app**. Use OS disk encryption and protect backups. Connecting an AI client shares its permitted results with that client; remote access also passes through your tunnel provider.

## Documentation

- [Local setup and troubleshooting](docs/setup.md)
- [Stats and archive processing](docs/stats.md)
- [MCP clients and remote access](docs/mcp.md)
- [Voice transcription, including Hebrew](docs/transcription.md)
- [Optional native models and media](docs/native-models.md)
- [Code structure and development](docs/development.md)
- [Contributing](CONTRIBUTING.md) and [security](SECURITY.md)
- [Changelog](CHANGELOG.md)

All guides are Markdown files in this repository.

Licensed under [GPL-3.0-only](LICENSE). Bundled and optional third-party components have their own notices in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
